package dotest

import (
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/digitalocean/godo"
)

// volumeSnapshot is a volume snapshot as the API holds it, with the order it was made in.
type volumeSnapshot struct {
	godo.Snapshot
	seq int
}

// snapshotSize is the billable size the emulator reports for a snapshot of a volume: a fraction
// of the volume, as a real snapshot holds only the data written (DigitalOcean reports 77.2326 for
// a 100 GiB volume holding 77 GiB). The emulator holds no data, so the fraction is made up.
func snapshotSize(volumeGiB int64) float64 {
	return math.Round(float64(volumeGiB)*0.772326*1e4) / 1e4
}

// VolumeSource is the id of the snapshot a volume was made from: "" for an empty one, or one the
// emulator does not have.
func (s *Server) VolumeSource(volumeID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volumeSource[volumeID]
}

func (s *Server) snapshotList(keep func(*volumeSnapshot) bool) []godo.Snapshot {
	all := make([]*volumeSnapshot, 0, len(s.snapshots))
	for _, v := range s.snapshots {
		if keep(v) {
			all = append(all, v)
		}
	}
	slices.SortFunc(all, func(a, b *volumeSnapshot) int { return a.seq - b.seq })
	out := make([]godo.Snapshot, len(all))
	for i, v := range all {
		out[i] = v.Snapshot
	}
	return out
}

// createVolumeSnapshot is POST /v2/volumes/{id}/snapshots: the snapshot exists when the API
// answers (201, no action). A volume may be attached; names need not be unique.
func (s *Server) createVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := s.volumes[r.PathValue("id")]
	if !ok {
		notFound(w)
		return
	}
	var req godo.SnapshotCreateRequest
	if !decode(w, r, &req) {
		return
	}
	switch {
	case req.Name == "":
		apiError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	case !s.validTags(w, req.Tags):
		return
	}
	for _, t := range req.Tags {
		s.tags[t] = true
	}
	s.snapSeq++
	snap := &volumeSnapshot{seq: s.snapSeq, Snapshot: godo.Snapshot{
		ID: uuid(), Name: req.Name, ResourceID: v.ID, ResourceType: "volume",
		Regions: []string{v.Region.Slug}, MinDiskSize: int(v.SizeGigaBytes),
		SizeGigaBytes: snapshotSize(v.SizeGigaBytes), Created: time.Now().UTC().Format(time.RFC3339),
		Tags: nonNil(slices.Clone(req.Tags)),
	}}
	s.snapshots[snap.ID] = snap
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot": snap.Snapshot})
}

// volumeSubresource is GET /v2/volumes/{a}/{b}: a volume's snapshots (/v2/volumes/{id}/snapshots)
// or one volume snapshot (/v2/volumes/snapshots/{id}). One pattern serves both, since neither is
// more specific than the other.
func (s *Server) volumeSubresource(w http.ResponseWriter, r *http.Request) {
	a, b := r.PathValue("a"), r.PathValue("b")
	switch {
	case a == "snapshots":
		s.getSnapshot(w, b)
	case b == "snapshots":
		if _, ok := s.volumes[a]; !ok {
			notFound(w)
			return
		}
		s.writeList(w, r, "snapshots", s.snapshotList(func(v *volumeSnapshot) bool { return v.ResourceID == a }))
	default:
		notFound(w)
	}
}

// listSnapshots is GET /v2/snapshots. The emulator holds volume snapshots only, so
// resource_type=droplet lists none; region narrows the list as the API does.
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ, region := q.Get("resource_type"), q.Get("region")
	s.writeList(w, r, "snapshots", s.snapshotList(func(v *volumeSnapshot) bool {
		return (typ == "" || typ == v.ResourceType) && (region == "" || slices.Contains(v.Regions, region))
	}))
}

func (s *Server) getSnapshotByID(w http.ResponseWriter, r *http.Request) {
	s.getSnapshot(w, r.PathValue("id"))
}

func (s *Server) getSnapshot(w http.ResponseWriter, id string) {
	v, ok := s.snapshots[id]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": v.Snapshot})
}

// deleteSnapshot is DELETE /v2/snapshots/{id} and /v2/volumes/snapshots/{id}. The volumes made
// from the snapshot, and the one it was taken of, are untouched.
func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.snapshots[id]; !ok {
		notFound(w)
		return
	}
	delete(s.snapshots, id)
	w.WriteHeader(http.StatusNoContent)
}
