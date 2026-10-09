package wardogs

import (
	"slices"
	"strings"
)

// Capabilities is GET /v1/capabilities: what this build of the server serves.
type Capabilities struct {
	APIVersion string       `json:"apiVersion"`
	Build      string       `json:"build"`
	Auth       Auth         `json:"auth"`
	Limits     Limits       `json:"limits"`
	Config     ConfigAccess `json:"config"`
	Routes     []string     `json:"routes"`
}

// Auth is how the server wants the token.
type Auth struct {
	Scheme string `json:"scheme"`
	Header string `json:"header"`
}

// Limits are the server's own: request body size and requests a minute per address.
type Limits struct {
	MaxBodyBytes              int `json:"maxBodyBytes"`
	MaxRequestsPerMinutePerIP int `json:"maxRequestsPerMinutePerIp"`
}

// ConfigAccess says whether the configuration document may be written, and where it lives.
type ConfigAccess struct {
	Writable bool   `json:"writable"`
	Document string `json:"document"`
}

// Capability is one thing the client can do against a server, named in the client's terms; the
// server grants it by advertising the route (the table below).
type Capability string

// The capabilities this client knows. GET /v1/capabilities itself is not one: it is how the set
// is learned.
const (
	CapHealth             Capability = "health"
	CapStatus             Capability = "status"
	CapPlayers            Capability = "players"
	CapKick               Capability = "players.kick"
	CapKill               Capability = "players.kill"
	CapMessage            Capability = "players.message"
	CapMovePlayer         Capability = "players.move"
	CapBroadcast          Capability = "broadcast"
	CapBans               Capability = "bans.list"
	CapBan                Capability = "bans.add"
	CapUnban              Capability = "bans.remove"
	CapReservedSlots      Capability = "reserved-slots"
	CapRotation           Capability = "rotation"
	CapChangeMap          Capability = "match.map"
	CapEndMatch           Capability = "match.end"
	CapRestartMatch       Capability = "match.restart"
	CapSetLighting        Capability = "world.lighting"
	CapConfigRead         Capability = "config.read"
	CapConfigValidate     Capability = "config.validate"
	CapConfigWrite        Capability = "config.write"
	CapAudit              Capability = "audit"
	CapServerID           Capability = "server-id"
	CapSponsor            Capability = "sponsor"
	CapCatalogMaps        Capability = "catalog.maps"
	CapCatalogLightings   Capability = "catalog.lightings"
	CapCatalogExperiences Capability = "catalog.experiences"
	CapMapExperiences     Capability = "catalog.map-experiences"
	CapMapAlternators     Capability = "catalog.map-alternators"
)

// routeFor is the route each capability needs, as CL-509546 spells it; matching is by shape, so
// the parameter names here are documentation.
var routeFor = map[Capability]Route{
	CapHealth:             mustRoute("GET /v1/health"),
	CapStatus:             mustRoute("GET /v1/status"),
	CapPlayers:            mustRoute("GET /v1/players"),
	CapKick:               mustRoute("POST /v1/players/{id}/kick"),
	CapKill:               mustRoute("POST /v1/players/{id}/kill"),
	CapMessage:            mustRoute("POST /v1/players/{id}/message"),
	CapMovePlayer:         mustRoute("PATCH /v1/players/{id}"),
	CapBroadcast:          mustRoute("POST /v1/broadcast"),
	CapBans:               mustRoute("GET /v1/bans"),
	CapBan:                mustRoute("POST /v1/bans"),
	CapUnban:              mustRoute("DELETE /v1/bans/{steamId}"),
	CapReservedSlots:      mustRoute("GET /v1/reserved-slots"),
	CapRotation:           mustRoute("GET /v1/rotation"),
	CapChangeMap:          mustRoute("POST /v1/match/map"),
	CapEndMatch:           mustRoute("POST /v1/match/end"),
	CapRestartMatch:       mustRoute("POST /v1/match/restart"),
	CapSetLighting:        mustRoute("PUT /v1/world/lighting"),
	CapConfigRead:         mustRoute("GET /v1/config"),
	CapConfigValidate:     mustRoute("POST /v1/config/validate"),
	CapConfigWrite:        mustRoute("PUT /v1/config"),
	CapAudit:              mustRoute("GET /v1/audit"),
	CapServerID:           mustRoute("GET /v1/server-id"),
	CapSponsor:            mustRoute("GET /v1/sponsor"),
	CapCatalogMaps:        mustRoute("GET /v1/catalog/maps"),
	CapCatalogLightings:   mustRoute("GET /v1/catalog/lightings"),
	CapCatalogExperiences: mustRoute("GET /v1/catalog/experiences"),
	CapMapExperiences:     mustRoute("GET /v1/catalog/maps/{map}/experiences"),
	CapMapAlternators:     mustRoute("GET /v1/catalog/maps/{map}/alternators"),
}

