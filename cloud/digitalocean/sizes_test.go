package digitalocean

import (
	"errors"
	"testing"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

func TestSizeFamily(t *testing.T) {
	for slug, want := range map[string]string{
		"s-1vcpu-1gb":        "basic",
		"s-2vcpu-4gb-amd":    "basic",
		"s-4vcpu-8gb-intel":  "basic",
		"c-2":                "cpu-optimized",
		"c2-4vcpu-8gb":       "cpu-optimized",
		"g-2vcpu-8gb":        "general-purpose",
		"gd-2vcpu-8gb":       "general-purpose",
		"m-2vcpu-16gb":       "memory-optimized",
		"m3-2vcpu-16gb":      "memory-optimized",
		"so-2vcpu-16gb":      "storage-optimized",
		"so1_5-2vcpu-16gb":   "storage-optimized",
		"s5-1vcpu-1gb-30gb":  "s5",
		"c5-2vcpu-4gb":       "c5",
		"gpu-h100x1-80gb":    "gpu",
		"something-else-new": "something",
	} {
		if got := sizeFamily(slug); got != want {
			t.Errorf("sizeFamily(%s) = %s, want %s", slug, got, want)
		}
	}
}

func TestToSize(t *testing.T) {
	for _, tc := range []struct {
		slug      string
		dedicated bool
	}{
		{"s-1vcpu-1gb", false}, {"s5-1vcpu-1gb-30gb", false}, {"c-2", true}, {"c2-2vcpu-4gb", true},
		{"g-2vcpu-8gb", true}, {"gd-2vcpu-8gb", true}, {"m-2vcpu-16gb", true}, {"so1_5-2vcpu-16gb", true},
		{"c5-2vcpu-4gb", true}, {"g5-2vcpu-8gb", true},
	} {
		s := toSize(godo.Size{Slug: tc.slug, Disk: 25, Regions: []string{"sfo3"}})
		if s.DedicatedCPU != tc.dedicated || !s.BillsWhenStopped || s.Family == "" {
			t.Errorf("toSize(%s) = %+v, want dedicated %v, billing when stopped, a family", tc.slug, s, tc.dedicated)
		}
	}
}

func TestCanResizeUsesTheCurrentDisk(t *testing.T) {
	p := &Provider{}
	small := cloud.Size{Slug: "s-1vcpu-1gb", DiskGiB: 25, Family: "basic"}
	big := cloud.Size{Slug: "s-2vcpu-4gb", DiskGiB: 80, Family: "basic"}
	if err := p.CanResize(big, small); !errors.Is(err, cloud.ErrResizeNotAllowed) {
		t.Errorf("down a disk: %v", err)
	}
	// A Droplet created small and resized up keeps its 25 GiB disk, so it may come back down.
	grown := big
	grown.DiskGiB = 25
	if err := p.CanResize(grown, small); err != nil {
		t.Errorf("down after a CPU-and-memory resize: %v", err)
	}
	// Families are derived when the caller left them empty.
	if err := p.CanResize(cloud.Size{Slug: "s-1vcpu-1gb", DiskGiB: 25}, cloud.Size{Slug: "c-2", DiskGiB: 25}); !errors.Is(err, cloud.ErrResizeNotAllowed) {
		t.Errorf("across families without Family set: %v", err)
	}
}
