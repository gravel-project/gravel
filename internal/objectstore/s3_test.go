package objectstore_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/objectstore"
	"github.com/gravel-project/gravel/internal/objectstore/objectstoretest"
)

// r2 is a configuration shaped like Cloudflare R2's, the first service the store is used with.
func r2() objectstore.S3Config {
	return objectstore.S3Config{
		Endpoint:  "0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com",
		Region:    "auto",
		Bucket:    "htg-demos",
		AccessKey: "AKEXAMPLE",
		SecretKey: "secret-example",
		PathStyle: true,
	}
}

func TestS3ConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*objectstore.S3Config)
		want string // a substring of the error, empty for none
	}{
		{"r2", func(*objectstore.S3Config) {}, ""},
		{"https URL", func(c *objectstore.S3Config) { c.Endpoint = "https://s3.example.com" }, ""},
		{"http URL with a port", func(c *objectstore.S3Config) { c.Endpoint = "http://localhost:9000" }, ""},
		{"trailing slash", func(c *objectstore.S3Config) { c.Endpoint = "https://s3.example.com/" }, ""},
		{"nothing", func(c *objectstore.S3Config) { *c = objectstore.S3Config{} }, "missing endpoint, region, bucket, access key, secret key"},
		{"no region", func(c *objectstore.S3Config) { c.Region = " " }, "missing region"},
		{"no secret", func(c *objectstore.S3Config) { c.SecretKey = "" }, "missing secret key"},
		{"bad bucket", func(c *objectstore.S3Config) { c.Bucket = "HTG_Demos" }, `bucket "HTG_Demos"`},
		{"scheme", func(c *objectstore.S3Config) { c.Endpoint = "ftp://s3.example.com" }, "scheme must be http or https"},
		{"path", func(c *objectstore.S3Config) { c.Endpoint = "https://s3.example.com/bucket" }, "no path"},
		{"user", func(c *objectstore.S3Config) { c.Endpoint = "https://key@s3.example.com" }, "no path, query or user"},
		{"bare host with a path", func(c *objectstore.S3Config) { c.Endpoint = "s3.example.com/bucket" }, "is not a host"},
		{"empty host", func(c *objectstore.S3Config) { c.Endpoint = "https://" }, "is not a host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := r2()
			tc.edit(&cfg)
			err := cfg.Validate()
			_, newErr := objectstore.NewS3(cfg)
			switch {
			case tc.want == "" && (err != nil || newErr != nil):
				t.Errorf("Validate() = %v, NewS3 = %v; want no error", err, newErr)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Validate() = %v; want an error containing %q", err, tc.want)
			case tc.want != "" && newErr == nil:
				t.Errorf("NewS3 accepted what Validate refused")
			}
			if err != nil && strings.Contains(err.Error(), cfg.SecretKey) && cfg.SecretKey != "" {
				t.Errorf("the error %q carries the secret key", err)
			}
		})
	}
}

