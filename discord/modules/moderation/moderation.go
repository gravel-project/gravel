package moderation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/omit"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Config is how the module runs.
type Config struct {
	// Command is the slash command's name (bot.yaml moderation.command).
	Command string
	// AccountURL is the hub's account page, where a member links their Discord account; it is
	// named in a refusal ("" leaves it out).
	AccountURL string
}

// Module is the moderation command.
type Module struct {
	hub     Hub
	cfg     Config
	pending *pending
	now     func() time.Time
	logger  *slog.Logger
	actions *prometheus.CounterVec
}

// New builds the module over the hub's API as the bot's app, which needs servers:read (who is
// on) and servers:moderate.
func New(hc *hubclient.Client, cfg Config) *Module {
	return NewWithHub(clientHub{hc: hc}, cfg)
}

// NewWithHub builds the module over any Hub (tests).
func NewWithHub(hub Hub, cfg Config) *Module {
	return &Module{hub: hub, cfg: cfg, pending: newPending(), now: time.Now, logger: slog.Default(),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_bot_moderation_actions_total",
			Help: "Moderation commands by action and result: ok, refused (the hub said no), failed, cancelled, expired.",
		}, []string{"action", "result"})}
}

// Name is the module's name.
func (*Module) Name() string { return "moderation" }

// Register adds the command and its confirmation buttons.
func (m *Module) Register(r *bot.Registry) error {
	m.logger = r.Logger()
	if err := r.Metrics().Register(m.actions); err != nil {
		return err
	}
	server := discord.ApplicationCommandOptionString{Name: "server", Description: "The server's id; needed only when there is more than one"}
	player := func(desc string) discord.ApplicationCommandOptionString {
		return discord.ApplicationCommandOptionString{Name: "player", Description: desc, Required: true}
	}
	text := func(name, desc string, required bool) discord.ApplicationCommandOptionString {
		return discord.ApplicationCommandOptionString{Name: name, Description: desc, Required: required, MaxLength: omit.Ptr(MaxText)}
	}
	perms := discord.PermissionModerateMembers
	cmd := discord.SlashCommandCreate{
		Name:                     m.cfg.Command,
		Description:              "Moderate the community's game servers through the hub (staff)",
		DefaultMemberPermissions: omit.NewPtr(perms),
		Contexts:                 []discord.InteractionContextType{discord.InteractionContextTypeGuild},
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{Name: Kick, Description: "Kick a player who is on",
				Options: []discord.ApplicationCommandOption{player("Their in-game name, or SteamID64"), text("reason", "Why", true), server}},
			discord.ApplicationCommandOptionSubCommand{Name: Ban, Description: "Ban a player, on or not",
				Options: []discord.ApplicationCommandOption{player("Their in-game name if they're on, else their SteamID64"), text("reason", "Why", true), server}},
			discord.ApplicationCommandOptionSubCommand{Name: Unban, Description: "Lift a ban",
				Options: []discord.ApplicationCommandOption{player("Their SteamID64"), text("reason", "Why (optional)", false), server}},
			discord.ApplicationCommandOptionSubCommand{Name: Message, Description: "Message one player in game",
				Options: []discord.ApplicationCommandOption{player("Their in-game name, or SteamID64"), text("text", "The message", true), server}},
			discord.ApplicationCommandOptionSubCommand{Name: Broadcast, Description: "Message everyone on the server",
				Options: []discord.ApplicationCommandOption{text("text", "The message", true), server}},
		},
	}
	if err := r.SlashCommand(cmd, m.command); err != nil {
		return err
	}
	r.Component("/moderation/confirm/{id}", m.confirm)
	r.Component("/moderation/cancel/{id}", m.cancel)
	return nil
}

