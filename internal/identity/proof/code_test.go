package proof

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestNewCode(t *testing.T) {
	// Forty bits, five at a time, most significant first.
	got, err := newCode(bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x00, 0x00}))
	if err != nil || got != "0000-0000" {
		t.Errorf("zeros: %q %v", got, err)
	}
	if got, _ := newCode(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0xff})); got != "ZZZZ-ZZZZ" {
		t.Errorf("ones: %q", got)
	}
	if got, _ := newCode(bytes.NewReader([]byte{0x08, 0x86, 0x42, 0x98, 0xe8})); got != "1234-5678" {
		t.Errorf("1234-5678: %q", got)
	}
	if _, err := newCode(bytes.NewReader([]byte{1, 2})); err == nil {
		t.Error("a short read made a code")
	}
	seen := map[string]bool{}
	for range 2000 {
		c, err := newCode(rand.Reader)
		if err != nil || len(c) != 9 || strings.ContainsAny(c, "ILOU") || seen[c] {
			t.Fatalf("code %q %v (seen %v)", c, err, seen[c])
		}
		seen[c] = true
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"7k3m-9qxa":   "7K3M9QXA",
		" iLoO-1100 ": "1100" + "1100",
		"ABCD-EFGH":   "ABCDEFGH",
	} {
		if got := normalize(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if !bytes.Equal(hashCode("7k3m9qxa"), hashCode("7K3M-9QXA")) {
		t.Error("the typed form hashes apart from the shown one")
	}
}
