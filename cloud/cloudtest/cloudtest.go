// Package cloudtest is the conformance suite every cloud.Provider passes (ADR-0003): the fake in
// CI, the DigitalOcean provider against its API emulator in CI and against the real API with
// `-tags live`. A provider passes when the lifecycle, the filters that make a half-done run
// findable again, the idempotent firewall, volumes, addresses and its own CanResize matrix all
// behave as the interface documents.
//
// Against a live cloud the suite costs money, so it shares one instance and one volume across
// the lifecycle steps (create, power and resize, detach and attach, addresses, delete), makes at
// most one more instance (the duplicate-name check, which Options.SkipDuplicateInstance drops),
// and deletes everything it made by tag at the end, even after a failure.
package cloudtest

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
)

// PublicKey is the suite's SSH key. Nobody logs in to the suite's instances, so its private
// half was thrown away when it was made; it exists because a cloud wants a real key to accept it.
const PublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE/YktJENnWZ8NpaCnunhEJnbar3Id1Qob11OuYeC9F0 gravel-cloudtest"

// UserData is the cloud-init document the suite's instances boot with: valid, and doing nothing.
const UserData = "#cloud-config\n{}\n"

// Default timings for a live cloud; the fake finishes long before either matters.
const (
	DefaultTimeout = 10 * time.Minute
	DefaultPoll    = 2 * time.Second
)

// Options say what the suite creates with. Every resource it makes is named with Prefix and
// tagged with Tag, so a failed run's leftovers are findable, and the suite deletes them at the
// end unless Keep is set.
type Options struct {
	Region string // a region the provider offers
	Size   string // a small size offered in Region
	Image  string // an image the provider boots
	// ResizeTo is a size Size may be resized to; empty skips the resize step.
	ResizeTo string
	// ResizeCases is the provider's CanResize matrix: each pair of size slugs and whether the
	// provider allows it.
	ResizeCases []ResizeCase
	Prefix      string // name prefix, e.g. "gravel-ct-<random>"
	Tag         string // a tag no other resource carries
	Keep        bool
	// SkipAddresses skips reserved addresses (a provider without them).
	SkipAddresses bool

	// Timeout bounds each call and each wait; zero is DefaultTimeout. A live cloud needs
	// minutes for a create or a resize.
	Timeout time.Duration
	// Poll is how often the suite re-reads a resource it expects to change (an instance that
	// should be gone); zero is DefaultPoll.
	Poll time.Duration
	// ResizeWhileRunningAllowed skips the check that ResizeInstance refuses a running instance.
	// The interface says a resize needs a stopped instance and the fake refuses one that is
	// not, but some clouds' APIs power the instance off by themselves (DigitalOcean's resize
	// action does), so a provider passing that through sets this.
	ResizeWhileRunningAllowed bool
	// SkipDuplicateInstance skips the second instance the duplicate-name check makes, to save a
	// live run its cost and minutes.
	SkipDuplicateInstance bool
}

// ResizeCase is one row of a CanResize matrix.
type ResizeCase struct {
	From, To string
	OK       bool
}

// suite is one run: the provider, its options and what the lifecycle steps share.
type suite struct {
	p    cloud.Provider
	opts Options

	keyID string
	vol   cloud.Volume
	inst  cloud.Instance
	addr  string // the reserved address the addresses step allocated, until it is released
}

