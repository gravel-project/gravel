package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/hubvm"
)

func (e *testEnv) createArgs(extra ...string) []string {
	return append([]string{
		"hub", "create", "--provider", "fake", "--name", "hub-1", "--region", "fake-1",
		"--size", "small", "--image", "fedora-44-x64", "--ssh-key", e.key,
	}, extra...)
}

func (e *testEnv) mustCreate(extra ...string) string {
	e.t.Helper()
	code, out, errOut := e.run(e.createArgs(extra...)...)
	if code != 0 {
		e.t.Fatalf("create: %d\n%s\n%s", code, out, errOut)
	}
	return out
}

func (e *testEnv) firewallSources() []string {
	e.t.Helper()
	fws, err := e.cloud.Firewalls(e.t.Context(), cloud.Filter{Name: hubvm.FirewallName("hub-1")})
	if err != nil || len(fws) != 1 {
		e.t.Fatalf("firewalls %v %v", fws, err)
	}
	return hubvm.SSHSources(fws[0])
}

func (e *testEnv) instanceIP() string {
	e.t.Helper()
	insts, err := e.cloud.Instances(e.t.Context(), cloud.Filter{Tag: hubvm.Tag("hub-1")})
	if err != nil || len(insts) != 1 {
		e.t.Fatalf("instances %v %v", insts, err)
	}
	return insts[0].PublicIPv4
}

func commands(calls []remoteCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.user + " " + c.command
	}
	return out
}

func TestHubCreateUsage(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"hub", "create", "--provider", "fake"},
		{"hub", "create", "--provider", "fake", "--name", "hub-1", "--region", "fake-1"},
		e.createArgs("--ssh-from-here", "extra-arg"),
		e.createArgs("--bogus-flag"),
		e.createArgs("--ssh-from-here", "--tunnel-token-file", e.write("t", "tok", 0o600)),
	} {
		if code, _, _ := e.run(args...); code != 2 {
			t.Errorf("%q: %d, want 2", args, code)
		}
	}
	if s := e.cloud.Counts(); s != (fake.Counts{}) {
		t.Errorf("a usage error made %+v", s)
	}

	open := e.write("open-token", "tok-0644", 0o644)
	code, out, errOut := e.run(e.createArgs("--ssh-from-here", "--profile", "stock", "--public-url", "https://hub.example.com", "--tunnel-token-file", open)...)
	if code != 1 || !strings.Contains(errOut, "readable by others") {
		t.Errorf("0644 token file: %d %q", code, errOut)
	}
	if strings.Contains(out+errOut, "tok-0644") {
		t.Error("the token was printed")
	}
	if s := e.cloud.Counts(); s != (fake.Counts{}) {
		t.Errorf("a refused token file made %+v", s)
	}

	code, _, errOut = e.run(e.createArgs()...)
	if code != 1 || !strings.Contains(errOut, "--ssh-from") {
		t.Errorf("no SSH sources: %d %q", code, errOut)
	}
}

func TestHubCreate(t *testing.T) {
	e := newEnv(t)
	e.remote.sshDownFor = 2
	out := e.mustCreate("--ssh-from-here")

	for _, kind := range []string{"firewall", "volume", "instance"} {
		if !strings.Contains(out, kind) {
			t.Errorf("stdout lacks %s:\n%s", kind, out)
		}
	}
	if strings.Count(out, " created") != 3 || !strings.Contains(out, "ready: ssh gravel@") {
		t.Errorf("stdout:\n%s", out)
	}
	if got := e.firewallSources(); !slices.Equal(got, []string{"203.0.113.9/32"}) {
		t.Errorf("firewall sources %v", got)
	}
	ip := e.instanceIP()
	calls, _, forgotten := e.remote.snapshot()
	want := []string{"admin true", "admin true", "admin true", "admin sudo cloud-init status --wait --long"}
	if got := commands(calls); !slices.Equal(got, want) {
		t.Errorf("remote calls %q, want %q", got, want)
	}
	for _, c := range calls {
		if c.host != ip {
			t.Errorf("remote host %s, want %s", c.host, ip)
		}
	}
	if !slices.Equal(forgotten, []string{ip}) {
		t.Errorf("forgotten %v, want [%s]", forgotten, ip)
	}

	// A second run finds everything and forgets no host key.
	counts := e.cloud.Counts()
	code, out, errOut := e.run(e.createArgs("--ssh-from-here")...)
	if code != 0 || strings.Count(out, " unchanged") != 3 || strings.Contains(out, "created") {
		t.Errorf("second run: %d %s\n%s", code, errOut, out)
	}
	if e.cloud.Counts() != counts || e.cloud.CallCount("CreateInstance") != 1 {
		t.Errorf("second run changed the cloud: %+v", e.cloud.Counts())
	}
	if _, _, forgotten := e.remote.snapshot(); len(forgotten) != 1 {
		t.Errorf("second run forgot %v", forgotten)
	}

	// A dry run after create changes nothing and exits 0.
	if code, out, _ := e.run(e.createArgs("--dry-run")...); code != 0 || strings.Contains(out, "would be") {
		t.Errorf("dry run after create: %d\n%s", code, out)
	}
	// One that would change the firewall exits 3.
	if code, out, _ := e.run(e.createArgs("--dry-run", "--ssh-from", "192.0.2.1")...); code != 3 || !strings.Contains(out, "would be updated") {
		t.Errorf("dry run with a new source: %d\n%s", code, out)
	}
	if got := e.firewallSources(); !slices.Equal(got, []string{"203.0.113.9/32"}) {
		t.Errorf("a dry run changed the firewall: %v", got)
	}
}