func (m *Module) command(e *handler.CommandEvent) error {
	data := e.SlashCommandInteractionData()
	kind := ""
	if data.SubCommandName != nil {
		kind = *data.SubCommandName
	}
	if !validKind(kind) {
		return e.CreateMessage(ephemeral("Pick one of kick, ban, unban, message or broadcast."))
	}
	if err := e.DeferCreateMessage(true); err != nil {
		return err
	}
	text := data.String("reason")
	if kind == Message || kind == Broadcast {
		text = data.String("text")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := Plan(ctx, m.hub, Request{Kind: kind, Server: data.String("server"), Player: data.String("player"), Text: text})
	if err != nil {
		_, uerr := e.UpdateInteractionResponse(discord.MessageUpdate{Content: omit.Ptr(planError(err)), AllowedMentions: &discord.AllowedMentions{}})
		return uerr
	}
	id, err := m.pending.put(a, e.User().ID.String(), m.now())
	if err != nil {
		return err
	}
	_, err = e.UpdateInteractionResponse(discord.MessageUpdate{
		Content:         omit.Ptr(Question(a)),
		AllowedMentions: &discord.AllowedMentions{},
		Components: &[]discord.LayoutComponent{discord.NewActionRow(
			discord.NewDangerButton("Confirm", "/moderation/confirm/"+id),
			discord.NewSecondaryButton("Cancel", "/moderation/cancel/"+id),
		)},
	})
	return err
}

func (m *Module) confirm(e *handler.ComponentEvent) error {
	a, err := m.pending.take(e.Vars["id"], e.User().ID.String(), m.now())
	if err != nil {
		if errors.Is(err, errExpired) {
			m.actions.WithLabelValues("unknown", "expired").Inc()
		}
		return e.CreateMessage(ephemeral(err.Error()))
	}
	if err := e.DeferUpdateMessage(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = m.hub.Do(ctx, a, &hubv1.Actor{Provider: "discord", Subject: e.User().ID.String()})
	m.actions.WithLabelValues(a.Kind, outcome(err)).Inc()
	if err != nil && !isAnswer(err) {
		m.logger.Error("moderation: the hub failed", "action", a.Kind, "server", a.ServerID, "error", err.Error())
	}
	_, uerr := e.UpdateInteractionResponse(discord.MessageUpdate{Content: omit.Ptr(Result(a, err, m.cfg.AccountURL)), Components: &[]discord.LayoutComponent{}, AllowedMentions: &discord.AllowedMentions{}})
	return uerr
}

func (m *Module) cancel(e *handler.ComponentEvent) error {
	a, err := m.pending.take(e.Vars["id"], e.User().ID.String(), m.now())
	if errors.Is(err, errNotYous) {
		return e.CreateMessage(ephemeral(err.Error()))
	}
	if err == nil {
		m.actions.WithLabelValues(a.Kind, "cancelled").Inc()
	}
	return e.UpdateMessage(discord.MessageUpdate{Content: omit.Ptr("Cancelled; nothing was sent."), Components: &[]discord.LayoutComponent{}})
}

// outcome is a result's metric label.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case isAnswer(err):
		return "refused"
	}
	return "failed"
}

// isAnswer reports whether an error is the hub's answer about the action (not a failure to ask).
func isAnswer(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeInvalidArgument, connect.CodeUnavailable:
		return true
	}
	return false
}

func planError(err error) string {
	if errors.Is(err, ErrInput) {
		return "Nothing done: " + err.Error()[len(ErrInput.Error())+2:] + "."
	}
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied:
		return "Nothing done: the bot can't read who's on (its app needs servers:read). Tell an admin."
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return "Nothing done: the hub isn't answering. Try again in a minute."
	}
	return "Nothing done: something went wrong reaching the hub. Try again, or use the web."
}

func ephemeral(s string) discord.MessageCreate {
	return discord.MessageCreate{Content: s, Flags: discord.MessageFlagEphemeral, AllowedMentions: &discord.AllowedMentions{}}
}

// clientHub is Hub over the hub's Connect API as the bot's app.
type clientHub struct{ hc *hubclient.Client }

func (c clientHub) Games(ctx context.Context) ([]*hubv1.Game, error) {
	resp, err := c.hc.Servers().ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGames(), nil
}

func (c clientHub) Servers(ctx context.Context) ([]*hubv1.Server, error) {
	resp, err := c.hc.Servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetServers(), nil
}

func (c clientHub) Players(ctx context.Context, serverID string) ([]*hubv1.ServerPlayer, error) {
	resp, err := c.hc.Servers().ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: serverID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPlayers(), nil
}

func (c clientHub) Do(ctx context.Context, a Action, by *hubv1.Actor) error {
	mod := c.hc.Moderation()
	var err error
	switch a.Kind {
	case Kick:
		_, err = mod.KickPlayer(ctx, connect.NewRequest(&hubv1.KickPlayerRequest{ServerId: a.ServerID, Subject: a.Subject, Reason: a.Text, OnBehalfOf: by}))
	case Ban:
		_, err = mod.BanPlayer(ctx, connect.NewRequest(&hubv1.BanPlayerRequest{ServerId: a.ServerID, Subject: a.Subject, Reason: a.Text, OnBehalfOf: by}))
	case Unban:
		_, err = mod.UnbanPlayer(ctx, connect.NewRequest(&hubv1.UnbanPlayerRequest{ServerId: a.ServerID, Subject: a.Subject, Reason: a.Text, OnBehalfOf: by}))
	case Message:
		_, err = mod.MessagePlayer(ctx, connect.NewRequest(&hubv1.MessagePlayerRequest{ServerId: a.ServerID, Subject: a.Subject, Message: a.Text, OnBehalfOf: by}))
	case Broadcast:
		_, err = mod.Broadcast(ctx, connect.NewRequest(&hubv1.BroadcastRequest{ServerId: a.ServerID, Message: a.Text, OnBehalfOf: by}))
	}
	return err
}