// TestS3SignedURLOffline presigns for R2 with nothing listening: with the region configured,
// signing is local.
func TestS3SignedURLOffline(t *testing.T) {
	s, err := objectstore.NewS3(r2())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	raw, err := s.SignedURL(ctx, "demos/42/de_mirage.dem", 15*time.Minute, "HTG - de_mirage.dem")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != r2().Endpoint || u.Path != "/htg-demos/demos/42/de_mirage.dem" {
		t.Errorf("signed URL %s: want https://%s/htg-demos/demos/42/de_mirage.dem", raw, r2().Endpoint)
	}
	q := u.Query()
	for param, want := range map[string]string{
		"X-Amz-Algorithm":              "AWS4-HMAC-SHA256",
		"X-Amz-Expires":                "900",
		"X-Amz-SignedHeaders":          "host",
		"response-content-disposition": `attachment; filename="HTG - de_mirage.dem"`,
	} {
		if got := q.Get(param); got != want {
			t.Errorf("%s = %q, want %q", param, got, want)
		}
	}
	// The credential's scope is the key, the signing day and the configured region.
	if cred := strings.Split(q.Get("X-Amz-Credential"), "/"); len(cred) != 5 || cred[0] != "AKEXAMPLE" || cred[2] != "auto" || cred[3] != "s3" {
		t.Errorf("X-Amz-Credential = %q, want AKEXAMPLE/<day>/auto/s3/aws4_request", q.Get("X-Amz-Credential"))
	}
	if sig := q.Get("X-Amz-Signature"); len(sig) != 64 {
		t.Errorf("X-Amz-Signature = %q, want 64 hex characters", sig)
	}
	if strings.Contains(raw, r2().SecretKey) {
		t.Errorf("the signed URL carries the secret key: %s", raw)
	}

	// Without path style the bucket is in the host.
	cfg := r2()
	cfg.PathStyle = false
	vs, err := objectstore.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = vs.SignedURL(ctx, "demos/42/de_mirage.dem", time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := url.Parse(raw); err != nil || u.Host != "htg-demos."+cfg.Endpoint || u.Path != "/demos/42/de_mirage.dem" {
		t.Errorf("virtual-hosted signed URL %s", raw)
	}
}

// emuStore is the S3 store over a fresh emulator.
func emuStore(t *testing.T) (*objectstore.S3, *s3emu, *httptest.Server) {
	t.Helper()
	cfg := r2()
	emu := newS3Emu(cfg.Bucket, cfg.AccessKey)
	srv := httptest.NewServer(emu)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL
	s, err := objectstore.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, emu, srv
}

func TestS3NewAndSignDoNotTouchTheNetwork(t *testing.T) {
	s, emu, _ := emuStore(t)
	if _, err := s.SignedURL(t.Context(), "demos/1.dem", time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	if n := emu.count(); n != 0 {
		t.Errorf("NewS3 and SignedURL made %d requests, want none", n)
	}
}

func TestS3Conformance(t *testing.T) {
	s, emu, srv := emuStore(t)
	t.Cleanup(func() {
		if keys := emu.keys(); len(keys) > 0 {
			t.Errorf("after the suite the bucket holds %q", keys)
		}
		if n := emu.completed(); n < 2 {
			t.Errorf("the suite completed %d multipart uploads, want at least 2 (the large step's)", n)
		}
		if n := emu.pending(); n > 0 {
			t.Errorf("the suite left %d multipart uploads open, want every failed one aborted", n)
		}
	})
	objectstoretest.Run(t, func(*testing.T) objectstore.Store { return s }, objectstoretest.Options{
		Download: objectstoretest.HTTPDownload(srv.Client()),
		// Above the store's 16 MiB part size, so both multipart paths run.
		LargeSize: 17 << 20,
	})
}

func TestS3OtherErrorsAreNotNotFound(t *testing.T) {
	_, emu, srv := emuStore(t)
	cfg := r2()
	cfg.Endpoint = srv.URL
	cfg.AccessKey = "SOMEONE-ELSE"
	s, err := objectstore.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(t.Context(), "demos/1.dem"); err == nil || errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("Stat with a key the service refuses: %v, want an error that is not ErrNotFound", err)
	}
	if n := emu.count(); n == 0 {
		t.Error("the refused Stat never reached the service")
	}
}

// TestS3ConformanceLive runs the suite against a real bucket when GRAVEL_TEST_S3_ENDPOINT is set,
// with GRAVEL_TEST_S3_REGION, _BUCKET, _ACCESS_KEY, _SECRET_KEY and _PATH_STYLE ("true" for R2).
// The keys live under a random prefix, and the suite deletes everything it made.
func TestS3ConformanceLive(t *testing.T) {
	endpoint := os.Getenv("GRAVEL_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("GRAVEL_TEST_S3_ENDPOINT is not set")
	}
	pathStyle, _ := strconv.ParseBool(os.Getenv("GRAVEL_TEST_S3_PATH_STYLE"))
	s, err := objectstore.NewS3(objectstore.S3Config{
		Endpoint:  endpoint,
		Region:    os.Getenv("GRAVEL_TEST_S3_REGION"),
		Bucket:    os.Getenv("GRAVEL_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("GRAVEL_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("GRAVEL_TEST_S3_SECRET_KEY"),
		PathStyle: pathStyle,
	})
	if err != nil {
		t.Fatal(err)
	}
	objectstoretest.Run(t, func(*testing.T) objectstore.Store { return s }, objectstoretest.Options{
		Prefix:    "gravel-objectstoretest-" + rand.Text()[:12],
		Download:  objectstoretest.HTTPDownload(nil),
		LargeSize: 17 << 20,
	})
}
