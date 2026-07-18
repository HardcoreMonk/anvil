package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
)

// newMetricsTestCP returns a ControlPlane carrying just enough state for the
// metrics handler and gauge funcs to operate without touching disk or the
// firecracker SDK. flockMgr is a real FlockManager pointing at a temp dir;
// snapshots / vms / clients are empty maps so gauge funcs report zero.
// agentHTTPClient is wired so stats tests that touch probeAgentBusy don't
// dereference a nil client.
func newMetricsTestCP(t *testing.T) *ControlPlane {
	t.Helper()
	cp := &ControlPlane{
		vms:             make(map[string]*runningVM),
		snapshots:       make(map[string]storage.SnapshotMetadata),
		flockMgr:        orchestrator.NewFlockManager(t.TempDir()),
		agentHTTPClient: &http.Client{Timeout: time.Second},
	}
	cp.metrics = newDaemonMetrics(cp)
	return cp
}

func TestMetrics_HandlerReturns200_AndContentType(t *testing.T) {
	cp := newMetricsTestCP(t)
	srv := httptest.NewServer(http.HandlerFunc(cp.handleMetrics))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("unexpected Content-Type %q", ct)
	}
}

func TestMetrics_RejectsNonGET(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	cp.handleMetrics(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestMetrics_UnauthByDefault_AcceptsNoBearer(t *testing.T) {
	// The mux split in NewControlPlane is responsible for unauthenticated
	// access. At the handler level we just verify that handleMetrics itself
	// does not require Authorization (no header → 200).
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMetrics_HandlerExposesVMCount(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.vms["vm-1"] = &runningVM{}
	cp.vms["vm-2"] = &runningVM{}
	cp.vms["vm-3"] = &runningVM{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, "# TYPE ephemera_vm_count gauge") {
		t.Errorf("expected vm_count TYPE line, got:\n%s", out)
	}
	if !strings.Contains(out, "\nephemera_vm_count 3\n") {
		t.Errorf("expected vm_count=3, got:\n%s", out)
	}
}

func TestMetrics_HandlerReflectsCounterUpdates(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.metrics.vmSpawnTotal.WithLabelValues("ok").Inc()
	cp.metrics.vmSpawnTotal.WithLabelValues("ok").Inc()
	cp.metrics.vmSpawnTotal.WithLabelValues("fail").Inc()
	cp.metrics.flockSpawn.Inc()
	cp.metrics.sighupReload.Add(5)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	wantLines := []string{
		`ephemera_vm_spawn_total{outcome="ok"} 2`,
		`ephemera_vm_spawn_total{outcome="fail"} 1`,
		`ephemera_flock_spawn_total 1`,
		`ephemera_sighup_reload_total 5`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
}

func TestMetrics_HandlerExposesSNIVerdictTotal(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.metrics.IncSNIVerdict(protoTCP, "allowed")
	cp.metrics.IncSNIVerdict(protoTCP, "allowed")
	cp.metrics.IncSNIVerdict(protoTCP, "denied")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	wantLines := []string{
		`ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"} 2`,
		`ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"} 1`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
}

func TestMetrics_SNIVerdictSeparatesByProto(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.metrics.IncSNIVerdict(protoTCP, "denied")
	cp.metrics.IncSNIVerdict(protoUDP, "denied")
	cp.metrics.IncSNIVerdict(protoUDP, "denied")
	cp.metrics.IncSNIVerdict(protoUnknown, "dropped")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	// proto is registered first, so it renders first: {proto="...",outcome="..."}.
	wantLines := []string{
		`ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"} 1`,
		`ephemera_egress_sni_verdict_total{proto="udp",outcome="denied"} 2`,
		`ephemera_egress_sni_verdict_total{proto="unknown",outcome="dropped"} 1`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
}

func TestMetrics_IncSNIVerdictNilMetricsSafe(t *testing.T) {
	var m *daemonMetrics
	m.IncSNIVerdict(protoTCP, "allowed") // must not panic on nil receiver
}

func TestMetrics_HandlerExposesSNIECHObserved(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.metrics.IncSNIECHObserved(protoTCP)
	cp.metrics.IncSNIECHObserved(protoTCP)
	cp.metrics.IncSNIECHObserved(protoUDP)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	wantLines := []string{
		`ephemera_egress_sni_ech_observed_total{proto="tcp"} 2`,
		`ephemera_egress_sni_ech_observed_total{proto="udp"} 1`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
}

func TestMetrics_IncSNIECHObservedNilMetricsSafe(t *testing.T) {
	var m *daemonMetrics
	m.IncSNIECHObserved(protoTCP) // must not panic on nil receiver
}
