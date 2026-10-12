package web_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/identity/proof"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// proofServers is what the identity API reads to take a code on htg-cs2-1: a running CS2 server
// whose chat the hub reads, its players known by Steam.
type proofServers struct{}

func (proofServers) Games(context.Context) ([]servers.Game, error) {
	return []servers.Game{{Spec: servers.Spec{ID: "cs2", Name: "Counter-Strike 2", IdentityProvider: "steam"}}}, nil
}

func (proofServers) Servers(ctx context.Context) ([]servers.Server, error) {
	s, err := proofServers{}.Server(ctx, "htg-cs2-1")
	return []servers.Server{s}, err
}

func (proofServers) Server(_ context.Context, id string) (servers.Server, error) {
	if id != "htg-cs2-1" {
		return servers.Server{}, store.ErrNotFound
	}
	return servers.Server{ID: id, Name: "HTG CS2", Game: "cs2"}, nil
}

func (proofServers) Observation(id string) (servers.Observation, bool) {
	return servers.Observation{ServerID: id, State: servers.StateOK, Reachable: true, Capabilities: []string{drivers.CapStatus, drivers.CapChat}}, id == "htg-cs2-1"
}

var codeOnPage = regexp.MustCompile(`<code>([0-9A-Z]{4}-[0-9A-Z]{4})</code>`)

// A member gets a code on the account page, the page shows it once with the server to type it on,
// and asks what became of it until a chat line proves the account (gravel#5).
func TestProveAnAccountInGame(t *testing.T) {
	r := newRig(t)
	withCS2(r)
	c := browser(t)
	r.loginAs(t, c, "jo")
	_, page := get(t, c, r.srv.URL+"/account")
	if strings.Contains(page, "proof-heading") {
		t.Fatal("offered with no server reading chat")
	}
	r.servers.mu.Lock()
	cs2 := r.servers.servers[len(r.servers.servers)-1]
	cs2.Status.Capabilities = append(cs2.Status.Capabilities, drivers.CapChat)
	r.servers.mu.Unlock()
	_, page = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(page, `<option value="htg-cs2-1">HTG CS2</option>`) || strings.Contains(page, "proof-subject") {
		t.Fatalf("the offer: %s", page)
	}
	csrf := csrfOf(t, page)
	resp, body := post(t, c, r.srv.URL+"/account/prove", url.Values{"_csrf": {csrf}, "server_id": {"htg-cs2-1"}}, nil)
	m := codeOnPage.FindStringSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil || !strings.Contains(body, "type in chat: !link "+m[1]) || !strings.Contains(body, "203.0.113.50:27015") ||
		!strings.Contains(body, `hx-trigger="every 5s"`) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the code's page: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	status := regexp.MustCompile(`hx-get="(/account/prove/[^"]+)"`).FindStringSubmatch(body)[1]
	if _, frag := get(t, c, r.srv.URL+status, "HX-Request", "true"); !strings.Contains(frag, "Waiting for the code") {
		t.Errorf("open: %s", frag)
	}

	// The server's log carries the line; the account is linked.
	if _, err := r.proofs.Present(context.Background(), proof.Evidence{Channel: proof.GameChatName, Scope: "htg-cs2-1",
		Account: proof.Account{Subject: "76561198000000009", DisplayName: "Jo in game"}, Text: "!link " + m[1]}); err != nil {
		t.Fatal(err)
	}
	_, frag := get(t, c, r.srv.URL+status, "HX-Request", "true")
	if !strings.Contains(frag, "Done: your Steam account <code>76561198000000009</code> is linked and proven.") || strings.Contains(frag, "hx-trigger") {
		t.Errorf("proven: %s", frag)
	}
	// Someone else asking for it finds nothing.
	other := browser(t)
	r.loginAs(t, other, "sam")
	if resp, _ := get(t, other, r.srv.URL+status); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another member: %d", resp.StatusCode)
	}

	// Refusals come back as a message on the account page.
	resp, _ = post(t, c, r.srv.URL+"/account/prove", url.Values{"_csrf": {csrf}, "server_id": {"nope"}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("an unknown server: %d", resp.StatusCode)
	}
	for range proof.CodesPerHour - 1 {
		post(t, c, r.srv.URL+"/account/prove", url.Values{"_csrf": {csrf}, "server_id": {"htg-cs2-1"}}, nil)
	}
	post(t, c, r.srv.URL+"/account/prove", url.Values{"_csrf": {csrf}, "server_id": {"htg-cs2-1"}}, nil)
	if _, page := get(t, c, r.srv.URL+"/account"); !strings.Contains(page, "That is a lot of codes in an hour") {
		t.Errorf("rate limited: %s", page)
	}
}
