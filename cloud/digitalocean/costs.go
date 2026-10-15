package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

var _ cloud.Coster = (*Provider)(nil)

// MonthToDate is the team's invoice preview (generated daily) for the provider's project: each
// item whose project_name is the project's (ADR-0034). A token without billing:read (and one
// without project:read, which names the project) is cloud.ErrForbidden. The preview is the whole
// team's: the other projects' items are read and dropped, never returned.
func (p *Provider) MonthToDate(ctx context.Context) (cloud.Bill, error) {
	project, err := p.projectName(ctx)
	if err != nil {
		return cloud.Bill{}, forbidden(err)
	}
	var bill cloud.Bill
	if list, _, err := p.c.Invoices.List(ctx, &godo.ListOptions{Page: 1, PerPage: 1}); err == nil {
		bill.GeneratedAt = list.InvoicePreview.UpdatedAt
		if m, err := time.Parse("2006-01", list.InvoicePreview.InvoicePeriod); err == nil {
			bill.Month = m
		}
	} else {
		return cloud.Bill{}, forbidden(apiErr("read the invoice preview", err))
	}
	items, err := listAll(ctx, "read the invoice preview", func(ctx context.Context, opt *godo.ListOptions) ([]godo.InvoiceItem, *godo.Response, error) {
		inv, resp, err := p.c.Invoices.Get(ctx, "preview", opt)
		if err != nil {
			return nil, resp, err
		}
		if resp != nil && resp.Links == nil {
			resp.Links = inv.Links
		}
		return inv.InvoiceItems, resp, nil
	})
	if err != nil {
		return cloud.Bill{}, forbidden(err)
	}
	for _, it := range items {
		if it.ProjectName != project {
			continue
		}
		usd, err := strconv.ParseFloat(strings.TrimSpace(it.Amount), 64)
		if err != nil {
			return cloud.Bill{}, fmt.Errorf("digitalocean: invoice preview: %q's amount %q: %w", it.Description, it.Amount, err)
		}
		id := it.ResourceID
		if id == "" {
			id = it.ResourceUUID
		}
		bill.Lines = append(bill.Lines, cloud.BillLine{ResourceID: id, Name: itemName(it.Description), Product: it.Product,
			Kind: costKind(it.Product, it.Category), Description: it.Description, USD: usd})
	}
	return bill, nil
}

// forbidden marks an unauthorized answer as cloud.ErrForbidden: a token without the scope.
func forbidden(err error) error {
	if errors.Is(err, ErrUnauthorized) && !errors.Is(err, cloud.ErrForbidden) {
		return fmt.Errorf("%w: %w", cloud.ErrForbidden, err)
	}
	return err
}

// projectName is the project's name, which invoice items carry (not its id): Config.Project when
// it is a name, else the project it names by id, else the team's default project.
func (p *Provider) projectName(ctx context.Context) (string, error) {
	switch {
	case p.project == "":
		pr, _, err := p.c.Projects.GetDefault(ctx)
		if err != nil {
			return "", apiErr("read the default project", err)
		}
		return pr.Name, nil
	case uuidRE.MatchString(p.project):
		pr, _, err := p.c.Projects.Get(ctx, p.project)
		if err != nil {
			return "", apiErr("read project "+p.project, err)
		}
		return pr.Name, nil
	}
	return p.project, nil
}

// costKind maps DigitalOcean's product (and category) to a cost kind by keyword. Its product
// names are not a documented list, so an unknown one is "other", never dropped.
func costKind(product, category string) string {
	s := strings.ToLower(product + " " + category)
	switch {
	case strings.Contains(s, "droplet") || strings.Contains(s, "kubernetes") || strings.Contains(s, "app platform"):
		return cloud.CostCompute
	case strings.Contains(s, "volume") || strings.Contains(s, "block storage") || strings.Contains(s, "snapshot") ||
		strings.Contains(s, "backup") || strings.Contains(s, "spaces"):
		return cloud.CostStorage
	case strings.Contains(s, " ip") || strings.Contains(s, "ip ") || strings.HasPrefix(s, "ip") || strings.Contains(s, "bandwidth") ||
		strings.Contains(s, "transfer") || strings.Contains(s, "load balancer"):
		return cloud.CostNetwork
	}
	return cloud.CostOther
}

// itemName is a resource's name from an item's description, which DigitalOcean writes as
// "name (size)" (a Droplet "htg-cs2-1 (c-2-4GiB)"): what is before the bracket, without a
// "Kind:" prefix. A description that isn't one name is returned as it is.
func itemName(description string) string {
	s := strings.TrimSpace(description)
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return strings.TrimSpace(s)
}