// Run runs the suite against the provider newProvider returns. Region, Size, Image, Prefix and
// Tag are required: the cleanup deletes what carries Tag and is named with Prefix, so they must
// be the run's own.
func Run(t *testing.T, newProvider func(t *testing.T) cloud.Provider, opts Options) {
	t.Helper()
	for name, v := range map[string]string{
		"Region": opts.Region, "Size": opts.Size, "Image": opts.Image, "Prefix": opts.Prefix, "Tag": opts.Tag,
	} {
		if v == "" {
			t.Fatalf("cloudtest: Options.%s is required", name)
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	s := &suite{p: newProvider(t), opts: opts}
	if !opts.Keep {
		t.Cleanup(func() { s.cleanup(t) })
	}

	t.Run("catalog", s.catalog)
	t.Run("not-found", s.notFound)
	t.Run("canresize", s.canResize)
	t.Run("firewall", s.firewall)
	t.Run("half-done", s.halfDone)
	t.Run("lifecycle", func(t *testing.T) {
		if !t.Run("create", s.create) {
			t.Skip("cloudtest: the instance was not created; the rest of the lifecycle needs it")
		}
		t.Run("duplicate-name", s.duplicateName)
		t.Run("power-and-resize", s.powerAndResize)
		t.Run("detach-attach", s.detachAttach)
		t.Run("addresses", s.addresses)
		t.Run("delete", s.delete)
	})
}

// ctx is a context bounded by one operation's timeout, cancelled when the (sub)test ends.
func (s *suite) ctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), s.opts.Timeout)
	t.Cleanup(cancel)
	return ctx
}

func (s *suite) name(step string) string { return s.opts.Prefix + "-" + step }

// wait waits for op within the timeout, failing the test if it does not succeed.
func (s *suite) wait(t *testing.T, op cloud.Operation, what string) {
	t.Helper()
	if err := s.p.Wait(s.ctx(t), op); err != nil {
		t.Fatalf("%s: wait for %+v: %v", what, op, err)
	}
}

// eventually polls check until it reports done, failing the test at the timeout with the last
// thing check said. A cloud's reads may lag its operations by a poll or two.
func (s *suite) eventually(t *testing.T, what string, check func(ctx context.Context) (bool, error)) {
	t.Helper()
	ctx := s.ctx(t)
	var last error
	for {
		done, err := check(ctx)
		if err == nil && done {
			return
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: not true within %s (last error: %v)", what, s.opts.Timeout, last)
		case <-time.After(s.opts.Poll):
		}
	}
}

// --- standalone steps ---

func (s *suite) catalog(t *testing.T) {
	regions, err := s.p.Regions(s.ctx(t))
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	i := slices.IndexFunc(regions, func(r cloud.Region) bool { return r.Slug == s.opts.Region })
	switch {
	case i < 0:
		t.Errorf("Regions has no %q: %+v", s.opts.Region, regions)
	case !regions[i].Available:
		t.Errorf("region %q is not available: %+v", s.opts.Region, regions[i])
	}

	sizes, err := s.p.Sizes(s.ctx(t))
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	for _, slug := range []string{s.opts.Size, s.opts.ResizeTo} {
		if slug == "" {
			continue
		}
		size, ok := findSize(sizes, slug)
		if !ok {
			t.Errorf("Sizes has no %q", slug)
			continue
		}
		if !slices.Contains(size.Regions, s.opts.Region) {
			t.Errorf("size %q is not offered in %s: %v", slug, s.opts.Region, size.Regions)
		}
		if size.VCPUs <= 0 || size.MemoryMiB <= 0 || size.DiskGiB <= 0 || size.PriceMonthly < 0 || size.PriceHourly < 0 {
			t.Errorf("size %q has insane fields: %+v", slug, size)
		}
	}
}

func (s *suite) notFound(t *testing.T) {
	if _, err := s.p.Instance(s.ctx(t), "does-not-exist"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf(`Instance("does-not-exist") = %v, want an error wrapping cloud.ErrNotFound`, err)
	}
}