func TestHubCreateDryRunFromNothing(t *testing.T) {
	e := newEnv(t)
	code, out, errOut := e.run(e.createArgs("--ssh-from-here", "--dry-run")...)
	if code != 3 || strings.Count(out, "would be created") != 3 {
		t.Errorf("%d %s\n%s", code, errOut, out)
	}
	if s := e.cloud.Counts(); s != (fake.Counts{}) {
		t.Errorf("a dry run made %+v", s)
	}
	if calls, _, _ := e.remote.snapshot(); len(calls) != 0 {
		t.Errorf("a dry run reached the host: %v", commands(calls))
	}
}

func TestHubCreateCloudInit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		again error // the second look, after systemd's boot jobs; nil: done then
		code  int
		out   string // in stdout or stderr
	}{
		{"done", nil, nil, 0, "ready:"},
		{"warnings", exitError{2}, nil, 0, "warnings"},
		{"failed", exitError{1}, exitError{1}, 1, "cloud-init on"},
		// cloud-init 25.3's "systemd unit failure" that clears once cloud-final.service finishes.
		{"a unit still finishing", exitError{1}, nil, 0, "ready:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.remote.cloudInitErr = tc.err
			e.remote.cloudInitAgain, e.remote.cloudInitSettles = tc.again, tc.again == nil
			e.remote.cloudInitOut = "status: done\ndetail: something\n"
			code, out, errOut := e.run(e.createArgs("--ssh-from", "192.0.2.1")...)
			if code != tc.code || !strings.Contains(out+errOut, tc.out) {
				t.Errorf("%d\n%s\n%s", code, out, errOut)
			}
			calls, _, _ := e.remote.snapshot()
			settled := slices.Contains(commands(calls), "admin systemctl is-system-running --wait")
			if want := errors.Is(tc.err, exitError{1}); settled != want {
				t.Errorf("asked again after the boot jobs: %t, want only after exit 1: %q", settled, commands(calls))
			}
			if tc.code != 0 && !strings.Contains(out+errOut, "  detail: something") {
				t.Errorf("cloud-init's output not shown:\n%s\n%s", out, errOut)
			}
		})
	}
}

func TestHubCreateNoWait(t *testing.T) {
	e := newEnv(t)
	out := e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")
	if !strings.Contains(out, "not waiting") {
		t.Errorf("stdout:\n%s", out)
	}
	if calls, _, forgotten := e.remote.snapshot(); len(calls) != 0 || len(forgotten) != 0 {
		t.Errorf("--no-wait reached the host: %v %v", commands(calls), forgotten)
	}
}

func TestHubCreateRecreate(t *testing.T) {
	e := newEnv(t)
	e.mustCreate("--ssh-from", "192.0.2.1")
	first := e.instanceIP()
	if code, _, errOut := e.run("hub", "destroy", "--provider", "fake", "--name", "hub-1", "--yes"); code != 0 {
		t.Fatalf("destroy: %d %s", code, errOut)
	}
	out := e.mustCreate()
	if !strings.Contains(out, "existing data volume") {
		t.Errorf("stdout:\n%s", out)
	}
	if _, _, forgotten := e.remote.snapshot(); !slices.Equal(forgotten, []string{first, e.instanceIP()}) {
		t.Errorf("forgotten %v", forgotten)
	}
}

