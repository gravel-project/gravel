package promquery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// store answers like a Prometheus-compatible store and records what it was asked.
func store(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	got := &http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*got = *r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

var (
	start = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	end   = start.Add(time.Hour)
)

// Prometheus's answer, and VictoriaMetrics' (the same, plus isPartial and stats).
const (
	prometheus = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"instance":"hub:9090","job":"gravel-hub"},"values":[[1792058400,"1.5"],[1792058415.5,"NaN"],[1792058430,"2"]]},
	  {"metric":{"instance":"hub-2:9090"},"values":[[1792058400,"+Inf"]]}]}}`
	victoria = `{"status":"success","isPartial":false,"data":{"resultType":"matrix","result":[
	  {"metric":{"instance":"hub:9090","job":"gravel-hub"},"values":[[1792058400,"1.5"],[1792058430,"2"]]}]},"stats":{"seriesFetched":"1"}}`
)

func TestQueryRangeShapes(t *testing.T) {
	for name, body := range map[string]string{"prometheus": prometheus, "victoriametrics": victoria} {
		t.Run(name, func(t *testing.T) {
			srv, got := store(t, http.StatusOK, body)
			c, err := New(srv.URL+"/prom/", "tok", nil)
			if err != nil {
				t.Fatal(err)
			}
			series, err := c.QueryRange(context.Background(), `rate(x{job="gravel-hub"}[1m])`, start, end, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if got.URL.Path != "/prom/api/v1/query_range" || got.Method != http.MethodPost || got.Header.Get("Authorization") != "Bearer tok" ||
				got.PostForm.Get("query") != `rate(x{job="gravel-hub"}[1m])` || got.PostForm.Get("step") != "30" ||
				got.PostForm.Get("start") != "1792058400.000" || got.PostForm.Get("end") != "1792062000.000" {
				t.Errorf("asked %s %s %v %v", got.Method, got.URL.Path, got.Header, got.PostForm)
			}
			first := series[0]
			if first.Labels["instance"] != "hub:9090" || len(first.Times) != 2 || first.Values[0] != 1.5 || first.Values[1] != 2 ||
				!first.Times[0].Equal(start) || !first.Times[1].Equal(start.Add(30*time.Second)) {
				t.Errorf("first series %+v (NaN left out, times in ms)", first)
			}
			if name == "prometheus" && (len(series) != 2 || series[1].Values[0] <= 1e308) {
				t.Errorf("an +Inf sample: %+v", series)
			}
		})
	}
}

func TestQueryRangeFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"a refusal", http.StatusBadRequest, `{"status":"error","errorType":"bad_data","error":"parse error at char 5"}`, ErrRefused},
		{"an error status on 200", http.StatusOK, `{"status":"error","error":"query timed out in expression evaluation"}`, ErrRefused},
		{"an instant vector", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`, ErrRefused},
		{"the store failing", http.StatusServiceUnavailable, `{"status":"error","error":"overloaded"}`, ErrUnavailable},
		{"not the query API", http.StatusOK, `<html>login</html>`, ErrUnavailable},
	}
	for _, tc := range cases {
		srv, _ := store(t, tc.status, tc.body)
		c, _ := New(srv.URL, "", nil)
		if _, err := c.QueryRange(context.Background(), "up", start, end, time.Minute); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	srv, _ := store(t, http.StatusOK, prometheus)
	c, _ := New(srv.URL, "", nil)
	if _, err := c.WithMaxBytes(64).QueryRange(context.Background(), "up", start, end, time.Minute); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an answer over the cap: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // past the client's timeout, and always over, so the server can close
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer slow.Close()
	c, _ = New(slow.URL, "", &http.Client{Timeout: 50 * time.Millisecond})
	if _, err := c.QueryRange(context.Background(), "up", start, end, time.Minute); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a store that never answers: %v", err)
	}
	if _, err := New("ftp://x", "", nil); err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Errorf("a non-http store: %v", err)
	}
}
