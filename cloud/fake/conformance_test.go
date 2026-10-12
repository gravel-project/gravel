package fake_test

import (
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/cloudtest"
	"github.com/gravel-project/gravel/cloud/fake"
)

// options is the suite's run against the fake's default catalog, with the fake's CanResize
// matrix: up a family is fine, down is refused once the disk would shrink, and families never
// mix.
func options() cloudtest.Options {
	return cloudtest.Options{
		Region:   "fake-1",
		Size:     "small",
		Image:    "fake-image",
		ResizeTo: "medium",
		ResizeCases: []cloudtest.ResizeCase{
			{From: "small", To: "medium", OK: true},
			{From: "medium", To: "small", OK: false},            // disk 80 → 25
			{From: "small", To: "dedicated-2", OK: false},       // family
			{From: "dedicated-2", To: "dedicated-4", OK: true},  // up within the family
			{From: "dedicated-4", To: "dedicated-2", OK: false}, // disk 50 → 25
			{From: "small", To: "small", OK: true},              // a no-op
			{From: "large", To: "medium", OK: false},            // disk 160 → 80
			{From: "dedicated-2", To: "small", OK: false},       // family, though the disk fits
		},
		Prefix: "gravel-ct",
		Tag:    "gravel-ct",
	}
}

func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []fake.Option
	}{
		// Operations done by the time anybody looks.
		{"instant", nil},
		// Operations that take a moment, so the suite's waits and polls do real work and every
		// read before a completion sees the cloud mid-change.
		{"slow", []fake.Option{fake.WithOperationDuration(2 * time.Millisecond)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fake.New(tc.opts...)
			// Registered before Run, so it runs after the suite's own cleanup: the suite must
			// leave nothing behind (no volume, snapshot, address or firewall) but its SSH key, which the
			// interface cannot delete.
			t.Cleanup(func() {
				if got, want := p.Counts(), (fake.Counts{SSHKeys: 1}); got != want {
					t.Errorf("after the suite the fake holds %+v, want %+v", got, want)
				}
			})
			opts := options()
			opts.Poll = time.Millisecond
			cloudtest.Run(t, func(*testing.T) cloud.Provider { return p }, opts)
		})
	}
}
