// Package cloud is the seam between gravel and the clouds it makes machines on (ADR-0003,
// ADR-0020). It is written in domain terms: instances, volumes, addresses, firewalls and the
// operations that change them. Each cloud is a package under cloud/ that implements Provider over
// that cloud's own SDK (cloud/digitalocean, godo); cloud/fake is the test double, and
// cloud/cloudtest is the conformance suite every implementation passes. Nothing outside cloud/
// imports a vendor SDK (a depguard rule enforces it).
//
// Two callers use it: the gravel CLI, which makes the hub's own VM (ADR-0020), and the cloud-VM
// driver inside the hub, which makes the VMs its servers run on (ADR-0003). Neither keeps local
// state about what it made: everything gravel creates carries its tags and a predictable name, so
// a re-run, or a run after a crash halfway through, finds what exists and makes only what is
// missing.
package cloud

import (
	"context"
	"errors"
	"time"
)

// Errors a provider returns, wrapping the SDK's own.
var (
	// ErrNotFound is a resource the cloud does not have (any more).
	ErrNotFound = errors.New("cloud: not found")
	// ErrRateLimited is the cloud's API asking the caller to wait.
	ErrRateLimited = errors.New("cloud: rate limited by the provider")
	// ErrResizeNotAllowed is a resize the provider's rules forbid (CanResize says why).
	ErrResizeNotAllowed = errors.New("cloud: the provider does not allow this resize")
	// ErrOperationFailed is an operation the cloud reports as failed (Wait wraps it with its id).
	ErrOperationFailed = errors.New("cloud: the operation failed")
	// ErrInvalid is a request the provider refused as malformed: a name the cloud does not accept,
	// a size not offered in the region.
	ErrInvalid = errors.New("cloud: invalid request")
)

// Provider is one cloud. Every mutating call that the cloud carries out asynchronously returns an
// Operation; the caller waits for it with Wait and a context deadline. Calls the cloud completes
// before it answers (a firewall, a volume, an address) return the resource directly.
type Provider interface {
	// Name is the provider's flag name ("digitalocean", "fake").
	Name() string

	// Regions are the regions the account can create in.
	Regions(ctx context.Context) ([]Region, error)
	// Sizes are the instance sizes the account can create, with the regions offering each.
	Sizes(ctx context.Context) ([]Size, error)
	// CanResize says whether an instance of size from may be resized to size to, CPU and memory
	// only (a resize never touches disk, ADR-0003 §3). nil means yes; an error wrapping
	// ErrResizeNotAllowed says why not. It is data, so a ladder is validated before any boundary.
	// An instance keeps the root disk it was created with, so for a resize of a running ladder
	// pass from with DiskGiB set to the instance's (Instance.DiskGiB), not its current size's.
	CanResize(from, to Size) error

	// Instances lists the instances matching the filter.
	Instances(ctx context.Context, f Filter) ([]Instance, error)
	// Instance reads one instance by id (ErrNotFound when it is gone).
	Instance(ctx context.Context, id string) (Instance, error)
	// CreateInstance creates an instance. The Instance it returns has the id; its addresses
	// arrive once the operation is done (read it again).
	CreateInstance(ctx context.Context, spec InstanceSpec) (Instance, Operation, error)
	// DeleteInstance destroys an instance. Its volumes are detached, not deleted.
	DeleteInstance(ctx context.Context, id string) (Operation, error)
	// StartInstance powers an instance on.
	StartInstance(ctx context.Context, id string) (Operation, error)
	// StopInstance powers an instance off: gracefully (the OS shuts down) or not.
	StopInstance(ctx context.Context, id string, graceful bool) (Operation, error)
	// ResizeInstance changes a stopped instance's size, CPU and memory only, never disk.
	ResizeInstance(ctx context.Context, id, size string) (Operation, error)
	// Adopt brings an instance made outside gravel under gravel's control by adding the tags
	// gravel finds it by. The instance is not rebuilt.
	Adopt(ctx context.Context, id string, tags []string) (Instance, error)

	// Volumes lists the volumes matching the filter.
	Volumes(ctx context.Context, f Filter) ([]Volume, error)
	// CreateVolume creates an unformatted volume (the instance formats it once, on first boot).
	CreateVolume(ctx context.Context, spec VolumeSpec) (Volume, error)
	// DeleteVolume destroys a detached volume and its data.
	DeleteVolume(ctx context.Context, id string) error
	// AttachVolume attaches a volume to an instance in the same region.
	AttachVolume(ctx context.Context, volumeID, instanceID string) (Operation, error)
	// DetachVolume detaches a volume from an instance.
	DetachVolume(ctx context.Context, volumeID, instanceID string) (Operation, error)
	// VolumeDevice is the stable path the instance's OS sees the volume at, for cloud-init.
	VolumeDevice(v Volume) string

	// Addresses lists the reserved (movable, inbound) addresses matching the filter.
	Addresses(ctx context.Context, f Filter) ([]Address, error)
	// AllocateAddress reserves an address in a region. Tags are kept where the cloud tags
	// addresses; on one that does not (DigitalOcean), a tag filter matches an address through the
	// instance it is assigned to, so a caller keeps an unassigned address's IP.
	AllocateAddress(ctx context.Context, region string, tags []string) (Address, error)
	// AssignAddress points an address at an instance.
	AssignAddress(ctx context.Context, ip, instanceID string) (Operation, error)
	// ReleaseAddress gives an unassigned address back.
	ReleaseAddress(ctx context.Context, ip string) error

	// Firewalls lists the firewalls matching the filter.
	Firewalls(ctx context.Context, f Filter) ([]Firewall, error)
	// EnsureFirewall creates the firewall named in the spec, or replaces an existing one's rules
	// and targets with the spec's. Calling it twice with the same spec changes nothing.
	EnsureFirewall(ctx context.Context, spec FirewallSpec) (Firewall, error)
	// DeleteFirewall removes a firewall.
	DeleteFirewall(ctx context.Context, id string) error

	// EnsureSSHKey registers a public key with the cloud if it does not have it yet, and returns
	// the id an InstanceSpec names it by.
	EnsureSSHKey(ctx context.Context, name, publicKey string) (string, error)

	// Wait blocks until the operation is done. It returns nil when the operation succeeded, an
	// error wrapping ErrOperationFailed when the cloud reports it failed, and the context's error
	// when the deadline passes first: the operation may still finish later.
	Wait(ctx context.Context, op Operation) error
}

