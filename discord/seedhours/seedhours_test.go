package seedhours_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/discord/seedhours"
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// htg is HTG's War Dogs hours: Wednesday, Saturday and Sunday evenings.
var htg = []seedhours.Window{{Days: []string{"wed", "sat", "sun"}, From: "18:00", To: "23:00"}}

func mustParse(t *testing.T, ws []seedhours.Window) seedhours.Hours {
	t.Helper()
	h, probs := seedhours.Parse(ws)
	if len(probs) > 0 {
		t.Fatalf("parse: %v", probs)
	}
	return h
}

func TestParseProblems(t *testing.T) {
	cases := []struct {
		name  string
		ws    []seedhours.Window
		field string
		want  string
	}{
		{"no days", []seedhours.Window{{From: "18:00", To: "19:00"}}, "[0].days", "required"},
		{"bad day", []seedhours.Window{{Days: []string{"Wed"}, From: "18:00", To: "19:00"}}, "[0].days", `"Wed" is not a day`},
		{"day twice", []seedhours.Window{{Days: []string{"wed", "wed"}, From: "18:00", To: "19:00"}}, "[0].days", "listed twice"},
		{"from not a time", []seedhours.Window{{Days: []string{"wed"}, From: "6pm", To: "19:00"}}, "[0].from", "not a time of day"},
		{"hour 24", []seedhours.Window{{Days: []string{"wed"}, From: "18:00", To: "24:00"}}, "[0].to", "not a time of day"},
		{"minute 60", []seedhours.Window{{Days: []string{"wed"}, From: "18:60", To: "19:00"}}, "[0].from", "not a time of day"},
		{"signed", []seedhours.Window{{Days: []string{"wed"}, From: "+1:00", To: "19:00"}}, "[0].from", "not a time of day"},
		{"one digit", []seedhours.Window{{Days: []string{"wed"}, From: "8:00", To: "19:00"}}, "[0].from", "not a time of day"},
		{"no length", []seedhours.Window{{Days: []string{"wed"}, From: "18:00", To: "18:00"}}, "[0]", "needs a length"},
		{"overlap same day", []seedhours.Window{{Days: []string{"wed"}, From: "18:00", To: "20:00"}, {Days: []string{"wed"}, From: "19:00", To: "21:00"}}, "[1]", "overlaps hours[0] on wed"},
		{"overnight overlaps next day", []seedhours.Window{{Days: []string{"fri"}, From: "22:00", To: "02:00"}, {Days: []string{"sat"}, From: "01:00", To: "03:00"}}, "[1]", "overlaps hours[0] on sat"},
		{"sunday night into monday", []seedhours.Window{{Days: []string{"sun"}, From: "23:00", To: "01:00"}, {Days: []string{"mon"}, From: "00:30", To: "02:00"}}, "[1]", "overlaps hours[0] on mon"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, probs := seedhours.Parse(c.ws)
			if !h.Empty() {
				t.Error("hours with problems are empty")
			}
			for _, p := range probs {
				if p.Field == c.field && strings.Contains(p.Reason, c.want) {
					return
				}
			}
			t.Errorf("want %s: %s, got %+v", c.field, c.want, probs)
		})
	}
	var many []seedhours.Window
	for i := range seedhours.MaxWindows + 1 {
		many = append(many, seedhours.Window{Days: []string{seedhours.Days[i%7]}, From: "0" + string(rune('0'+i/7)) + ":00", To: "0" + string(rune('0'+i/7)) + ":30"})
	}
	if _, probs := seedhours.Parse(many); len(probs) != 1 || !strings.Contains(probs[0].Reason, "15 windows, at most 14") {
		t.Errorf("too many windows: %+v", probs)
	}
	// Back to back is not an overlap.
	mustParse(t, []seedhours.Window{{Days: []string{"wed"}, From: "18:00", To: "20:00"}, {Days: []string{"wed"}, From: "20:00", To: "23:00"}})
	if !mustParse(t, nil).Empty() {
		t.Error("no windows: empty")
	}
}

