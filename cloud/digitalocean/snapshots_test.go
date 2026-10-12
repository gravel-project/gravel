package digitalocean_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/digitalocean"
	"github.com/gravel-project/gravel/cloud/digitalocean/dotest"
)

// TestVolumeSnapshots checks the requests behind the snapshot calls: made through the volume,
// read and deleted under /v2/volumes/snapshots/ (the paths the block_storage_snapshot scope
// grants), a volume made from one carrying its id, and a refusal sending no create.
func TestVolumeSnapshots(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()
	src, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 100})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := p.CreateVolumeSnapshot(ctx, src.ID, "data-seed", []string{"gravel"})
	if err != nil {
		t.Fatalf("CreateVolumeSnapshot: %v", err)
	}
	if n := count(srv.Requests(), http.MethodPost, "/v2/volumes/"+src.ID+"/snapshots"); n != 1 {
		t.Errorf("snapshot create requests = %d, want 1 through the volume", n)
	}
	// The emulator reports a fraction of the volume, as DigitalOcean does: 77.2326 of 100 GiB.
	if snap.SizeGiB != 77.2326 || snap.MinVolumeGiB != 100 || snap.Region != "sfo3" || snap.CreatedAt.IsZero() {
		t.Errorf("snapshot = %+v, want 77.2326 GiB of data, 100 GiB minimum, in sfo3, with a time", snap)
	}

	if _, err := p.GetVolumeSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("GetVolumeSnapshot: %v", err)
	}
	if n := count(srv.Requests(), http.MethodGet, "/v2/volumes/snapshots/"+snap.ID); n != 1 {
		t.Errorf("GET /v2/volumes/snapshots/{id} requests = %d, want 1", n)
	}
	before := len(srv.Requests())
	if _, err := p.GetVolumeSnapshot(ctx, "not-a-uuid"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("GetVolumeSnapshot of a non-UUID: %v, want ErrNotFound", err)
	}
	if len(srv.Requests()) != before {
		t.Error("GetVolumeSnapshot of a non-UUID sent a request")
	}

	v, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data-2", Region: "sfo3", SizeGiB: 120, FromSnapshot: &cloud.SnapshotRef{Name: "data-seed"}})
	if err != nil {
		t.Fatalf("CreateVolume from the snapshot by name: %v", err)
	}
	if got := srv.VolumeSource(v.ID); got != snap.ID {
		t.Errorf("the volume was made from %q, want %s", got, snap.ID)
	}

	creates := count(srv.Requests(), http.MethodPost, "/v2/volumes")
	for _, spec := range []cloud.VolumeSpec{
		{Name: "small", Region: "sfo3", SizeGiB: 99, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID}},
		{Name: "far", Region: "nyc3", SizeGiB: 100, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID}},
		{Name: "far-by-name", Region: "nyc3", SizeGiB: 100, FromSnapshot: &cloud.SnapshotRef{Name: "data-seed"}},
	} {
		_, err := p.CreateVolume(ctx, spec)
		if !errors.Is(err, cloud.ErrInvalid) && !errors.Is(err, cloud.ErrNotFound) {
			t.Errorf("CreateVolume(%s): %v, want a refusal", spec.Name, err)
		}
	}
	if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "small", Region: "sfo3", SizeGiB: 99, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID}}); err == nil ||
		!strings.Contains(err.Error(), "at least 100 GiB") {
		t.Errorf("too small a volume: %v, want the minimum named", err)
	}
	if n := count(srv.Requests(), http.MethodPost, "/v2/volumes") - creates; n != 0 {
		t.Errorf("refused creates sent %d POST /v2/volumes, want none", n)
	}

	if err := p.DeleteVolumeSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("DeleteVolumeSnapshot: %v", err)
	}
	if n := count(srv.Requests(), http.MethodDelete, "/v2/volumes/snapshots/"+snap.ID); n != 1 {
		t.Errorf("DELETE /v2/volumes/snapshots/{id} requests = %d, want 1", n)
	}
	if got := srv.Counts().Snapshots; got != 0 {
		t.Errorf("the emulator holds %d snapshots, want 0", got)
	}
}

