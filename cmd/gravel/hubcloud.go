package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/digitalocean"
	"github.com/gravel-project/gravel/internal/hubvm"
	"github.com/gravel-project/gravel/internal/publicaddr"
)

// providerConfig is what every provider is made from: a token file, and the provider's own
// grouping of resources (a DigitalOcean project).
type providerConfig struct {
	TokenFile string
	Project   string
}

// providers are the clouds `--provider` names. Tests add a fake.
var providers = map[string]func(providerConfig) (cloud.Provider, error){
	"digitalocean": func(c providerConfig) (cloud.Provider, error) {
		token, err := readSecretFile(c.TokenFile)
		if err != nil {
			return nil, err
		}
		return digitalocean.New(digitalocean.Config{Token: token, Project: c.Project, UserAgent: "gravel/" + buildVersion()})
	},
}

// defaultImages are the images a hub host boots on each provider: the Fedora release the
// cloud-init document is written and tested for (deploy/hub-vm.sh boots the same one).
var defaultImages = map[string]string{"digitalocean": "fedora-44-x64"}

// tokenEnv names the environment variable a provider's --token-file defaults to.
func tokenEnv(provider string) string {
	return "GRAVEL_" + strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_TOKEN_FILE"
}

// readSecretFile reads a token from a file only its owner can read. A token in a world-readable
// file has already leaked to every account on the machine, so gravel refuses to use it.
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("no token file (--token-file)")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s is readable by others (mode %04o); chmod 600 it, and rotate the token if others could have read it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return t, nil
}

// providerFlags are --provider, --token-file and --project, shared by every command that talks to
// a cloud.
type providerFlags struct {
	provider, tokenFile, project *string
}

func addProviderFlags(fs *flag.FlagSet) providerFlags {
	return providerFlags{
		provider:  fs.String("provider", "digitalocean", "the cloud: digitalocean"),
		tokenFile: fs.String("token-file", "", "a file holding the provider's API token, mode 0600 (default $GRAVEL_<PROVIDER>_TOKEN_FILE)"),
		project:   fs.String("project", "", "the provider's project (DigitalOcean: a project name or id) new resources go in; empty is the default project"),
	}
}

func (f providerFlags) open() (cloud.Provider, error) {
	mk, ok := providers[*f.provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", *f.provider)
	}
	tokenFile := *f.tokenFile
	if tokenFile == "" {
		tokenFile = os.Getenv(tokenEnv(*f.provider))
	}
	return mk(providerConfig{TokenFile: tokenFile, Project: *f.project})
}

// sshFromFlags are --ssh-from and --ssh-from-here.
type sshFromFlags struct {
	from *stringList
	here *bool
}

func addSSHFromFlags(fs *flag.FlagSet) sshFromFlags {
	from := &stringList{}
	fs.Var(from, "ssh-from", "an address or CIDR the firewall admits SSH from (repeatable)")
	return sshFromFlags{from: from, here: fs.Bool("ssh-from-here", false, "admit SSH from this machine's public IPv4 address")}
}

