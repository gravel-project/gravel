package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/uuid"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/promquery"
	"github.com/gravel-project/gravel/internal/store"
)

// The operator dashboards through the hub (gravel#204): a caller names a panel and the hub runs that
// panel's own expressions, so no PromQL from a caller reaches the store.
func TestMetricsService(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	op := r.user(t, "Op", "2")
	r.grant(t, owner.ID, op.ID, store.RoleOperator)
	mod := r.user(t, "Mod", "3")
	r.grant(t, owner.ID, mod.ID, store.RoleModerator)
	client := func(id uuid.UUID) hubv1connect.MetricsServiceClient {
		return hubv1connect.NewMetricsServiceClient(cookieClient(http.DefaultClient, r.login(t, id)), r.srv.URL)
	}
	c := client(op.ID)
	if _, err := client(mod.ID).ListDashboards(ctx, connect.NewRequest(&hubv1.ListDashboardsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a moderator: %v", err)
	}

	list, err := c.ListDashboards(ctx, connect.NewRequest(&hubv1.ListDashboardsRequest{}))
	if err != nil || !list.Msg.GetAvailable() || len(list.Msg.GetDashboards()) != 2 || list.Msg.GetDashboards()[0].GetUid() != "gravel-hub" {
		t.Fatalf("dashboards: %v %v", list, err)
	}
	// The API row's first panel: calls by procedure.
	var panel *hubv1.MetricsPanel
	for _, row := range list.Msg.GetDashboards()[0].GetRows() {
		for _, p := range row.GetPanels() {
			if p.GetTitle() == "Calls by procedure" {
				panel = p
			}
		}
	}
	if panel == nil || panel.GetId() == "" || panel.GetKind() != "timeseries" || panel.GetUnit() != "reqps" {
		t.Fatalf("the panel: %v", panel)
	}

	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	r.store.series = []promquery.Series{{Labels: map[string]string{"procedure": "/gravel.hub.v1.ServerService/ListServers"},
		Times: []time.Time{start, start.Add(time.Minute)}, Values: []float64{1.5, 2}}}
	ask := func(uid, id string, from, to time.Time, step time.Duration) (*connect.Response[hubv1.QueryRangeResponse], error) {
		return c.QueryRange(ctx, connect.NewRequest(&hubv1.QueryRangeRequest{DashboardUid: uid, PanelId: id,
			Start: timestamppb.New(from), End: timestamppb.New(to), Step: durationpb.New(step)}))
	}
	got, err := ask("gravel-hub", panel.GetId(), start, start.Add(time.Hour), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := got.Msg.GetSeries()
	if len(s) != 1 || s[0].GetName() != "/gravel.hub.v1.ServerService/ListServers" || len(s[0].GetTimes()) != 2 ||
		s[0].GetTimes()[1] != start.Add(time.Minute).UnixMilli() || s[0].GetValues()[1] != 2 {
		t.Errorf("series %v", s)
	}
	// What reached the store: the definition's expression, the hub's job, every instance, no variable.
	expr := r.store.exprs[0]
	if !strings.Contains(expr, `gravel_rpc_requests_total{job="gravel-hub", instance=~".+"}`) || !strings.Contains(expr, "[60s]") || strings.Contains(expr, "$") {
		t.Errorf("the store ran %s", expr)
	}
	// A finer step than the store can serve is widened: 30 days at 15 s would be 172,800 points.
	got, err = ask("gravel-hub", panel.GetId(), start.Add(-30*24*time.Hour), start, time.Second)
	if err != nil || got.Msg.GetStep().AsDuration() < 30*24*time.Hour/11_000 || r.store.steps[1] != got.Msg.GetStep().AsDuration() {
		t.Errorf("a month at 1s: step %v (store %v), %v", got.Msg.GetStep().AsDuration(), r.store.steps, err)
	}

	for name, call := range map[string]func() error{
		"an unknown panel": func() error {
			_, err := ask("gravel-hub", "r99p0", start, start.Add(time.Hour), time.Minute)
			return err
		},
		"another dashboard id": func() error {
			_, err := ask("node-exporter", panel.GetId(), start, start.Add(time.Hour), time.Minute)
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ask("gravel-hub", panel.GetId(), start, start, time.Minute); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an empty range: %v", err)
	}
	if _, err := ask("gravel-hub", panel.GetId(), start.Add(-31*24*time.Hour), start, time.Hour); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("31 days: %v", err)
	}
	if n := len(r.store.exprs); n != 2 {
		t.Errorf("refused requests reached the store: %d queries", n)
	}

	for _, tc := range []struct {
		err  error
		want connect.Code
	}{
		{fmt.Errorf("%w: dial tcp: refused", promquery.ErrUnavailable), connect.CodeUnavailable},
		{fmt.Errorf("%w: unknown function", promquery.ErrRefused), connect.CodeFailedPrecondition},
		{promquery.ErrTooLarge, connect.CodeResourceExhausted},
	} {
		r.store.err = tc.err
		if _, err := ask("gravel-hub", panel.GetId(), start, start.Add(time.Hour), time.Minute); connect.CodeOf(err) != tc.want {
			t.Errorf("%v: %v, want %v", tc.err, err, tc.want)
		}
	}
	// One count per query that reached the gate of the store, by its result.
	if got := strings.Join(r.results, ","); got != "ok,ok,unavailable,refused,too_large" {
		t.Errorf("counted %s", got)
	}
}
