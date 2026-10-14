// Command gravel is the operator's command line (README build order step 5). Its first commands
// make the hub's own host (ADR-0020); the ones that act on a running hub call its HTTP-JSON API
// only (principle 4), never gRPC, Postgres or the host, with a credential on this machine
// (ADR-0031).
//
//	gravel login         log in to a hub: a local account (--local) or an app (--app)
//	gravel logout        end the session and forget the hub's credential
//	gravel whoami        who this machine is on the hub, and what they may do
//	gravel claim         become the owner of an unowned hub with its one-time token
//	gravel fleet         every server's health and the open alarms; exit 1 when one is down
//	gravel servers       list the servers, or one server's last observation
//	gravel events        the hub's events log; --follow keeps printing
//	gravel hub create    make (or find) the hub's VM, data volume and firewall through a cloud provider
//	gravel hub status    what exists for a hub, its address, and the owner-claim token while unowned
//	gravel hub destroy   delete the hub's VM (the data volume and firewall stay unless asked)
//	gravel hub allow-ssh replace the addresses the hub's firewall admits SSH from
//	gravel hub ssh       open a shell on the hub's host
//	gravel hub cloud-init print the document a hub host boots with
//	gravel version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// version is set by the linker (-X main.version=...); goreleaser does.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "hub":
		return hubCmd(ctx, args, stdin, stdout, stderr)
	case "login":
		return loginCmd(ctx, args, stdin, stdout, stderr)
	case "logout":
		return logoutCmd(ctx, args, stdout, stderr)
	case "whoami":
		return whoamiCmd(ctx, args, stdout, stderr)
	case "claim":
		return claimCmd(ctx, args, stdout, stderr)
	case "fleet":
		return fleetCmd(ctx, args, stdout, stderr)
	case "servers":
		return serversCmd(ctx, args, stdout, stderr)
	case "events":
		return eventsCmd(ctx, args, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "gravel: unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gravel <command> [flags]

  hub      make and look after the hub's own host through a cloud provider (docs/cli.md)

  login    log in to a hub: --local (a username and password) or --app (client credentials)
  logout   end the session and forget the hub's credential on this machine
  whoami   who this machine is on the hub, and what they may do
  claim    become the owner of an unowned hub with the token it printed
  fleet    every server's health and the open alarms; exit 1 when one is down
  servers  list the servers, or one server's last observation
  events   the hub's events log; --follow keeps printing what comes

  The hub is --hub, else GRAVEL_HUB, else the one last logged in to. Exit 3: log in (again).

  version  print the version
`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("gravel "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return "dev-" + s.Value[:12]
			}
		}
	}
	return version
}
