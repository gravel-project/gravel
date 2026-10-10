package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/bot/stock"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

const shipped = "../../../deploy/observability/grafana/dashboards"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The files on disk are what gen writes from defs.go (make generate-check guards it in CI; this
// catches it in make test too).
func TestShippedDashboardsAreGenerated(t *testing.T) {
	dir := t.TempDir()
	if err := gen(dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range dashboards() {
		want, _ := os.ReadFile(filepath.Join(dir, d.UID+".json"))
		got, err := os.ReadFile(filepath.Join(shipped, d.UID+".json"))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s.json is stale (run make generate): %v", d.UID, err)
		}
	}
}

// Every gravel_* name a panel reads is one the hub or the bot registers, and every one they
// register is on a panel: a renamed or new metric fails here until the dashboards follow.
func TestDashboardsMatchTheMetrics(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.URL = storetest.DatabaseURL(t) // skips without a test database
	cfg.Database.ConnectTimeout = 10 * time.Second
	cfg.Server.Listen, cfg.Server.InternalListen = "127.0.0.1:0", "127.0.0.1:0"
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)

	bcfg := bot.Default() // role sync and Linked Roles on: every stock module
	bcfg.Discord.ApplicationID = "1558123590786748516"
	bcfg.Discord.PublicKey = hex.EncodeToString(make([]byte, 32))
	bcfg.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(bcfg.Discord.ApplicationID)) + ".Xa1b2c.not-a-real-token"
	bcfg.Discord.Gateway = false
	bcfg.Moderation.Enabled = true // off by default; on here so its metric is registered
	bcfg.Hub.URL, bcfg.Hub.PublicURL, bcfg.Hub.ClientID, bcfg.Hub.ClientSecret = "http://hub.invalid:8080", "https://app.example.com", "gravel_x", "s"
	hc, err := hubclient.New(hubclient.Config{URL: bcfg.Hub.URL, ClientID: bcfg.Hub.ClientID, ClientSecret: bcfg.Hub.ClientSecret})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := bot.New(bot.Options{Config: bcfg, Logger: quiet(), Version: "test"}, stock.Modules(bcfg, hc)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(ctx) })

	registered := map[string]string{}
	for _, n := range h.MetricNames() {
		registered[n] = "hub"
	}
	for _, n := range rt.MetricNames() {
		registered[n] = "bot"
	}
	ds, err := readDashboards(shipped)
	if err != nil {
		t.Fatal(err)
	}
	queried := map[string]string{}
	for _, d := range ds {
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				for _, n := range metricNames(tg.Expr) {
					queried[n] = d.UID + " / " + p.Title
					if _, ok := registered[n]; !ok {
						t.Errorf("%s / %s reads %s, which neither the hub nor the bot registers", d.UID, p.Title, n)
					}
				}
			}
		}
	}
	var missing []string
	for n, who := range registered {
		if strings.HasPrefix(n, "gravel_") && queried[n] == "" {
			missing = append(missing, n+" ("+who+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("registered but on no panel: %s", strings.Join(missing, ", "))
	}
	if len(registered) < 20 {
		t.Errorf("only %d names registered: is MetricNames measuring?", len(registered))
	}
}

func TestMetricNamesAndExpand(t *testing.T) {
	got := metricNames(`histogram_quantile(0.95, sum by (le) (rate(gravel_http_request_duration_seconds_bucket{job="$job"}[$__rate_interval]))) / gravel_users_total + gravel_x_seconds_sum`)
	if strings.Join(got, ",") != "gravel_http_request_duration_seconds,gravel_users_total,gravel_x_seconds" {
		t.Errorf("names: %v", got)
	}
	e := expand(`rate(x{job="$job", instance=~"$instance"}[$__rate_interval])`, map[string]string{"job": "gravel-hub", "instance": ".+"})
	if e != `rate(x{job="gravel-hub", instance=~".+"}[5m])` {
		t.Errorf("expand: %s", e)
	}
}

func TestRulesHasOneRulePerTarget(t *testing.T) {
	var b bytes.Buffer
	if err := rules(shipped, &b); err != nil {
		t.Fatal(err)
	}
	ds, _ := readDashboards(shipped)
	want := 0
	for _, d := range ds {
		for _, p := range d.Panels {
			want += len(p.Targets)
		}
	}
	if got := strings.Count(b.String(), "- record: "); got != want || want == 0 || strings.Contains(b.String(), "$") {
		t.Errorf("%d rules for %d targets, or a variable left in:\n%s", got, want, b.String())
	}
}

// check against a fake Grafana: data, optional without data, required without data, a refused
// query, a dashboard that isn't provisioned.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(uid string, panels []map[string]any) {
		b, _ := json.Marshal(map[string]any{"uid": uid, "panels": panels, "templating": map[string]any{"list": []any{
			map[string]any{"name": "job", "definition": "label_values(gravel_build_info, job)"},
		}}})
		if err := os.WriteFile(filepath.Join(dir, uid+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tgt := func(expr string) []map[string]any { return []map[string]any{{"refId": "A", "expr": expr}} }
	write("good", []map[string]any{
		{"id": 1, "type": "row", "title": "Row"},
		{"id": 2, "type": "stat", "title": "Has data", "targets": tgt(`has_data{job="$job"}`)},
		{"id": 3, "type": "stat", "title": "Optional", "gravelOptional": "nothing happened", "targets": tgt("empty")},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/datasources":
			_, _ = io.WriteString(w, `[{"uid":"loki1","type":"loki"},{"uid":"prom1","type":"prometheus"}]`)
		case strings.HasPrefix(r.URL.Path, "/api/dashboards/uid/"):
			if strings.HasSuffix(r.URL.Path, "/missing") {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/api/datasources/proxy/uid/prom1/api/v1/label/job/values"):
			_, _ = io.WriteString(w, `{"data":["zeta","gravel-hub"]}`)
		case r.URL.Path == "/api/ds/query":
			var q struct {
				Queries []struct{ Expr string } `json:"queries"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			expr := q.Queries[0].Expr
			switch {
			case expr == `has_data{job="gravel-hub"}`: // the job resolved to the first, sorted
				_, _ = io.WriteString(w, `{"results":{"A":{"frames":[{"data":{"values":[[1],[2]]}}]}}}`)
			case strings.Contains(expr, "bad("):
				_, _ = io.WriteString(w, `{"results":{"A":{"error":"parse error"}}}`)
			default:
				_, _ = io.WriteString(w, `{"results":{"A":{"frames":[{"data":{"values":[[],[]]}}]}}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	pw := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pw, []byte("pw\n"), 0o600)
	runCheck := func(extra ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), append([]string{"check", "--dir", dir, "--grafana", srv.URL, "--password-file", pw}, extra...), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := runCheck(); code != 0 || !strings.Contains(out, "ok   good / Has data") || !strings.Contains(out, "--   good / Optional: no data (expected when nothing happened)") {
		t.Errorf("good: %d\n%s", code, out)
	}

	write("bad", []map[string]any{
		{"id": 1, "type": "stat", "title": "Required", "targets": tgt("empty")},
		{"id": 2, "type": "stat", "title": "Refused", "gravelOptional": "even optional", "targets": tgt("bad(")},
	})
	write("missing", []map[string]any{{"id": 1, "type": "stat", "title": "x", "targets": tgt("x")}})
	code, out := runCheck()
	for _, want := range []string{"FAIL bad / Required: no data", "FAIL bad / Refused: A: parse error", "FAIL missing: not provisioned", "3 panel(s) failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if code != 1 {
		t.Errorf("failures exit 1: %d", code)
	}
	if code, out := runCheck("--skip", "bad,missing"); code != 0 || !strings.Contains(out, "bad: skipped") {
		t.Errorf("skip: %d\n%s", code, out)
	}
	if code := run(context.Background(), []string{"check", "--dir", dir}, io.Discard, io.Discard); code != 1 {
		t.Errorf("no password file: %d", code)
	}
	if code := run(context.Background(), []string{"nope"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
}

// job and instance are the scrape target's labels, which sel already pins; a metric's own label of
// that name is stored as exported_job. A panel that groups, labels or matches by job otherwise
// collapses every series into the scrape job (#114).
func TestPanelsUseExportedJob(t *testing.T) {
	for _, d := range dashboards() {
		for _, r := range d.Rows {
			for _, p := range r.Panels {
				for _, tg := range p.Targets {
					// exported_job is the right label; mask it so its "job" doesn't count.
					expr := strings.ReplaceAll(strings.ReplaceAll(tg.Expr, sel, ""), "exported_job", "exported_JOB")
					legend := strings.ReplaceAll(tg.Legend, "exported_job", "exported_JOB")
					for _, bad := range []string{"by (job", "by(job", ", job)", ", job,", "job=", "job!", "{{job}}"} {
						if strings.Contains(expr, bad) || strings.Contains(legend, bad) {
							t.Errorf("%s / %s: %q uses the scrape label job; group, label and match by exported_job", d.Title, p.Title, tg.Expr+" "+tg.Legend)
						}
					}
				}
			}
		}
	}
}
