//go:build live

package digitalocean_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/cloudtest"
	"github.com/gravel-project/gravel/cloud/digitalocean"
)

// TestLiveConformance runs the conformance suite against DigitalOcean itself. It creates one
// Droplet of the smallest size, a 1 GiB volume, a reserved IP and a firewall, and costs a few
// cents; everything is named gravel-ct-<unix time> and tagged gravel-ct, and deleted at the end
// even after a failure:
//
//	GRAVEL_DO_TOKEN_FILE=~/.config/gravel/do-token GRAVEL_DO_PROJECT=my-project \
//	  go test -tags live -run Live -v -timeout 30m ./cloud/digitalocean/
//
// GRAVEL_DO_REGION picks the region (sfo3 by default). The token needs read and write on
// Droplets, volumes, reserved IPs, firewalls, SSH keys, tags and projects.
func TestLiveConformance(t *testing.T) {
	file := os.Getenv("GRAVEL_DO_TOKEN_FILE")
	if file == "" {
		t.Skip("GRAVEL_DO_TOKEN_FILE is not set")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	region := os.Getenv("GRAVEL_DO_REGION")
	if region == "" {
		region = "sfo3"
	}
	cloudtest.Run(t, func(t *testing.T) cloud.Provider {
		p, err := digitalocean.New(digitalocean.Config{
			Token: token, Project: os.Getenv("GRAVEL_DO_PROJECT"), UserAgent: "gravel-cloudtest",
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}, cloudtest.Options{
		Region:   region,
		Size:     "s-1vcpu-512mb-10gb",
		ResizeTo: "s-1vcpu-1gb",
		Image:    "fedora-44-x64",
		ResizeCases: []cloudtest.ResizeCase{
			{From: "s-1vcpu-512mb-10gb", To: "s-1vcpu-1gb", OK: true},
			{From: "s-1vcpu-1gb", To: "s-1vcpu-512mb-10gb", OK: false}, // disk 25 → 10
			{From: "s-1vcpu-1gb", To: "s-2vcpu-4gb", OK: true},
			{From: "s-2vcpu-4gb", To: "s-1vcpu-1gb", OK: false}, // disk 80 → 25
			{From: "s-1vcpu-1gb", To: "c-2", OK: false},         // family
			{From: "c-2", To: "c-4", OK: true},
			{From: "c-4", To: "c-2", OK: false}, // disk 50 → 25
		},
		Prefix:                fmt.Sprintf("gravel-ct-%d", time.Now().Unix()),
		Tag:                   "gravel-ct",
		SkipDuplicateInstance: true,
	})
}