func TestHubCreateStockTunnel(t *testing.T) {
	const token = "eyJhIjoiVHVubmVsVG9rZW4tc2VjcmV0LTEyMzQ1In0"
	e := newEnv(t)
	tokenFile := e.write("tunnel-token", token+"\n", 0o600)
	code, out, errOut := e.run(e.createArgs("--ssh-from-here", "--profile", "stock", "--public-url", "https://hub.example.com",
		"--org-name", "Example", "--tunnel-token-file", tokenFile)...)
	if code != 0 || !strings.Contains(out, "gravel-tunnel-token installed") || !strings.Contains(out, "gravel hub status") {
		t.Fatalf("%d %s\n%s", code, errOut, out)
	}
	calls, _, _ := e.remote.snapshot()
	last := calls[len(calls)-1]
	if last.user != hubvm.DefaultUser || last.stdin != token {
		t.Errorf("tunnel call as %q with stdin %q", last.user, last.stdin)
	}
	if !strings.Contains(last.command, "podman secret create --replace gravel-tunnel-token -") {
		t.Errorf("tunnel command %q", last.command)
	}
	for _, c := range calls {
		if strings.Contains(c.command, token) {
			t.Errorf("the token is in a command line: %q", c.command)
		}
	}
	if strings.Contains(out+errOut, token) {
		t.Error("the token was printed")
	}
	e.cloud.mu.Lock()
	ud := strings.Join(e.cloud.userData, "\n")
	e.cloud.mu.Unlock()
	if ud == "" || strings.Contains(ud, token) {
		t.Errorf("the token is in the user data (or there is none: %d bytes)", len(ud))
	}

	// A failed install says so without the token.
	e.remote.tunnelErr = exitError{1}
	code, out, errOut = e.run(e.createArgs("--profile", "stock", "--public-url", "https://hub.example.com", "--tunnel-token-file", tokenFile)...)
	if code != 1 || !strings.Contains(errOut, "install the tunnel") || strings.Contains(out+errOut, token) {
		t.Errorf("failed install: %d\n%s\n%s", code, out, errOut)
	}
}

func TestHubStatus(t *testing.T) {
	e := newEnv(t)
	code, out, _ := e.run("hub", "status", "--provider", "fake", "--name", "hub-1")
	if code != 0 || strings.Count(out, " none") != 3 {
		t.Errorf("nothing: %d\n%s", code, out)
	}
	if code, _, _ := e.run("hub", "status", "--provider", "fake"); code != 2 {
		t.Errorf("no --name: %d", code)
	}

	e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")
	unowned := `{"time":"2026-10-11T00:00:00Z","level":"WARN","msg":"the hub is unowned: claim it once with this token (it expires; a restart mints a new one)","token":"abc","expires_at":"2026-10-12T00:00:00Z"}`
	owned := `{"time":"2026-10-11T00:01:00Z","level":"INFO","msg":"organization","name":"Example","owned":true}`

	e.remote.journal = "not json\n" + unowned + "\n" + owned + "\n"
	code, out, errOut := e.run("hub", "status", "--provider", "fake", "--name", "hub-1")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, want := range []string{"firewall", "SSH from 192.0.2.1/32", "volume", "hub-1-data", "attached to", "instance", "running", e.instanceIP()} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "abc") {
		t.Errorf("an owned hub's old token was printed:\n%s", out)
	}
	calls, _, _ := e.remote.snapshot()
	if len(calls) != 1 || calls[0].user != hubvm.DefaultUser || !strings.HasPrefix(calls[0].command, "journalctl --user -u gravel-hub.service") {
		t.Errorf("remote calls %+v", calls)
	}

	e.remote.journal = unowned + "\n"
	_, out, _ = e.run("hub", "status", "--provider", "fake", "--name", "hub-1")
	if !strings.Contains(out, "claim token abc (expires 2026-10-12T00:00:00Z)") {
		t.Errorf("unowned:\n%s", out)
	}

	n := len(calls) + 1
	if code, _, _ := e.run("hub", "status", "--provider", "fake", "--name", "hub-1", "--no-ssh"); code != 0 {
		t.Error(code)
	}
	if calls, _, _ := e.remote.snapshot(); len(calls) != n {
		t.Errorf("--no-ssh reached the host: %v", commands(calls[n:]))
	}
}