// TestSnapshotScope is a token without the block_storage_snapshot scope: DigitalOcean answers
// 403, which is ErrUnauthorized, never a quiet not-found.
func TestSnapshotScope(t *testing.T) {
	p, srv := newProvider(t, "")
	ctx := t.Context()
	src, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "data", Region: "sfo3", SizeGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv.Fault(http.MethodPost, "/v2/volumes/"+src.ID+"/snapshots", http.StatusForbidden, 1)
	if _, err := p.CreateVolumeSnapshot(ctx, src.ID, "seed", nil); !errors.Is(err, digitalocean.ErrUnauthorized) {
		t.Errorf("CreateVolumeSnapshot with a 403: %v, want ErrUnauthorized", err)
	}
	id := "00000000-0000-4000-8000-000000000000"
	srv.Fault(http.MethodGet, "/v2/volumes/snapshots/"+id, http.StatusForbidden, 1)
	if _, err := p.GetVolumeSnapshot(ctx, id); !errors.Is(err, digitalocean.ErrUnauthorized) {
		t.Errorf("GetVolumeSnapshot with a 403: %v, want ErrUnauthorized", err)
	}
	if _, err := p.GetVolumeSnapshot(ctx, id); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("GetVolumeSnapshot of a snapshot nothing has: %v, want ErrNotFound", err)
	}
}

// TestEmulatorRefusesASnapshotVolume keeps the emulator honest without the provider's own
// checks in front of it: the API itself refuses a volume from a snapshot in another region or
// smaller than the volume it was taken of.
func TestEmulatorRefusesASnapshotVolume(t *testing.T) {
	srv := dotest.New(t)
	c, err := godo.New(&http.Client{Timeout: 10 * time.Second, Transport: bearer{dotest.Token}}, godo.SetBaseURL(srv.URL()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	v, _, err := c.Storage.CreateVolume(ctx, &godo.VolumeCreateRequest{Name: "data", Region: "sfo3", SizeGigaBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := c.Storage.CreateSnapshot(ctx, &godo.SnapshotCreateRequest{VolumeID: v.ID, Name: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []godo.VolumeCreateRequest{
		{Name: "far", Region: "nyc3", SizeGigaBytes: 10, SnapshotID: s.ID},
		{Name: "small", Region: "sfo3", SizeGigaBytes: 9, SnapshotID: s.ID},
	} {
		_, resp, err := c.Storage.CreateVolume(ctx, &req)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("POST /v2/volumes %s: %v, want 422", req.Name, err)
		}
	}
	_, resp, err := c.Storage.CreateVolume(ctx, &godo.VolumeCreateRequest{Name: "none", Region: "sfo3", SizeGigaBytes: 10, SnapshotID: "00000000-0000-4000-8000-000000000000"})
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /v2/volumes from a snapshot nothing has: %v, want 404", err)
	}
	if _, _, err := c.Storage.CreateVolume(ctx, &godo.VolumeCreateRequest{Name: "copy", Region: "sfo3", SizeGigaBytes: 10, SnapshotID: s.ID}); err != nil {
		t.Errorf("POST /v2/volumes from the snapshot: %v", err)
	}
	// The snapshot outlives the volume it was taken of, and lists under both paths.
	if _, err := c.Storage.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Snapshots.Get(ctx, s.ID); err != nil {
		t.Errorf("GET /v2/snapshots/{id} after its volume went: %v", err)
	}
	list, _, err := c.Snapshots.ListVolume(ctx, nil)
	if err != nil || len(list) != 1 || list[0].ResourceType != "volume" {
		t.Errorf("GET /v2/snapshots?resource_type=volume = %+v, %v, want the snapshot", list, err)
	}
	if list, _, err := c.Snapshots.ListDroplet(ctx, nil); err != nil || len(list) != 0 {
		t.Errorf("GET /v2/snapshots?resource_type=droplet = %+v, %v, want none", list, err)
	}
}

// bearer adds the emulator's token to a plain godo client.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
