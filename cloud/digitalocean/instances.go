package digitalocean

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// Synthetic operation ids for what DigitalOcean completes without an action: Wait recognises
// them and polls the Droplet instead.
const (
	opDropletGone   = "droplet-gone:"   // a delete: done when the Droplet answers 404
	opDropletActive = "droplet-active:" // a create without an action link: done when it is active
)

// Instances lists the Droplets matching the filter. The API filters by tag or by name but not
// both at once, so a second field is matched here.
func (p *Provider) Instances(ctx context.Context, f cloud.Filter) ([]cloud.Instance, error) {
	list := p.c.Droplets.List
	switch {
	case f.Tag != "":
		list = func(ctx context.Context, opt *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
			return p.c.Droplets.ListByTag(ctx, f.Tag, opt)
		}
	case f.Name != "":
		list = func(ctx context.Context, opt *godo.ListOptions) ([]godo.Droplet, *godo.Response, error) {
			return p.c.Droplets.ListByName(ctx, f.Name, opt)
		}
	}
	droplets, err := listAll(ctx, "list droplets", list)
	if err != nil {
		return nil, err
	}
	var out []cloud.Instance
	for _, d := range droplets {
		if (f.Tag != "" && !hasTag(d.Tags, f.Tag)) || (f.Name != "" && d.Name != f.Name) {
			continue
		}
		out = append(out, toInstance(&d))
	}
	return out, nil
}

// Instance reads one Droplet.
func (p *Provider) Instance(ctx context.Context, id string) (cloud.Instance, error) {
	d, err := p.droplet(ctx, id)
	if err != nil {
		return cloud.Instance{}, err
	}
	return toInstance(d), nil
}

func (p *Provider) droplet(ctx context.Context, id string) (*godo.Droplet, error) {
	n, err := dropletID(id)
	if err != nil {
		return nil, err
	}
	d, _, err := p.c.Droplets.Get(ctx, n)
	if err != nil {
		return nil, apiErr("get droplet "+id, err)
	}
	return d, nil
}

// CreateInstance creates a Droplet and puts it in the configured project. When the project
// assignment fails the Droplet exists anyway: the Instance and Operation come back with the
// error, so the caller can wait for it and delete it, or find it again by its tags.
func (p *Provider) CreateInstance(ctx context.Context, spec cloud.InstanceSpec) (cloud.Instance, cloud.Operation, error) {
	if spec.Name == "" || spec.Region == "" || spec.Size == "" || spec.Image == "" {
		return cloud.Instance{}, cloud.Operation{}, fmt.Errorf("digitalocean: create droplet: %w: name, region, size and image are required", cloud.ErrInvalid)
	}
	req := &godo.DropletCreateRequest{
		Name:       spec.Name,
		Region:     spec.Region,
		Size:       spec.Size,
		Image:      godo.DropletCreateImage{Slug: spec.Image},
		UserData:   spec.UserData,
		Monitoring: spec.Monitoring,
		Tags:       spec.Tags,
		VPCUUID:    spec.Network,
	}
	if n, err := strconv.Atoi(spec.Image); err == nil {
		req.Image = godo.DropletCreateImage{ID: n}
	}
	for _, k := range spec.SSHKeyIDs {
		if n, err := strconv.Atoi(k); err == nil {
			req.SSHKeys = append(req.SSHKeys, godo.DropletCreateSSHKey{ID: n})
		} else {
			req.SSHKeys = append(req.SSHKeys, godo.DropletCreateSSHKey{Fingerprint: k})
		}
	}
	for _, v := range spec.VolumeIDs {
		req.Volumes = append(req.Volumes, godo.DropletCreateVolume{ID: v})
	}
	project, err := p.projectID(ctx)
	if err != nil {
		return cloud.Instance{}, cloud.Operation{}, err
	}
	d, resp, err := p.c.Droplets.Create(ctx, req)
	if err != nil {
		return cloud.Instance{}, cloud.Operation{}, apiErr("create droplet "+spec.Name, err)
	}
	inst := toInstance(d)
	op := cloud.Operation{ID: opDropletActive + inst.ID, Kind: "create", Resource: inst.ID}
	if resp != nil && resp.Links != nil && len(resp.Links.Actions) > 0 {
		op.ID = strconv.Itoa(resp.Links.Actions[0].ID)
	}
	return inst, op, p.assign(ctx, project, d.URN())
}

// DeleteInstance destroys a Droplet; DigitalOcean detaches its volumes and unassigns its
// reserved IP, and deletes neither. The delete has no action, so the Operation is done when the
// Droplet is gone.
func (p *Provider) DeleteInstance(ctx context.Context, id string) (cloud.Operation, error) {
	n, err := dropletID(id)
	if err != nil {
		return cloud.Operation{}, err
	}
	if _, err := p.c.Droplets.Delete(ctx, n); err != nil {
		return cloud.Operation{}, apiErr("delete droplet "+id, err)
	}
	return cloud.Operation{ID: opDropletGone + id, Kind: "delete", Resource: id}, nil
}

// StartInstance powers a Droplet on; one already on needs no operation.
func (p *Provider) StartInstance(ctx context.Context, id string) (cloud.Operation, error) {
	d, err := p.droplet(ctx, id)
	if err != nil {
		return cloud.Operation{}, err
	}
	if d.Status == "active" {
		return cloud.Operation{Kind: "start", Resource: id}, nil
	}
	a, _, err := p.c.DropletActions.PowerOn(ctx, d.ID)
	if err != nil {
		return cloud.Operation{}, apiErr("power on droplet "+id, err)
	}
	return actionOp("start", id, a), nil
}

