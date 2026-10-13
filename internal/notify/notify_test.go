package notify_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/org"
)

var t0 = time.Date(2026, 10, 13, 22, 0, 0, 0, time.UTC)

func deps(secret string) notify.Deps {
	return notify.Deps{Secret: secret, Hub: "HTG", Link: "https://hub.example.com/admin", Now: func() time.Time { return t0 }}
}

func unreachable(id int64) notify.Event {
	return notify.Event{ID: id, At: t0, Kind: "server.unreachable", Severity: events.Warning, ServerID: "htg-wardogs-1", GameID: "wardogs",
		Summary: "htg-wardogs-1 stopped answering its polls", Detail: map[string]any{"misses": 3}}
}

// sink is an HTTP receiver that records each request.
type sink struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []*http.Request
	bodies [][]byte
	status int
}

func newSink(t *testing.T) *sink {
	s := &sink{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs, s.bodies = append(s.reqs, r), append(s.bodies, b)
		status := s.status
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sink) setStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

func TestWebhookJSONIsSigned(t *testing.T) {
	s := newSink(t)
	n, err := notify.New(org.Notifier{Name: "ops-hook", Kind: org.NotifierWebhook, URL: s.URL}, deps("signing-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), []notify.Event{unreachable(7), unreachable(8)}); err != nil {
		t.Fatal(err)
	}
	r, body := s.reqs[0], s.bodies[0]
	if err := notify.Verify("signing-key", r.Header, body, t0.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("a receiver's check: %v", err)
	}
	if err := notify.Verify("other-key", r.Header, body, t0, 5*time.Minute); err == nil {
		t.Error("the wrong key verifies")
	}
	if err := notify.Verify("signing-key", r.Header, body, t0.Add(time.Hour), 5*time.Minute); err == nil {
		t.Error("a replay an hour later verifies")
	}
	if r.Header.Get(notify.HeaderTimestamp) != strconv.FormatInt(t0.Unix(), 10) || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", r.Header)
	}
	var got notify.WebhookBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Hub != "HTG" || got.Link != "https://hub.example.com/admin" || len(got.Events) != 2 ||
		got.Events[0].ID != 7 || got.Events[0].Kind != "server.unreachable" || got.Events[0].Severity != "warning" || got.Events[0].ServerID != "htg-wardogs-1" {
		t.Errorf("body %s", body)
	}
	if _, err := notify.New(org.Notifier{Name: "ops-hook", Kind: org.NotifierWebhook, URL: s.URL}, deps("")); err == nil ||
		!strings.Contains(err.Error(), "notify.secrets.ops-hook.file") {
		t.Errorf("a JSON webhook needs its signing key: %v", err)
	}
}

func TestWebhookDiscordFlavour(t *testing.T) {
	s := newSink(t)
	hook := s.URL + "/api/webhooks/1/secret-token"
	// The test server is http; a real Discord URL is https, which the factory insists on.
	if _, err := notify.New(org.Notifier{Name: "discord", Kind: org.NotifierWebhook, Flavour: org.FlavourDiscord}, deps(hook)); err == nil {
		t.Fatal("an http Discord URL is refused")
	}
	n, err := notify.New(org.Notifier{Name: "discord", Kind: org.NotifierWebhook, Flavour: org.FlavourDiscord},
		notify.Deps{Secret: strings.Replace(hook, "http://", "https://", 1), Hub: "HTG", Client: s.Client()})
	if err != nil {
		t.Fatal(err)
	}
	// The https URL can't reach the plain test server: the error must not carry the token.
	err = n.Notify(context.Background(), []notify.Event{unreachable(1)})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("an error carries the webhook's token: %v", err)
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs, s.bodies = append(s.reqs, r), append(s.bodies, b)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	n, err = notify.New(org.Notifier{Name: "discord", Kind: org.NotifierWebhook, Flavour: org.FlavourDiscord},
		notify.Deps{Secret: ts.URL + "/api/webhooks/1/secret-token", Hub: "HTG", Client: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	evs := make([]notify.Event, 0, 40)
	for i := range 40 {
		e := unreachable(int64(i))
		e.Summary = strings.Repeat("long words ", 10)
		evs = append(evs, e)
	}
	if err := n.Notify(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Username        string `json:"username"`
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(s.bodies[len(s.bodies)-1], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Username != "HTG" || len(msg.Content) > 2000 || !strings.Contains(msg.Content, "⚠️ server.unreachable · htg-wardogs-1") ||
		!strings.Contains(msg.Content, "more") || msg.AllowedMentions.Parse == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Errorf("message %d bytes: %+v", len(msg.Content), msg)
	}
}

func TestNtfy(t *testing.T) {
	s := newSink(t)
	n, err := notify.New(org.Notifier{Name: "john-phone", Kind: org.NotifierNtfy, URL: s.URL + "/", Topic: "htg-ops"}, deps("tk_access"))
	if err != nil {
		t.Fatal(err)
	}
	crit := notify.Event{ID: 9, At: t0, Kind: "backup.failed", Severity: events.Critical, Summary: "the last base backup failed"}
	if err := n.Notify(context.Background(), []notify.Event{unreachable(8), crit}); err != nil {
		t.Fatal(err)
	}
	r := s.reqs[0]
	if r.URL.Path != "/" || r.Header.Get("Authorization") != "Bearer tk_access" {
		t.Errorf("request %s %v", r.URL, r.Header)
	}
	var msg struct {
		Topic, Title, Message, Click string
		Priority                     int
		Tags                         []string
	}
	if err := json.Unmarshal(s.bodies[0], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Topic != "htg-ops" || msg.Title != "HTG: 2 events" || msg.Priority != 5 || msg.Tags[0] != "rotating_light" ||
		msg.Click != "https://hub.example.com/admin" || !strings.Contains(msg.Message, "backup.failed · the last base backup failed") {
		t.Errorf("message %+v", msg)
	}
	// One event: its kind is the title, its severity the priority; no token, no header.
	n, _ = notify.New(org.Notifier{Name: "p", Kind: org.NotifierNtfy, URL: s.URL, Topic: "t"}, deps(""))
	if err := n.Notify(context.Background(), []notify.Event{unreachable(1)}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(s.bodies[1], &msg)
	if msg.Title != "HTG: server.unreachable" || msg.Priority != 4 || s.reqs[1].Header.Get("Authorization") != "" {
		t.Errorf("one event %+v", msg)
	}
	s.setStatus(http.StatusTooManyRequests)
	if err := n.Notify(context.Background(), []notify.Event{unreachable(1)}); err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("a refusal is a failure: %v", err)
	}
}

// fakeSMTP is a mail server that takes every message, for the email notifier's test.
type fakeSMTP struct {
	addr string
	mu   sync.Mutex
	rcpt []string
	data []string
	auth bool
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	f := &fakeSMTP{addr: l.Addr().String()}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(l))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-fake")
			say("250-AUTH PLAIN")
			say("250 8BITMIME")
		case strings.HasPrefix(cmd, "AUTH"):
			f.mu.Lock()
			f.auth = true
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, strings.Trim(strings.TrimSpace(l)[8:], "<>"))
			f.mu.Unlock()
			say("250 ok")
		case cmd == "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" {
					break
				}
				b.WriteString(dl)
			}
			f.mu.Lock()
			f.data = append(f.data, b.String())
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestEmail(t *testing.T) {
	f := newFakeSMTP(t)
	host, port, _ := net.SplitHostPort(f.addr)
	p, _ := strconv.Atoi(port)
	d := deps("")
	d.SMTP = notify.SMTP{Host: host, Port: p, From: "hub@example.com", TLS: "none"}
	n, err := notify.New(org.Notifier{Name: "mail", Kind: org.NotifierEmail, To: []string{"ops@example.com", "john@example.com"}}, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), []notify.Event{unreachable(3)}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.rcpt, ",") != "ops@example.com,john@example.com" || len(f.data) != 1 || f.auth {
		t.Fatalf("rcpt %v, %d messages, auth %v", f.rcpt, len(f.data), f.auth)
	}
	head, body, _ := strings.Cut(f.data[0], "\r\n\r\n")
	text, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	msg := head + "\n" + string(text)
	for _, want := range []string{"Subject: HTG: server.unreachable", "From: <hub@example.com>", "2026-10-13 22:00:00 UTC  warning  server.unreachable · htg-wardogs-1", "https://hub.example.com/admin"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, msg)
		}
	}
	if _, err := notify.New(org.Notifier{Name: "mail", Kind: org.NotifierEmail, To: []string{"a@b.c"}}, deps("")); err == nil || !strings.Contains(err.Error(), "notify.smtp") {
		t.Errorf("email needs a mail server: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 7: 640 * time.Second / 1, 8: notify.MaxBackoff, 50: notify.MaxBackoff} {
		if got := notify.Backoff(n); got != min(want, notify.MaxBackoff) {
			t.Errorf("Backoff(%d) = %v, want %v", n, got, want)
		}
	}
}
