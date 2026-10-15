package hubvm_test

import (
	"slices"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/digitalocean"
	"github.com/gravel-project/gravel/cloud/digitalocean/dotest"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/hubvm"
)

const conformanceKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE/YktJENnWZ8NpaCnunhEJnbar3Id1Qob11OuYeC9F0"

// TestConformance is the path a control plane provisioning hubs takes (gravel#211), from outside
// the package and through its exported names only, on every cloud gravel ships: the fake, and
// DigitalOcean's provider against its API emulator. It is the contract the package promises such
// a caller: a dry run makes nothing, a create is found again by name, a second create changes
// nothing, a host replaced on its volume comes back with its data, and a full destroy leaves
// nothing behind.
func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		name, region, size string
		open               func(t *testing.T) cloud.Provider
	}{
		{"fake", "fake-1", "small", func(*testing.T) cloud.Provider { return fake.New() }},
		{"digitalocean", "sfo3", "s-1vcpu-1gb", func(t *testing.T) cloud.Provider {
			srv := dotest.New(t, dotest.WithActionDelay(2*time.Millisecond), dotest.WithMaxPerPage(2))
			p, err := digitalocean.New(digitalocean.Config{
				BaseURL: srv.URL(), Token: dotest.Token, PollInterval: time.Millisecond, RetryWait: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conformance(t, tc.open(t), hubvm.Spec{
				Name: "tenant-7", Region: tc.region, Size: tc.size, Image: "fedora-44-x64", VolumeGiB: 10,
				SSHKeyName: "grvl-tenant-7", SSHPublicKey: conformanceKey,
				SSHFrom: []string{"203.0.113.9"}, Tags: []string{"grvl", "grvl:org:7"},
				Document: hubvm.Params{
					Profile: hubvm.ProfileStock, SSHKeys: []string{conformanceKey},
					Stock: &hubvm.StockParams{PublicURL: "https://tenant-7.example.net", OrgName: "Tenant 7", BehindTunnel: true},
				},
			})
		})
	}
}

func conformance(t *testing.T, p cloud.Provider, s hubvm.Spec) {
	ctx := t.Context()
	steps := func(t *testing.T, got []hubvm.Step, want ...string) {
		t.Helper()
		var parts []string
		for _, st := range got {
			parts = append(parts, string(st.Kind)+":"+string(st.Action))
		}
		if !slices.Equal(parts, want) {
			t.Fatalf("steps %v, want %v", parts, want)
		}
	}
	status := func(t *testing.T) hubvm.Result {
		t.Helper()
		r, err := hubvm.Status(ctx, p, s.Name)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		return r
	}

	// A dry run says what it would make and makes nothing.
	dry, err := hubvm.Create(ctx, p, s, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	steps(t, dry.Steps, "firewall:created", "volume:created", "instance:created")
	if !dry.Changed() {
		t.Error("a dry run from nothing does not say it would change anything")
	}
	if r := status(t); r.Firewall != nil || r.Volume != nil || r.Instance != nil {
		t.Fatalf("a dry run made %+v", r)
	}

	// Create makes all three, tagged gravel's way and the caller's.
	made, err := hubvm.Create(ctx, p, s, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	steps(t, made.Steps, "firewall:created", "volume:created", "instance:created")
	in := made.Instance
	if in == nil || in.Status != cloud.StatusRunning || in.PublicIPv4 == "" || made.Recreated {
		t.Fatalf("instance %+v, recreated %v", in, made.Recreated)
	}
	for _, tag := range append([]string{hubvm.BaseTag, hubvm.Tag(s.Name)}, s.Tags...) {
		if !slices.Contains(in.Tags, tag) || !slices.Contains(made.Volume.Tags, tag) {
			t.Errorf("tag %s: instance %v, volume %v", tag, in.Tags, made.Volume.Tags)
		}
	}
	for _, st := range made.Steps {
		if st.Resource == "" {
			t.Errorf("%s step has no resource id", st.Kind)
		}
	}

	// Status finds the same resources by name alone.
	got := status(t)
	if got.Instance == nil || got.Instance.ID != in.ID || got.Volume == nil || got.Volume.ID != made.Volume.ID ||
		got.Firewall == nil || got.Firewall.ID != made.Firewall.ID {
		t.Fatalf("Status %+v, want the resources Create made", got)
	}
	if src := hubvm.SSHSources(*got.Firewall); !slices.Equal(src, []string{"203.0.113.9/32"}) {
		t.Errorf("SSH from %v", src)
	}

	// A second create changes nothing: a control plane may reconcile as often as it likes.
	again, err := hubvm.Create(ctx, p, s, false)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	steps(t, again.Steps, "firewall:unchanged", "volume:unchanged", "instance:unchanged")
	if again.Changed() || again.Instance.ID != in.ID {
		t.Errorf("second create changed something: %+v", again)
	}

	// AllowSSH replaces the sources; a dry run of the same change says it would update.
	if st, err := hubvm.AllowSSH(ctx, p, s.Name, []string{"198.51.100.0/24"}, true); err != nil || st.Action != hubvm.Updated {
		t.Fatalf("AllowSSH dry run: %+v %v", st, err)
	}
	if st, err := hubvm.AllowSSH(ctx, p, s.Name, []string{"198.51.100.0/24"}, false); err != nil || st.Kind != hubvm.KindFirewall || st.Action != hubvm.Updated {
		t.Fatalf("AllowSSH: %+v %v", st, err)
	}
	if src := hubvm.SSHSources(*status(t).Firewall); !slices.Equal(src, []string{"198.51.100.0/24"}) {
		t.Errorf("SSH from %v after AllowSSH", src)
	}

	// Replacing the host: destroy the instance, keep the volume, create again.
	gone, err := hubvm.Destroy(ctx, p, s.Name, hubvm.DestroyOptions{})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	steps(t, gone, "instance:deleted")
	if r := status(t); r.Instance != nil || r.Volume == nil || r.Firewall == nil {
		t.Fatalf("after destroying the instance: %+v", r)
	}
	// No SSHFrom keeps the sources AllowSSH set; naming them would put the spec's back.
	s.SSHFrom = nil
	back, err := hubvm.Create(ctx, p, s, false)
	if err != nil {
		t.Fatalf("Create on the kept volume: %v", err)
	}
	steps(t, back.Steps, "firewall:unchanged", "volume:unchanged", "instance:created")
	if !back.Recreated || back.Instance.ID == in.ID || !slices.Contains(back.Instance.VolumeIDs, made.Volume.ID) {
		t.Errorf("replaced host: recreated %v, instance %+v, want it on volume %s", back.Recreated, back.Instance, made.Volume.ID)
	}

	// A full destroy leaves nothing a later Status or Create would find.
	plan, err := hubvm.Destroy(ctx, p, s.Name, hubvm.DestroyOptions{DeleteVolume: true, DeleteFirewall: true, DryRun: true})
	if err != nil {
		t.Fatalf("Destroy dry run: %v", err)
	}
	steps(t, plan, "instance:deleted", "volume:deleted", "firewall:deleted")
	if r := status(t); r.Instance == nil {
		t.Fatal("a dry-run destroy deleted the instance")
	}
	if _, err := hubvm.Destroy(ctx, p, s.Name, hubvm.DestroyOptions{DeleteVolume: true, DeleteFirewall: true}); err != nil {
		t.Fatalf("Destroy everything: %v", err)
	}
	if r := status(t); r.Firewall != nil || r.Volume != nil || r.Instance != nil {
		t.Errorf("after a full destroy Status finds %+v", r)
	}
	if left, err := hubvm.Destroy(ctx, p, s.Name, hubvm.DestroyOptions{DeleteVolume: true, DeleteFirewall: true}); err != nil || len(left) != 0 {
		t.Errorf("a second full destroy: %v %v", left, err)
	}
}
