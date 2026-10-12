package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

// partSize is the multipart part size for an upload of unknown or large size. minio-go buffers
// one part in memory at a time, and its default for an unknown size is a part big enough for a
// 5 TiB object (over 500 MiB), far more than a demo needs. 16 MiB parts allow objects up to
// 160 GB under S3's limit of 10,000 parts, and are above R2's and S3's 5 MiB minimum.
const partSize = 16 << 20

// S3Config says where an S3-compatible bucket is and how to sign for it. For Cloudflare R2 the
// endpoint is "<account id>.r2.cloudflarestorage.com", the region "auto", and PathStyle true.
type S3Config struct {
	// Endpoint is the service's host, optionally with a port, or a URL whose scheme says
	// whether to use TLS ("http://localhost:9000" for a local MinIO). A bare host means HTTPS.
	Endpoint string
	// Region is the bucket's region. It is required so that the store never asks the service
	// for it, which keeps the constructor and SignedURL off the network.
	Region string
	Bucket string
	// AccessKey and SecretKey are an access key pair allowed to read, write and delete objects
	// in Bucket and nothing more.
	AccessKey string
	SecretKey string
	// PathStyle addresses the bucket as https://endpoint/bucket/key rather than
	// https://bucket.endpoint/key.
	PathStyle bool
}

// Validate checks the configuration without touching the network.
func (c S3Config) Validate() error {
	_, _, err := c.endpoint()
	return err
}

// endpoint is the host[:port] minio-go dials and whether to use TLS, after checking every field.
func (c S3Config) endpoint() (host string, secure bool, err error) {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"endpoint", c.Endpoint},
		{"region", c.Region},
		{"bucket", c.Bucket},
		{"access key", c.AccessKey},
		{"secret key", c.SecretKey},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return "", false, fmt.Errorf("objectstore: s3: missing %s", strings.Join(missing, ", "))
	}
	if err := s3utils.CheckValidBucketNameStrict(c.Bucket); err != nil {
		return "", false, fmt.Errorf("objectstore: s3: bucket %q: %w", c.Bucket, err)
	}
	host, secure = c.Endpoint, true
	if strings.Contains(c.Endpoint, "://") {
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return "", false, fmt.Errorf("objectstore: s3: endpoint %q: %w", c.Endpoint, err)
		}
		switch u.Scheme {
		case "https":
		case "http":
			secure = false
		default:
			return "", false, fmt.Errorf("objectstore: s3: endpoint %q: scheme must be http or https", c.Endpoint)
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			return "", false, fmt.Errorf("objectstore: s3: endpoint %q must be a host, with no path, query or user", c.Endpoint)
		}
		host = u.Host
	}
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return "", false, fmt.Errorf("objectstore: s3: endpoint %q is not a host", c.Endpoint)
	}
	return host, secure, nil
}

// S3 is a Store over an S3-compatible service.
type S3 struct {
	client *minio.Client
	bucket string
}

var _ Store = (*S3)(nil)

// NewS3 is a Store over the bucket cfg names. It checks cfg and does not touch the network, so a
// wrong key or a missing bucket shows up on the first call, not here.
func NewS3(cfg S3Config) (*S3, error) {
	host, secure, err := cfg.endpoint()
	if err != nil {
		return nil, err
	}
	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: s3: %w", err)
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

// Put streams r to the bucket: one request when the size is known and at most a part, a
// multipart upload otherwise, which minio-go aborts if a part fails.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ValidateSize(size); err != nil {
		return err
	}
	ct, err := ContentType(contentType)
	if err != nil {
		return err
	}
	if size >= 0 {
		// minio-go reads exactly the parts it planned from size, so a reader that runs long would
		// be cut short without a word on a multipart upload. exactReader makes a size that does not
		// match fail the same way on every path.
		r = &exactReader{r: r, left: size}
	}
	_, err = s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: ct,
		PartSize:    partSize,
	})
	if err != nil {
		return fmt.Errorf("objectstore: put %q: %w", key, mapErr(err))
	}
	return nil
}

// SignedURL presigns a GET with the response's Content-Disposition set as a query parameter,
// which S3 and R2 honor on a signed request. With the region configured, presigning is local.
func (s *S3) SignedURL(ctx context.Context, key string, ttl time.Duration, filename string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if err := ValidateTTL(ttl); err != nil {
		return "", err
	}
	cd, err := ContentDisposition(key, filename)
	if err != nil {
		return "", err
	}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{"response-content-disposition": {cd}})
	if err != nil {
		return "", fmt.Errorf("objectstore: sign %q: %w", key, mapErr(err))
	}
	return u.String(), nil
}

// Delete removes the object. S3 answers a delete of a missing key with success already.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: delete %q: %w", key, mapErr(err))
	}
	return nil
}

// Stat reads the object's metadata with a HEAD.
func (s *S3) Stat(ctx context.Context, key string) (Object, error) {
	if err := ValidateKey(key); err != nil {
		return Object{}, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return Object{}, fmt.Errorf("objectstore: stat %q: %w", key, mapErr(err))
	}
	return Object{
		Key:          key,
		Size:         info.Size,
		ContentType:  info.ContentType,
		LastModified: info.LastModified.UTC(),
	}, nil
}

// mapErr turns the service's answer for a missing object into ErrNotFound, keeping its message.
// A HEAD has no body, so minio-go names a 404 on an object NoSuchKey itself.
func mapErr(err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || (resp.StatusCode == http.StatusNotFound && resp.Code != "NoSuchBucket") {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// errSizeMismatch is a reader that yielded a different number of bytes than Put was told.
var errSizeMismatch = errors.New("the reader's length does not match the size")

// exactReader yields r's bytes and fails when r ends before left bytes or has more after them.
// It checks for more as soon as the last expected byte is read, since the reader's consumer
// may never ask again.
type exactReader struct {
	r    io.Reader
	left int64
	done bool
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	var n int
	var err error
	if len(p) > 0 {
		n, err = e.r.Read(p)
		e.left -= int64(n)
	}
	if e.left > 0 {
		if errors.Is(err, io.EOF) {
			return n, fmt.Errorf("%w: %d bytes short", errSizeMismatch, e.left)
		}
		return n, err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	// Every expected byte is read, and anything more is an error. It is returned with no bytes,
	// since io.ReadFull and its kind drop an error that comes with a full buffer.
	if !errors.Is(err, io.EOF) {
		if perr := e.probe(); perr != nil {
			return 0, perr
		}
	}
	e.done = true
	return n, nil
}

// probe reads past the expected end: nil when r is at EOF, an error when it has more. Like
// io.ReadFull, it gives up on a reader that keeps returning nothing without an error.
func (e *exactReader) probe() error {
	var b [1]byte
	for range 100 {
		m, err := e.r.Read(b[:])
		switch {
		case m > 0:
			return fmt.Errorf("%w: the reader has more", errSizeMismatch)
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
	return io.ErrNoProgress
}
