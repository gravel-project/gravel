package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/gravel-project/gravel/internal/store"
)

type fakeStater struct {
	s     store.Stats
	err   error
	delay time.Duration
}

func (f fakeStater) Stats(ctx context.Context) (store.Stats, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return store.Stats{}, ctx.Err()
		}
	}
	return f.s, f.err
}

func collect(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	out, err := testutil.CollectAndFormat(c, expfmt.TypeTextPlain, "gravel_users", "gravel_identities", "gravel_database_size_bytes", "gravel_store_stats_readable")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestStatsCollector(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := fakeStater{s: store.Stats{Users: 3, Identities: map[string]int64{"discord": 3, "steam": 1, "xbox": 2}, DatabaseBytes: 8 << 20}}
	got := collect(t, newStatsCollector(ok, []string{"discord", "steam", "twitch"}, quiet))
	for _, want := range []string{
		"gravel_store_stats_readable 1", "gravel_users 3", "gravel_database_size_bytes 8.388608e+06",
		`gravel_identities{provider="discord"} 3`, `gravel_identities{provider="steam"} 1`,
		`gravel_identities{provider="twitch"} 0`, `gravel_identities{provider="other"} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ok: lacks %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, `provider="xbox"`) {
		t.Errorf("an unconfigured provider leaked as a label:\n%s", got)
	}

	// Nothing unconfigured: no "other" series.
	got = collect(t, newStatsCollector(fakeStater{s: store.Stats{Identities: map[string]int64{"discord": 1}}}, []string{"discord"}, quiet))
	if strings.Contains(got, `provider="other"`) {
		t.Errorf("other without unconfigured rows:\n%s", got)
	}

	// The database fails, or doesn't answer in time: readable 0 and no numbers.
	slow := newStatsCollector(fakeStater{s: ok.s, delay: time.Second}, []string{"discord"}, quiet)
	slow.timeout = 20 * time.Millisecond
	for name, c := range map[string]*statsCollector{
		"error":   newStatsCollector(fakeStater{err: errors.New("down")}, []string{"discord"}, quiet),
		"timeout": slow,
	} {
		start := time.Now()
		got := collect(t, c)
		if !strings.Contains(got, "gravel_store_stats_readable 0") || strings.Contains(got, "gravel_users") || strings.Contains(got, "gravel_identities") || strings.Contains(got, "gravel_database_size_bytes") {
			t.Errorf("%s:\n%s", name, got)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%s: the scrape waited %s", name, time.Since(start))
		}
	}
}
