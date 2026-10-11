package cloud

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// NormalizeCIDRs parses the addresses and prefixes a firewall admits from ("203.0.113.4" becomes
// "203.0.113.4/32"), masks them, and returns them sorted without duplicates. A prefix that admits
// every address (/0) is refused: whoever asks names the addresses; AnyAddress is for the rules
// that mean it.
func NormalizeCIDRs(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var pfx netip.Prefix
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("cloud: %q is not an address or CIDR", s)
			}
			pfx = p.Masked()
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("cloud: %q is not an address or CIDR", s)
			}
			pfx = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if pfx.Bits() == 0 {
			return nil, fmt.Errorf("cloud: %q admits every address; name the addresses to admit", s)
		}
		out = append(out, pfx.String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// SameFirewallSpec says whether two firewall specs admit the same traffic to the same tags under
// the same name, whatever the order of their rules, addresses and tags.
func SameFirewallSpec(a, b FirewallSpec) bool {
	norm := func(s FirewallSpec) string {
		var parts []string
		for _, set := range [][]Rule{s.Inbound, s.Outbound} {
			var rules []string
			for _, r := range set {
				addrs := slices.Clone(r.Addresses)
				slices.Sort(addrs)
				rules = append(rules, r.Protocol+"/"+r.Ports+"/"+strings.Join(addrs, ","))
			}
			slices.Sort(rules)
			parts = append(parts, strings.Join(rules, ";"))
		}
		tags := slices.Clone(s.Tags)
		slices.Sort(tags)
		return s.Name + "|" + strings.Join(tags, ",") + "|" + strings.Join(parts, "|")
	}
	return norm(a) == norm(b)
}
