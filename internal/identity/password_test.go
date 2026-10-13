package identity_test

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/internal/identity"
)

var cheap = identity.PasswordParams{MemoryKiB: 64, Passes: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	h, err := identity.HashPassword("correct horse battery", cheap, rand.Reader)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") || strings.Count(h, "$") != 5 {
		t.Fatalf("hash = %q %v", h, err)
	}
	h2, _ := identity.HashPassword("correct horse battery", cheap, rand.Reader)
	if h == h2 {
		t.Error("two hashes of one password share a salt")
	}
	if ok, rehash, err := identity.VerifyPassword(h, "correct horse battery", cheap); !ok || rehash || err != nil {
		t.Errorf("right password: %v %v %v", ok, rehash, err)
	}
	if ok, _, err := identity.VerifyPassword(h, "correct horse batterY", cheap); ok || err != nil {
		t.Errorf("wrong password: %v %v", ok, err)
	}
	// NFKC: a fullwidth digit is the digit.
	h3, _ := identity.HashPassword("passphrase 1", cheap, rand.Reader)
	if ok, _, _ := identity.VerifyPassword(h3, "passphrase １", cheap); !ok {
		t.Error("NFKC: fullwidth 1 is not 1")
	}
	// New parameters: the old hash still verifies and asks to be rehashed; a wrong password never does.
	stronger := cheap
	stronger.Passes = 2
	if ok, rehash, _ := identity.VerifyPassword(h, "correct horse battery", stronger); !ok || !rehash {
		t.Errorf("parameters changed: ok=%v rehash=%v", ok, rehash)
	}
	if _, rehash, _ := identity.VerifyPassword(h, "nope", stronger); rehash {
		t.Error("a wrong password asks for a rehash")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=64,t=1,p=1$AAAA$AAAA", "$argon2id$v=16$m=64,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=64,t=1,p=1$!!$AAAA"} {
		if ok, _, err := identity.VerifyPassword(bad, "x", cheap); ok || err == nil {
			t.Errorf("VerifyPassword(%q) = %v %v", bad, ok, err)
		}
	}
}
