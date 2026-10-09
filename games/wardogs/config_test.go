package wardogs_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

func TestRedactConfig(t *testing.T) {
	in := "[/Script/WDRCON.WDRCONSettings]\r\nbEnabled=true\r\nPassword=hunter2\r\nPasswordHash=abc\r\nPort=7789\r\n\r\n" +
		"[/Script/WDGame.WDGameSession]\r\nServerName=My Server\r\nServerPassword=\r\n\r\n" +
		"[WDServerFeed]\r\nUrl=https://sink.example/feed\r\nToken=t0k\r\n\r\n" +
		"[Other]\r\nUrl=https://keep.example\r\n+Token=x\n"
	want := "[/Script/WDRCON.WDRCONSettings]\r\nbEnabled=true\r\nPassword=<redacted>\r\nPasswordHash=<redacted>\r\nPort=7789\r\n\r\n" +
		"[/Script/WDGame.WDGameSession]\r\nServerName=My Server\r\nServerPassword=\r\n\r\n" +
		"[WDServerFeed]\r\nUrl=<redacted>\r\nToken=<redacted>\r\n\r\n" +
		"[Other]\r\nUrl=https://keep.example\r\n+Token=<redacted>\n"
	if got := wardogs.RedactConfig(in); got != want {
		t.Errorf("redacted =\n%q\nwant\n%q", got, want)
	}
	if got := wardogs.RedactConfig(want); got != want {
		t.Error("redacting twice changed the document")
	}
}

func TestLineEndings(t *testing.T) {
	if got := wardogs.ToCRLF("a\nb\r\nc\rd"); got != "a\r\nb\r\nc\r\nd" {
		t.Errorf("ToCRLF = %q", got)
	}
	if got := wardogs.NormalizeEOL("a\r\nb\rc"); got != "a\nb\nc" {
		t.Errorf("NormalizeEOL = %q", got)
	}
}

func TestTheRecordedDocumentIsRedacted(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	d, err := newClient(t, srv.URL, wardogstest.Token).Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text, "\r\nPassword=<redacted>\r\n") {
		t.Error("the recorded document's RCON password is not redacted")
	}
	if wardogs.RedactConfig(d.Text) != d.Text {
		t.Error("the recorded document holds a secret RedactConfig would blank")
	}
}

// The schema answers what a plan needs per key: honoured, writable, locked by what, and when it
// takes effect (from the recorded build).
func TestConfigSchema(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	d, err := newClient(t, srv.URL, wardogstest.Token).Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		section, key string
		ok, writable bool
		lockedBy     string
		applies      string
	}{
		{"/Script/WDRCON.WDRCONSettings", "Port", true, false, "RCONPort", wardogs.AppliesNextRestart},
		{"/Script/WDRCON.WDRCONSettings", "Password", true, true, "", wardogs.AppliesNextRestart},
		{"/Script/WDGame.WDGameSession", "ServerImageURL", true, true, "", wardogs.AppliesPending},
		{"/Script/WDGame.WDGameSession", "servername", true, true, "", wardogs.AppliesNow},
		{"MatchState.Playing.KOTH", "ScorePeriod", true, true, "", wardogs.AppliesNextMatch},
		{"/Script/WDGame.WDGameSession", "TickRate", false, false, "", ""},
		{"NoSuchSection", "Key", false, false, "", ""},
	} {
		k, ok := d.Key(c.section, c.key)
		if ok != c.ok || k.Writable != c.writable || k.LockedBy != c.lockedBy || k.AppliesWhen != c.applies {
			t.Errorf("%s %s = %+v, %v", c.section, c.key, k, ok)
		}
	}
}

func TestPutConfig(t *testing.T) {
	type seen struct{ query, ifMatch, ctype, body string }
	var (
		mu  sync.Mutex
		got []seen
	)
	status, answer := http.StatusOK, `{"ok":true,"revision":"r2","changed":[{"section":"MatchState.Playing.KOTH","added":false,"removed":false,"keys":["ScorePeriod"]}],"outcomes":[{"section":"MatchState.Playing.KOTH","state":"next-match","detail":"read at the next match"}],"shadowed":[],"stripped":[{"section":"X","reason":"unknown section"}],"warnings":["pinned"]}`
	put := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.URL.RawQuery, r.Header.Get("If-Match"), r.Header.Get("Content-Type"), string(b)})
		st, a := status, answer
		mu.Unlock()
		w.WriteHeader(st)
		_, _ = io.WriteString(w, a)
	}
	srv := wardogstest.New(t, latest(t), wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"PUT /v1/config": put, "POST /v1/config/validate": put,
	}})
	c := newClient(t, srv.URL, wardogstest.Token)
	ctx := context.Background()

	res, err := c.PutConfig(ctx, "[MatchState.Playing.KOTH]\nScorePeriod=27\n", "r1", wardogs.ApplyOptions{Force: true, FullApply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Revision != "r2" || res.Outcomes[0].State != wardogs.AppliesNextMatch ||
		res.Changed[0].Keys[0] != "ScorePeriod" || res.Stripped[0].Reason != "unknown section" || res.Warnings[0] != "pinned" {
		t.Errorf("result = %+v", res)
	}
	mu.Lock()
	first := got[0]
	mu.Unlock()
	if first.ifMatch != `"r1"` || first.ctype != "text/plain" || first.body != "[MatchState.Playing.KOTH]\r\nScorePeriod=27\r\n" ||
		first.query != "force=true&fullApply=true" {
		t.Errorf("request = %+v", first)
	}

	mu.Lock()
	status, answer = http.StatusPreconditionFailed, `{"ok":false,"error":{"code":"revision_mismatch","message":"the document changed"},"conflict":[{"section":"X"}]}`
	mu.Unlock()
	res, err = c.PutConfig(ctx, "x", `"r1"`, wardogs.ApplyOptions{})
	if !errors.Is(err, wardogs.ErrRevisionMismatch) || res.Error == nil || res.Error.Code != "revision_mismatch" || len(res.Conflict) != 1 {
		t.Errorf("412 = %+v, %v", res, err)
	}
	mu.Lock()
	if got[1].ifMatch != `"r1"` || got[1].query != "" {
		t.Errorf("an already-quoted revision was sent as %q, query %q", got[1].ifMatch, got[1].query)
	}
	status, answer = http.StatusUnprocessableEntity, `{"ok":false,"errors":[{"section":"MatchState.Playing.KOTH","key":"ScorePeriod","code":"out_of_range","message":"18-30"}]}`
	mu.Unlock()
	res, err = c.ValidateConfig(ctx, "x")
	if !errors.Is(err, wardogs.ErrConfigRejected) || len(res.Errors) != 1 || res.Errors[0].Code != "out_of_range" {
		t.Errorf("422 = %+v, %v", res, err)
	}
	mu.Lock()
	if got[2].ifMatch != "" {
		t.Errorf("validate sent If-Match %q", got[2].ifMatch)
	}
	mu.Unlock()

	if _, err := c.PutConfig(ctx, "x", "", wardogs.ApplyOptions{}); err == nil {
		t.Error("a write without a revision was sent")
	}
}

func TestAReadOnlyDocumentIsNotWritten(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{ReadOnly: true})
	c := newClient(t, srv.URL, wardogstest.Token)
	if _, err := c.PutConfig(context.Background(), "x", "r1", wardogs.ApplyOptions{}); !errors.Is(err, wardogs.ErrNotSupported) {
		t.Errorf("write to a read-only document = %v", err)
	}
	for _, r := range srv.Requests() {
		if r.Method == "PUT" {
			t.Error("a PUT was sent")
		}
	}
}
