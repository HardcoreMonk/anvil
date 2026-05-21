# Anvil Script Workload Runner Design

## 목표

VM workload E2E가 LLM/provider 상태와 무관하게 VM 내부 서비스를 설치하고 실행할 수
있도록 script-only workload runner를 추가한다.

이 기능은 범용 remote shell이 아니다. control plane caller는 VM의
`/workspace/workloads/` 아래에 미리 업로드된 `.sh` 파일만 실행할 수 있다. 이를 통해
nginx, Go HTTP server, Redis, PostgreSQL 같은 workload smoke와 benchmark를
deterministic하게 검증한다.

## 문제

현재 workload E2E는 다음 경로를 사용한다.

```text
host harness
  -> PUT /vms/{vm_id}/workspace
  -> POST /vms/{vm_id}/tasks
  -> goose-agent
  -> goose run
  -> LLM provider
  -> model이 shell 실행을 수행
```

이 구조는 real LLM 검증에는 유용하지만, VM 서비스 설치와 성능 baseline에는 부적합하다.
provider key가 없거나 모델 호출이 실패하면 workload script가 실행되기 전 단계에서
테스트가 실패한다.

## 비목표

- 범용 `/exec` 또는 임의 command 실행 API를 추가하지 않는다.
- request body에 shell command string을 받지 않는다.
- `/workspace/workloads/` 밖의 파일을 실행하지 않는다.
- interactive shell, stdin streaming, PTY, background job manager는 제공하지 않는다.
- benchmark 결과를 제품 성능 보증치로 선언하지 않는다.
- real LLM E2E를 대체하지 않는다. real LLM suite는 `/tasks` 경로로 별도 유지한다.

## API 계약

Control plane endpoint:

```text
POST /vms/{vm_id}/workloads/run
```

Guest agent endpoint:

```text
POST /workloads/run
```

Request:

```json
{
  "script": "workloads/nginx-smoke.sh",
  "timeout_seconds": 600
}
```

Response success:

```json
{
  "script": "workloads/nginx-smoke.sh",
  "exit_code": 0,
  "stdout": "...",
  "stderr": "",
  "duration_ms": 12345,
  "timed_out": false
}
```

Response command failure:

```json
{
  "script": "workloads/nginx-smoke.sh",
  "exit_code": 21,
  "stdout": "...",
  "stderr": "...",
  "duration_ms": 12345,
  "timed_out": false
}
```

Command failure returns HTTP `200` because the runner itself completed and the
script exit code is part of the result. API or validation failures return HTTP
4xx/5xx JSON errors.

Timeout response:

```json
{
  "script": "workloads/go-http-bench.sh",
  "exit_code": -1,
  "stdout": "...",
  "stderr": "workload timed out after 600s",
  "duration_ms": 600000,
  "timed_out": true
}
```

## Validation

`script` is required and must satisfy all conditions:

- relative path
- clean path begins with `workloads/`
- clean path ends with `.sh`
- no `..`
- resolved absolute path remains inside `/workspace/workloads/`
- target exists
- target is a regular file

`timeout_seconds`:

- optional
- default: `600`
- minimum: `1`
- maximum: `1800`

Output limits:

- stdout max: `1 MiB`
- stderr max: `1 MiB`
- if either stream exceeds the limit, truncate the stream and set
  `stdout_truncated` or `stderr_truncated` in the response.

## Guest Agent Behavior

`goose-agent` adds an authenticated `/workloads/run` endpoint. It uses the same
agent token middleware as `/tasks`, `/workspace`, `/stop`, and `/townwall/post`.

Execution:

```text
POST /workloads/run
  -> validate JSON
  -> validate script path
  -> acquire existing single-agent busy mutex
  -> context.WithTimeout(request context, timeout_seconds)
  -> exec.CommandContext(ctx, "bash", "/workspace/<script>")
  -> run with working directory /workspace
  -> capture stdout/stderr separately with limits
  -> return WorkloadRunResult JSON
  -> release busy mutex
```

