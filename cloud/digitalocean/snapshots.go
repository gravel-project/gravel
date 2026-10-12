package digitalocean

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// Volume snapshots sit under the token scope block_storage_snapshot (create, read, delete), not
// snapshot, which is the Droplet snapshots' (gravel#166). A snapshot is made through its volume
// (POST /v2/volumes/{id}/snapshots) and read and deleted under /v2/volumes/snapshots/{id}, the
// paths that scope grants: godo's Storage.GetSnapshot and DeleteSnapshot use /v2/snapshots/{id},
// whose delete wants snapshot:delete as well, so those two go through godo's client with the
// volume paths. The list is /v2/snapshots?resource_type=volume, which needs snapshot:read; the
// control panel adds that to any token with block_storage_snapshot:read, which depends on it.
const volumeSnapshotPath = "v2/volumes/snapshots/"

// CreateVolumeSnapshot snapshots a volume. DigitalOcean answers 201 with the snapshot, which a
// volume can be made from at once; it waits on no action.
func (p *Provider) CreateVolumeSnapshot(ctx context.Context, volumeID, name string, tags []string) (cloud.Snapshot, error) {
	if strings.TrimSpace(name) == "" {
		return cloud.Snapshot{}, fmt.Errorf("digitalocean: snapshot of volume %s: %w: a snapshot needs a name", volumeID, cloud.ErrInvalid)
	}
	s, _, err := p.c.Storage.CreateSnapshot(ctx, &godo.SnapshotCreateRequest{VolumeID: volumeID, Name: name, Tags: tags})
	if err != nil {
		return cloud.Snapshot{}, apiErr("snapshot volume "+volumeID, err)
	}
	return toSnapshot(s), nil
}

// GetVolumeSnapshot reads a volume snapshot. A Droplet snapshot's id is not found, nor is
// anything that is not a UUID, which every volume snapshot's id is.
func (p *Provider) GetVolumeSnapshot(ctx context.Context, id string) (cloud.Snapshot, error) {
	s, err := p.volumeSnapshot(ctx, id)
	if err != nil {
		return cloud.Snapshot{}, err
	}
	return toSnapshot(s), nil
}

func (p *Provider) volumeSnapshot(ctx context.Context, id string) (*godo.Snapshot, error) {
	if !uuidRE.MatchString(id) {
		return nil, fmt.Errorf("digitalocean: %q is not a snapshot id: %w", id, cloud.ErrNotFound)
	}
	req, err := p.c.NewRequest(ctx, http.MethodGet, volumeSnapshotPath+id, nil)
	if err != nil {
		return nil, fmt.Errorf("digitalocean: get snapshot %s: %w", id, err)
	}
	var root struct {
		Snapshot *godo.Snapshot `json:"snapshot"`
	}
	if _, err := p.c.Do(ctx, req, &root); err != nil {
		return nil, apiErr("get snapshot "+id, err)
	}
	if root.Snapshot == nil || (root.Snapshot.ResourceType != "" && root.Snapshot.ResourceType != "volume") {
		return nil, fmt.Errorf("digitalocean: snapshot %s is not a volume snapshot: %w", id, cloud.ErrNotFound)
	}
	return root.Snapshot, nil
}

// FindVolumeSnapshot lists the volume snapshots and keeps the ones with the name in the region.
func (p *Provider) FindVolumeSnapshot(ctx context.Context, name, region string) (cloud.Snapshot, error) {
	all, err := p.volumeSnapshots(ctx)
	if err != nil {
		return cloud.Snapshot{}, err
	}
	var found []godo.Snapshot
	for _, s := range all {
		if s.Name == name && slices.Contains(s.Regions, region) {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return cloud.Snapshot{}, fmt.Errorf("digitalocean: volume snapshot %q in %s: %w", name, region, cloud.ErrNotFound)
	case 1:
		return toSnapshot(&found[0]), nil
	}
	ids := make([]string, len(found))
	for i, s := range found {
		ids[i] = s.ID
	}
	return cloud.Snapshot{}, fmt.Errorf("digitalocean: volume snapshot %q in %s: %w: %d snapshots have the name (%s); name one by its id",
		name, region, cloud.ErrInvalid, len(found), strings.Join(ids, ", "))
}

// ListVolumeSnapshots lists the volume snapshots; the API filters by neither name nor tag, so
// both are matched here.
func (p *Provider) ListVolumeSnapshots(ctx context.Context, f cloud.Filter) ([]cloud.Snapshot, error) {
	all, err := p.volumeSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var out []cloud.Snapshot
	for _, s := range all {
		if (f.Tag != "" && !hasTag(s.Tags, f.Tag)) || (f.Name != "" && s.Name != f.Name) {
			continue
		}
		out = append(out, toSnapshot(&s))
	}
	return out, nil
}

func (p *Provider) volumeSnapshots(ctx context.Context) ([]godo.Snapshot, error) {
	return listAll(ctx, "list volume snapshots", p.c.Snapshots.ListVolume)
}

// DeleteVolumeSnapshot deletes a volume snapshot.
func (p *Provider) DeleteVolumeSnapshot(ctx context.Context, id string) error {
	if !uuidRE.MatchString(id) {
		return fmt.Errorf("digitalocean: %q is not a snapshot id: %w", id, cloud.ErrNotFound)
	}
	req, err := p.c.NewRequest(ctx, http.MethodDelete, volumeSnapshotPath+id, nil)
	if err != nil {
		return fmt.Errorf("digitalocean: delete snapshot %s: %w", id, err)
	}
	_, err = p.c.Do(ctx, req, nil)
	return apiErr("delete snapshot "+id, err)
}

// sourceSnapshot resolves a VolumeSpec's snapshot and checks the volume can be made from it, so
// a refusal says why before anything is asked of the API.
func (p *Provider) sourceSnapshot(ctx context.Context, spec cloud.VolumeSpec) (string, error) {
	ref := spec.FromSnapshot
	if err := ref.Check(); err != nil {
		return "", err
	}
	var snap cloud.Snapshot
	if ref.ID != "" {
		s, err := p.volumeSnapshot(ctx, ref.ID)
		if err != nil {
			return "", err
		}
		snap = toSnapshot(s)
		// A volume snapshot is kept in one region; should one ever list several, the volume's
		// is the one that counts.
		if slices.Contains(s.Regions, spec.Region) {
			snap.Region = spec.Region
		}
	} else {
		s, err := p.FindVolumeSnapshot(ctx, ref.Name, spec.Region)
		if err != nil {
			return "", err
		}
		snap = s
		snap.Region = spec.Region
	}
	if err := snap.Fits(spec); err != nil {
		return "", err
	}
	return snap.ID, nil
}

func toSnapshot(s *godo.Snapshot) cloud.Snapshot {
	out := cloud.Snapshot{
		ID:           s.ID,
		Name:         s.Name,
		SizeGiB:      s.SizeGigaBytes,
		MinVolumeGiB: s.MinDiskSize,
		Tags:         append([]string(nil), s.Tags...),
	}
	if out.MinVolumeGiB <= 0 {
		out.MinVolumeGiB = int(math.Ceil(s.SizeGigaBytes))
	}
	if len(s.Regions) > 0 {
		out.Region = s.Regions[0]
	}
	if t, err := time.Parse(time.RFC3339, s.Created); err == nil {
		out.CreatedAt = t
	}
	return out
}
