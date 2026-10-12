// Package fake is a deterministic in-memory cloud.Provider for tests of the gravel CLI and the
// cloud-VM driver (ADR-0003, ADR-0020). It keeps DigitalOcean's rules where a caller could
// otherwise get away with something a real cloud refuses: a resize needs a stopped instance and
// may never shrink the root disk or leave its family, names have DigitalOcean's shape, two
// volumes in a region may not share a name, and two instances or two snapshots may (so a caller
// that wants idempotency has to find what exists itself), and a volume is made from a snapshot
// only in the snapshot's region and never smaller than the volume it was taken of. It passes the
// conformance suite in cloud/cloudtest.
//
// Asynchronous calls return an operation that completes on the fake's Clock after a configurable
// duration (none by default). A test injects faults by method name: a call that fails, one that
// is rate limited, an operation that never completes or one the cloud reports as failed.
package fake

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravel-project/gravel/cloud"
)

// Provider is the fake cloud. The zero value is not usable; call New. It is safe for concurrent
// use.
type Provider struct {
	mu sync.Mutex

	clock      Clock
	opDuration time.Duration
	durations  map[string]time.Duration // per method, over opDuration
	regions    []cloud.Region
	sizes      []cloud.Size

	seq       map[string]int // the last id issued per kind ("i", "v", …)
	publicN   int            // the last instance address issued
	privateN  int
	addressN  int
	instances map[string]*instance
	volumes   map[string]*volume
	snapshots map[string]*snapshot
	firewalls map[string]*firewall
	addresses map[string]*address // by IP
	keys      []sshKey
	ops       map[string]*operation

	fail      map[string][]error // injected errors for the next calls, per method
	hang      map[string]int     // next operations of a method that never complete
	failOps   map[string]int     // next operations of a method the cloud reports as failed
	calls     []string
	stopModes []bool // the graceful flag of every StopInstance, in order
}

type instance struct {
	seq int
	cloud.Instance
	// diskGiB is the root disk the instance was created with. A resize never changes it, so it
	// is what CanResize must compare a target against, not the current size's disk.
	diskGiB int
}

type volume struct {
	seq int
	cloud.Volume
	fromSnapshot string // the id of the snapshot it was made from, if any
}

type snapshot struct {
	seq int
	cloud.Snapshot
}

type firewall struct {
	seq int
	cloud.Firewall
}

type address struct {
	seq int
	cloud.Address
	tags []string
}

type sshKey struct {
	id, name, publicKey string
}

type operation struct {
	cloud.Operation
	seq    int
	doneAt time.Time
	hang   bool
	done   bool
	failed bool         // injected: the cloud reports the operation failed and nothing changes
	apply  func() error // the change, made when the operation completes
	err    error        // the outcome once done
}

// Option configures a Provider.
type Option func(*Provider)

// WithClock makes operations complete on c instead of the wall clock.
func WithClock(c Clock) Option {
	return func(p *Provider) { p.clock = c }
}

// WithOperationDuration is how long every asynchronous operation takes; the default is zero, so
// an operation is done by the time anybody looks.
func WithOperationDuration(d time.Duration) Option {
	return func(p *Provider) { p.opDuration = d }
}

// WithMethodDuration is how long the operations of one method take (by interface method name,
// "ResizeInstance"), over WithOperationDuration.
func WithMethodDuration(method string, d time.Duration) Option {
	return func(p *Provider) { p.durations[method] = d }
}

// WithRegions replaces the default regions.
func WithRegions(regions ...cloud.Region) Option {
	return func(p *Provider) { p.regions = slices.Clone(regions) }
}

// WithSizes replaces the default sizes.
func WithSizes(sizes ...cloud.Size) Option {
	return func(p *Provider) {
		p.sizes = make([]cloud.Size, len(sizes))
		for i, s := range sizes {
			p.sizes[i] = cloneSize(s)
		}
	}
}

// DefaultRegions are the fake's regions: "fake-1", available, and "fake-2", which exists but
// the account may not create in, so a test can see Available honoured.
func DefaultRegions() []cloud.Region {
	return []cloud.Region{
		{Slug: "fake-1", Name: "Fake 1", Available: true},
		{Slug: "fake-2", Name: "Fake 2", Available: false},
	}
}

// DefaultSizes are the fake's sizes, shaped like DigitalOcean's: a shared-CPU "basic" family
// whose disks grow with the size, and a dedicated "cpu" family. Every size bills while stopped,
// as a Droplet does, so the driver's parking path is the destroy-and-keep-the-volume one.
func DefaultSizes() []cloud.Size {
	regions := []string{"fake-1", "fake-2"}
	size := func(slug string, vcpus, mem, disk int, monthly float64, family string, dedicated bool) cloud.Size {
		return cloud.Size{
			Slug: slug, VCPUs: vcpus, MemoryMiB: mem, DiskGiB: disk,
			PriceHourly: monthly / 672, PriceMonthly: monthly,
			Family: family, DedicatedCPU: dedicated, BillsWhenStopped: true,
			Regions: slices.Clone(regions),
		}
	}
	return []cloud.Size{
		size("small", 1, 1024, 25, 6, "basic", false),
		size("medium", 2, 4096, 80, 24, "basic", false),
		size("large", 4, 8192, 160, 48, "basic", false),
		size("dedicated-2", 2, 4096, 25, 42, "cpu", true),
		size("dedicated-4", 4, 8192, 50, 84, "cpu", true),
	}
}

