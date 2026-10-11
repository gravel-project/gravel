package cloud

import (
	"slices"
	"testing"
)

func TestNormalizeCIDRs(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"203.0.113.4"}, []string{"203.0.113.4/32"}},
		{[]string{"2001:db8::1"}, []string{"2001:db8::1/128"}},
		{[]string{"10.1.2.3/8"}, []string{"10.0.0.0/8"}},
		{[]string{"192.0.2.1", " 10.0.0.0/8 ", "192.0.2.1/32", "", "10.9.9.9/8"}, []string{"10.0.0.0/8", "192.0.2.1/32"}},
		{[]string{"::ffff:192.0.2.1"}, []string{"192.0.2.1/32"}},
		{nil, nil},
	} {
		got, err := NormalizeCIDRs(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("NormalizeCIDRs(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"0.0.0.0/0", "::/0", "10.0.0.1/0", "not-an-ip", "300.1.1.1", "10.0.0.0/33"} {
		if got, err := NormalizeCIDRs([]string{in}); err == nil {
			t.Errorf("NormalizeCIDRs(%q) = %v, want an error", in, got)
		}
	}
}

func TestSameFirewallSpec(t *testing.T) {
	a := FirewallSpec{Name: "fw", Tags: []string{"b", "a"}, Inbound: []Rule{
		{Protocol: "tcp", Ports: "22", Addresses: []string{"10.0.0.2/32", "10.0.0.1/32"}},
		{Protocol: "udp", Ports: "27015", Addresses: AnyAddress},
	}, Outbound: AllowAllOutbound()}
	b := FirewallSpec{Name: "fw", Tags: []string{"a", "b"}, Inbound: []Rule{
		{Protocol: "udp", Ports: "27015", Addresses: AnyAddress},
		{Protocol: "tcp", Ports: "22", Addresses: []string{"10.0.0.1/32", "10.0.0.2/32"}},
	}, Outbound: AllowAllOutbound()}
	if !SameFirewallSpec(a, b) {
		t.Error("the same rules in another order differ")
	}
	b.Inbound[1].Addresses = []string{"10.0.0.1/32"}
	if SameFirewallSpec(a, b) {
		t.Error("a dropped address is the same")
	}
	b = a
	b.Name = "other"
	if SameFirewallSpec(a, b) {
		t.Error("another name is the same")
	}
}
