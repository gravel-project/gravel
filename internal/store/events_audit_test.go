package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// newestEvent is the newest event of the log and how many there are.
func newestEvent(t *testing.T, st *store.Store, orgID uuid.UUID) (store.Event, int) {
	t.Helper()
	evs, _, err := st.ListEvents(context.Background(), orgID, store.EventQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		return store.Event{}, 0
	}
	return evs[0], len(evs)
}

func detailKeys(e store.Event) []string { return slices.Sorted(maps.Keys(e.Detail)) }

// The claim, a settings update, an app's registration and revocation and a member's grants each
// leave exactly one event with who made the change; a change that fails leaves none (gravel#189).
func TestEventsClaimSettingsApps(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	owner, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Owner"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}
	jo, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "2", t0))
	if err != nil {
		t.Fatal(err)
	}
	_, base := newestEvent(t, st, o.ID)

	// The claim: a wrong token records nothing; the claim records the owner and nothing of the token.
	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := st.SetClaimToken(ctx, o.ID, hash, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	by := store.Actor{UserID: &owner.ID, Via: store.ViaAPI, RequestID: "req-claim"}
	if _, err := st.ClaimOrganization(ctx, o.ID, []byte("wrong"), by, t0.Add(time.Minute)); !errors.Is(err, store.ErrClaimRejected) {
		t.Fatalf("a wrong token: %v", err)
	}
	if _, n := newestEvent(t, st, o.ID); n != base {
		t.Fatalf("a refused claim was recorded: %d events, want %d", n, base)
	}
	if _, err := st.ClaimOrganization(ctx, o.ID, hash, by, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	e, n := newestEvent(t, st, o.ID)
	if n != base+1 || e.Kind != "ownership.claimed" || e.Severity != "notice" || *e.ActorUserID != owner.ID || *e.SubjectUserID != owner.ID ||
		e.ActorAppID != nil || e.RequestID != "req-claim" || e.Summary != "Owner claimed the hub and became its owner" {
		t.Errorf("the claim: %d events, %+v", n, e)
	}
	raw, _ := json.Marshal(e)
	if got := detailKeys(e); !slices.Equal(got, []string{"via"}) || strings.Contains(string(raw), string(hash)) {
		t.Errorf("the claim's event carries more than how it came: %v %s", got, raw)
	}

	// Settings: the sections that changed, when, and who; an unknown organization records nothing.
	at := t0.Add(2 * time.Minute)
	if _, err := st.UpdateOrganizationSettings(ctx, uuid.New(), json.RawMessage(`{}`), at, by, []string{"nav"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown organization: %v", err)
	}
	if _, n := newestEvent(t, st, o.ID); n != base+1 {
		t.Fatalf("a failed settings update was recorded: %d events", n)
	}
	if _, err := st.UpdateOrganizationSettings(ctx, o.ID, json.RawMessage(`{"version":1}`), at, store.Actor{UserID: &owner.ID, Via: store.ViaAPI}, []string{"nav", "theme"}); err != nil {
		t.Fatal(err)
	}
	e, _ = newestEvent(t, st, o.ID)
	if e.Kind != "settings.updated" || *e.ActorUserID != owner.ID || e.SubjectUserID != nil || e.Summary != "The settings changed: nav, theme" ||
		e.Detail["via"] != "api" || e.Detail["settings_updated_at"] != "2026-10-13T01:02:00Z" || !slices.Equal(anyStrings(e.Detail["sections"]), []string{"nav", "theme"}) {
		t.Errorf("settings: %+v", e)
	}
	if _, err := st.UpdateOrganizationSettings(ctx, o.ID, json.RawMessage(`{"version":1}`), at.Add(time.Second), store.Actor{Via: store.ViaCLI}, nil); err != nil {
		t.Fatal(err)
	}
	e, _ = newestEvent(t, st, o.ID)
	if e.ActorUserID != nil || e.Detail["via"] != "cli" || e.Summary != "The settings were saved unchanged" || len(anyStrings(e.Detail["sections"])) != 0 {
		t.Errorf("settings from the command line, unchanged: %+v", e)
	}

	// An app from the command line: no actor, never its secret.
	app := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "Muster", ClientID: "gravel_m", SecretHash: []byte("secret-hash"),
		Scopes: []string{"stats:read"}, RedirectURIs: []string{"https://muster.example/cb"}, CreatedAt: t0.Add(3 * time.Minute)}
	if err := st.CreateApp(ctx, app, store.Actor{Via: store.ViaCLI}); err != nil {
		t.Fatal(err)
	}
	e, _ = newestEvent(t, st, o.ID)
	raw, _ = json.Marshal(e.Detail)
	if e.Kind != "app.created" || e.ActorUserID != nil || e.ActorAppID != nil || e.Summary != `The app "Muster" was registered` || e.Detail["via"] != "cli" ||
		e.Detail["client_id"] != "gravel_m" || strings.Contains(string(raw), "secret") {
		t.Errorf("app created: %+v %s", e, raw)
	}

	// A member's grant: new, the same again (nothing), widened, taken back, taken back again (nothing).
	grant := func(scopes []string, at time.Time) {
		t.Helper()
		if err := st.SaveAppGrant(ctx, store.AppGrant{UserID: jo.ID, AppID: app.ID, Scopes: scopes, GrantedAt: at}, store.Actor{UserID: &jo.ID, Via: store.ViaAPI}); err != nil {
			t.Fatal(err)
		}
	}
	grant([]string{"stats:read"}, t0.Add(4*time.Minute))
	e, n = newestEvent(t, st, o.ID)
	if e.Kind != "app.granted" || e.Severity != "info" || *e.ActorUserID != jo.ID || *e.SubjectUserID != jo.ID || e.Summary != `Jo let "Muster" act for them` {
		t.Errorf("a new grant: %+v", e)
	}
	grant([]string{"stats:read"}, t0.Add(5*time.Minute))
	if _, again := newestEvent(t, st, o.ID); again != n {
		t.Errorf("the same grant again was recorded")
	}
	grant([]string{"identity:read", "stats:read"}, t0.Add(6*time.Minute))
	e, _ = newestEvent(t, st, o.ID)
	if e.Kind != "app.granted" || e.Summary != `Jo let "Muster" do more for them: identity:read` || !slices.Equal(anyStrings(e.Detail["added"]), []string{"identity:read"}) {
		t.Errorf("a widened grant: %+v", e)
	}
	if _, err := st.DeleteAppGrant(ctx, jo.ID, app.ID, t0.Add(7*time.Minute), store.Actor{UserID: &jo.ID, Via: store.ViaAPI}); err != nil {
		t.Fatal(err)
	}
	e, n = newestEvent(t, st, o.ID)
	if e.Kind != "app.grant_revoked" || *e.SubjectUserID != jo.ID || e.Summary != `Jo took back "Muster"'s access` {
		t.Errorf("a grant taken back: %+v", e)
	}
	if _, err := st.DeleteAppGrant(ctx, jo.ID, app.ID, t0.Add(8*time.Minute), store.Actor{UserID: &jo.ID}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no grant: %v", err)
	}
	if _, again := newestEvent(t, st, o.ID); again != n {
		t.Errorf("taking back no grant was recorded")
	}

	// Revoked, with the grants it took; revoking again records nothing.
	grant([]string{"stats:read"}, t0.Add(9*time.Minute))
	if err := st.RevokeApp(ctx, app.ID, t0.Add(10*time.Minute), store.Actor{Via: store.ViaCLI}); err != nil {
		t.Fatal(err)
	}
	e, n = newestEvent(t, st, o.ID)
	if e.Kind != "app.revoked" || e.Summary != `The app "Muster" was revoked` || e.Detail["grants_removed"] != float64(1) || e.Detail["via"] != "cli" {
		t.Errorf("app revoked: %+v", e)
	}
	if err := st.RevokeApp(ctx, app.ID, t0.Add(11*time.Minute), store.Actor{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoked twice: %v", err)
	}
	if _, again := newestEvent(t, st, o.ID); again != n {
		t.Errorf("a second revocation was recorded")
	}

	// Every kind written here is in the registry with the severity the log holds.
	evs, _, err := st.ListEvents(ctx, o.ID, store.EventQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if sev, err := events.SeverityOf(e.Kind); err != nil || string(sev) != e.Severity {
			t.Errorf("%s: severity %q, registry %q %v", e.Kind, e.Severity, sev, err)
		}
	}
}

// Migration 00027 backfills the claim, the apps and the grants members hold, oldest first, with the
// registry's severities, and running it again writes nothing twice.
func TestEventsBackfillClaimAndApps(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	owner, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Owner"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}
	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := st.SetClaimToken(ctx, o.ID, hash, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrganization(ctx, o.ID, hash, store.Actor{UserID: &owner.ID}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	muster := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "Muster", ClientID: "gravel_m", SecretHash: []byte("h"), Scopes: []string{"stats:read"}, CreatedAt: t0.Add(2 * time.Minute)}
	old := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "Old", ClientID: "gravel_o", SecretHash: []byte("h"), CreatedAt: t0.Add(3 * time.Minute)}
	for _, a := range []store.App{muster, old} {
		if err := st.CreateApp(ctx, a, store.Actor{}); err != nil {
			t.Fatal(err)
		}
	}
	granted := time.Date(2026, 10, 1, 1, 4, 0, 123456000, time.UTC) // microseconds, as Postgres keeps them
	if err := st.SaveAppGrant(ctx, store.AppGrant{UserID: owner.ID, AppID: muster.ID, Scopes: []string{"stats:read"}, GrantedAt: granted}, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeApp(ctx, old.ID, t0.Add(5*time.Minute), store.Actor{}); err != nil {
		t.Fatal(err)
	}
	live := eventKinds(t, st, o.ID, store.EventQuery{Limit: 500})

	// Down past the events log and up again: only the backfills write it.
	if err := st.MigrateDownTo(ctx, 25); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	evs, _, err := st.ListEvents(ctx, o.ID, store.EventQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		kinds = append(kinds, e.Kind)
		if i < len(evs)-1 && e.At.Before(evs[i+1].At) {
			t.Errorf("not oldest first: %s at %s after %s", e.Kind, e.At, evs[i+1].At)
		}
		if sev, _ := events.SeverityOf(e.Kind); string(sev) != e.Severity {
			t.Errorf("%s backfilled as %q, the registry says %q", e.Kind, e.Severity, sev)
		}
		if e.Kind != "identity.registered" && e.Detail["backfilled"] != true {
			t.Errorf("%s not marked backfilled: %v", e.Kind, e.Detail)
		}
	}
	want := "identity.registered ownership.claimed app.created app.created app.granted app.revoked"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("backfilled: %s, want %s (live: %v)", got, want, live)
	}
	for _, e := range evs {
		switch e.Kind {
		case "ownership.claimed":
			if *e.ActorUserID != owner.ID || e.Summary != "Owner claimed the hub and became its owner" {
				t.Errorf("claim: %+v", e)
			}
		case "app.granted":
			if *e.SubjectUserID != owner.ID || e.Summary != `Owner let "Muster" act for them` || e.SourceID == "" {
				t.Errorf("grant: %+v", e)
			}
		}
	}

	// 00027 again, over the same rows and with the live writers' keys: nothing twice.
	if err := st.MigrateDownTo(ctx, 26); err != nil {
		t.Fatal(err)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{Limit: 500}); len(got) != 1 {
		t.Errorf("00027's down left %v; want only the registration", got)
	}
	if err := st.SaveAppGrant(ctx, store.AppGrant{UserID: owner.ID, AppID: muster.ID, Scopes: []string{"stats:read", "identity:read"}, GrantedAt: granted}, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{Limit: 500}); len(got) != len(evs) {
		t.Errorf("after 00027 ran again over a live grant event: %v, want %d events", got, len(evs))
	}
}

func anyStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