// New is an empty fake cloud with the default catalog and the wall clock.
func New(opts ...Option) *Provider {
	p := &Provider{
		clock:     realClock{},
		durations: map[string]time.Duration{},
		regions:   DefaultRegions(),
		sizes:     DefaultSizes(),
		seq:       map[string]int{},
		instances: map[string]*instance{},
		volumes:   map[string]*volume{},
		snapshots: map[string]*snapshot{},
		firewalls: map[string]*firewall{},
		addresses: map[string]*address{},
		ops:       map[string]*operation{},
		fail:      map[string][]error{},
		hang:      map[string]int{},
		failOps:   map[string]int{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

var _ cloud.Provider = (*Provider)(nil)

// Name is "fake".
func (p *Provider) Name() string { return "fake" }

// --- fault injection and inspection ---

// FailNext makes the next call of method (the interface method's name, "CreateInstance") return
// err without changing anything. Calls queue: two FailNext fail the next two calls.
func (p *Provider) FailNext(method string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail[method] = append(p.fail[method], err)
}

// RateLimitNext makes the next call of method fail with an error wrapping cloud.ErrRateLimited.
func (p *Provider) RateLimitNext(method string) {
	p.FailNext(method, fmt.Errorf("fake: %s: %w", method, cloud.ErrRateLimited))
}

// HangNext makes the next operation method starts never complete: the call itself succeeds and
// the resource stays as it was (an instance being created stays pending), and Wait returns the
// context's error. CreateVolumeSnapshot, which a cloud answers without an operation, hangs
// itself: the next call blocks until its context ends, makes nothing and returns the context's
// error.
func (p *Provider) HangNext(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hang[method]++
}

// FailOperationNext makes the next operation method starts complete as failed: the call
// succeeds, nothing changes, and Wait returns an error wrapping cloud.ErrOperationFailed. It is
// how a test sees a resize the cloud gave up on.
func (p *Provider) FailOperationNext(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOps[method]++
}

// Calls are the methods called so far, in order: every call that would reach a cloud's API (so
// not Name, CanResize or VolumeDevice), including the ones that failed. A test asserts with it
// that a re-run made no second CreateInstance.
func (p *Provider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// CallCount is how many times method was called.
func (p *Provider) CallCount(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c == method {
			n++
		}
	}
	return n
}

// StopModes are the graceful flags of every StopInstance call, in order.
func (p *Provider) StopModes() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.stopModes)
}

// Counts is how many of each resource the fake holds, for asserting that a re-run made nothing
// new.
type Counts struct {
	Instances, Volumes, Snapshots, Firewalls, Addresses, SSHKeys int
	// PendingOperations are operations not complete yet, the hung ones included.
	PendingOperations int
}

// Counts counts the fake's resources after completing every operation that is due.
func (p *Provider) Counts() Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settleLocked()
	s := Counts{
		Instances: len(p.instances), Volumes: len(p.volumes), Snapshots: len(p.snapshots),
		Firewalls: len(p.firewalls), Addresses: len(p.addresses), SSHKeys: len(p.keys),
	}
	for _, o := range p.ops {
		if !o.done {
			s.PendingOperations++
		}
	}
	return s
}

// --- catalog ---

// Regions are the catalog's regions.
func (p *Provider) Regions(ctx context.Context) ([]cloud.Region, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Regions"); err != nil {
		return nil, err
	}
	return slices.Clone(p.regions), nil
}

// Sizes are the catalog's sizes.
func (p *Provider) Sizes(ctx context.Context) ([]cloud.Size, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Sizes"); err != nil {
		return nil, err
	}
	out := make([]cloud.Size, len(p.sizes))
	for i, s := range p.sizes {
		out[i] = cloneSize(s)
	}
	return out, nil
}

// CanResize is DigitalOcean's rule: within one family, and never to a size whose root disk is
// smaller than from's (the disk cannot shrink, and a CPU-and-memory resize keeps it). A size to
// itself is allowed and changes nothing.
func (p *Provider) CanResize(from, to cloud.Size) error {
	return canResize(from, to)
}

func canResize(from, to cloud.Size) error {
	if from.Family != to.Family {
		return fmt.Errorf("fake: resize %s to %s: family %q is not %q: %w",
			from.Slug, to.Slug, from.Family, to.Family, cloud.ErrResizeNotAllowed)
	}
	if to.DiskGiB < from.DiskGiB {
		return fmt.Errorf("fake: resize %s to %s: the root disk would shrink from %d to %d GiB: %w",
			from.Slug, to.Slug, from.DiskGiB, to.DiskGiB, cloud.ErrResizeNotAllowed)
	}
	return nil
}

// --- instances ---

// Instances lists the instances matching f, oldest first.
func (p *Provider) Instances(ctx context.Context, f cloud.Filter) ([]cloud.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Instances"); err != nil {
		return nil, err
	}
	var out []cloud.Instance
	for _, i := range sortedBySeq(p.instances, func(i *instance) int { return i.seq }) {
		if matches(f, i.Name, i.Tags) {
			out = append(out, cloneInstance(i.Instance))
		}
	}
	return out, nil
}

// Instance reads one instance.
func (p *Provider) Instance(ctx context.Context, id string) (cloud.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Instance"); err != nil {
		return cloud.Instance{}, err
	}
	i, err := p.instanceLocked(id)
	if err != nil {
		return cloud.Instance{}, err
	}
	return cloneInstance(i.Instance), nil
}

