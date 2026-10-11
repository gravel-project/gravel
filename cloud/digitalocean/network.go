package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// Addresses lists the reserved IPs matching the filter. DigitalOcean's reserved IPs take no tags
// and no name, so a Tag matches the tags of the Droplet the address is assigned to (an
// unassigned address matches no tag), and a Name matches the address itself.
func (p *Provider) Addresses(ctx context.Context, f cloud.Filter) ([]cloud.Address, error) {
	ips, err := listAll(ctx, "list reserved ips", p.c.ReservedIPs.List)
	if err != nil {
		return nil, err
	}
	var out []cloud.Address
	for _, ip := range ips {
		if f.Name != "" && ip.IP != f.Name {
			continue
		}
		if f.Tag != "" && (ip.Droplet == nil || !hasTag(ip.Droplet.Tags, f.Tag)) {
			continue
		}
		out = append(out, toAddress(&ip))
	}
	return out, nil
}

// AllocateAddress reserves an IPv4 address in a region, in the configured project. The tags are
// not stored: DigitalOcean's reserved IPs have none (see Addresses).
func (p *Provider) AllocateAddress(ctx context.Context, region string, _ []string) (cloud.Address, error) {
	project, err := p.projectID(ctx)
	if err != nil {
		return cloud.Address{}, err
	}
	ip, _, err := p.c.ReservedIPs.Create(ctx, &godo.ReservedIPCreateRequest{Region: region, ProjectID: project})
	if err != nil {
		return cloud.Address{}, apiErr("reserve an ip in "+region, err)
	}
	return toAddress(ip), nil
}

// AssignAddress points a reserved IP at a Droplet in its region; one already pointing there
// needs no operation.
func (p *Provider) AssignAddress(ctx context.Context, ip, instanceID string) (cloud.Operation, error) {
	n, err := dropletID(instanceID)
	if err != nil {
		return cloud.Operation{}, err
	}
	cur, _, err := p.c.ReservedIPs.Get(ctx, ip)
	if err != nil {
		return cloud.Operation{}, apiErr("get reserved ip "+ip, err)
	}
	if cur.Droplet != nil && cur.Droplet.ID == n {
		return cloud.Operation{Kind: "assign", Resource: ip}, nil
	}
	a, _, err := p.c.ReservedIPActions.Assign(ctx, ip, n)
	if err != nil {
		return cloud.Operation{}, apiErr("assign reserved ip "+ip+" to droplet "+instanceID, err)
	}
	return actionOp("assign", ip, a), nil
}

// ReleaseAddress gives a reserved IP back.
func (p *Provider) ReleaseAddress(ctx context.Context, ip string) error {
	_, err := p.c.ReservedIPs.Delete(ctx, ip)
	return apiErr("release reserved ip "+ip, err)
}

func toAddress(ip *godo.ReservedIP) cloud.Address {
	a := cloud.Address{IP: ip.IP}
	if ip.Region != nil {
		a.Region = ip.Region.Slug
	}
	if ip.Droplet != nil && ip.Droplet.ID != 0 {
		a.InstanceID = strconv.Itoa(ip.Droplet.ID)
		if a.Region == "" && ip.Droplet.Region != nil {
			a.Region = ip.Droplet.Region.Slug
		}
	}
	return a
}

// Firewalls lists the cloud firewalls matching the filter; a Tag matches the tags a firewall
// applies to.
func (p *Provider) Firewalls(ctx context.Context, f cloud.Filter) ([]cloud.Firewall, error) {
	fws, err := listAll(ctx, "list firewalls", p.c.Firewalls.List)
	if err != nil {
		return nil, err
	}
	var out []cloud.Firewall
	for _, fw := range fws {
		if (f.Tag != "" && !hasTag(fw.Tags, f.Tag)) || (f.Name != "" && fw.Name != f.Name) {
			continue
		}
		out = append(out, toFirewall(&fw))
	}
	return out, nil
}

// EnsureFirewall creates the firewall or replaces an existing one's rules and tags with the
// spec's (DigitalOcean's update replaces both wholesale). A firewall that already matches the
// spec is left alone, so a second call sends no update. The spec's tags are created first: a
// firewall cannot name a tag that does not exist yet.
func (p *Provider) EnsureFirewall(ctx context.Context, spec cloud.FirewallSpec) (cloud.Firewall, error) {
	if spec.Name == "" {
		return cloud.Firewall{}, fmt.Errorf("digitalocean: ensure firewall: %w: a name is required", cloud.ErrInvalid)
	}
	existing, err := p.Firewalls(ctx, cloud.Filter{Name: spec.Name})
	if err != nil {
		return cloud.Firewall{}, err
	}
	if len(existing) > 0 && specKey(existing[0].FirewallSpec) == specKey(spec) {
		return existing[0], nil
	}
	if err := p.ensureTags(ctx, spec.Tags); err != nil {
		return cloud.Firewall{}, err
	}
	req := firewallRequest(spec)
	var fw *godo.Firewall
	if len(existing) == 0 {
		fw, _, err = p.c.Firewalls.Create(ctx, req)
		err = apiErr("create firewall "+spec.Name, err)
	} else {
		fw, _, err = p.c.Firewalls.Update(ctx, existing[0].ID, req)
		err = apiErr("update firewall "+spec.Name, err)
	}
	if err != nil {
		return cloud.Firewall{}, err
	}
	return toFirewall(fw), nil
}

