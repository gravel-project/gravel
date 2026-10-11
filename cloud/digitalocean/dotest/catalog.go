package dotest

import "github.com/digitalocean/godo"

// catalog is a realistic subset of the account catalog: sfo3 and nyc3 with Basic and
// CPU-Optimized sizes at DigitalOcean's 2026 prices, and a made-up region mkc1 that offers only
// a newer-line size, for tests of a size a region does not offer.
func catalog() ([]godo.Region, []godo.Size, []godo.Image) {
	both := []string{"nyc3", "sfo3"}
	sizes := []godo.Size{
		{Slug: "s-1vcpu-512mb-10gb", Memory: 512, Vcpus: 1, Disk: 10, PriceMonthly: 4, PriceHourly: 0.00595, Transfer: 0.5, Description: "Basic", Regions: both, Available: true},
		{Slug: "s-1vcpu-1gb", Memory: 1024, Vcpus: 1, Disk: 25, PriceMonthly: 6, PriceHourly: 0.00893, Transfer: 1, Description: "Basic", Regions: both, Available: true},
		{Slug: "s-2vcpu-4gb", Memory: 4096, Vcpus: 2, Disk: 80, PriceMonthly: 24, PriceHourly: 0.03571, Transfer: 4, Description: "Basic", Regions: both, Available: true},
		{Slug: "c-2", Memory: 4096, Vcpus: 2, Disk: 25, PriceMonthly: 42, PriceHourly: 0.0625, Transfer: 4, Description: "CPU-Optimized", Regions: both, Available: true},
		{Slug: "c-4", Memory: 8192, Vcpus: 4, Disk: 50, PriceMonthly: 84, PriceHourly: 0.125, Transfer: 5, Description: "CPU-Optimized", Regions: both, Available: true},
		{Slug: "s5-1vcpu-1gb-30gb", Memory: 1024, Vcpus: 1, Disk: 30, PriceMonthly: 7, PriceHourly: 0.01042, Transfer: 1, Description: "Basic", Regions: []string{"mkc1"}, Available: true},
	}
	features := []string{"backups", "ipv6", "metadata", "install_agent", "storage", "image_transfer"}
	regionSizes := func(region string) []string {
		var out []string
		for _, s := range sizes {
			for _, r := range s.Regions {
				if r == region {
					out = append(out, s.Slug)
				}
			}
		}
		return out
	}
	regions := []godo.Region{
		{Slug: "nyc3", Name: "New York 3", Sizes: regionSizes("nyc3"), Available: true, Features: features},
		{Slug: "sfo3", Name: "San Francisco 3", Sizes: regionSizes("sfo3"), Available: true, Features: features},
		{Slug: "mkc1", Name: "Mock City 1", Sizes: regionSizes("mkc1"), Available: true, Features: features},
	}
	all := []string{"mkc1", "nyc3", "sfo3"}
	images := []godo.Image{
		{ID: 190000001, Name: "44 x64", Distribution: "Fedora", Slug: "fedora-44-x64", Public: true, Regions: all, MinDiskSize: 7, Type: "base", Status: "available"},
		{ID: 190000002, Name: "24.04 (LTS) x64", Distribution: "Ubuntu", Slug: "ubuntu-24-04-x64", Public: true, Regions: all, MinDiskSize: 7, Type: "base", Status: "available"},
		{ID: 190000003, Name: "12 x64", Distribution: "Debian", Slug: "debian-12-x64", Public: true, Regions: all, MinDiskSize: 7, Type: "base", Status: "available"},
	}
	return regions, sizes, images
}