// Filter selects resources. Every set field must match; an empty Filter matches everything.
type Filter struct {
	// Tag is one tag the resource must carry (for an address: see AllocateAddress).
	Tag string
	// Name is the resource's exact name; for an address, its IP.
	Name string
}

// Region is where resources are made.
type Region struct {
	Slug      string // the provider's id: "sfo3"
	Name      string // "San Francisco 3"
	Available bool   // the account may create in it now
}

// Size is an instance size.
type Size struct {
	Slug         string
	VCPUs        int
	MemoryMiB    int
	DiskGiB      int // the root disk that comes with the size
	PriceHourly  float64
	PriceMonthly float64
	// Family groups sizes the provider resizes between ("basic", "cpu-optimized", …).
	Family string
	// DedicatedCPU is a size whose vCPUs are not shared with other tenants.
	DedicatedCPU bool
	// BillsWhenStopped is a size that costs the same powered off (DigitalOcean): parking such an
	// instance means destroying it and keeping its volume and address (ADR-0003 §5).
	BillsWhenStopped bool
	// Regions offering this size now.
	Regions []string
}

// Status is an instance's power state, in domain terms.
type Status string

// Instance statuses.
const (
	StatusPending Status = "pending" // being created
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
	StatusUnknown Status = "unknown" // anything the provider reports that is none of the above
)

// Instance is a virtual machine.
type Instance struct {
	ID     string
	Name   string
	Region string
	Size   string
	// DiskGiB is the root disk the instance was created with. A resize never changes it, so a
	// resize target's disk is compared with it, not with the current size's (CanResize).
	DiskGiB     int
	Image       string
	Status      Status
	PublicIPv4  string // empty until the cloud assigns one
	PrivateIPv4 string
	Tags        []string
	VolumeIDs   []string
	Created     time.Time
}

// InstanceSpec is what CreateInstance makes.
type InstanceSpec struct {
	Name   string
	Region string
	Size   string
	Image  string // the provider's image slug or id ("fedora-44-x64")
	// UserData is the cloud-init document the instance boots with. Anything on the instance can
	// read it from the metadata service, so it never holds a secret (ADR-0020).
	UserData  string
	SSHKeyIDs []string // ids from EnsureSSHKey
	Tags      []string
	VolumeIDs []string // attached at creation
	// Network is the provider's private network id; empty is the region's default.
	Network string
	// Monitoring turns on the provider's own metrics agent where it has one.
	Monitoring bool
}

// Volume is block storage that attaches to one instance at a time and moves between instances in
// the same region.
type Volume struct {
	ID          string
	Name        string
	Region      string
	SizeGiB     int
	InstanceIDs []string
	Tags        []string
}

// VolumeSpec is what CreateVolume makes.
type VolumeSpec struct {
	Name    string
	Region  string
	SizeGiB int
	Tags    []string
}

// Address is a reserved IPv4 address that moves between instances. It carries inbound traffic
// only: an instance's outbound traffic leaves from its own address.
type Address struct {
	IP         string
	Region     string
	InstanceID string // empty when unassigned
}

// Rule is one firewall rule. Ports is a port, a range "7000-8000", or empty for every port.
// Addresses are CIDRs ("203.0.113.4/32", "0.0.0.0/0", "::/0").
type Rule struct {
	Protocol  string // "tcp", "udp", "icmp"
	Ports     string
	Addresses []string
}

// FirewallSpec is a firewall applied to every instance carrying one of Tags.
type FirewallSpec struct {
	Name     string
	Tags     []string
	Inbound  []Rule
	Outbound []Rule
}

// Firewall is a firewall the provider holds.
type Firewall struct {
	ID string
	FirewallSpec
}

// Operation is an asynchronous change the provider is carrying out. Its fields are the
// provider's; callers only pass it to Wait.
type Operation struct {
	ID       string // the provider's action id; empty for an operation that was already done
	Kind     string // "create", "delete", "start", "stop", "resize", "attach", …
	Resource string // the id of the resource it acts on
}

// Done is an operation that needs no wait.
func (o Operation) Done() bool { return o.ID == "" }

// AnyAddress is every IPv4 and IPv6 address, for outbound rules.
var AnyAddress = []string{"0.0.0.0/0", "::/0"}

// AllowAllOutbound is the outbound rule set that lets an instance reach anything. A cloud
// firewall with no outbound rules blocks all outbound traffic on some providers (DigitalOcean), so
// a FirewallSpec that means "inbound only" says this explicitly.
func AllowAllOutbound() []Rule {
	return []Rule{
		{Protocol: "tcp", Addresses: AnyAddress},
		{Protocol: "udp", Addresses: AnyAddress},
		{Protocol: "icmp", Addresses: AnyAddress},
	}
}
