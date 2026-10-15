package digitalocean_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/digitalocean/dotest"
)

// The team's preview holds every project's items, as DigitalOcean writes them (the shapes seen on
// Hidden Token Gaming's preview on 2026-10-15; the amounts are made up): volume snapshots in the
// default project and a Droplet's public IPv4 in none, whichever project the resource is in.
var previewItems = []godo.InvoiceItem{
	{Product: "Droplets", ResourceID: "608243204", Description: "htg-cs2-1 (c-2-4GiB)", Amount: "12.40", ProjectName: "hidden-token-gaming", Category: "iaas"},
	{Product: "Volumes", ResourceUUID: "ab28e8c9-1111-4c1e-9d1d-0f3d1e5d7a10", Description: "htg-cs2-1-data (sfo3) - 100.00GB Volume", Amount: "6.67", ProjectName: "hidden-token-gaming", Category: "iaas"},
	{Product: "Volume Snapshots", ResourceUUID: "9bfdeb3c-2222-4c1e-9d1d-0f3d1e5d7a10", Description: "htg-cs2-1-data-cs2-1.41.9.0-2026-10-12 (sfo3) 77.23GB Volume Snapshot", Amount: "2.04", ProjectName: "Default", Category: "iaas"},
	{Product: "Public IPv4", ResourceUUID: "do:droplet:608172308", Description: "Public IPv4 - htg-spike-mem1 (1 instance)", Amount: "0.50", Category: "iaas"},
	{Product: "Public IPv4", ResourceUUID: "do:droplet:608188895", Description: "Public IPv4 - htg-hub-1 (1 instance)", Amount: "0.40", Category: "iaas"},
	{Product: "Taxes", Description: "Texas State Sales Tax Rate (6.25%)", Amount: "1.20", Category: "iaas"},
	{Product: "Droplets", ResourceID: "608188895", Description: "htg-hub-1 (s-2vcpu-4gb)", Amount: "11.20", ProjectName: "hidden-token-gaming", Category: "iaas"},
	{Product: "Something New", Description: "a product nobody mapped", Amount: "0.10", ProjectName: "hidden-token-gaming"},
	// Another named project on the same team: never returned.
	{Product: "Droplets", ResourceID: "5000", Description: "mkzsys-web (s-1vcpu-1gb)", Amount: "4.00", ProjectName: "mkzsys", Category: "iaas"},
}

// The invoice preview: the project's items (by name or by id), and the unassigned ones (no
// project, or the default one) marked so, each with its kind, id and name.
func TestMonthToDate(t *testing.T) {
	updated := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	for name, project := range map[string]string{"by name": "hidden-token-gaming", "by id": projectID} {
		t.Run(name, func(t *testing.T) {
			p, srv := newProvider(t, project, dotest.WithInvoiceItems(updated, previewItems...), dotest.WithMaxPerPage(2))
			bill, err := p.MonthToDate(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !bill.GeneratedAt.Equal(updated) || !bill.Month.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || len(bill.Lines) != 8 {
				t.Fatalf("bill = %+v", bill)
			}
			want := []cloud.BillLine{
				{ResourceID: "608243204", Name: "htg-cs2-1", Product: "Droplets", Kind: cloud.CostCompute, Description: "htg-cs2-1 (c-2-4GiB)", USD: 12.40},
				{ResourceID: "ab28e8c9-1111-4c1e-9d1d-0f3d1e5d7a10", Name: "htg-cs2-1-data", Product: "Volumes", Kind: cloud.CostStorage,
					Description: "htg-cs2-1-data (sfo3) - 100.00GB Volume", USD: 6.67},
				{ResourceID: "9bfdeb3c-2222-4c1e-9d1d-0f3d1e5d7a10", Name: "htg-cs2-1-data-cs2-1.41.9.0-2026-10-12", Product: "Volume Snapshots", Kind: cloud.CostStorage,
					Description: "htg-cs2-1-data-cs2-1.41.9.0-2026-10-12 (sfo3) 77.23GB Volume Snapshot", USD: 2.04, Unassigned: true},
				{ResourceID: "608172308", Name: "htg-spike-mem1", Product: "Public IPv4", Kind: cloud.CostNetwork, Description: "Public IPv4 - htg-spike-mem1 (1 instance)",
					USD: 0.50, Unassigned: true},
				// The project's own Droplet's address: the project's, not unassigned.
				{ResourceID: "608188895", Name: "htg-hub-1", Product: "Public IPv4", Kind: cloud.CostNetwork, Description: "Public IPv4 - htg-hub-1 (1 instance)", USD: 0.40},
				{Name: "Texas State Sales Tax Rate", Product: "Taxes", Kind: cloud.CostOther, Description: "Texas State Sales Tax Rate (6.25%)", USD: 1.20, Unassigned: true},
				{ResourceID: "608188895", Name: "htg-hub-1", Product: "Droplets", Kind: cloud.CostCompute, Description: "htg-hub-1 (s-2vcpu-4gb)", USD: 11.20},
				{Name: "a product nobody mapped", Product: "Something New", Kind: cloud.CostOther, Description: "a product nobody mapped", USD: 0.10},
			}
			for i, w := range want {
				if bill.Lines[i] != w {
					t.Errorf("line %d = %+v\nwant %+v", i, bill.Lines[i], w)
				}
			}
			// Nine items at two a page: five pages read.
			if n := count(srv.Requests(), "GET", "/v2/customers/my/invoices/preview"); n != 5 {
				t.Errorf("%d preview pages read", n)
			}
		})
	}
}

// A project left unset is the team's default project; a token without billing:read is
// cloud.ErrForbidden, which never carries the token.
func TestMonthToDateDefaultProjectAndForbidden(t *testing.T) {
	items := []godo.InvoiceItem{{Product: "Droplets", Description: "box (s-1vcpu-1gb)", Amount: "1.00", ProjectName: "Default"}, previewItems[0]}
	p, srv := newProvider(t, "", dotest.WithInvoiceItems(time.Now(), items...))
	bill, err := p.MonthToDate(t.Context())
	// The default project's items are the project's own here, not unassigned.
	if err != nil || len(bill.Lines) != 1 || bill.Lines[0].Name != "box" || bill.Lines[0].Unassigned {
		t.Fatalf("default project: %+v, %v", bill, err)
	}
	srv.Fault("GET", "/v2/customers/my/invoices", 403, 1)
	_, err = p.MonthToDate(t.Context())
	if !errors.Is(err, cloud.ErrForbidden) || strings.Contains(err.Error(), secretToken) {
		t.Errorf("forbidden: %v", err)
	}
	srv.Fault("GET", "/v2/projects/default", 403, 1)
	if _, err := p.MonthToDate(t.Context()); !errors.Is(err, cloud.ErrForbidden) {
		t.Errorf("no project:read: %v", err)
	}
}
