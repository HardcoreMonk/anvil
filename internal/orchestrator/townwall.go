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

// Message is a single Town Wall entry.
type Message struct {
	Timestamp string `json:"timestamp"`
	AgentID   string `json:"agent_id"`
	Body      string `json:"body"`
}

// TownWall is the shared append-only log between agents in a single flock.
// All Post calls and subscriber notifications are serialized through mu so
// the on-disk file order matches the order subscribers observe.
type TownWall struct {
	mu      sync.Mutex
	path    string
	flockID string
	subs    map[chan Message]struct{}
}

// NewTownWall opens (creating if missing) the log file at path. The file's
// parent directory is created if it does not already exist.
func NewTownWall(flockID, path string) (*TownWall, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("townwall: create parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("townwall: open log: %w", err)
	}
	f.Close()
	return &TownWall{
		path:    path,
		flockID: flockID,
		subs:    make(map[chan Message]struct{}),
	}, nil
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

	msg := Message{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		AgentID:   agentID,
		Body:      body,
	}
	f, err := os.OpenFile(tw.path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return msg, fmt.Errorf("townwall: open for append: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "[%s] <%s> %s\n", msg.Timestamp, msg.AgentID, msg.Body); err != nil {
		return msg, fmt.Errorf("townwall: append: %w", err)
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

// History returns every parseable message currently in the log file.
func (tw *TownWall) History() ([]Message, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	f, err := os.Open(tw.path)
	if err != nil {
		return nil, fmt.Errorf("townwall: open for read: %w", err)
	}
	defer f.Close()

	var out []Message
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if m, ok := parseLine(scanner.Text()); ok {
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
