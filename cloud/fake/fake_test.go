package fake_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
)

var errBoom = errors.New("boom")

func spec(name string, volumes ...string) cloud.InstanceSpec {
	return cloud.InstanceSpec{Name: name, Region: "fake-1", Size: "small", Image: "fake-image", Tags: []string{"t"}, VolumeIDs: volumes}
}

// running creates an instance and waits for it.
func running(t *testing.T, p *fake.Provider, s cloud.InstanceSpec) cloud.Instance {
	t.Helper()
	i, op, err := p.CreateInstance(t.Context(), s)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	wait(t, p, op)
	got, err := p.Instance(t.Context(), i.ID)
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	return got
}

func wait(t *testing.T, p *fake.Provider, op cloud.Operation) {
	t.Helper()
	if err := p.Wait(t.Context(), op); err != nil {
		t.Fatalf("Wait(%+v): %v", op, err)
	}
}

func do(t *testing.T, p *fake.Provider) func(cloud.Operation, error) {
	t.Helper()
	return func(op cloud.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		wait(t, p, op)
	}
}

func volume(t *testing.T, p *fake.Provider, name, region string) cloud.Volume {
	t.Helper()
	v, err := p.CreateVolume(t.Context(), cloud.VolumeSpec{Name: name, Region: region, SizeGiB: 10, Tags: []string{"t"}})
	if err != nil {
		t.Fatalf("CreateVolume(%s): %v", name, err)
	}
	return v
}

