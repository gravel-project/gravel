package digitalocean

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// Regions are the account's regions.
func (p *Provider) Regions(ctx context.Context) ([]cloud.Region, error) {
	regions, err := listAll(ctx, "list regions", p.c.Regions.List)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.Region, 0, len(regions))
	for _, r := range regions {
		out = append(out, cloud.Region{Slug: r.Slug, Name: r.Name, Available: r.Available})
	}
	return out, nil
}

// Sizes are the sizes the account can create now.
func (p *Provider) Sizes(ctx context.Context) ([]cloud.Size, error) {
	sizes, err := listAll(ctx, "list sizes", p.c.Sizes.List)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.Size, 0, len(sizes))
	for _, s := range sizes {
		if !s.Available {
			continue
		}
		out = append(out, toSize(s))
	}
	return out, nil
}

func toSize(s godo.Size) cloud.Size {
	fam := sizeFamily(s.Slug)
	return cloud.Size{
		Slug:         s.Slug,
		VCPUs:        s.Vcpus,
		MemoryMiB:    s.Memory,
		DiskGiB:      s.Disk,
		PriceHourly:  s.PriceHourly,
		PriceMonthly: s.PriceMonthly,
		Family:       fam,
		DedicatedCPU: dedicatedFamily(fam),
		// A powered-off Droplet bills in full, whatever its size.
		BillsWhenStopped: true,
		Regions:          append([]string(nil), s.Regions...),
	}
}

// Families. DigitalOcean resizes between compatible plans; which plans are compatible is not
// published as data, so each line of plans is its own family and a resize across lines is
// refused. Lines DigitalOcean presents as variants of one plan (Basic with AMD or Intel premium
// CPUs; CPU-Optimized with one or two times the SSD) count as one family, because the disk rule
// already refuses the moves among them that DigitalOcean refuses.
const (
	familyBasic            = "basic"
	familyCPUOptimized     = "cpu-optimized"
	familyGeneralPurpose   = "general-purpose"
	familyMemoryOptimized  = "memory-optimized"
	familyStorageOptimized = "storage-optimized"
)

// sizeFamily derives a size's family from its slug's first segment: "s-2vcpu-4gb-amd" is
// basic, "c2-4vcpu-8gb" cpu-optimized, "so1_5-2vcpu-16gb" storage-optimized. The newer lines
// ("s5-", "c5-", "g5-") and anything unrecognised are a family of their own named by that
// segment, so they resize only within themselves until somebody verifies more.
func sizeFamily(slug string) string {
	prefix, _, _ := strings.Cut(slug, "-")
	switch prefix {
	case "s":
		return familyBasic
	case "c", "c2":
		return familyCPUOptimized
	case "g", "gd":
		return familyGeneralPurpose
	case "m", "m3":
		return familyMemoryOptimized
	case "so", "so1_5":
		return familyStorageOptimized
	}
	return prefix
}

// dedicatedFamily is a family whose vCPUs are dedicated: every line but the basic ones ("s",
// "s5"). An unrecognised line is assumed shared, the cheaper assumption to be wrong about.
func dedicatedFamily(fam string) bool {
	switch fam {
	case familyCPUOptimized, familyGeneralPurpose, familyMemoryOptimized, familyStorageOptimized,
		"c5", "g5", "m5":
		return true
	}
	return false
}

// CanResize says whether an instance of size from may become size to, CPU and memory only: the
// two must be in one family, and to's disk must hold from's (a Droplet's disk never shrinks).
// Pass the instance's current disk in from.DiskGiB when it differs from its size's.
func (p *Provider) CanResize(from, to cloud.Size) error {
	if from.Slug == to.Slug {
		return nil
	}
	ff, tf := from.Family, to.Family
	if ff == "" {
		ff = sizeFamily(from.Slug)
	}
	if tf == "" {
		tf = sizeFamily(to.Slug)
	}
	if ff != tf {
		return fmt.Errorf("digitalocean: %s (%s) to %s (%s): %w: different plan families", from.Slug, ff, to.Slug, tf, cloud.ErrResizeNotAllowed)
	}
	if to.DiskGiB < from.DiskGiB {
		return fmt.Errorf("digitalocean: %s to %s: %w: its %d GiB disk is smaller than the Droplet's %d GiB", from.Slug, to.Slug, cloud.ErrResizeNotAllowed, to.DiskGiB, from.DiskGiB)
	}
	return nil
}
