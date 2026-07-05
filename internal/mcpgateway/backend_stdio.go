package mcpgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// 학습 주석 개요: v0.6.4 에서 추가된 stdio backend(로컬 subprocess) 구현. 핵심
// 하드닝 4가지가 이 파일에 모여 있다.
//   1. minimalChildEnv — daemon 자신의 환경(EPHEMERA_* 등 비밀 포함 가능)을
//      절대 상속하지 않고 PATH/HOME/LANG 세 값 + credential_env 하나만으로
//      자식 환경을 백지에서 재구성한다.
//   2. applyRlimits — NOFILE/CORE(+ de-privileged 일 때 NPROC)를 prlimit(2)
//      으로 강제해 폭주 자식을 억제한다(RLIMIT_AS 는 의도적으로 안 건다, 이유는
//      상단 주석 참고).
//   3. root 로 daemon 이 뜨면 spec.Cred(EPHEMERA_MCP_STDIO_USER, 기본
//      "nobody")로 setuid 하고, ensureScratchDir 이 만든 per-server scratch
//      디렉터리를 cwd+HOME 으로 쓴다.
//   4. Setpgid: true 로 자식이 자기 그룹의 leader 가 되고, terminate 는 그
//      pgid 전체를 SIGKILL 한다 — 단 reap 이후에는 pgid 가 재사용될 수 있어
//      고아 프로세스를 그룹 kill 하지 않는다(재활용 안전, terminate 는 항상
//      leader 가 살아있는 동안에만 호출됨).
// credential 은 registry.go 가 만든 StdioSpawnSpec.Env 를 통해 자식 프로세스
// 환경변수 하나로만 전달되고 Args(argv)에는 절대 섞이지 않는다.

const (
	// stdioSpawnCooldown is the minimum interval between spawn attempts for one
	// backend, bounding a crash-looping child to one spawn per cooldown.
	stdioSpawnCooldown = 5 * time.Second
	// stdioCloseGrace is how long Close waits for a child to exit after stdin
	// EOF (the MCP stdio shutdown signal) before SIGKILLing its process group.
	stdioCloseGrace = 2 * time.Second
	// stdioHandshakeTimeout bounds the MCP initialize exchange. Generous on
	// purpose: a first spawn may install dependencies (npx-style servers).
	stdioHandshakeTimeout = 60 * time.Second
	// stdioWriteTimeout bounds one stdin line write. A child that has not
	// drained the pipe for this long is wedged and gets killed.
	stdioWriteTimeout = 30 * time.Second
	// stdioMaxLine caps one stdout JSON-RPC line (mirrors the SSE event cap).
	stdioMaxLine = 4 * 1024 * 1024
	// stdioStderrTail / stdioStderrLineMax bound the stderr kept for error
	// reports (the health endpoint's "error" field).
	stdioStderrTail    = 4
	stdioStderrLineMax = 512
)

// Child rlimits, applied via prlimit(2) right after spawn. Fixed in v0.6.4 (no
// knobs): generous enough for node/python MCP servers, tight enough to stop
// runaway children. Two limits are deliberately conditional or absent:
//   - NPROC counts every process AND thread of the uid system-wide, so it is
//     only applied when the child runs as the dedicated unprivileged user;
//     clamping a shared uid (daemon not root) can kill the child at startup
//     because of unrelated load while isolating nothing.
//   - RLIMIT_AS is not set at all: modern runtimes reserve huge virtual ranges
//     up front (V8 cages, JVM, sanitizer shadow memory) and an AS cap kills
//     them at startup while doing nothing about real memory use — RSS
//     containment is a cgroup job and out of scope here.
const (
	stdioRlimitNOFILE = 256
	stdioRlimitNPROC  = 512
)

// defaultStdioUser is the unprivileged account stdio children run as when the
// daemon is root and no override is configured.
const defaultStdioUser = "nobody"

// StdioSpawnSpec is everything needed to exec one stdio backend, fully
// resolved at registry-build time.
type StdioSpawnSpec struct {
	Command string              // absolute path (LookPath-resolved in NewRegistry)
	Args    []string            // argument vector
	Env     map[string]string   // extra child env (credential_env → token)
	Cred    *syscall.Credential // run-as user; nil = no privilege drop (daemon not root)
	Dir     string              // per-server scratch dir, used as cwd and HOME
}

