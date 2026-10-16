package bot_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gravel-project/gravel/discord/bot"
)

// A PNG's signature and enough after it to tell two images apart.
var (
	pngA = append([]byte("\x89PNG\r\n\x1a\n"), "image A"...)
	pngB = append([]byte("\x89PNG\r\n\x1a\n"), "image B"...)
)

// avatarAPI is Discord's REST API and CDN for the avatar job: the bot user's avatar hash, the
// application's icon hash, and the bytes the CDN serves for each avatar hash. An upload is stored
// under a hash of its bytes, as Discord gives one image one hash.
type avatarAPI struct {
	srv *httptest.Server
	mu  sync.Mutex

	avatar, icon string            // "" for none
	cdn          map[string][]byte // avatar hash → the bytes the CDN serves
	cdnStatus    int               // when not 0, the CDN answers this instead
	refuseUser   bool              // the avatar upload is refused (Discord's rate limit)

	calls   []string // "METHOD path" of every call, the CDN's included
	uploads map[string][]byte
}

func newAvatarAPI(t *testing.T) *avatarAPI {
	t.Helper()
	a := &avatarAPI{cdn: map[string][]byte{}, uploads: map[string][]byte{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := path.Clean(r.URL.Path) // disgo joins the base and an endpoint with two slashes
		a.calls = append(a.calls, r.Method+" "+p)
		if hash, ok := strings.CutPrefix(p, "/cdn/avatars/1558123590786748516/"); ok {
			if r.URL.Query().Get("size") != "4096" {
				t.Errorf("the CDN is asked for size %q, want 4096 (the uploaded bytes)", r.URL.Query().Get("size"))
			}
			if a.cdnStatus != 0 {
				w.WriteHeader(a.cdnStatus)
				return
			}
			b, ok := a.cdn[strings.TrimSuffix(hash, ".png")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		null := func(s string) string {
			if s == "" {
				return "null"
			}
			return `"` + s + `"`
		}
		switch r.Method + " " + p {
		case "GET /api/users/@me":
			_, _ = io.WriteString(w, `{"id":"1558123590786748516","username":"bot","discriminator":"0","bot":true,"avatar":`+null(a.avatar)+`}`)
		case "PATCH /api/users/@me":
			if a.refuseUser {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":50035,"message":"Invalid Form Body","errors":{"avatar":{"_errors":[{"code":"AVATAR_RATE_LIMIT","message":"You are changing your avatar too fast."}]}}}`)
				return
			}
			a.avatar = a.store(t, r, "avatar")
			_, _ = io.WriteString(w, `{"id":"1558123590786748516","username":"bot","discriminator":"0","bot":true,"avatar":`+null(a.avatar)+`}`)
		case "GET /api/applications/@me":
			_, _ = io.WriteString(w, `{"id":"1558123590786748516","name":"Bot","icon":`+null(a.icon)+`}`)
		case "PATCH /api/applications/@me":
			a.icon = a.store(t, r, "icon")
			_, _ = io.WriteString(w, `{"id":"1558123590786748516","name":"Bot","icon":`+null(a.icon)+`}`)
		default:
			t.Errorf("unexpected call %s %s", r.Method, p)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// store decodes an upload's data URI and returns its hash; the CDN serves it from then on.
func (a *avatarAPI) store(t *testing.T, r *http.Request, field string) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 {
		t.Errorf("the upload sends %v, want only %s", body, field)
	}
	b64, ok := strings.CutPrefix(body[field], "data:image/png;base64,")
	if !ok {
		t.Fatalf("%s is not a PNG data URI: %.40q", field, body[field])
	}
	img, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	hash := "h" + strings.TrimPrefix(string(img[8:]), "image ")
	a.cdn[hash] = img
	a.uploads[field] = img
	return hash
}

func (a *avatarAPI) writes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.calls {
		if strings.HasPrefix(c, "PATCH ") {
			out = append(out, c)
		}
	}
	return out
}

func avatarRuntime(t *testing.T, api *avatarAPI, file string) *bot.Runtime {
	t.Helper()
	cfg := testConfig(newSigner(t), newFakeHub(t))
	cfg.Discord.AvatarFile = file
	rt, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", RESTURL: api.srv.URL + "/api/", CDNURL: api.srv.URL + "/cdn/"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt
}

func writeFile(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "avatar.png")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	patchUser = "PATCH /api/users/@me"
	patchApp  = "PATCH /api/applications/@me"
)

func TestAvatarSync(t *testing.T) {
	cases := []struct {
		name         string
		avatar, icon string
		cdn          map[string][]byte
		cdnStatus    int
		refuseUser   bool
		writes       []string
		avatarAfter  string
		iconAfter    string
	}{
		{name: "both match: a restart writes nothing", avatar: "hA", icon: "hA", cdn: map[string][]byte{"hA": pngA}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "a new application: both set", writes: []string{patchUser, patchApp}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "another avatar: both set", avatar: "hB", icon: "hB", cdn: map[string][]byte{"hB": pngB}, writes: []string{patchUser, patchApp}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "the avatar matches, the icon was changed by hand", avatar: "hA", icon: "hB", cdn: map[string][]byte{"hA": pngA}, writes: []string{patchApp}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "the icon matches, the avatar was changed by hand", avatar: "hB", icon: "hA", cdn: map[string][]byte{"hB": pngB}, writes: []string{patchUser}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "an animated avatar is never the file", avatar: "a_x", icon: "hA", writes: []string{patchUser}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "the CDN no longer has the avatar", avatar: "hgone", icon: "hgone", writes: []string{patchUser, patchApp}, avatarAfter: "hA", iconAfter: "hA"},
		{name: "the CDN fails: nothing is written blind", avatar: "hB", icon: "hB", cdnStatus: http.StatusBadGateway, avatarAfter: "hB", iconAfter: "hB"},
		{name: "the avatar upload is refused: the icon waits for the next start", avatar: "hB", icon: "hB", cdn: map[string][]byte{"hB": pngB}, refuseUser: true, writes: []string{patchUser}, avatarAfter: "hB", iconAfter: "hB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAvatarAPI(t)
			api.avatar, api.icon, api.cdnStatus, api.refuseUser = c.avatar, c.icon, c.cdnStatus, c.refuseUser
			for k, v := range c.cdn {
				api.cdn[k] = v
			}
			rt := avatarRuntime(t, api, writeFile(t, pngA))
			if err := bot.SyncAvatar(rt, context.Background()); err != nil {
				t.Fatalf("the job never fails the bot: %v", err)
			}
			if got := api.writes(); !slices.Equal(got, c.writes) {
				t.Errorf("writes %q, want %q", got, c.writes)
			}
			if api.avatar != c.avatarAfter || api.icon != c.iconAfter {
				t.Errorf("after: avatar %q icon %q, want %q %q", api.avatar, api.icon, c.avatarAfter, c.iconAfter)
			}
			for field, img := range api.uploads {
				if !bytes.Equal(img, pngA) {
					t.Errorf("the %s upload is %q, not the file", field, img)
				}
			}
			// A second start finds everything in place, unless Discord refused the first.
			if c.cdnStatus == 0 && !c.refuseUser {
				api.calls = nil
				if err := bot.SyncAvatar(rt, context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := api.writes(); len(got) != 0 {
					t.Errorf("a restart writes %q", got)
				}
			}
		})
	}
}

func TestAvatarBadFile(t *testing.T) {
	dir := t.TempDir()
	big := append(slices.Clone(pngA), make([]byte, bot.MaxAvatarBytes)...)
	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "nope.png"),
		"not a PNG": writeFile(t, []byte("GIF89a…")),
		"too large": writeFile(t, big),
		"empty":     writeFile(t, nil),
	} {
		t.Run(name, func(t *testing.T) {
			api := newAvatarAPI(t)
			api.avatar, api.icon = "hB", "hB"
			rt := avatarRuntime(t, api, path)
			if err := bot.SyncAvatar(rt, context.Background()); err != nil {
				t.Fatalf("a bad file is logged, never fatal: %v", err)
			}
			if len(api.calls) != 0 {
				t.Errorf("a bad file costs no Discord call: %q", api.calls)
			}
		})
	}
}

func TestAvatarJobOnlyWhenConfigured(t *testing.T) {
	api := newAvatarAPI(t)
	if names := bot.JobNames(avatarRuntime(t, api, "")); slices.Contains(names, "runtime/avatar") {
		t.Errorf("no avatar_file, no job: %q", names)
	}
	if names := bot.JobNames(avatarRuntime(t, api, writeFile(t, pngA))); !slices.Contains(names, "runtime/avatar") {
		t.Errorf("avatar_file set, the job runs: %q", names)
	}
	if len(api.calls) != 0 {
		t.Errorf("New talks to Discord about the avatar: %q", api.calls)
	}
}