func TestHubDestroy(t *testing.T) {
	e := newEnv(t)
	destroy := func(extra ...string) (int, string, string) {
		return e.run(append([]string{"hub", "destroy", "--provider", "fake", "--name", "hub-1"}, extra...)...)
	}
	if code, _, errOut := destroy(); code != 2 || !strings.Contains(errOut, "--yes") {
		t.Errorf("without --yes: %d %q", code, errOut)
	}
	if code, out, _ := destroy("--dry-run"); code != 0 || !strings.Contains(out, "nothing to destroy") {
		t.Errorf("dry run of nothing: %d\n%s", code, out)
	}

	e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")
	code, out, _ := destroy("--dry-run", "--delete-volume", "--delete-firewall")
	if code != 3 || strings.Count(out, "would be deleted") != 3 {
		t.Errorf("dry run: %d\n%s", code, out)
	}
	if s := e.cloud.Counts(); s.Instances != 1 || s.Volumes != 1 || s.Firewalls != 1 {
		t.Errorf("a dry run deleted something: %+v", s)
	}

	code, out, _ = destroy("--yes")
	if code != 0 || !strings.Contains(out, "is kept") || !strings.Contains(out, "gravel hub create --name hub-1") {
		t.Errorf("destroy: %d\n%s", code, out)
	}
	if s := e.cloud.Counts(); s.Instances != 0 || s.Volumes != 1 || s.Firewalls != 1 {
		t.Errorf("destroy left %+v", s)
	}

	code, out, _ = destroy("--yes", "--delete-volume", "--delete-firewall")
	if code != 0 || strings.Contains(out, "is kept") {
		t.Errorf("full destroy: %d\n%s", code, out)
	}
	if s := e.cloud.Counts(); s.Instances != 0 || s.Volumes != 0 || s.Firewalls != 0 {
		t.Errorf("full destroy left %+v", s)
	}
}

func TestHubAllowSSH(t *testing.T) {
	e := newEnv(t)
	allow := func(extra ...string) (int, string, string) {
		return e.run(append([]string{"hub", "allow-ssh", "--provider", "fake", "--name", "hub-1"}, extra...)...)
	}
	if code, _, errOut := allow("--ssh-from", "192.0.2.1"); code != 1 || !strings.Contains(errOut, "run create first") {
		t.Errorf("no firewall: %d %q", code, errOut)
	}
	e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")

	if code, out, _ := allow("--ssh-from", "198.51.100.7", "--dry-run"); code != 3 || !strings.Contains(out, "would be updated") {
		t.Errorf("dry run: %d\n%s", code, out)
	}
	if got := e.firewallSources(); !slices.Equal(got, []string{"192.0.2.1/32"}) {
		t.Errorf("a dry run changed the firewall: %v", got)
	}
	if code, out, _ := allow("--ssh-from", "198.51.100.7", "--ssh-from-here"); code != 0 || !strings.Contains(out, "updated") {
		t.Errorf("allow: %d\n%s", code, out)
	}
	if got := e.firewallSources(); !slices.Equal(got, []string{"198.51.100.7/32", "203.0.113.9/32"}) {
		t.Errorf("sources %v", got)
	}
	if code, out, _ := allow("--ssh-from", "203.0.113.9", "--ssh-from", "198.51.100.7", "--dry-run"); code != 0 || !strings.Contains(out, "unchanged") {
		t.Errorf("dry run, same sources: %d\n%s", code, out)
	}
	if code, _, _ := allow(); code != 1 {
		t.Errorf("no sources: %d", code)
	}
}

func TestHubSSH(t *testing.T) {
	e := newEnv(t)
	if code, _, errOut := e.run("hub", "ssh", "--provider", "fake", "--name", "hub-1"); code != 1 || !strings.Contains(errOut, "no running instance") {
		t.Errorf("no instance: %d %q", code, errOut)
	}
	if code, _, _ := e.run("hub", "ssh", "--provider", "fake"); code != 2 {
		t.Errorf("no --name: %d", code)
	}
	e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")
	ip := e.instanceIP()

	if code, _, errOut := e.run("hub", "ssh", "--provider", "fake", "--name", "hub-1", "uptime", "-p"); code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if code, _, _ := e.run("hub", "ssh", "--provider", "fake", "--name", "hub-1", "--admin"); code != 0 {
		t.Fatal(code)
	}
	e.remote.interactiveErr = exitError{7}
	if code, _, _ := e.run("hub", "ssh", "--provider", "fake", "--name", "hub-1", "--", "false"); code != 7 {
		t.Errorf("the remote command's exit code: %d, want 7", code)
	}
	_, interactive, _ := e.remote.snapshot()
	if len(interactive) != 3 {
		t.Fatalf("interactive calls %+v", interactive)
	}
	for i, want := range []struct {
		user string
		args []string
	}{
		{hubvm.DefaultUser, []string{"uptime", "-p"}},
		{"admin", nil},
		{hubvm.DefaultUser, []string{"false"}},
	} {
		got := interactive[i]
		if got.user != want.user || got.host != ip || !slices.Equal(got.args, want.args) {
			t.Errorf("call %d: %+v, want %s@%s %q", i, got, want.user, ip, want.args)
		}
	}
}