func (f sshFromFlags) cidrs(ctx context.Context) ([]string, error) {
	out := []string(*f.from)
	if *f.here {
		ip, err := publicAddress(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, nil
}

// traceURL is where this machine's public address is read from; a test points it elsewhere.
var traceURL = publicaddr.TraceURL

func publicAddress(ctx context.Context) (string, error) {
	a, err := publicaddr.Detect(ctx, nil, traceURL)
	if err != nil {
		return "", err
	}
	return a.String(), nil
}

func printSteps(w io.Writer, steps []hubvm.Step, dryRun bool) {
	for _, s := range steps {
		action := string(s.Action)
		if dryRun && s.Action != hubvm.Unchanged {
			action = "would be " + action
		}
		line := fmt.Sprintf("%-9s %-28s %s", s.Kind, s.Name, action)
		if s.Detail != "" {
			line += " — " + s.Detail
		}
		fmt.Fprintln(w, line)
	}
}

func hubCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hub create", stderr)
	prov := addProviderFlags(fs)
	doc := addDocumentFlags(fs)
	sshFrom := addSSHFromFlags(fs)
	conn := addConnFlags(fs)
	name := fs.String("name", "", "the hub's name: its host, instance and volume are named after it (required)")
	region := fs.String("region", "", "the provider's region, e.g. sfo3 (required)")
	size := fs.String("size", "", "the instance size, e.g. s-2vcpu-4gb (required)")
	image := fs.String("image", "", "the image to boot (default: the provider's Fedora image this document is tested on)")
	volumeGiB := fs.Int("volume-size", 20, "the data volume's size in GiB (never changed on an existing volume)")
	monitoring := fs.Bool("monitoring", true, "turn on the provider's own metrics agent")
	dryRun := fs.Bool("dry-run", false, "change nothing; say what would change (exit 3 when it would)")
	noWait := fs.Bool("no-wait", false, "return once the instance runs, without waiting for SSH and cloud-init")
	tunnelTokenFile := fs.String("tunnel-token-file", "", "stock: a Cloudflare Tunnel token file (mode 0600) to install as the gravel-tunnel-token secret and start gravel-cloudflared")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || *region == "" || *size == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel hub create --name NAME --region REGION --size SIZE [--ssh-from CIDR | --ssh-from-here] [--profile host|stock] [flags]; gravel hub create -h lists them")
		return 2
	}
	if *tunnelTokenFile != "" && *doc.profile != hubvm.ProfileStock {
		fmt.Fprintln(stderr, "gravel hub create: --tunnel-token-file is for the stock profile; with host, your own tooling installs the tunnel")
		return 2
	}
	if *image == "" {
		*image = defaultImages[*prov.provider]
	}
	if *image == "" {
		fmt.Fprintf(stderr, "gravel hub create: provider %s has no default image; pass --image\n", *prov.provider)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	fail := func(err error) int { fmt.Fprintln(stderr, "gravel hub create:", err); return 1 }
	var tunnelToken string
	if *tunnelTokenFile != "" {
		t, err := readSecretFile(*tunnelTokenFile)
		if err != nil {
			return fail(fmt.Errorf("tunnel %w", err))
		}
		tunnelToken = t
	}
	params, err := doc.params(*name, "/dev/placeholder")
	if err != nil {
		return fail(err)
	}
	cidrs, err := sshFrom.cidrs(ctx)
	if err != nil {
		return fail(err)
	}
	p, err := prov.open()
	if err != nil {
		return fail(err)
	}
	res, err := hubvm.Create(ctx, p, hubvm.Spec{
		Name: *name, Region: *region, Size: *size, Image: *image, VolumeGiB: *volumeGiB,
		SSHKeyName: "gravel-" + *name, SSHPublicKey: params.SSHKeys[0],
		SSHFrom: cidrs, Monitoring: *monitoring, Document: params,
	}, *dryRun)
	printSteps(stdout, res.Steps, *dryRun)
	if err != nil {
		return fail(err)
	}
	if *dryRun {
		if res.Changed() {
			return 3
		}
		return 0
	}
	inst := res.Instance
	if inst == nil || inst.PublicIPv4 == "" {
		return fail(errors.New("the instance has no public address yet; run create again in a minute"))
	}
	created := slicesContainsCreated(res.Steps)
	if res.Recreated {
		fmt.Fprintln(stdout, "the new instance mounts the existing data volume: the hub comes back with its data")
	}
	if *noWait {
		fmt.Fprintf(stdout, "address %s (not waiting for cloud-init)\n", inst.PublicIPv4)
		return 0
	}
	r := conn.remote()
	if created {
		// A new instance has new host keys, and clouds reuse addresses: forget whatever key this
		// address had, then trust the first one it shows (TOFU, right after we made it).
		r.forget(ctx, inst.PublicIPv4)
	}
	fmt.Fprintf(stdout, "waiting for %s to finish booting (SSH, then cloud-init)\n", inst.PublicIPv4)
	if err := waitReady(ctx, r, inst.PublicIPv4, stdout); err != nil {
		return fail(err)
	}
	if tunnelToken != "" {
		if err := installTunnel(ctx, r, inst.PublicIPv4, tunnelToken); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "tunnel: gravel-tunnel-token installed, gravel-cloudflared started")
	}
	fmt.Fprintf(stdout, "ready: ssh %s@%s (OS maintenance: admin@%s)\n", hubvm.DefaultUser, inst.PublicIPv4, inst.PublicIPv4)
	if *doc.profile == hubvm.ProfileStock {
		fmt.Fprintf(stdout, "next: gravel hub status --name %s prints the owner-claim token\n", *name)
	}
	return 0
}