// CreateInstance makes a pending instance with its volumes attached; the operation makes it
// running and gives it its addresses.
func (p *Provider) CreateInstance(ctx context.Context, spec cloud.InstanceSpec) (cloud.Instance, cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const method = "CreateInstance"
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Instance{}, cloud.Operation{}, err
	}
	if err := validName("instance", spec.Name); err != nil {
		return cloud.Instance{}, cloud.Operation{}, err
	}
	if err := p.regionLocked(spec.Region); err != nil {
		return cloud.Instance{}, cloud.Operation{}, err
	}
	size, err := p.sizeLocked(spec.Size, spec.Region)
	if err != nil {
		return cloud.Instance{}, cloud.Operation{}, err
	}
	if spec.Image == "" {
		return cloud.Instance{}, cloud.Operation{}, fmt.Errorf("fake: instance %q names no image: %w", spec.Name, cloud.ErrInvalid)
	}
	if err := validTags(spec.Tags); err != nil {
		return cloud.Instance{}, cloud.Operation{}, err
	}
	for _, k := range spec.SSHKeyIDs {
		if !slices.ContainsFunc(p.keys, func(s sshKey) bool { return s.id == k }) {
			return cloud.Instance{}, cloud.Operation{}, fmt.Errorf("fake: instance %q: no SSH key %q: %w", spec.Name, k, cloud.ErrInvalid)
		}
	}
	for _, vid := range spec.VolumeIDs {
		v, ok := p.volumes[vid]
		switch {
		case !ok:
			return cloud.Instance{}, cloud.Operation{}, fmt.Errorf("fake: instance %q: no volume %q: %w", spec.Name, vid, cloud.ErrInvalid)
		case v.Region != spec.Region:
			return cloud.Instance{}, cloud.Operation{}, fmt.Errorf("fake: instance %q: volume %q is in %s, not %s: %w", spec.Name, vid, v.Region, spec.Region, cloud.ErrInvalid)
		case len(v.InstanceIDs) > 0:
			return cloud.Instance{}, cloud.Operation{}, fmt.Errorf("fake: instance %q: volume %q is attached to %s: %w", spec.Name, vid, v.InstanceIDs[0], cloud.ErrInvalid)
		}
	}

	i := &instance{
		seq: p.nextSeqLocked("i"),
		Instance: cloud.Instance{
			Name: spec.Name, Region: spec.Region, Size: spec.Size, DiskGiB: size.DiskGiB, Image: spec.Image,
			Status: cloud.StatusPending, Tags: dedupe(spec.Tags), VolumeIDs: dedupe(spec.VolumeIDs),
			Created: p.clock.Now(),
		},
		diskGiB: size.DiskGiB,
	}
	i.ID = "i-" + strconv.Itoa(i.seq)
	p.instances[i.ID] = i
	// The volumes are attached as part of the create, as DigitalOcean does: they are taken now,
	// so a second create cannot claim them while this one is pending.
	for _, vid := range i.VolumeIDs {
		p.volumes[vid].InstanceIDs = []string{i.ID}
	}
	op := p.newOpLocked(method, "create", i.ID, func() error {
		if _, ok := p.instances[i.ID]; !ok {
			return fmt.Errorf("instance %s is gone", i.ID)
		}
		p.publicN++
		p.privateN++
		i.Status = cloud.StatusRunning
		i.PublicIPv4 = "203.0.113." + strconv.Itoa(octet(p.publicN))
		i.PrivateIPv4 = "10.10.0." + strconv.Itoa(octet(p.privateN))
		return nil
	})
	return cloneInstance(i.Instance), op, nil
}

// DeleteInstance destroys an instance once its operation completes: its volumes are detached
// and survive, its reserved addresses are unassigned and survive.
func (p *Provider) DeleteInstance(ctx context.Context, id string) (cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const method = "DeleteInstance"
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Operation{}, err
	}
	if _, err := p.instanceLocked(id); err != nil {
		return cloud.Operation{}, err
	}
	return p.newOpLocked(method, "delete", id, func() error {
		i, ok := p.instances[id]
		if !ok {
			return nil // deleted twice: the outcome is the same
		}
		for _, vid := range i.VolumeIDs {
			if v, ok := p.volumes[vid]; ok {
				v.InstanceIDs = slices.DeleteFunc(v.InstanceIDs, func(s string) bool { return s == id })
			}
		}
		for _, a := range p.addresses {
			if a.InstanceID == id {
				a.InstanceID = ""
			}
		}
		delete(p.instances, id)
		return nil
	}), nil
}

// StartInstance powers an instance on; starting a running one changes nothing.
func (p *Provider) StartInstance(ctx context.Context, id string) (cloud.Operation, error) {
	return p.power(ctx, "StartInstance", "start", id, cloud.StatusRunning)
}

// StopInstance powers an instance off; stopping a stopped one changes nothing. The fake keeps
// the graceful flag for StopModes and otherwise treats both kinds alike.
func (p *Provider) StopInstance(ctx context.Context, id string, graceful bool) (cloud.Operation, error) {
	p.mu.Lock()
	p.stopModes = append(p.stopModes, graceful)
	p.mu.Unlock()
	return p.power(ctx, "StopInstance", "stop", id, cloud.StatusStopped)
}

func (p *Provider) power(ctx context.Context, method, kind, id string, to cloud.Status) (cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Operation{}, err
	}
	i, err := p.instanceLocked(id)
	if err != nil {
		return cloud.Operation{}, err
	}
	if i.Status != cloud.StatusRunning && i.Status != cloud.StatusStopped {
		return cloud.Operation{}, fmt.Errorf("fake: %s instance %s: it is %s: %w", kind, id, i.Status, cloud.ErrInvalid)
	}
	return p.newOpLocked(method, kind, id, func() error {
		i, ok := p.instances[id]
		if !ok {
			return fmt.Errorf("instance %s is gone", id)
		}
		i.Status = to
		return nil
	}), nil
}

