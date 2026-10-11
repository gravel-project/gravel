package digitalocean

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// volumeNameRE is DigitalOcean's rule for a volume name: lowercase letters, digits and hyphens,
// starting with a letter, at most 64 characters. Checked here so a bad name costs no request.
var volumeNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Volumes lists the volumes matching the filter. The API filters by name; the tag is matched
// here.
func (p *Provider) Volumes(ctx context.Context, f cloud.Filter) ([]cloud.Volume, error) {
	vols, err := listAll(ctx, "list volumes", func(ctx context.Context, opt *godo.ListOptions) ([]godo.Volume, *godo.Response, error) {
		return p.c.Storage.ListVolumes(ctx, &godo.ListVolumeParams{Name: f.Name, ListOptions: opt})
	})
	if err != nil {
		return nil, err
	}
	var out []cloud.Volume
	for _, v := range vols {
		if (f.Tag != "" && !hasTag(v.Tags, f.Tag)) || (f.Name != "" && v.Name != f.Name) {
			continue
		}
		out = append(out, toVolume(&v))
	}
	return out, nil
}

// CreateVolume creates an unformatted volume (no filesystem type: the instance's cloud-init
// formats it once) and puts it in the configured project. When the project assignment fails
// the volume exists anyway and comes back with the error.
func (p *Provider) CreateVolume(ctx context.Context, spec cloud.VolumeSpec) (cloud.Volume, error) {
	if !volumeNameRE.MatchString(spec.Name) {
		return cloud.Volume{}, fmt.Errorf("digitalocean: create volume %q: %w: a name is lowercase letters, digits and hyphens, starts with a letter, at most 64 characters", spec.Name, cloud.ErrInvalid)
	}
	if spec.Region == "" || spec.SizeGiB < 1 {
		return cloud.Volume{}, fmt.Errorf("digitalocean: create volume %s: %w: a region and a size are required", spec.Name, cloud.ErrInvalid)
	}
	project, err := p.projectID(ctx)
	if err != nil {
		return cloud.Volume{}, err
	}
	v, _, err := p.c.Storage.CreateVolume(ctx, &godo.VolumeCreateRequest{
		Region:        spec.Region,
		Name:          spec.Name,
		SizeGigaBytes: int64(spec.SizeGiB),
		Tags:          spec.Tags,
	})
	if err != nil {
		return cloud.Volume{}, apiErr("create volume "+spec.Name, err)
	}
	return toVolume(v), p.assign(ctx, project, v.URN())
}

// DeleteVolume destroys a detached volume and its data.
func (p *Provider) DeleteVolume(ctx context.Context, id string) error {
	_, err := p.c.Storage.DeleteVolume(ctx, id)
	return apiErr("delete volume "+id, err)
}

// AttachVolume attaches a volume to a Droplet in its region.
func (p *Provider) AttachVolume(ctx context.Context, volumeID, instanceID string) (cloud.Operation, error) {
	n, err := dropletID(instanceID)
	if err != nil {
		return cloud.Operation{}, err
	}
	a, _, err := p.c.StorageActions.Attach(ctx, volumeID, n)
	if err != nil {
		return cloud.Operation{}, apiErr("attach volume "+volumeID+" to droplet "+instanceID, err)
	}
	return actionOp("attach", volumeID, a), nil
}

// DetachVolume detaches a volume from a Droplet.
func (p *Provider) DetachVolume(ctx context.Context, volumeID, instanceID string) (cloud.Operation, error) {
	n, err := dropletID(instanceID)
	if err != nil {
		return cloud.Operation{}, err
	}
	a, _, err := p.c.StorageActions.DetachByDropletID(ctx, volumeID, n)
	if err != nil {
		return cloud.Operation{}, apiErr("detach volume "+volumeID+" from droplet "+instanceID, err)
	}
	return actionOp("detach", volumeID, a), nil
}

// VolumeDevice is where a Droplet's OS sees the volume: DigitalOcean names the SCSI id after
// the volume's name.
func (p *Provider) VolumeDevice(v cloud.Volume) string {
	return "/dev/disk/by-id/scsi-0DO_Volume_" + v.Name
}

func toVolume(v *godo.Volume) cloud.Volume {
	out := cloud.Volume{
		ID:      v.ID,
		Name:    v.Name,
		SizeGiB: int(v.SizeGigaBytes),
		Tags:    append([]string(nil), v.Tags...),
	}
	if v.Region != nil {
		out.Region = v.Region.Slug
	}
	for _, d := range v.DropletIDs {
		out.InstanceIDs = append(out.InstanceIDs, strconv.Itoa(d))
	}
	return out
}
