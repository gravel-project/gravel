// Package moderation is the moderation slash command (bot.yaml moderation.command, "/mod" by
// default): kick, ban, unban, message and broadcast, each confirmed by the moderator before it is
// sent. The bot never talks to a game server: every action is a call to the hub's
// ModerationService with the bot's app credential and on_behalf_of the Discord moderator, so the
// hub decides whether they may act (ADR-0013: the owner or a hub moderator) and its audit log
// records it, exactly as for the web, which stays the authoritative surface (principle 9). The
// command is hidden from members without Moderate Members; the hub's check is the one that counts.
package moderation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Actions /wd takes.
const (
	Kick      = "kick"
	Ban       = "ban"
	Unban     = "unban"
	Message   = "message"
	Broadcast = "broadcast"
)

// MaxText is the longest reason or message: the game caps messages at 256 characters.
const MaxText = 256

// ConfirmFor is how long a moderator has to confirm an action.
const ConfirmFor = 2 * time.Minute

// steamID is a SteamID64 (an individual account).
var steamID = regexp.MustCompile(`^7656119[0-9]{10}$`)

// looksLikeSubject reports whether what was typed is a player id at the game's identity provider
// rather than a name: a SteamID64 for Steam games; for others, one word of digits.
func looksLikeSubject(provider, s string) bool {
	if provider == "steam" {
		return steamID.MatchString(s)
	}
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// Hub is what the command needs from the hub.
type Hub interface {
	Games(ctx context.Context) ([]*hubv1.Game, error)
	Servers(ctx context.Context) ([]*hubv1.Server, error)
	Players(ctx context.Context, serverID string) ([]*hubv1.ServerPlayer, error)
	Do(ctx context.Context, a Action, onBehalf *hubv1.Actor) error
}

// Action is one moderation action, resolved and ready to confirm.
type Action struct {
	Kind       string
	ServerID   string
	ServerName string
	Subject    string // the player's id at the game's identity provider; empty for a broadcast
	Name       string // their in-game name, or their id when they aren't on
	Text       string // the reason, or the message
}

// Request is what the moderator typed.
type Request struct {
	Kind   string
	Server string // a server id or name; empty when there is only one
	Player string // an in-game name of someone on, or their id (a SteamID64)
	Text   string
}

// ErrInput is a request the command can't act on; its message is for the moderator.
var ErrInput = errors.New("moderation")

func inputf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInput}, a...)...)
}

// Plan resolves a request against the hub: which server, which player, and what will be sent.
func Plan(ctx context.Context, hub Hub, req Request) (Action, error) {
	a := Action{Kind: req.Kind, Text: strings.TrimSpace(req.Text)}
	switch req.Kind {
	case Kick, Ban, Message, Broadcast:
		if a.Text == "" {
			return Action{}, inputf("say why, or what to send")
		}
	case Unban:
	default:
		return Action{}, inputf("%q is not something this command does", req.Kind)
	}
	if utf8.RuneCountInString(a.Text) > MaxText {
		return Action{}, inputf("keep it to %d characters; the game cuts longer ones", MaxText)
	}
	srv, err := pickServer(ctx, hub, strings.TrimSpace(req.Server))
	if err != nil {
		return Action{}, err
	}
	a.ServerID, a.ServerName = srv.GetId(), srv.GetName()
	if req.Kind == Broadcast {
		return a, nil
	}
	player := strings.TrimSpace(req.Player)
	if player == "" {
		return Action{}, inputf("name the player")
	}
	provider := gameProvider(ctx, hub, srv.GetGameId())
	if looksLikeSubject(provider, player) {
		a.Subject, a.Name = player, player
		if req.Kind != Unban {
			if on, err := hub.Players(ctx, a.ServerID); err == nil {
				for _, p := range on {
					if p.GetSubject() == player {
						a.Name = p.GetName()
					}
				}
			}
		}
		return a, nil
	}
	if req.Kind == Unban {
		return Action{}, inputf("unban takes the player's id (a SteamID64); the ban list on the web shows it")
	}
	on, err := hub.Players(ctx, a.ServerID)
	if err != nil {
		return Action{}, err
	}
	p, err := pickPlayer(on, player, a.ServerName, req.Kind)
	if err != nil {
		return Action{}, err
	}
	a.Subject, a.Name = p.GetSubject(), p.GetName()
	return a, nil
}

// gameProvider is the identity provider a game keys its players by ("" when the hub doesn't say).
func gameProvider(ctx context.Context, hub Hub, gameID string) string {
	games, err := hub.Games(ctx)
	if err != nil {
		return ""
	}
	for _, g := range games {
		if g.GetId() == gameID {
			return g.GetIdentityProvider()
		}
	}
	return ""
}

func pickServer(ctx context.Context, hub Hub, want string) (*hubv1.Server, error) {
	all, err := hub.Servers(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, inputf("the hub has no servers")
	}
	if want == "" {
		if len(all) == 1 {
			return all[0], nil
		}
		return nil, inputf("name the server: %s", serverIDs(all))
	}
	for _, s := range all {
		if strings.EqualFold(s.GetId(), want) || strings.EqualFold(s.GetName(), want) {
			return s, nil
		}
	}
	return nil, inputf("no server %q; there are %s", want, serverIDs(all))
}