// StdioBackend runs one backend MCP server as a local host subprocess and
// speaks newline-delimited JSON-RPC over its stdin/stdout (the MCP stdio
// transport). The child is spawned lazily on first use, de-privileged and
// rlimited, restarted on demand after a crash (rate-bound by a cooldown), and
// torn down by Close.
// 학습 주석: HTTPBackend(backend.go)와 마찬가지로 Backend interface 를 구현해
// gateway.go 는 stdio 인지 http 인지 구분하지 않는다. 차이는 네트워크 요청 대신
// 자식 프로세스의 stdin/stdout 으로 JSON-RPC 를 주고받는다는 점뿐이다.
type StdioBackend struct {
	id        string
	namespace string
	spec      StdioSpawnSpec

	// Test seams; production keeps the stdio* constants.
	spawnCooldown time.Duration
	closeGrace    time.Duration

	idCounter  atomic.Int64
	warnNoDrop sync.Once

	mu        sync.Mutex // guards proc, lastSpawn, lastExit, closed
	proc      *stdioProc // live (or starting) child; nil between lives
	lastSpawn time.Time  // cooldown anchor; failed attempts count too
	lastExit  error      // why the previous child died (for cooldown errors)
	closed    bool       // Close called; no further spawns
}

// stdioProc is one live child process: its pipes, in-flight requests, and
// death machinery. It never outlives its StdioBackend.
type stdioProc struct {
	serverID string
	cmd      *exec.Cmd
	stdin    *os.File // parent write end
	stdout   *os.File // parent read end
	stderr   *os.File // parent read end

	writeMu   sync.Mutex // serializes stdin line writes
	stdinOnce sync.Once  // stdin closes exactly once (graceful EOF / cleanup)

	pendingMu     sync.Mutex
	pending       map[int64]chan rpcResponse // in-flight requests by id
	pendingClosed bool                       // set on death; no further registrations

	causeMu sync.Mutex
	cause   error // first explicit failure cause, if any (nil = exit status speaks)

	ready      chan struct{} // closed once the MCP handshake finished; see readyErr
	readyErr   error         // handshake failure; written happens-before close(ready)
	dead       chan struct{} // closed once the child is reaped; exitErr is then valid
	exitErr    error
	stderrDone chan struct{} // closed when the stderr drain finished

	stderrTail *tailBuffer
}

var (
	_ Backend   = (*StdioBackend)(nil)
	_ io.Closer = (*StdioBackend)(nil)
)

// NewStdioBackend builds a StdioBackend. namespace prefixes the backend's tool
// names in the aggregated catalog; spec must carry an absolute command path.
func NewStdioBackend(id, namespace string, spec StdioSpawnSpec) *StdioBackend {
	if namespace == "" {
		namespace = id
	}
	return &StdioBackend{
		id:            id,
		namespace:     namespace,
		spec:          spec,
		spawnCooldown: stdioSpawnCooldown,
		closeGrace:    stdioCloseGrace,
	}
}

func (b *StdioBackend) ID() string        { return b.id }
func (b *StdioBackend) Namespace() string { return b.namespace }

func (b *StdioBackend) nextID() int64 { return b.idCounter.Add(1) }