// ResizeInstance changes a stopped instance's size. The target must be offered in the
// instance's region and allowed by CanResize against the disk the instance was created with,
// which a resize never changes: so small → medium → small is fine on one instance although
// CanResize(medium, small) is refused for the sizes alone.
func (p *Provider) ResizeInstance(ctx context.Context, id, size string) (cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const method = "ResizeInstance"
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Operation{}, err
	}
	i, err := p.instanceLocked(id)
	if err != nil {
		return cloud.Operation{}, err
	}
	if i.Status != cloud.StatusStopped {
		return cloud.Operation{}, fmt.Errorf("fake: resize instance %s: it is %s, not stopped: %w", id, i.Status, cloud.ErrInvalid)
	}
	to, err := p.sizeLocked(size, i.Region)
	if err != nil {
		return cloud.Operation{}, err
	}
	from, err := p.sizeLocked(i.Size, i.Region)
	if err != nil {
		return cloud.Operation{}, err
	}
	from.DiskGiB = i.diskGiB
	if err := canResize(from, to); err != nil {
		return cloud.Operation{}, err
	}
	return p.newOpLocked(method, "resize", id, func() error {
		i, ok := p.instances[id]
		if !ok {
			return fmt.Errorf("instance %s is gone", id)
		}
		i.Size = size
		return nil
	}), nil
}

// Adopt adds tags to an existing instance.
func (p *Provider) Adopt(ctx context.Context, id string, tags []string) (cloud.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Adopt"); err != nil {
		return cloud.Instance{}, err
	}
	i, err := p.instanceLocked(id)
	if err != nil {
		return cloud.Instance{}, err
	}
	if err := validTags(tags); err != nil {
		return cloud.Instance{}, err
	}
	i.Tags = dedupe(append(i.Tags, tags...))
	return cloneInstance(i.Instance), nil
}

// --- volumes ---

// Volumes lists the volumes matching f, oldest first.
func (p *Provider) Volumes(ctx context.Context, f cloud.Filter) ([]cloud.Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Volumes"); err != nil {
		return nil, err
	}
	var out []cloud.Volume
	for _, v := range sortedBySeq(p.volumes, func(v *volume) int { return v.seq }) {
		if matches(f, v.Name, v.Tags) {
			out = append(out, cloneVolume(v.Volume))
		}
	}
	return out, nil
}

// CreateVolume makes a detached volume, empty or from a snapshot (SourceSnapshot says which).
// Its name must be unique in its region, as DigitalOcean requires.
func (p *Provider) CreateVolume(ctx context.Context, spec cloud.VolumeSpec) (cloud.Volume, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "CreateVolume"); err != nil {
		return cloud.Volume{}, err
	}
	if err := validName("volume", spec.Name); err != nil {
		return cloud.Volume{}, err
	}
	var from string
	if ref := spec.FromSnapshot; ref != nil {
		if err := ref.Check(); err != nil {
			return cloud.Volume{}, err
		}
		var snap cloud.Snapshot
		if ref.ID != "" {
			s, ok := p.snapshots[ref.ID]
			if !ok {
				return cloud.Volume{}, fmt.Errorf("fake: volume %q from snapshot %q: %w", spec.Name, ref.ID, cloud.ErrNotFound)
			}
			snap = s.Snapshot
		} else {
			s, err := p.findSnapshotLocked(ref.Name, spec.Region)
			if err != nil {
				return cloud.Volume{}, fmt.Errorf("fake: volume %q: %w", spec.Name, err)
			}
			snap = s
		}
		if err := snap.Fits(spec); err != nil {
			return cloud.Volume{}, err
		}
		from = snap.ID
	}
	if err := p.regionLocked(spec.Region); err != nil {
		return cloud.Volume{}, err
	}
	if spec.SizeGiB < 1 {
		return cloud.Volume{}, fmt.Errorf("fake: volume %q: size %d GiB: %w", spec.Name, spec.SizeGiB, cloud.ErrInvalid)
	}
	if err := validTags(spec.Tags); err != nil {
		return cloud.Volume{}, err
	}
	for _, v := range p.volumes {
		if v.Name == spec.Name && v.Region == spec.Region {
			return cloud.Volume{}, fmt.Errorf("fake: volume %q already exists in %s (%s): %w", spec.Name, spec.Region, v.ID, cloud.ErrInvalid)
		}
	}
	v := &volume{seq: p.nextSeqLocked("v"), fromSnapshot: from}
	v.Volume = cloud.Volume{
		ID: "v-" + strconv.Itoa(v.seq), Name: spec.Name, Region: spec.Region, SizeGiB: spec.SizeGiB,
		Tags: dedupe(spec.Tags),
	}
	p.volumes[v.ID] = v
	return cloneVolume(v.Volume), nil
}

// DeleteVolume destroys a detached volume.
func (p *Provider) DeleteVolume(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "DeleteVolume"); err != nil {
		return err
	}
	v, ok := p.volumes[id]
	if !ok {
		return fmt.Errorf("fake: volume %q: %w", id, cloud.ErrNotFound)
	}
	if len(v.InstanceIDs) > 0 {
		return fmt.Errorf("fake: volume %s is attached to %s: %w", id, v.InstanceIDs[0], cloud.ErrInvalid)
	}
	delete(p.volumes, id)
	return nil
}

// SourceSnapshot is the id of the snapshot a volume was made from: "" for a volume made empty or
// one the fake does not have.
func (p *Provider) SourceSnapshot(volumeID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.volumes[volumeID]; ok {
		return v.fromSnapshot
	}
	return ""
}

// --- volume snapshots ---

