package news

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SteamURL is the origin of Steam's Web API.
const SteamURL = "https://api.steampowered.com"

const (
	// httpTimeout bounds one call to Steam.
	httpTimeout = 15 * time.Second
	// maxBody bounds what is read of Steam's answer: ten announcements with their full BBCode.
	maxBody = 2 << 20
	// newsCount is how many of an app's latest items are asked for, as the Worker did.
	newsCount = 10
	// defaultRetry is the back-off for a 429 that names no time.
	defaultRetry = time.Minute
	// maxRetry caps a 429's back-off.
	maxRetry = 15 * time.Minute
)

// HTTPClient is the client Steam is called with.
func HTTPClient() *http.Client { return &http.Client{Timeout: httpTimeout} }

// Item is one entry of ISteamNews/GetNewsForApp's newsitems.
type Item struct {
	GID      string
	Title    string
	Contents string // Steam's BBCode
	Date     time.Time
	FeedName string
	AppID    int
}

// ThrottledError is Steam's rate limit: ask again after RetryAfter.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
}

// Steam reads an app's news from Steam's public API, which needs no key.
type Steam struct {
	base string
	hc   *http.Client
}

// NewSteam returns the client for the API at base (SteamURL when empty), called with hc
// (HTTPClient when nil).
func NewSteam(base string, hc *http.Client) *Steam {
	if base == "" {
		base = SteamURL
	}
	if hc == nil {
		hc = HTTPClient()
	}
	return &Steam{base: strings.TrimSuffix(base, "/"), hc: hc}
}

// News returns the app's latest items in feeds, as Steam lists them (newest first). Errors are a
// *ThrottledError or any other failure; on an error nothing is known.
func (s *Steam) News(ctx context.Context, appID int, feeds []string) ([]Item, error) {
	q := url.Values{"appid": {strconv.Itoa(appID)}, "count": {strconv.Itoa(newsCount)}, "feeds": {strings.Join(feeds, ",")}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/ISteamNews/GetNewsForApp/v2/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, &ThrottledError{RetryAfter: retryAfter(resp.Header, time.Now())}
	default:
		return nil, fmt.Errorf("news from Steam: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AppNews *struct {
			NewsItems []struct {
				GID      json.RawMessage `json:"gid"`
				Title    string          `json:"title"`
				Contents string          `json:"contents"`
				Date     int64           `json:"date"`
				FeedName string          `json:"feedname"`
				AppID    int             `json:"appid"`
			} `json:"newsitems"`
		} `json:"appnews"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("news from Steam: decode the answer: %w", err)
	}
	if out.AppNews == nil || out.AppNews.NewsItems == nil {
		return nil, errors.New("news from Steam: no appnews.newsitems in the answer")
	}
	items := make([]Item, 0, len(out.AppNews.NewsItems))
	for _, n := range out.AppNews.NewsItems {
		// A gid that isn't a string of digits could never be found again in the channel's links.
		var gid string
		if !bytes.HasPrefix(n.GID, []byte(`"`)) || json.Unmarshal(n.GID, &gid) != nil || !digits(gid) {
			continue
		}
		items = append(items, Item{GID: gid, Title: n.Title, Contents: n.Contents, Date: time.Unix(n.Date, 0).UTC(), FeedName: n.FeedName, AppID: n.AppID})
	}
	return items, nil
}

func digits(s string) bool {
	return s != "" && len(s) <= 24 && strings.Trim(s, "0123456789") == ""
}

// retryAfter reads a 429's wait: Retry-After in seconds or as an HTTP date; else a minute. It is
// never more than maxRetry.
func retryAfter(h http.Header, now time.Time) time.Duration {
	d := defaultRetry
	v := strings.TrimSpace(h.Get("Retry-After"))
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	} else if at, err := http.ParseTime(v); err == nil && at.After(now) {
		d = at.Sub(now)
	}
	return min(d, maxRetry)
}
