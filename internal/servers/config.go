package servers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/store"
)

// configTarget is a server ready for a configuration call: its driver, which must let the hub
// write, the hub's ban list (adopted first) and its game's bands.
func (m *Moderation) configTarget(ctx context.Context, server string, need string) (Server, drivers.ExternalReachable, drivers.ConfigDraft, error) {
	srv, err := m.svc.Server(ctx, server)
	if err != nil {
		return Server{}, nil, drivers.ConfigDraft{}, err
	}
	drv, caps, ok := m.src.Driver(srv.ID)
	if !ok || caps == nil {
		return Server{}, nil, drivers.ConfigDraft{}, ErrNotObserved
	}
	if !slices.Contains(caps, need) {
		return Server{}, nil, drivers.ConfigDraft{}, fmt.Errorf("%w: %s", drivers.ErrNotSupported, need)
	}
	var draft drivers.ConfigDraft
	if need == drivers.CapConfigWrite {
		if err := m.adopt(ctx, srv.ID, drv); err != nil {
			return Server{}, nil, drivers.ConfigDraft{}, err
		}
		if draft.Bans, err = m.hubList(ctx, srv.ID); err != nil {
			return Server{}, nil, drivers.ConfigDraft{}, err
		}
		if draft.Bans == nil {
			draft.Bans = []drivers.Identity{} // adopted and empty: write an empty list, not the server's
		}
		games, err := m.svc.Games(ctx)
		if err != nil {
			return Server{}, nil, drivers.ConfigDraft{}, err
		}
		for _, g := range games {
			if g.ID == srv.Game {
				for _, b := range g.Bands {
					draft.Bands = append(draft.Bands, drivers.Band{Section: b.Section, Key: b.Key, Min: b.Min, Max: b.Max})
				}
			}
		}
	}
	return srv, drv, draft, nil
}

// Config is a server's configuration, redacted.
func (m *Moderation) Config(ctx context.Context, server string) (drivers.ConfigDocument, error) {
	_, drv, _, err := m.configTarget(ctx, server, drivers.CapConfigRead)
	if err != nil {
		return drivers.ConfigDocument{}, err
	}
	return drv.Config(ctx)
}

// PlanConfig merges a deployment's document with what the hub owns, checks the bands and has the
// server validate it. Nothing is written and nothing is audited.
func (m *Moderation) PlanConfig(ctx context.Context, server, text string) (drivers.ConfigPlan, error) {
	if err := validateConfigText(text); err != nil {
		return drivers.ConfigPlan{}, err
	}
	_, drv, draft, err := m.configTarget(ctx, server, drivers.CapConfigWrite)
	if err != nil {
		return drivers.ConfigPlan{}, err
	}
	draft.Text = text
	return drv.PlanConfig(ctx, draft)
}

// ApplyConfig writes a deployment's document, merged, if the server is still at the planned
// revision. It is audited like a moderation call: the row names the revision, and the reason is
// the caller's (a commit, a change request).
func (m *Moderation) ApplyConfig(ctx context.Context, actor Actor, server, text, revision, reason string) (int64, drivers.ConfigResult, error) {
	var errs []string
	if err := validateConfigText(text); err != nil {
		errs = append(errs, strings.TrimPrefix(err.Error(), ErrInvalidModeration.Error()+": "))
	}
	if strings.TrimSpace(revision) == "" {
		errs = append(errs, "the revision the document was planned against is required")
	}
	if utf8.RuneCountInString(reason) > MaxReasonLength {
		errs = append(errs, fmt.Sprintf("the reason is over %d characters", MaxReasonLength))
	}
	if err := validate(actor, Action{Kind: ActionBroadcast, Message: "-"}); err != nil { // the actor rules alone
		errs = append(errs, strings.TrimPrefix(err.Error(), ErrInvalidModeration.Error()+": "))
	}
	if len(errs) > 0 {
		return 0, drivers.ConfigResult{}, fmt.Errorf("%w: %s", ErrInvalidModeration, strings.Join(errs, "; "))
	}
	srv, drv, draft, err := m.configTarget(ctx, server, drivers.CapConfigWrite)
	if err != nil {
		return 0, drivers.ConfigResult{}, err
	}
	draft.Text = text
	detail, err := json.Marshal(map[string]string{"revision": revision})
	if err != nil {
		return 0, drivers.ConfigResult{}, err
	}
	entry := store.AuditEntry{OrganizationID: m.orgID, At: m.now().UTC(), ActorUserID: actor.UserID, ActorAppID: actor.AppID,
		ServerID: srv.ID, Action: ActionApplyConfig, Reason: reason, Detail: detail, RequestID: actor.RequestID}
	if o := actor.OnBehalfOf; o != nil {
		entry.OnBehalfProvider, entry.OnBehalfSubject = o.Provider, o.Subject
	}
	id, err := m.st.InsertAuditEntry(ctx, entry)
	if err != nil {
		return 0, drivers.ConfigResult{}, err
	}
	res, callErr := drv.ApplyConfig(ctx, draft, revision)
	m.finish(ctx, srv.ID, ActionApplyConfig, id, actor.RequestID, "", callErr)
	return id, res, callErr
}

func validateConfigText(text string) error {
	switch {
	case strings.TrimSpace(text) == "":
		return fmt.Errorf("%w: the document is empty", ErrInvalidModeration)
	case len(text) > MaxConfigBytes:
		return fmt.Errorf("%w: the document is over %d bytes", ErrInvalidModeration, MaxConfigBytes)
	case !utf8.ValidString(text):
		return fmt.Errorf("%w: the document is not UTF-8", ErrInvalidModeration)
	}
	return nil
}
