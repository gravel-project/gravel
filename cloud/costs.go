package cloud

import (
	"context"
	"time"
)

// Coster is a Provider that can read its own bill (ADR-0034). It is optional: a caller finds it
// with a type assertion, and a provider without it is costed from the hub's own records only.
type Coster interface {
	// MonthToDate is the cloud's bill for the current month so far, line by line, for the
	// provider's project only: never another project's resources on a shared account. A token
	// without the scope to read bills is ErrForbidden.
	MonthToDate(ctx context.Context) (Bill, error)
}

// Bill is a month's bill so far.
type Bill struct {
	// GeneratedAt is when the cloud last brought it up to date (zero when the cloud doesn't say):
	// a bill lags its usage by up to a day.
	GeneratedAt time.Time
	// Month is the bill's month, its first day in UTC (zero: the current month).
	Month time.Time
	Lines []BillLine
}

// BillLine is one charge on a bill: one resource's (or one service's) amount so far.
type BillLine struct {
	// ResourceID is the cloud's id for the resource where it gives one, as Instance.ID and
	// Volume.ID spell it; Name is its name where the bill says it. Either may be empty.
	ResourceID string
	Name       string
	// Product is the cloud's own word for what it charged ("Droplets"); Kind is one of the
	// Cost kinds below.
	Product     string
	Kind        string
	Description string
	USD         float64
}

// What a line is for.
const (
	CostCompute = "compute"
	CostStorage = "storage"
	CostNetwork = "network"
	CostOther   = "other"
)

// CostKinds are the kinds, in the order a page shows them.
var CostKinds = []string{CostCompute, CostStorage, CostNetwork, CostOther}
