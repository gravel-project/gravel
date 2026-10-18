package ingest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/store"
)

// memStore keeps batches and events as the store would: events unique by server, source and key,
// a parsed batch's events and state saved together.
type memStore struct {
	mu       sync.Mutex
	batches  []store.IngestBatch
	events   []store.StatsEvent
	failSave bool
}

func (m *memStore) add(server, source, body string, at time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := int64(len(m.batches) + 1)
	m.batches = append(m.batches, store.IngestBatch{ID: id, ServerID: server, Source: source, ReceivedAt: at, Body: []byte(body), State: store.IngestPending})
	return id
}

func (m *memStore) PendingIngestBatches(_ context.Context, _ uuid.UUID, source string, limit int) ([]store.IngestBatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.IngestBatch
	for _, b := range m.batches {
		if b.Source == source && b.State == store.IngestPending && len(out) < limit {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *memStore) insert(events []store.StatsEvent) []bool {
	stored := make([]bool, len(events))
	for i, e := range events {
		dup := false
		for _, have := range m.events {
			if have.ServerID == e.ServerID && have.Source == e.Source && have.SourceKey == e.SourceKey {
				dup = true
			}
		}
		if !dup {
			m.events = append(m.events, e)
			stored[i] = true
		}
	}
	return stored
}

func (m *memStore) SaveParsedBatch(_ context.Context, p store.ParsedBatch) ([]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave {
		return nil, errors.New("database away")
	}
	stored := m.insert(p.Events)
	b := &m.batches[p.BatchID-1]
	b.State, b.ParserVersion = p.State, p.ParserVersion
	return stored, nil
}

func (m *memStore) RequeueIngestBatches(_ context.Context, _ uuid.UUID, source string, version int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for i := range m.batches {
		if b := &m.batches[i]; b.Source == source && b.State != store.IngestPending && b.ParserVersion < version {
			b.State = store.IngestPending
			n++
		}
	}
	return n, nil
}

func (m *memStore) RecordStatsEvents(_ context.Context, events []store.StatsEvent) ([]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.insert(events), nil
}

func (m *memStore) state(id int64) (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.batches[id-1].State, m.batches[id-1].ParserVersion
}

// lineParser reads a body of lines "key kind [player]"; "!" fails the batch, "?x" is an unknown
// kind x, "-" an invalid event.
type lineParser struct{ version int }

func (p lineParser) Version() int { return p.version }

func (p lineParser) Parse(b Batch) (Parsed, error) {
	var out Parsed
	for _, l := range strings.Split(strings.TrimSpace(string(b.Body)), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
		case f[0] == "!":
			return Parsed{}, errors.New("not a batch")
		case strings.HasPrefix(f[0], "?"):
			out.Unknown = append(out.Unknown, f[0][1:])
		case f[0] == "-":
			out.Invalid++
		default:
			e := Event{Key: f[0], Kind: f[1], Extensions: map[string]any{"n": len(f)}}
			if len(f) > 2 {
				e.Player = store.PlayerKey{Provider: "steam", Subject: f[2]}
			}
			out.Events = append(out.Events, e)
		}
	}
	return out, nil
}

type parseRig struct {
	st  *memStore
	p   *Parsing
	org uuid.UUID
	at  time.Time
}

func newParseRig(t *testing.T) *parseRig {
	t.Helper()
	r := &parseRig{st: &memStore{}, org: uuid.New(), at: time.Date(2026, 10, 18, 17, 4, 31, 0, time.UTC)}
	r.p = r.parsing(1)
	return r
}

func (r *parseRig) parsing(version int) *Parsing {
	info := func(_ context.Context, server string) (string, string, error) {
		if server == "gone" {
			return "", "", errors.New("no such server")
		}
		return "wardogs", "official", nil
	}
	return NewParsing(r.st, r.org, map[string]Parser{"wardogs": lineParser{version: version}}, info, prometheus.NewRegistry(), quiet())
}

func TestParseStoresEventsWithProvenance(t *testing.T) {
	r := newParseRig(t)
	ctx := context.Background()
	b1 := r.st.add("wd-1", "wardogs", "e1 kill 76561190000000001\ne2 match_state", r.at)
	logBatch := r.st.add("cs-1", "cs2", "a log line", r.at)
	if err := r.p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st, v := r.st.state(b1); st != store.IngestParsed || v != 1 {
		t.Errorf("batch = %s v%d", st, v)
	}
	if st, _ := r.st.state(logBatch); st != store.IngestPending {
		t.Errorf("a source with no parser was read: %s", st)
	}
	if len(r.st.events) != 2 {
		t.Fatalf("events = %+v", r.st.events)
	}
	e := r.st.events[0]
	if e.OrganizationID != r.org || e.ServerID != "wd-1" || e.GameID != "wardogs" || e.Source != SourceServerAdapter || e.SourceKey != "e1" ||
		e.Kind != "kill" || !e.ObservedAt.Equal(r.at) || !e.ReceivedAt.Equal(r.at) || e.Trust != "official" || e.BatchID != b1 ||
		e.SchemaVersion != StatsSchemaVersion || e.Player.Subject != "76561190000000001" || string(e.Extensions) != `{"n":3}` {
		t.Errorf("event = %+v", e)
	}
	if n := testutil.ToFloat64(r.p.events.WithLabelValues("wd-1", "kill")); n != 1 {
		t.Errorf("kill events counted %v", n)
	}
	if n := testutil.ToFloat64(r.p.parsed.WithLabelValues("wd-1", "wardogs", "parsed")); n != 1 {
		t.Errorf("parsed counted %v", n)
	}
}

func TestParseDropsDuplicatesAndKeepsFailures(t *testing.T) {
	r := newParseRig(t)
	ctx := context.Background()
	r.st.add("wd-1", "wardogs", "e1 kill", r.at)
	r.st.add("wd-1", "wardogs", "e1 kill\ne2 kill", r.at.Add(2*time.Second)) // the source sent e1 again
	r.st.add("wd-2", "wardogs", "e1 kill", r.at)                             // another server's e1 is its own
	bad := r.st.add("wd-1", "wardogs", "!", r.at)
	odd := r.st.add("wd-1", "wardogs", "?kill_v2\n?kill_v2\n-\ne3 match_state", r.at)
	if err := r.p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.st.events) != 4 {
		t.Errorf("events = %+v", r.st.events)
	}
	if n := testutil.ToFloat64(r.p.duplicates.WithLabelValues("wd-1")); n != 1 {
		t.Errorf("duplicates counted %v", n)
	}
	if st, v := r.st.state(bad); st != store.IngestFailed || v != 1 {
		t.Errorf("a batch the parser could not read = %s v%d", st, v)
	}
	if st, _ := r.st.state(odd); st != store.IngestParsed {
		t.Errorf("a batch with unknown kinds = %s", st)
	}
	if n := testutil.ToFloat64(r.p.events.WithLabelValues("wd-1", "unknown")); n != 2 {
		t.Errorf("unknown counted %v", n)
	}
	if n := testutil.ToFloat64(r.p.events.WithLabelValues("wd-1", "invalid")); n != 1 {
		t.Errorf("invalid counted %v", n)
	}
	if n := testutil.ToFloat64(r.p.parsed.WithLabelValues("wd-1", "wardogs", "failed")); n != 1 {
		t.Errorf("failed counted %v", n)
	}

	// A newer parser reads every batch again on its first run: the failed one, and the parsed ones,
	// whose events it finds stored already, which is no duplicate the source sent.
	r.p = r.parsing(2)
	if err := r.p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if st, v := r.st.state(bad); st != store.IngestFailed || v != 2 {
		t.Errorf("the failed batch after the new parser = %s v%d", st, v)
	}
	if st, v := r.st.state(odd); st != store.IngestParsed || v != 2 {
		t.Errorf("a parsed batch after the new parser = %s v%d", st, v)
	}
	if len(r.st.events) != 4 {
		t.Errorf("events after reading again = %d", len(r.st.events))
	}
	if n := testutil.ToFloat64(r.p.duplicates.WithLabelValues("wd-1")); n != 0 {
		t.Errorf("reading again counted %v duplicates", n)
	}
	// Later runs read only what is new.
	r.st.add("wd-1", "wardogs", "e9 kill", r.at.Add(time.Minute))
	if err := r.p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := testutil.ToFloat64(r.p.parsed.WithLabelValues("wd-1", "wardogs", "parsed")); n != 4 {
		t.Errorf("parsed in the new parser's runs: %v", n)
	}
}

