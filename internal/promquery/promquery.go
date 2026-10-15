// Package promquery asks a Prometheus-compatible metrics store (Prometheus, VictoriaMetrics) for a
// range query over its HTTP API, /api/v1/query_range (gravel#204, ADR-0030). It is the one place
// the hub reads a metrics store; the hub never keeps what it reads.
package promquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrUnavailable is a store that didn't answer, timed out, or failed on its side (5xx).
	ErrUnavailable = errors.New("the metrics store did not answer")
	// ErrRefused is a store that refused the query (4xx, or status "error"): the message says why.
	ErrRefused = errors.New("the metrics store refused the query")
	// ErrTooLarge is an answer over the client's size cap.
	ErrTooLarge = errors.New("the metrics store's answer is too large")
)

// DefaultMaxBytes caps an answer: a dashboard panel over a month at the finest step stays well under.
const DefaultMaxBytes = 8 << 20

// Series is one time series of a range query: its labels and its samples, oldest first. A sample
// the store reports as NaN is left out.
type Series struct {
	Labels map[string]string
	Times  []time.Time
	Values []float64
}

// Client queries one store.
type Client struct {
	base     *url.URL
	token    string
	http     *http.Client
	maxBytes int64
}

// New is a client for the store at base (http(s)), sending token as a bearer token when it is not
// empty. hc's timeout bounds a query; a nil hc is http.DefaultClient's transport with none.
func New(base, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("promquery: %q is not an http(s) address", base)
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: u, token: token, http: hc, maxBytes: DefaultMaxBytes}, nil
}

// WithMaxBytes changes the size cap (tests).
func (c *Client) WithMaxBytes(n int64) *Client {
	c.maxBytes = n
	return c
}

// QueryRange runs a range query. The expression is the caller's to trust: the hub sends only its
// dashboards' own.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	form := url.Values{
		"query": {expr},
		"start": {strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64)},
		"end":   {strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	u := c.base.JoinPath("api", "v1", "query_range")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("promquery: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if int64(len(body)) > c.maxBytes {
		return nil, fmt.Errorf("%w (over %d bytes)", ErrTooLarge, c.maxBytes)
	}
	var out struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	jsonErr := json.Unmarshal(body, &out)
	switch {
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s %s", ErrUnavailable, resp.Status, out.Error)
	case jsonErr != nil:
		return nil, fmt.Errorf("%w: %s, not the query API's JSON", ErrUnavailable, resp.Status)
	case resp.StatusCode >= 400 || out.Status != "success":
		return nil, fmt.Errorf("%w: %s", ErrRefused, cmpString(out.Error, resp.Status))
	case out.Data.ResultType != "matrix":
		return nil, fmt.Errorf("%w: a %q result, not a matrix", ErrRefused, out.Data.ResultType)
	}
	series := make([]Series, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		s := Series{Labels: r.Metric}
		for _, pair := range r.Values {
			t, ok := pair[0].(float64)
			text, ok2 := pair[1].(string)
			if !ok || !ok2 {
				return nil, fmt.Errorf("%w: a sample is not [time, \"value\"]", ErrRefused)
			}
			v, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(v) {
				continue
			}
			s.Times = append(s.Times, time.UnixMilli(int64(math.Round(t*1000))))
			s.Values = append(s.Values, v)
		}
		series = append(series, s)
	}
	return series, nil
}

func cmpString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
