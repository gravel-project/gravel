package cloudvm

import (
	"slices"
	"strconv"

	"github.com/gravel-project/gravel/cloud"
)

// firewallSpec admits the game's public ports from everyone, its other ports (RCON) and SSH from
// the hub's egress only (and SSH from the location's ssh_from), and everything outbound (the
// game's updates, its log push).
func firewallSpec(s ServerSpec, egress []string) cloud.FirewallSpec {
	values := s.Profile.Values(s.Settings, s.Name)
	var in []cloud.Rule
	for _, p := range s.Profile.Ports {
		if !p.Open(values) {
			continue
		}
		from := egress
		if p.Public {
			from = cloud.AnyAddress
		}
		in = append(in, cloud.Rule{Protocol: p.Protocol, Ports: strconv.Itoa(p.Port), Addresses: slices.Clone(from)})
	}
	ssh := append(slices.Clone(egress), s.Location.SSHFrom...)
	slices.Sort(ssh)
	in = append(in, cloud.Rule{Protocol: "tcp", Ports: "22", Addresses: slices.Compact(ssh)})
	return cloud.FirewallSpec{Name: FirewallName(s.ID), Tags: []string{Tag(s.ID)}, Inbound: in, Outbound: cloud.AllowAllOutbound()}
}