// lookupStdioUser resolves a user name to the credential stdio children are
// spawned with. Supplementary groups are cleared (empty Groups, NoSetGroups
// unset).
func lookupStdioUser(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("gid %q: %w", u.Gid, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// ensureStarted returns a handshaken child, spawning one if needed. The wait
// honors ctx, but the spawn itself does not: a caller that gives up (e.g. a 5s
// health probe racing a slow first start) must not kill a child that is still
// coming up — it just returns, and the spawn converges in the background.
// 학습 주석: 이 함수가 lazy spawn 의 진입점 — 첫 tools/list 나 health 호출에서만
// 실제 자식이 뜬다. spawnCooldown 체크가 crash loop 를 5초 간격으로 억제한다.
func (b *StdioBackend) ensureStarted(ctx context.Context) (*stdioProc, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("backend %s: closed", b.id)
	}
	p := b.proc
	if p == nil {
		if since := time.Since(b.lastSpawn); since < b.spawnCooldown {
			lastExit := b.lastExit
			b.mu.Unlock()
			msg := fmt.Sprintf("backend %s: respawn cooling down (%s left)", b.id, (b.spawnCooldown - since).Round(time.Millisecond))
			if lastExit != nil {
				return nil, fmt.Errorf("%s; last: %v", msg, lastExit)
			}
			return nil, errors.New(msg)
		}
		b.lastSpawn = time.Now()
		var err error
		p, err = b.spawnLocked()
		if err != nil {
			b.lastExit = err
			b.mu.Unlock()
			return nil, err
		}
		b.proc = p
	}
	b.mu.Unlock()

	select {
	case <-p.ready:
		if p.readyErr != nil {
			return nil, p.readyErr
		}
		return p, nil
	case <-p.dead:
		return nil, p.exitError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// spawnLocked starts the child and its lifecycle goroutines. Caller holds b.mu.
// 학습 주석: cmd.Env = minimalChildEnv(...) 로 daemon 환경 백지화, SysProcAttr 에
// Setpgid+Credential 로 그룹 격리와 권한 하강을 동시에 설정한다 — 이 두 줄이
// v0.6.4 하드닝의 물리적 실체다.
func (b *StdioBackend) spawnLocked() (*stdioProc, error) {
	if err := b.ensureScratchDir(); err != nil {
		return nil, err
	}
	if b.spec.Cred == nil && os.Geteuid() != 0 {
		b.warnNoDrop.Do(func() {
			slog.Warn("mcp stdio backend runs as the daemon's own user (daemon is not root; cannot drop privileges)", "server", b.id)
		})
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("backend %s: pipe: %w", b.id, err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, fmt.Errorf("backend %s: pipe: %w", b.id, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("backend %s: pipe: %w", b.id, err)
	}
	cmd := exec.Command(b.spec.Command, b.spec.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	cmd.Dir = b.spec.Dir
	cmd.Env = minimalChildEnv(b.spec)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,            // own group → group SIGKILL reaps helper processes too
		Pdeathsig:  syscall.SIGKILL, // daemon-crash insurance; Registry.Close is the primary path
		Credential: b.spec.Cred,     // nil → run as the daemon's own user
	}
	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("backend %s: spawn %s: %w", b.id, b.spec.Command, err)
	}
	// The child owns its pipe ends now; keep only ours so EOFs propagate.
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	p := &stdioProc{
		serverID:   b.id,
		cmd:        cmd,
		stdin:      stdinW,
		stdout:     stdoutR,
		stderr:     stderrR,
		pending:    map[int64]chan rpcResponse{},
		ready:      make(chan struct{}),
		dead:       make(chan struct{}),
		stderrDone: make(chan struct{}),
		stderrTail: &tailBuffer{max: stdioStderrTail},
	}
	if err := applyRlimits(cmd.Process.Pid, b.spec.Cred != nil); err != nil {
		// The limits are part of this feature's guarantee: fail closed rather
		// than run an unconfined child.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("backend %s: apply rlimits: %w", b.id, err)
	}
	slog.Warn("mcp stdio backend spawned", "server", b.id, "command", b.spec.Command, "pid", cmd.Process.Pid, "user", stdioUserLabel(b.spec.Cred))
	go p.readLoop()
	go p.stderrLoop()
	go b.reap(p)
	go b.handshake(p)
	return p, nil
}

// ensureScratchDir creates the per-server scratch dir (the child's cwd and
// HOME, kept across restarts so npx/uvx-style servers can cache). The parent
// chain is created 0755 — the de-privileged child must be able to TRAVERSE
// into its own dir, and a plain MkdirAll would propagate the leaf's 0700 to
// every created parent (which, combined with a scratch base under a 0700/0750
// home directory, made the child's post-setuid chdir fail with EACCES in the
// v0.6.4 e2e). Only the leaf is 0700, chmodded + chowned every spawn so a
// changed EPHEMERA_MCP_STDIO_USER self-heals. The Lstat guard refuses a leaf
// that is not a real directory (e.g. a symlink planted in a world-writable
// base like the os.TempDir() library default) before root chowns it.
// 학습 주석: leaf 디렉터리만 0700 + chown 하고 조상 디렉터리는 0755 로 열어,
// setuid 된 자식이 자기 소유가 아닌 상위 경로도 traverse 할 수 있게 한다 — root
// 소유 /var/lib 아래 두는 이유(mcp_gateway.go 의 mcpStdioScratchBase 주석 참고).
func (b *StdioBackend) ensureScratchDir() error {
	// Note which ancestors are about to be created, so each can be chmodded
	// 0755 explicitly afterwards — immune to the daemon's umask, and without
	// ever touching the modes of pre-existing (system) directories.
	parent := filepath.Dir(b.spec.Dir)
	var created []string
	for p := parent; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		created = append(created, p)
		if p == filepath.Dir(p) { // reached the filesystem root
			break
		}
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("backend %s: scratch base: %w", b.id, err)
	}
	for _, p := range created {
		if err := os.Chmod(p, 0o755); err != nil {
			return fmt.Errorf("backend %s: chmod scratch base: %w", b.id, err)
		}
	}
	if err := os.MkdirAll(b.spec.Dir, 0o700); err != nil {
		return fmt.Errorf("backend %s: scratch dir: %w", b.id, err)
	}
	fi, err := os.Lstat(b.spec.Dir)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("backend %s: scratch dir %s is not a directory", b.id, b.spec.Dir)
	}
	if err := os.Chmod(b.spec.Dir, 0o700); err != nil {
		return fmt.Errorf("backend %s: chmod scratch dir: %w", b.id, err)
	}
	if b.spec.Cred != nil {
		if err := os.Chown(b.spec.Dir, int(b.spec.Cred.Uid), int(b.spec.Cred.Gid)); err != nil {
			return fmt.Errorf("backend %s: chown scratch dir: %w", b.id, err)
		}
	}
	return nil
}

// handshake drives the MCP initialize exchange on its own clock, deliberately
// decoupled from any caller's ctx (see ensureStarted).
// 학습 주석: 이 핸드셰이크는 gateway.go 의 handleInitialize(goose 쪽 세션)와
// 완전히 별개다 — backend 자식과의 MCP initialize 는 백그라운드 goroutine 에서
// 독립적으로 진행된다.
func (b *StdioBackend) handshake(p *stdioProc) {
	ctx, cancel := context.WithTimeout(context.Background(), stdioHandshakeTimeout)
	defer cancel()
	params := initializeParams{
		ProtocolVersion: defaultProtocolVersion,
		Capabilities:    json.RawMessage(`{}`),
		ClientInfo:      json.RawMessage(`{"name":"` + gatewayServerName + `","version":"` + gatewayServerVersion + `"}`),
	}
	resp, err := p.call(ctx, b.nextID(), "initialize", mustMarshal(params))
	if err == nil && resp.Error != nil {
		err = errors.New(resp.Error.Message)
	}
	if err != nil {
		p.readyErr = fmt.Errorf("initialize %s: %w", b.id, err)
		close(p.ready)
		p.terminate(p.readyErr)
		return
	}
	// Per spec, follow initialize with the initialized notification before any
	// other request. A backend that ignores it still works.
	_ = p.notify("notifications/initialized", json.RawMessage(`{}`))
	close(p.ready)
}

// reap waits for the child (the sole cmd.Wait caller), then completes the
// death: records the cause, fails in-flight calls, detaches the proc so a
// later call can respawn, and closes dead. Helper processes a child orphans by
// exiting on its own are not group-killed here: after Wait the pgid may be
// recycled, so a kill could hit an innocent process. Kill paths (terminate)
// only ever run while the leader is alive, which pins the pgid.
// 학습 주석: "pgid 재활용 안전"의 근거 코드 — Wait() 이후에는 group kill 을 시도
// 하지 않는다(pgid 가 커널에 의해 다른 프로세스에 재할당될 수 있으므로). kill
// 경로(terminate)는 leader 가 아직 살아있다고 보장되는 시점에서만 호출된다.
func (b *StdioBackend) reap(p *stdioProc) {
	werr := p.cmd.Wait()
	p.closeStdin() // release the write end; harmless if already closed
	select {       // let the stderr drain finish so the tail is complete...
	case <-p.stderrDone:
	case <-time.After(500 * time.Millisecond): // ...unless an orphan holds the pipe
	}
	msg := fmt.Sprintf("backend %s: process exited", b.id)
	if werr != nil {
		msg += ": " + werr.Error()
	}
	if tail := p.stderrTail.String(); tail != "" {
		msg += " (stderr: " + tail + ")"
	}
	exitErr := errors.New(msg)
	p.causeMu.Lock()
	if p.cause != nil {
		exitErr = fmt.Errorf("%s; %v", msg, p.cause)
	}
	p.causeMu.Unlock()
	p.exitErr = exitErr // written happens-before failPending and close(dead)

	p.failPending()
	b.detach(p, exitErr)
	close(p.dead)
	slog.Warn("mcp stdio backend process exited", "server", b.id, "err", exitErr)
}

// detach clears b.proc if it still points at p, so the next call respawns.
func (b *StdioBackend) detach(p *stdioProc, exitErr error) {
	b.mu.Lock()
	if b.proc == p {
		b.proc = nil
		b.lastExit = exitErr
	}
	b.mu.Unlock()
}

// Close shuts the backend down for good: no further spawns, and the running
// child (if any) is asked to exit via stdin EOF (the MCP stdio convention),
// then SIGKILLed with its whole process group after a short grace period.
// 학습 주석: registry.go 의 Registry.Close 가 모든 backend 의 Close 를 병렬로
// 호출한다 — cmd/goose-daemon/mcp_gateway.go 의 stopMCPGateway 가 daemon 종료
// 시 그 경로를 탄다.
func (b *StdioBackend) Close() error {
	b.mu.Lock()
	b.closed = true
	p := b.proc
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	p.closeStdin()
	select {
	case <-p.dead:
		return nil
	case <-time.After(b.closeGrace):
	}
	p.terminate(errors.New("closed"))
	select {
	case <-p.dead:
	case <-time.After(3 * time.Second):
		// SIGKILL cannot be ignored; only an unkillable (D-state) child gets
		// here. Give up rather than block daemon shutdown.
	}
	return nil
}

// do runs one JSON-RPC request against the lazily started child.
func (b *StdioBackend) do(ctx context.Context, method string, params json.RawMessage) (rpcResponse, error) {
	p, err := b.ensureStarted(ctx)
	if err != nil {
		return rpcResponse{}, err
	}
	return p.call(ctx, b.nextID(), method, params)
}

// ListTools returns the backend's advertised tools (un-namespaced).
func (b *StdioBackend) ListTools(ctx context.Context) ([]Tool, error) {
	resp, err := b.do(ctx, "tools/list", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/list %s: %s", b.id, resp.Error.Message)
	}
	var out toolsListResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("tools/list %s: bad result: %w", b.id, err)
	}
	return out.Tools, nil
}

// CallTool invokes one tool by its un-namespaced name and returns the raw MCP
// result object (content/isError) for the gateway to relay verbatim.
func (b *StdioBackend) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	resp, err := b.do(ctx, "tools/call", mustMarshal(toolsCallParams{Name: tool, Arguments: args}))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/call %s/%s: %s", b.id, tool, resp.Error.Message)
	}
	return resp.Result, nil
}

