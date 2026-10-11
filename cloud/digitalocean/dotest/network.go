package dotest

import (
	"crypto/md5" //nolint:gosec // DigitalOcean's key fingerprint is MD5; nothing here relies on it for security
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/digitalocean/godo"
)

var (
	firewallNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,254}$`)
	portsRE        = regexp.MustCompile(`^(0|all|[0-9]{1,5}(-[0-9]{1,5})?)$`)
)

// --- firewalls ---

func (s *Server) listFirewalls(w http.ResponseWriter, r *http.Request) {
	out := make([]godo.Firewall, 0, len(s.firewalls))
	for _, fw := range s.firewalls {
		out = append(out, *fw)
	}
	slices.SortFunc(out, func(a, b godo.Firewall) int { return strings.Compare(a.Created+a.ID, b.Created+b.ID) })
	s.writeList(w, r, "firewalls", out)
}

func (s *Server) getFirewall(w http.ResponseWriter, r *http.Request) {
	fw, ok := s.firewalls[r.PathValue("id")]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"firewall": fw})
}

func (s *Server) createFirewall(w http.ResponseWriter, r *http.Request) {
	var req godo.FirewallRequest
	if !decode(w, r, &req) || !s.validFirewall(w, &req) {
		return
	}
	fw := &godo.Firewall{ID: uuid(), Status: "succeeded", Created: now(), PendingChanges: []godo.PendingChange{}}
	s.applyFirewall(fw, &req)
	s.firewalls[fw.ID] = fw
	writeJSON(w, http.StatusAccepted, map[string]any{"firewall": fw})
}

func (s *Server) updateFirewall(w http.ResponseWriter, r *http.Request) {
	fw, ok := s.firewalls[r.PathValue("id")]
	if !ok {
		notFound(w)
		return
	}
	var req godo.FirewallRequest
	if !decode(w, r, &req) || !s.validFirewall(w, &req) {
		return
	}
	s.applyFirewall(fw, &req)
	writeJSON(w, http.StatusOK, map[string]any{"firewall": fw})
}

func (s *Server) deleteFirewall(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.firewalls[r.PathValue("id")]; !ok {
		notFound(w)
		return
	}
	delete(s.firewalls, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// validFirewall checks a firewall the way the API does for what the provider sends: a name,
// known tags (a firewall does not create its tags), and rules with a protocol, ports for tcp and
// udp, and somewhere to come from or go to.
func (s *Server) validFirewall(w http.ResponseWriter, req *godo.FirewallRequest) bool {
	if !firewallNameRE.MatchString(req.Name) {
		unprocessable(w, "name is invalid")
		return false
	}
	for _, t := range req.Tags {
		if !s.tags[t] {
			unprocessable(w, fmt.Sprintf("tag %q does not exist", t))
			return false
		}
	}
	for _, id := range req.DropletIDs {
		if _, ok := s.droplets[id]; !ok {
			unprocessable(w, fmt.Sprintf("droplet %d does not exist", id))
			return false
		}
	}
	check := func(protocol, ports string, addrs, tags []string) bool {
		switch protocol {
		case "tcp", "udp":
			if !portsRE.MatchString(ports) {
				unprocessable(w, fmt.Sprintf("ports %q are invalid for %s", ports, protocol))
				return false
			}
		case "icmp":
		default:
			unprocessable(w, fmt.Sprintf("protocol %q is invalid", protocol))
			return false
		}
		if len(addrs) == 0 && len(tags) == 0 {
			unprocessable(w, "a rule needs addresses or tags")
			return false
		}
		return true
	}
	for _, in := range req.InboundRules {
		if in.Sources == nil || !check(in.Protocol, in.PortRange, in.Sources.Addresses, in.Sources.Tags) {
			if in.Sources == nil {
				unprocessable(w, "an inbound rule needs sources")
			}
			return false
		}
	}
	for _, out := range req.OutboundRules {
		if out.Destinations == nil || !check(out.Protocol, out.PortRange, out.Destinations.Addresses, out.Destinations.Tags) {
			if out.Destinations == nil {
				unprocessable(w, "an outbound rule needs destinations")
			}
			return false
		}
	}
	return true
}

// applyFirewall replaces a firewall's name, rules and targets, as the API's PUT does. The API
// reports icmp's ports, and "every port", as "0".
func (s *Server) applyFirewall(fw *godo.Firewall, req *godo.FirewallRequest) {
	ports := func(protocol, p string) string {
		if protocol == "icmp" || p == "all" {
			return "0"
		}
		return p
	}
	fw.Name = req.Name
	fw.InboundRules = []godo.InboundRule{}
	for _, in := range req.InboundRules {
		in.PortRange = ports(in.Protocol, in.PortRange)
		fw.InboundRules = append(fw.InboundRules, in)
	}
	fw.OutboundRules = []godo.OutboundRule{}
	for _, out := range req.OutboundRules {
		out.PortRange = ports(out.Protocol, out.PortRange)
		fw.OutboundRules = append(fw.OutboundRules, out)
	}
	fw.Tags = nonNil(slices.Clone(req.Tags))
	fw.DropletIDs = nonNil(slices.Clone(req.DropletIDs))
}

// --- reserved IPs ---

func (s *Server) reservedJSON(ip *reservedIP) godo.ReservedIP {
	region, _ := s.region(ip.region)
	out := godo.ReservedIP{Region: &region, IP: ip.ip, ProjectID: ip.project}
	if d, ok := s.droplets[ip.dropletID]; ok && ip.dropletID != 0 {
		dd := d.Droplet
		out.Droplet = &dd
	}
	return out
}

func (s *Server) listReservedIPs(w http.ResponseWriter, r *http.Request) {
	ips := make([]string, 0, len(s.reserved))
	for ip := range s.reserved {
		ips = append(ips, ip)
	}
	slices.Sort(ips)
	out := make([]godo.ReservedIP, 0, len(ips))
	for _, ip := range ips {
		out = append(out, s.reservedJSON(s.reserved[ip]))
	}
	s.writeList(w, r, "reserved_ips", out)
}

func (s *Server) getReservedIP(w http.ResponseWriter, r *http.Request) {
	ip, ok := s.reserved[r.PathValue("ip")]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reserved_ip": s.reservedJSON(ip)})
}

func (s *Server) createReservedIP(w http.ResponseWriter, r *http.Request) {
	var req godo.ReservedIPCreateRequest
	if !decode(w, r, &req) {
		return
	}
	ip := &reservedIP{region: req.Region, project: DefaultProjectID}
	if req.DropletID != 0 {
		d, ok := s.droplets[req.DropletID]
		if !ok {
			unprocessable(w, "droplet_id is invalid")
			return
		}
		ip.region, ip.dropletID = d.Region.Slug, d.ID
	} else if _, ok := s.region(req.Region); !ok {
		unprocessable(w, "region is invalid")
		return
	}
	if req.ProjectID != "" {
		if !s.hasProject(req.ProjectID) {
			unprocessable(w, "project_id is invalid")
			return
		}
		ip.project = req.ProjectID
	}
	s.ipSeq++
	ip.ip = fmt.Sprintf("198.51.100.%d", s.ipSeq)
	s.reserved[ip.ip] = ip
	if req.ProjectID != "" {
		s.assigned[req.ProjectID] = append(s.assigned[req.ProjectID], "do:reservedip:"+ip.ip)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"reserved_ip": s.reservedJSON(ip), "links": godo.Links{}})
}

func (s *Server) deleteReservedIP(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.reserved[r.PathValue("ip")]; !ok {
		notFound(w)
		return
	}
	delete(s.reserved, r.PathValue("ip"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reservedIPAction(w http.ResponseWriter, r *http.Request) {
	ip, ok := s.reserved[r.PathValue("ip")]
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
	var apply func()
	dropletID := 0
	switch req.Type {
	case "assign":
		d, ok := s.droplets[req.DropletID]
		switch {
		case !ok || !d.deleteAt.IsZero():
			unprocessable(w, "droplet_id is invalid")
			return
		case d.Region.Slug != ip.region:
			unprocessable(w, "the reserved IP and the Droplet must be in the same region")
			return
		}
		dropletID = d.ID
		apply = func() { ip.dropletID = d.ID }
	case "unassign":
		if ip.dropletID == 0 {
			unprocessable(w, "the reserved IP is not assigned")
			return
		}
		apply = func() { ip.dropletID = 0 }
	default:
		unprocessable(w, fmt.Sprintf("Action type %q is not supported by this emulator", req.Type))
		return
	}
	a := s.newAction(req.Type+"_ip", "reserved_ip", 0, dropletID, ip.region, apply)
	writeJSON(w, http.StatusCreated, map[string]any{"action": a.Action})
}

// --- SSH keys ---

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	ids := make([]int, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]godo.Key, 0, len(ids))
	for _, id := range ids {
		out = append(out, *s.keys[id])
	}
	s.writeList(w, r, "ssh_keys", out)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req godo.KeyCreateRequest
	if !decode(w, r, &req) {
		return
	}
	fp := fingerprint(req.PublicKey)
	if req.Name == "" || fp == "" {
		unprocessable(w, "Key invalid, key should be of the format `type key [comment]`")
		return
	}
	for _, k := range s.keys {
		if k.Fingerprint == fp {
			unprocessable(w, "SSH Key is already in use on your account")
			return
		}
	}
	k := &godo.Key{ID: s.id(), Name: req.Name, PublicKey: req.PublicKey, Fingerprint: fp}
	s.keys[k.ID] = k
	writeJSON(w, http.StatusCreated, map[string]any{"ssh_key": k})
}

// fingerprint is the key's MD5 fingerprint as DigitalOcean shows it, or "" for something that is
// not an OpenSSH public key.
func fingerprint(pub string) string {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) == 0 {
		return ""
	}
	sum := md5.Sum(blob) //nolint:gosec // see the import
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":")
}

// --- tags ---

func (s *Server) createTag(w http.ResponseWriter, r *http.Request) {
	var req godo.TagCreateRequest
	if !decode(w, r, &req) || !s.validTags(w, []string{req.Name}) {
		return
	}
	if s.tags[req.Name] {
		// The API's answer to an existing tag is not documented; the emulator takes the strict
		// reading, so a client has to cope with a refusal.
		unprocessable(w, "tag already exists")
		return
	}
	s.tags[req.Name] = true
	writeJSON(w, http.StatusCreated, map[string]any{"tag": godo.Tag{Name: req.Name, Resources: &godo.TaggedResources{}}})
}

func (s *Server) getTag(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.tags[name] {
		notFound(w)
		return
	}
	n := 0
	for _, d := range s.droplets {
		if slices.Contains(d.Tags, name) {
			n++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tag": godo.Tag{Name: name, Resources: &godo.TaggedResources{
		Count: n, Droplets: &godo.TaggedDropletsResources{Count: n},
	}}})
}

func (s *Server) tagResources(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.tags[name] {
		notFound(w)
		return
	}
	var req godo.TagResourcesRequest
	if !decode(w, r, &req) {
		return
	}
	for _, res := range req.Resources {
		switch res.Type {
		case godo.DropletResourceType:
			id, _ := strconv.Atoi(res.ID)
			d, ok := s.droplets[id]
			if !ok {
				notFound(w)
				return
			}
			if !slices.Contains(d.Tags, name) {
				d.Tags = append(d.Tags, name)
			}
		case godo.VolumeResourceType:
			v, ok := s.volumes[res.ID]
			if !ok {
				notFound(w)
				return
			}
			if !slices.Contains(v.Tags, name) {
				v.Tags = append(v.Tags, name)
			}
		default:
			unprocessable(w, fmt.Sprintf("resource_type %q is not supported by this emulator", res.Type))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- projects ---

func (s *Server) hasProject(id string) bool {
	return slices.ContainsFunc(s.projects, func(p godo.Project) bool { return p.ID == id })
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	s.writeList(w, r, "projects", s.projects)
}

// assignResources moves resources into a project; each URN must name a resource that exists.
func (s *Server) assignResources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.hasProject(id) {
		notFound(w)
		return
	}
	var req struct {
		Resources []string `json:"resources"`
	}
	if !decode(w, r, &req) {
		return
	}
	out := make([]godo.ProjectResource, 0, len(req.Resources))
	for _, urn := range req.Resources {
		if !s.urnExists(urn) {
			unprocessable(w, fmt.Sprintf("resource %q does not exist", urn))
			return
		}
		s.assigned[id] = append(s.assigned[id], urn)
		out = append(out, godo.ProjectResource{URN: urn, AssignedAt: now(), Status: "ok",
			Links: &godo.ProjectResourceLinks{Self: s.srv.URL + "/v2/" + strings.ReplaceAll(strings.TrimPrefix(urn, "do:"), ":", "s/")}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": out})
}

func (s *Server) urnExists(urn string) bool {
	parts := strings.SplitN(urn, ":", 3)
	if len(parts) != 3 || parts[0] != "do" {
		return false
	}
	switch parts[1] {
	case "droplet":
		id, _ := strconv.Atoi(parts[2])
		_, ok := s.droplets[id]
		return ok
	case "volume":
		_, ok := s.volumes[parts[2]]
		return ok
	case "reservedip", "floatingip":
		_, ok := s.reserved[parts[2]]
		return ok
	}
	return false
}
