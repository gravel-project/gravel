package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/store"
)

// Moderation actions, as the audit log names them.
const (
	ActionKick       = "kick"
	ActionBan        = "ban"
	ActionUnban      = "unban"
	ActionMessage    = "message"
	ActionBroadcast  = "broadcast"
	ActionMovePlayer = "move_player"
)

// Outcomes the audit log records beside the monitor's failure states (StateUnreachable,
// StateCredentialRefused, StateCredentialMissing, StateRateLimited, StateError). Like them, an
// outcome is a word: the error's text, which can name the control address, goes to the log.
const (
	OutcomeOK             = "ok"
	OutcomePlayerNotFound = "player_not_found" // the player is not on the server
	OutcomeBanNotFound    = "ban_not_found"    // an unban with no ban to lift
	OutcomeRejected       = "rejected"         // the server refused the request as invalid
	OutcomeNotAvailable   = "not_available"    // the server stopped offering the action mid-call
	// OutcomeMovedNotRespawned is a move that worked followed by a respawn that did not.
	OutcomeMovedNotRespawned = "moved_not_respawned"
	OutcomeConflict          = "conflict" // the configuration moved since it was planned
)

// ActionApplyConfig is a configuration write, audited like a moderation call.
const ActionApplyConfig = "apply_config"

// MaxConfigBytes is the largest document the hub sends (War Dogs advertises 64 KiB).
const MaxConfigBytes = 64 << 10

// Limits on what a moderator sends. The game may cap lower (War Dogs: 256 characters for a
// message) and its refusal is OutcomeRejected.
const (
	MaxReasonLength   = 512
	MaxMessageLength  = 1024
	MaxTeamLength     = 64
	MaxIdentityLength = 128
	// MaxAuditPage is the most entries one page of the audit log holds.
	MaxAuditPage = 200
)

// Errors.
var (
	// ErrInvalidModeration is a moderation request that is missing or over-long somewhere; the
	// message says where. Nothing was sent or recorded.
	ErrInvalidModeration = errors.New("servers: invalid moderation request")
	// ErrNotObserved is a server the hub has not reached yet, so it does not know what the
	// server offers. Nothing was sent or recorded.
	ErrNotObserved = errors.New("servers: the hub has not reached the server yet")
	// ErrRespawnFailed is a move that worked followed by a respawn that did not; it wraps the
	// respawn's error.
	ErrRespawnFailed = errors.New("servers: the player was moved, but the respawn failed")
)

// Actor is who asks for a moderation action: a logged-in user or an app, exactly one.
type Actor struct {
	UserID *uuid.UUID
	AppID  *uuid.UUID
	// OnBehalfOf is the person an app acts for (a Discord moderator behind a bot command), as
	// the app asserts it. A user acts as themselves and has none.
	OnBehalfOf *drivers.Identity
	RequestID  string
}

// Action is one moderation call on a server.
type Action struct {
	Server string
	Kind   string
	// Player is the player acted on; empty for a broadcast. An empty provider is the game's.
	Player  drivers.Identity
	Reason  string
	Message string
	Team    string
	// Respawn kills the player after a move so they respawn on the new team (War Dogs' official
	// console does this).
	Respawn bool
}

// AuditStore is what moderation needs from the database.
type AuditStore interface {
	InsertAuditEntry(ctx context.Context, e store.AuditEntry) (int64, error)
	FinishAuditEntry(ctx context.Context, orgID uuid.UUID, id int64, outcome string, at time.Time) error
	ListAuditEntries(ctx context.Context, orgID uuid.UUID, f store.AuditFilter) ([]store.AuditEntry, error)
}

// BanStore is the hub's ban list.
type BanStore interface {
	BanListAdopted(ctx context.Context, orgID uuid.UUID, serverID string) (bool, error)
	AdoptBanList(ctx context.Context, orgID uuid.UUID, serverID string, imported []store.Ban, at time.Time) (bool, error)
	ActiveBans(ctx context.Context, orgID uuid.UUID, serverID string) ([]store.Ban, error)
	AddBan(ctx context.Context, b store.Ban) (int64, error)
	LiftBan(ctx context.Context, orgID uuid.UUID, serverID, provider, subject string, liftAuditID *int64, at time.Time) error
}

// DriverSource hands out the driver the monitor runs for a server, with the capabilities it
// last read; ok is false for a server the monitor does not watch.
type DriverSource interface {
	Driver(id string) (drv drivers.ExternalReachable, capabilities []string, ok bool)
}

// Moderation performs the calls that change a server, moderation and configuration, through its
// driver, and records each in the audit log (ADR-0010 §5–6). The row is written before the call,
// so nothing is done unrecorded. It keeps the hub's ban list, which it writes into a server's
// configuration when the server lets the hub write it.
type Moderation struct {
	svc    *Service
	src    DriverSource
	st     AuditStore
	bans   BanStore
	orgID  uuid.UUID
	logger *slog.Logger
	now    func() time.Time
	total  *prometheus.CounterVec
}

