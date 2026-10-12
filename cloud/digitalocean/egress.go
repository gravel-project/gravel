package digitalocean

import _ "embed"

//go:embed reserved-egress.sh
var reservedEgressScript string

// ReservedEgressScript makes an instance's outbound traffic leave from its reserved IP: on
// DigitalOcean a reserved IP reaches the instance on its anchor address and carries outbound
// traffic only through the anchor gateway, so without it a game answers from the instance's own
// address and the player who dialled the reserved IP never hears back.
func (p *Provider) ReservedEgressScript() string { return reservedEgressScript }
