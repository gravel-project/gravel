package fake

import (
	"context"
	"slices"

	"github.com/gravel-project/gravel/cloud"
)

var _ cloud.Coster = (*Provider)(nil)

// SetBill sets what MonthToDate answers (ADR-0034); FailNext("MonthToDate", cloud.ErrForbidden)
// plays a token without the scope to read bills.
func (p *Provider) SetBill(b cloud.Bill) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b.Lines = slices.Clone(b.Lines)
	p.bill = b
}

// MonthToDate is the bill SetBill set: none until then.
func (p *Provider) MonthToDate(ctx context.Context) (cloud.Bill, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.beginLocked(ctx, "MonthToDate"); err != nil {
		return cloud.Bill{}, err
	}
	b := p.bill
	b.Lines = slices.Clone(b.Lines)
	return b, nil
}
