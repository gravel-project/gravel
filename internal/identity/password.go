package identity

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

// PasswordParams are argon2id's costs (RFC 9106). A hash records the ones it was made with, so a
// change rehashes each account at its next good login, and old hashes keep verifying.
type PasswordParams struct {
	MemoryKiB uint32
	Passes    uint32
	Lanes     uint8
	SaltLen   uint32
	KeyLen    uint32
}

// DefaultPasswordParams are OWASP's argon2id minimum (19 MiB, 2 passes, 1 lane): about 25 ms on
// the hub's two vCPUs, and 19 MiB for each login in flight, which the login rate limit bounds.
var DefaultPasswordParams = PasswordParams{MemoryKiB: 19456, Passes: 2, Lanes: 1, SaltLen: 16, KeyLen: 32}

// MaxPasswordBytes bounds what is hashed, so a request can't make the hub hash a megabyte.
const MaxPasswordBytes = 1024

var errBadHash = errors.New("identity: not an argon2id PHC string")

// normalizePassword is the password as hashed: NFKC, so the same characters typed on two
// keyboards (a composed é and e + accent) are the same password (NIST SP 800-63B §5.1.1.2).
func normalizePassword(pw string) string { return norm.NFKC.String(pw) }

// HashPassword returns a PHC string: $argon2id$v=19$m=<KiB>,t=<passes>,p=<lanes>$<salt>$<hash>,
// salt and hash in unpadded standard base64.
func HashPassword(pw string, p PasswordParams, rand io.Reader) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := io.ReadFull(rand, salt); err != nil {
		return "", fmt.Errorf("identity: salt: %w", err)
	}
	key := argon2.IDKey([]byte(normalizePassword(pw)), salt, p.Passes, p.MemoryKiB, p.Lanes, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Passes, p.Lanes, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches the PHC string, in constant time over the key, and
// whether the hash was made with other parameters than want (rehash it after a good login).
func VerifyPassword(encoded, pw string, want PasswordParams) (ok, rehash bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false, errBadHash
	}
	var p PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Passes, &p.Lanes); err != nil || p.MemoryKiB == 0 || p.Passes == 0 || p.Lanes == 0 {
		return false, false, errBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, false, errBadHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return false, false, errBadHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key)) //nolint:gosec // decoded from a stored hash, bounded by the column
	got := argon2.IDKey([]byte(normalizePassword(pw)), salt, p.Passes, p.MemoryKiB, p.Lanes, p.KeyLen)
	ok = subtle.ConstantTimeCompare(got, key) == 1
	return ok, ok && p != want, nil
}