// CreateVolumeSnapshot copies a volume, attached or not, into a snapshot in its region. The fake
// holds no data, so a snapshot is as big as its volume and a volume made from it is at least
// that big. Names need not be unique, as on DigitalOcean. HangNext applies to it.
func (p *Provider) CreateVolumeSnapshot(ctx context.Context, volumeID, name string, tags []string) (cloud.Snapshot, error) {
	const method = "CreateVolumeSnapshot"
	p.mu.Lock()
	if err := p.beginLocked(ctx, method); err != nil {
		p.mu.Unlock()
		return cloud.Snapshot{}, err
	}
	if p.hang[method] > 0 {
		p.hang[method]--
		p.mu.Unlock()
		<-ctx.Done()
		return cloud.Snapshot{}, ctx.Err()
	}
	defer p.mu.Unlock()
	v, ok := p.volumes[volumeID]
	if !ok {
		return cloud.Snapshot{}, fmt.Errorf("fake: snapshot of volume %q: %w", volumeID, cloud.ErrNotFound)
	}
	if strings.TrimSpace(name) == "" {
		return cloud.Snapshot{}, fmt.Errorf("fake: snapshot of volume %s without a name: %w", volumeID, cloud.ErrInvalid)
	}
	if err := validTags(tags); err != nil {
		return cloud.Snapshot{}, err
	}
	s := &snapshot{seq: p.nextSeqLocked("snap")}
	s.Snapshot = cloud.Snapshot{
		ID: "snap-" + strconv.Itoa(s.seq), Name: name, Region: v.Region,
		SizeGiB: float64(v.SizeGiB), MinVolumeGiB: v.SizeGiB, CreatedAt: p.clock.Now(), Tags: dedupe(tags),
	}
	p.snapshots[s.ID] = s
	return cloneSnapshot(s.Snapshot), nil
}

// GetVolumeSnapshot reads one snapshot.
func (p *Provider) GetVolumeSnapshot(ctx context.Context, id string) (cloud.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "GetVolumeSnapshot"); err != nil {
		return cloud.Snapshot{}, err
	}
	s, ok := p.snapshots[id]
	if !ok {
		return cloud.Snapshot{}, fmt.Errorf("fake: snapshot %q: %w", id, cloud.ErrNotFound)
	}
	return cloneSnapshot(s.Snapshot), nil
}

// FindVolumeSnapshot is the one snapshot named name in region.
func (p *Provider) FindVolumeSnapshot(ctx context.Context, name, region string) (cloud.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "FindVolumeSnapshot"); err != nil {
		return cloud.Snapshot{}, err
	}
	s, err := p.findSnapshotLocked(name, region)
	if err != nil {
		return cloud.Snapshot{}, fmt.Errorf("fake: %w", err)
	}
	return s, nil
}

func (p *Provider) findSnapshotLocked(name, region string) (cloud.Snapshot, error) {
	var found []string
	var out cloud.Snapshot
	for _, s := range sortedBySeq(p.snapshots, func(s *snapshot) int { return s.seq }) {
		if s.Name == name && s.Region == region {
			found = append(found, s.ID)
			out = cloneSnapshot(s.Snapshot)
		}
	}
	switch len(found) {
	case 0:
		return cloud.Snapshot{}, fmt.Errorf("snapshot %q in %s: %w", name, region, cloud.ErrNotFound)
	case 1:
		return out, nil
	}
	return cloud.Snapshot{}, fmt.Errorf("snapshot %q in %s: %w: %d snapshots have the name (%s); name one by its id",
		name, region, cloud.ErrInvalid, len(found), strings.Join(found, ", "))
}

// ListVolumeSnapshots lists the snapshots matching f, oldest first.
func (p *Provider) ListVolumeSnapshots(ctx context.Context, f cloud.Filter) ([]cloud.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "ListVolumeSnapshots"); err != nil {
		return nil, err
	}
	var out []cloud.Snapshot
	for _, s := range sortedBySeq(p.snapshots, func(s *snapshot) int { return s.seq }) {
		if matches(f, s.Name, s.Tags) {
			out = append(out, cloneSnapshot(s.Snapshot))
		}
	}
	return out, nil
}

// DeleteVolumeSnapshot destroys a snapshot; the volumes made from it keep their data.
func (p *Provider) DeleteVolumeSnapshot(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "DeleteVolumeSnapshot"); err != nil {
		return err
	}
	if _, ok := p.snapshots[id]; !ok {
		return fmt.Errorf("fake: snapshot %q: %w", id, cloud.ErrNotFound)
	}
	delete(p.snapshots, id)
	return nil
}

// AttachVolume attaches a volume to an instance in its region. A volume attached to another
// instance is refused; one already attached to this instance is a done operation.
func (p *Provider) AttachVolume(ctx context.Context, volumeID, instanceID string) (cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const method = "AttachVolume"
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Operation{}, err
	}
	v, ok := p.volumes[volumeID]
	if !ok {
		return cloud.Operation{}, fmt.Errorf("fake: volume %q: %w", volumeID, cloud.ErrNotFound)
	}
	i, err := p.instanceLocked(instanceID)
	if err != nil {
		return cloud.Operation{}, err
	}
	if v.Region != i.Region {
		return cloud.Operation{}, fmt.Errorf("fake: attach volume %s in %s to instance %s in %s: %w", volumeID, v.Region, instanceID, i.Region, cloud.ErrInvalid)
	}
	switch {
	case slices.Equal(v.InstanceIDs, []string{instanceID}):
		return cloud.Operation{Kind: "attach", Resource: volumeID}, nil
	case len(v.InstanceIDs) > 0:
		return cloud.Operation{}, fmt.Errorf("fake: attach volume %s: it is attached to %s: %w", volumeID, v.InstanceIDs[0], cloud.ErrInvalid)
	}
	return p.newOpLocked(method, "attach", volumeID, func() error {
		v, ok := p.volumes[volumeID]
		if !ok {
			return fmt.Errorf("volume %s is gone", volumeID)
		}
		i, ok := p.instances[instanceID]
		if !ok {
			return fmt.Errorf("instance %s is gone", instanceID)
		}
		if len(v.InstanceIDs) > 0 && v.InstanceIDs[0] != instanceID {
			return fmt.Errorf("volume %s was attached to %s meanwhile", volumeID, v.InstanceIDs[0])
		}
		v.InstanceIDs = []string{instanceID}
		i.VolumeIDs = dedupe(append(i.VolumeIDs, volumeID))
		return nil
	}), nil
}

