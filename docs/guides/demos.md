# anvil 데모 가이드

> 운영자가 직접 실행하는 라이브 데모 모음입니다. 프로젝트 개요는 [README](../../README.md)를 참고하세요.
> Observability 데모(`observability_demo.sh`)는
> [security-and-resilience.md](security-and-resilience.md#observability-v035)의 "Try the demo" 절에 있습니다.

## Multi-Agent Webdev Demo (v0.3.6)

`webdev_demo.sh` is a one-shot operator demo that exercises the full flock stack: it stands up an **orchestrator + worker + reviewer** flock and has them collaboratively design, build, and publish a small React + Vite portfolio site — entirely from inside the VMs, with the host acting only as a passive harvester.

### What it does

1. Preflight (memory headroom, `/dev/kvm`, vite-template present), then swaps each role's `*.webdev.{md,yaml}` overrides over its `system.md` / `goose.yaml` and starts the daemon.
2. `POST /flocks` spawns the three agents.
3. A background SSE subscriber (`GET /flocks/{id}/wall`) harvests `<<<FILE: path>>> … <<<END>>>` sentinels off the Town Wall, writes each file under a working `site/` tree, and exits on `<<<DONE>>>`.
4. One `POST /vms/{orchestrator}/tasks` kicks off the orchestrator, which drives the whole job in a single Goose session: for each of `src/App.jsx`, `src/main.jsx`, `src/index.css`, `index.html` it runs `gtcall worker-1 '…'` to generate the file, `gtwall` to publish it to the Town Wall, then a best-effort `gtcall reviewer-1 '…'` review note — and finally posts `<<<DONE>>>`.
5. The host overlays the harvested files onto the vite-template, runs `npm install` + `vite build`, and serves the result with `vite preview` on `:5173` until `Ctrl-C`.

### Run it

```bash
sudo WEBDEV_MIN_MEM_MIB=5000 bash webdev_demo.sh
```

Requirements: a Google Gemini API key **and** a `GROQ_API_KEY` in `configs/goose-secrets.yaml`, `/dev/kvm` + root, and enough free RAM for three 2 GiB VMs (`WEBDEV_MIN_MEM_MIB` sets the preflight floor; Firecracker allocates guest RAM lazily and host swap cushions the peak). Open `http://localhost:5173` to see the generated site; `GET /flocks/{id}/wall/history` shows the four `<<<FILE:>>>` posts authored by `orchestrator-1`.

### Notes

- **Manual gate, not CI.** Like `observability_demo.sh`, this demo needs an LLM key and `/dev/kvm`, neither of which exists on GitHub Actions runners, so it is an operator-run gate rather than an automated test.
- **Model choice.** The orchestrator runs `gemini-2.5-flash` — it must drive a ~13-step tool-calling loop without stalling, which `gemini-2.5-flash-lite` could not do reliably (it tended to plan and then stop). Worker and reviewer run on Groq (`GOOSE_PROVIDER: groq`, `GOOSE_MODEL: openai/gpt-oss-20b`) for single-shot generation/review — a hybrid Gemini-orchestrator + Groq-workers setup. On the free tier all models share a 20 RPM cap that multi-turn orchestration exhausts in seconds, so the demo assumes a **paid-tier** Gemini key.
- **No host authorship.** Every published file is authored by an in-VM agent via `gtwall`; the host only harvests and builds. If the orchestrator fails to publish a file, the host keeps that file's vite-template placeholder so `vite build` still succeeds.

