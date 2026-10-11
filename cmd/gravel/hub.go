package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gravel-project/gravel/internal/hubvm"
)

func hubUsage(w io.Writer) {
	fmt.Fprint(w, `usage: gravel hub <command> [flags]

  create      make (or find) the hub's VM, data volume and firewall; --dry-run exits 3 when it would change something
  status      what exists for the hub, its address, and the owner-claim token while it is unowned
  destroy     delete the hub's VM; --delete-volume and --delete-firewall also delete those
  allow-ssh   replace the addresses the hub's firewall admits SSH from
  ssh         open a shell on the hub's host (as the runtime user; --admin for the admin user)
  cloud-init  print the cloud-init document a hub host boots with (for a local VM or another tool)
`)
}

func hubCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		hubUsage(stderr)
		return 2
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "create":
		return hubCreate(ctx, args, stdout, stderr)
	case "status":
		return hubStatus(ctx, args, stdout, stderr)
	case "destroy":
		return hubDestroy(ctx, args, stdout, stderr)
	case "allow-ssh":
		return hubAllowSSH(ctx, args, stdout, stderr)
	case "ssh":
		return hubSSH(ctx, args, stdin, stdout, stderr)
	case "cloud-init":
		return hubCloudInit(args, stdout, stderr)
	case "help", "-h", "--help":
		hubUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "gravel hub: unknown command %q\n", cmd)
		hubUsage(stderr)
		return 2
	}
}

// documentFlags are the flags that shape the cloud-init document, shared by create and
// cloud-init.
type documentFlags struct {
	profile       *string
	sshKeys       *stringList
	extraPackages *string
	publicURL     *string
	orgName       *string
	hubImage      *string
	behindTunnel  *bool
}

func addDocumentFlags(fs *flag.FlagSet) documentFlags {
	keys := &stringList{}
	d := documentFlags{
		profile:       fs.String("profile", hubvm.ProfileHost, "host (the machine; your own tooling installs the hub) or stock (gravel's own units: a running hub)"),
		sshKeys:       keys,
		extraPackages: fs.String("extra-packages", "", "comma-separated packages to install beside the base set"),
		publicURL:     fs.String("public-url", "", "stock: the origin members use, https://… (auth.base_url)"),
		orgName:       fs.String("org-name", "", "stock: the organization's display name"),
		hubImage:      fs.String("hub-image", "", "stock: a gravel-hub image to run instead of the one the stock unit pins"),
		behindTunnel:  fs.Bool("behind-tunnel", true, "stock: trust CF-Connecting-IP for the client address (the hub is reached through a Cloudflare Tunnel)"),
	}
	fs.Var(keys, "ssh-key", "an OpenSSH public key file, authorized for the admin and the runtime user (repeatable; default ~/.ssh/id_ed25519.pub)")
	return d
}

// params builds the document's parameters; the device and hostname come from the caller.
func (d documentFlags) params(hostname, device string) (hubvm.Params, error) {
	keyFiles := []string(*d.sshKeys)
	if len(keyFiles) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return hubvm.Params{}, fmt.Errorf("no --ssh-key and no home directory: %w", err)
		}
		keyFiles = []string{home + "/.ssh/id_ed25519.pub"}
	}
	var keys []string
	for _, f := range keyFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return hubvm.Params{}, fmt.Errorf("ssh key: %w", err)
		}
		n := len(keys)
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				keys = append(keys, line)
			}
		}
		if len(keys) == n {
			return hubvm.Params{}, fmt.Errorf("ssh key: %s holds no key", f)
		}
	}
	p := hubvm.Params{
		Profile:       *d.profile,
		Hostname:      hostname,
		SSHKeys:       keys,
		DataDevice:    device,
		ExtraPackages: splitList(*d.extraPackages),
	}
	if p.Profile == hubvm.ProfileStock {
		p.Stock = &hubvm.StockParams{
			PublicURL:    *d.publicURL,
			OrgName:      *d.orgName,
			HubImage:     *d.hubImage,
			BehindTunnel: *d.behindTunnel,
		}
	}
	return p, nil
}

func hubCloudInit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hub cloud-init", stderr)
	doc := addDocumentFlags(fs)
	name := fs.String("name", "", "the host's name (required)")
	device := fs.String("device", "", "the data volume's device path as the instance sees it, e.g. /dev/disk/by-id/virtio-gravel-data (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || *device == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel hub cloud-init --name NAME --device PATH [--profile host|stock] [--ssh-key FILE]... [--extra-packages a,b] [--public-url URL]")
		return 2
	}
	p, err := doc.params(*name, *device)
	if err != nil {
		fmt.Fprintln(stderr, "gravel hub cloud-init:", err)
		return 1
	}
	out, err := hubvm.Render(p)
	if err != nil {
		fmt.Fprintln(stderr, "gravel hub cloud-init:", err)
		return 2
	}
	fmt.Fprint(stdout, out)
	return 0
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
