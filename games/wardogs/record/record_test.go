package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

const recorded = "../testdata/CL-509546"

// The people, addresses and secrets a recording must never keep.
const (
	token      = "s3cret-rcon-token"
	steamA     = "76561198012345678"
	steamB     = "76561198087654321"
	steamC     = "76561198099999999"
	nameA      = "RealName"
	nameB      = "Bob"
	addrV4     = "198.18.0.5"
	addrV6     = "2a01:4f8:c0c:1::1"
	serverUUID = "a968511e-a098-48ec-9bdb-909255478f15"
	feedToken  = "feed-secret-value"
)

// source copies the recorded build and seeds it with people, addresses and secrets.
func source(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir(recorded, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(recorded, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("v1/players.json", `{"players":[
		{"name":"`+nameA+`","steamId":"`+steamA+`","faction":"Valkyra","kills":3,"deaths":1,"cash":1200,"pingMs":40},
		{"name":"`+nameB+`","steamId":`+steamB+`,"faction":"Lonestar","kills":0,"deaths":2,"cash":0,"pingMs":90}],"count":2}`)
	write("v1/bans.json", `{"bans":[{"steamId":"`+steamC+`","bannedAtUtc":"2026-10-01T00:00:00.000Z","bannedBy":"admin at `+addrV4+`","reason":"cheating with `+nameA+`"}],"count":1}`)
	write("v1/audit.json", `{"limit":20,"count":3,"entries":[
		{"timestampUtc":"2026-10-09T22:58:29.444Z","peer":"`+addrV4+`:41440","sessionId":"s1","event":"AUTH_OK","detail":null},
		{"timestampUtc":"2026-10-09T22:58:30.000Z","peer":"[`+addrV6+`]:443","sessionId":"s2","event":"HTTP","detail":"POST /v1/players/`+steamA+`/kick -> 200 (`+nameA+`)"},
		{"timestampUtc":"2026-10-09T22:58:31.000Z","peer":"`+addrV4+`:41441","sessionId":"s3","event":"HTTP","detail":"login with `+token+`"}]}`)
	write("v1/server-id.json", `{"serverId":"`+serverUUID+`"}`)
	var cfg map[string]any
	data, err := os.ReadFile(filepath.Join(dir, "v1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(cfg["text"].(string), "Password=<redacted>", "Password="+token, 1)
	text += "[WDServerFeed]\r\nUrl=https://sink.example/ingest\r\nToken=" + feedToken + "\r\n" +
		"[/Script/WDGame.WDGameSession]\r\n+DefaultBannedPlayerIds=" + steamC + "\r\n"
	cfg["text"] = text
	data, _ = json.Marshal(cfg)
	write("v1/config.json", string(data))
	return dir
}

func newClient(t *testing.T, url, tok string, now func() time.Time) *wardogs.Client {
	t.Helper()
	c, err := wardogs.New(url, wardogs.Options{Token: tok, Strict: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fixedNow() time.Time { return time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC) }

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordScrubsPeopleAddressesAndSecrets(t *testing.T) {
	srv := wardogstest.New(t, source(t), wardogstest.Options{Token: token})
	out := t.TempDir()
	rep, err := Record(context.Background(), newClient(t, srv.URL, token, nil), Options{Dir: out, Token: token, Recorder: "test", Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Build != "++Wardogs+Live-CL-509546" || rep.Dir != filepath.Join(out, "CL-509546") || len(rep.Skipped) != 0 {
		t.Errorf("report = %+v", rep)
	}
	files := readTree(t, rep.Dir)
	for _, want := range []string{"recording.json", "v1/capabilities.json", "v1/health.json", "v1/config.json",
		"v1/players.json", "v1/catalog/maps/Kavkazi/alternators.json", "v1/catalog/maps/Europe/experiences.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s not written", want)
		}
	}
	if len(files) != len(rep.Files)+1 {
		t.Errorf("%d files on disk, %d in the manifest", len(files), len(rep.Files))
	}
	for name, body := range files {
		for _, bad := range []string{token, steamA, steamB, steamC, nameA, addrV4, addrV6, serverUUID, feedToken, "sink.example"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s still holds %q", name, bad)
			}
		}
	}
	// One stand-in per person and address, the same in every file, numbered in the order the
	// files are walked (audit, bans, …, players).
	players, audit, bans, cfg := files["v1/players.json"], files["v1/audit.json"], files["v1/bans.json"], files["v1/config.json"]
	for _, c := range []struct{ file, want string }{
		{players, `"name": "Player 1"`}, {players, `"steamId": "76561190000000001"`},
		{players, `"name": "Player 2"`}, {players, `"steamId": 76561190000000003`},
		{audit, `/v1/players/76561190000000001/kick -> 200 (Player 1)`},
		{audit, `"peer": "192.0.2.1:41440"`}, {audit, `"peer": "[2001:db8::2]:443"`},
		{audit, `"detail": "login with <redacted>"`},
		{bans, `"steamId": "76561190000000002"`}, {bans, `admin at 192.0.2.1`}, {bans, `cheating with Player 1`},
		{cfg, `Password=<redacted>`}, {cfg, `Token=<redacted>`}, {cfg, `Url=<redacted>`},
		{cfg, `DefaultBannedPlayerIds=76561190000000002`},
		{files["v1/server-id.json"], `"serverId": "00000000-0000-4000-8000-000000000001"`},
	} {
		if !strings.Contains(c.file, c.want) {
			t.Errorf("missing %s", c.want)
		}
	}
	if s := rep.Scrubbed; s.SteamIDs != 3 || s.Names != 2 || s.Addresses != 2 || s.ServerIDs != 1 || s.Secrets < 3 {
		t.Errorf("scrubbed = %+v", s)
	}
	if !strings.Contains(cfg, `\r\n`) {
		t.Error("the document lost its CRLF")
	}

	// The same answers record to the same bytes.
	again, err := Record(context.Background(), newClient(t, srv.URL, token, nil), Options{Dir: out, Token: token, Recorder: "test", Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	second := readTree(t, again.Dir)
	for name, body := range files {
		if second[name] != body {
			t.Errorf("%s changed on a second recording", name)
		}
	}
}

func TestRecordWithoutATokenRecordsThePublicRoutes(t *testing.T) {
	srv := wardogstest.New(t, source(t), wardogstest.Options{Token: token})
	rep, err := Record(context.Background(), newClient(t, srv.URL, "", nil), Options{Dir: t.TempDir(), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range rep.Files {
		got = append(got, f.Path)
	}
	if !slices.Equal(got, []string{"v1/capabilities.json", "v1/health.json"}) {
		t.Errorf("files = %v", got)
	}
	if len(rep.Skipped) != 14 || rep.Skipped[0].Reason != "needs the token" {
		t.Errorf("skipped = %+v", rep.Skipped)
	}
	if srv.Strikes() != 0 {
		t.Errorf("strikes = %d", srv.Strikes())
	}
}

func TestARefusedTokenWritesNothing(t *testing.T) {
	srv := wardogstest.New(t, source(t), wardogstest.Options{Token: token})
	out := t.TempDir()
	_, err := Record(context.Background(), newClient(t, srv.URL, "wrong", nil), Options{Dir: out, Token: "wrong"})
	if !errors.Is(err, wardogs.ErrTokenRefused) {
		t.Fatalf("err = %v", err)
	}
	if srv.Strikes() != 1 {
		t.Errorf("strikes = %d", srv.Strikes())
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestRecordWaitsOutOne429(t *testing.T) {
	now := fixedNow()
	var calls atomic.Int32
	srv := wardogstest.New(t, source(t), wardogstest.Options{Token: token, Handle: map[string]http.HandlerFunc{
		"GET /v1/status": func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"map":"Ozeti"}`)
		},
	}})
	var slept []time.Duration
	rep, err := Record(context.Background(), newClient(t, srv.URL, token, func() time.Time { return now }), Options{
		Dir: t.TempDir(), Token: token, Now: fixedNow,
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] <= 0 || slept[0] > 3*time.Second || calls.Load() != 2 {
		t.Errorf("slept %v, %d calls", slept, calls.Load())
	}
	if data, _ := os.ReadFile(filepath.Join(rep.Dir, "v1", "status.json")); !bytes.Contains(data, []byte("Ozeti")) {
		t.Errorf("status = %s", data)
	}
}

func TestRecordReplacesTheBuildAndDiffsThePrevious(t *testing.T) {
	src := source(t)
	srv := wardogstest.New(t, src, wardogstest.Options{Token: token})
	out := t.TempDir()
	// An older build with one route fewer and one spelled differently, and a stale file in the
	// directory this recording replaces.
	old := filepath.Join(out, "CL-501228", "v1")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "capabilities.json"),
		[]byte(`{"build":"++Wardogs+Live-CL-501228","routes":["GET /v1/status","DELETE /v1/bans/{id}","GET /v1/gone"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(out, "CL-509546", "v1", "stale.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Record(context.Background(), newClient(t, srv.URL, token, nil), Options{Dir: out, Token: token, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the stale file survived")
	}
	if rep.Previous != "CL-501228" || !slices.Contains(rep.Diff.Removed, "GET /v1/gone") ||
		!slices.Equal(rep.Diff.Renamed, []string{"DELETE /v1/bans/{id} -> DELETE /v1/bans/{steamId}"}) || len(rep.Diff.Added) != 27 {
		t.Errorf("previous %q, diff %+v", rep.Previous, rep.Diff)
	}
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".old") {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

// The final check fails closed: a value the scrub should have replaced stops the write, and
// nothing is left behind.
func TestLeakCheckFailsClosed(t *testing.T) {
	out := t.TempDir()
	final := filepath.Join(out, "CL-1")
	err := writeAll(out, final, map[string][]byte{"v1/a.json": []byte(`{"ok":true}`), "v1/b.json": []byte(`{"x":"` + token + `"}`)}, []string{token})
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "b.json") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestBuildDirAndOrder(t *testing.T) {
	for in, want := range map[string]string{
		"++Wardogs+Live-CL-509546": "CL-509546", "CL-7": "CL-7", "++Wardogs+Beta 2": "Wardogs_Beta_2", "": "unknown-build", "+/+": "_",
	} {
		if got := BuildDir(in); got != want {
			t.Errorf("BuildDir(%q) = %q, want %q", in, got, want)
		}
	}
	builds := []string{"CL-509546", "CL-99999", "CL-501228", "beta"}
	slices.SortFunc(builds, CompareBuilds)
	if !slices.Equal(builds, []string{"CL-99999", "CL-501228", "CL-509546", "beta"}) {
		t.Errorf("order = %v", builds)
	}
}

// A recording of a recording (a fake serving scrubbed fixtures, or a server whose answers echo
// stand-ins) keeps every stand-in as it is: same bytes, and the final check passes.
func TestReRecordingAScrubbedRecordingIsStable(t *testing.T) {
	first := wardogstest.New(t, source(t), wardogstest.Options{Token: token})
	rep1, err := Record(context.Background(), newClient(t, first.URL, token, nil), Options{Dir: t.TempDir(), Token: token, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	second := wardogstest.New(t, rep1.Dir, wardogstest.Options{Token: token})
	rep2, err := Record(context.Background(), newClient(t, second.URL, token, nil), Options{Dir: t.TempDir(), Token: token, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	a, b := readTree(t, rep1.Dir), readTree(t, rep2.Dir)
	for name, body := range a {
		if name != ManifestName && b[name] != body {
			t.Errorf("%s changed when re-recorded", name)
		}
	}
	if s := rep2.Scrubbed; s.SteamIDs+s.Names+s.Addresses+s.ServerIDs != 0 {
		t.Errorf("re-recording scrubbed %+v", s)
	}
}