// ListResources returns the backend's advertised resources (un-namespaced URIs).
func (b *StdioBackend) ListResources(ctx context.Context) ([]Resource, error) {
	resp, err := b.do(ctx, "resources/list", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("resources/list %s: %s", b.id, resp.Error.Message)
	}
	var out resourcesListResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("resources/list %s: bad result: %w", b.id, err)
	}
	return out.Resources, nil
}

// ReadResource reads one resource by its un-namespaced URI and returns the raw
// MCP result object (contents) for the gateway to relay verbatim.
func (b *StdioBackend) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	resp, err := b.do(ctx, "resources/read", mustMarshal(resourcesReadParams{URI: uri}))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("resources/read %s/%s: %s", b.id, uri, resp.Error.Message)
	}
	return resp.Result, nil
}

// ListPrompts returns the backend's advertised prompts (un-namespaced names).
func (b *StdioBackend) ListPrompts(ctx context.Context) ([]Prompt, error) {
	resp, err := b.do(ctx, "prompts/list", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("prompts/list %s: %s", b.id, resp.Error.Message)
	}
	var out promptsListResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("prompts/list %s: bad result: %w", b.id, err)
	}
	return out.Prompts, nil
}

// GetPrompt fetches one prompt by its un-namespaced name and returns the raw
// MCP result object (messages) for the gateway to relay verbatim.
func (b *StdioBackend) GetPrompt(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	resp, err := b.do(ctx, "prompts/get", mustMarshal(promptsGetParams{Name: name, Arguments: args}))
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("prompts/get %s/%s: %s", b.id, name, resp.Error.Message)
	}
	return resp.Result, nil
}

