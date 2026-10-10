package servers

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/store"
)

// BanEntry is one ban as the server holds it, with the hub's record of it when the hub has one.
type BanEntry struct {
	drivers.Ban
	// Hub is the hub's active ban for the identity; nil for a ban only the server knows (one made
	// on the server's own console, or by RCON before the hub adopted the list).
	Hub *store.Ban
}

// adopt takes over a server's ban list the first time the hub writes it: the identities the
// server's configuration bans are imported as hub bans, so nothing a host banned before is
// dropped when the hub writes its list.
func (m *Moderation) adopt(ctx context.Context, serverID string, drv drivers.ExternalReachable) error {
	done, err := m.bans.BanListAdopted(ctx, m.orgID, serverID)
	if err != nil || done {
		return err
	}
	doc, err := drv.Config(ctx)
	if err != nil {
		return err
	}
	imported := make([]store.Ban, 0, len(doc.Bans))
	for _, b := range doc.Bans {
		imported = append(imported, store.Ban{Provider: b.Provider, Subject: b.Subject})
	}
	adopted, err := m.bans.AdoptBanList(ctx, m.orgID, serverID, imported, m.now().UTC())
	if err != nil {
		return err
	}
	if adopted {
		m.logger.Info("adopted the server's ban list; the hub writes it from now on", "server", serverID, "imported", len(imported))
	}
	return nil
}

// hubList is the identities of a server's active hub bans.
func (m *Moderation) hubList(ctx context.Context, serverID string) ([]drivers.Identity, error) {
	active, err := m.bans.ActiveBans(ctx, m.orgID, serverID)
	if err != nil {
		return nil, err
	}
	out := make([]drivers.Identity, 0, len(active))
	for _, b := range active {
		out = append(out, drivers.Identity{Provider: b.Provider, Subject: b.Subject})
	}
	return out, nil
}

// hubBan bans or unbans through the hub's list, written into the server's configuration. Both
// directions fail toward banned: a ban is recorded before the server is written and rolled back
// if the server refuses; an unban writes the server first and lifts the record after, so a lift
// that fails is re-applied as a ban by the next write. The server's own ban route runs too, best
// effort: it kicks a player who is on (a configuration ban takes effect at their next join) and
// clears an RCON-made ban.
func (m *Moderation) hubBan(ctx context.Context, serverID string, drv drivers.ExternalReachable, caps []string, a Action, auditID int64) error {
	now := m.now().UTC()
	switch a.Kind {
	case ActionBan:
		_, err := m.bans.AddBan(ctx, store.Ban{OrganizationID: m.orgID, ServerID: serverID, Provider: a.Player.Provider, Subject: a.Player.Subject,
			Reason: a.Reason, BannedAt: now, BanAuditID: &auditID})
		added := err == nil
		if err != nil && !errors.Is(err, store.ErrBanExists) {
			return err
		}
		list, err := m.hubList(ctx, serverID)
		if err != nil {
			return err
		}
		if _, err := drv.SetConfigBans(ctx, list); err != nil {
			if added {
				if lerr := m.bans.LiftBan(ctx, m.orgID, serverID, a.Player.Provider, a.Player.Subject, nil, m.now().UTC()); lerr != nil {
					m.logger.Error("the server refused the ban and the hub's record could not be rolled back; the next write will send it",
						"server", serverID, "audit_id", auditID, "error", lerr.Error())
				}
			}
			return err
		}
		if slices.Contains(caps, drivers.CapBan) {
			if err := drv.Ban(ctx, a.Player, a.Reason); err != nil && !errors.Is(err, drivers.ErrPlayerNotFound) {
				m.logger.Warn("banned in the configuration; the immediate RCON ban failed", "server", serverID, "audit_id", auditID, "error", err.Error())
			}
		}
		return nil

	case ActionUnban:
		active, err := m.bans.ActiveBans(ctx, m.orgID, serverID)
		if err != nil {
			return err
		}
		had := false
		var rest []drivers.Identity
		for _, b := range active {
			if b.Provider == a.Player.Provider && b.Subject == a.Player.Subject {
				had = true
				continue
			}
			rest = append(rest, drivers.Identity{Provider: b.Provider, Subject: b.Subject})
		}
		if had {
			if _, err := drv.SetConfigBans(ctx, rest); err != nil {
				return err
			}
			if err := m.bans.LiftBan(ctx, m.orgID, serverID, a.Player.Provider, a.Player.Subject, &auditID, m.now().UTC()); err != nil {
				m.logger.Error("unbanned on the server; the hub's record could not be lifted, so the next write bans again", "server", serverID, "audit_id", auditID, "error", err.Error())
				return err
			}
		}
		rcon := false
		if slices.Contains(caps, drivers.CapUnban) {
			switch err := drv.Unban(ctx, a.Player); {
			case err == nil:
				rcon = true
			case errors.Is(err, drivers.ErrBanNotFound):
			case had:
				m.logger.Warn("unbanned in the configuration; the RCON unban failed", "server", serverID, "audit_id", auditID, "error", err.Error())
			default:
				return err
			}
		}
		if !had && !rcon {
			return drivers.ErrBanNotFound
		}
		return nil
	}
	return fmt.Errorf("%w: %s is not a ban", ErrInvalidModeration, a.Kind)
}

// Bans are a server's bans with the hub's records: the server's own view when it offers one, and
// any hub ban it does not show. Not audited (a read).
func (m *Moderation) Bans(ctx context.Context, server string) ([]BanEntry, error) {
	srv, err := m.svc.Server(ctx, server)
	if err != nil {
		return nil, err
	}
	drv, caps, ok := m.src.Driver(srv.ID)
	if !ok || caps == nil {
		return nil, ErrNotObserved
	}
	active, err := m.bans.ActiveBans(ctx, m.orgID, srv.ID)
	if err != nil {
		return nil, err
	}
	hub := map[drivers.Identity]*store.Ban{}
	for i := range active {
		hub[drivers.Identity{Provider: active[i].Provider, Subject: active[i].Subject}] = &active[i]
	}
	var out []BanEntry
	if slices.Contains(caps, drivers.CapBans) {
		served, err := drv.Bans(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range served {
			e := BanEntry{Ban: b, Hub: hub[b.Identity]}
			delete(hub, b.Identity)
			out = append(out, e)
		}
	} else if len(active) == 0 {
		return nil, fmt.Errorf("%w: %s", drivers.ErrNotSupported, drivers.CapBans)
	}
	for _, b := range active { // the hub's bans the server did not show, in ban order
		id := drivers.Identity{Provider: b.Provider, Subject: b.Subject}
		if h, left := hub[id]; left {
			out = append(out, BanEntry{Ban: drivers.Ban{Identity: id, At: h.BannedAt, By: "hub", Reason: h.Reason}, Hub: h})
		}
	}
	return out, nil
}