func (s *suite) canResize(t *testing.T) {
	if len(s.opts.ResizeCases) == 0 {
		t.Skip("cloudtest: no ResizeCases")
	}
	sizes, err := s.p.Sizes(s.ctx(t))
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	for _, c := range s.opts.ResizeCases {
		from, okFrom := findSize(sizes, c.From)
		to, okTo := findSize(sizes, c.To)
		if !okFrom || !okTo {
			t.Errorf("%s → %s: a size is not in the catalog", c.From, c.To)
			continue
		}
		err := s.p.CanResize(from, to)
		switch {
		case c.OK && err != nil:
			t.Errorf("CanResize(%s, %s) = %v, want nil", c.From, c.To, err)
		case !c.OK && !errors.Is(err, cloud.ErrResizeNotAllowed):
			t.Errorf("CanResize(%s, %s) = %v, want an error wrapping cloud.ErrResizeNotAllowed", c.From, c.To, err)
		}
	}
}

func (s *suite) firewall(t *testing.T) {
	spec := cloud.FirewallSpec{
		Name:     s.name("fw"),
		Tags:     []string{s.opts.Tag},
		Inbound:  []cloud.Rule{{Protocol: "tcp", Ports: "22", Addresses: []string{"192.0.2.10/32"}}},
		Outbound: cloud.AllowAllOutbound(),
	}
	first, err := s.p.EnsureFirewall(s.ctx(t), spec)
	if err != nil {
		t.Fatalf("EnsureFirewall: %v", err)
	}
	if !s.opts.Keep {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
			defer cancel()
			if err := s.p.DeleteFirewall(ctx, first.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				t.Errorf("DeleteFirewall(%s): %v", first.ID, err)
			}
		})
	}
	if first.ID == "" || first.Name != spec.Name {
		t.Fatalf("EnsureFirewall = %+v, want an id and the name %q", first, spec.Name)
	}
	checkRules(t, "first ensure", first.FirewallSpec, spec)

	again, err := s.p.EnsureFirewall(s.ctx(t), spec)
	if err != nil {
		t.Fatalf("EnsureFirewall again: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("EnsureFirewall again made id %s, want the same %s", again.ID, first.ID)
	}
	checkRules(t, "second ensure", again.FirewallSpec, spec)

	spec.Inbound = []cloud.Rule{{Protocol: "tcp", Ports: "22", Addresses: []string{"192.0.2.11/32"}}}
	changed, err := s.p.EnsureFirewall(s.ctx(t), spec)
	if err != nil {
		t.Fatalf("EnsureFirewall with a new source: %v", err)
	}
	if changed.ID != first.ID {
		t.Errorf("EnsureFirewall with a new source made id %s, want the same %s", changed.ID, first.ID)
	}
	checkRules(t, "changed source", changed.FirewallSpec, spec)

	found, err := s.p.Firewalls(s.ctx(t), cloud.Filter{Name: spec.Name})
	if err != nil {
		t.Fatalf("Firewalls: %v", err)
	}
	if len(found) != 1 || found[0].ID != first.ID {
		t.Fatalf("Firewalls(name %q) = %+v, want exactly %s", spec.Name, found, first.ID)
	}
	checkRules(t, "listed", found[0].FirewallSpec, spec)
}

// halfDone is a run that crashed after making its volume and before its instance: listing by
// tag and by name must find the volume, so the re-run attaches it instead of making another.
func (s *suite) halfDone(t *testing.T) {
	name := s.name("half")
	v, err := s.p.CreateVolume(s.ctx(t), cloud.VolumeSpec{Name: name, Region: s.opts.Region, SizeGiB: 1, Tags: []string{s.opts.Tag}})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if !s.opts.Keep {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
			defer cancel()
			if err := s.p.DeleteVolume(ctx, v.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				t.Errorf("DeleteVolume(%s): %v", v.ID, err)
			}
		})
	}
	for _, f := range []cloud.Filter{{Tag: s.opts.Tag}, {Name: name}, {Tag: s.opts.Tag, Name: name}} {
		vols, err := s.p.Volumes(s.ctx(t), f)
		if err != nil {
			t.Fatalf("Volumes(%+v): %v", f, err)
		}
		if !slices.ContainsFunc(vols, func(x cloud.Volume) bool { return x.ID == v.ID }) {
			t.Errorf("Volumes(%+v) = %+v, missing %s", f, vols, v.ID)
		}
		if f.Name != "" && len(vols) != 1 {
			t.Errorf("Volumes(%+v) = %d volumes, want 1", f, len(vols))
		}
	}
	insts, err := s.p.Instances(s.ctx(t), cloud.Filter{Name: name})
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(insts) != 0 {
		t.Errorf("Instances(name %q) = %+v, want none: the run made no instance", name, insts)
	}
}