// DeleteFirewall removes a firewall.
func (p *Provider) DeleteFirewall(ctx context.Context, id string) error {
	_, err := p.c.Firewalls.Delete(ctx, id)
	return apiErr("delete firewall "+id, err)
}

// allPorts is how the API spells "every port" for tcp and udp, and what it reports for icmp.
const allPorts = "0"

func firewallRequest(spec cloud.FirewallSpec) *godo.FirewallRequest {
	req := &godo.FirewallRequest{Name: spec.Name, Tags: spec.Tags}
	for _, r := range spec.Inbound {
		req.InboundRules = append(req.InboundRules, godo.InboundRule{
			Protocol: r.Protocol, PortRange: apiPorts(r), Sources: &godo.Sources{Addresses: r.Addresses},
		})
	}
	for _, r := range spec.Outbound {
		req.OutboundRules = append(req.OutboundRules, godo.OutboundRule{
			Protocol: r.Protocol, PortRange: apiPorts(r), Destinations: &godo.Destinations{Addresses: r.Addresses},
		})
	}
	return req
}

func apiPorts(r cloud.Rule) string {
	switch {
	case r.Protocol == "icmp":
		return ""
	case r.Ports == "":
		return allPorts
	}
	return r.Ports
}

// domainPorts reads the API's ports back: its spellings of "every port" are the empty string.
func domainPorts(protocol, ports string) string {
	if protocol == "icmp" {
		return ""
	}
	switch ports {
	case allPorts, "all", "1-65535":
		return ""
	}
	return ports
}

func toFirewall(fw *godo.Firewall) cloud.Firewall {
	out := cloud.Firewall{ID: fw.ID, FirewallSpec: cloud.FirewallSpec{Name: fw.Name, Tags: append([]string(nil), fw.Tags...)}}
	for _, r := range fw.InboundRules {
		rule := cloud.Rule{Protocol: r.Protocol, Ports: domainPorts(r.Protocol, r.PortRange)}
		if r.Sources != nil {
			rule.Addresses = append([]string(nil), r.Sources.Addresses...)
		}
		out.Inbound = append(out.Inbound, rule)
	}
	for _, r := range fw.OutboundRules {
		rule := cloud.Rule{Protocol: r.Protocol, Ports: domainPorts(r.Protocol, r.PortRange)}
		if r.Destinations != nil {
			rule.Addresses = append([]string(nil), r.Destinations.Addresses...)
		}
		out.Outbound = append(out.Outbound, rule)
	}
	return out
}

// specKey is a firewall spec in a canonical form, so two specs that differ only in the order of
// their rules, addresses or tags compare equal.
func specKey(s cloud.FirewallSpec) string {
	rules := func(rs []cloud.Rule) string {
		keys := make([]string, 0, len(rs))
		for _, r := range rs {
			addrs := slices.Clone(r.Addresses)
			slices.Sort(addrs)
			ports := r.Ports
			if r.Protocol == "icmp" {
				ports = ""
			}
			keys = append(keys, r.Protocol+"/"+domainPorts(r.Protocol, ports)+"/"+strings.Join(addrs, ","))
		}
		slices.Sort(keys)
		return strings.Join(keys, ";")
	}
	tags := slices.Clone(s.Tags)
	slices.Sort(tags)
	return s.Name + "|" + strings.Join(tags, ",") + "|" + rules(s.Inbound) + "|" + rules(s.Outbound)
}

// EnsureSSHKey returns the id of the account's key with this public key, registering it under
// name first when the account does not have it. Keys are compared by type and key material: the
// comment after them is a label, not part of the key.
func (p *Provider) EnsureSSHKey(ctx context.Context, name, publicKey string) (string, error) {
	want := normalizeKey(publicKey)
	if want == "" {
		return "", fmt.Errorf("digitalocean: ensure ssh key %s: %w: not an OpenSSH public key", name, cloud.ErrInvalid)
	}
	find := func() (string, error) {
		keys, err := listAll(ctx, "list ssh keys", p.c.Keys.List)
		if err != nil {
			return "", err
		}
		for _, k := range keys {
			if normalizeKey(k.PublicKey) == want {
				return strconv.Itoa(k.ID), nil
			}
		}
		return "", nil
	}
	if id, err := find(); err != nil || id != "" {
		return id, err
	}
	k, _, err := p.c.Keys.Create(ctx, &godo.KeyCreateRequest{Name: name, PublicKey: publicKey})
	if err == nil {
		return strconv.Itoa(k.ID), nil
	}
	err = apiErr("create ssh key "+name, err)
	if errors.Is(err, cloud.ErrInvalid) { // registered by somebody else since the list
		if id, ferr := find(); ferr == nil && id != "" {
			return id, nil
		}
	}
	return "", err
}

// normalizeKey is "type base64" from an authorized_keys line, without options or comment.
func normalizeKey(s string) string {
	f := strings.Fields(s)
	if len(f) < 2 {
		return ""
	}
	return f[0] + " " + f[1]
}
