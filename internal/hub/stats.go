package hub

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/store"
)

// stater is what the stats collector needs from the database.
type stater interface {
	Stats(ctx context.Context) (store.Stats, error)
}

var (
	usersDesc      = prometheus.NewDesc("gravel_users", "Members registered on the hub.", nil, nil)
	identitiesDesc = prometheus.NewDesc("gravel_identities", "Linked identities by provider: each configured provider, and \"other\" for any the hub no longer has.", []string{"provider"}, nil)
	dbSizeDesc     = prometheus.NewDesc("gravel_database_size_bytes", "The hub database's size on disk (pg_database_size).", nil, nil)
	statsOKDesc    = prometheus.NewDesc("gravel_store_stats_readable", "1 when the counts were read at this scrape, 0 when the database did not answer in time.", nil, nil)
)

// statsCollector reads the counts at scrape time. When the database doesn't answer it reports
// readable 0 and nothing else, never numbers from an earlier scrape.
type statsCollector struct {
	db        stater
	providers []string
	logger    *slog.Logger
	timeout   time.Duration
}

func newStatsCollector(db stater, providers []string, logger *slog.Logger) *statsCollector {
	return &statsCollector{db: db, providers: providers, logger: logger, timeout: 2 * time.Second}
}

func (c *statsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{usersDesc, identitiesDesc, dbSizeDesc, statsOKDesc} {
		ch <- d
	}
}

func (c *statsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	s, err := c.db.Stats(ctx)
	if err != nil {
		c.logger.Warn("store stats unreadable", "error", err.Error())
		ch <- prometheus.MustNewConstMetric(statsOKDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(statsOKDesc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(usersDesc, prometheus.GaugeValue, float64(s.Users))
	ch <- prometheus.MustNewConstMetric(dbSizeDesc, prometheus.GaugeValue, float64(s.DatabaseBytes))
	// Every configured provider has a series, 0 included, so a panel shows the provider before
	// its first link. A provider removed from the config keeps its rows; they count as "other".
	known := map[string]bool{}
	for _, p := range c.providers {
		known[p] = true
		ch <- prometheus.MustNewConstMetric(identitiesDesc, prometheus.GaugeValue, float64(s.Identities[p]), p)
	}
	var other int64
	for p, n := range s.Identities {
		if !known[p] {
			other += n
		}
	}
	if other > 0 {
		ch <- prometheus.MustNewConstMetric(identitiesDesc, prometheus.GaugeValue, float64(other), "other")
	}
}
