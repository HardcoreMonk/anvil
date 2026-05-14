package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOTELTraceExporterDisabledWithoutEndpoint(t *testing.T) {
	t.Setenv(envAnvilOTLPEndpoint, "")
	t.Setenv(envOTLPEndpoint, "")
	exporter := newTraceExporterFromEnv(http.DefaultClient)
	if exporter.Enabled() {
		t.Fatal("Enabled() = true, want false without endpoint")
	}
	if err := exporter.Export(context.Background(), "vm_create", time.Now(), time.Second, map[string]string{"agent_token": "secret"}); err != nil {
		t.Fatalf("disabled Export returned error: %v", err)
	}
}

func TestOTELTraceExporterPostsSanitizedLifecycleSpan(t *testing.T) {
	var gotPath string
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = string(data)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv(envAnvilOTLPEndpoint, server.URL)
	t.Setenv(envOTLPEndpoint, "")

	exporter := newTraceExporterFromEnv(server.Client())
	if !exporter.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	if err := exporter.Export(context.Background(), "vm_create", time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC), 1500*time.Millisecond, map[string]string{
		"vm_id":       "vm-1",
		"agent_token": "secret-token",
		"secret":      "must-not-leak",
	}); err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	if gotPath != "/v1/traces" {
		t.Fatalf("path = %q, want /v1/traces", gotPath)
	}
	for _, want := range []string{`"name":"vm_create"`, `"duration_ms":1500`, `"vm_id":"vm-1"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("trace body missing %q: %s", want, gotBody)
		}
	}
	if strings.Contains(gotBody, "agent_token") || strings.Contains(gotBody, "secret-token") || strings.Contains(gotBody, "must-not-leak") {
		t.Fatalf("trace body leaked sensitive data: %s", gotBody)
	}
}