// DetachVolume detaches a volume from the instance it is attached to.
func (p *Provider) DetachVolume(ctx context.Context, volumeID, instanceID string) (cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const method = "DetachVolume"
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Operation{}, err
	}
	v, ok := p.volumes[volumeID]
	if !ok {
		return cloud.Operation{}, fmt.Errorf("fake: volume %q: %w", volumeID, cloud.ErrNotFound)
	}
	if !slices.Contains(v.InstanceIDs, instanceID) {
		return cloud.Operation{}, fmt.Errorf("fake: detach volume %s: it is not attached to %q: %w", volumeID, instanceID, cloud.ErrInvalid)
	}
	return p.newOpLocked(method, "detach", volumeID, func() error {
		if v, ok := p.volumes[volumeID]; ok {
			v.InstanceIDs = slices.DeleteFunc(v.InstanceIDs, func(s string) bool { return s == instanceID })
		}
		if i, ok := p.instances[instanceID]; ok {
			i.VolumeIDs = slices.DeleteFunc(i.VolumeIDs, func(s string) bool { return s == volumeID })
		}
		return nil
	}), nil
}

// VolumeDevice is where the instance sees the volume, shaped like DigitalOcean's by-id path.
func (p *Provider) VolumeDevice(v cloud.Volume) string {
	return "/dev/disk/by-id/scsi-0FAKE_Volume_" + v.Name
}

// ReservedEgressScript is "": the fake's reserved addresses carry outbound traffic by themselves.
func (p *Provider) ReservedEgressScript() string { return "" }

// --- addresses ---

// Addresses lists the reserved addresses matching f, oldest first. An address has no name, so
// Filter.Name matches its IP; Filter.Tag matches the tags it was allocated with.
func (p *Provider) Addresses(ctx context.Context, f cloud.Filter) ([]cloud.Address, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Addresses"); err != nil {
		return nil, err
	}
	var out []cloud.Address
	for _, a := range sortedBySeq(p.addresses, func(a *address) int { return a.seq }) {
		if matches(f, a.IP, a.tags) {
			out = append(out, a.Address)
		}
	}
	return out, nil
}

// AllocateAddress reserves an address from TEST-NET-2 (198.51.100.0/24).
func (p *Provider) AllocateAddress(ctx context.Context, region string, tags []string) (cloud.Address, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "AllocateAddress"); err != nil {
		return cloud.Address{}, err
	}
	if err := p.regionLocked(region); err != nil {
		return cloud.Address{}, err
	}
	if err := validTags(tags); err != nil {
		return cloud.Address{}, err
	}
	p.addressN++
	a := &address{seq: p.addressN, tags: dedupe(tags)}
	a.Address = cloud.Address{IP: "198.51.100." + strconv.Itoa(octet(p.addressN)), Region: region}
	if _, taken := p.addresses[a.IP]; taken {
		return cloud.Address{}, fmt.Errorf("fake: the fake's 254 addresses are all allocated: %w", cloud.ErrInvalid)
	}
	p.addresses[a.IP] = a
	return a.Address, nil
}

// AssignAddress points an address at an instance in its region, moving it from any other.
func (p *Provider) AssignAddress(ctx context.Context, ip, instanceID string) (cloud.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const method = "AssignAddress"
	if err := p.beginLocked(ctx, method); err != nil {
		return cloud.Operation{}, err
	}
	a, ok := p.addresses[ip]
	if !ok {
		return cloud.Operation{}, fmt.Errorf("fake: address %q: %w", ip, cloud.ErrNotFound)
	}
	i, err := p.instanceLocked(instanceID)
	if err != nil {
		return cloud.Operation{}, err
	}
	if a.Region != i.Region {
		return cloud.Operation{}, fmt.Errorf("fake: assign address %s in %s to instance %s in %s: %w", ip, a.Region, instanceID, i.Region, cloud.ErrInvalid)
	}
	return p.newOpLocked(method, "assign", ip, func() error {
		a, ok := p.addresses[ip]
		if !ok {
			return fmt.Errorf("address %s is gone", ip)
		}
		if _, ok := p.instances[instanceID]; !ok {
			return fmt.Errorf("instance %s is gone", instanceID)
		}
		a.InstanceID = instanceID
		return nil
	}), nil
}

// ReleaseAddress gives back an unassigned address; it is gone when the call returns.
func (p *Provider) ReleaseAddress(ctx context.Context, ip string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "ReleaseAddress"); err != nil {
		return err
	}
	a, ok := p.addresses[ip]
	if !ok {
		return fmt.Errorf("fake: address %q: %w", ip, cloud.ErrNotFound)
	}
	if a.InstanceID != "" {
		return fmt.Errorf("fake: release address %s: it is assigned to %s: %w", ip, a.InstanceID, cloud.ErrInvalid)
	}
	delete(p.addresses, ip)
	return nil
}

// --- firewalls ---

