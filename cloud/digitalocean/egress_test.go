package digitalocean

import (
	"os/exec"
	"strings"
	"testing"
)

// TestReservedEgressScript checks the script parses and reads what it routes by from the
// metadata service: the assignment, the anchor address and gateway, the public address and gateway.
func TestReservedEgressScript(t *testing.T) {
	s := (&Provider{}).ReservedEgressScript()
	for _, want := range []string{
		"reserved_ip/ipv4/active", "anchor_ipv4/address", "anchor_ipv4/gateway", "ipv4/address", "ipv4/gateway",
		"lookup 201 priority 999", `src "$anchor"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		cmd := exec.CommandContext(t.Context(), sh, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	}
}
