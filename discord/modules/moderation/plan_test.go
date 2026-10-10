package moderation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

type planHub struct {
	servers []*hubv1.Server
	players []*hubv1.ServerPlayer
	games   []*hubv1.Game
}

func (h planHub) Games(context.Context) ([]*hubv1.Game, error)     { return h.games, nil }
func (h planHub) Servers(context.Context) ([]*hubv1.Server, error) { return h.servers, nil }
func (h planHub) Players(context.Context, string) ([]*hubv1.ServerPlayer, error) {
	return h.players, nil
}
func (planHub) Do(context.Context, Action, *hubv1.Actor) error { return nil }

func TestPlan(t *testing.T) {
	one := planHub{
		servers: []*hubv1.Server{{Id: "wd-1", Name: "War Dogs #1", GameId: "wardogs"}},
		players: []*hubv1.ServerPlayer{{Name: "Jo", Subject: "76561190000000001"}, {Name: "JoJo", Subject: "76561190000000004"}, {Name: "Stranger", Subject: "76561190000000002"}},
		games:   []*hubv1.Game{{Id: "wardogs", IdentityProvider: "steam"}},
	}
	two := one
	two.servers = append(two.servers, &hubv1.Server{Id: "wd-2", Name: "War Dogs #2", GameId: "wardogs"})
	ctx := context.Background()
	for _, c := range []struct {
		name string
		hub  planHub
		req  Request
		want Action
		err  string
	}{
		{"exact name beats a longer one", one, Request{Kind: Kick, Player: "jo", Text: "afk"}, Action{Kind: Kick, ServerID: "wd-1", ServerName: "War Dogs #1", Subject: "76561190000000001", Name: "Jo", Text: "afk"}, ""},
		{"part of a name", one, Request{Kind: Message, Player: "strang", Text: "hi"}, Action{Kind: Message, ServerID: "wd-1", ServerName: "War Dogs #1", Subject: "76561190000000002", Name: "Stranger", Text: "hi"}, ""},
		{"an id of someone on", one, Request{Kind: Ban, Player: "76561190000000002", Text: "x"}, Action{Kind: Ban, ServerID: "wd-1", ServerName: "War Dogs #1", Subject: "76561190000000002", Name: "Stranger", Text: "x"}, ""},
		{"an id of someone off", one, Request{Kind: Ban, Player: "76561190000000099", Text: "x"}, Action{Kind: Ban, ServerID: "wd-1", ServerName: "War Dogs #1", Subject: "76561190000000099", Name: "76561190000000099", Text: "x"}, ""},
		{"unban without a reason", one, Request{Kind: Unban, Player: "76561190000000099"}, Action{Kind: Unban, ServerID: "wd-1", ServerName: "War Dogs #1", Subject: "76561190000000099", Name: "76561190000000099"}, ""},
		{"broadcast", one, Request{Kind: Broadcast, Text: " restart in 5 "}, Action{Kind: Broadcast, ServerID: "wd-1", ServerName: "War Dogs #1", Text: "restart in 5"}, ""},
		{"two servers, one named", two, Request{Kind: Broadcast, Server: "war dogs #2", Text: "x"}, Action{Kind: Broadcast, ServerID: "wd-2", ServerName: "War Dogs #2", Text: "x"}, ""},
		{"two servers, none named", two, Request{Kind: Broadcast, Text: "x"}, Action{}, "name the server: wd-1, wd-2"},
		{"an unknown server", one, Request{Kind: Broadcast, Server: "cs-1", Text: "x"}, Action{}, `no server "cs-1"`},
		{"ambiguous", one, Request{Kind: Kick, Player: "j", Text: "x"}, Action{}, `"j" matches Jo, JoJo`},
		{"nobody", one, Request{Kind: Ban, Player: "ghost", Text: "x"}, Action{}, "To ban someone who isn't on, use their id"},
		{"unban by name", one, Request{Kind: Unban, Player: "Jo"}, Action{}, "unban takes the player's id"},
		{"no reason", one, Request{Kind: Kick, Player: "Jo"}, Action{}, "say why"},
		{"too long", one, Request{Kind: Broadcast, Text: strings.Repeat("é", MaxText+1)}, Action{}, "keep it to 256 characters"},
		{"not an id for steam", one, Request{Kind: Ban, Player: "12345", Text: "x"}, Action{}, `no one called "12345"`},
		{"an unknown action", one, Request{Kind: "nuke", Text: "x"}, Action{}, "not something this command does"},
	} {
		got, err := Plan(ctx, c.hub, c.req)
		switch {
		case c.err != "" && (err == nil || !errors.Is(err, ErrInput) || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s: %+v, %v", c.name, got, err)
		}
	}
	// A game keyed by another provider takes any number as an id.
	other := one
	other.games = []*hubv1.Game{{Id: "wardogs", IdentityProvider: "xbox"}}
	if a, err := Plan(ctx, other, Request{Kind: Ban, Player: "2535412345678901", Text: "x"}); err != nil || a.Subject != "2535412345678901" {
		t.Errorf("an xbox id: %+v %v", a, err)
	}
}

func TestQuestionAndResultTextIsSafe(t *testing.T) {
	a := Action{Kind: Ban, ServerID: "wd-1", ServerName: "War Dogs #1", Subject: "76561190000000002", Name: "@everyone *bold*", Text: "spam <@1> #general"}
	q := Question(a)
	for _, bad := range []string{"@everyone", "*bold*", "<@1>", " #general"} {
		if strings.Contains(q, bad) {
			t.Errorf("the question carries %q unescaped: %s", bad, q)
		}
	}
	if !strings.Contains(q, "(`76561190000000002`)") || !strings.Contains(q, "Reason:") || !strings.Contains(q, "moderation log under your name") {
		t.Errorf("question = %s", q)
	}
	if m := Question(Action{Kind: Message, ServerName: "S", Subject: "1", Name: "1", Text: "hi"}); strings.Contains(m, "(`1`)") || !strings.Contains(m, "Message: hi") {
		t.Errorf("an id-only message question = %s", m)
	}

	for code, want := range map[connect.Code]string{
		connect.CodePermissionDenied:   "https://hub/account",
		connect.CodeNotFound:           "isn't on War Dogs #1 any more",
		connect.CodeFailedPrecondition: "doesn't offer that",
		connect.CodeUnavailable:        "isn't answering",
		connect.CodeInvalidArgument:    "Nothing done: bad reason.",
		connect.CodeInternal:           "something went wrong",
	} {
		if got := Result(a, connect.NewError(code, errors.New("bad reason")), "https://hub/account"); !strings.Contains(got, want) {
			t.Errorf("%v: %s", code, got)
		}
	}
	if got := Result(Action{Kind: Unban, ServerName: "S"}, connect.NewError(connect.CodeNotFound, errors.New("x")), ""); !strings.Contains(got, "isn't banned") {
		t.Errorf("unban not found: %s", got)
	}
	if got := Result(a, connect.NewError(connect.CodePermissionDenied, errors.New("x")), ""); !strings.Contains(got, "the hub's account page") {
		t.Errorf("no account url: %s", got)
	}
}

func TestPending(t *testing.T) {
	p := newPending()
	now := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	a := Action{Kind: Kick, Subject: "1"}
	id, err := p.put(a, "42", now)
	if err != nil || len(id) != 16 {
		t.Fatalf("put: %q %v", id, err)
	}
	if _, err := p.take(id, "43", now); !errors.Is(err, errNotYous) {
		t.Errorf("another user: %v", err)
	}
	if got, err := p.take(id, "42", now.Add(time.Minute)); err != nil || got != a {
		t.Errorf("take: %+v %v", got, err)
	}
	if _, err := p.take(id, "42", now); !errors.Is(err, errExpired) {
		t.Errorf("a second take: %v", err)
	}
	old, _ := p.put(a, "42", now)
	if _, err := p.take(old, "42", now.Add(ConfirmFor)); !errors.Is(err, errExpired) {
		t.Errorf("expired: %v", err)
	}
	// Expired entries are forgotten as new ones arrive.
	stale, _ := p.put(a, "42", now)
	if _, err := p.put(a, "42", now.Add(ConfirmFor+time.Second)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	_, kept := p.actions[stale]
	p.mu.Unlock()
	if kept {
		t.Error("an expired entry was kept")
	}
}
