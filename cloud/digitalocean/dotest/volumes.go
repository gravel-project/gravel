package dotest

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/digitalocean/godo"
)

var volumeNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func (s *Server) volumeList() []godo.Volume {
	out := make([]godo.Volume, 0, len(s.volumes))
	for _, v := range s.volumes {
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b godo.Volume) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

func (s *Server) volumeByRef(id, name, region string) *godo.Volume {
	if id != "" {
		return s.volumes[id]
	}
	for _, v := range s.volumes {
		if v.Name == name && v.Region != nil && v.Region.Slug == region {
			return v
		}
	}
	return nil
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, region := q.Get("name"), q.Get("region")
	var out []godo.Volume
	for _, v := range s.volumeList() {
		if (name == "" || v.Name == name) && (region == "" || v.Region.Slug == region) {
			out = append(out, v)
		}
	}
	s.writeList(w, r, "volumes", nonNil(out))
}

func (s *Server) getVolume(w http.ResponseWriter, r *http.Request) {
	v, ok := s.volumes[r.PathValue("id")]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": v})
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var req godo.VolumeCreateRequest
	if !decode(w, r, &req) {
		return
	}
	region, ok := s.region(req.Region)
	switch {
	case !volumeNameRE.MatchString(req.Name):
		unprocessable(w, "name must be lowercase letters, numbers and hyphens, start with a letter, and be at most 64 characters")
		return
	case !ok:
		unprocessable(w, "region is invalid")
		return
	case req.SizeGigaBytes < 1 || req.SizeGigaBytes > 16384:
		unprocessable(w, "size_gigabytes must be between 1 and 16384")
		return
	case req.FilesystemType != "" && req.FilesystemType != "ext4" && req.FilesystemType != "xfs":
		unprocessable(w, "filesystem_type must be ext4 or xfs")
		return
	case s.volumeByRef("", req.Name, req.Region) != nil:
		unprocessable(w, fmt.Sprintf("a volume named %s already exists in %s", req.Name, req.Region))
		return
	case !s.validTags(w, req.Tags):
		return
	}
	for _, t := range req.Tags {
		s.tags[t] = true
	}
	v := &godo.Volume{
		ID: uuid(), Region: &region, Name: req.Name, SizeGigaBytes: req.SizeGigaBytes,
		Description: req.Description, DropletIDs: []int{}, CreatedAt: time.Now().UTC(),
		FilesystemType: req.FilesystemType, FilesystemLabel: req.FilesystemLabel, Tags: nonNil(slices.Clone(req.Tags)),
	}
	s.volumes[v.ID] = v
	writeJSON(w, http.StatusCreated, map[string]any{"volume": v})
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) {
	v, ok := s.volumes[r.PathValue("id")]
	if !ok {
		notFound(w)
		return
	}
	if len(v.DropletIDs) > 0 {
		apiError(w, http.StatusConflict, "conflict", "the volume is attached to a Droplet; detach it first")
		return
	}
	delete(s.volumes, v.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) volumeAction(w http.ResponseWriter, r *http.Request) {
	v, ok := s.volumes[r.PathValue("id")]
	if !ok {
		notFound(w)
		return
	}
	var req struct {
		Type      string `json:"type"`
		DropletID int    `json:"droplet_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	d, ok := s.droplets[req.DropletID]
	if !ok || !d.deleteAt.IsZero() {
		notFound(w)
		return
	}
	if s.pending(d.ID) {
		unprocessable(w, godo.ErrDropletPendingEvent)
		return
	}
	var apply func()
	switch req.Type {
	case "attach":
		switch {
		case v.Region.Slug != d.Region.Slug:
			unprocessable(w, "the volume and the Droplet must be in the same region")
			return
		case len(v.DropletIDs) > 0:
			unprocessable(w, godo.ErrVolumeAlreadyAttached)
			return
		}
		apply = func() {
			v.DropletIDs = []int{d.ID}
			d.VolumeIDs = append(d.VolumeIDs, v.ID)
		}
	case "detach":
		if !slices.Contains(v.DropletIDs, d.ID) {
			apiError(w, http.StatusNotFound, "not_found", godo.ErrAttachmentNotFound)
			return
		}
		apply = func() {
			v.DropletIDs = slices.DeleteFunc(v.DropletIDs, func(id int) bool { return id == d.ID })
			d.VolumeIDs = slices.DeleteFunc(d.VolumeIDs, func(id string) bool { return id == v.ID })
		}
	default:
		unprocessable(w, fmt.Sprintf("Action type %q is not supported by this emulator", req.Type))
		return
	}
	a := s.newAction(req.Type+"_volume", "backend", 0, d.ID, v.Region.Slug, apply)
	writeJSON(w, http.StatusAccepted, map[string]any{"action": a.Action})
}