// NewModeration builds the service and registers its metric.
func NewModeration(svc *Service, src DriverSource, st AuditStore, bans BanStore, orgID uuid.UUID, reg prometheus.Registerer, logger *slog.Logger) *Moderation {
	m := &Moderation{
		svc: svc, src: src, st: st, bans: bans, orgID: orgID, logger: logger, now: time.Now,
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_moderation_actions_total", Help: "Moderation and configuration calls the hub made to a server, by server, action and outcome.",
		}, []string{"server", "action", "outcome"}),
	}
	reg.MustRegister(m.total)
	return m
}

// Do validates an action, records it, calls the driver and records the outcome. It returns the
// audit entry's id and the driver's error, if any.
func (m *Moderation) Do(ctx context.Context, actor Actor, a Action) (int64, error) {
	srv, err := m.svc.Server(ctx, a.Server)
	if err != nil {
		return 0, err
	}
	if a.Player.Provider == "" && a.Kind != ActionBroadcast {
		a.Player.Provider = m.svc.specs[srv.Game].IdentityProvider
	}
	if err := validate(actor, a); err != nil {
		return 0, err
	}
	drv, caps, ok := m.src.Driver(srv.ID)
	if !ok || caps == nil {
		return 0, ErrNotObserved
	}
	for _, c := range needs(a, caps) {
		if !slices.Contains(caps, c) {
			return 0, fmt.Errorf("%w: %s", drivers.ErrNotSupported, c)
		}
	}
	hubBans := (a.Kind == ActionBan || a.Kind == ActionUnban) && slices.Contains(caps, drivers.CapConfigWrite)
	if hubBans {
		if err := m.adopt(ctx, srv.ID, drv); err != nil {
			return 0, err
		}
	}

	entry := store.AuditEntry{
		OrganizationID: m.orgID, At: m.now().UTC(), ActorUserID: actor.UserID, ActorAppID: actor.AppID,
		ServerID: srv.ID, Action: a.Kind, TargetProvider: a.Player.Provider, TargetSubject: a.Player.Subject,
		Reason: a.Reason, RequestID: actor.RequestID,
	}
	if o := actor.OnBehalfOf; o != nil {
		entry.OnBehalfProvider, entry.OnBehalfSubject = o.Provider, o.Subject
	}
	if entry.Detail, err = detail(a); err != nil {
		return 0, err
	}
	id, err := m.st.InsertAuditEntry(ctx, entry)
	if err != nil {
		return 0, err // not recorded, so not done
	}

	var callErr error
	if hubBans {
		callErr = m.hubBan(ctx, srv.ID, drv, caps, a, id)
	} else {
		callErr = m.call(ctx, drv, a)
	}
	outcome := ""
	if a.Kind == ActionMovePlayer && errors.Is(callErr, ErrRespawnFailed) {
		outcome = OutcomeMovedNotRespawned
	}
	m.finish(ctx, srv.ID, a.Kind, id, actor.RequestID, a.Player.Provider+":"+a.Player.Subject, callErr, outcome)
	return id, callErr
}

