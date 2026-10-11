package publicaddr

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{"fl=1\nip=203.0.113.9\nts=1\n", "203.0.113.9"},
		{"ip=2001:db8::1\n", "2001:db8::1"},
		{"ip=::ffff:203.0.113.9\n", "203.0.113.9"},
		{"fl=1\nts=1\n", ""},
		{"ip=not-an-address\n", ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
		got, err := Detect(t.Context(), srv.Client(), srv.URL)
		srv.Close()
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), "no ip=") {
				t.Errorf("%q: %v %v, want an error", tc.body, got, err)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("%q: %v %v, want %q", tc.body, got, err, tc.want)
		}
	}
}