func TestManualClockDrivesOperations(t *testing.T) {
	clock := fake.NewManualClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
	p := fake.New(fake.WithClock(clock), fake.WithOperationDuration(time.Minute))
	i, op, err := p.CreateInstance(t.Context(), spec("vm"))
	if err != nil {
		t.Fatal(err)
	}
	if i.ID != "i-1" || i.Status != cloud.StatusPending || i.PublicIPv4 != "" {
		t.Fatalf("CreateInstance = %+v, want i-1 pending without an address", i)
	}
	if !i.Created.Equal(clock.Now()) {
		t.Errorf("Created = %v, want the clock's %v", i.Created, clock.Now())
	}

	done := make(chan error, 1)
	go func() { done <- p.Wait(context.Background(), op) }()
	for clock.Timers() == 0 { // the Wait is armed
		time.Sleep(time.Millisecond)
	}
	clock.Advance(30 * time.Second)
	if got, _ := p.Instance(t.Context(), i.ID); got.Status != cloud.StatusPending {
		t.Fatalf("half way: status %s, want pending", got.Status)
	}
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v half way", err)
	default:
	}
	clock.Advance(30 * time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got, err := p.Instance(t.Context(), i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != cloud.StatusRunning || got.PublicIPv4 != "203.0.113.1" {
		t.Errorf("after the operation: %+v, want running at 203.0.113.1", got)
	}
}

func TestWaitHonoursTheDeadline(t *testing.T) {
	clock := fake.NewManualClock(time.Now())
	p := fake.New(fake.WithClock(clock), fake.WithMethodDuration("CreateInstance", time.Hour))
	_, op, err := p.CreateInstance(t.Context(), spec("vm"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx, op); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want the deadline", err)
	}
	// Other methods keep the default duration of zero.
	v := volume(t, p, "data", "fake-1")
	_ = v
	if got := p.Counts().PendingOperations; got != 1 {
		t.Errorf("pending operations = %d, want 1", got)
	}
}

func TestFaultInjection(t *testing.T) {
	ctx := t.Context()

	t.Run("FailNext", func(t *testing.T) {
		p := fake.New()
		p.FailNext("CreateInstance", errBoom)
		p.FailNext("CreateInstance", errBoom)
		for range 2 {
			if _, _, err := p.CreateInstance(ctx, spec("vm")); !errors.Is(err, errBoom) {
				t.Fatalf("CreateInstance = %v, want the injected error", err)
			}
		}
		if n := p.Counts().Instances; n != 0 {
			t.Fatalf("a failed call made %d instances", n)
		}
		running(t, p, spec("vm"))
		if got, want := p.Calls(), []string{"CreateInstance", "CreateInstance", "CreateInstance", "Wait", "Instance"}; !slices.Equal(got, want) {
			t.Errorf("Calls = %v, want %v", got, want)
		}
		if n := p.CallCount("CreateInstance"); n != 3 {
			t.Errorf("CallCount = %d, want 3", n)
		}
	})

	t.Run("RateLimitNext", func(t *testing.T) {
		p := fake.New()
		p.RateLimitNext("Instances")
		if _, err := p.Instances(ctx, cloud.Filter{}); !errors.Is(err, cloud.ErrRateLimited) {
			t.Fatalf("Instances = %v, want ErrRateLimited", err)
		}
		if _, err := p.Instances(ctx, cloud.Filter{}); err != nil {
			t.Fatalf("the second call: %v", err)
		}
	})

	t.Run("HangNext", func(t *testing.T) {
		p := fake.New()
		i := running(t, p, spec("vm"))
		p.HangNext("StopInstance")
		op, err := p.StopInstance(ctx, i.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		if err := p.Wait(short, op); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v, want the deadline", err)
		}
		if got, _ := p.Instance(ctx, i.ID); got.Status != cloud.StatusRunning {
			t.Errorf("a hung stop changed the status to %s", got.Status)
		}
		if n := p.Counts().PendingOperations; n != 1 {
			t.Errorf("pending operations = %d, want the hung one", n)
		}
		do(t, p)(p.StopInstance(ctx, i.ID, false)) // only the next one hangs
		if got := p.StopModes(); !slices.Equal(got, []bool{true, false}) {
			t.Errorf("StopModes = %v", got)
		}
	})

	t.Run("FailOperationNext", func(t *testing.T) {
		p := fake.New()
		i := running(t, p, spec("vm"))
		do(t, p)(p.StopInstance(ctx, i.ID, true))
		p.FailOperationNext("ResizeInstance")
		op, err := p.ResizeInstance(ctx, i.ID, "medium")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Wait(ctx, op); !errors.Is(err, cloud.ErrOperationFailed) {
			t.Fatalf("Wait = %v, want ErrOperationFailed", err)
		}
		if got, _ := p.Instance(ctx, i.ID); got.Size != "small" {
			t.Errorf("a failed resize changed the size to %s", got.Size)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		p := fake.New()
		dead, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.Regions(dead); !errors.Is(err, context.Canceled) {
			t.Fatalf("Regions = %v, want context.Canceled", err)
		}
	})
}

func TestResizeRules(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	i := running(t, p, spec("vm"))

	if _, err := p.ResizeInstance(ctx, i.ID, "medium"); !errors.Is(err, cloud.ErrInvalid) {
		t.Fatalf("resize while running = %v, want ErrInvalid", err)
	}
	do(t, p)(p.StopInstance(ctx, i.ID, true))
	if _, err := p.ResizeInstance(ctx, i.ID, "dedicated-2"); !errors.Is(err, cloud.ErrResizeNotAllowed) {
		t.Fatalf("resize across families = %v, want ErrResizeNotAllowed", err)
	}
	if _, err := p.ResizeInstance(ctx, i.ID, "huge"); !errors.Is(err, cloud.ErrInvalid) {
		t.Fatalf("resize to an unknown size = %v, want ErrInvalid", err)
	}
	// Up and back down: the instance kept its 25 GiB disk, so small is reachable again although
	// CanResize(medium, small) refuses for the sizes alone.
	for _, size := range []string{"medium", "large", "small", "small"} {
		do(t, p)(p.ResizeInstance(ctx, i.ID, size))
		if got, _ := p.Instance(ctx, i.ID); got.Size != size {
			t.Fatalf("after resizing to %s the size is %s", size, got.Size)
		}
	}

	// An instance born on the larger disk cannot go below it.
	big := running(t, p, cloud.InstanceSpec{Name: "big", Region: "fake-1", Size: "dedicated-4", Image: "x"})
	do(t, p)(p.StopInstance(ctx, big.ID, false))
	if _, err := p.ResizeInstance(ctx, big.ID, "dedicated-2"); !errors.Is(err, cloud.ErrResizeNotAllowed) {
		t.Fatalf("shrink below the original disk = %v, want ErrResizeNotAllowed", err)
	}
}

func TestValidation(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	for _, name := range []string{"", "Upper", "-lead", "under_score", "a.b", string(make([]byte, 64))} {
		if _, _, err := p.CreateInstance(ctx, spec(name)); !errors.Is(err, cloud.ErrInvalid) {
			t.Errorf("instance name %q: %v, want ErrInvalid", name, err)
		}
		if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: name, Region: "fake-1", SizeGiB: 1}); !errors.Is(err, cloud.ErrInvalid) {
			t.Errorf("volume name %q: %v, want ErrInvalid", name, err)
		}
		if _, err := p.EnsureFirewall(ctx, cloud.FirewallSpec{Name: name}); !errors.Is(err, cloud.ErrInvalid) {
			t.Errorf("firewall name %q: %v, want ErrInvalid", name, err)
		}
	}
	bad := []struct {
		what string
		spec cloud.InstanceSpec
	}{
		{"unknown region", cloud.InstanceSpec{Name: "a", Region: "nowhere", Size: "small", Image: "x"}},
		{"unavailable region", cloud.InstanceSpec{Name: "a", Region: "fake-2", Size: "small", Image: "x"}},
		{"unknown size", cloud.InstanceSpec{Name: "a", Region: "fake-1", Size: "huge", Image: "x"}},
		{"no image", cloud.InstanceSpec{Name: "a", Region: "fake-1", Size: "small"}},
		{"unknown key", cloud.InstanceSpec{Name: "a", Region: "fake-1", Size: "small", Image: "x", SSHKeyIDs: []string{"key-9"}}},
		{"unknown volume", cloud.InstanceSpec{Name: "a", Region: "fake-1", Size: "small", Image: "x", VolumeIDs: []string{"v-9"}}},
		{"bad tag", cloud.InstanceSpec{Name: "a", Region: "fake-1", Size: "small", Image: "x", Tags: []string{"a tag"}}},
	}
	for _, b := range bad {
		if _, _, err := p.CreateInstance(ctx, b.spec); !errors.Is(err, cloud.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", b.what, err)
		}
	}
	for _, r := range []cloud.Rule{
		{Protocol: "sctp", Addresses: cloud.AnyAddress},
		{Protocol: "tcp", Ports: "0", Addresses: cloud.AnyAddress},
		{Protocol: "tcp", Ports: "8000-7000", Addresses: cloud.AnyAddress},
		{Protocol: "icmp", Ports: "1", Addresses: cloud.AnyAddress},
		{Protocol: "udp", Ports: "53"},
		{Protocol: "udp", Ports: "53", Addresses: []string{"203.0.113.4"}},
	} {
		if _, err := p.EnsureFirewall(ctx, cloud.FirewallSpec{Name: "fw", Inbound: []cloud.Rule{r}}); !errors.Is(err, cloud.ErrInvalid) {
			t.Errorf("rule %+v: %v, want ErrInvalid", r, err)
		}
	}
	if _, err := p.EnsureSSHKey(ctx, "k", "not-a-key"); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("EnsureSSHKey(junk) = %v, want ErrInvalid", err)
	}
	if n := p.Counts(); n != (fake.Counts{}) {
		t.Errorf("refused calls made %+v", n)
	}
}

