// Package orchestrator implements the multi-agent coordination primitives used
// by Goosetown-style flocks: a shared append-only Town Wall log, flock state
// tracking, and structured handoff helpers.
package orchestrator

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SystemAuthor is the Town Wall author label for messages the control plane posts
// on its own behalf — flock lifecycle events (spawn / join / leave / role change /
// pause / resume / broadcast) and watchdog notices — as opposed to messages
// authored by an agent. It is intentionally not a name a user would assign to an
// agent role or profile, so it never collides with a user-defined "orchestrator".
const SystemAuthor = "control-plane"

// Message is a single Town Wall entry. Seq is a monotonic per-flock counter
// starting at 1; subscribers can detect dropped messages by checking for gaps.
type Message struct {
	Seq       uint64 `json:"seq"`
	Timestamp string `json:"timestamp"`
	AgentID   string `json:"agent_id"`
	Body      string `json:"body"`
}

// TownWall is the shared append-only log between agents in a single flock.
// All Post calls and subscriber notifications are serialized through mu so
// the on-disk file order matches the order subscribers observe.
type TownWall struct {
	mu       sync.Mutex
	path     string
	flockID  string
	subs     map[chan Message]struct{}
	nextSeq  uint64
	maxBytes int64 // size-based rotation threshold (v0.4.3); 0 = no rotation
	keep     int   // number of rotated backups to retain
}

// NewTownWall opens (creating if missing) the log file at path. The file's
// parent directory is created if it does not already exist. If the log already
// has entries, nextSeq is initialized so that subsequent Post calls keep the
// monotonic seq sequence across daemon restarts.
func NewTownWall(flockID, path string) (*TownWall, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("townwall: create parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("townwall: open log: %w", err)
	}
	f.Close()
	tw := &TownWall{
		path:    path,
		flockID: flockID,
		subs:    make(map[chan Message]struct{}),
	}
	// Seed nextSeq from the existing log line count. Read failures are
	// non-fatal — the file may be missing entries from a corrupt write,
	// but starting fresh from 0 is preferable to refusing to open.
	if existing, err := tw.History(); err == nil {
		tw.nextSeq = uint64(len(existing))
	}
	return tw, nil
}

// FlockID returns the flock identifier this wall belongs to.
func (tw *TownWall) FlockID() string { return tw.flockID }

// Path returns the on-disk log file path.
func (tw *TownWall) Path() string { return tw.path }

// Post appends a message and fans it out to active subscribers.
// Subscribers that cannot keep up are dropped from this fan-out (their channel
// stays registered for future messages).
func (tw *TownWall) Post(agentID, body string) (Message, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	tw.nextSeq++
	msg := Message{
		Seq:       tw.nextSeq,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		AgentID:   agentID,
		Body:      body,
	}
	f, err := os.OpenFile(tw.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return msg, fmt.Errorf("townwall: open for append: %w", err)
	}
	_, werr := fmt.Fprintf(f, "[%s] <%s> %s\n", msg.Timestamp, msg.AgentID, msg.Body)
	f.Close()
	if werr != nil {
		return msg, fmt.Errorf("townwall: append: %w", werr)
	}
	// Size-based rotation (v0.4.3): once the active log reaches maxBytes, shift it
	// to .1 (…→.keep) and let the next Post recreate a fresh active file. History
	// then reflects only the active file (rotated backups stay on disk).
	if tw.maxBytes > 0 {
		if fi, statErr := os.Stat(tw.path); statErr == nil && fi.Size() >= tw.maxBytes {
			tw.rotateLocked()
		}
	}
	for sub := range tw.subs {
		select {
		case sub <- msg:
		default:
			// Slow subscriber — drop this message rather than block writers.
		}
	}
	return msg, nil
}

// History returns every parseable message currently in the log file. Seq
// values are reassigned 1..N in file order so they are stable across reads
// regardless of when the underlying log was written (the on-disk format does
// not store seq, so this is the canonical assignment).
func (tw *TownWall) History() ([]Message, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	f, err := os.Open(tw.path)
	if err != nil {
		return nil, fmt.Errorf("townwall: open for read: %w", err)
	}
	defer f.Close()

	var out []Message
	var seq uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if m, ok := parseLine(scanner.Text()); ok {
			seq++
			m.Seq = seq
			out = append(out, m)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("townwall: scan: %w", err)
	}
	return out, nil
}

// Subscribe returns a buffered channel receiving subsequent Post messages.
// Caller must Unsubscribe to release resources and avoid goroutine leaks.
func (tw *TownWall) Subscribe() chan Message {
	ch := make(chan Message, 32)
	tw.mu.Lock()
	tw.subs[ch] = struct{}{}
	tw.mu.Unlock()
	return ch
}

// Unsubscribe removes a previously subscribed channel and closes it.
func (tw *TownWall) Unsubscribe(ch chan Message) {
	tw.mu.Lock()
	if _, ok := tw.subs[ch]; ok {
		delete(tw.subs, ch)
		close(ch)
	}
	tw.mu.Unlock()
}

// parseLine extracts a Message from "[ts] <agent> body".
// Returns (_, false) for any line that does not match the expected shape.
func parseLine(line string) (Message, bool) {
	if !strings.HasPrefix(line, "[") {
		return Message{}, false
	}
	end := strings.Index(line, "]")
	if end < 0 {
		return Message{}, false
	}
	ts := line[1:end]
	rest := strings.TrimLeft(line[end+1:], " ")
	if !strings.HasPrefix(rest, "<") {
		return Message{}, false
	}
	angleEnd := strings.Index(rest, ">")
	if angleEnd < 0 {
		return Message{}, false
	}
	agent := rest[1:angleEnd]
	body := strings.TrimLeft(rest[angleEnd+1:], " ")
	return Message{Timestamp: ts, AgentID: agent, Body: body}, true
}

// SetRotation enables size-based rotation of the Town Wall log (v0.4.3).
// maxBytes <= 0 disables it (the default, preserving unbounded append).
func (tw *TownWall) SetRotation(maxBytes int64, keep int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.maxBytes = maxBytes
	tw.keep = keep
}

// rotateLocked shifts path.(keep-1)→.keep … path→.1, dropping the oldest beyond
// the window, so the next Post recreates a fresh active file. Mirrors the audit
// logger's rotation. Caller must hold tw.mu.
func (tw *TownWall) rotateLocked() {
	if tw.keep < 1 {
		return
	}
	os.Remove(fmt.Sprintf("%s.%d", tw.path, tw.keep)) // drop oldest beyond the window
	for i := tw.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", tw.path, i), fmt.Sprintf("%s.%d", tw.path, i+1))
	}
	os.Rename(tw.path, tw.path+".1")
	// Recreate an empty active file so History and the next Post always find
	// tw.path (mirrors the audit logger reopening after rotate).
	if f, err := os.OpenFile(tw.path, os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		f.Close()
	}
}