// Firewalls lists the firewalls matching f, oldest first. Filter.Tag matches a tag the firewall
// applies to.
func (p *Provider) Firewalls(ctx context.Context, f cloud.Filter) ([]cloud.Firewall, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "Firewalls"); err != nil {
		return nil, err
	}
	var out []cloud.Firewall
	for _, fw := range sortedBySeq(p.firewalls, func(fw *firewall) int { return fw.seq }) {
		if matches(f, fw.Name, fw.Tags) {
			out = append(out, cloneFirewall(fw.Firewall))
		}
	}
	return out, nil
}

// EnsureFirewall creates the firewall spec names, or replaces the rules and tags of the one that
// has its name, keeping its id.
func (p *Provider) EnsureFirewall(ctx context.Context, spec cloud.FirewallSpec) (cloud.Firewall, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "EnsureFirewall"); err != nil {
		return cloud.Firewall{}, err
	}
	if err := validName("firewall", spec.Name); err != nil {
		return cloud.Firewall{}, err
	}
	if err := validTags(spec.Tags); err != nil {
		return cloud.Firewall{}, err
	}
	for _, rules := range [][]cloud.Rule{spec.Inbound, spec.Outbound} {
		for _, r := range rules {
			if err := validRule(spec.Name, r); err != nil {
				return cloud.Firewall{}, err
			}
		}
	}
	spec = cloneFirewall(cloud.Firewall{FirewallSpec: spec}).FirewallSpec
	spec.Tags = dedupe(spec.Tags)
	for _, fw := range p.firewalls {
		if fw.Name == spec.Name {
			fw.FirewallSpec = spec
			return cloneFirewall(fw.Firewall), nil
		}
	}
	fw := &firewall{seq: p.nextSeqLocked("fw")}
	fw.Firewall = cloud.Firewall{ID: "fw-" + strconv.Itoa(fw.seq), FirewallSpec: spec}
	p.firewalls[fw.ID] = fw
	return cloneFirewall(fw.Firewall), nil
}

// DeleteFirewall removes a firewall.
func (p *Provider) DeleteFirewall(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "DeleteFirewall"); err != nil {
		return err
	}
	if _, ok := p.firewalls[id]; !ok {
		return fmt.Errorf("fake: firewall %q: %w", id, cloud.ErrNotFound)
	}
	delete(p.firewalls, id)
	return nil
}

// --- SSH keys ---

// EnsureSSHKey registers a public key once: the same key (its type and base64, whatever its
// comment) always comes back with the same id, as a cloud keyed by fingerprint does.
func (p *Provider) EnsureSSHKey(ctx context.Context, name, publicKey string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "EnsureSSHKey"); err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("fake: SSH key without a name: %w", cloud.ErrInvalid)
	}
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return "", fmt.Errorf("fake: SSH key %q is not an OpenSSH public key: %w", name, cloud.ErrInvalid)
	}
	key := fields[0] + " " + fields[1]
	for _, k := range p.keys {
		if k.publicKey == key {
			return k.id, nil
		}
	}
	id := "key-" + strconv.Itoa(p.nextSeqLocked("key"))
	p.keys = append(p.keys, sshKey{id: id, name: name, publicKey: key})
	return id, nil
}

// --- operations ---

// Wait blocks until the operation completes on the fake's clock, or ctx ends.
func (p *Provider) Wait(ctx context.Context, op cloud.Operation) error {
	p.mu.Lock()
	if err := p.beginLocked(ctx, "Wait"); err != nil {
		p.mu.Unlock()
		return err
	}
	if op.Done() {
		p.mu.Unlock()
		return nil
	}
	o, ok := p.ops[op.ID]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("fake: operation %q: %w", op.ID, cloud.ErrNotFound)
	}
	for {
		p.mu.Lock()
		p.settleLocked()
		done, err, hang, doneAt := o.done, o.err, o.hang, o.doneAt
		p.mu.Unlock()
		if done {
			if err != nil {
				return fmt.Errorf("fake: operation %s (%s %s): %w", o.ID, o.Kind, o.Resource, err)
			}
			return nil
		}
		if hang {
			<-ctx.Done()
			return ctx.Err()
		}
		// The clock may move between reading it and arming the timer; a ManualClock arms the
		// timer from its own now, so if now changed in between the timer could be set too late.
		// Re-read and start over in that case.
		t0 := p.clock.Now()
		timer := p.clock.After(doneAt.Sub(t0))
		if !p.clock.Now().Equal(t0) {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer:
		}
	}
}

// newOpLocked starts an operation of method that makes apply's change when it completes.
func (p *Provider) newOpLocked(method, kind, resource string, apply func() error) cloud.Operation {
	d, ok := p.durations[method]
	if !ok {
		d = p.opDuration
	}
	o := &operation{seq: p.nextSeqLocked("op"), doneAt: p.clock.Now().Add(d), apply: apply}
	o.Operation = cloud.Operation{ID: "op-" + strconv.Itoa(o.seq), Kind: kind, Resource: resource}
	if p.hang[method] > 0 {
		p.hang[method]--
		o.hang = true
	}
	if p.failOps[method] > 0 {
		p.failOps[method]--
		o.failed = true
	}
	p.ops[o.ID] = o
	return o.Operation
}

// settleLocked completes every operation that is due, in the order they fall due. Operations
// complete lazily, when somebody looks, so the fake needs no goroutine of its own.
func (p *Provider) settleLocked() {
	now := p.clock.Now()
	var due []*operation
	for _, o := range p.ops {
		if !o.done && !o.hang && !o.doneAt.After(now) {
			due = append(due, o)
		}
	}
	slices.SortFunc(due, func(a, b *operation) int {
		return cmp.Or(a.doneAt.Compare(b.doneAt), cmp.Compare(a.seq, b.seq))
	})
	for _, o := range due {
		o.done = true
		switch {
		case o.failed:
			o.err = fmt.Errorf("%w: injected failure", cloud.ErrOperationFailed)
		case o.apply != nil:
			if err := o.apply(); err != nil {
				o.err = fmt.Errorf("%w: %w", cloud.ErrOperationFailed, err)
			}
		}
		o.apply = nil
	}
}

