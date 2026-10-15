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

// The team's preview holds every project's items; a provider returns its own project's only.
var previewItems = []godo.InvoiceItem{
	{Product: "Droplets", ResourceID: "4101", Description: "htg-cs2-1 (c-2-4GiB)", Amount: "12.40", ProjectName: "hidden-token-gaming", Category: "iaas"},
	{Product: "Volumes", ResourceUUID: "7c0f9c1e-6a35-4c1e-9d1d-0f3d1e5d7a10", Description: "htg-cs2-1-data (100 GiB)", Amount: "6.67", ProjectName: "hidden-token-gaming"},
	{Product: "Snapshots", Description: "htg-cs2-1-data-cs2-2026-10-12 (48.2 GiB)", Amount: "0.81", ProjectName: "hidden-token-gaming"},
	{Product: "Reserved IPs", Description: "Unused Reserved IP - 144.126.220.9", Amount: "2.50", ProjectName: "hidden-token-gaming"},
	{Product: "Droplets", ResourceID: "4000", Description: "htg-hub-1 (s-2vcpu-4gb)", Amount: "11.20", ProjectName: "hidden-token-gaming"},
	{Product: "Something New", Description: "a product nobody mapped", Amount: "0.10", ProjectName: "hidden-token-gaming"},
	// Another project on the same team: never returned.
	{Product: "Droplets", ResourceID: "5000", Description: "mkzsys-web (s-1vcpu-1gb)", Amount: "4.00", ProjectName: "mkzsys"},
}

// The invoice preview, filtered to the project (by name or by id), each item's kind and name.
func TestMonthToDate(t *testing.T) {
	updated := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	for name, project := range map[string]string{"by name": "hidden-token-gaming", "by id": projectID} {
		t.Run(name, func(t *testing.T) {
			p, srv := newProvider(t, project, dotest.WithInvoiceItems(updated, previewItems...), dotest.WithMaxPerPage(2))
			bill, err := p.MonthToDate(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !bill.GeneratedAt.Equal(updated) || !bill.Month.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || len(bill.Lines) != 6 {
				t.Fatalf("bill = %+v", bill)
			}
			want := []cloud.BillLine{
				{ResourceID: "4101", Name: "htg-cs2-1", Product: "Droplets", Kind: cloud.CostCompute, Description: "htg-cs2-1 (c-2-4GiB)", USD: 12.40},
				{ResourceID: "7c0f9c1e-6a35-4c1e-9d1d-0f3d1e5d7a10", Name: "htg-cs2-1-data", Product: "Volumes", Kind: cloud.CostStorage, Description: "htg-cs2-1-data (100 GiB)", USD: 6.67},
				{Name: "htg-cs2-1-data-cs2-2026-10-12", Product: "Snapshots", Kind: cloud.CostStorage, Description: "htg-cs2-1-data-cs2-2026-10-12 (48.2 GiB)", USD: 0.81},
				{Name: "Unused Reserved IP - 144.126.220.9", Product: "Reserved IPs", Kind: cloud.CostNetwork, Description: "Unused Reserved IP - 144.126.220.9", USD: 2.50},
				{ResourceID: "4000", Name: "htg-hub-1", Product: "Droplets", Kind: cloud.CostCompute, Description: "htg-hub-1 (s-2vcpu-4gb)", USD: 11.20},
				{Name: "a product nobody mapped", Product: "Something New", Kind: cloud.CostOther, Description: "a product nobody mapped", USD: 0.10},
			}
			for i, w := range want {
				if bill.Lines[i] != w {
					t.Errorf("line %d = %+v\nwant %+v", i, bill.Lines[i], w)
				}
			}
			// Seven items at two a page: four pages read.
			if n := count(srv.Requests(), "GET", "/v2/customers/my/invoices/preview"); n != 4 {
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
	if err != nil || len(bill.Lines) != 1 || bill.Lines[0].Name != "box" {
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