// Health verifies the child is up (spawning it if needed) and answering: a
// ping round-trip, where a method-not-found reply still counts as alive (ping
// is optional for servers).
func (b *StdioBackend) Health(ctx context.Context) error {
	resp, err := b.do(ctx, "ping", json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	if resp.Error != nil && resp.Error.Code != codeMethodNotFound {
		return fmt.Errorf("ping %s: %s", b.id, resp.Error.Message)
	}
	return nil
}

// call sends one request and waits for its response by id. ctx expiry only
// abandons the wait (the entry is dropped; a late response is discarded) — it
// never kills the child.
func (p *stdioProc) call(ctx context.Context, id int64, method string, params json.RawMessage) (rpcResponse, error) {
	ch := make(chan rpcResponse, 1)
	p.pendingMu.Lock()
	if p.pendingClosed {
		p.pendingMu.Unlock()
		return rpcResponse{}, p.exitError()
	}
	p.pending[id] = ch
	p.pendingMu.Unlock()
	defer p.unregister(id)

	msg := rpcRequest{JSONRPC: jsonRPCVersion, ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: params}
	body, _ := json.Marshal(msg)
	if err := p.send(body); err != nil {
		return rpcResponse{}, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return rpcResponse{}, p.exitError()
		}
		return resp, nil
	case <-ctx.Done():
		return rpcResponse{}, ctx.Err()
	case <-p.dead:
		return rpcResponse{}, p.exitError()
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (p *stdioProc) notify(method string, params json.RawMessage) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": jsonRPCVersion, "method": method, "params": params})
	return p.send(body)
}

// send writes one newline-terminated JSON-RPC line to the child's stdin. A
// write failure (or a child that stops draining the pipe) is fatal to the proc.
func (p *stdioProc) send(line []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.stdin.SetWriteDeadline(time.Now().Add(stdioWriteTimeout))
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		err = fmt.Errorf("write to %s: %w", p.serverID, err)
		p.terminate(err)
		return err
	}
	return nil
}

