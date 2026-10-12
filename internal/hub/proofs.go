package hub

import (
	"context"
	"errors"
	"time"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/proof"
	"github.com/gravel-project/gravel/internal/ratelimit"
)

// Proof of control in game chat (gravel#5): a member asks for a code on the account page, joins a
// cloud-VM server and types "!link CODE"; the server's log carries the line with the account that
// typed it, and the hub links the account and answers in chat.

// proofTimeout bounds one presented code: the redemption and the answer in chat.
const proofTimeout = 30 * time.Second

// chatProofs takes a server's chat lines from its log and presents each that holds a code. It runs
// on the log route's goroutine, so the work goes on its own.
func (h *Hub) chatProofs(serverID string, lines []cloudvm.ChatLine) {
	ch, ok := h.proofs.Channel(proof.GameChatName)
	if !ok {
		return
	}
	for _, l := range lines {
		if _, ok := ch.Code(l.Text); !ok || l.Player.Identity.Provider != ch.Provider() {
			continue
		}
		go h.presentProof(serverID, ch.Provider(), l)
	}
}

func (h *Hub) presentProof(serverID, provider string, l cloudvm.ChatLine) {
	ctx, cancel := context.WithTimeout(context.Background(), proofTimeout)
	defer cancel()
	out, err := h.proofs.Present(ctx, proof.Evidence{Channel: proof.GameChatName, Scope: serverID,
		Account: proof.Account{Subject: l.Player.Identity.Subject, DisplayName: l.Player.Name}, Text: l.Text})
	name := provider
	if r, ok := h.ids.Provider(provider); ok {
		name = r.Provider.DisplayName()
	}
	var tm *proof.TooManyError
	var reply string
	switch {
	case err == nil && out.Linked:
		reply = "Code accepted: this " + name + " account is now linked to your hub account."
	case err == nil:
		reply = "Code accepted: this " + name + " account is proven again."
	case errors.Is(err, identity.ErrIdentityTaken):
		reply = "This " + name + " account is linked to another member of the hub, so nothing changed."
	case errors.Is(err, proof.ErrBadCode), errors.As(err, &tm):
		// A failure is answered at most once a minute per server, so a chat can't be flooded.
		if ok, _ := h.proofReplies.Allow(serverID); !ok {
			return
		}
		reply = "That code is unknown or expired. Get a new one on your account page."
		if tm != nil {
			reply = "Too many codes from here. Wait a few minutes, then try again."
		}
	default:
		h.logger.Error("a code typed in chat could not be checked", "server", serverID, "error", err.Error())
		return
	}
	if err := h.cloudvm.Say(ctx, serverID, reply); err != nil {
		h.logger.Warn("the answer to a code typed in chat was not said", "server", serverID, "error", err.Error())
	}
}

// newProofReplies is the limit on failure answers in a server's chat: one a minute.
func newProofReplies() *ratelimit.Limiter { return ratelimit.New(1, 1) }