func TestNames(t *testing.T) {
	ctx := t.Context()
	p := fake.New(fake.WithRegions(
		cloud.Region{Slug: "fake-1", Available: true},
		cloud.Region{Slug: "fake-2", Available: true},
	))
	volume(t, p, "data", "fake-1")
	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "fake-1", SizeGiB: 1}); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("a second volume named data in fake-1: %v, want ErrInvalid", err)
	}
	volume(t, p, "data", "fake-2") // another region is another namespace

	a := running(t, p, spec("vm"))
	b := running(t, p, spec("vm"))
	got, err := p.Instances(ctx, cloud.Filter{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Errorf("Instances(name vm) = %+v, want both duplicates", got)
	}
}

func TestVolumesAndAddresses(t *testing.T) {
	ctx := t.Context()
	p := fake.New(fake.WithRegions(
		cloud.Region{Slug: "fake-1", Available: true},
		cloud.Region{Slug: "fake-2", Available: true},
	))
	v := volume(t, p, "data", "fake-1")
	far := volume(t, p, "far", "fake-2")
	i := running(t, p, spec("vm", v.ID))
	other := running(t, p, spec("other"))

	if _, err := p.AttachVolume(ctx, v.ID, other.ID); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("attach a volume attached elsewhere: %v, want ErrInvalid", err)
	}
	if _, err := p.AttachVolume(ctx, far.ID, i.ID); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("attach across regions: %v, want ErrInvalid", err)
	}
	if op, err := p.AttachVolume(ctx, v.ID, i.ID); err != nil || !op.Done() {
		t.Errorf("attach to the instance it is on = %+v, %v; want a done operation", op, err)
	}
	if _, _, err := p.CreateInstance(ctx, spec("thief", v.ID)); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("create with an attached volume: %v, want ErrInvalid", err)
	}
	if err := p.DeleteVolume(ctx, v.ID); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("delete an attached volume: %v, want ErrInvalid", err)
	}
	if _, err := p.DetachVolume(ctx, v.ID, other.ID); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("detach from the wrong instance: %v, want ErrInvalid", err)
	}

	addr, err := p.AllocateAddress(ctx, "fake-1", []string{"t"})
	if err != nil {
		t.Fatal(err)
	}
	if addr.IP != "198.51.100.1" {
		t.Errorf("the first address is %s, want 198.51.100.1", addr.IP)
	}
	do(t, p)(p.AssignAddress(ctx, addr.IP, i.ID))
	if err := p.ReleaseAddress(ctx, addr.IP); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("release an assigned address: %v, want ErrInvalid", err)
	}
	if got, _ := p.Addresses(ctx, cloud.Filter{Tag: "t"}); len(got) != 1 || got[0].InstanceID != i.ID {
		t.Errorf("Addresses(tag t) = %+v, want the address on %s", got, i.ID)
	}

	do(t, p)(p.DeleteInstance(ctx, i.ID))
	if _, err := p.Instance(ctx, i.ID); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("Instance after delete = %v, want ErrNotFound", err)
	}
	vols, _ := p.Volumes(ctx, cloud.Filter{Name: "data"})
	if len(vols) != 1 || len(vols[0].InstanceIDs) != 0 {
		t.Errorf("the volume after its instance went: %+v, want it detached", vols)
	}
	addrs, _ := p.Addresses(ctx, cloud.Filter{Name: addr.IP})
	if len(addrs) != 1 || addrs[0].InstanceID != "" {
		t.Errorf("the address after its instance went: %+v, want it unassigned", addrs)
	}
	if err := p.ReleaseAddress(ctx, addr.IP); err != nil {
		t.Errorf("ReleaseAddress: %v", err)
	}
	if err := p.DeleteVolume(ctx, v.ID); err != nil {
		t.Errorf("DeleteVolume: %v", err)
	}
	for _, err := range []error{
		p.DeleteVolume(ctx, v.ID), p.ReleaseAddress(ctx, addr.IP), p.DeleteFirewall(ctx, "fw-1"),
	} {
		if !errors.Is(err, cloud.ErrNotFound) {
			t.Errorf("a second delete = %v, want ErrNotFound", err)
		}
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	s := cloud.FirewallSpec{
		Name: "fw", Tags: []string{"t"},
		Inbound:  []cloud.Rule{{Protocol: "tcp", Ports: "22", Addresses: []string{"192.0.2.10/32"}}},
		Outbound: cloud.AllowAllOutbound(),
	}
	first, err := p.EnsureFirewall(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.EnsureFirewall(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "fw-1" || fmt.Sprint(first) != fmt.Sprint(again) {
		t.Errorf("EnsureFirewall twice: %+v then %+v", first, again)
	}
	// The returned firewall is a copy: changing it changes nothing in the cloud.
	first.Inbound[0].Addresses[0] = "0.0.0.0/0"
	s.Inbound[0].Addresses[0] = "192.0.2.99/32" // nor does changing the caller's spec afterwards
	got, _ := p.Firewalls(ctx, cloud.Filter{Tag: "t"})
	if len(got) != 1 || got[0].Inbound[0].Addresses[0] != "192.0.2.10/32" {
		t.Errorf("Firewalls = %+v, want the rule as ensured", got)
	}
	if n := p.Counts().Firewalls; n != 1 {
		t.Errorf("%d firewalls, want 1", n)
	}

	k1, err := p.EnsureSSHKey(ctx, "a", "ssh-ed25519 AAAAkey one@host")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := p.EnsureSSHKey(ctx, "b", "ssh-ed25519 AAAAkey another-comment")
	if err != nil {
		t.Fatal(err)
	}
	k3, _ := p.EnsureSSHKey(ctx, "c", "ssh-ed25519 AAAAother")
	if k1 != "key-1" || k2 != k1 || k3 != "key-2" {
		t.Errorf("SSH key ids %s %s %s, want key-1 key-1 key-2", k1, k2, k3)
	}
}

func TestAdopt(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	i := running(t, p, spec("vm"))
	got, err := p.Adopt(ctx, i.ID, []string{"gravel", "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Tags, []string{"t", "gravel"}) {
		t.Errorf("tags after Adopt = %v", got.Tags)
	}
	if found, _ := p.Instances(ctx, cloud.Filter{Tag: "gravel"}); len(found) != 1 {
		t.Errorf("the adopted instance is not found by its new tag: %+v", found)
	}
	if _, err := p.Adopt(ctx, "i-99", []string{"gravel"}); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("Adopt(unknown) = %v, want ErrNotFound", err)
	}
}

func TestListingsAreOrderedCopies(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	for n := range 11 {
		volume(t, p, fmt.Sprintf("v%d", n), "fake-1")
	}
	vols, err := p.Volumes(ctx, cloud.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for n, v := range vols {
		if want := fmt.Sprintf("v-%d", n+1); v.ID != want {
			t.Fatalf("volume %d is %s, want %s (oldest first, not by string)", n, v.ID, want)
		}
	}
	i := running(t, p, spec("vm"))
	i.Tags[0] = "changed"
	if got, _ := p.Instance(ctx, i.ID); got.Tags[0] != "t" {
		t.Errorf("changing a returned instance changed the cloud: %v", got.Tags)
	}
}

func TestCatalogOptions(t *testing.T) {
	ctx := t.Context()
	p := fake.New(fake.WithSizes(cloud.Size{Slug: "only", VCPUs: 1, MemoryMiB: 512, DiskGiB: 10, Family: "f", Regions: []string{"fake-1"}}))
	sizes, err := p.Sizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes[0].Slug != "only" {
		t.Errorf("Sizes = %+v", sizes)
	}
	if _, _, err := p.CreateInstance(ctx, spec("vm")); !errors.Is(err, cloud.ErrInvalid) {
		t.Errorf("a size not in the catalog: %v, want ErrInvalid", err)
	}
	for _, s := range fake.DefaultSizes() {
		if !s.BillsWhenStopped {
			t.Errorf("default size %s does not bill when stopped", s.Slug)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	p := fake.New(fake.WithOperationDuration(time.Millisecond))
	var wg sync.WaitGroup
	for n := range 16 {
		wg.Go(func() {
			ctx := context.Background()
			v, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: fmt.Sprintf("v%d", n), Region: "fake-1", SizeGiB: 1})
			if err != nil {
				t.Error(err)
				return
			}
			i, op, err := p.CreateInstance(ctx, spec(fmt.Sprintf("vm%d", n), v.ID))
			if err == nil {
				err = p.Wait(ctx, op)
			}
			if err == nil {
				op, err = p.DeleteInstance(ctx, i.ID)
			}
			if err == nil {
				err = p.Wait(ctx, op)
			}
			if err == nil {
				err = p.DeleteVolume(ctx, v.ID)
			}
			if err != nil {
				t.Error(err)
			}
			_, _ = p.Instances(ctx, cloud.Filter{Tag: "t"})
			_ = p.Counts()
		})
	}
	wg.Wait()
	if s := p.Counts(); s.Instances != 0 || s.Volumes != 0 || s.PendingOperations != 0 {
		t.Errorf("after every goroutine cleaned up: %+v", s)
	}
}

func TestVolumeSnapshots(t *testing.T) {
	ctx := t.Context()
	p := fake.New(fake.WithRegions(
		cloud.Region{Slug: "fake-1", Available: true},
		cloud.Region{Slug: "fake-3", Available: true},
	))
	src, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "fake-1", SizeGiB: 50, Tags: []string{"t"}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := p.CreateVolumeSnapshot(ctx, src.ID, "seed", []string{"t"})
	if err != nil {
		t.Fatalf("CreateVolumeSnapshot: %v", err)
	}
	if snap.Region != "fake-1" || snap.MinVolumeGiB != 50 || snap.SizeGiB != 50 {
		t.Errorf("snapshot = %+v, want fake-1 and 50 GiB", snap)
	}
	if p.SourceSnapshot(src.ID) != "" {
		t.Errorf("an empty volume has source %q", p.SourceSnapshot(src.ID))
	}

	v, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "copy", Region: "fake-1", SizeGiB: 60, FromSnapshot: &cloud.SnapshotRef{Name: "seed"}})
	if err != nil {
		t.Fatalf("CreateVolume from the snapshot: %v", err)
	}
	if got := p.SourceSnapshot(v.ID); got != snap.ID {
		t.Errorf("SourceSnapshot = %q, want %s", got, snap.ID)
	}

	for _, c := range []struct {
		spec cloud.VolumeSpec
		want error
		says string
	}{
		{cloud.VolumeSpec{Name: "far", Region: "fake-3", SizeGiB: 50, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID}}, cloud.ErrInvalid, "the snapshot is in fake-1"},
		{cloud.VolumeSpec{Name: "far", Region: "fake-3", SizeGiB: 50, FromSnapshot: &cloud.SnapshotRef{Name: "seed"}}, cloud.ErrNotFound, "fake-3"},
		{cloud.VolumeSpec{Name: "small", Region: "fake-1", SizeGiB: 49, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID}}, cloud.ErrInvalid, "at least 50 GiB"},
		{cloud.VolumeSpec{Name: "both", Region: "fake-1", SizeGiB: 50, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID, Name: "seed"}}, cloud.ErrInvalid, "not both"},
		{cloud.VolumeSpec{Name: "none", Region: "fake-1", SizeGiB: 50, FromSnapshot: &cloud.SnapshotRef{}}, cloud.ErrInvalid, "neither"},
		{cloud.VolumeSpec{Name: "gone", Region: "fake-1", SizeGiB: 50, FromSnapshot: &cloud.SnapshotRef{ID: "snap-99"}}, cloud.ErrNotFound, "snap-99"},
	} {
		_, err := p.CreateVolume(ctx, c.spec)
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("CreateVolume(%s) = %v, want %v saying %q", c.spec.Name, err, c.want, c.says)
		}
	}
	if n := p.Counts(); n.Volumes != 2 || n.Snapshots != 1 {
		t.Errorf("Counts = %+v, want the two volumes and the snapshot", n)
	}

	// Names need not be unique, which makes a name ambiguous.
	dup, err := p.CreateVolumeSnapshot(ctx, v.ID, "seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.FindVolumeSnapshot(ctx, "seed", "fake-1"); !errors.Is(err, cloud.ErrInvalid) || !strings.Contains(err.Error(), dup.ID) {
		t.Errorf("FindVolumeSnapshot of a shared name = %v, want ErrInvalid naming both", err)
	}
	if list, err := p.ListVolumeSnapshots(ctx, cloud.Filter{Tag: "t"}); err != nil || len(list) != 1 || list[0].ID != snap.ID {
		t.Errorf("ListVolumeSnapshots(tag t) = %+v, %v, want only %s", list, err, snap.ID)
	}

	// The snapshot outlives its volume.
	if err := p.DeleteVolume(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetVolumeSnapshot(ctx, snap.ID); err != nil {
		t.Errorf("GetVolumeSnapshot after its volume went: %v", err)
	}
	for _, id := range []string{snap.ID, dup.ID} {
		if err := p.DeleteVolumeSnapshot(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.DeleteVolumeSnapshot(ctx, snap.ID); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("deleting twice: %v, want ErrNotFound", err)
	}
	if p.SourceSnapshot(v.ID) != snap.ID {
		t.Error("a volume forgot its source when the snapshot went")
	}
}

func TestVolumeSnapshotFaults(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	v := volume(t, p, "data", "fake-1")
	p.FailNext("CreateVolumeSnapshot", errBoom)
	if _, err := p.CreateVolumeSnapshot(ctx, v.ID, "seed", nil); !errors.Is(err, errBoom) {
		t.Errorf("CreateVolumeSnapshot = %v, want the injected error", err)
	}
	p.RateLimitNext("ListVolumeSnapshots")
	if _, err := p.ListVolumeSnapshots(ctx, cloud.Filter{}); !errors.Is(err, cloud.ErrRateLimited) {
		t.Errorf("ListVolumeSnapshots = %v, want ErrRateLimited", err)
	}
	p.HangNext("CreateVolumeSnapshot")
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := p.CreateVolumeSnapshot(short, v.ID, "seed", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a hung CreateVolumeSnapshot = %v, want the deadline", err)
	}
	if n := p.Counts().Snapshots; n != 0 {
		t.Errorf("failed and hung calls made %d snapshots", n)
	}
	if _, err := p.CreateVolumeSnapshot(ctx, v.ID, "seed", nil); err != nil {
		t.Errorf("only the next call hangs: %v", err)
	}
	if _, err := p.CreateVolumeSnapshot(ctx, "v-99", "seed", nil); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("a snapshot of a volume nothing has: %v, want ErrNotFound", err)
	}
}
