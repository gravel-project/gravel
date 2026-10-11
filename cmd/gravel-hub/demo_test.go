package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestDemoUsage(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	writeFile(t, token, "sim\n")
	for _, args := range [][]string{
		{"demo"},
		{"demo", "bogus"},
		{"demo", "wardogs-server", "--token-file", token},
		{"demo", "wardogs-server", "--listen", "127.0.0.1:0"},
		{"demo", "wardogs-server", "--listen", "127.0.0.1:0", "--token-file", token, "--feed-token-file", token},
		{"demo", "seed", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: %d %q", args, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"demo", "wardogs-server", "--listen", "127.0.0.1:0", "--token-file", filepath.Join(dir, "absent")}, &out, &errb); code != 1 {
		t.Errorf("a missing token file: %d", code)
	}
	bad := filepath.Join(dir, "profile.yaml")
	writeFile(t, bad, "version: 1\nnames: [Ana]\n")
	if code := run(context.Background(), []string{"demo", "wardogs-server", "--listen", "127.0.0.1:0", "--token-file", token, "--profile", bad}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "names") {
		t.Errorf("a bad profile: %d %q", code, errb.String())
	}
}

// The seed's first refusal needs no database: a hub without demo: true is refused before one is
// opened (the URL here leads nowhere).
func TestDemoSeedRefusesAHubThatIsNotADemo(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "hub.yaml")
	writeFile(t, cfg, "version: 1\norganization:\n  name: HTG\ndatabase:\n  url: postgres://u:p@127.0.0.1:1/db\n")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"demo", "seed", "--config", cfg}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "demo: true") || strings.Contains(errb.String(), "connect") {
		t.Errorf("seed without demo: %d %q", code, errb.String())
	}
	writeFile(t, cfg, "version: 1\norganization:\n  name: HTG\ndatabase:\n  url: postgres://u:p@127.0.0.1:1/db\ndemo: false\n")
	if code := run(context.Background(), []string{"demo", "seed", "--config", cfg}, &out, &errb); code != 1 {
		t.Errorf("seed with demo: false: %d", code)
	}
}

func TestConfigCheckSaysDemo(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "hub.yaml")
	writeFile(t, cfg, "version: 1\norganization:\n  name: HTG\ndatabase:\n  url: postgres://u:p@h/db\ndemo: true\n")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"config", "check", "--config", cfg}, &out, &errb); code != 0 || !strings.Contains(out.String(), "demo hub: every page says its members, matches and stats are made up") {
		t.Errorf("config check of a demo hub: %d %q", code, out.String())
	}
}

// The seed on a real database: refused without a War Dogs server, written once, refused again;
// and refused on a database where somebody signed in.
func TestDemoSeed(t *testing.T) {
	st, cfgPath, dir, run := freshHub(t)
	url := storetest.DatabaseURL(t)
	writeFile(t, cfgPath, "version: 1\norganization:\n  name: Demo\ndatabase:\n  url: "+url+"\ndemo: true\nstats:\n  pseudonym_key: a demo pseudonym key, at least 32 bytes long\n")

	if code, _, errOut := run("demo", "seed", "--config", cfgPath, "--days", "3", "--members", "5"); code != 1 || !strings.Contains(errOut, "no War Dogs server") {
		t.Errorf("a seed before servers.yaml: %d %q", code, errOut)
	}
	cred := filepath.Join(dir, "rcon")
	writeFile(t, cred, "secret\n")
	m := filepath.Join(dir, "servers.yaml")
	writeFile(t, m, serversManifest(cred, "Demo #1"))
	if code, _, errOut := run("servers", "apply", m, "--config", cfgPath); code != 0 {
		t.Fatalf("apply: %d %q", code, errOut)
	}
	code, out, errOut := run("demo", "seed", "--config", cfgPath, "--days", "3", "--members", "5", "--seed", "9")
	if code != 0 || !strings.Contains(out, "5 members (3 moderators") || !strings.Contains(out, "wd-1: ") || !strings.Contains(out, "pseudonyms for ") {
		t.Fatalf("seed: %d %q %q", code, out, errOut)
	}
	ctx := context.Background()
	c, err := st.DemoCensus(ctx)
	if err != nil || c.DemoUsers != 5 || c.RealUsers() != 0 || c.Matches == 0 || c.Pseudonyms == 0 {
		t.Errorf("after the seed: %+v %v", c, err)
	}
	if code, _, errOut := run("demo", "seed", "--config", cfgPath); code != 1 || !strings.Contains(errOut, "nothing was written") || !strings.Contains(errOut, "seeded already") {
		t.Errorf("a second seed: %d %q", code, errOut)
	}
	if again, _ := st.DemoCensus(ctx); again != c {
		t.Errorf("a refused seed wrote: %+v, was %+v", again, c)
	}
}

func TestDemoSeedRefusesADatabaseWithARealMember(t *testing.T) {
	st, cfgPath, dir, run := freshHub(t)
	url := storetest.DatabaseURL(t)
	writeFile(t, cfgPath, "version: 1\norganization:\n  name: Demo\ndatabase:\n  url: "+url+"\ndemo: true\n")
	cred := filepath.Join(dir, "rcon")
	writeFile(t, cred, "secret\n")
	m := filepath.Join(dir, "servers.yaml")
	writeFile(t, m, serversManifest(cred, "Demo #1"))
	if code, _, errOut := run("servers", "apply", m, "--config", cfgPath); code != 0 {
		t.Fatalf("apply: %d %q", code, errOut)
	}
	ctx := context.Background()
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"},
		store.Identity{Provider: "discord", Subject: "80351110224678912", VerificationMethod: "oauth2", VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run("demo", "seed", "--config", cfgPath); code != 1 || !strings.Contains(errOut, "signed in for real") {
		t.Errorf("a seed over a real member: %d %q", code, errOut)
	}
	if c, _ := st.DemoCensus(ctx); c.DemoUsers != 0 || c.Matches != 0 {
		t.Errorf("a refused seed wrote: %+v", c)
	}
}

// syncBuffer is a bytes.Buffer safe for the command's goroutine and the test's.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestDemoWardogsServer(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	writeFile(t, token, "sim-secret\n")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"demo", "wardogs-server", "--listen", addr, "--token-file", token, "--seed", "3"}, &out, &errb)
	}()
	get := func(path, token string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	deadline := time.Now().Add(10 * time.Second)
	for get("/v1/health", "") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatalf("the simulator never answered: %s %s", out.String(), errb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := get("/v1/players", "sim-secret"); code != http.StatusOK {
		t.Errorf("players with the token file's token: %d", code)
	}
	if code := get("/v1/players", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("players with another token: %d", code)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the simulator did not stop")
	}
	if s := out.String(); !strings.Contains(s, "made up") || strings.Contains(s, "sim-secret") {
		t.Errorf("log: %s", s)
	}
	_ = os.Remove(token)
}
