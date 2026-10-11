package digitalocean_test

import (
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/cloudtest"
	"github.com/gravel-project/gravel/cloud/digitalocean"
	"github.com/gravel-project/gravel/cloud/digitalocean/dotest"
)

// projectID is the emulator's project for the provider's Project setting, by name.
const projectID = "6f1d2c3b-4a59-4e8d-9c7b-0a1b2c3d4e5f"

// resizeCases is DigitalOcean's CanResize matrix over the emulator's catalog, which is a subset
// of the real one: up a family is fine, down is refused once the disk would shrink, and plan
// families never mix.
var resizeCases = []cloudtest.ResizeCase{
	{From: "s-1vcpu-1gb", To: "s-2vcpu-4gb", OK: true},
	{From: "s-2vcpu-4gb", To: "s-1vcpu-1gb", OK: false}, // disk 80 → 25
	{From: "s-1vcpu-1gb", To: "c-2", OK: false},         // family
	{From: "c-2", To: "c-4", OK: true},
	{From: "c-4", To: "c-2", OK: false},                       // disk 50 → 25
	{From: "s-1vcpu-512mb-10gb", To: "s-1vcpu-1gb", OK: true}, // the live run's resize
	{From: "s-1vcpu-1gb", To: "s-1vcpu-1gb", OK: true},        // a no-op
	{From: "c-2", To: "s-2vcpu-4gb", OK: false},               // family, though the disk fits
	{From: "s-1vcpu-1gb", To: "s5-1vcpu-1gb-30gb", OK: false}, // a newer line is its own family
}

func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []dotest.Option
	}{
		// Actions done at the next request.
		{"instant", nil},
		// Actions that take a moment, so Wait polls and every read before a completion sees the
		// Droplet mid-change; small pages, so every list pages.
		{"slow", []dotest.Option{dotest.WithActionDelay(3 * time.Millisecond), dotest.WithMaxPerPage(2)}},
		// A create that answers without an action link, so Wait watches the Droplet.
		{"no-action-links", []dotest.Option{dotest.WithoutActionLinks()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := dotest.New(t, append([]dotest.Option{dotest.WithProject(projectID, "hidden-token-gaming")}, tc.opts...)...)
			// Registered before Run, so it runs after the suite's own cleanup: the suite must
			// leave nothing behind but its SSH key, which the interface cannot delete.
			t.Cleanup(func() {
				if got, want := srv.Counts(), (dotest.Counts{SSHKeys: 1}); got != want {
					t.Errorf("after the suite the emulator holds %+v, want %+v", got, want)
				}
			})
			cloudtest.Run(t, func(t *testing.T) cloud.Provider {
				p, err := digitalocean.New(digitalocean.Config{
					BaseURL: srv.URL(), Token: dotest.Token, PollInterval: time.Millisecond,
					RetryWait: time.Millisecond, Project: "hidden-token-gaming",
				})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}, cloudtest.Options{
				Region:      "sfo3",
				Size:        "s-1vcpu-1gb",
				ResizeTo:    "s-2vcpu-4gb",
				Image:       "fedora-44-x64",
				ResizeCases: resizeCases,
				Prefix:      "gravel-ct",
				Tag:         "gravel-ct",
				Poll:        time.Millisecond,
			})
		})
	}
}
