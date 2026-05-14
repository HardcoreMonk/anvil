package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	envAnvilOTLPEndpoint = "ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPEndpoint      = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

type traceExporter struct {
	endpoint string
	http     *http.Client
}

type traceEnvelope struct {
	Spans []traceSpan `json:"spans"`
}

type traceSpan struct {
	Name       string            `json:"name"`
	StartedAt  time.Time         `json:"started_at"`
	DurationMS int64             `json:"duration_ms"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

func newTraceExporterFromEnv(httpClient *http.Client) *traceExporter {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(envAnvilOTLPEndpoint)), "/")
	if endpoint == "" {
		endpoint = strings.TrimRight(strings.TrimSpace(os.Getenv(envOTLPEndpoint)), "/")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &traceExporter{endpoint: endpoint, http: httpClient}
}

func (e *traceExporter) Enabled() bool {
	return e != nil && strings.TrimSpace(e.endpoint) != ""
}

func (e *traceExporter) Export(ctx context.Context, name string, startedAt time.Time, duration time.Duration, attrs map[string]string) error {
	if !e.Enabled() {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	body, err := json.Marshal(traceEnvelope{Spans: []traceSpan{{
		Name:       name,
		StartedAt:  startedAt.UTC(),
		DurationMS: duration.Milliseconds(),
		Attributes: sanitizeTraceAttributes(attrs),
	}}})
	if err != nil {
		return fmt.Errorf("encode trace span: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create trace export request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("export trace span: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("export trace span: status %d", resp.StatusCode)
	}
	return nil
}

func sanitizeTraceAttributes(attrs map[string]string) map[string]string {
	out := make(map[string]string)
	for key, value := range attrs {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if lowerKey == "" || strings.Contains(lowerKey, "token") || strings.Contains(lowerKey, "secret") || strings.Contains(lowerKey, "authorization") {
			continue
		}
		lowerValue := strings.ToLower(value)
		if strings.Contains(lowerValue, "agent_token") || strings.Contains(lowerValue, "bearer ") || strings.Contains(lowerValue, "secret") {
			continue
		}
		out[strings.TrimSpace(key)] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