// readLoop routes the child's stdout: responses are matched to pending
// requests by id; server-initiated requests get a method-not-found reply so
// the child does not hang; notifications and junk lines are dropped. EOF or a
// scan error ends the proc (a child that closed stdout is protocol-broken).
func (p *stdioProc) readLoop() {
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), stdioMaxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			// Spec says stdout is MCP-only, but tolerate banners and such.
			slog.Debug("mcp stdio: ignoring non-JSON stdout line", "server", p.serverID)
			continue
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 && string(msg.ID) != "null" {
				reply, _ := json.Marshal(newError(msg.ID, codeMethodNotFound, "ephemera gateway accepts no server requests"))
				_ = p.send(reply)
			}
			continue
		}
		if len(msg.Result) == 0 && msg.Error == nil {
			continue // not a response shape
		}
		var id int64
		if err := json.Unmarshal(msg.ID, &id); err != nil {
			continue // our requests always carry numeric ids
		}
		p.deliver(id, rpcResponse{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: msg.Result, Error: msg.Error})
	}
	if err := sc.Err(); err != nil {
		p.terminate(fmt.Errorf("read stdout of %s: %w", p.serverID, err))
	} else {
		p.terminate(nil) // plain EOF: let the exit status speak
	}
	_ = p.stdout.Close()
}

