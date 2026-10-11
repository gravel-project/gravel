package dotest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/digitalocean/godo"
)

var (
	dropletNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,254}$`)
	tagRE         = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,255}$`)
)

func (s *Server) dropletList() []godo.Droplet {
	ids := make([]int, 0, len(s.droplets))
	for id := range s.droplets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]godo.Droplet, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.droplets[id].Droplet)
	}
	return out
}

func (s *Server) listDroplets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tag, name := q.Get("tag_name"), q.Get("name")
	if tag != "" && name != "" {
		unprocessable(w, "tag_name cannot be combined with name")
		return
	}
	var out []godo.Droplet
	for _, d := range s.dropletList() {
		if (tag == "" || slices.Contains(d.Tags, tag)) && (name == "" || d.Name == name) {
			out = append(out, d)
		}
	}
	s.writeList(w, r, "droplets", nonNil(out))
}

func (s *Server) lookupDroplet(w http.ResponseWriter, r *http.Request) *droplet {
	id, err := strconv.Atoi(r.PathValue("id"))
	d, ok := s.droplets[id]
	if err != nil || !ok {
		notFound(w)
		return nil
	}
	return d
}

func (s *Server) getDroplet(w http.ResponseWriter, r *http.Request) {
	if d := s.lookupDroplet(w, r); d != nil {
		writeJSON(w, http.StatusOK, map[string]any{"droplet": d.Droplet})
	}
}