// StopInstance shuts a Droplet down (graceful: the OS is asked through ACPI) or cuts its power;
// one already off needs no operation. A stopped Droplet still bills.
func (p *Provider) StopInstance(ctx context.Context, id string, graceful bool) (cloud.Operation, error) {
	d, err := p.droplet(ctx, id)
	if err != nil {
		return cloud.Operation{}, err
	}
	if d.Status == "off" {
		return cloud.Operation{Kind: "stop", Resource: id}, nil
	}
	act, what := p.c.DropletActions.PowerOff, "power off"
	if graceful {
		act, what = p.c.DropletActions.Shutdown, "shut down"
	}
	a, _, err := act(ctx, d.ID)
	if err != nil {
		return cloud.Operation{}, apiErr(what+" droplet "+id, err)
	}
	return actionOp("stop", id, a), nil
}

// ResizeInstance changes a stopped Droplet's CPU and memory, never its disk, and leaves it off.
// DigitalOcean's resize would power a running Droplet off by itself, but hard, with no
// shutdown; the interface wants the caller to stop it first (gracefully, at a boundary), so a
// running Droplet is refused. The resize is checked with CanResize first, against the Droplet's
// current disk, so a refusal costs no action.
func (p *Provider) ResizeInstance(ctx context.Context, id, size string) (cloud.Operation, error) {
	d, err := p.droplet(ctx, id)
	if err != nil {
		return cloud.Operation{}, err
	}
	if d.SizeSlug == size {
		return cloud.Operation{Kind: "resize", Resource: id}, nil
	}
	if d.Status != "off" {
		return cloud.Operation{}, fmt.Errorf("digitalocean: resize droplet %s: %w: it is %s; stop it first", id, cloud.ErrInvalid, d.Status)
	}
	sizes, err := p.Sizes(ctx)
	if err != nil {
		return cloud.Operation{}, err
	}
	from := cloud.Size{Slug: d.SizeSlug, Family: sizeFamily(d.SizeSlug)}
	var to *cloud.Size
	for i := range sizes {
		switch sizes[i].Slug {
		case d.SizeSlug:
			from = sizes[i]
		case size:
			to = &sizes[i]
		}
	}
	from.DiskGiB = d.Disk
	if to == nil {
		return cloud.Operation{}, fmt.Errorf("digitalocean: resize droplet %s: %w: size %q is not offered", id, cloud.ErrInvalid, size)
	}
	if d.Region != nil && !slices.Contains(to.Regions, d.Region.Slug) {
		return cloud.Operation{}, fmt.Errorf("digitalocean: resize droplet %s: %w: size %q is not offered in %s", id, cloud.ErrInvalid, size, d.Region.Slug)
	}
	if err := p.CanResize(from, *to); err != nil {
		return cloud.Operation{}, fmt.Errorf("resize droplet %s: %w", id, err)
	}
	a, _, err := p.c.DropletActions.Resize(ctx, d.ID, size, false)
	if err != nil {
		return cloud.Operation{}, apiErr("resize droplet "+id, err)
	}
	return actionOp("resize", id, a), nil
}

// Adopt tags a Droplet made outside gravel with the tags gravel finds it by. It does not move
// the Droplet into the configured project: the scaffolding that made it chose its project.
func (p *Provider) Adopt(ctx context.Context, id string, tags []string) (cloud.Instance, error) {
	d, err := p.droplet(ctx, id)
	if err != nil {
		return cloud.Instance{}, err
	}
	var missing []string
	for _, t := range tags {
		if !hasTag(d.Tags, t) {
			missing = append(missing, t)
		}
	}
	if err := p.ensureTags(ctx, missing); err != nil {
		return cloud.Instance{}, err
	}
	for _, t := range missing {
		_, err := p.c.Tags.TagResources(ctx, t, &godo.TagResourcesRequest{
			Resources: []godo.Resource{{ID: id, Type: godo.DropletResourceType}},
		})
		if err != nil {
			return cloud.Instance{}, apiErr("tag droplet "+id+" with "+t, err)
		}
	}
	return p.Instance(ctx, id)
}

func toInstance(d *godo.Droplet) cloud.Instance {
	inst := cloud.Instance{
		ID:        strconv.Itoa(d.ID),
		Name:      d.Name,
		Size:      d.SizeSlug,
		DiskGiB:   d.Disk,
		Status:    dropletStatus(d.Status),
		Tags:      append([]string(nil), d.Tags...),
		VolumeIDs: append([]string(nil), d.VolumeIDs...),
	}
	if inst.Size == "" && d.Size != nil {
		inst.Size = d.Size.Slug
	}
	if d.Region != nil {
		inst.Region = d.Region.Slug
	}
	if d.Image != nil {
		inst.Image = d.Image.Slug
		if inst.Image == "" && d.Image.ID != 0 {
			inst.Image = strconv.Itoa(d.Image.ID)
		}
	}
	if d.Networks != nil {
		inst.PublicIPv4, _ = d.PublicIPv4()
		inst.PrivateIPv4, _ = d.PrivateIPv4()
	}
	if t, err := time.Parse(time.RFC3339, d.Created); err == nil {
		inst.Created = t
	}
	return inst
}

func dropletStatus(s string) cloud.Status {
	switch s {
	case "new":
		return cloud.StatusPending
	case "active":
		return cloud.StatusRunning
	case "off":
		return cloud.StatusStopped
	}
	return cloud.StatusUnknown
}

// actionOp is the Operation for an action; one DigitalOcean already finished needs no wait.
func actionOp(kind, resource string, a *godo.Action) cloud.Operation {
	op := cloud.Operation{Kind: kind, Resource: resource}
	if a != nil && a.Status != godo.ActionCompleted {
		op.ID = strconv.Itoa(a.ID)
	}
	return op
}
