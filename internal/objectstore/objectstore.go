// Package objectstore is the hub's object storage: large files that do not belong in Postgres,
// first the match demos a Counter-Strike 2 server records (a few hundred MB each). It is written
// in domain terms, keys and objects, so a caller never sees which service holds them.
//
// S3 is the implementation over any S3-compatible service (Cloudflare R2 first); the
// objectstoretest package holds an in-memory Store for tests and the conformance suite every
// implementation passes. Nothing outside this package imports an object-storage SDK (a depguard
// rule enforces it).
//
// Objects are never served through the hub. A member downloads one through a signed URL the
// store hands out for a short time, so the bucket itself stays private and a demo's bytes never
// pass through the hub twice.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Errors a Store returns, wrapping the service's own.
var (
	// ErrNotFound is an object the store does not have (any more).
	ErrNotFound = errors.New("objectstore: not found")
	// ErrInvalid is a request refused before it reached the service: a malformed key, size,
	// content type, filename or lifetime.
	ErrInvalid = errors.New("objectstore: invalid request")
)

// Limits every implementation enforces the same way.
const (
	// MaxKeyLength is the longest key in bytes; S3 allows no more.
	MaxKeyLength = 1024
	// MinSignedURLTTL and MaxSignedURLTTL bound a signed URL's lifetime. A signature version 4
	// presigned URL cannot live longer than seven days.
	MinSignedURLTTL = time.Second
	MaxSignedURLTTL = 7 * 24 * time.Hour
	// DefaultContentType is what an object is stored as when Put is given no content type.
	DefaultContentType = "application/octet-stream"
)

// Store holds objects by key.
type Store interface {
	// Put stores what r yields under key, replacing any object already there, and streams it
	// rather than holding it in memory. size is the number of bytes r yields, or -1 when it is
	// not known in advance; when it is known and r yields fewer or more, Put fails. contentType
	// is a media type, DefaultContentType when empty. A Put that fails leaves whatever was there
	// before.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// SignedURL is a URL anybody holding it can GET the object from until ttl has passed, with
	// no other credentials. The response asks the browser to save it as an attachment called
	// filename, the key's last segment when filename is empty. It does not check that the object
	// exists, so a URL for a missing object answers with the service's own not-found.
	SignedURL(ctx context.Context, key string, ttl time.Duration, filename string) (string, error)
	// Delete removes the object. Deleting a key that holds nothing is not an error.
	Delete(ctx context.Context, key string) error
	// Stat describes the object, ErrNotFound when there is none.
	Stat(ctx context.Context, key string) (Object, error)
}

// Object describes one stored object.
type Object struct {
	Key          string
	Size         int64
	ContentType  string
	LastModified time.Time
}

// ValidateKey checks a key the way every Store does. A key is at most MaxKeyLength bytes of
// printable UTF-8, does not start with a slash, and has no empty, "." or ".." segment, so a key
// means the same thing to every service and to anything that ever copies the objects to a file
// system.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("%w: key is %d bytes, more than %d", ErrInvalid, len(key), MaxKeyLength)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: key %q is not UTF-8", ErrInvalid, key)
	}
	for _, r := range key {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: key %q has an unprintable character", ErrInvalid, key)
		}
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: key %q starts with a slash", ErrInvalid, key)
	}
	for seg := range strings.SplitSeq(key, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: key %q has an empty segment", ErrInvalid, key)
		case ".", "..":
			return fmt.Errorf("%w: key %q has a %q segment", ErrInvalid, key, seg)
		}
	}
	return nil
}

// ValidateSize checks Put's size: a byte count, or -1 for one not known in advance.
func ValidateSize(size int64) error {
	if size < -1 {
		return fmt.Errorf("%w: size %d", ErrInvalid, size)
	}
	return nil
}

// ContentType is the content type Put stores: DefaultContentType for an empty one, and an error
// for one that is not a media type.
func ContentType(contentType string) (string, error) {
	if contentType == "" {
		return DefaultContentType, nil
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return "", fmt.Errorf("%w: content type %q: %w", ErrInvalid, contentType, err)
	}
	return contentType, nil
}

// ValidateTTL checks a signed URL's lifetime against MinSignedURLTTL and MaxSignedURLTTL.
func ValidateTTL(ttl time.Duration) error {
	if ttl < MinSignedURLTTL || ttl > MaxSignedURLTTL {
		return fmt.Errorf("%w: a signed URL lives between %s and %s, not %s", ErrInvalid, MinSignedURLTTL, MaxSignedURLTTL, ttl)
	}
	return nil
}

// ContentDisposition is the Content-Disposition a signed URL's response carries: an attachment
// called filename, or the key's last segment when filename is empty. A filename is a single
// name, so one with a slash, a backslash or a control character is refused rather than left to
// each browser to interpret.
func ContentDisposition(key, filename string) (string, error) {
	if filename == "" {
		filename = path.Base(key)
	}
	if strings.ContainsAny(filename, `/\`) || strings.ContainsFunc(filename, unicode.IsControl) {
		return "", fmt.Errorf("%w: filename %q is not a single name", ErrInvalid, filename)
	}
	// FormatMediaType quotes the name, and writes one that is not ASCII in RFC 2231's encoding,
	// which every current browser reads.
	v := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if v == "" {
		return "", fmt.Errorf("%w: filename %q", ErrInvalid, filename)
	}
	return v, nil
}