func TestAtInAZone(t *testing.T) {
	loc := chicago(t)
	h := mustParse(t, htg)
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		now        string
		open       bool
		start, end string
	}{
		{"2026-10-14 17:59", false, "", ""},                                // Wednesday, a minute early
		{"2026-10-14 18:00", true, "2026-10-14 18:00", "2026-10-14 23:00"}, // the start is in
		{"2026-10-14 22:59", true, "2026-10-14 18:00", "2026-10-14 23:00"},
		{"2026-10-14 23:00", false, "", ""},                                // the end is out
		{"2026-10-15 19:00", false, "", ""},                                // Thursday
		{"2026-10-17 20:00", true, "2026-10-17 18:00", "2026-10-17 23:00"}, // Saturday
		{"2026-10-18 20:00", true, "2026-10-18 18:00", "2026-10-18 23:00"}, // Sunday
		{"2026-10-19 20:00", false, "", ""},                                // Monday
	}
	for _, c := range cases {
		start, end, ok := h.At(at(c.now), loc)
		if ok != c.open {
			t.Errorf("%s: open %t, want %t", c.now, ok, c.open)
			continue
		}
		if ok && (!start.Equal(at(c.start)) || !end.Equal(at(c.end))) {
			t.Errorf("%s: %s–%s, want %s–%s", c.now, start, end, c.start, c.end)
		}
	}
	// The same instant is outside in UTC: 20:00 Chicago on a Saturday is 01:00 UTC on Sunday.
	if _, _, ok := h.At(at("2026-10-17 20:00"), time.UTC); ok {
		t.Error("in UTC, 01:00 Sunday is outside 18:00–23:00")
	}
}

func TestAtAcrossDaylightSaving(t *testing.T) {
	loc := chicago(t)
	h := mustParse(t, htg)
	// 2026-11-01 (a Sunday) ends daylight saving in Chicago at 02:00: Saturday's window is at
	// UTC-5, Sunday's at UTC-6, and both stay 18:00–23:00 on the wall clock.
	sat := time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC) // 18:30 CDT
	if start, end, ok := h.At(sat, loc); !ok || !start.Equal(time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)) || end.Sub(start) != 5*time.Hour {
		t.Errorf("Saturday before the change: %t %s %s", ok, start, end)
	}
	if _, _, ok := h.At(time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC), loc); ok {
		t.Error("Sunday 17:30 CST is before the window")
	}
	sun := time.Date(2026, 11, 2, 0, 30, 0, 0, time.UTC) // 18:30 CST
	if start, end, ok := h.At(sun, loc); !ok || !start.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("Sunday after the change: %t %s %s", ok, start, end)
	}
	// 2027-03-14 (a Sunday) starts it again: the window moves an hour earlier in UTC.
	if start, _, ok := h.At(time.Date(2027, 3, 14, 23, 30, 0, 0, time.UTC), loc); !ok || !start.Equal(time.Date(2027, 3, 14, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("Sunday after spring forward: %t %s", ok, start)
	}
	// A window across the night of the change ends at its local time, so it is an hour longer in
	// real time: 22:00 to 06:00 is nine hours that night.
	night := mustParse(t, []seedhours.Window{{Days: []string{"sat"}, From: "22:00", To: "06:00"}})
	start, end, ok := night.At(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), loc) // 04:00 CST Sunday
	if !ok || end.Sub(start) != 9*time.Hour {
		t.Errorf("overnight across fall back: %t %s", ok, end.Sub(start))
	}
}

func TestOvernight(t *testing.T) {
	h := mustParse(t, []seedhours.Window{{Days: []string{"fri", "sun"}, From: "22:00", To: "02:00"}})
	at := func(d, hh int) time.Time { return time.Date(2026, 10, d, hh, 0, 0, 0, time.UTC) } // 2026-10-16 is a Friday
	for _, c := range []struct {
		t    time.Time
		open bool
	}{
		{at(16, 21), false}, {at(16, 22), true}, {at(17, 1), true}, {at(17, 2), false},
		{at(17, 23), false},                   // Saturday is not a start day
		{at(18, 23), true}, {at(19, 1), true}, // Sunday into Monday
		{at(19, 3), false},
	} {
		if _, _, ok := h.At(c.t, time.UTC); ok != c.open {
			t.Errorf("%s: open %t, want %t", c.t.Format(time.DateTime), ok, c.open)
		}
	}
	if start, _, _ := h.At(at(17, 1), time.UTC); !start.Equal(at(16, 22)) {
		t.Errorf("an overnight window starts the day before: %s", start)
	}
}