// stderrLoop drains the child's stderr into debug logs and the diagnostic tail.
func (p *stdioProc) stderrLoop() {
	defer close(p.stderrDone)
	sc := bufio.NewScanner(p.stderr)
	sc.Buffer(make([]byte, 0, 4*1024), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > stdioStderrLineMax {
			line = line[:stdioStderrLineMax]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.stderrTail.add(line)
		slog.Debug("mcp stdio stderr", "server", p.serverID, "line", line)
	}
	_ = p.stderr.Close()
}

// deliver hands a response to the caller waiting on its id, if still there.
func (p *stdioProc) deliver(id int64, resp rpcResponse) {
	p.pendingMu.Lock()
	ch, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
	if ok {
		ch <- resp // buffered; never blocks
	}
}

// unregister drops a pending entry (caller gave up or already got its answer).
func (p *stdioProc) unregister(id int64) {
	p.pendingMu.Lock()
	if !p.pendingClosed {
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
}

// failPending closes every in-flight wait and blocks new registrations.
func (p *stdioProc) failPending() {
	p.pendingMu.Lock()
	p.pendingClosed = true
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
}

// terminate records the first failure cause (nil = let the exit status speak)
// and SIGKILLs the child's process group. Idempotent; reap completes the death.
func (p *stdioProc) terminate(cause error) {
	if cause != nil {
		p.causeMu.Lock()
		if p.cause == nil {
			p.cause = cause
		}
		p.causeMu.Unlock()
	}
	if p.cmd.Process != nil {
		// Setpgid at spawn → the child leads its own group (pgid == its pid).
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// closeStdin closes the child's stdin write end exactly once. Per the MCP
// stdio transport, EOF on stdin asks the server to exit.
func (p *stdioProc) closeStdin() {
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
}

// exitError returns why the child died. Only called after the proc is dead
// (dead closed, or pending failed), which happens-after exitErr is written.
func (p *stdioProc) exitError() error { return p.exitErr }

// applyRlimits clamps a freshly spawned child via prlimit(2). dropped reports
// whether the child runs as the dedicated unprivileged user (gates NPROC, see
// the constants above). There is a tiny window between Start and the limits
// taking effect; accepted — the command is operator-declared, and the limits
// guard against runaway behavior, not malice.
// 학습 주석: NPROC 은 dropped(전용 unprivileged user 로 실행 중)일 때만 추가된다
// — daemon 이 root 가 아니면 공유 uid 의 전역 프로세스 수를 제한해 무관한 부하로
// 자식이 시작부터 죽는 것을 피한다(상단 const 블록 주석 참고).
func applyRlimits(pid int, dropped bool) error {
	limits := []struct {
		resource int
		limit    uint64
	}{
		{unix.RLIMIT_NOFILE, stdioRlimitNOFILE},
		{unix.RLIMIT_CORE, 0}, // no core dumps of a process whose env holds a token
	}
	if dropped {
		limits = append(limits, struct {
			resource int
			limit    uint64
		}{unix.RLIMIT_NPROC, stdioRlimitNPROC})
	}
	for _, l := range limits {
		rl := &unix.Rlimit{Cur: l.limit, Max: l.limit}
		if err := unix.Prlimit(pid, l.resource, rl, nil); err != nil {
			return fmt.Errorf("prlimit resource %d: %w", l.resource, err)
		}
	}
	return nil
}

// minimalChildEnv builds the child environment from scratch: the daemon's own
// environment (EPHEMERA_* config, possibly key material) is never inherited.
// 학습 주석: 개요에서 말한 "child env 백지 구성"이 바로 이 함수다. spec.Env 는
// registry.go 의 NewRegistry 가 credential_env 하나만 채워 넣은 map — 즉 credential
// 은 여기서 딱 한 줄의 env 항목으로만 자식에 들어간다.
func minimalChildEnv(spec StdioSpawnSpec) []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + spec.Dir,
		"LANG=C.UTF-8",
	}
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// stdioUserLabel renders the run-as identity for logs.
func stdioUserLabel(c *syscall.Credential) string {
	if c == nil {
		return "daemon"
	}
	return fmt.Sprintf("uid=%d", c.Uid)
}

// tailBuffer keeps the last max lines written to it (stderr diagnostics).
type tailBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
	t.mu.Unlock()
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}