// --- the lifecycle, one instance and one volume in order ---

func (s *suite) create(t *testing.T) {
	keyID, err := s.p.EnsureSSHKey(s.ctx(t), s.name("key"), PublicKey)
	if err != nil {
		t.Fatalf("EnsureSSHKey: %v", err)
	}
	again, err := s.p.EnsureSSHKey(s.ctx(t), s.name("key"), PublicKey)
	if err != nil {
		t.Fatalf("EnsureSSHKey again: %v", err)
	}
	if keyID == "" || again != keyID {
		t.Fatalf("EnsureSSHKey = %q then %q, want one non-empty id", keyID, again)
	}
	s.keyID = keyID

	s.vol, err = s.p.CreateVolume(s.ctx(t), cloud.VolumeSpec{Name: s.name("data"), Region: s.opts.Region, SizeGiB: 1, Tags: []string{s.opts.Tag}})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if s.vol.ID == "" || s.vol.Name != s.name("data") || s.vol.Region != s.opts.Region || len(s.vol.InstanceIDs) != 0 {
		t.Fatalf("CreateVolume = %+v, want an id, the name, the region and no instance", s.vol)
	}

	inst, op, err := s.p.CreateInstance(s.ctx(t), cloud.InstanceSpec{
		Name: s.name("vm"), Region: s.opts.Region, Size: s.opts.Size, Image: s.opts.Image,
		UserData: UserData, SSHKeyIDs: []string{keyID}, Tags: []string{s.opts.Tag}, VolumeIDs: []string{s.vol.ID},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.ID == "" {
		t.Fatalf("CreateInstance = %+v, want an id", inst)
	}
	s.inst = inst
	s.wait(t, op, "CreateInstance")

	s.eventually(t, "the instance runs with a public address", func(ctx context.Context) (bool, error) {
		got, err := s.p.Instance(ctx, inst.ID)
		if err == nil {
			s.inst = got
		}
		return err == nil && got.Status == cloud.StatusRunning && got.PublicIPv4 != "", err
	})
	if s.inst.Name != s.name("vm") || s.inst.Region != s.opts.Region || s.inst.Size != s.opts.Size {
		t.Errorf("Instance = %+v, want name %s, region %s, size %s", s.inst, s.name("vm"), s.opts.Region, s.opts.Size)
	}
	if !slices.Contains(s.inst.Tags, s.opts.Tag) {
		t.Errorf("Instance tags = %v, missing %q", s.inst.Tags, s.opts.Tag)
	}
	if !slices.Contains(s.inst.VolumeIDs, s.vol.ID) {
		t.Errorf("Instance volumes = %v, missing %s", s.inst.VolumeIDs, s.vol.ID)
	}
	// CanResize is checked against the disk an instance was created with, so a provider must say.
	if sizes, err := s.p.Sizes(s.ctx(t)); err == nil {
		if size, ok := findSize(sizes, s.opts.Size); ok && s.inst.DiskGiB != size.DiskGiB {
			t.Errorf("Instance.DiskGiB = %d, want %s's %d", s.inst.DiskGiB, s.opts.Size, size.DiskGiB)
		}
	}
	for _, f := range []cloud.Filter{{Tag: s.opts.Tag}, {Name: s.name("vm")}} {
		if n := s.countInstances(t, f, inst.ID); n != 1 {
			t.Errorf("Instances(%+v) lists %s %d times, want once", f, inst.ID, n)
		}
	}
	s.eventually(t, "the volume lists the instance", func(ctx context.Context) (bool, error) {
		v, err := s.findVolume(ctx, s.vol.ID)
		if err == nil {
			s.vol = v
		}
		return err == nil && slices.Equal(v.InstanceIDs, []string{inst.ID}), err
	})
	if dev := s.p.VolumeDevice(s.vol); dev == "" || !path.IsAbs(dev) {
		t.Errorf("VolumeDevice = %q, want an absolute path", dev)
	}
}

// duplicateName makes a second instance with the lifecycle instance's name. A cloud that allows
// it must show both: a caller decides what a duplicate means, and a provider that hid one would
// leave it running and billing unseen.
func (s *suite) duplicateName(t *testing.T) {
	if s.opts.SkipDuplicateInstance {
		t.Skip("cloudtest: SkipDuplicateInstance")
	}
	dup, op, err := s.p.CreateInstance(s.ctx(t), cloud.InstanceSpec{
		Name: s.name("vm"), Region: s.opts.Region, Size: s.opts.Size, Image: s.opts.Image,
		UserData: UserData, SSHKeyIDs: []string{s.keyID}, Tags: []string{s.opts.Tag},
	})
	if err != nil {
		t.Fatalf("CreateInstance with a taken name: %v", err)
	}
	defer func() {
		del, err := s.p.DeleteInstance(s.ctx(t), dup.ID)
		if err != nil {
			t.Errorf("DeleteInstance(%s): %v", dup.ID, err)
			return
		}
		s.wait(t, del, "DeleteInstance of the duplicate")
	}()
	s.wait(t, op, "CreateInstance of the duplicate")
	if dup.ID == s.inst.ID {
		t.Fatalf("the duplicate has the first instance's id %s", dup.ID)
	}
	got, err := s.p.Instances(s.ctx(t), cloud.Filter{Name: s.name("vm")})
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	ids := make([]string, len(got))
	for i, g := range got {
		ids[i] = g.ID
	}
	if len(got) != 2 || !slices.Contains(ids, s.inst.ID) || !slices.Contains(ids, dup.ID) {
		t.Errorf("Instances(name %q) = %v, want both %s and %s", s.name("vm"), ids, s.inst.ID, dup.ID)
	}
}

func (s *suite) powerAndResize(t *testing.T) {
	if s.opts.ResizeTo != "" && !s.opts.ResizeWhileRunningAllowed {
		if op, err := s.p.ResizeInstance(s.ctx(t), s.inst.ID, s.opts.ResizeTo); err == nil {
			_ = s.p.Wait(s.ctx(t), op)
			t.Fatalf("ResizeInstance of a running instance succeeded; the interface says it needs a stopped one")
		}
	}

	op, err := s.p.StopInstance(s.ctx(t), s.inst.ID, true)
	if err != nil {
		t.Fatalf("StopInstance(graceful): %v", err)
	}
	if err := s.p.Wait(s.ctx(t), op); err != nil {
		// A just-booted instance may not answer an ACPI shutdown in the cloud's window; a driver
		// falls back to a hard stop, and so does the suite.
		if !errors.Is(err, cloud.ErrOperationFailed) {
			t.Fatalf("wait for the graceful stop: %v", err)
		}
		t.Logf("the graceful stop failed (%v); stopping hard", err)
		op, err := s.p.StopInstance(s.ctx(t), s.inst.ID, false)
		if err != nil {
			t.Fatalf("StopInstance(hard): %v", err)
		}
		s.wait(t, op, "StopInstance(hard)")
	}
	s.expectStatus(t, cloud.StatusStopped)

	if s.opts.ResizeTo != "" {
		sizes, err := s.p.Sizes(s.ctx(t))
		if err != nil {
			t.Fatalf("Sizes: %v", err)
		}
		from, okFrom := findSize(sizes, s.opts.Size)
		to, okTo := findSize(sizes, s.opts.ResizeTo)
		if !okFrom || !okTo {
			t.Fatalf("Size %q or ResizeTo %q is not in the catalog", s.opts.Size, s.opts.ResizeTo)
		}
		if err := s.p.CanResize(from, to); err != nil {
			t.Fatalf("CanResize(%s, %s) = %v, want nil", s.opts.Size, s.opts.ResizeTo, err)
		}
		op, err := s.p.ResizeInstance(s.ctx(t), s.inst.ID, s.opts.ResizeTo)
		if err != nil {
			t.Fatalf("ResizeInstance: %v", err)
		}
		s.wait(t, op, "ResizeInstance")
		s.eventually(t, "the instance has the new size", func(ctx context.Context) (bool, error) {
			got, err := s.p.Instance(ctx, s.inst.ID)
			s.inst = got
			return err == nil && got.Size == s.opts.ResizeTo, err
		})
	}

	op, err = s.p.StartInstance(s.ctx(t), s.inst.ID)
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	s.wait(t, op, "StartInstance")
	s.expectStatus(t, cloud.StatusRunning)
}

func (s *suite) detachAttach(t *testing.T) {
	op, err := s.p.DetachVolume(s.ctx(t), s.vol.ID, s.inst.ID)
	if err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}
	s.wait(t, op, "DetachVolume")
	s.eventually(t, "the volume is detached", func(ctx context.Context) (bool, error) {
		v, err := s.findVolume(ctx, s.vol.ID)
		return err == nil && len(v.InstanceIDs) == 0, err
	})

	op, err = s.p.AttachVolume(s.ctx(t), s.vol.ID, s.inst.ID)
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	s.wait(t, op, "AttachVolume")
	s.eventually(t, "the volume is attached again", func(ctx context.Context) (bool, error) {
		v, err := s.findVolume(ctx, s.vol.ID)
		return err == nil && slices.Equal(v.InstanceIDs, []string{s.inst.ID}), err
	})
}

func (s *suite) addresses(t *testing.T) {
	if s.opts.SkipAddresses {
		t.Skip("cloudtest: SkipAddresses")
	}
	a, err := s.p.AllocateAddress(s.ctx(t), s.opts.Region, []string{s.opts.Tag})
	if err != nil {
		t.Fatalf("AllocateAddress: %v", err)
	}
	if a.IP == "" || a.Region != s.opts.Region || a.InstanceID != "" {
		t.Fatalf("AllocateAddress = %+v, want an unassigned IP in %s", a, s.opts.Region)
	}
	s.addr = a.IP
	op, err := s.p.AssignAddress(s.ctx(t), a.IP, s.inst.ID)
	if err != nil {
		t.Fatalf("AssignAddress: %v", err)
	}
	s.wait(t, op, "AssignAddress")
	s.eventually(t, "the address points at the instance", func(ctx context.Context) (bool, error) {
		got, err := s.findAddress(ctx, a.IP)
		return err == nil && got.InstanceID == s.inst.ID, err
	})
}

// delete destroys the lifecycle instance: the instance goes, the volume and the address stay
// (detached, unassigned) for the next instance, which is how parking works on a cloud that bills
// a stopped instance (ADR-0003 §5). Then they go too.
func (s *suite) delete(t *testing.T) {
	op, err := s.p.DeleteInstance(s.ctx(t), s.inst.ID)
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	s.wait(t, op, "DeleteInstance")
	s.eventually(t, "the instance is gone", func(ctx context.Context) (bool, error) {
		_, err := s.p.Instance(ctx, s.inst.ID)
		if errors.Is(err, cloud.ErrNotFound) {
			return true, nil
		}
		return false, err
	})
	s.eventually(t, "the volume survives, detached", func(ctx context.Context) (bool, error) {
		v, err := s.findVolume(ctx, s.vol.ID)
		return err == nil && len(v.InstanceIDs) == 0, err
	})
	if s.addr != "" {
		s.eventually(t, "the address survives, unassigned", func(ctx context.Context) (bool, error) {
			a, err := s.findAddress(ctx, s.addr)
			return err == nil && a.InstanceID == "", err
		})
		if err := s.p.ReleaseAddress(s.ctx(t), s.addr); err != nil {
			t.Fatalf("ReleaseAddress: %v", err)
		}
		// ReleaseAddress returns once the address is gone: a provider that returns while the
		// cloud still holds it leaks an address that bills (gravel#153). s.addr stays set until
		// this holds, so the cleanup releases an address this step could not.
		s.eventually(t, "the released address is gone", func(ctx context.Context) (bool, error) {
			return s.addressGone(ctx, s.addr)
		})
		if err := s.p.ReleaseAddress(s.ctx(t), s.addr); !errors.Is(err, cloud.ErrNotFound) {
			t.Errorf("ReleaseAddress of a released address = %v, want an error wrapping cloud.ErrNotFound", err)
		}
		s.addr = ""
	}
	if err := s.p.DeleteVolume(s.ctx(t), s.vol.ID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	vols, err := s.p.Volumes(s.ctx(t), cloud.Filter{Name: s.vol.Name})
	if err != nil {
		t.Fatalf("Volumes: %v", err)
	}
	if slices.ContainsFunc(vols, func(v cloud.Volume) bool { return v.ID == s.vol.ID }) {
		t.Errorf("Volumes still lists %s after DeleteVolume", s.vol.ID)
	}
}

// --- helpers ---

func (s *suite) expectStatus(t *testing.T, want cloud.Status) {
	t.Helper()
	s.eventually(t, "the instance is "+string(want), func(ctx context.Context) (bool, error) {
		got, err := s.p.Instance(ctx, s.inst.ID)
		if err == nil {
			s.inst = got
		}
		return err == nil && got.Status == want, err
	})
}

func (s *suite) countInstances(t *testing.T, f cloud.Filter, id string) int {
	t.Helper()
	got, err := s.p.Instances(s.ctx(t), f)
	if err != nil {
		t.Fatalf("Instances(%+v): %v", f, err)
	}
	n := 0
	for _, i := range got {
		if i.ID == id {
			n++
		}
	}
	return n
}

func (s *suite) findVolume(ctx context.Context, id string) (cloud.Volume, error) {
	vols, err := s.p.Volumes(ctx, cloud.Filter{Tag: s.opts.Tag})
	if err != nil {
		return cloud.Volume{}, err
	}
	for _, v := range vols {
		if v.ID == id {
			return v, nil
		}
	}
	return cloud.Volume{}, fmt.Errorf("volume %s is not listed by tag %q", id, s.opts.Tag)
}

// findAddress lists every address: a reserved address carries no tag on some clouds
// (DigitalOcean), so the suite finds its own by IP.
func (s *suite) findAddress(ctx context.Context, ip string) (cloud.Address, error) {
	addrs, err := s.p.Addresses(ctx, cloud.Filter{})
	if err != nil {
		return cloud.Address{}, err
	}
	for _, a := range addrs {
		if a.IP == ip {
			return a, nil
		}
	}
	return cloud.Address{}, fmt.Errorf("address %s is not listed", ip)
}

// addressGone is whether Addresses no longer lists the address by its IP.
func (s *suite) addressGone(ctx context.Context, ip string) (bool, error) {
	addrs, err := s.p.Addresses(ctx, cloud.Filter{Name: ip})
	if err != nil {
		return false, err
	}
	return len(addrs) == 0, nil
}

func findSize(sizes []cloud.Size, slug string) (cloud.Size, bool) {
	i := slices.IndexFunc(sizes, func(s cloud.Size) bool { return s.Slug == slug })
	if i < 0 {
		return cloud.Size{}, false
	}
	return sizes[i], true
}

// checkRules compares a firewall's tags and rules with the spec's, ignoring order: a cloud may
// hand rules and addresses back in its own order.
func checkRules(t *testing.T, what string, got, want cloud.FirewallSpec) {
	t.Helper()
	if !sameSet(got.Tags, want.Tags) {
		t.Errorf("%s: tags = %v, want %v", what, got.Tags, want.Tags)
	}
	if !sameSet(ruleKeys(got.Inbound), ruleKeys(want.Inbound)) {
		t.Errorf("%s: inbound = %+v, want %+v", what, got.Inbound, want.Inbound)
	}
	if !sameSet(ruleKeys(got.Outbound), ruleKeys(want.Outbound)) {
		t.Errorf("%s: outbound = %+v, want %+v", what, got.Outbound, want.Outbound)
	}
}

func ruleKeys(rules []cloud.Rule) []string {
	keys := make([]string, len(rules))
	for i, r := range rules {
		addrs := slices.Clone(r.Addresses)
		slices.Sort(addrs)
		keys[i] = r.Protocol + "|" + r.Ports + "|" + strings.Join(addrs, ",")
	}
	return keys
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// cleanup deletes what the run left behind: everything tagged with Tag and named with Prefix,
// and the address it allocated. It is best effort and reports what it could not delete, which on
// a live cloud is money still being spent.
func (s *suite) cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
	defer cancel()
	ours := func(name string) bool { return strings.HasPrefix(name, s.opts.Prefix+"-") }

	insts, err := s.p.Instances(ctx, cloud.Filter{Tag: s.opts.Tag})
	if err != nil {
		t.Errorf("cleanup: Instances: %v", err)
	}
	for _, i := range insts {
		if !ours(i.Name) {
			continue
		}
		op, err := s.p.DeleteInstance(ctx, i.ID)
		if err == nil {
			err = s.p.Wait(ctx, op)
		}
		if err != nil && !errors.Is(err, cloud.ErrNotFound) {
			t.Errorf("cleanup: delete instance %s (%s): %v", i.ID, i.Name, err)
		}
	}
	if s.addr != "" {
		// Released means gone: retry until the address no longer lists, not just until a
		// release answered (gravel#153).
		err := s.retry(ctx, func() error {
			if err := s.p.ReleaseAddress(ctx, s.addr); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return err
			}
			gone, err := s.addressGone(ctx, s.addr)
			if err == nil && !gone {
				err = fmt.Errorf("address %s is still listed after its release", s.addr)
			}
			return err
		})
		if err != nil {
			t.Errorf("cleanup: release address %s: %v", s.addr, err)
		}
	}
	vols, err := s.p.Volumes(ctx, cloud.Filter{Tag: s.opts.Tag})
	if err != nil {
		t.Errorf("cleanup: Volumes: %v", err)
	}
	for _, v := range vols {
		if !ours(v.Name) {
			continue
		}
		// A cloud detaches a deleted instance's volumes on its own clock: retry until it has.
		if err := s.retry(ctx, func() error { return s.p.DeleteVolume(ctx, v.ID) }); err != nil {
			t.Errorf("cleanup: delete volume %s (%s): %v", v.ID, v.Name, err)
		}
	}
	fws, err := s.p.Firewalls(ctx, cloud.Filter{Tag: s.opts.Tag})
	if err != nil {
		t.Errorf("cleanup: Firewalls: %v", err)
	}
	for _, fw := range fws {
		if !ours(fw.Name) {
			continue
		}
		if err := s.p.DeleteFirewall(ctx, fw.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			t.Errorf("cleanup: delete firewall %s (%s): %v", fw.ID, fw.Name, err)
		}
	}
}

// retry calls f until it succeeds, reports the resource gone, or ctx ends.
func (s *suite) retry(ctx context.Context, f func() error) error {
	for {
		err := f()
		if err == nil || errors.Is(err, cloud.ErrNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(s.opts.Poll):
		}
	}
}