The endpoint deliberately invokes `bash <script>` instead of executing arbitrary
request-provided commands. The request controls only which allowed script file is
run and the bounded timeout.

## Control Plane Behavior

`cmd/goose-daemon` extends the existing agent proxy pattern.

```text
POST /vms/{vm_id}/workloads/run
  -> authenticate external caller with existing control-plane auth
  -> find VM by vm_id
  -> proxy request to http://<guest_ip>:8080/workloads/run
  -> inject per-VM agent token
  -> stream JSON response back to caller
```

The control plane does not parse script output and does not log request bodies,
stdout, stderr, agent tokens, or control-plane tokens.

## Harness Integration

`scripts/vm-workload-e2e.sh` stops using `/tasks` for deterministic workload
execution.

New sequence:

1. Upload `scripts/workloads/nginx-smoke.sh`.
2. Upload `scripts/workloads/go-http-server.go`.
3. Upload `scripts/workloads/go-http-bench.sh`.
4. Call `POST /vms/{vm_id}/workloads/run` for `workloads/nginx-smoke.sh`.
5. Call `POST /vms/{vm_id}/workloads/run` for `workloads/go-http-bench.sh`.
6. Fetch `workloads/results/nginx.log`, `go-http.log`, and `bench.txt`.
7. Run host probes and host benchmark.

`task-output.json` is replaced by:

- `nginx-run.json`
- `go-http-run.json`

The harness pass condition checks:

- workload runner HTTP status is `200`
- `exit_code` is `0`
- expected marker appears in `stdout`
- result logs are fetchable
- host probes pass

## Security

- No general command execution API is exposed.
- Request body never contains arbitrary shell text.
- Only `/workspace/workloads/*.sh` can be executed.
- Existing per-VM agent token protects the guest endpoint.
- Existing control-plane auth protects the daemon endpoint.
- Output artifacts must not include provider keys, control-plane tokens, or
  agent tokens.
- The runner does not read `/root/.config/goose/secrets.yaml`,
  `/root/.ephemera-agent-token`, or other secret paths.

## Error Handling

Validation errors:

- invalid JSON: HTTP `400`
- empty script: HTTP `400`
- absolute path: HTTP `400`
- traversal path: HTTP `400`
- path outside `workloads/`: HTTP `400`
- non-`.sh` script: HTTP `400`
- missing script: HTTP `404`

Runtime outcomes:

- script exits nonzero: HTTP `200`, `exit_code` set to script exit code
- script timeout: HTTP `200`, `timed_out: true`, `exit_code: -1`
- agent busy: HTTP `503`
- VM missing at control plane: HTTP `404`
- agent unreachable from control plane: HTTP `502`

## Testing Strategy

Unit tests in `cmd/goose-agent`:

- rejects empty, absolute, traversal, non-workload, and non-`.sh` paths
- returns `404` for missing allowed script
- runs a small allowed script and captures stdout/stderr
- returns nonzero exit code without HTTP `500`
- enforces timeout with `timed_out: true`
- rejects concurrent workload when busy

Daemon tests in `cmd/goose-daemon`:

- routes `POST /vms/{vm_id}/workloads/run`
- rejects non-POST
- proxies to `/workloads/run`
- injects agent token via existing proxy path

Harness checks:

- `bash -n scripts/vm-workload-e2e.sh`
- fake HTTP server test for harness request/response handling before KVM execution
- full KVM workload E2E on KVM host:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo -n bash scripts/vm-workload-e2e.sh
```

## Documentation

Update:

- `docs/architecture/runtime-architecture.md`
- `docs/architecture/service-logic.md`
- `docs/operations/runbook.md`
- `docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md`

The updated docs must state that deterministic workload E2E no longer depends on
LLM provider credentials. Real LLM tests still use `/tasks`.
