// Package stock is gravel's stock set of bot modules, as a bot.yaml enables them: core
// (/whoami, /link) always, role sync, Linked Roles, the server cards, the moderation command, the
// mod log, seeding, the presence card, the session recaps, the go-live posts and the Steam news when
// their sections turn them on. A host's bot starts from this set and adds its own modules, so a module gravel adds
// later reaches every host bot at its next gravel upgrade (ADR-0008 §1).
package stock

import (
	"strings"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/core"
	"github.com/gravel-project/gravel/discord/modules/linkedroles"
	"github.com/gravel-project/gravel/discord/modules/moderation"
	"github.com/gravel-project/gravel/discord/modules/modlog"
	"github.com/gravel-project/gravel/discord/modules/news"
	"github.com/gravel-project/gravel/discord/modules/presence"
	"github.com/gravel-project/gravel/discord/modules/recaps"
	"github.com/gravel-project/gravel/discord/modules/rolesync"
	"github.com/gravel-project/gravel/discord/modules/seeding"
	"github.com/gravel-project/gravel/discord/modules/servercards"
	"github.com/gravel-project/gravel/discord/modules/streams"
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
		mods = append(mods, servercards.New(hub, servercards.Config{Interval: cfg.ServerCards.Interval, Placeholder: servercards.PlaceholderURL(cfg.Hub.PublicURL)}))
	}
	if cfg.Moderation.Enabled {
		account := ""
		if cfg.Hub.PublicURL != "" {
			account = strings.TrimSuffix(cfg.Hub.PublicURL, "/") + "/account"
		}
		mods = append(mods, moderation.New(hub, moderation.Config{Command: cfg.Moderation.Command, AccountURL: account}))
	}
	if cfg.ModLog.Enabled {
		mods = append(mods, modlog.New(hub, modlog.Config{Interval: cfg.ModLog.Interval}))
	}
	if cfg.Seeding.Enabled {
		mods = append(mods, seeding.New(hub, seeding.Config{Interval: cfg.Seeding.Interval}))
	}
	if cfg.Presence.Enabled {
		mods = append(mods, presence.New(hub, presence.Config{Interval: cfg.Presence.Interval}))
	}
	if cfg.Recaps.Enabled {
		mods = append(mods, recaps.New(hub, recaps.Config{Interval: cfg.Recaps.Interval, PublicURL: cfg.Hub.PublicURL}))
	}
	if cfg.Streams.Enabled {
		hc := streams.HTTPClient()
		var platforms []streams.Platform
		if a := cfg.Streams.Twitch; a.Set() {
			platforms = append(platforms, streams.NewTwitch(streams.App{ClientID: a.ClientID, ClientSecret: a.ClientSecret}, hc))
		}
		if a := cfg.Streams.Kick; a.Set() {
			platforms = append(platforms, streams.NewKick(streams.App{ClientID: a.ClientID, ClientSecret: a.ClientSecret}, hc))
		}
		mods = append(mods, streams.New(hub, streams.Config{Interval: cfg.Streams.Interval, Platforms: platforms}))
	}
	if cfg.News.Enabled {
		mods = append(mods, news.New(hub, news.Config{Interval: cfg.News.Interval}))
	}
	return mods
}
