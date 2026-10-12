package web

import (
	"net/http"
	"slices"
	"strings"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// Proof of control in game (gravel#5): the account page offers the servers whose chat takes a code;
// a code is shown once on its own page, which asks what became of it until it is done.

// proofOffer fills the account page's servers and accounts for a code typed in chat.
func (h *Handler) proofOffer(r *http.Request, b *browser, ap *templates.AccountPage) error {
	ctx := withBrowserContext(r, b)
	list, err := h.serverAPI.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return err
	}
	games, err := h.serverAPI.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		return err
	}
	providers := map[string]bool{}
	for _, s := range list.Msg.GetServers() {
		st := s.GetStatus()
		if st.GetState() != "ok" || !st.GetReachable() || !slices.Contains(st.GetCapabilities(), drivers.CapChat) {
			continue
		}
		ap.ProofServers = append(ap.ProofServers, templates.ProofServer{ID: s.GetId(), Name: s.GetName()})
		for _, g := range games.Msg.GetGames() {
			if g.GetId() == s.GetGameId() {
				providers[g.GetIdentityProvider()] = true
			}
		}
	}
	for _, i := range ap.Identities {
		if providers[i.Provider] {
			ap.ProofAccounts = append(ap.ProofAccounts, i)
		}
	}
	return nil
}

// startProof gets a code and shows it once.
func (h *Handler) startProof(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	serverID := r.PostFormValue("server_id")
	resp, err := h.idAPI.StartProof(ctx, connect.NewRequest(&hubv1.StartProofRequest{ServerId: serverID, Subject: strings.TrimSpace(r.PostFormValue("subject"))}))
	switch connect.CodeOf(err) {
	case connect.CodeResourceExhausted:
		h.accountDone(w, r, b, templates.Flash{Kind: "err", Text: "That is a lot of codes in an hour. Wait a little, then get a new one."})
		return
	case connect.CodeFailedPrecondition, connect.CodeNotFound, connect.CodeInvalidArgument:
		h.accountDone(w, r, b, templates.Flash{Kind: "err", Text: "No code: " + connectMessage(err) + "."})
		return
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
	}
	m := resp.Msg
	p, b2, err := h.page(r, "Your code")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	pp := templates.ProofPage{Page: p.Page, Code: m.GetCode(), Instructions: m.GetInstructions(), ServerName: serverID,
		ExpiresAt: m.GetExpiresAt().AsTime(), Status: templates.ProofStatus{ID: m.GetProofId(), State: "open", ProviderDisplay: h.providerDisplay(m.GetProvider())}}
	if s, err := h.serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: serverID})); err == nil {
		pp.ServerName = s.Msg.GetServer().GetName()
		if j := s.Msg.GetServer().GetStatus().GetJoin(); j.GetValue() != "" {
			pp.Join = &templates.Join{Label: templates.JoinLabel(j.GetMethod()), Value: j.GetValue(), Hint: j.GetHint()}
		}
	}
	b.relay(w)
	w.Header().Set("Cache-Control", "no-store") // the code is on this page only
	h.render(w, r, http.StatusOK, b2, templates.Proof(pp), templates.ProofContent(pp))
}

// proofStatus is what became of a code: the fragment the code's page asks for.
func (h *Handler) proofStatus(w http.ResponseWriter, r *http.Request) {
	ctx, b := withBrowser(r)
	resp, err := h.idAPI.GetProof(ctx, connect.NewRequest(&hubv1.GetProofRequest{ProofId: r.PathValue("id")}))
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument:
		h.fail(w, r, http.StatusNotFound, "No such code", "That code isn't yours, or it is long gone.")
		return
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
	}
	m := resp.Msg
	st := templates.ProofStatus{ID: m.GetProofId(), State: m.GetState(), ProviderDisplay: h.providerDisplay(m.GetProvider()), Subject: m.GetSubject()}
	b.relay(w)
	h.render(w, r, http.StatusOK, b, templates.ProofStatusView(st), templates.ProofStatusView(st))
}
