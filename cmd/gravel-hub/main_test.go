package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "gravel-hub ") {
		t.Errorf("version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "config check") {
		t.Errorf("help: %d %q", code, out.String())
	}
	if code := run(context.Background(), []string{"bogus"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("unknown: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"config"}, &out, &errb); code != 2 {
		t.Errorf("config without check: %d", code)
	}
}

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "hub.yaml")
	_ = os.WriteFile(good, []byte("version: 1\norganization:\n  name: HTG\ndatabase:\n  url: postgres://u:p@h/db\n"), 0o600)
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("version: 1\norganization:\n  name: HTG\n"), 0o600)

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"config", "check", "--config", good}, &out, &errb); code != 0 {
		t.Errorf("good config: %d %s", code, errb.String())
	}
	if strings.Contains(out.String(), ":p@") || !strings.Contains(out.String(), "config OK") {
		t.Errorf("output must not leak the password: %q", out.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"config", "check", "--config", bad}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "GRAVEL_DATABASE_URL") {
		t.Errorf("bad config: %d %s", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"config", "check", "--config", filepath.Join(dir, "missing.yaml")}, &out, &errb); code != 1 {
		t.Errorf("missing config: %d", code)
	}
	t.Setenv("GRAVEL_CONFIG", good)
	if code := run(context.Background(), []string{"config", "check"}, &out, &errb); code != 0 {
		t.Errorf("GRAVEL_CONFIG: %d %s", code, errb.String())
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"healthcheck", "--url", srv.URL + "/healthz"}, &errb, &errb); code != 0 {
		t.Errorf("healthy: %d %s", code, errb.String())
	}
	if code := run(context.Background(), []string{"healthcheck", "--url", srv.URL + "/down"}, &errb, &errb); code != 1 || !strings.Contains(errb.String(), "503") {
		t.Errorf("unhealthy: %d %s", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"healthcheck", "--url", "http://127.0.0.1:1/healthz"}, &errb, &errb); code != 1 {
		t.Errorf("unreachable: %d", code)
	}
}
