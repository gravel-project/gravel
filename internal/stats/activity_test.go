package stats

import (
	"testing"
	"time"
)

// An hour of the week is averaged over how often it starts in the window, in the organization's
// zone: a week holds each hour once, and the spring change skips an hour that then isn't counted.
func TestHourCounts(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, chicago) // a Monday
	week := HourCounts(from, from.AddDate(0, 0, 7), chicago)
	for d := range week {
		for h, n := range week[d] {
			if n != 1 {
				t.Fatalf("a week: day %d hour %d counted %d times", d, h, n)
			}
		}
	}
	// A partial first hour counts: 20:30 Wednesday to 22:00 is 20:00 and 21:00.
	part := HourCounts(time.Date(2026, 10, 7, 20, 30, 0, 0, chicago), time.Date(2026, 10, 7, 22, 0, 0, 0, chicago), chicago)
	if part[2][20] != 1 || part[2][21] != 1 || part[2][22] != 0 {
		t.Errorf("Wednesday evening = %v", part[2])
	}
	// Sunday 2026-03-08: 02:00 doesn't happen in Chicago.
	spring := HourCounts(time.Date(2026, 3, 8, 0, 0, 0, 0, chicago), time.Date(2026, 3, 9, 0, 0, 0, 0, chicago), chicago)
	if spring[6][2] != 0 || spring[6][1] != 1 || spring[6][3] != 1 {
		t.Errorf("the spring change: %v", spring[6])
	}
	// Sunday 2026-11-01: 01:00 happens twice and counts once.
	fall := HourCounts(time.Date(2026, 11, 1, 0, 0, 0, 0, chicago), time.Date(2026, 11, 2, 0, 0, 0, 0, chicago), chicago)
	if fall[6][1] != 1 {
		t.Errorf("the fall change: %v", fall[6])
	}
}
