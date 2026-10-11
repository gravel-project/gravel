package digitalocean_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/digitalocean"
	"github.com/gravel-project/gravel/cloud/digitalocean/dotest"
)

// secretToken looks like a real token, so a leak is unmistakable.
const secretToken = "dop_v1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE/YktJENnWZ8NpaCnunhEJnbar3Id1Qob11OuYeC9F0"

// newProvider starts an emulator that accepts secretToken and returns a provider for it.
func newProvider(t *testing.T, project string, opts ...dotest.Option) (*digitalocean.Provider, *dotest.Server) {
	t.Helper()
	srv := dotest.New(t, append([]dotest.Option{dotest.WithToken(secretToken), dotest.WithProject(projectID, "hidden-token-gaming")}, opts...)...)
	p, err := digitalocean.New(digitalocean.Config{
		BaseURL: srv.URL(), Token: secretToken, Project: project,
		PollInterval: time.Millisecond, RetryWait: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

// running creates a small Droplet in sfo3 and waits for it.
func running(t *testing.T, p *digitalocean.Provider, name string, tags ...string) cloud.Instance {
	t.Helper()
	inst, op, err := p.CreateInstance(t.Context(), cloud.InstanceSpec{Name: name, Region: "sfo3", Size: "s-1vcpu-1gb", Image: "fedora-44-x64", Tags: tags})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := p.Wait(t.Context(), op); err != nil {
		t.Fatalf("Wait for the create: %v", err)
	}
	return inst
}

func count(reqs []dotest.Request, method, pathPrefix string) int {
	n := 0
	for _, r := range reqs {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			n++
		}
	}
	return n
}

func TestNewNeedsAToken(t *testing.T) {
	if _, err := digitalocean.New(digitalocean.Config{}); err == nil {
		t.Fatal("New without a token succeeded")
	}
}

func TestErrorMapping(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()

	if _, err := p.Instance(ctx, "424242"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("unknown Droplet: %v, want ErrNotFound", err)
	}
	if err := p.DeleteVolume(ctx, "no-such-volume"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("unknown volume: %v, want ErrNotFound", err)
	}

	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1})
	if !errors.Is(err, cloud.ErrInvalid) || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate volume name: %v, want ErrInvalid with DigitalOcean's message", err)
	}
	if _, _, err := p.CreateInstance(ctx, cloud.InstanceSpec{Name: "vm", Region: "mkc1", Size: "s-1vcpu-1gb", Image: "fedora-44-x64"}); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("a size the region does not offer: %v, want ErrInvalid", err)
	}

	// A 429 is retried, and surfaces as ErrRateLimited when it does not stop.
	srv.Fault(http.MethodGet, "/v2/droplets", http.StatusTooManyRequests, 2)
	if _, err := p.Instances(ctx, cloud.Filter{}); err != nil {
		t.Errorf("two 429s then an answer: %v, want the answer", err)
	}
	srv.Fault(http.MethodGet, "/v2/droplets", http.StatusTooManyRequests, 100)
	before := count(srv.Requests(), http.MethodGet, "/v2/droplets")
	if _, err := p.Instances(ctx, cloud.Filter{}); !errors.Is(err, cloud.ErrRateLimited) {
		t.Errorf("429 every time: %v, want ErrRateLimited", err)
	}
	if n := count(srv.Requests(), http.MethodGet, "/v2/droplets") - before; n != 1+digitalocean.DefaultRetries {
		t.Errorf("429 every time: %d requests, want %d", n, 1+digitalocean.DefaultRetries)
	}

	// A 5xx on a POST is not retried: the API may have done it.
	srv.Fault(http.MethodPost, "/v2/volumes", http.StatusInternalServerError, 1)
	before = count(srv.Requests(), http.MethodPost, "/v2/volumes")
	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "other", Region: "sfo3", SizeGiB: 1}); err == nil {
		t.Error("a 500 on create succeeded")
	}
	if n := count(srv.Requests(), http.MethodPost, "/v2/volumes") - before; n != 1 {
		t.Errorf("a 500 on create was sent %d times, want once", n)
	}
	// A 5xx on a GET is.
	srv.Fault(http.MethodGet, "/v2/volumes", http.StatusBadGateway, 1)
	if _, err := p.Volumes(ctx, cloud.Filter{}); err != nil {
		t.Errorf("a 502 then an answer: %v", err)
	}

	bad, err := digitalocean.New(digitalocean.Config{BaseURL: srv.URL(), Token: "dop_v1_wrong"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Regions(ctx); !errors.Is(err, digitalocean.ErrUnauthorized) {
		t.Errorf("wrong token: %v, want ErrUnauthorized", err)
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	p, srv := newProvider(t, "no-such-project")
	ctx := t.Context()
	srv.Fault("", "/v2/regions", http.StatusTooManyRequests, 100)
	srv.Fault("", "/v2/sizes", http.StatusInternalServerError, 100)
	var errs []error
	_, err := p.Instance(ctx, "424242")
	errs = append(errs, err)
	_, err = p.Regions(ctx)
	errs = append(errs, err)
	_, err = p.Sizes(ctx)
	errs = append(errs, err)
	_, err = p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1})
	errs = append(errs, err)
	_, err = p.EnsureSSHKey(ctx, "k", "not a key")
	errs = append(errs, err)
	bad, err := digitalocean.New(digitalocean.Config{BaseURL: srv.URL(), Token: secretToken + "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bad.Regions(ctx)
	errs = append(errs, err)
	for i, err := range errs {
		if err == nil {
			t.Errorf("call %d succeeded; every call here should fail", i)
			continue
		}
		if strings.Contains(err.Error(), secretToken) || strings.Contains(fmt.Sprintf("%+v", err), secretToken) {
			t.Errorf("call %d's error carries the token: %v", i, err)
		}
	}
	cfg := digitalocean.Config{Token: secretToken}
	if s := fmt.Sprintf("%v %+v %s", cfg, cfg, cfg); strings.Contains(s, secretToken) {
		t.Errorf("Config prints its token: %s", s)
	}
}

func TestProjectAssignment(t *testing.T) {
	for _, project := range []string{"hidden-token-gaming", projectID} {
		t.Run(project, func(t *testing.T) {
			p, srv := newProvider(t, project)
			ctx := t.Context()
			inst := running(t, p, "vm")
			vol, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1})
			if err != nil {
				t.Fatal(err)
			}
			addr, err := p.AllocateAddress(ctx, "sfo3", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"do:droplet:" + inst.ID, "do:volume:" + vol.ID, "do:reservedip:" + addr.IP}
			if got := srv.ProjectResources(projectID); !slices.Equal(got, want) {
				t.Errorf("project resources = %v, want %v", got, want)
			}
			lists := count(srv.Requests(), http.MethodGet, "/v2/projects")
			if project == projectID && lists != 0 {
				t.Errorf("a project id was looked up %d times, want never", lists)
			}
			if project != projectID && lists != 1 {
				t.Errorf("a project name was looked up %d times, want once", lists)
			}
		})
	}
}

