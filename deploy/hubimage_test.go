package deploy

import (
	"regexp"
	"testing"
)

// The stock hub unit names its image twice: Image=, which podman runs, and GRAVEL_IMAGE, which
// /healthz reports (gravel#214). A pin moved in one place only would make the hub misreport
// what it runs.
func TestHubImageIsReportedAsPinned(t *testing.T) {
	b, err := Quadlet.ReadFile("quadlet/gravel-hub.container")
	if err != nil {
		t.Fatal(err)
	}
	image := regexp.MustCompile(`(?m)^Image=(\S+)$`).FindSubmatch(b)
	env := regexp.MustCompile(`(?m)^Environment=GRAVEL_IMAGE=(\S+)$`).FindSubmatch(b)
	switch {
	case image == nil:
		t.Fatal("gravel-hub.container has no Image=")
	case env == nil:
		t.Fatal("gravel-hub.container sets no GRAVEL_IMAGE")
	case string(image[1]) != string(env[1]):
		t.Errorf("Image=%s but GRAVEL_IMAGE=%s: move them together", image[1], env[1])
	}
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).Match(image[1]) {
		t.Errorf("the stock hub runs %s: pin it by digest", image[1])
	}
}