// finish records a call's outcome (a word: the given one, or the error's), counts it and logs it.
// The outcome is recorded even when the caller has gone: the call was made.
func (m *Moderation) finish(ctx context.Context, serverID, action string, id int64, requestID, target string, callErr error, outcome ...string) {
	word := outcomeOf(callErr)
	if len(outcome) > 0 && outcome[0] != "" {
		word = outcome[0]
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.st.FinishAuditEntry(fctx, m.orgID, id, word, m.now().UTC()); err != nil {
		m.logger.Error("outcome not recorded; the audit entry stays unfinished", "audit_id", id, "outcome", word, "error", err.Error())
	}
	m.total.WithLabelValues(serverID, action, word).Inc()
	attrs := []any{"server", serverID, "action", action, "outcome", word, "audit_id", id, "request_id", requestID}
	if target != "" && target != ":" {
		attrs = append(attrs, "target", target)
	}
	if callErr != nil && word != OutcomePlayerNotFound && word != OutcomeBanNotFound {
		m.logger.Warn("server call failed", append(attrs, "error", callErr.Error())...)
	} else {
		m.logger.Info("server call", attrs...)
	}
}

func (m *Moderation) call(ctx context.Context, drv drivers.ExternalReachable, a Action) error {
	switch a.Kind {
	case ActionKick:
		return drv.Kick(ctx, a.Player, a.Reason)
	case ActionBan:
		return drv.Ban(ctx, a.Player, a.Reason)
	case ActionUnban:
		return drv.Unban(ctx, a.Player)
	case ActionMessage:
		return drv.Message(ctx, a.Player, a.Message)
	case ActionBroadcast:
		return drv.Broadcast(ctx, a.Message)
	case ActionMovePlayer:
		if err := drv.MovePlayer(ctx, a.Player, a.Team); err != nil {
			return err
		}
		if a.Respawn {
			if err := drv.Kill(ctx, a.Player); err != nil {
				return fmt.Errorf("%w: %w", ErrRespawnFailed, err)
			}
		}
		return nil
	}
	return fmt.Errorf("%w: unknown action %q", ErrInvalidModeration, a.Kind)
}

// Log is a page of the audit log, newest first: at most limit entries (MaxAuditPage when 0 or
// over it) before the id (0 = the newest), for one server or ("") all. more says whether older
// entries exist.
func (m *Moderation) Log(ctx context.Context, server string, before int64, limit int) (entries []store.AuditEntry, more bool, err error) {
	if limit <= 0 || limit > MaxAuditPage {
		limit = MaxAuditPage
	}
	entries, err = m.st.ListAuditEntries(ctx, m.orgID, store.AuditFilter{ServerID: server, BeforeID: before, Limit: limit + 1})
	if err != nil {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

// needs are the capabilities an action uses. A ban or an unban goes into the server's
// configuration when the server lets the hub write it (the hub's list), else through the
// server's own ban route, which bans only a player who is on.
func needs(a Action, caps []string) []string {
	switch a.Kind {
	case ActionKick:
		return []string{drivers.CapKick}
	case ActionBan:
		if slices.Contains(caps, drivers.CapConfigWrite) {
			return []string{drivers.CapConfigWrite}
		}
		return []string{drivers.CapBan}
	case ActionUnban:
		if slices.Contains(caps, drivers.CapConfigWrite) {
			return []string{drivers.CapConfigWrite}
		}
		return []string{drivers.CapUnban}
	case ActionMessage:
		return []string{drivers.CapMessage}
	case ActionBroadcast:
		return []string{drivers.CapBroadcast}
	case ActionMovePlayer:
		if a.Respawn {
			return []string{drivers.CapMovePlayer, drivers.CapKill}
		}
		return []string{drivers.CapMovePlayer}
	}
	return nil
}

func validate(actor Actor, a Action) error {
	var errs []string
	if (actor.UserID == nil) == (actor.AppID == nil) {
		errs = append(errs, "exactly one of a user and an app must ask")
	}
	if o := actor.OnBehalfOf; o != nil {
		if actor.AppID == nil {
			errs = append(errs, "on_behalf_of is an app's; a user acts as themselves")
		}
		if !text(o.Provider, MaxIdentityLength) || !text(o.Subject, MaxIdentityLength) {
			errs = append(errs, fmt.Sprintf("on_behalf_of needs a provider and a subject of at most %d characters", MaxIdentityLength))
		}
	}
	player := func() {
		if !text(a.Player.Subject, MaxIdentityLength) {
			errs = append(errs, "the player's subject (their id at the game's provider) is required")
		}
	}
	reason := func(required bool) {
		if required && strings.TrimSpace(a.Reason) == "" {
			errs = append(errs, "a reason is required")
		}
		if utf8.RuneCountInString(a.Reason) > MaxReasonLength {
			errs = append(errs, fmt.Sprintf("the reason is over %d characters", MaxReasonLength))
		}
	}
	message := func() {
		if !text(a.Message, MaxMessageLength) {
			errs = append(errs, fmt.Sprintf("a message of at most %d characters is required", MaxMessageLength))
		}
	}
	switch a.Kind {
	case ActionKick, ActionBan:
		player()
		reason(true)
	case ActionUnban:
		player()
		reason(false)
	case ActionMessage:
		player()
		message()
	case ActionBroadcast:
		message()
	case ActionMovePlayer:
		player()
		if !text(a.Team, MaxTeamLength) {
			errs = append(errs, fmt.Sprintf("a team of at most %d characters is required", MaxTeamLength))
		}
	default:
		errs = append(errs, fmt.Sprintf("unknown action %q", a.Kind))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidModeration, strings.Join(errs, "; "))
	}
	return nil
}

// text is a non-blank string of at most max characters.
func text(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}

// detail is what the audit row keeps beside the reason.
func detail(a Action) (json.RawMessage, error) {
	d := map[string]any{}
	switch a.Kind {
	case ActionMessage, ActionBroadcast:
		d["message"] = a.Message
	case ActionMovePlayer:
		d["team"] = a.Team
		d["respawn"] = a.Respawn
	}
	return json.Marshal(d)
}

func outcomeOf(err error) string {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, drivers.ErrPlayerNotFound):
		return OutcomePlayerNotFound
	case errors.Is(err, drivers.ErrBanNotFound):
		return OutcomeBanNotFound
	case errors.Is(err, drivers.ErrRejected):
		return OutcomeRejected
	case errors.Is(err, drivers.ErrNotSupported):
		return OutcomeNotAvailable
	case errors.Is(err, drivers.ErrConfigRejected), errors.Is(err, drivers.ErrInvalidConfig):
		return OutcomeRejected
	case errors.Is(err, drivers.ErrConfigConflict):
		return OutcomeConflict
	}
	return stateOf(err)
}