// The two routes the server answers without a token.
var (
	routeCapabilities = mustRoute("GET /v1/capabilities")
	publicShapes      = map[string]bool{routeCapabilities.Shape(): true, routeFor[CapHealth].Shape(): true}
)

// Known are every capability this client knows, sorted.
func Known() []Capability {
	out := make([]Capability, 0, len(routeFor))
	for c := range routeFor {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// RouteOf is the route the server advertises for a capability, with the server's own parameter
// names; ok is false when the server does not advertise it (or the capability is unknown).
func (c Capabilities) RouteOf(cap Capability) (Route, bool) {
	want, known := routeFor[cap]
	if !known {
		return Route{}, false
	}
	if cap == CapConfigWrite && !c.Config.Writable {
		return Route{}, false
	}
	shape := want.Shape()
	for _, s := range c.Routes {
		r, err := ParseRoute(s)
		if err == nil && r.Shape() == shape {
			return r, true
		}
	}
	return Route{}, false
}

// Supports reports whether the server grants a capability. Writing the configuration also needs
// the document to be writable.
func (c Capabilities) Supports(cap Capability) bool {
	_, ok := c.RouteOf(cap)
	return ok
}

// Set is every known capability the server grants, sorted.
func (c Capabilities) Set() []Capability {
	var out []Capability
	for _, cap := range Known() {
		if c.Supports(cap) {
			out = append(out, cap)
		}
	}
	return out
}

// Missing is every known capability the server does not grant, sorted.
func (c Capabilities) Missing() []Capability {
	var out []Capability
	for _, cap := range Known() {
		if !c.Supports(cap) {
			out = append(out, cap)
		}
	}
	return out
}

// Unrecognised are the advertised routes this client has no capability for (a new route in a
// new build, or one it cannot parse), as the server writes them, sorted.
func (c Capabilities) Unrecognised() []string {
	known := map[string]bool{routeCapabilities.Shape(): true}
	for _, r := range routeFor {
		known[r.Shape()] = true
	}
	var out []string
	for _, s := range c.Routes {
		r, err := ParseRoute(s)
		if err != nil || !known[r.Shape()] {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// RouteDiff is how two builds' route lists differ, by shape.
type RouteDiff struct {
	// Added and Removed are routes as the newer and the older build write them.
	Added, Removed []string
	// Renamed are routes whose shape stayed and whose parameter names changed, "old -> new".
	Renamed []string
}

// Empty reports whether the route lists are the same, parameter names included.
func (d RouteDiff) Empty() bool { return len(d.Added)+len(d.Removed)+len(d.Renamed) == 0 }

// DiffRoutes compares an older build's routes with a newer one's.
func DiffRoutes(older, newer Capabilities) RouteDiff {
	index := func(c Capabilities) map[string]string {
		m := map[string]string{}
		for _, s := range c.Routes {
			if r, err := ParseRoute(s); err == nil {
				m[r.Shape()] = r.String()
			} else {
				m[strings.TrimSpace(s)] = strings.TrimSpace(s)
			}
		}
		return m
	}
	o, n := index(older), index(newer)
	var d RouteDiff
	for shape, s := range n {
		switch prev, ok := o[shape]; {
		case !ok:
			d.Added = append(d.Added, s)
		case prev != s:
			d.Renamed = append(d.Renamed, prev+" -> "+s)
		}
	}
	for shape, s := range o {
		if _, ok := n[shape]; !ok {
			d.Removed = append(d.Removed, s)
		}
	}
	slices.Sort(d.Added)
	slices.Sort(d.Removed)
	slices.Sort(d.Renamed)
	return d
}
