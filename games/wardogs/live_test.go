//go:build live

package wardogs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/record"
)

// TestLiveContract runs the contract against a real server, reading only:
//
//	WARDOGS_BASE_URL=http://host:port WARDOGS_TOKEN_FILE=rcon.txt go test -tags live -run Live ./games/wardogs/
//
// One token and no retry: a refused token fails the test after one request. It also fails when
// the server runs a build with no recording under testdata/, the cue to run gravel-hub wardogs
// record.
func TestLiveContract(t *testing.T) {
	base := os.Getenv("WARDOGS_BASE_URL")
	if base == "" {
		t.Skip("WARDOGS_BASE_URL is not set")
	}
	token := ""
	if f := os.Getenv("WARDOGS_TOKEN_FILE"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		token = strings.TrimSpace(string(data))
	}
	c, err := wardogs.New(base, wardogs.Options{Token: token, Strict: true, UserAgent: "gravel contract test"})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("testdata", record.BuildDir(caps.Build))); err != nil {
		t.Errorf("the server runs %s, which has no recording under testdata/: run gravel-hub wardogs record", caps.Build)
	}
	if token == "" {
		t.Log("WARDOGS_TOKEN_FILE is not set: the protected reads fail with ErrNoToken")
	}
	contract(t, c)
	if token != "" && !c.HasToken() {
		t.Fatal("the server refused the token; not retried")
	}
}
