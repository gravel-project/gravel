package hubvm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/gravel-project/gravel/cloud"
)

// BaseTag is on everything gravel makes for any hub; Tag(name) is on everything for one hub. The
// CLI keeps no state of its own: these tags and the names below are how it finds a hub again,
// after a crash halfway through a create as much as on a later run.
const BaseTag = "gravel-hub"

// Tag is the tag on one hub's instance, volume and firewall.
func Tag(name string) string { return BaseTag + ":" + name }

// VolumeName is the hub's data volume.
func VolumeName(name string) string { return name + "-data" }

// FirewallName is the hub's firewall.
func FirewallName(name string) string { return BaseTag + "-" + name }

// A hub's name becomes a hostname, an instance name and a volume name, so it takes the strictest
// of the three: a letter first (DigitalOcean's volume names), then letters, digits and hyphens.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}[a-z0-9]$`)

// ValidName reports whether name can name a hub.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("hubvm: hub name %q: a lower-case letter, then letters, digits and hyphens, 2–41 characters, not ending in a hyphen", name)
	}
	return nil
}

// Spec is one hub's host.
type Spec struct {
	Name      string
	Region    string
	Size      string
	Image     string
	VolumeGiB int
	// SSHKeyName and SSHPublicKey register the operator's key with the cloud (the instance also
	// gets every key in Document.SSHKeys through cloud-init).
	SSHKeyName   string
	SSHPublicKey string
	// SSHFrom are the CIDRs the firewall admits SSH from. Empty keeps an existing firewall's
	// sources; a new firewall needs at least one.
	SSHFrom    []string
	Monitoring bool
	// Document is the cloud-init document's parameters; Create fills in the hostname and the
	// data device.
	Document Params
}

// Action is what Create did, or would do, to one resource.
type Action string

// Actions.
const (
	Unchanged Action = "unchanged"
	Created   Action = "created"
	Updated   Action = "updated"
)

// Step is one resource and what happened to it.
type Step struct {
	Kind     string // "firewall", "volume", "instance"
	Name     string
	Action   Action
	Detail   string
	Resource string // the provider's id, when there is one
}

// Result is a hub's resources after Create, Status or AllowSSH.
type Result struct {
	Steps    []Step
	Instance *cloud.Instance
	Volume   *cloud.Volume
	Firewall *cloud.Firewall
	// Recreated is a new instance on a volume that existed before it: the hub's data came back.
	Recreated bool
}

// Changed reports whether any step created or updated something.
func (r Result) Changed() bool {
	return slices.ContainsFunc(r.Steps, func(s Step) bool { return s.Action != Unchanged })
}

