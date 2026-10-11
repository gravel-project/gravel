package seedhours_test

import (
	"testing"
	"time"

	"github.com/gravel-project/gravel/discord/seedhours"
)

// A window that starts in the hour the spring change skips opens an hour earlier, as the package
// comment says (Go's time.Date). Chicago skips 02:00-02:59 on 2026-03-08.
func TestSpringGap(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	h, probs := seedhours.Parse([]seedhours.Window{{Days: []string{"sun"}, From: "02:30", To: "04:00"}})
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	start, end, ok := h.At(time.Date(2026, 3, 8, 7, 45, 0, 0, time.UTC), chicago)
	want := time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC) // 01:30 CST
	if !ok || !start.Equal(want) || !end.Equal(time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("At = %s - %s %t, want 07:30Z (01:30 CST) - 09:00Z (04:00 CDT)", start.UTC(), end.UTC(), ok)
	}
	if got := start.In(chicago).Format("15:04 MST"); got != "01:30 CST" {
		t.Errorf("the skipped 02:30 reads %s, want 01:30 CST", got)
	}
}
