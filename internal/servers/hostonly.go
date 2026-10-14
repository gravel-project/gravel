package servers

import (
	"fmt"
	"reflect"
)

// HostOnly lists what want changes, against current, that only the hub's host operator may
// change: every file the hub reads a secret from, and everything that decides where such a
// secret goes (#195, ADR-0031). Over the API a manifest may change everything else (names, games
// and their bands, poll intervals, trust, sizes, scaling, settings) and remove servers; the
// command on the host (`gravel-hub servers apply`) changes anything. Without this an admin could
// point a credential file at any file on the host and the endpoint at a machine of their own, and
// the hub would send them the file.
//
// Host-only: every location (its provider, token and SSH key files, image, extra keys and SSH
// sources decide who reaches the VMs and their secrets); a new server (it names its credential
// file); and of an existing server its driver, location, endpoint, credential file, feed, and its
// VM's password and publisher token files and share_password.
func HostOnly(current, want Manifest) []string {
	current, want = current.Normalized(), want.Normalized()
	var out []string
	locs := func(m Manifest) map[string]Location {
		byID := map[string]Location{}
		for _, l := range m.Locations {
			byID[l.ID] = l
		}
		return byID
	}
	have, next := locs(current), locs(want)
	for _, id := range sortedKeys(next) {
		if l, ok := have[id]; !ok {
			out = append(out, fmt.Sprintf("location %s: adding a location (its cloud token and SSH key files)", id))
		} else if !reflect.DeepEqual(l, next[id]) {
			out = append(out, fmt.Sprintf("location %s: changing a location (its cloud account, files, image, keys or SSH sources)", id))
		}
	}
	for _, id := range sortedKeys(have) {
		if _, ok := next[id]; !ok {
			out = append(out, fmt.Sprintf("location %s: removing a location", id))
		}
	}
	servers := map[string]Server{}
	for _, s := range current.Servers {
		servers[s.ID] = s
	}
	for _, w := range want.Servers {
		c, ok := servers[w.ID]
		if !ok {
			out = append(out, fmt.Sprintf("server %s: adding a server (its credential file)", w.ID))
			continue
		}
		field := func(name string, same bool) {
			if !same {
				out = append(out, fmt.Sprintf("server %s: its %s", w.ID, name))
			}
		}
		field("driver", c.Driver == w.Driver)
		field("location", c.Location == w.Location)
		field("endpoint", c.Endpoint == w.Endpoint)
		field("credential_file", c.CredentialFile == w.CredentialFile)
		field("feed", reflect.DeepEqual(c.Feed, w.Feed))
		var cv, wv VMSpec
		if c.VM != nil {
			cv = *c.VM
		}
		if w.VM != nil {
			wv = *w.VM
		}
		field("vm.password_file", cv.PasswordFile == wv.PasswordFile)
		field("vm.publisher_token_file", cv.PublisherTokenFile == wv.PublisherTokenFile)
		field("vm.share_password", cv.SharePassword == wv.SharePassword)
	}
	return out
}
