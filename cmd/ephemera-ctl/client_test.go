package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestResolveURL(t *testing.T) {
	os.Unsetenv("EPHEMERA_CTL_URL")
	if got := resolveURL(); got != defaultURL {
		t.Errorf("default: got %q want %q", got, defaultURL)
	}
	os.Setenv("EPHEMERA_CTL_URL", "http://10.0.0.5:3000")
	defer os.Unsetenv("EPHEMERA_CTL_URL")
	if got := resolveURL(); got != "http://10.0.0.5:3000" {
		t.Errorf("env override: got %q", got)
	}
}

func TestResolveToken(t *testing.T) {
	os.Unsetenv("EPHEMERA_CTL_TOKEN")
	os.Unsetenv("EPHEMERA_API_TOKEN")
	if got := resolveToken(""); got != "" {
		t.Errorf("none: got %q", got)
	}
	os.Setenv("EPHEMERA_API_TOKEN", "apitok")
	defer os.Unsetenv("EPHEMERA_API_TOKEN")
	if got := resolveToken(""); got != "apitok" {
		t.Errorf("api fallback: got %q", got)
	}
	os.Setenv("EPHEMERA_CTL_TOKEN", "ctltok")
	defer os.Unsetenv("EPHEMERA_CTL_TOKEN")
	if got := resolveToken(""); got != "ctltok" {
		t.Errorf("ctl over api: got %q", got)
	}
	if got := resolveToken("override"); got != "override" {
		t.Errorf("--token override: got %q", got)
	}
}

func TestExtractCommon(t *testing.T) {
	j, tok, rest := extractCommon([]string{"--json", "ls", "--token", "T", "x"})
	if !j || tok != "T" {
		t.Errorf("got json=%v token=%q", j, tok)
	}
	if strings.Join(rest, ",") != "ls,x" {
		t.Errorf("rest=%v", rest)
	}
	if _, tok2, _ := extractCommon([]string{"--token=abc"}); tok2 != "abc" {
		t.Errorf("--token=val form: %q", tok2)
	}
	if j3, _, rest3 := extractCommon([]string{"spawn", "--profile", "x"}); j3 || len(rest3) != 3 {
		t.Errorf("leaf flags pass through: json=%v rest=%v", j3, rest3)
	}
}

func TestClientDo_SuccessAndBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	data, err := newClient(srv.URL, "tok123").do("GET", "/vms", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("body=%s", data)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization=%q, want 'Bearer tok123'", gotAuth)
	}
}

func TestClientDo_NoTokenNoHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	if _, err := newClient(srv.URL, "").do("GET", "/vms", nil); err != nil {
		t.Fatal(err)
	}
	if hadAuth {
		t.Error("no Authorization header should be sent when token is empty")
	}
}

func TestClientDo_ErrorStatusCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"vm not found"}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL, "").do("DELETE", "/vms/x", nil)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "vm not found") {
		t.Errorf("expected a 404 error carrying the server body, got %v", err)
	}
}

func TestClientDo_PostEncodesBody(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL, "").do("POST", "/flocks", map[string]any{"task": "t", "roles": []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type=%q", gotCT)
	}
	if !strings.Contains(gotBody, `"task":"t"`) || !strings.Contains(gotBody, `"roles":["a","b"]`) {
		t.Errorf("body=%s", gotBody)
	}
}

func TestAuditCmd_EscapesFilterValues(t *testing.T) {
	var gotPath string
	var gotQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		q := r.URL.Query()
		gotQuery = map[string]string{
			"limit":  q.Get("limit"),
			"client": q.Get("client"),
			"method": q.Get("method"),
			"status": q.Get("status"),
			"role":   q.Get("role"),
			"debug":  q.Get("debug"),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	t.Setenv("EPHEMERA_CTL_URL", srv.URL)
	t.Setenv("EPHEMERA_CTL_TOKEN", "")
	t.Setenv("EPHEMERA_API_TOKEN", "")

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
		r.Close()
	}()

	auditCmd([]string{
		"--json",
		"--limit", "7",
		"--client", "alice&role=admin",
		"--method", "POST+PATCH",
		"--status", "201&debug=true",
	})

	w.Close()
	_, _ = io.ReadAll(r)

	if gotPath != "/audit" {
		t.Fatalf("path = %q, want /audit", gotPath)
	}
	want := map[string]string{
		"limit":  "7",
		"client": "alice&role=admin",
		"method": "POST+PATCH",
		"status": "201&debug=true",
		"role":   "",
		"debug":  "",
	}
	for key, wantValue := range want {
		if gotQuery[key] != wantValue {
			t.Fatalf("query[%s] = %q, want %q (all query: %#v)", key, gotQuery[key], wantValue, gotQuery)
		}
	}
}
