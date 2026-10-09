package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

func TestWardogsRecordUsage(t *testing.T) {
	for _, args := range [][]string{
		{"wardogs"},
		{"wardogs", "replay"},
		{"wardogs", "record"},
		{"wardogs", "record", "--base-url", "http://h:1", "extra"},
		{"wardogs", "record", "--base-url", "http://h:1", "--audit-limit", "0"},
		{"wardogs", "record", "--base-url", "http://h:1", "--audit-limit", "501"},
		{"wardogs", "record", "--base-url", "ftp://h"},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}

func TestWardogsRecord(t *testing.T) {
	const token = "cmd-test-token"
	srv := wardogstest.New(t, "../../games/wardogs/testdata/CL-509546", wardogstest.Options{Token: token})
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "rcon.txt")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "testdata")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"wardogs", "record", "--base-url", srv.URL, "--token-file", tokenFile, "--dir", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"token from " + tokenFile, "recorded ++Wardogs+Live-CL-509546 (API 1)", "20 files", "scrubbed ", "no earlier build", "Review every file"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Error("the token was printed")
	}
	if _, err := os.Stat(filepath.Join(out, "CL-509546", "v1", "config.json")); err != nil {
		t.Error(err)
	}

	// A wrong token: one request, a clear message, exit 1.
	if err := os.WriteFile(tokenFile, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	strikes := srv.Strikes()
	if code := run(context.Background(), []string{"wardogs", "record", "--base-url", srv.URL, "--token-file", tokenFile, "--dir", out}, &stdout, &stderr); code != 1 {
		t.Errorf("wrong token: exit %d", code)
	}
	if !strings.Contains(stderr.String(), "not retrying") || srv.Strikes() != strikes+1 {
		t.Errorf("wrong token: %d strikes, stderr %q", srv.Strikes()-strikes, stderr.String())
	}

	// An empty token file is refused before anything is sent.
	if err := os.WriteFile(tokenFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), []string{"wardogs", "record", "--base-url", srv.URL, "--token-file", tokenFile, "--dir", out}, &stdout, &stderr); code != 1 {
		t.Errorf("empty token file: exit %d", code)
	}
}
