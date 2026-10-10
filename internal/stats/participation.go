package stats

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// Participation events (ADR-0014): what a first-party app records about a player taking part in
// something it tracks, a seeding session or an event they attended. They are first_party by
// construction and feed participation boards and profiles only (ADR-0011's trust mapping), never a
// combat board: they live in tables of their own that no combat board reads.

// SourceFirstParty is the provenance of participation events (ADR-0011).
const SourceFirstParty = "first_party"

// ParticipationSchemaVersion is the version of the participation event schema stored with each
// event; a request's schema_version 0 means this one.
const ParticipationSchemaVersion = 1

// Limits on a request and its events.
const (
	MaxParticipationEvents = 100
	MaxParticipationKey    = 128
	MaxParticipationKind   = 32
	MaxParticipationSubj   = 64
	MaxParticipationSess   = 128
	MaxParticipationSecs   = 86400
	// ParticipationFutureSkew is how far ahead of the hub's clock an event may be, for an app's
	// clock running a little fast.
	ParticipationFutureSkew = 5 * time.Minute
)

// What became of an event, as RecordParticipation answers and the metric counts.
const (
	OutcomeRecorded  = "recorded"
	OutcomeDuplicate = "duplicate"
	OutcomeInvalid   = "invalid"
)

// ParticipationProviders are the identity providers a participation event may name: the ones a
// member can link (internal/identity/providers), so an event can reach a member's profile.
var ParticipationProviders = org.MappedProviders

// ErrInvalidParticipation is a request with an invalid event; the message names every problem.
var ErrInvalidParticipation = errors.New("invalid participation events")

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidKind reports whether a kind is well formed: a lower-case letter, then up to 31 lower-case
// letters, digits or underscores.
func ValidKind(kind string) bool { return kindPattern.MatchString(kind) }

// ParticipationEvent is one event as an app sends it. Identity is nil when the app sent none.
type ParticipationEvent struct {
	Key             string
	Kind            string
	Identity        *store.PlayerKey
	GameID          string
	ServerID        string
	Session         string
	OccurredAt      time.Time
	DurationSeconds int
	SchemaVersion   int
}

// ParticipationResult is what became of one event: OutcomeRecorded or OutcomeDuplicate.
type ParticipationResult struct {
	Key     string
	Outcome string
}

// ParticipationStore is where participation events are written.
type ParticipationStore interface {
	RecordParticipation(ctx context.Context, events []store.ParticipationEvent) ([]bool, error)
}

// ServerSource finds a live server by id, or store.ErrNotFound (servers.Service).
type ServerSource interface {
	Server(ctx context.Context, id string) (servers.Server, error)
}

// Participation records participation events from first-party apps.
type Participation struct {
	st       ParticipationStore
	orgID    uuid.UUID
	servers  ServerSource
	games    []string
	settings SettingsSource
	changes  *Changes
	now      func() time.Time
	events   *prometheus.CounterVec
}

// NewParticipation builds the recorder and registers its metric. games are the games this hub
// ships (servers.GameIDs); changes is bumped when an event is stored, for the boards' cache.
func NewParticipation(st ParticipationStore, orgID uuid.UUID, srv ServerSource, games []string, settings SettingsSource, changes *Changes, reg prometheus.Registerer) *Participation {
	p := &Participation{st: st, orgID: orgID, servers: srv, games: games, settings: settings, changes: changes, now: time.Now,
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_participation_events_total",
			Help: `Participation events from first-party apps (ADR-0014), by kind and outcome (recorded, duplicate, invalid); kind is "invalid" when the kind itself is.`,
		}, []string{"kind", "outcome"})}
	reg.MustRegister(p.events)
	return p
}

// Record checks every event, then stores them all for the app in one transaction and says what
// became of each, in order. One invalid event refuses the request (ErrInvalidParticipation,
// naming every problem by index) and nothing is stored. A key the app used before is a duplicate:
// it is not stored again, even when its other fields differ.
func (p *Participation) Record(ctx context.Context, appID uuid.UUID, events []ParticipationEvent) ([]ParticipationResult, error) {
	if appID == uuid.Nil {
		return nil, errors.New("stats: participation needs the app that sends it")
	}
	if len(events) == 0 || len(events) > MaxParticipationEvents {
		p.count(events, OutcomeInvalid)
		return nil, fmt.Errorf("%w: events: %d events, send 1 to %d", ErrInvalidParticipation, len(events), MaxParticipationEvents)
	}
	set, err := p.settings(ctx)
	if err != nil {
		return nil, err
	}
	now := p.now()
	rows, problems, err := p.check(ctx, appID, events, now, Cutoff(now, set.Location(), set.RawRetentionMonthsOrDefault()))
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		p.count(events, OutcomeInvalid)
		return nil, fmt.Errorf("%w: %s", ErrInvalidParticipation, strings.Join(problems, "; "))
	}
	stored, err := p.st.RecordParticipation(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := make([]ParticipationResult, len(events))
	wrote := false
	for i, e := range events {
		out[i] = ParticipationResult{Key: e.Key, Outcome: OutcomeDuplicate}
		if stored[i] {
			out[i].Outcome, wrote = OutcomeRecorded, true
		}
		p.events.WithLabelValues(e.Kind, out[i].Outcome).Inc()
	}
	if wrote {
		p.changes.Bump()
	}
	return out, nil
}

