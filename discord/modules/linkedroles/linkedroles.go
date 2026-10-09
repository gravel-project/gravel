// Package linkedroles registers gravel's Linked Roles metadata schema (discord/rolemeta) with the
// bot's Discord application at start, so a guild can require "Steam linked" on a role in its
// settings. The member's values are written by the hub's /auth/discord/roles flow, with the
// member's own grant (ADR-0008 §4); the bot only declares the fields.
package linkedroles

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/rolemeta"
)

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetApplicationRoleConnectionMetadata(applicationID snowflake.ID, opts ...rest.RequestOpt) ([]discord.ApplicationRoleConnectionMetadata, error)
	UpdateApplicationRoleConnectionMetadata(applicationID snowflake.ID, newRecords []discord.ApplicationRoleConnectionMetadata, opts ...rest.RequestOpt) ([]discord.ApplicationRoleConnectionMetadata, error)
}

// Retry bounds: Discord unreachable at start is retried, never fatal.
const (
	firstBackoff = time.Second
	maxBackoff   = time.Minute
)

// Module is the Linked Roles module.
type Module struct {
	discord    Discord
	app        snowflake.ID
	logger     *slog.Logger
	registered prometheus.Gauge
	backoff    time.Duration
}

// New builds the module.
func New() *Module { return &Module{logger: slog.Default(), backoff: firstBackoff} }

// Name is the module's name.
func (m *Module) Name() string { return "linkedroles" }

// Register adds the job that registers the schema.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.app = r.ApplicationID()
	m.logger = r.Logger()
	m.registered = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_linked_roles_schema_registered", Help: "1 once the application carries the Linked Roles metadata schema."})
	if err := r.Metrics().Register(m.registered); err != nil {
		return fmt.Errorf("linkedroles: metrics: %w", err)
	}
	r.Job("register-metadata", m.Run)
	return nil
}

// Run registers the schema, retrying with backoff until it is in place or ctx is done.
func (m *Module) Run(ctx context.Context) error {
	wait := m.backoff
	for {
		changed, err := m.Sync(ctx)
		if err == nil {
			m.registered.Set(1)
			if changed {
				m.logger.InfoContext(ctx, "linked roles metadata registered", "records", len(rolemeta.Schema), "application_id", m.app.String())
			} else {
				m.logger.DebugContext(ctx, "linked roles metadata already registered")
			}
			return nil
		}
		m.logger.WarnContext(ctx, "linked roles metadata not registered, retrying", "error", err.Error(), "in", wait.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(wait*2, maxBackoff)
	}
}

// Sync puts the schema on the application unless it is there already, and reports whether it
// wrote it.
func (m *Module) Sync(ctx context.Context) (bool, error) {
	current, err := m.discord.GetApplicationRoleConnectionMetadata(m.app, rest.WithCtx(ctx))
	if err != nil {
		return false, fmt.Errorf("read the metadata: %w", err)
	}
	want := Records()
	if slices.EqualFunc(current, want, same) {
		return false, nil
	}
	if _, err := m.discord.UpdateApplicationRoleConnectionMetadata(m.app, want, rest.WithCtx(ctx)); err != nil {
		return false, fmt.Errorf("write the metadata: %w", err)
	}
	return true, nil
}

// Records is the schema in Discord's shape.
func Records() []discord.ApplicationRoleConnectionMetadata {
	out := make([]discord.ApplicationRoleConnectionMetadata, 0, len(rolemeta.Schema))
	for _, r := range rolemeta.Schema {
		out = append(out, discord.ApplicationRoleConnectionMetadata{
			Type: discord.ApplicationRoleConnectionMetadataType(r.Type), Key: r.Key, Name: r.Name, Description: r.Description,
		})
	}
	return out
}

func same(a, b discord.ApplicationRoleConnectionMetadata) bool {
	return a.Type == b.Type && a.Key == b.Key && a.Name == b.Name && a.Description == b.Description
}
