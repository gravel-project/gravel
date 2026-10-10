package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestIngestBatches(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official",
		Feed: json.RawMessage(`{"url": "https://ingest.example.com", "token_file": "/run/secrets/feed"}`)}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, []store.ManagedServer{srv}, t0); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListManagedServers(ctx, o.ID, false)
	if err != nil || len(got) != 1 {
		t.Fatalf("servers = %+v, %v", got, err)
	}
	var feed map[string]string
	if err := json.Unmarshal(got[0].Feed, &feed); err != nil || feed["token_file"] != "/run/secrets/feed" {
		t.Errorf("feed = %s, %v", got[0].Feed, err)
	}
	// The same server again changes nothing; without its feed it changes.
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, []store.ManagedServer{srv}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ListManagedServers(ctx, o.ID, false); !got[0].UpdatedAt.Equal(t0) {
		t.Errorf("an unchanged feed updated the row: %v", got[0].UpdatedAt)
	}

	bodies := [][]byte{[]byte(`[{"eventId":"a"}]`), []byte(`[{"eventId":"b"}]`), {0xff, 0x00}}
	var ids []int64
	for i, b := range bodies {
		id, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "wd-1", Source: "wardogs", ReceivedAt: t0.Add(time.Duration(i) * 24 * time.Hour), Body: b})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "nope", Source: "wardogs", ReceivedAt: t0, Body: []byte("x")}); err == nil {
		t.Error("a batch for a server that does not exist was stored")
	}
	list, err := st.ListIngestBatches(ctx, o.ID, "wd-1", 0, 2)
	if err != nil || len(list) != 2 || list[0].ID != ids[0] || list[1].ID != ids[1] {
		t.Fatalf("first page = %+v, %v", list, err)
	}
	sum := sha256.Sum256(bodies[0])
	if b := list[0]; !bytes.Equal(b.Body, bodies[0]) || !bytes.Equal(b.BodySHA256, sum[:]) || b.State != store.IngestPending || string(b.Headers) != "{}" || !b.ReceivedAt.Equal(t0) {
		t.Errorf("batch = %+v", b)
	}
	if rest, err := st.ListIngestBatches(ctx, o.ID, "wd-1", ids[1], 10); err != nil || len(rest) != 1 || !bytes.Equal(rest[0].Body, bodies[2]) {
		t.Errorf("after = %+v, %v", rest, err)
	}
	n, err := st.DeleteIngestBatchesBefore(ctx, t0.Add(36*time.Hour))
	if err != nil || n != 2 {
		t.Errorf("pruned %d, %v", n, err)
	}
	if left, _ := st.ListIngestBatches(ctx, o.ID, "wd-1", 0, 10); len(left) != 1 || left[0].ID != ids[2] {
		t.Errorf("left = %+v", left)
	}
}
