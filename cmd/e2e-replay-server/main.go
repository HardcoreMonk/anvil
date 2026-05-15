package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultAddr              = "127.0.0.1:8787"
	defaultFullKVMRecording  = "docs/replays/full-kvm-e2e.txt"
	defaultIronClawRecording = "/tmp/anvil-real-e2e-recording.typescript"
)

var (
	ansiCSI       = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	ansiCharset   = regexp.MustCompile(`\x1b\([A-Za-z0-9]`)
	googleAPIKey  = regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`)
	bearerToken   = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`)
	agentToken    = regexp.MustCompile(`("agent_token"\s*:\s*")[^"]+(")`)
	secretSetting = regexp.MustCompile(`(?i)(API_KEY|SECRET|TOKEN|PASSWORD)(["']?\s*[:=]\s*["']?)[^\s"',}]+`)
)

type replayEntry struct {
	ID          string
	Title       string
	Description string
	Path        string
}

type playlistResponse struct {
	GeneratedAt string         `json:"generated_at"`
	Items       []playlistItem `json:"items"`
}

type playlistItem struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Available   bool     `json:"available"`
	LineCount   int      `json:"line_count"`
	Summary     []string `json:"summary,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type recordingResponse struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Source      string   `json:"source"`
	LineCount   int      `json:"line_count"`
	GeneratedAt string   `json:"generated_at"`
	Summary     []string `json:"summary"`
	Lines       []string `json:"lines"`
}

func main() {
	addr := flag.String("addr", defaultAddr, "bind address")
	fullKVMRecording := flag.String("full-kvm-recording", defaultFullKVMRecording, "full KVM E2E replay text file")
	ironClawRecording := flag.String("recording", defaultIronClawRecording, "IronClaw typescript recording file")
	flag.Parse()

	entries := defaultPlaylist(*fullKVMRecording, *ironClawRecording)
	server := &http.Server{
		Addr:              *addr,
		Handler:           newMux(entries),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("anvil E2E replay server listening on http://%s", *addr)
	for _, entry := range entries {
		log.Printf("replay %s: %s", entry.ID, entry.Path)
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func defaultPlaylist(fullKVMRecording, ironClawRecording string) []replayEntry {
	return []replayEntry{
		{
			ID:          "full-kvm-e2e",
			Title:       "Full KVM E2E",
			Description: "58-step daemon, VM, snapshot, COW, proxy, Goosetown, and watchdog replay",
			Path:        fullKVMRecording,
		},
		{
			ID:          "ironclaw-e2e",
			Title:       "IronClaw MCP E2E",
			Description: "Sanitized IronClaw terminal recording when /tmp/anvil-real-e2e-recording.typescript exists",
			Path:        ironClawRecording,
		},
	}
}

func newMux(entries []replayEntry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/playlist", playlistHandler(entries))
	mux.HandleFunc("/api/recording", recordingHandler(entries))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return noStore(mux)
}

func playlistHandler(entries []replayEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		resp := playlistResponse{
			GeneratedAt: time.Now().Format(time.RFC3339),
			Items:       make([]playlistItem, 0, len(entries)),
		}
		for _, entry := range entries {
			item := playlistItem{
				ID:          entry.ID,
				Title:       entry.Title,
				Description: entry.Description,
				Source:      entry.Path,
			}
			lines, err := readReplayLines(entry)
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Available = true
				item.LineCount = len(lines)
				item.Summary = summarizeRecording(lines)
			}
			resp.Items = append(resp.Items, item)
		}
		writeJSON(w, resp)
	}
}

func recordingHandler(entries []replayEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, ok := selectReplay(entries, r.URL.Query().Get("id"))
		if !ok {
			http.Error(w, "replay not found", http.StatusNotFound)
			return
		}

		lines, err := readReplayLines(entry)
		if err != nil {
			http.Error(w, fmt.Sprintf("read recording: %v", err), http.StatusNotFound)
			return
		}

		resp := recordingResponse{
			ID:          entry.ID,
			Title:       entry.Title,
			Source:      filepath.Base(entry.Path),
			LineCount:   len(lines),
			GeneratedAt: time.Now().Format(time.RFC3339),
			Summary:     summarizeRecording(lines),
			Lines:       lines,
		}
		writeJSON(w, resp)
	}
}

func selectReplay(entries []replayEntry, id string) (replayEntry, bool) {
	if id != "" {
		for _, entry := range entries {
			if entry.ID == id {
				return entry, true
			}
		}
		return replayEntry{}, false
	}
	for _, entry := range entries {
		if _, err := os.Stat(entry.Path); err == nil {
			return entry, true
		}
	}
	if len(entries) == 0 {
		return replayEntry{}, false
	}
	return entries[0], true
}

func readReplayLines(entry replayEntry) ([]string, error) {
	data, err := os.ReadFile(entry.Path)
	if err != nil {
		return nil, err
	}
	return sanitizeRecording(string(data)), nil
}

func sanitizeRecording(input string) []string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	input = ansiCSI.ReplaceAllString(input, "")
	input = ansiCharset.ReplaceAllString(input, "")

	var out []string
	lastBlank := false
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.HasPrefix(line, "Script started on ") || strings.HasPrefix(line, "Script done on ") {
			continue
		}
		line = redactSecrets(line)
		if line == "" {
			if len(out) == 0 || lastBlank {
				continue
			}
			lastBlank = true
			out = append(out, line)
			continue
		}
		lastBlank = false
		out = append(out, line)
	}

	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func redactSecrets(line string) string {
	line = googleAPIKey.ReplaceAllString(line, "<redacted-google-api-key>")
	line = bearerToken.ReplaceAllString(line, "Bearer <redacted>")
	line = agentToken.ReplaceAllString(line, `${1}<redacted>${2}`)
	line = secretSetting.ReplaceAllString(line, `${1}${2}<redacted>`)
	return line
}

func summarizeRecording(lines []string) []string {
	joined := strings.Join(lines, "\n")
	checks := []struct {
		match string
		label string
	}{
		{"anvil_anvil_spawn_vm", "anvil_spawn_vm"},
		{"anvil_anvil_copy_in", "anvil_copy_in"},
		{"anvil_anvil_copy_out", "anvil_copy_out"},
		{"anvil_anvil_run_task", "anvil_run_task"},
		{"anvil_anvil_get_vm_health", "anvil_get_vm_health"},
		{"anvil_anvil_stop_vm", "anvil_stop_vm"},
		{"anvil_anvil_delete_vm", "anvil_delete_vm"},
		{"anvil-smoke-ok", "task_output=anvil-smoke-ok"},
		{"copy_match", "copy_match=true"},
		{"ironclaw-anvil-real-e2e", "copy_match=true"},
		{"snapshot", "snapshot"},
		{"COW", "COW"},
		{"flock", "Goosetown"},
		{"Town Wall", "Goosetown"},
		{"agent_token", "agent_token redaction"},
		{"Watchdog", "watchdog"},
		{"All test steps passed", "all steps passed"},
	}

	var summary []string
	seen := map[string]bool{}
	for _, check := range checks {
		if strings.Contains(joined, check.match) && !seen[check.label] {
			summary = append(summary, check.label)
			seen[check.label] = true
		}
	}
	return summary
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode JSON response: %v", err)
	}
}

func indexHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

const indexHTML = `<!doctype html>
<html lang="ko">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>anvil E2E Replay</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #0d1218;
      --panel: #101820;
      --bar: #1c2632;
      --border: #374655;
      --text: #dae6f1;
      --muted: #899aab;
      --cyan: #5dd5ff;
      --green: #50dc8c;
      --yellow: #f5c95c;
      --red: #ff6e6e;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      background: var(--bg);
      color: var(--text);
      font-family: Inter, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    main {
      min-height: 100vh;
      display: grid;
      grid-template-rows: auto 1fr;
    }
    .topbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 18px;
      padding: 14px 18px;
      border-bottom: 1px solid var(--border);
      background: #111923;
    }
    .title {
      display: flex;
      flex-wrap: wrap;
      align-items: baseline;
      gap: 10px;
      min-width: 0;
    }
    h1 {
      margin: 0;
      font-size: 18px;
      font-weight: 700;
      letter-spacing: 0;
    }
    .subtitle {
      color: var(--muted);
      font-size: 13px;
      overflow-wrap: anywhere;
    }
    .controls {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      flex-wrap: wrap;
      gap: 8px;
    }
    button, select {
      border: 1px solid var(--border);
      background: #172231;
      color: var(--text);
      border-radius: 6px;
      padding: 8px 10px;
      font: inherit;
      font-size: 13px;
    }
    button {
      cursor: pointer;
      min-width: 70px;
    }
    button:hover, select:hover { border-color: #5a6d80; }
    .status {
      color: var(--muted);
      font-size: 13px;
      min-width: 120px;
      text-align: right;
    }
    .workspace {
      padding: 18px;
      min-height: 0;
    }
    .terminal {
      height: calc(100vh - 91px);
      min-height: 420px;
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--panel);
      overflow: hidden;
      display: grid;
      grid-template-rows: auto 1fr auto;
    }
    .terminal-bar {
      height: 44px;
      display: flex;
      align-items: center;
      gap: 9px;
      padding: 0 16px;
      background: var(--bar);
      border-bottom: 1px solid #213040;
      color: var(--muted);
      font-family: "JetBrains Mono", "DejaVu Sans Mono", Consolas, monospace;
      font-size: 13px;
    }
    .dot {
      width: 12px;
      height: 12px;
      border-radius: 50%;
      display: inline-block;
    }
    .red { background: #ff5f57; }
    .amber { background: #ffbd2e; }
    .green-dot { background: #28c840; }
    .terminal-title { margin-left: 8px; }
    .screen {
      overflow: auto;
      padding: 18px 22px;
      font-family: "JetBrains Mono", "DejaVu Sans Mono", Consolas, monospace;
      font-size: 14px;
      line-height: 1.55;
      white-space: pre-wrap;
      word-break: break-word;
    }
    .line.command { color: var(--cyan); }
    .line.ok { color: var(--green); }
    .line.tool { color: var(--yellow); }
    .line.error { color: var(--red); }
    .line.muted { color: var(--muted); }
    .footer {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      padding: 10px 16px;
      border-top: 1px solid #213040;
      color: var(--muted);
      font-family: "JetBrains Mono", "DejaVu Sans Mono", Consolas, monospace;
      font-size: 12px;
    }
    @media (max-width: 760px) {
      .topbar { align-items: stretch; flex-direction: column; }
      .controls { justify-content: flex-start; }
      .status { text-align: left; }
      .workspace { padding: 10px; }
      .terminal { height: calc(100vh - 170px); }
    }
  </style>
</head>
<body>
  <main>
    <header class="topbar">
      <div class="title">
        <h1>anvil E2E Replay</h1>
        <span class="subtitle" id="source">loading playlist...</span>
      </div>
      <div class="controls">
        <select id="playlist" aria-label="Replay playlist"></select>
        <button id="play">Play</button>
        <button id="pause">Pause</button>
        <button id="restart">Restart</button>
        <select id="speed" aria-label="Playback speed">
          <option value="500">1x</option>
          <option value="250" selected>2x</option>
          <option value="120">4x</option>
          <option value="60">8x</option>
        </select>
        <span class="status" id="status">0 / 0</span>
      </div>
    </header>
    <section class="workspace">
      <div class="terminal">
        <div class="terminal-bar">
          <span class="dot red"></span>
          <span class="dot amber"></span>
          <span class="dot green-dot"></span>
          <span class="terminal-title" id="terminal-title">anvil replay</span>
        </div>
        <div class="screen" id="screen"></div>
        <div class="footer">
          <span id="summary">recordings are sanitized server-side</span>
          <span id="generated">playlist</span>
        </div>
      </div>
    </section>
  </main>
  <script>
    const screen = document.getElementById('screen');
    const statusEl = document.getElementById('status');
    const sourceEl = document.getElementById('source');
    const summaryEl = document.getElementById('summary');
    const speedEl = document.getElementById('speed');
    const playlistEl = document.getElementById('playlist');
    const titleEl = document.getElementById('terminal-title');
    const generatedEl = document.getElementById('generated');
    let lines = [];
    let cursor = 0;
    let timer = null;
    let playlist = [];

    function classFor(line) {
      const text = line.trim();
      if (text.startsWith('$')) return 'command';
      if (text.startsWith('PASS') || text.includes('All test steps passed') || text.includes('successfully') || text.includes('anvil-smoke-ok') || text.includes('status_code')) return 'ok';
      if (text.includes('Running anvil_') || text.includes('Thinking') || text.includes('Available tools')) return 'tool';
      if (text.includes('Error') || text.includes('failed') || text.includes('INVALID')) return 'error';
      if (text.includes('INFO') || text.startsWith('==')) return 'muted';
      return '';
    }

    function updateStatus() {
      statusEl.textContent = cursor + ' / ' + lines.length;
    }

    function appendLine(line) {
      const row = document.createElement('div');
      row.className = 'line ' + classFor(line);
      row.textContent = line || ' ';
      screen.appendChild(row);
      screen.scrollTop = screen.scrollHeight;
    }

    function step() {
      if (cursor >= lines.length) {
        pause();
        return;
      }
      appendLine(lines[cursor]);
      cursor += 1;
      updateStatus();
      timer = window.setTimeout(step, Number(speedEl.value));
    }

    function play() {
      if (!timer && lines.length > 0) step();
    }

    function pause() {
      if (timer) {
        window.clearTimeout(timer);
        timer = null;
      }
    }

    function restart() {
      pause();
      cursor = 0;
      screen.textContent = '';
      updateStatus();
      play();
    }

    function setUnavailable(item) {
      pause();
      lines = [];
      cursor = 0;
      screen.textContent = item.title + ' is unavailable.\n\n' + (item.error || 'recording file not found');
      sourceEl.textContent = item.source + ' · unavailable';
      titleEl.textContent = item.title;
      summaryEl.textContent = item.description || 'unavailable replay';
      updateStatus();
    }

    function loadReplay(id) {
      const item = playlist.find((entry) => entry.id === id);
      if (item && !item.available) {
        setUnavailable(item);
        return;
      }
      pause();
      cursor = 0;
      lines = [];
      screen.textContent = 'Loading replay...';
      updateStatus();
      fetch('/api/recording?id=' + encodeURIComponent(id))
        .then((res) => {
          if (!res.ok) throw new Error('HTTP ' + res.status);
          return res.json();
        })
        .then((data) => {
          lines = data.lines || [];
          cursor = 0;
          screen.textContent = '';
          sourceEl.textContent = data.source + ' · ' + data.line_count + ' lines';
          titleEl.textContent = data.title || data.id;
          summaryEl.textContent = (data.summary || []).join(' · ') || 'recording loaded';
          generatedEl.textContent = data.generated_at || '';
          updateStatus();
          play();
        })
        .catch((err) => {
          pause();
          lines = [];
          cursor = 0;
          sourceEl.textContent = 'recording unavailable';
          screen.textContent = 'Failed to load recording: ' + err.message;
          updateStatus();
        });
    }

    function renderPlaylist(data) {
      playlist = data.items || [];
      playlistEl.textContent = '';
      for (const item of playlist) {
        const option = document.createElement('option');
        option.value = item.id;
        option.textContent = item.available ? item.title : item.title + ' (missing)';
        playlistEl.appendChild(option);
      }
      generatedEl.textContent = data.generated_at || '';
      const first = playlist.find((item) => item.available) || playlist[0];
      if (first) {
        playlistEl.value = first.id;
        loadReplay(first.id);
      } else {
        sourceEl.textContent = 'no replay entries configured';
      }
    }

    document.getElementById('play').addEventListener('click', play);
    document.getElementById('pause').addEventListener('click', pause);
    document.getElementById('restart').addEventListener('click', restart);
    playlistEl.addEventListener('change', () => loadReplay(playlistEl.value));
    speedEl.addEventListener('change', () => {
      if (timer) {
        pause();
        play();
      }
    });

    fetch('/api/playlist')
      .then((res) => {
        if (!res.ok) throw new Error('HTTP ' + res.status);
        return res.json();
      })
      .then(renderPlaylist)
      .catch((err) => {
        sourceEl.textContent = 'playlist unavailable';
        screen.textContent = 'Failed to load playlist: ' + err.message;
      });
  </script>
</body>
</html>
`
