package orchestrator

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTownWall_PostAndHistory(t *testing.T) {
	tmp := t.TempDir()
	tw, err := NewTownWall("flock-1", filepath.Join(tmp, "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("NewTownWall: %v", err)
	}
	if _, err := tw.Post("agent-1", "hello"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, err := tw.Post("agent-2", "world"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	hist, err := tw.History()
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(hist))
	}
	if hist[0].AgentID != "agent-1" || hist[0].Body != "hello" {
		t.Errorf("unexpected first message: %+v", hist[0])
	}
	if hist[1].AgentID != "agent-2" || hist[1].Body != "world" {
		t.Errorf("unexpected second message: %+v", hist[1])
	}
}

func TestTownWall_Subscribe(t *testing.T) {
	tmp := t.TempDir()
	tw, err := NewTownWall("flock-sub", filepath.Join(tmp, "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("NewTownWall: %v", err)
	}
	sub := tw.Subscribe()
	defer tw.Unsubscribe(sub)

	if _, err := tw.Post("a", "msg"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	select {
	case m := <-sub:
		if m.AgentID != "a" || m.Body != "msg" {
			t.Errorf("unexpected subscriber message: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive message within 1s")
	}
}

func TestTownWall_ConcurrentPost(t *testing.T) {
	tmp := t.TempDir()
	tw, err := NewTownWall("flock-concur", filepath.Join(tmp, "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("NewTownWall: %v", err)
	}
	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tw.Post("agent", "body")
			}
		}(w)
	}
	wg.Wait()
	hist, err := tw.History()
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != workers*perWorker {
		t.Errorf("expected %d messages, got %d", workers*perWorker, len(hist))
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		agent string
		body  string
	}{
		{"[2026-05-13T12:00:00Z] <alice> hello world", true, "alice", "hello world"},
		{"not a wall line", false, "", ""},
		{"[no-end-bracket <bob> body", false, "", ""},
		{"[2026-05-13T12:00:00Z] missing-angle bob", false, "", ""},
	}
	for _, c := range cases {
		m, ok := parseLine(c.in)
		if ok != c.ok {
			t.Errorf("parseLine(%q): ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if m.AgentID != c.agent || m.Body != c.body {
			t.Errorf("parseLine(%q) = %+v, want agent=%q body=%q", c.in, m, c.agent, c.body)
		}
	}
}