func TestParseStopsOnTheStore(t *testing.T) {
	r := newParseRig(t)
	ctx := context.Background()
	b := r.st.add("wd-1", "wardogs", "e1 kill", r.at)
	r.st.failSave = true
	if err := r.p.Run(ctx); err == nil {
		t.Fatal("a store failure was not an error")
	}
	if st, _ := r.st.state(b); st != store.IngestPending {
		t.Errorf("a batch not saved left pending: %s", st)
	}
	r.st.failSave = false
	// A batch whose server cannot be read stays pending, and the ones after it are read.
	gone := r.st.add("gone", "wardogs", "e1 kill", r.at)
	after := r.st.add("wd-1", "wardogs", "e2 kill", r.at)
	if err := r.p.Run(ctx); err == nil {
		t.Error("a server that cannot be read was not an error")
	}
	for id, want := range map[int64]string{b: store.IngestParsed, gone: store.IngestPending, after: store.IngestParsed} {
		if st, _ := r.st.state(id); st != want {
			t.Errorf("batch %d = %s, want %s", id, st, want)
		}
	}
}

// TestParseEndsOnAPageItCannotRead: a full page of batches whose server cannot be read ends the
// run instead of asking for the same page forever.
func TestParseEndsOnAPageItCannotRead(t *testing.T) {
	r := newParseRig(t)
	for range parsePage + 5 {
		r.st.add("gone", "wardogs", "e1 kill", r.at)
	}
	last := r.st.add("wd-1", "wardogs", "e2 kill", r.at)
	done := make(chan error, 1)
	go func() { done <- r.p.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
	if st, _ := r.st.state(last); st != store.IngestParsed {
		t.Errorf("the batch past them = %s", st)
	}
}