// createDropletRequest is godo.DropletCreateRequest as received: its image, keys and volumes
// marshal to a slug or an id, which godo's types do not unmarshal.
type createDropletRequest struct {
	Name       string            `json:"name"`
	Region     string            `json:"region"`
	Size       string            `json:"size"`
	Image      json.RawMessage   `json:"image"`
	SSHKeys    []json.RawMessage `json:"ssh_keys"`
	Monitoring bool              `json:"monitoring"`
	UserData   string            `json:"user_data"`
	Volumes    []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"volumes"`
	Tags    []string `json:"tags"`
	VPCUUID string   `json:"vpc_uuid"`
}

func (s *Server) createDroplet(w http.ResponseWriter, r *http.Request) {
	var req createDropletRequest
	if !decode(w, r, &req) {
		return
	}
	if !dropletNameRE.MatchString(req.Name) {
		unprocessable(w, "Name is invalid: only letters, numbers, '.' and '-' are allowed")
		return
	}
	region, ok := s.region(req.Region)
	if !ok || !region.Available {
		unprocessable(w, "Region is not available")
		return
	}
	size, ok := s.size(req.Size)
	if !ok {
		unprocessable(w, "Size is invalid")
		return
	}
	if !slices.Contains(size.Regions, req.Region) {
		unprocessable(w, "Size is not available in this region.")
		return
	}
	image, ok := s.image(req.Image)
	if !ok {
		unprocessable(w, "You specified an invalid image for Droplet creation.")
		return
	}
	for _, raw := range req.SSHKeys {
		if !s.knownKey(raw) {
			unprocessable(w, "You specified an invalid SSH key for Droplet creation.")
			return
		}
	}
	if !s.validTags(w, req.Tags) {
		return
	}
	var vols []*godo.Volume
	for _, ref := range req.Volumes {
		v := s.volumeByRef(ref.ID, ref.Name, req.Region)
		switch {
		case v == nil:
			unprocessable(w, "Volume not found")
			return
		case v.Region == nil || v.Region.Slug != req.Region:
			unprocessable(w, "Volume and Droplet must be in the same region")
			return
		case len(v.DropletIDs) > 0:
			unprocessable(w, godo.ErrVolumeAlreadyAttached)
			return
		}
		vols = append(vols, v)
	}
	vpc := req.VPCUUID
	if vpc == "" {
		vpc = "vpc-default-" + req.Region
	}
	d := &droplet{Droplet: godo.Droplet{
		ID: s.id(), Name: req.Name, Memory: size.Memory, Vcpus: size.Vcpus, Disk: size.Disk,
		Region: &region, Image: &image, Size: &size, SizeSlug: size.Slug,
		Status: "new", Networks: &godo.Networks{V4: []godo.NetworkV4{}, V6: []godo.NetworkV6{}},
		Created: now(), Tags: nonNil(slices.Clone(req.Tags)), VolumeIDs: []string{},
		Features: []string{}, VPCUUID: vpc,
	}}
	if req.Monitoring {
		d.Features = append(d.Features, "monitoring")
	}
	for _, t := range req.Tags {
		s.tags[t] = true
	}
	for _, v := range vols { // attached at creation
		v.DropletIDs = []int{d.ID}
		d.VolumeIDs = append(d.VolumeIDs, v.ID)
	}
	s.droplets[d.ID] = d
	n := d.ID % 250
	a := s.newAction("create", "droplet", d.ID, d.ID, req.Region, func() {
		d.Status = "active"
		d.Networks.V4 = []godo.NetworkV4{
			{IPAddress: fmt.Sprintf("203.0.113.%d", n+1), Netmask: "255.255.255.0", Gateway: "203.0.113.254", Type: "public"},
			{IPAddress: fmt.Sprintf("10.120.0.%d", n+1), Netmask: "255.255.240.0", Gateway: "10.120.0.1", Type: "private"},
		}
	})
	body := map[string]any{"droplet": d.Droplet}
	if !s.noLinks {
		body["links"] = godo.Links{Actions: []godo.LinkAction{{ID: a.ID, Rel: "create", HREF: fmt.Sprintf("%s/v2/actions/%d", s.srv.URL, a.ID)}}}
	}
	writeJSON(w, http.StatusAccepted, body)
}

// deleteDroplet starts destroying a Droplet: its volumes are detached and its reserved IPs
// unassigned at once, and it disappears after the action delay.
func (s *Server) deleteDroplet(w http.ResponseWriter, r *http.Request) {
	d := s.lookupDroplet(w, r)
	if d == nil {
		return
	}
	if !d.deleteAt.IsZero() {
		notFound(w)
		return
	}
	for _, v := range s.volumes {
		v.DropletIDs = slices.DeleteFunc(v.DropletIDs, func(id int) bool { return id == d.ID })
	}
	for _, ip := range s.reserved {
		if ip.dropletID == d.ID {
			ip.dropletID = 0
		}
	}
	d.VolumeIDs = []string{}
	d.deleteAt = time.Now().Add(s.delay)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) dropletAction(w http.ResponseWriter, r *http.Request) {
	d := s.lookupDroplet(w, r)
	if d == nil {
		return
	}
	var req struct {
		Type string `json:"type"`
		Size string `json:"size"`
		Disk bool   `json:"disk"`
	}
	if !decode(w, r, &req) {
		return
	}
	if s.pending(d.ID) || !d.deleteAt.IsZero() {
		unprocessable(w, godo.ErrDropletPendingEvent)
		return
	}
	var apply func()
	switch req.Type {
	case "power_on":
		if d.Status == "active" {
			unprocessable(w, "Droplet is already powered on.")
			return
		}
		apply = func() { d.Status = "active" }
	case "power_off", "shutdown":
		if d.Status != "active" {
			unprocessable(w, "Droplet is already powered off.")
			return
		}
		apply = func() { d.Status = "off" }
	case "resize":
		size, ok := s.size(req.Size)
		switch {
		case !ok:
			unprocessable(w, "Size is invalid")
			return
		case !slices.Contains(size.Regions, d.Region.Slug):
			unprocessable(w, "Size is not available in this region.")
			return
		case size.Disk < d.Disk:
			unprocessable(w, "This size is not available because it has a smaller disk.")
			return
		}
		apply = func() {
			sz := size
			d.Size, d.SizeSlug, d.Memory, d.Vcpus = &sz, sz.Slug, sz.Memory, sz.Vcpus
			if req.Disk {
				d.Disk = sz.Disk
			}
			d.Status = "off" // the API powered it off for the resize and leaves it off
		}
	default:
		unprocessable(w, fmt.Sprintf("Action type %q is not supported by this emulator", req.Type))
		return
	}
	a := s.newAction(req.Type, "droplet", d.ID, d.ID, d.Region.Slug, apply)
	writeJSON(w, http.StatusCreated, map[string]any{"action": a.Action})
}

func (s *Server) image(raw json.RawMessage) (godo.Image, bool) {
	var slug string
	if err := json.Unmarshal(raw, &slug); err == nil {
		for _, im := range s.images {
			if im.Slug == slug {
				return im, true
			}
		}
		return godo.Image{}, false
	}
	var id int
	if err := json.Unmarshal(raw, &id); err == nil {
		for _, im := range s.images {
			if im.ID == id {
				return im, true
			}
		}
	}
	return godo.Image{}, false
}

func (s *Server) knownKey(raw json.RawMessage) bool {
	var id int
	if err := json.Unmarshal(raw, &id); err == nil {
		_, ok := s.keys[id]
		return ok
	}
	var fp string
	if err := json.Unmarshal(raw, &fp); err == nil {
		for _, k := range s.keys {
			if k.Fingerprint == fp {
				return true
			}
		}
	}
	return false
}

func (s *Server) validTags(w http.ResponseWriter, tags []string) bool {
	for _, t := range tags {
		if !tagRE.MatchString(t) {
			unprocessable(w, fmt.Sprintf("tag %q is invalid: only letters, numbers, ':', '-' and '_' are allowed", t))
			return false
		}
	}
	return true
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