func slicesContainsCreated(steps []hubvm.Step) bool {
	for _, s := range steps {
		if s.Kind == "instance" && s.Action == hubvm.Created {
			return true
		}
	}
	return false
}

// waitReady waits for SSH as admin, then for cloud-init. cloud-init's exit 2 is "done, with
// warnings" (a module warned: DigitalOcean's metadata names a hostname cloud-init sets before
// the system bus is up on Fedora, and its retry succeeds), so the warnings are shown and the host
// counts as ready; exit 1 is a failure.
func waitReady(ctx context.Context, r remote, host string, stdout io.Writer) error {
	for {
		err := r.run(ctx, sshAdmin, host, nil, io.Discard, io.Discard, "true")
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no SSH to %s: %w", host, err)
		case <-time.After(sshPoll):
		}
	}
	var out strings.Builder
	err := r.run(ctx, sshAdmin, host, nil, &out, &out, "sudo cloud-init status --wait --long")
	var ee interface{ ExitCode() int }
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 2:
		fmt.Fprintf(stdout, "cloud-init finished with warnings:\n%s", indentLines(out.String()))
		return nil
	default:
		return fmt.Errorf("cloud-init on %s failed: %w\n%s(details: gravel hub ssh --admin --name … sudo cat /var/log/cloud-init-output.log)", host, err, indentLines(out.String()))
	}
}

// installTunnel gives a stock hub its tunnel: the token goes over SSH on stdin straight into
// podman's secret store, never into a file, an argument or the cloud's metadata.
func installTunnel(ctx context.Context, r remote, host, token string) error {
	const script = `set -e
podman secret create --replace gravel-tunnel-token - >/dev/null
install -m 0644 /usr/local/share/gravel/quadlet/gravel-cloudflared.container ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user restart gravel-cloudflared.service`
	var out strings.Builder
	if err := r.run(ctx, hubvm.DefaultUser, host, strings.NewReader(token), &out, &out, script); err != nil {
		return fmt.Errorf("install the tunnel: %w\n%s", err, indentLines(out.String()))
	}
	return nil
}

func indentLines(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ") + "\n"
}

func hubStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hub status", stderr)
	prov := addProviderFlags(fs)
	conn := addConnFlags(fs)
	name := fs.String("name", "", "the hub's name (required)")
	noSSH := fs.Bool("no-ssh", false, "only ask the cloud; don't read the owner-claim token from the host")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel hub status --name NAME [--no-ssh]")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "gravel hub status:", err); return 1 }
	p, err := prov.open()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := hubvm.Status(ctx, p, *name)
	if err != nil {
		return fail(err)
	}
	if res.Firewall != nil {
		fmt.Fprintf(stdout, "firewall  %-28s SSH from %s\n", res.Firewall.Name, strings.Join(hubvm.SSHSources(*res.Firewall), ", "))
	} else {
		fmt.Fprintf(stdout, "firewall  %-28s none\n", hubvm.FirewallName(*name))
	}
	if res.Volume != nil {
		att := "detached"
		if len(res.Volume.InstanceIDs) > 0 {
			att = "attached to " + strings.Join(res.Volume.InstanceIDs, ", ")
		}
		fmt.Fprintf(stdout, "volume    %-28s %d GiB in %s, %s\n", res.Volume.Name, res.Volume.SizeGiB, res.Volume.Region, att)
	} else {
		fmt.Fprintf(stdout, "volume    %-28s none\n", hubvm.VolumeName(*name))
	}
	if res.Instance == nil {
		fmt.Fprintf(stdout, "instance  %-28s none\n", *name)
		return 0
	}
	in := res.Instance
	fmt.Fprintf(stdout, "instance  %-28s %s, %s in %s, %s (id %s)\n", in.Name, in.Status, in.Size, in.Region, orNoAddress(in.PublicIPv4), in.ID)
	if *noSSH || in.PublicIPv4 == "" || in.Status != cloud.StatusRunning {
		return 0
	}
	token, expires, err := claimToken(ctx, conn.remote(), in.PublicIPv4)
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "hub       could not read the hub's log over SSH: %v\n", err)
	case token != "":
		fmt.Fprintf(stdout, "hub       unowned; claim token %s (expires %s): log in, then paste it on the account page\n", token, expires)
	}
	return 0
}

func orNoAddress(ip string) string {
	if ip == "" {
		return "no address yet"
	}
	return ip
}

