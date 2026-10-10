// Package stock is gravel's stock set of bot modules, as a bot.yaml enables them: core
// (/whoami, /link) always, role sync, Linked Roles, the server cards and the moderation command
// when their sections turn them on. A host's
// bot starts from this set and adds its own modules, so a module gravel adds later reaches every
// host bot at its next gravel upgrade (ADR-0008 §1).
package stock

import (
	"strings"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/core"
	"github.com/gravel-project/gravel/discord/modules/linkedroles"
	"github.com/gravel-project/gravel/discord/modules/moderation"
	"github.com/gravel-project/gravel/discord/modules/rolesync"
	"github.com/gravel-project/gravel/discord/modules/servercards"
)

// Modules returns the stock modules the configuration enables, in registration order.
func Modules(cfg bot.Config, hub *hubclient.Client) []bot.Module {
	mods := []bot.Module{core.New(hub, cfg.Hub.PublicURL)}
	if cfg.RoleSync.Enabled {
		mods = append(mods, rolesync.New(hub, rolesync.Config{Interval: cfg.RoleSync.Interval, PollInterval: cfg.RoleSync.PollInterval, DryRun: cfg.RoleSync.DryRun}))
	}
	if cfg.LinkedRoles.Enabled {
		mods = append(mods, linkedroles.New())
	}
	if cfg.ServerCards.Enabled {
		mods = append(mods, servercards.New(hub, servercards.Config{Interval: cfg.ServerCards.Interval}))
	}
	if cfg.Moderation.Enabled {
		account := ""
		if cfg.Hub.PublicURL != "" {
			account = strings.TrimSuffix(cfg.Hub.PublicURL, "/") + "/account"
		}
		mods = append(mods, moderation.New(hub, moderation.Config{Command: cfg.Moderation.Command, AccountURL: account}))
	}
	return mods
}