// Create makes whatever of the hub's firewall, data volume and instance does not exist yet, in
// that order, and returns all three. A second run changes nothing. A run after the instance was
// destroyed with its volume kept makes a new instance on that volume, which is how a hub host is
// replaced. With dryRun nothing is changed and the steps say what would be.
func Create(ctx context.Context, p cloud.Provider, s Spec, dryRun bool) (Result, error) {
	if err := ValidName(s.Name); err != nil {
		return Result{}, err
	}
	if s.Region == "" || s.Size == "" || s.Image == "" {
		return Result{}, fmt.Errorf("hubvm: region, size and image are required")
	}
	if s.VolumeGiB < 1 {
		return Result{}, fmt.Errorf("hubvm: the data volume needs a size")
	}
	sshFrom, err := cloud.NormalizeCIDRs(s.SSHFrom)
	if err != nil {
		return Result{}, err
	}
	var r Result

	// The firewall first: an instance must never be up before the rules that guard it are.
	fw, step, err := ensureFirewall(ctx, p, s.Name, sshFrom, dryRun)
	if err != nil {
		return r, err
	}
	r.Firewall, r.Steps = fw, append(r.Steps, step)

	vol, step, existed, err := ensureVolume(ctx, p, s, dryRun)
	if err != nil {
		return r, err
	}
	r.Volume, r.Steps = vol, append(r.Steps, step)

	insts, err := p.Instances(ctx, cloud.Filter{Tag: Tag(s.Name)})
	if err != nil {
		return r, fmt.Errorf("hubvm: list instances: %w", err)
	}
	switch len(insts) {
	case 0:
	case 1:
		inst := insts[0]
		if vol.ID != "" && !slices.Contains(inst.VolumeIDs, vol.ID) {
			return r, fmt.Errorf("hubvm: instance %s (%s) carries %s but not its data volume %s; destroy it (gravel hub destroy) and run create again", inst.Name, inst.ID, Tag(s.Name), vol.Name)
		}
		r.Instance = &inst
		r.Steps = append(r.Steps, Step{Kind: "instance", Name: inst.Name, Action: Unchanged, Resource: inst.ID,
			Detail: fmt.Sprintf("%s, %s in %s, %s", inst.Status, inst.Size, inst.Region, orNone(inst.PublicIPv4))})
		return r, nil
	default:
		ids := make([]string, len(insts))
		for i, in := range insts {
			ids[i] = in.Name + " (" + in.ID + ")"
		}
		return r, fmt.Errorf("hubvm: %d instances carry %s: %s; delete all but one", len(insts), Tag(s.Name), strings.Join(ids, ", "))
	}

	doc := s.Document
	doc.Hostname = s.Name
	doc.DataDevice = p.VolumeDevice(*vol)
	userData, err := Render(doc)
	if err != nil {
		return r, err
	}
	detail := fmt.Sprintf("%s in %s, %s, profile %s", s.Size, s.Region, s.Image, doc.Profile)
	if existed {
		detail += ", on the existing data volume"
	}
	if dryRun {
		r.Steps = append(r.Steps, Step{Kind: "instance", Name: s.Name, Action: Created, Detail: detail})
		return r, nil
	}
	keyID, err := p.EnsureSSHKey(ctx, s.SSHKeyName, s.SSHPublicKey)
	if err != nil {
		return r, fmt.Errorf("hubvm: register the SSH key: %w", err)
	}
	inst, op, err := p.CreateInstance(ctx, cloud.InstanceSpec{
		Name:       s.Name,
		Region:     s.Region,
		Size:       s.Size,
		Image:      s.Image,
		UserData:   userData,
		SSHKeyIDs:  []string{keyID},
		Tags:       []string{BaseTag, Tag(s.Name)},
		VolumeIDs:  []string{vol.ID},
		Monitoring: s.Monitoring,
	})
	if err != nil {
		return r, fmt.Errorf("hubvm: create the instance: %w", err)
	}
	if err := p.Wait(ctx, op); err != nil {
		return r, fmt.Errorf("hubvm: instance %s (%s) was created but is not running yet: %w; run create again to pick it up", inst.Name, inst.ID, err)
	}
	if inst, err = p.Instance(ctx, inst.ID); err != nil {
		return r, fmt.Errorf("hubvm: read the new instance: %w", err)
	}
	r.Instance, r.Recreated = &inst, existed
	r.Steps = append(r.Steps, Step{Kind: "instance", Name: inst.Name, Action: Created, Resource: inst.ID, Detail: detail + ", " + orNone(inst.PublicIPv4)})
	return r, nil
}

func ensureFirewall(ctx context.Context, p cloud.Provider, name string, sshFrom []string, dryRun bool) (*cloud.Firewall, Step, error) {
	fwName := FirewallName(name)
	fws, err := p.Firewalls(ctx, cloud.Filter{Name: fwName})
	if err != nil {
		return nil, Step{}, fmt.Errorf("hubvm: list firewalls: %w", err)
	}
	if len(fws) > 1 {
		return nil, Step{}, fmt.Errorf("hubvm: %d firewalls are named %s; delete all but one", len(fws), fwName)
	}
	var cur *cloud.Firewall
	if len(fws) == 1 {
		cur = &fws[0]
	}
	if len(sshFrom) == 0 {
		if cur == nil {
			return nil, Step{}, fmt.Errorf("hubvm: a new hub needs the addresses SSH is admitted from (--ssh-from or --ssh-from-here)")
		}
		sshFrom = SSHSources(*cur)
	}
	want := firewallSpec(name, sshFrom)
	step := Step{Kind: "firewall", Name: fwName, Detail: "SSH from " + strings.Join(sshFrom, ", ")}
	switch {
	case cur == nil:
		step.Action = Created
	case cloud.SameFirewallSpec(cur.FirewallSpec, want):
		step.Action, step.Resource = Unchanged, cur.ID
		return cur, step, nil
	default:
		step.Action, step.Resource = Updated, cur.ID
		step.Detail = "SSH from " + strings.Join(SSHSources(*cur), ", ") + " → " + strings.Join(sshFrom, ", ")
	}
	if dryRun {
		fw := cloud.Firewall{FirewallSpec: want}
		if cur != nil {
			fw.ID = cur.ID
		}
		return &fw, step, nil
	}
	fw, err := p.EnsureFirewall(ctx, want)
	if err != nil {
		return nil, Step{}, fmt.Errorf("hubvm: firewall %s: %w", fwName, err)
	}
	step.Resource = fw.ID
	return &fw, step, nil
}