// beginLocked records the call, honours a done context and an injected failure, and completes
// the operations that are due so the call sees the cloud as it is now.
func (p *Provider) beginLocked(ctx context.Context, method string) error {
	p.calls = append(p.calls, method)
	if err := ctx.Err(); err != nil {
		return err
	}
	if q := p.fail[method]; len(q) > 0 {
		p.fail[method] = q[1:]
		return q[0]
	}
	p.settleLocked()
	return nil
}

// --- helpers ---

func (p *Provider) nextSeqLocked(kind string) int {
	p.seq[kind]++
	return p.seq[kind]
}

func (p *Provider) instanceLocked(id string) (*instance, error) {
	i, ok := p.instances[id]
	if !ok {
		return nil, fmt.Errorf("fake: instance %q: %w", id, cloud.ErrNotFound)
	}
	return i, nil
}

func (p *Provider) regionLocked(slug string) error {
	for _, r := range p.regions {
		if r.Slug == slug {
			if !r.Available {
				return fmt.Errorf("fake: region %q is not available: %w", slug, cloud.ErrInvalid)
			}
			return nil
		}
	}
	return fmt.Errorf("fake: no region %q: %w", slug, cloud.ErrInvalid)
}

func (p *Provider) sizeLocked(slug, region string) (cloud.Size, error) {
	for _, s := range p.sizes {
		if s.Slug == slug {
			if !slices.Contains(s.Regions, region) {
				return cloud.Size{}, fmt.Errorf("fake: size %q is not offered in %s: %w", slug, region, cloud.ErrInvalid)
			}
			return cloneSize(s), nil
		}
	}
	return cloud.Size{}, fmt.Errorf("fake: no size %q: %w", slug, cloud.ErrInvalid)
}

// nameRE is the shape of a DigitalOcean volume name, the strictest of the names the fake
// stands in for; holding instances and firewalls to it too keeps a caller portable.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// tagRE is DigitalOcean's tag shape.
var tagRE = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,255}$`)

func validName(kind, name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("fake: %s name %q must match %s: %w", kind, name, nameRE, cloud.ErrInvalid)
	}
	return nil
}

func validTags(tags []string) error {
	for _, t := range tags {
		if !tagRE.MatchString(t) {
			return fmt.Errorf("fake: tag %q must match %s: %w", t, tagRE, cloud.ErrInvalid)
		}
	}
	return nil
}

func validRule(firewall string, r cloud.Rule) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("fake: firewall %q: rule %+v: %s: %w", firewall, r, fmt.Sprintf(format, args...), cloud.ErrInvalid)
	}
	switch r.Protocol {
	case "tcp", "udp":
		if r.Ports != "" {
			lo, hi, isRange := strings.Cut(r.Ports, "-")
			if !isRange {
				hi = lo
			}
			a, errA := strconv.Atoi(lo)
			b, errB := strconv.Atoi(hi)
			if errA != nil || errB != nil || a < 1 || b > 65535 || a > b {
				return bad("ports %q is not a port or a range", r.Ports)
			}
		}
	case "icmp":
		if r.Ports != "" {
			return bad("icmp has no ports")
		}
	default:
		return bad("protocol %q is not tcp, udp or icmp", r.Protocol)
	}
	if len(r.Addresses) == 0 {
		return bad("no addresses")
	}
	for _, a := range r.Addresses {
		if _, err := netip.ParsePrefix(a); err != nil {
			return bad("address %q is not a CIDR", a)
		}
	}
	return nil
}

// matches is Filter's rule: every set field must match.
func matches(f cloud.Filter, name string, tags []string) bool {
	return (f.Name == "" || f.Name == name) && (f.Tag == "" || slices.Contains(tags, f.Tag))
}

func sortedBySeq[T any](m map[string]T, seq func(T) int) []T {
	out := make([]T, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b T) int { return cmp.Compare(seq(a), seq(b)) })
	return out
}

// octet maps 1, 2, … onto 1–254, so a long test wraps instead of making an invalid address.
func octet(n int) int { return (n-1)%254 + 1 }

// dedupe is tags or ids without repeats, in first-seen order.
func dedupe(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func cloneInstance(i cloud.Instance) cloud.Instance {
	i.Tags = slices.Clone(i.Tags)
	i.VolumeIDs = slices.Clone(i.VolumeIDs)
	return i
}

func cloneVolume(v cloud.Volume) cloud.Volume {
	v.InstanceIDs = slices.Clone(v.InstanceIDs)
	v.Tags = slices.Clone(v.Tags)
	return v
}

func cloneSnapshot(s cloud.Snapshot) cloud.Snapshot {
	s.Tags = slices.Clone(s.Tags)
	return s
}

func cloneSize(s cloud.Size) cloud.Size {
	s.Regions = slices.Clone(s.Regions)
	return s
}

func cloneFirewall(fw cloud.Firewall) cloud.Firewall {
	fw.Tags = slices.Clone(fw.Tags)
	fw.Inbound = cloneRules(fw.Inbound)
	fw.Outbound = cloneRules(fw.Outbound)
	return fw
}

func cloneRules(rules []cloud.Rule) []cloud.Rule {
	if rules == nil {
		return nil
	}
	out := make([]cloud.Rule, len(rules))
	for i, r := range rules {
		r.Addresses = slices.Clone(r.Addresses)
		out[i] = r
	}
	return out
}
