package servers

import (
	"bytes"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/games/wardogs"
)

// Spec is a Game spec gravel ships (ADR-0010): what a game is, who its players are keyed by,
// which drivers control it and the publisher's bands. A deployment enables a game by naming it
// in servers.yaml and may tighten a band, never loosen one.
type Spec struct {
	ID               string   `yaml:"id"`
	Name             string   `yaml:"name"`
	IdentityProvider string   `yaml:"identity_provider"`
	Drivers          []string `yaml:"drivers"`
	Bands            []Band   `yaml:"bands"`
}

// Band is the range a configuration key may take (gravel#8). A nil bound is open.
type Band struct {
	Section string `yaml:"section" json:"section"`
	Key     string `yaml:"key" json:"key"`
	Min     *int64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max     *int64 `yaml:"max,omitempty" json:"max,omitempty"`
}

// Band finds the spec's band for a key.
func (s Spec) Band(section, key string) (Band, bool) {
	for _, b := range s.Bands {
		if b.Section == section && b.Key == key {
			return b, true
		}
	}
	return Band{}, false
}

// Tighten is the spec's bands with a manifest's tightenings applied; Validate has checked that
// each one tightens.
func (s Spec) Tighten(over []Band) []Band {
	out := slices.Clone(s.Bands)
	for _, o := range over {
		for i, b := range out {
			if b.Section == o.Section && b.Key == o.Key {
				if o.Min != nil {
					out[i].Min = o.Min
				}
				if o.Max != nil {
					out[i].Max = o.Max
				}
			}
		}
	}
	return out
}

// ParseSpec reads a spec strictly.
func ParseSpec(b []byte) (Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("servers: game spec: %w", err)
	}
	if s.ID == "" || s.Name == "" || s.IdentityProvider == "" || len(s.Drivers) == 0 {
		return Spec{}, fmt.Errorf("servers: game spec %q: id, name, identity_provider and drivers are required", s.ID)
	}
	return s, nil
}

// Specs are the Game specs this hub ships, by id.
func Specs() map[string]Spec {
	out := map[string]Spec{}
	for _, raw := range [][]byte{wardogs.GameSpec} {
		s, err := ParseSpec(raw)
		if err != nil {
			panic(err) // a shipped spec is tested; a broken one is a build error
		}
		out[s.ID] = s
	}
	return out
}
