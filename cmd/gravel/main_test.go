package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
)

// testKey is a well-formed OpenSSH public key line; it authorizes nothing anywhere.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 gravel-test"

// exitError is what a remote command that ran and failed returns, as *exec.ExitError does.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitError) ExitCode() int { return e.code }

type remoteCall struct {
	user, host, command, stdin string
	args                       []string // interactive only
}

// fakeRemote records what the CLI asks of a hub host and answers from a script.
type fakeRemote struct {
	mu        sync.Mutex
	calls     []remoteCall
	shells    []remoteCall
	forgotten []string

	sshDownFor     int    // the first n "true" probes fail, as SSH does while the host boots
	cloudInitErr   error  // what `cloud-init status` returns
	cloudInitOut   string // and prints
	journal        string // what journalctl prints
	tunnelErr      error
	interactiveErr error
}

func (r *fakeRemote) run(_ context.Context, user, host string, stdin io.Reader, stdout, _ io.Writer, command string) error {
	var in string
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		in = string(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, remoteCall{user: user, host: host, command: command, stdin: in})
	switch {
	case command == "true":
		if r.sshDownFor > 0 {
			r.sshDownFor--
			return exitError{255}
		}
		return nil
	case strings.Contains(command, "cloud-init status"):
		fmt.Fprint(stdout, r.cloudInitOut)
		return r.cloudInitErr
	case strings.HasPrefix(command, "journalctl"):
		fmt.Fprint(stdout, r.journal)
		return nil
	case strings.Contains(command, "gravel-tunnel-token"):
		return r.tunnelErr
	}
	return fmt.Errorf("fake remote: unexpected command %q", command)
}

func (r *fakeRemote) interactive(_ context.Context, user, host string, _ io.Reader, _, _ io.Writer, args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shells = append(r.shells, remoteCall{user: user, host: host, args: args})
	return r.interactiveErr
}

func (r *fakeRemote) forget(_ context.Context, host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgotten = append(r.forgotten, host)
}

func (r *fakeRemote) snapshot() (calls, shells []remoteCall, forgotten []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]remoteCall(nil), r.calls...), append([]remoteCall(nil), r.shells...), append([]string(nil), r.forgotten...)
}

// recordingProvider is the fake cloud, keeping the user data of every instance it creates.
type recordingProvider struct {
	*fake.Provider
	mu       sync.Mutex
	userData []string
}

func (p *recordingProvider) CreateInstance(ctx context.Context, spec cloud.InstanceSpec) (cloud.Instance, cloud.Operation, error) {
	p.mu.Lock()
	p.userData = append(p.userData, spec.UserData)
	p.mu.Unlock()
	return p.Provider.CreateInstance(ctx, spec)
}

// testEnv is a fake cloud registered as --provider fake, a fake remote, and a trace endpoint
// that says this machine is 203.0.113.9.
type testEnv struct {
	t      *testing.T
	cloud  *recordingProvider
	remote *fakeRemote
	dir    string
	key    string

	mu      sync.Mutex
	configs []providerConfig
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, cloud: &recordingProvider{Provider: fake.New()}, remote: &fakeRemote{}, dir: t.TempDir()}
	e.key = e.write("id_ed25519.pub", "# my key\n"+testKey+"\n", 0o644)

	providers["fake"] = func(c providerConfig) (cloud.Provider, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.configs = append(e.configs, c)
		return e.cloud, nil
	}
	oldRemote, oldPoll, oldTrace := newRemote, sshPoll, traceURL
	newRemote = func(string) remote { return e.remote }
	sshPoll = time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "fl=1\nip=203.0.113.9\nts=1\n")
	}))
	traceURL = srv.URL
	t.Cleanup(func() {
		delete(providers, "fake")
		newRemote, sshPoll, traceURL = oldRemote, oldPoll, oldTrace
		srv.Close()
	})
	return e
}

func (e *testEnv) write(name, content string, mode os.FileMode) string {
	e.t.Helper()
	path := filepath.Join(e.dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // past the umask
		e.t.Fatal(err)
	}
	return path
}

func (e *testEnv) run(args ...string) (int, string, string) {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(e.t.Context(), args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (e *testEnv) lastConfig() providerConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.configs) == 0 {
		e.t.Fatal("the provider was never opened")
	}
	return e.configs[len(e.configs)-1]
}

func TestRunCommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string // in stdout
		err  string // in stderr
	}{
		{nil, 2, "", "usage: gravel"},
		{[]string{"version"}, 0, "", ""},
		{[]string{"help"}, 0, "usage: gravel", ""},
		{[]string{"bogus"}, 2, "", `unknown command "bogus"`},
		{[]string{"hub"}, 2, "", "usage: gravel hub"},
		{[]string{"hub", "help"}, 0, "cloud-init", ""},
		{[]string{"hub", "bogus"}, 2, "", `unknown command "bogus"`},
	} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), tc.args, nil, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stdout.String(), tc.out) || !strings.Contains(stderr.String(), tc.err) {
			t.Errorf("gravel %q: %d\nstdout %q\nstderr %q", tc.args, code, stdout.String(), stderr.String())
		}
		if tc.args != nil && tc.args[0] == "version" && strings.TrimSpace(stdout.String()) == "" {
			t.Error("gravel version printed nothing")
		}
	}
}

