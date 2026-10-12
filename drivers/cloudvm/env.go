package cloudvm

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// IngestPath is where a cloud-VM game pushes its log on the hub, the feed token appended: a
// game that sends no Authorization header (CS2's logaddress_add_http) carries it in the path.
const IngestPath = "/api/ingest/log/"

// RenderEnv is the environment file a server's game reads: the profile's fixed lines, each
// setting's value, and the secrets. podman takes an env file's values literally (no quotes, no
// escapes), so a value with a line break is refused rather than written. The file holds secrets:
// it is pushed over SSH, written 0600, and only its hash is kept.
//
// size is the host's size: with a scaling policy, the rung it is on sets the game's players
// setting (Profile.PlayersSetting), so a host resized to a bigger rung takes more players.
func RenderEnv(s ServerSpec, size string) ([]byte, error) {
	env := maps.Clone(s.Profile.Env)
	if env == nil {
		env = map[string]string{}
	}
	settings := s.Settings
	if sc, key := s.Scaling, s.Profile.PlayersSetting; sc != nil && key != "" {
		if i := Rung(sc.Ladder, size); i >= 0 {
			settings = maps.Clone(settings)
			if settings == nil {
				settings = map[string]string{}
			}
			settings[key] = strconv.Itoa(sc.Ladder[i].Players)
		}
	}
	values := s.Profile.Values(settings, s.Name)
	for _, st := range s.Profile.Settings {
		v, ok := values[st.Key]
		if !ok {
			continue
		}
		if st.Type == TypeBool {
			v = map[bool]string{true: "1", false: "0"}[v == "true"]
		}
		env[st.Env] = v
	}
	sec := s.Profile.Secrets
	env[sec.RCONPassword] = s.RCONPassword
	for name, v := range map[string]string{sec.JoinPassword: s.JoinPassword, sec.PublisherToken: s.PublisherToken, sec.FeedURL: s.FeedURL} {
		if name != "" && v != "" {
			env[name] = v
		}
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		v := env[k]
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Errorf("cloudvm: server %s: %s has a line break; an env file value is one line", s.ID, k)
		}
		b.WriteString(k + "=" + v + "\n")
	}
	return []byte(b.String()), nil
}

// envHash is what the hub keeps of a pushed env file.
func envHash(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// FeedURL is the URL a server pushes its log to: the feed's origin, the path, the token.
func FeedURL(origin, token string) string {
	return strings.TrimRight(origin, "/") + IngestPath + token
}