func serverIDs(all []*hubv1.Server) string {
	ids := make([]string, 0, len(all))
	for _, s := range all {
		ids = append(ids, s.GetId())
	}
	return strings.Join(ids, ", ")
}

// pickPlayer finds a player on the server by name: an exact match (ignoring case), else the one
// whose name contains what was typed.
func pickPlayer(on []*hubv1.ServerPlayer, want, server, kind string) (*hubv1.ServerPlayer, error) {
	var exact, partial []*hubv1.ServerPlayer
	for _, p := range on {
		switch {
		case strings.EqualFold(p.GetName(), want):
			exact = append(exact, p)
		case strings.Contains(strings.ToLower(p.GetName()), strings.ToLower(want)):
			partial = append(partial, p)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		hint := ""
		if kind == Ban {
			hint = " To ban someone who isn't on, use their id (a SteamID64)."
		}
		return nil, inputf("no one called %q is on %s.%s", want, server, hint)
	}
	names := make([]string, 0, 5)
	for _, p := range matches[:min(5, len(matches))] {
		names = append(names, p.GetName())
	}
	return nil, inputf("%q matches %s; type more of the name, or their id", want, strings.Join(names, ", "))
}

// Question is the confirmation the moderator answers.
func Question(a Action) string {
	var b strings.Builder
	switch a.Kind {
	case Broadcast:
		fmt.Fprintf(&b, "Message everyone on **%s**?", a.ServerName)
	default:
		verb := map[string]string{Kick: "Kick", Ban: "Ban", Unban: "Unban", Message: "Message"}[a.Kind]
		prep := map[string]string{Kick: "from", Ban: "from", Unban: "on", Message: "on"}[a.Kind]
		who := "**" + escape(a.Name) + "**"
		if a.Name != a.Subject {
			who += " (`" + a.Subject + "`)"
		}
		fmt.Fprintf(&b, "%s %s %s **%s**?", verb, who, prep, a.ServerName)
	}
	if a.Text != "" {
		label := "Reason"
		if a.Kind == Message || a.Kind == Broadcast {
			label = "Message"
		}
		fmt.Fprintf(&b, "\n%s: %s", label, escape(a.Text))
	}
	b.WriteString("\nIt goes in the hub's moderation log under your name.")
	return b.String()
}

// Result is what the moderator is told after the hub answers; account is the hub's account page,
// where a member links their Discord account.
func Result(a Action, err error, account string) string {
	if err == nil {
		done := map[string]string{Kick: "Kicked", Ban: "Banned", Unban: "Unbanned", Message: "Messaged"}[a.Kind]
		if a.Kind == Broadcast {
			return fmt.Sprintf("Sent to everyone on %s. It's in the moderation log.", a.ServerName)
		}
		return fmt.Sprintf("%s %s on %s. It's in the moderation log.", done, escape(a.Name), a.ServerName)
	}
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied:
		where := "the hub's account page"
		if account != "" {
			where = account
		}
		return "Nothing done: the hub doesn't have you as a moderator. Link your Discord account on " + where + " and ask the hub's owner for the moderator role."
	case connect.CodeNotFound:
		if a.Kind == Unban {
			return "Nothing done: that player isn't banned on " + a.ServerName + "."
		}
		return "Nothing done: " + escape(a.Name) + " isn't on " + a.ServerName + " any more."
	case connect.CodeFailedPrecondition:
		return "Nothing done: " + a.ServerName + " doesn't offer that right now."
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return "Nothing done: " + a.ServerName + " isn't answering. Try again in a minute."
	case connect.CodeInvalidArgument:
		var ce *connect.Error
		if errors.As(err, &ce) {
			return "Nothing done: " + ce.Message() + "."
		}
	}
	return "Nothing done: something went wrong on the hub's side. Try again, or use the web."
}

// escape keeps names and reasons from formatting Discord markdown or pinging anyone.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "'", "|", `\|`, ">", `\>`, "@", "@\u200b", "#", "#\u200b")
	return r.Replace(s)
}

// pending are actions waiting for their moderator's Confirm.
type pending struct {
	mu      sync.Mutex
	actions map[string]waiting
}

type waiting struct {
	action  Action
	user    string
	expires time.Time
}

func newPending() *pending { return &pending{actions: map[string]waiting{}} }

// put stores an action for its moderator and returns the id its buttons carry.
func (p *pending) put(a Action, user string, now time.Time) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, w := range p.actions { // forget the expired ones as we go
		if !now.Before(w.expires) {
			delete(p.actions, k)
		}
	}
	p.actions[id] = waiting{action: a, user: user, expires: now.Add(ConfirmFor)}
	return id, nil
}

// Errors taking a pending action.
var (
	errExpired = errors.New("that confirmation has expired; run the command again")
	errNotYous = errors.New("only the moderator who ran the command can answer it")
)

// take removes and returns an action, for its own moderator only, before it expires.
func (p *pending) take(id, user string, now time.Time) (Action, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.actions[id]
	if !ok || !now.Before(w.expires) {
		delete(p.actions, id)
		return Action{}, errExpired
	}
	if w.user != user {
		return Action{}, errNotYous
	}
	delete(p.actions, id)
	return w.action, nil
}

// kinds lists the actions, the command's subcommands.
var kinds = []string{Kick, Ban, Unban, Message, Broadcast}

func validKind(k string) bool { return slices.Contains(kinds, k) }
