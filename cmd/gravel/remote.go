package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// sshAdmin is the hub host's OS-maintenance user (sudo); the runtime user is hubvm.DefaultUser.
const sshAdmin = "admin"

// sshPoll is how often waitReady retries SSH while an instance boots.
var sshPoll = 5 * time.Second

// remote runs commands on a hub host. The CLI uses the system's ssh: the operator's agent, keys
// and configuration work as they do for any other host, and nothing here reimplements them.
type remote interface {
	// run runs a command non-interactively, with stdin as its input.
	run(ctx context.Context, user, host string, stdin io.Reader, stdout, stderr io.Writer, command string) error
	// interactive opens a shell, or runs args with a terminal.
	interactive(ctx context.Context, user, host string, stdin io.Reader, stdout, stderr io.Writer, args []string) error
	// forget drops what known_hosts remembers for host.
	forget(ctx context.Context, host string)
}

// newRemote makes the remote for a command's flags; tests replace it.
var newRemote = func(knownHosts string) remote { return sshRemote{knownHosts: knownHosts} }

// connFlags are the SSH settings shared by create, status and ssh.
type connFlags struct {
	knownHosts *string
}

func addConnFlags(fs *flag.FlagSet) connFlags {
	return connFlags{knownHosts: fs.String("known-hosts", defaultKnownHosts(), "the known_hosts file gravel keeps hub host keys in")}
}

func (c connFlags) remote() remote { return newRemote(*c.knownHosts) }

// defaultKnownHosts is gravel's own known_hosts, apart from the operator's: gravel trusts a hub
// host's key the first time it connects, right after creating it, and forgets it when it
// replaces the instance, which would be wrong to do to the operator's file.
func defaultKnownHosts() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "gravel", "known_hosts")
}

type sshRemote struct{ knownHosts string }

func (s sshRemote) opts() []string {
	o := []string{"-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15"}
	if s.knownHosts != "" {
		_ = os.MkdirAll(filepath.Dir(s.knownHosts), 0o700)
		o = append(o, "-o", "UserKnownHostsFile="+s.knownHosts)
	}
	return o
}

func (s sshRemote) run(ctx context.Context, user, host string, stdin io.Reader, stdout, stderr io.Writer, command string) error {
	args := append(s.opts(), "-o", "BatchMode=yes", "-T", user+"@"+host, command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

func (s sshRemote) interactive(ctx context.Context, user, host string, stdin io.Reader, stdout, stderr io.Writer, extra []string) error {
	args := append(s.opts(), user+"@"+host)
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

func (s sshRemote) forget(ctx context.Context, host string) {
	if s.knownHosts == "" {
		return
	}
	if _, err := os.Stat(s.knownHosts); err != nil {
		return
	}
	_ = exec.CommandContext(ctx, "ssh-keygen", "-q", "-R", host, "-f", s.knownHosts).Run()
}