func TestHubCloudInit(t *testing.T) {
	e := newEnv(t)
	base := []string{"hub", "cloud-init", "--ssh-key", e.key}

	if code, _, _ := e.run(append(base, "--device", "/dev/vdb")...); code != 2 {
		t.Errorf("no --name: %d", code)
	}
	if code, _, _ := e.run(append(base, "--name", "hub-1")...); code != 2 {
		t.Errorf("no --device: %d", code)
	}
	code, out, errOut := e.run(append(base, "--name", "hub-1", "--device", "/dev/vdb", "--extra-packages", "golang, gh,")...)
	if code != 0 || !strings.HasPrefix(out, "#cloud-config\n") || !strings.Contains(out, "device: /dev/vdb") || !strings.Contains(out, "  - gh\n") {
		t.Errorf("host: %d %s\n%s", code, errOut, out)
	}
	code, _, errOut = e.run(append(base, "--name", "hub-1", "--device", "/dev/vdb", "--profile", "stock")...)
	if code != 2 || !strings.Contains(errOut, "public URL") {
		t.Errorf("stock without --public-url: %d %q", code, errOut)
	}
	code, out, _ = e.run(append(base, "--name", "hub-1", "--device", "/dev/vdb", "--profile", "stock", "--public-url", "https://hub.example.com")...)
	if code != 0 || !strings.Contains(out, "base_url: https://hub.example.com") {
		t.Errorf("stock: %d", code)
	}
	code, _, errOut = e.run("hub", "cloud-init", "--name", "hub-1", "--device", "/dev/vdb", "--ssh-key", filepath.Join(e.dir, "missing.pub"))
	if code != 1 || !strings.Contains(errOut, "ssh key") {
		t.Errorf("missing key file: %d %q", code, errOut)
	}
	empty := e.write("empty.pub", "# nothing here\n\n", 0o644)
	code, _, errOut = e.run("hub", "cloud-init", "--name", "hub-1", "--device", "/dev/vdb", "--ssh-key", empty)
	if code != 1 || !strings.Contains(errOut, "holds no key") {
		t.Errorf("empty key file: %d %q", code, errOut)
	}
}

func TestTokenEnv(t *testing.T) {
	if got := tokenEnv("digitalocean"); got != "GRAVEL_DIGITALOCEAN_TOKEN_FILE" {
		t.Errorf("tokenEnv = %q", got)
	}
	if got := tokenEnv("some-cloud"); got != "GRAVEL_SOME_CLOUD_TOKEN_FILE" {
		t.Errorf("tokenEnv = %q", got)
	}

	e := newEnv(t)
	t.Setenv(tokenEnv("fake"), "/from/env")
	if code, _, errOut := e.run("hub", "status", "--provider", "fake", "--name", "hub-1", "--no-ssh", "--project", "p1"); code != 0 {
		t.Fatalf("status: %d %s", code, errOut)
	}
	if c := e.lastConfig(); c.TokenFile != "/from/env" || c.Project != "p1" {
		t.Errorf("config %+v", c)
	}
	if code, _, _ := e.run("hub", "status", "--provider", "fake", "--name", "hub-1", "--no-ssh", "--token-file", "/from/flag"); code != 0 {
		t.Fatal(code)
	}
	if c := e.lastConfig(); c.TokenFile != "/from/flag" {
		t.Errorf("--token-file did not win: %+v", c)
	}
	code, _, errOut := e.run("hub", "status", "--provider", "nope", "--name", "hub-1")
	if code != 1 || !strings.Contains(errOut, `unknown provider "nope"`) {
		t.Errorf("unknown provider: %d %q", code, errOut)
	}
}

func TestReadSecretFile(t *testing.T) {
	e := newEnv(t)
	ok := e.write("ok", "  s3cret\n", 0o600)
	if got, err := readSecretFile(ok); err != nil || got != "s3cret" {
		t.Errorf("%q %v", got, err)
	}
	open := e.write("open", "s3cret", 0o644)
	if _, err := readSecretFile(open); err == nil || !strings.Contains(err.Error(), "readable by others") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("0644: %v", err)
	}
	if _, err := readSecretFile(e.write("group", "s3cret", 0o640)); err == nil {
		t.Error("0640: no error")
	}
	if _, err := readSecretFile(e.write("empty", "\n", 0o600)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty: %v", err)
	}
	if _, err := readSecretFile(""); err == nil {
		t.Error("no path: no error")
	}
	if _, err := readSecretFile(filepath.Join(e.dir, "missing")); err == nil {
		t.Error("missing: no error")
	}
}

func TestPublicAddress(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{"fl=1\nip=203.0.113.9\nts=1\n", "203.0.113.9"},
		{"ip=2001:db8::1\n", "2001:db8::1"},
		{"fl=1\nts=1\n", ""},
		{"ip=not-an-address\n", ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
		old := traceURL
		traceURL = srv.URL
		got, err := publicAddress(t.Context())
		traceURL = old
		srv.Close()
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), "no ip=") {
				t.Errorf("%q: %q %v, want an error", tc.body, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: %q %v, want %q", tc.body, got, err, tc.want)
		}
	}
}

func TestPrintSteps(t *testing.T) {
	var b bytes.Buffer
	printSteps(&b, nil, false)
	if b.Len() != 0 {
		t.Errorf("no steps printed %q", b.String())
	}
	if got := indentLines("a\nb\n\n"); got != "  a\n  b\n" {
		t.Errorf("indentLines %q", got)
	}
	if got := indentLines("\n"); got != "" {
		t.Errorf("indentLines of nothing %q", got)
	}
}
