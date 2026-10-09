// Package core is the stock module every gravel bot carries: /whoami, which tells a member what
// the hub knows about them, and /link, which points at the account page.
package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Module is the core module.
type Module struct {
	hub       *hubclient.Client
	publicURL string
}

// New builds the module. publicURL is the hub's origin members use, for the links in replies.
func New(hub *hubclient.Client, publicURL string) *Module {
	return &Module{hub: hub, publicURL: strings.TrimSuffix(publicURL, "/")}
}

// Name is the module's name.
func (m *Module) Name() string { return "core" }

// Register adds /whoami and /link.
func (m *Module) Register(r *bot.Registry) error {
	if err := r.SlashCommand(discord.SlashCommandCreate{Name: "whoami", Description: "What the hub knows about you: your linked accounts"}, m.whoami); err != nil {
		return err
	}
	return r.SlashCommand(discord.SlashCommandCreate{Name: "link", Description: "Where to link your game accounts"}, m.link)
}

// Replies are ephemeral: an account is the member's business.
func ephemeral(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral}
}

func (m *Module) link(e *handler.CommandEvent) error {
	return e.CreateMessage(ephemeral(LinkMessage(m.publicURL)))
}

func (m *Module) whoami(e *handler.CommandEvent) error {
	ctx, cancel := context.WithTimeout(e.Ctx, 2500*time.Millisecond) // Discord gives three seconds for the first answer
	defer cancel()
	resp, err := m.hub.Identity().LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: e.User().ID.String()}))
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound:
		return e.CreateMessage(ephemeral(NotLinkedMessage(m.publicURL)))
	case err != nil:
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return fmt.Errorf("whoami: hub: %w", cerr)
		}
		return fmt.Errorf("whoami: hub: %w", err)
	}
	return e.CreateMessage(ephemeral(WhoamiMessage(resp.Msg.GetUser(), m.publicURL)))
}

// LinkMessage is /link's reply.
func LinkMessage(publicURL string) string {
	return "Link your game accounts on your account page: " + publicURL + "/account"
}

// NotLinkedMessage is /whoami's reply for a Discord account the hub does not know.
func NotLinkedMessage(publicURL string) string {
	return "The hub doesn't know this Discord account yet. Log in with Discord at " + publicURL + "/login and link your game accounts there."
}

// WhoamiMessage is /whoami's reply for a known member: their name and each linked account.
func WhoamiMessage(u *hubv1.User, publicURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**", u.GetDisplayName())
	if u.GetOwner() {
		b.WriteString(" (owner)")
	}
	b.WriteString("\n")
	for _, id := range u.GetIdentities() {
		name := id.GetDisplayName()
		if name == "" {
			name = id.GetSubject()
		}
		fmt.Fprintf(&b, "• %s: %s", providerLabel(id.GetProvider()), name)
		if at := id.GetLinkedAt(); at != nil {
			fmt.Fprintf(&b, " (linked %s)", at.AsTime().UTC().Format("2006-01-02"))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Manage them at %s/account", publicURL)
	return b.String()
}

func providerLabel(p string) string {
	switch p {
	case "discord":
		return "Discord"
	case "steam":
		return "Steam"
	default:
		return p
	}
}