// firewallSpec admits SSH from the given sources and nothing else; everything outbound is open
// (the hub reaches the world through a tunnel it dials, so nothing else comes in).
func firewallSpec(name string, sshFrom []string) cloud.FirewallSpec {
	return cloud.FirewallSpec{
		Name:     FirewallName(name),
		Tags:     []string{Tag(name)},
		Inbound:  []cloud.Rule{{Protocol: "tcp", Ports: "22", Addresses: sshFrom}},
		Outbound: cloud.AllowAllOutbound(),
	}
}

// SSHSources are the addresses a hub firewall admits SSH from.
func SSHSources(fw cloud.Firewall) []string {
	var out []string
	for _, r := range fw.Inbound {
		if r.Protocol == "tcp" && r.Ports == "22" {
			out = append(out, r.Addresses...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func ensureVolume(ctx context.Context, p cloud.Provider, s Spec, dryRun bool) (*cloud.Volume, Step, bool, error) {
	name := VolumeName(s.Name)
	vols, err := p.Volumes(ctx, cloud.Filter{Name: name})
	if err != nil {
		return nil, Step{}, false, fmt.Errorf("hubvm: list volumes: %w", err)
	}
	vols = slices.DeleteFunc(vols, func(v cloud.Volume) bool { return v.Region != s.Region })
	switch len(vols) {
	case 0:
	case 1:
		v := vols[0]
		if !slices.Contains(v.Tags, Tag(s.Name)) {
			return nil, Step{}, false, fmt.Errorf("hubvm: volume %s exists in %s but is not tagged %s; it is not this hub's", name, s.Region, Tag(s.Name))
		}
		detail := fmt.Sprintf("%d GiB in %s", v.SizeGiB, v.Region)
		if v.SizeGiB != s.VolumeGiB {
			detail += fmt.Sprintf(" (asked for %d GiB; a volume is never resized by create)", s.VolumeGiB)
		}
		if len(v.InstanceIDs) > 0 {
			detail += ", attached to " + strings.Join(v.InstanceIDs, ", ")
		}
		return &v, Step{Kind: "volume", Name: name, Action: Unchanged, Resource: v.ID, Detail: detail}, true, nil
	default:
		return nil, Step{}, false, fmt.Errorf("hubvm: %d volumes are named %s in %s", len(vols), name, s.Region)
	}
	step := Step{Kind: "volume", Name: name, Action: Created, Detail: fmt.Sprintf("%d GiB in %s", s.VolumeGiB, s.Region)}
	if dryRun {
		return &cloud.Volume{Name: name, Region: s.Region, SizeGiB: s.VolumeGiB}, step, false, nil
	}
	v, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: name, Region: s.Region, SizeGiB: s.VolumeGiB, Tags: []string{BaseTag, Tag(s.Name)}})
	if err != nil {
		return nil, Step{}, false, fmt.Errorf("hubvm: create volume %s: %w", name, err)
	}
	step.Resource = v.ID
	return &v, step, false, nil
}

// Status reads what exists for a hub without changing anything.
func Status(ctx context.Context, p cloud.Provider, name string) (Result, error) {
	if err := ValidName(name); err != nil {
		return Result{}, err
	}
	var r Result
	fws, err := p.Firewalls(ctx, cloud.Filter{Name: FirewallName(name)})
	if err != nil {
		return r, fmt.Errorf("hubvm: list firewalls: %w", err)
	}
	if len(fws) > 0 {
		r.Firewall = &fws[0]
	}
	vols, err := p.Volumes(ctx, cloud.Filter{Name: VolumeName(name), Tag: Tag(name)})
	if err != nil {
		return r, fmt.Errorf("hubvm: list volumes: %w", err)
	}
	if len(vols) > 0 {
		r.Volume = &vols[0]
	}
	insts, err := p.Instances(ctx, cloud.Filter{Tag: Tag(name)})
	if err != nil {
		return r, fmt.Errorf("hubvm: list instances: %w", err)
	}
	if len(insts) > 1 {
		return r, fmt.Errorf("hubvm: %d instances carry %s; delete all but one", len(insts), Tag(name))
	}
	if len(insts) == 1 {
		r.Instance = &insts[0]
	}
	return r, nil
}

// DestroyOptions say what besides the instance goes.
type DestroyOptions struct {
	DeleteVolume   bool // the hub's data: gone for good
	DeleteFirewall bool
	DryRun         bool
}

// Destroy deletes the hub's instance and, when asked, its volume and firewall. Without them, a
// later Create makes a new instance on the same data.
func Destroy(ctx context.Context, p cloud.Provider, name string, o DestroyOptions) ([]Step, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	var steps []Step
	insts, err := p.Instances(ctx, cloud.Filter{Tag: Tag(name)})
	if err != nil {
		return nil, fmt.Errorf("hubvm: list instances: %w", err)
	}
	for _, in := range insts {
		steps = append(steps, Step{Kind: "instance", Name: in.Name, Action: "deleted", Resource: in.ID})
		if o.DryRun {
			continue
		}
		op, err := p.DeleteInstance(ctx, in.ID)
		if err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return steps, fmt.Errorf("hubvm: delete instance %s: %w", in.ID, err)
		}
		if err == nil {
			if err := p.Wait(ctx, op); err != nil {
				return steps, fmt.Errorf("hubvm: wait for instance %s to go: %w", in.ID, err)
			}
		}
	}
	if o.DeleteVolume {
		vols, err := p.Volumes(ctx, cloud.Filter{Name: VolumeName(name), Tag: Tag(name)})
		if err != nil {
			return steps, fmt.Errorf("hubvm: list volumes: %w", err)
		}
		for _, v := range vols {
			steps = append(steps, Step{Kind: "volume", Name: v.Name, Action: "deleted", Resource: v.ID})
			if o.DryRun {
				continue
			}
			if err := deleteVolume(ctx, p, v); err != nil {
				return steps, err
			}
		}
	}
	if o.DeleteFirewall {
		fws, err := p.Firewalls(ctx, cloud.Filter{Name: FirewallName(name)})
		if err != nil {
			return steps, fmt.Errorf("hubvm: list firewalls: %w", err)
		}
		for _, fw := range fws {
			steps = append(steps, Step{Kind: "firewall", Name: fw.Name, Action: "deleted", Resource: fw.ID})
			if o.DryRun {
				continue
			}
			if err := p.DeleteFirewall(ctx, fw.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return steps, fmt.Errorf("hubvm: delete firewall %s: %w", fw.ID, err)
			}
		}
	}
	return steps, nil
}

// deleteVolume deletes a volume, detaching it first if a cloud still reports it attached to the
// instance that was just deleted.
func deleteVolume(ctx context.Context, p cloud.Provider, v cloud.Volume) error {
	for _, iid := range v.InstanceIDs {
		op, err := p.DetachVolume(ctx, v.ID, iid)
		if err != nil {
			if errors.Is(err, cloud.ErrNotFound) {
				continue
			}
			return fmt.Errorf("hubvm: detach volume %s: %w", v.ID, err)
		}
		if err := p.Wait(ctx, op); err != nil {
			return fmt.Errorf("hubvm: detach volume %s: %w", v.ID, err)
		}
	}
	if err := p.DeleteVolume(ctx, v.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
		return fmt.Errorf("hubvm: delete volume %s: %w", v.ID, err)
	}
	return nil
}

// AllowSSH replaces the addresses the hub's firewall admits SSH from.
func AllowSSH(ctx context.Context, p cloud.Provider, name string, cidrs []string, dryRun bool) (Step, error) {
	if err := ValidName(name); err != nil {
		return Step{}, err
	}
	sshFrom, err := cloud.NormalizeCIDRs(cidrs)
	if err != nil {
		return Step{}, err
	}
	if len(sshFrom) == 0 {
		return Step{}, fmt.Errorf("hubvm: name at least one address to admit SSH from")
	}
	fws, err := p.Firewalls(ctx, cloud.Filter{Name: FirewallName(name)})
	if err != nil {
		return Step{}, fmt.Errorf("hubvm: list firewalls: %w", err)
	}
	if len(fws) == 0 {
		return Step{}, fmt.Errorf("hubvm: hub %s has no firewall (%s); run create first", name, FirewallName(name))
	}
	_, step, err := ensureFirewall(ctx, p, name, sshFrom, dryRun)
	return step, err
}

func orNone(ip string) string {
	if ip == "" {
		return "no address yet"
	}
	return ip
}
