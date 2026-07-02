package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// auditMiddleware is the OUTER request wrapper (v0.4.1): it installs a
// request-scoped clientHolder that the INNER authMiddleware back-fills with the
// matched client name, wraps the ResponseWriter to capture status + bytes, runs
// the chain, then writes one audit record. Being outside auth, it records
// unauthorized (401) responses too (client "-"). No-op write when audit is
// disabled, but the wrapper still runs so the holder/recorder are always set.
func (cp *ControlPlane) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holder := &clientHolder{}
		r = r.WithContext(withClientHolder(r.Context(), holder))
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)

		if cp.audit == nil || !cp.audit.enabled {
			return
		}
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		client := holder.name
		if client == "" {
			client = "-"
		}
		cp.audit.Write(auditRecord{
			TS:         start.UTC().Format(time.RFC3339),
			Client:     client,
			Method:     r.Method,
			Path:       r.URL.Path, // path only — query string excluded (no secrets)
			Status:     rec.status,
			DurationMs: time.Since(start).Milliseconds(),
			RemoteAddr: r.RemoteAddr,
			Bytes:      rec.bytes,
		})
	})
}

// auditRecord is one access-log line, written as JSON to the audit file (v0.4.1).
//
// HARD invariant: this record must NEVER contain secrets — no Authorization
// header, no token, and no request/response body. Path is r.URL.Path only (the
// query string is excluded so a secret accidentally placed in a query param is
// not persisted). See CONTRIBUTING "Areas that need extra care".
type auditRecord struct {
	TS         string `json:"ts"`
	Client     string `json:"client"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	RemoteAddr string `json:"remote_addr"`
	Bytes      int64  `json:"bytes"`
}

// statusRecorder wraps http.ResponseWriter to capture the status code and bytes
// written for the access log. It forwards http.Flusher so SSE handlers
// (streamTownWall) keep working — without this their w.(http.Flusher) assertion
// would fail and the Town Wall stream would break.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports http.Flusher (SSE).
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// auditLogger appends access records to a size-rotated jsonl file under
// {workDir}/audit/. A single mutex guards write/rotate/tail so concurrent
// requests, rotation, and GET /audit reads never interleave.
type auditLogger struct {
	mu       sync.Mutex
	dir      string
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	keep     int
	enabled  bool
}

// newAuditLogger opens (creating if needed) {workDir}/audit/access.jsonl. The
// audit log is ON by default; EPHEMERA_AUDIT_DISABLE=true returns a no-op
// logger. EPHEMERA_AUDIT_MAX_MIB (default 100) caps the active file before
// rotation; EPHEMERA_AUDIT_KEEP (default 5) bounds the rotated files.
func newAuditLogger(workDir string) (*auditLogger, error) {
	a := &auditLogger{
		dir:      filepath.Join(workDir, "audit"),
		maxBytes: int64(envInt("EPHEMERA_AUDIT_MAX_MIB", 100)) << 20,
		keep:     envInt("EPHEMERA_AUDIT_KEEP", 5),
		enabled:  !envBool("EPHEMERA_AUDIT_DISABLE", false),
	}
	if !a.enabled {
		return a, nil
	}
	a.path = filepath.Join(a.dir, "access.jsonl")
	if err := os.MkdirAll(a.dir, 0700); err != nil {
		return a, fmt.Errorf("audit: create dir: %w", err)
	}
	dirInfo, err := os.Lstat(a.dir)
	if err != nil {
		return a, fmt.Errorf("audit: stat dir: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return a, fmt.Errorf("audit: dir is a symlink")
	}
	if !dirInfo.IsDir() {
		return a, fmt.Errorf("audit: path is not a dir")
	}
	if err := os.Chmod(a.dir, 0700); err != nil {
		return a, fmt.Errorf("audit: chmod dir: %w", err)
	}
	f, err := openAuditFileAppend(a.path)
	if err != nil {
		return a, fmt.Errorf("audit: open log: %w", err)
	}
	a.file = f
	if fi, statErr := f.Stat(); statErr == nil {
		a.size = fi.Size()
	}
	return a, nil
}

func openAuditFileAppend(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("audit file is a symlink")
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("audit file is not regular")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat audit file: %w", err)
	}

	fd, err := unix.Open(path, unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat opened audit file: %w", err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("opened audit file is not regular")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, fmt.Errorf("chmod audit file: %w", err)
	}
	return f, nil
}

// Write appends one record. Best-effort: errors are logged, never propagated to
// the request handler. Rotates when the active file exceeds maxBytes.
func (a *auditLogger) Write(rec auditRecord) {
	if a == nil || !a.enabled {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line = append(line, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return
	}
	n, werr := a.file.Write(line)
	if werr != nil {
		slog.Warn("audit: write failed", "err", werr)
		return
	}
	a.size += int64(n)
	if a.size >= a.maxBytes {
		a.rotateLocked()
	}
}

// rotateLocked closes the active file, shifts access.jsonl.(keep-1)→.keep …→.1,
// access.jsonl→.1, drops anything beyond keep, and reopens a fresh file. Renames
// are atomic (os.Rename). Caller must hold a.mu.
func (a *auditLogger) rotateLocked() {
	if a.file != nil {
		if err := a.file.Chmod(0600); err != nil {
			slog.Warn("audit: chmod before rotate failed", "err", err)
		}
		a.file.Close()
		a.file = nil
	}
	os.Remove(fmt.Sprintf("%s.%d", a.path, a.keep)) // drop oldest beyond the window
	for i := a.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1))
	}
	if a.keep >= 1 {
		if err := os.Rename(a.path, a.path+".1"); err != nil && !os.IsNotExist(err) {
			slog.Warn("audit: rotate active file failed", "err", err)
		} else if err == nil {
			err := os.Chmod(a.path+".1", 0600)
			if err != nil && !os.IsNotExist(err) {
				slog.Warn("audit: chmod rotated file failed", "err", err)
			}
		}
	}
	f, err := openAuditFileAppend(a.path)
	if err != nil {
		slog.Warn("audit: reopen after rotate failed", "err", err)
		return
	}
	a.file = f
	a.size = 0
}

func openAuditFileRead(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("audit file is a symlink")
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("audit file is not regular")
		}
	} else {
		return nil, err
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat opened audit file: %w", err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("opened audit file is not regular")
	}
	return f, nil
}

// Close closes the active file.
func (a *auditLogger) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file != nil {
		err := a.file.Close()
		a.file = nil
		return err
	}
	return nil
}

type auditFilter struct {
	client string
	method string
	status int // 0 = any
}

// tail returns up to limit most-recent records (newest first) from the active
// file matching the filter. Reads the current file only (rotated files are out
// of scope for v1). A ring buffer bounds memory to limit records regardless of
// file size. Held under a.mu so a concurrent rotate cannot race it.
func (a *auditLogger) tail(limit int, f auditFilter) ([]auditRecord, error) {
	if a == nil || !a.enabled || limit <= 0 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fh, err := openAuditFileRead(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()

	ring := make([]auditRecord, 0, limit)
	start := 0 // index of the oldest record once the ring is full
	full := false
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec auditRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if f.client != "" && rec.Client != f.client {
			continue
		}
		if f.method != "" && rec.Method != f.method {
			continue
		}
		if f.status != 0 && rec.Status != f.status {
			continue
		}
		if !full {
			ring = append(ring, rec)
			if len(ring) == limit {
				full = true
				start = 0
			}
		} else {
			ring[start] = rec
			start = (start + 1) % limit
		}
	}

	// Reassemble chronological order, then reverse to newest-first.
	ordered := make([]auditRecord, 0, len(ring))
	if full {
		ordered = append(ordered, ring[start:]...)
		ordered = append(ordered, ring[:start]...)
	} else {
		ordered = append(ordered, ring...)
	}
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	if err := sc.Err(); err != nil {
		return ordered, err
	}
	return ordered, nil
}

// handleAudit serves GET /audit — recent access-log records as a JSON array.
// Query params: limit (default 100, max 1000), client, method, status.
func (cp *ControlPlane) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	f := auditFilter{
		client: r.URL.Query().Get("client"),
		method: r.URL.Query().Get("method"),
	}
	if v := r.URL.Query().Get("status"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.status = n
		}
	}
	records, err := cp.audit.tail(limit, f)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"audit read: %v"}`, err), http.StatusInternalServerError)
		return
	}
	if records == nil {
		records = []auditRecord{}
	}
	writeJSON(w, http.StatusOK, records)
}