// count counts events under an outcome, by kind ("invalid" for a kind that is not well formed, so
// a bad sender cannot grow the label set).
func (p *Participation) count(events []ParticipationEvent, outcome string) {
	for _, e := range events {
		kind := e.Kind
		if !ValidKind(kind) {
			kind = OutcomeInvalid
		}
		p.events.WithLabelValues(kind, outcome).Inc()
	}
}

// check validates every event and turns the valid ones into rows. problems name each invalid
// field by index; err is a failure to check (the database), not a bad event.
func (p *Participation) check(ctx context.Context, appID uuid.UUID, events []ParticipationEvent, now, cutoff time.Time) ([]store.ParticipationEvent, []string, error) {
	var problems []string
	bad := func(i int, field, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("events[%d].%s: ", i, field)+fmt.Sprintf(format, args...))
	}
	gameOf := map[string]string{} // server id → its game; "" for a server the hub does not know
	rows := make([]store.ParticipationEvent, 0, len(events))
	for i, e := range events {
		row := store.ParticipationEvent{OrganizationID: p.orgID, AppID: appID, Key: e.Key, Kind: e.Kind, GameID: e.GameID, ServerID: e.ServerID,
			Session: e.Session, OccurredAt: e.OccurredAt, DurationSeconds: e.DurationSeconds, ReceivedAt: now,
			Source: SourceFirstParty, SchemaVersion: ParticipationSchemaVersion}
		switch n := len(e.Key); {
		case n == 0:
			bad(i, "key", "required")
		case n > MaxParticipationKey:
			bad(i, "key", "%d characters, at most %d", n, MaxParticipationKey)
		case !printableASCII(e.Key):
			bad(i, "key", "must be printable ASCII")
		}
		if !ValidKind(e.Kind) {
			bad(i, "kind", "%s is not a lower-case letter followed by up to %d lower-case letters, digits or underscores", quote(e.Kind), MaxParticipationKind-1)
		}
		if e.Identity == nil {
			bad(i, "identity", "required")
		} else {
			row.Provider, row.Subject = e.Identity.Provider, e.Identity.Subject
			if !slices.Contains(ParticipationProviders, e.Identity.Provider) {
				bad(i, "identity.provider", "%s is not one of %s", quote(e.Identity.Provider), strings.Join(ParticipationProviders, ", "))
			}
			switch n := utf8.RuneCountInString(e.Identity.Subject); {
			case n == 0:
				bad(i, "identity.subject", "required")
			case n > MaxParticipationSubj:
				bad(i, "identity.subject", "%d characters, at most %d", n, MaxParticipationSubj)
			case hasControl(e.Identity.Subject):
				bad(i, "identity.subject", "must not hold control characters")
			}
		}
		if e.GameID != "" && !slices.Contains(p.games, e.GameID) {
			bad(i, "game_id", "%s is not a game this hub ships", quote(e.GameID))
		}
		if e.ServerID != "" {
			game, seen := gameOf[e.ServerID]
			if !seen {
				srv, err := p.servers.Server(ctx, e.ServerID)
				switch {
				case errors.Is(err, store.ErrNotFound):
				case err != nil:
					return nil, nil, err
				default:
					game = srv.Game
				}
				gameOf[e.ServerID] = game
			}
			switch {
			case game == "":
				bad(i, "server_id", "%s is not a server this hub knows", quote(e.ServerID))
			case e.GameID != "" && e.GameID != game:
				bad(i, "game_id", "%s is not the game of server %s (%s)", quote(e.GameID), quote(e.ServerID), game)
			default:
				row.GameID = game
			}
		}
		switch n := utf8.RuneCountInString(e.Session); {
		case n > MaxParticipationSess:
			bad(i, "session", "%d characters, at most %d", n, MaxParticipationSess)
		case hasControl(e.Session):
			bad(i, "session", "must not hold control characters")
		}
		switch {
		case e.OccurredAt.IsZero():
			bad(i, "occurred_at", "required")
		case e.OccurredAt.After(now.Add(ParticipationFutureSkew)):
			bad(i, "occurred_at", "%s is more than %s ahead of the hub's clock (%s)", e.OccurredAt.UTC().Format(time.RFC3339), ParticipationFutureSkew, now.UTC().Format(time.RFC3339))
		case e.OccurredAt.Before(cutoff):
			bad(i, "occurred_at", "%s is before %s, older than the stats store keeps events (stats.raw_retention_months)",
				e.OccurredAt.UTC().Format(time.RFC3339), cutoff.Format(time.DateOnly))
		}
		if e.DurationSeconds < 0 || e.DurationSeconds > MaxParticipationSecs {
			bad(i, "duration_seconds", "%d is not between 0 and %d", e.DurationSeconds, MaxParticipationSecs)
		}
		if e.SchemaVersion != 0 && e.SchemaVersion != ParticipationSchemaVersion {
			bad(i, "schema_version", "%d is not %d (or 0 for it)", e.SchemaVersion, ParticipationSchemaVersion)
		}
		rows = append(rows, row)
	}
	return rows, problems, nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == utf8.RuneError })
}

// quote is a value as an error message shows it: quoted, and cut short when long.
func quote(s string) string {
	const most = 40
	if utf8.RuneCountInString(s) > most {
		s = string([]rune(s)[:most]) + "…"
	}
	return fmt.Sprintf("%q", s)
}