func TestDefaultProjectAssignsNothing(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()
	running(t, p, "vm")
	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AllocateAddress(ctx, "sfo3", nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		if strings.HasPrefix(r.Path, "/v2/projects") || strings.Contains(r.Body, "project_id") {
			t.Errorf("an empty project sent %s %s %s", r.Method, r.Path, r.Body)
		}
	}
}

func TestUnknownProject(t *testing.T) {
	p, srv := newProvider(t, "no-such-project")
	ctx := t.Context()
	if _, _, err := p.CreateInstance(ctx, cloud.InstanceSpec{Name: "vm", Region: "sfo3", Size: "s-1vcpu-1gb", Image: "fedora-44-x64"}); !errors.Is(err, cloud.ErrNotFound) || !strings.Contains(err.Error(), "no-such-project") {
		t.Errorf("CreateInstance: %v, want ErrNotFound naming the project", err)
	}
	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1}); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("CreateVolume: %v, want ErrNotFound", err)
	}
	if _, err := p.AllocateAddress(ctx, "sfo3", nil); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("AllocateAddress: %v, want ErrNotFound", err)
	}
	if got := srv.Counts(); got != (dotest.Counts{}) {
		t.Errorf("an unknown project still made %+v", got)
	}
}

// TestProjectNames resolves Config.Project against a team whose projects the emulator holds. A
// name that matches no project fails before anything is made, lists the team's projects by name
// and id, and suggests one whose name differs only in case and punctuation without using it: on
// 2026-10-11 "hidden-token-gaming" was renamed "Hidden Token Gaming" and the error said only
// "not found" (gravel#152).
func TestProjectNames(t *testing.T) {
	const renamedID = "7fd3841c-2b1a-4c3d-8e9f-0a1b2c3d4e5f"
	for _, tc := range []struct {
		name     string
		projects [][2]string // id, name, beside the default project
		project  string
		ok       bool
		contains []string
		absent   []string
	}{
		{name: "exact name", projects: [][2]string{{renamedID, "Hidden Token Gaming"}}, project: "Hidden Token Gaming", ok: true},
		{name: "id", projects: [][2]string{{renamedID, "Hidden Token Gaming"}}, project: renamedID, ok: true},
		{
			name: "renamed", projects: [][2]string{{renamedID, "Hidden Token Gaming"}, {projectID, "Other"}}, project: "hidden-token-gaming",
			contains: []string{
				`project "hidden-token-gaming"`,
				`did you mean "Hidden Token Gaming" (` + renamedID + `)?`,
				`"Default" (` + dotest.DefaultProjectID + `)`,
				`"Hidden Token Gaming" (` + renamedID + `)`,
				`"Other" (` + projectID + `)`,
				"by its id",
			},
		},
		{
			name: "no near match", projects: [][2]string{{renamedID, "Hidden Token Gaming"}}, project: "my-community",
			contains: []string{`"Default" (` + dotest.DefaultProjectID + `)`, `"Hidden Token Gaming" (` + renamedID + `)`},
			absent:   []string{"did you mean"},
		},
		{
			name: "only the default project", project: "hidden-token-gaming",
			contains: []string{`the team's only project is "Default" (` + dotest.DefaultProjectID + `)`},
			absent:   []string{"did you mean"},
		},
		{
			// Punctuation alone is no name: it must not suggest every project with no letters.
			name: "punctuation only", projects: [][2]string{{renamedID, "---"}}, project: "--",
			absent: []string{"did you mean"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts []dotest.Option
			for _, pr := range tc.projects {
				opts = append(opts, dotest.WithProject(pr[0], pr[1]))
			}
			srv := dotest.New(t, append(opts, dotest.WithToken(secretToken))...)
			p, err := digitalocean.New(digitalocean.Config{
				BaseURL: srv.URL(), Token: secretToken, Project: tc.project, PollInterval: time.Millisecond, RetryWait: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.CreateVolume(t.Context(), cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1})
			if tc.ok {
				if err != nil {
					t.Fatalf("CreateVolume in project %q: %v", tc.project, err)
				}
				if got := srv.ProjectResources(renamedID); len(got) != 1 {
					t.Errorf("project %s holds %v, want the volume", renamedID, got)
				}
				return
			}
			if !errors.Is(err, cloud.ErrNotFound) {
				t.Fatalf("CreateVolume in project %q: %v, want an error wrapping cloud.ErrNotFound", tc.project, err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("error %q contains %q", err, unwanted)
				}
			}
			if strings.Contains(err.Error(), secretToken) {
				t.Errorf("error carries the token: %v", err)
			}
			if got := srv.Counts(); got != (dotest.Counts{}) {
				t.Errorf("an unknown project still made %+v", got)
			}
		})
	}
}

func TestPagination(t *testing.T) {
	p, srv := newProvider(t, "", dotest.WithMaxPerPage(2))
	ctx := t.Context()
	for i := range 5 {
		if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: fmt.Sprintf("data-%d", i), Region: "sfo3", SizeGiB: 1, Tags: []string{"t"}}); err != nil {
			t.Fatal(err)
		}
	}
	before := count(srv.Requests(), http.MethodGet, "/v2/volumes")
	vols, err := p.Volumes(ctx, cloud.Filter{Tag: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 5 {
		t.Errorf("Volumes = %d, want 5", len(vols))
	}
	if n := count(srv.Requests(), http.MethodGet, "/v2/volumes") - before; n != 3 {
		t.Errorf("listing 5 volumes 2 a page took %d requests, want 3", n)
	}
	sizes, err := p.Sizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 6 {
		t.Errorf("Sizes = %d, want the emulator's 6", len(sizes))
	}
	regions, err := p.Regions(ctx)
	if err != nil || len(regions) != 3 {
		t.Errorf("Regions = %v, %v; want 3", regions, err)
	}
}

func TestInstanceFilters(t *testing.T) {
	p, _ := newProvider(t, "", dotest.WithMaxPerPage(1))
	a := running(t, p, "vm-a", "gravel", "a")
	running(t, p, "vm-b", "gravel")
	running(t, p, "vm-a")
	for _, tc := range []struct {
		f    cloud.Filter
		want int
	}{
		{cloud.Filter{}, 3},
		{cloud.Filter{Tag: "gravel"}, 2},
		{cloud.Filter{Name: "vm-a"}, 2},
		{cloud.Filter{Tag: "gravel", Name: "vm-a"}, 1},
		{cloud.Filter{Tag: "nope"}, 0},
	} {
		got, err := p.Instances(t.Context(), tc.f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.want {
			t.Errorf("Instances(%+v) = %d, want %d", tc.f, len(got), tc.want)
		}
	}
	got, err := p.Instance(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != cloud.StatusRunning || got.PublicIPv4 == "" || got.PrivateIPv4 == "" || got.Image != "fedora-44-x64" || got.Created.IsZero() {
		t.Errorf("Instance = %+v, want running with both addresses, the image and a creation time", got)
	}
}

func TestEnsureFirewall(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()
	spec := cloud.FirewallSpec{
		Name:     "gravel-cs2",
		Tags:     []string{"gravel-cs2"},
		Inbound:  []cloud.Rule{{Protocol: "udp", Ports: "27015", Addresses: cloud.AnyAddress}, {Protocol: "tcp", Ports: "22", Addresses: []string{"192.0.2.1/32"}}},
		Outbound: cloud.AllowAllOutbound(),
	}
	first, err := p.EnsureFirewall(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || len(first.Outbound) != 3 || first.Outbound[0].Ports != "" {
		t.Errorf("first = %+v, want an id and three outbound rules for every port", first)
	}
	puts := func() int { return count(srv.Requests(), http.MethodPut, "/v2/firewalls") }

	slices.Reverse(spec.Inbound) // order does not matter
	if _, err := p.EnsureFirewall(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if n := puts(); n != 0 {
		t.Errorf("an unchanged spec sent %d updates, want none", n)
	}

	spec.Inbound = []cloud.Rule{{Protocol: "tcp", Ports: "22", Addresses: []string{"192.0.2.2/32"}}}
	spec.Tags = append(spec.Tags, "gravel-extra")
	changed, err := p.EnsureFirewall(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if puts() != 1 || changed.ID != first.ID {
		t.Errorf("a changed spec: %d updates, id %s; want one update keeping %s", puts(), changed.ID, first.ID)
	}
	if len(changed.Inbound) != 1 || changed.Inbound[0].Addresses[0] != "192.0.2.2/32" || len(changed.Tags) != 2 {
		t.Errorf("changed = %+v, want the new rules and tags only", changed.FirewallSpec)
	}
	byTag, err := p.Firewalls(ctx, cloud.Filter{Tag: "gravel-extra"})
	if err != nil || len(byTag) != 1 {
		t.Errorf("Firewalls by tag = %v, %v; want the one", byTag, err)
	}
	if err := p.DeleteFirewall(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteFirewall(ctx, first.ID); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("deleting twice: %v, want ErrNotFound", err)
	}
}

func TestEnsureSSHKey(t *testing.T) {
	p, srv := newProvider(t, "")
	existing := srv.AddSSHKey("john", testKey+" john@earth")
	id, err := p.EnsureSSHKey(t.Context(), "gravel", "  "+testKey+"   gravel@hub\n")
	if err != nil {
		t.Fatal(err)
	}
	if id != fmt.Sprint(existing) {
		t.Errorf("EnsureSSHKey = %s, want the existing key %d", id, existing)
	}
	if n := count(srv.Requests(), http.MethodPost, "/v2/account/keys"); n != 0 {
		t.Errorf("a key the account has was registered again (%d posts)", n)
	}
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOvd6DDSsPu9wZIF4C7Bm0aLLfDN3h4E5S6nD+0e3Pmg other"
	id2, err := p.EnsureSSHKey(t.Context(), "other", other)
	if err != nil || id2 == "" || id2 == id {
		t.Errorf("a new key: %q, %v; want a new id", id2, err)
	}
}

func TestWaitOnErroredAction(t *testing.T) {
	p, srv := newProvider(t, "")
	inst := running(t, p, "vm")
	srv.FailNextAction("power_off")
	op, err := p.StopInstance(t.Context(), inst.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(t.Context(), op); !errors.Is(err, cloud.ErrOperationFailed) {
		t.Errorf("Wait = %v, want ErrOperationFailed", err)
	}
}

func TestWaitDeadline(t *testing.T) {
	p, _ := newProvider(t, "", dotest.WithActionDelay(time.Hour))
	_, op, err := p.CreateInstance(t.Context(), cloud.InstanceSpec{Name: "vm", Region: "sfo3", Size: "s-1vcpu-1gb", Image: "fedora-44-x64"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx, op); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want the context's deadline", err)
	}
}

func TestDeleteWaitsUntilGone(t *testing.T) {
	p, srv := newProvider(t, "", dotest.WithActionDelay(20*time.Millisecond))
	inst := running(t, p, "vm")
	op, err := p.DeleteInstance(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Done() {
		t.Fatal("the delete's operation is done before the Droplet is gone")
	}
	before := count(srv.Requests(), http.MethodGet, "/v2/droplets/"+inst.ID)
	if err := p.Wait(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if n := count(srv.Requests(), http.MethodGet, "/v2/droplets/"+inst.ID) - before; n < 2 {
		t.Errorf("Wait read the Droplet %d times, want it to poll until the 404", n)
	}
	if _, err := p.Instance(t.Context(), inst.ID); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("after the wait: %v, want ErrNotFound", err)
	}
}

func TestResize(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()
	inst := running(t, p, "vm")
	if _, err := p.ResizeInstance(ctx, inst.ID, "s-2vcpu-4gb"); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("resizing a running Droplet: %v, want ErrInvalid", err)
	}
	stop, err := p.StopInstance(ctx, inst.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, stop); err != nil {
		t.Fatal(err)
	}
	again, err := p.StopInstance(ctx, inst.ID, true)
	if err != nil || !again.Done() {
		t.Errorf("stopping a stopped Droplet: %+v, %v; want a done operation", again, err)
	}
	up, err := p.ResizeInstance(ctx, inst.ID, "s-2vcpu-4gb")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, up); err != nil {
		t.Fatal(err)
	}
	// The Droplet keeps its 25 GiB disk, so it can come back down.
	down, err := p.ResizeInstance(ctx, inst.ID, "s-1vcpu-1gb")
	if err != nil {
		t.Fatalf("resizing back down after a CPU-and-memory resize: %v", err)
	}
	if err := p.Wait(ctx, down); err != nil {
		t.Fatal(err)
	}
	actions := count(srv.Requests(), http.MethodPost, "/v2/droplets/"+inst.ID+"/actions")
	for _, size := range []string{"c-2", "no-such-size"} {
		if _, err := p.ResizeInstance(ctx, inst.ID, size); err == nil {
			t.Errorf("resizing to %s succeeded", size)
		}
	}
	if _, err := p.ResizeInstance(ctx, inst.ID, "c-2"); !errors.Is(err, cloud.ErrResizeNotAllowed) {
		t.Errorf("resizing across families: %v, want ErrResizeNotAllowed", err)
	}
	if n := count(srv.Requests(), http.MethodPost, "/v2/droplets/"+inst.ID+"/actions"); n != actions {
		t.Errorf("refused resizes sent %d actions, want none", n-actions)
	}
	d, _ := srv.Droplet(mustAtoi(t, inst.ID))
	if d.Disk != 25 || d.SizeSlug != "s-1vcpu-1gb" || d.Status != "off" {
		t.Errorf("Droplet = disk %d, size %s, %s; want 25, s-1vcpu-1gb, off", d.Disk, d.SizeSlug, d.Status)
	}
}

func TestAdopt(t *testing.T) {
	p, _ := newProvider(t, "")
	inst := running(t, p, "made-by-tofu", "tofu")
	got, err := p.Adopt(t.Context(), inst.ID, []string{"gravel", "gravel-cs2", "tofu"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got.Tags)
	if !slices.Equal(got.Tags, []string{"gravel", "gravel-cs2", "tofu"}) {
		t.Errorf("tags = %v", got.Tags)
	}
	found, err := p.Instances(t.Context(), cloud.Filter{Tag: "gravel-cs2"})
	if err != nil || len(found) != 1 || found[0].ID != inst.ID {
		t.Errorf("Instances by the new tag = %v, %v", found, err)
	}
	if _, err := p.Adopt(t.Context(), "424242", []string{"gravel"}); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("adopting a missing Droplet: %v, want ErrNotFound", err)
	}
}

func TestVolumes(t *testing.T) {
	p, _ := newProvider(t, "")
	ctx := t.Context()
	inst := running(t, p, "vm")
	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "Bad_Name", Region: "sfo3", SizeGiB: 1}); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("a bad name: %v, want ErrInvalid", err)
	}
	far, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "far", Region: "nyc3", SizeGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AttachVolume(ctx, far.ID, inst.ID); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("attaching across regions: %v, want ErrInvalid", err)
	}
	if got := p.VolumeDevice(far); got != "/dev/disk/by-id/scsi-0DO_Volume_far" {
		t.Errorf("VolumeDevice = %s", got)
	}
}

func TestAddresses(t *testing.T) {
	p, _ := newProvider(t, "")
	ctx := t.Context()
	inst := running(t, p, "vm", "gravel")
	a, err := p.AllocateAddress(ctx, "sfo3", []string{"gravel"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Addresses(ctx, cloud.Filter{Tag: "gravel"}); len(got) != 0 {
		t.Errorf("an unassigned address matched a tag: %v", got)
	}
	op, err := p.AssignAddress(ctx, a.IP, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, op); err != nil {
		t.Fatal(err)
	}
	again, err := p.AssignAddress(ctx, a.IP, inst.ID)
	if err != nil || !again.Done() {
		t.Errorf("assigning again: %+v, %v; want a done operation", again, err)
	}
	got, err := p.Addresses(ctx, cloud.Filter{Tag: "gravel"})
	if err != nil || len(got) != 1 || got[0].InstanceID != inst.ID || got[0].Region != "sfo3" {
		t.Errorf("Addresses by the instance's tag = %+v, %v", got, err)
	}
	if got, _ := p.Addresses(ctx, cloud.Filter{Name: a.IP}); len(got) != 1 {
		t.Errorf("Addresses by IP = %v", got)
	}
	if err := p.ReleaseAddress(ctx, a.IP); err != nil {
		t.Fatal(err)
	}
}

// destroyedWithAddress makes a Droplet with a reserved IP assigned, destroys the Droplet and
// waits until it is gone and the address reads unassigned: the state the conformance suite's
// delete step releases the address in.
func destroyedWithAddress(t *testing.T, p *digitalocean.Provider) string {
	t.Helper()
	ctx := t.Context()
	inst := running(t, p, "vm")
	a, err := p.AllocateAddress(ctx, "sfo3", nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := p.AssignAddress(ctx, a.IP, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, op); err != nil {
		t.Fatal(err)
	}
	if op, err = p.DeleteInstance(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, op); err != nil {
		t.Fatal(err)
	}
	got, err := p.Addresses(ctx, cloud.Filter{Name: a.IP})
	if err != nil || len(got) != 1 || got[0].InstanceID != "" {
		t.Fatalf("the address after its Droplet went = %+v, %v; want it there, unassigned", got, err)
	}
	return a.IP
}

// TestReleaseAddressOutlastsTheLock is the live run's leak (gravel#153) against the emulator: the
// first DELETE after the Droplet's destruction is answered 204 and does nothing, the next three
// are refused with 422, and the unassign errored once first. A ReleaseAddress that trusts one
// DELETE (the old one) returns nil with the address still there and fails here; this one sends
// the DELETE again until a GET answers 404.
func TestReleaseAddressOutlastsTheLock(t *testing.T) {
	p, srv := newProvider(t, "", dotest.WithReleaseLock(3))
	srv.FailNextAction("unassign_ip")
	ip := destroyedWithAddress(t, p)
	if err := p.ReleaseAddress(t.Context(), ip); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	if got := srv.Counts().ReservedIPs; got != 0 {
		t.Fatalf("ReleaseAddress returned with %d reserved IPs still in the account", got)
	}
	if got, err := p.Addresses(t.Context(), cloud.Filter{Name: ip}); err != nil || len(got) != 0 {
		t.Errorf("Addresses(%s) after the release = %+v, %v; want none", ip, got, err)
	}
	if n := count(srv.Requests(), http.MethodDelete, "/v2/reserved_ips/"+ip); n != 5 {
		t.Errorf("ReleaseAddress sent %d DELETEs, want 5 (one swallowed, three refused, one that took)", n)
	}
	if err := p.ReleaseAddress(t.Context(), ip); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("ReleaseAddress of a released address: %v, want ErrNotFound", err)
	}
}

// TestReleaseAddressGivesUpAtTheDeadline is an address the API never lets go: the release fails
// at the context's end with ErrOperationFailed, naming the address and the API's last answer,
// and never the token.
func TestReleaseAddressGivesUpAtTheDeadline(t *testing.T) {
	p, srv := newProvider(t, "", dotest.WithReleaseLock(1<<30))
	ip := destroyedWithAddress(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := p.ReleaseAddress(ctx, ip)
	if !errors.Is(err, cloud.ErrOperationFailed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReleaseAddress = %v, want ErrOperationFailed and the deadline", err)
	}
	for _, want := range []string{ip, "422", "The reserved IP action is unprocessable."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", err), secretToken) {
		t.Errorf("error carries the token: %v", err)
	}
	if got := srv.Counts().ReservedIPs; got != 1 {
		t.Errorf("reserved IPs = %d, want the one the API kept", got)
	}
}

func TestReleaseAddressRefusals(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()
	if err := p.ReleaseAddress(ctx, "203.0.113.9"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("ReleaseAddress of an unknown address: %v, want ErrNotFound", err)
	}
	// An assigned address the API refuses with 422 is the caller's mistake, not a lock to wait
	// out: ErrInvalid at once.
	inst := running(t, p, "vm")
	a, err := p.AllocateAddress(ctx, "sfo3", nil)
	if err != nil {
		t.Fatal(err)
	}
	op, err := p.AssignAddress(ctx, a.IP, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(ctx, op); err != nil {
		t.Fatal(err)
	}
	srv.Fault(http.MethodDelete, "/v2/reserved_ips/"+a.IP, http.StatusUnprocessableEntity, 1)
	if err := p.ReleaseAddress(ctx, a.IP); !errors.Is(err, cloud.ErrInvalid) || !strings.Contains(err.Error(), inst.ID) {
		t.Errorf("ReleaseAddress of an assigned address: %v, want ErrInvalid naming droplet %s", err, inst.ID)
	}
}

func TestOperationIDs(t *testing.T) {
	p, _ := newProvider(t, "")
	if err := p.Wait(t.Context(), cloud.Operation{ID: "not-ours"}); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("Wait on a foreign id: %v, want ErrInvalid", err)
	}
	if err := p.Wait(t.Context(), cloud.Operation{}); err != nil {
		t.Errorf("Wait on a done operation: %v", err)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}
