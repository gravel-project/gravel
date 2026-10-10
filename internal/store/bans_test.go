package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestBanLists(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official"}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs", Bands: json.RawMessage(`[]`)}}, []store.ManagedServer{srv}, t0); err != nil {
		t.Fatal(err)
	}
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Owner"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := st.BanListAdopted(ctx, o.ID, "wd-1"); err != nil || ok {
		t.Fatalf("adopted before adoption: %v, %v", ok, err)
	}
	host := []store.Ban{{Provider: "steam", Subject: "76561190000000009"}, {Provider: "steam", Subject: "76561190000000009"}}
	if ok, err := st.AdoptBanList(ctx, o.ID, "wd-1", host, t0); err != nil || !ok {
		t.Fatalf("adopt: %v, %v", ok, err)
	}
	if ok, err := st.AdoptBanList(ctx, o.ID, "wd-1", []store.Ban{{Provider: "steam", Subject: "76561190000000008"}}, t0); err != nil || ok {
		t.Errorf("a second adoption: %v, %v", ok, err)
	}
	if ok, _ := st.BanListAdopted(ctx, o.ID, "wd-1"); !ok {
		t.Error("not adopted")
	}
	active, err := st.ActiveBans(ctx, o.ID, "wd-1")
	if err != nil || len(active) != 1 || active[0].BanAuditID != nil || !active[0].BannedAt.Equal(t0) {
		t.Fatalf("imported = %+v, %v (a duplicate in the host's list is one ban)", active, err)
	}

	aid, err := st.InsertAuditEntry(ctx, store.AuditEntry{OrganizationID: o.ID, At: t0, ActorUserID: &u.ID, ServerID: "wd-1", Action: "ban",
		TargetProvider: "steam", TargetSubject: "76561190000000001", Reason: "cheating"})
	if err != nil {
		t.Fatal(err)
	}
	ban := store.Ban{OrganizationID: o.ID, ServerID: "wd-1", Provider: "steam", Subject: "76561190000000001", Reason: "cheating", BannedAt: t0, BanAuditID: &aid}
	if _, err := st.AddBan(ctx, ban); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBan(ctx, ban); !errors.Is(err, store.ErrBanExists) {
		t.Errorf("a second active ban: %v", err)
	}
	if active, _ := st.ActiveBans(ctx, o.ID, "wd-1"); len(active) != 2 || active[1].Reason != "cheating" || *active[1].BanAuditID != aid {
		t.Errorf("active = %+v", active)
	}

	lift, err := st.InsertAuditEntry(ctx, store.AuditEntry{OrganizationID: o.ID, At: t0, ActorUserID: &u.ID, ServerID: "wd-1", Action: "unban",
		TargetProvider: "steam", TargetSubject: "76561190000000001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LiftBan(ctx, o.ID, "wd-1", "steam", "76561190000000001", &lift, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.LiftBan(ctx, o.ID, "wd-1", "steam", "76561190000000001", nil, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("lifting a lifted ban: %v", err)
	}
	if active, _ := st.ActiveBans(ctx, o.ID, "wd-1"); len(active) != 1 || active[0].Subject != "76561190000000009" {
		t.Errorf("active after the lift = %+v", active)
	}
	// Banned again after the lift: a new active ban beside the lifted record.
	ban.BanAuditID = nil
	if _, err := st.AddBan(ctx, ban); err != nil {
		t.Errorf("a ban after a lift: %v", err)
	}
	if other, err := st.ActiveBans(ctx, uuid.New(), "wd-1"); err != nil || len(other) != 0 {
		t.Errorf("another organization = %+v, %v", other, err)
	}
}
