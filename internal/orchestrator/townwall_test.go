package orchestrator

import (
	"fmt"
	"os"
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

func TestTownWall_SeqMonotonic(t *testing.T) {
	tmp := t.TempDir()
	tw, err := NewTownWall("flock-seq", filepath.Join(tmp, "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("NewTownWall: %v", err)
	}
	m1, _ := tw.Post("a", "first")
	m2, _ := tw.Post("a", "second")
	m3, _ := tw.Post("b", "third")
	if m1.Seq != 1 || m2.Seq != 2 || m3.Seq != 3 {
		t.Errorf("expected seq 1,2,3; got %d,%d,%d", m1.Seq, m2.Seq, m3.Seq)
	}
	hist, _ := tw.History()
	if len(hist) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(hist))
	}
	for i, m := range hist {
		if m.Seq != uint64(i+1) {
			t.Errorf("history[%d].Seq = %d, want %d", i, m.Seq, i+1)
		}
	}
}

func TestTownWall_SeqResumesAcrossReopen(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "TOWN_WALL.log")

	tw1, err := NewTownWall("flock-resume", path)
	if err != nil {
		t.Fatalf("NewTownWall #1: %v", err)
	}
	tw1.Post("a", "msg1")
	tw1.Post("a", "msg2")

	tw2, err := NewTownWall("flock-resume", path)
	if err != nil {
		t.Fatalf("NewTownWall #2: %v", err)
	}
	m3, _ := tw2.Post("a", "msg3")
	if m3.Seq != 3 {
		t.Errorf("expected seq 3 after reopen, got %d", m3.Seq)
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

// TestTownWall_Rotation verifies size-based rotation (v0.4.3): once the active
// log passes maxBytes it shifts to .1 and a fresh active file continues.
func TestTownWall_Rotation(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "TOWN_WALL.log")
	tw, err := NewTownWall("flock-rot", path)
	if err != nil {
		t.Fatalf("NewTownWall: %v", err)
	}
	tw.SetRotation(300, 2) // small threshold forces rotation within a few posts
	for i := 0; i < 60; i++ {
		if _, err := tw.Post("worker-1", fmt.Sprintf("padded message body number %d", i)); err != nil {
			t.Fatalf("Post %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated backup %s.1: %v", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if fi.Size() >= 300*4 {
		t.Errorf("active file should have rotated; size=%d", fi.Size())
	}
}