// claimToken reads the newest owner-claim token the stock hub logged (ADR-0002), if it is still
// unowned: the last "organization" or "unowned" line in its log decides.
func claimToken(ctx context.Context, r remote, host string) (token, expires string, err error) {
	var out strings.Builder
	if err := r.run(ctx, hubvm.DefaultUser, host, nil, &out, io.Discard,
		"journalctl --user -u gravel-hub.service -o cat --no-pager -n 2000 2>/dev/null || true"); err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out.String(), "\n") {
		var rec struct {
			Msg     string `json:"msg"`
			Token   string `json:"token"`
			Expires string `json:"expires_at"`
			Owned   bool   `json:"owned"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(rec.Msg, "the hub is unowned"):
			token, expires = rec.Token, rec.Expires
		case rec.Msg == "organization" && rec.Owned:
			token, expires = "", ""
		}
	}
	return token, expires, nil
}

func hubDestroy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hub destroy", stderr)
	prov := addProviderFlags(fs)
	name := fs.String("name", "", "the hub's name (required)")
	deleteVolume := fs.Bool("delete-volume", false, "also delete the data volume: the hub's data is gone for good")
	deleteFirewall := fs.Bool("delete-firewall", false, "also delete the firewall")
	yes := fs.Bool("yes", false, "really destroy (required unless --dry-run)")
	dryRun := fs.Bool("dry-run", false, "change nothing; say what would go (exit 3 when something would)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel hub destroy --name NAME --yes [--delete-volume] [--delete-firewall] [--dry-run]")
		return 2
	}
	if !*yes && !*dryRun {
		fmt.Fprintln(stderr, "gravel hub destroy: this takes the hub down; pass --yes (or --dry-run to see what would go)")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "gravel hub destroy:", err); return 1 }
	p, err := prov.open()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	steps, err := hubvm.Destroy(ctx, p, *name, hubvm.DestroyOptions{DeleteVolume: *deleteVolume, DeleteFirewall: *deleteFirewall, DryRun: *dryRun})
	printSteps(stdout, steps, *dryRun)
	if err != nil {
		return fail(err)
	}
	if len(steps) == 0 {
		fmt.Fprintln(stdout, "nothing to destroy")
	}
	if !*deleteVolume {
		fmt.Fprintf(stdout, "the data volume %s is kept: gravel hub create --name %s makes a new instance on it\n", hubvm.VolumeName(*name), *name)
	}
	if *dryRun && len(steps) > 0 {
		return 3
	}
	return 0
}

func hubAllowSSH(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hub allow-ssh", stderr)
	prov := addProviderFlags(fs)
	sshFrom := addSSHFromFlags(fs)
	name := fs.String("name", "", "the hub's name (required)")
	dryRun := fs.Bool("dry-run", false, "change nothing; say what would change (exit 3 when it would)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel hub allow-ssh --name NAME (--ssh-from CIDR)... | --ssh-from-here")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "gravel hub allow-ssh:", err); return 1 }
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cidrs, err := sshFrom.cidrs(ctx)
	if err != nil {
		return fail(err)
	}
	p, err := prov.open()
	if err != nil {
		return fail(err)
	}
	step, err := hubvm.AllowSSH(ctx, p, *name, cidrs, *dryRun)
	if err != nil {
		return fail(err)
	}
	printSteps(stdout, []hubvm.Step{step}, *dryRun)
	if *dryRun && step.Action != hubvm.Unchanged {
		return 3
	}
	return 0
}

func hubSSH(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("hub ssh", stderr)
	prov := addProviderFlags(fs)
	conn := addConnFlags(fs)
	name := fs.String("name", "", "the hub's name (required)")
	admin := fs.Bool("admin", false, "log in as the admin user (sudo; OS maintenance) instead of the runtime user")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "usage: gravel hub ssh --name NAME [--admin] [command...]")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "gravel hub ssh:", err); return 1 }
	p, err := prov.open()
	if err != nil {
		return fail(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	res, err := hubvm.Status(sctx, p, *name)
	cancel()
	if err != nil {
		return fail(err)
	}
	if res.Instance == nil || res.Instance.PublicIPv4 == "" {
		return fail(fmt.Errorf("hub %s has no running instance with an address", *name))
	}
	user := hubvm.DefaultUser
	if *admin {
		user = sshAdmin
	}
	err = conn.remote().interactive(ctx, user, res.Instance.PublicIPv4, stdin, stdout, stderr, fs.Args())
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return fail(err)
	}
	return 0
}
