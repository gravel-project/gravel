// Package seedhours is what a seeding rule's hours mean (ADR-0016): weekly windows, each a set of
// days and a time of day from and to, in the Organization settings' stats.timezone. The hub
// validates a rule's hours with it and the seeding module (discord/modules/seeding) acts on them,
// so the two cannot disagree. It imports nothing from a Discord library, because the hub uses it
// too.
//
// A window whose to is before its from runs overnight: {days: [fri], from: "22:00", to: "02:00"}
// starts on Friday at 22:00 and ends on Saturday at 02:00. Times are wall-clock times in the zone,
// so a window keeps its local hours across a daylight-saving change. A time the change skips (the
// hour that does not exist in spring) moves forward by the change, as Go's time.Date does.
package seedhours

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Window is one weekly window as the settings write it.
type Window struct {
	// Days are the days it starts on: mon, tue, wed, thu, fri, sat, sun.
	Days []string
	// From and To are times of day, "18:00" and "23:00" (24-hour, HH:MM). To before From is a
	// window that ends the next day; To equal to From is refused.
	From, To string
}

// Days are the day names a window may use, Monday first.
var Days = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// MaxWindows bounds one rule's windows.
const MaxWindows = 14

const (
	minutesPerDay  = 24 * 60
	minutesPerWeek = 7 * minutesPerDay
)

// Hours are a rule's windows, parsed.
type Hours struct {
	spans []span
}

// span is one window on one day: the weekday it starts on and its minutes from that day's
// midnight (to may pass 1440 for an overnight window).
type span struct {
	day      time.Weekday
	from, to int
}

// Problem is one thing wrong with a rule's hours: the field under hours ("[1].from") and why.
type Problem struct {
	Field, Reason string
}

// Parse reads windows. Every problem is returned, each naming its field; the Hours are usable only
// when there is none. Windows of one rule must not overlap, overnight ones included.
func Parse(ws []Window) (Hours, []Problem) {
	var probs []Problem
	bad := func(field, format string, a ...any) {
		probs = append(probs, Problem{Field: field, Reason: fmt.Sprintf(format, a...)})
	}
	if len(ws) > MaxWindows {
		bad("", "%d windows, at most %d", len(ws), MaxWindows)
	}
	var h Hours
	owner := []int{} // the window each span came from, for naming an overlap
	for i, w := range ws {
		field := fmt.Sprintf("[%d]", i)
		from, okFrom := clock(w.From)
		if !okFrom {
			bad(field+".from", "%q is not a time of day (HH:MM, 24-hour)", w.From)
		}
		to, okTo := clock(w.To)
		if !okTo {
			bad(field+".to", "%q is not a time of day (HH:MM, 24-hour)", w.To)
		}
		if okFrom && okTo && from == to {
			bad(field, "from and to are both %s: a window needs a length", w.From)
			okTo = false
		}
		if len(w.Days) == 0 {
			bad(field+".days", "required: one or more of %v", Days)
		}
		seen := map[string]bool{}
		for _, d := range w.Days {
			idx := slices.Index(Days, d)
			switch {
			case idx < 0:
				bad(field+".days", "%q is not a day (%v)", d, Days)
			case seen[d]:
				bad(field+".days", "%q is listed twice", d)
			default:
				seen[d] = true
				if okFrom && okTo {
					end := to
					if to < from {
						end += minutesPerDay
					}
					h.spans = append(h.spans, span{day: time.Weekday((idx + 1) % 7), from: from, to: end})
					owner = append(owner, i)
				}
			}
		}
	}
	for a := range h.spans {
		for b := a + 1; b < len(h.spans); b++ {
			if overlap(h.spans[a], h.spans[b]) {
				bad(fmt.Sprintf("[%d]", owner[b]), "overlaps hours[%d] on %s", owner[a], Days[(int(h.spans[b].day)+6)%7])
			}
		}
	}
	if len(probs) > 0 {
		return Hours{}, probs
	}
	return h, nil
}

// clock reads "HH:MM" as minutes after midnight.
func clock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' || strings.Trim(s[:2]+s[3:], "0123456789") != "" {
		return 0, false
	}
	hh, _ := strconv.Atoi(s[:2])
	mm, _ := strconv.Atoi(s[3:])
	if hh > 23 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// overlap compares two spans on the week's minutes, wrapping Sunday night into Monday.
func overlap(a, b span) bool {
	as := int(a.day)*minutesPerDay + a.from
	bs := int(b.day)*minutesPerDay + b.from
	ae, be := as+(a.to-a.from), bs+(b.to-b.from)
	for _, shift := range []int{-minutesPerWeek, 0, minutesPerWeek} {
		if as < be+shift && bs+shift < ae {
			return true
		}
	}
	return false
}

// Empty reports whether there is no window: a rule that never calls.
func (h Hours) Empty() bool { return len(h.spans) == 0 }

// At is the window open at now in loc: its start (included) and end (excluded) as instants, and
// false when none is open. Windows do not overlap (Parse refuses it), so at most one is.
func (h Hours) At(now time.Time, loc *time.Location) (start, end time.Time, ok bool) {
	local := now.In(loc)
	for _, back := range []int{0, 1} { // a window opened today, or an overnight one from yesterday
		y, m, d := local.Date()
		day := time.Date(y, m, d-back, 12, 0, 0, 0, loc)
		dy, dm, dd := day.Date()
		for _, s := range h.spans {
			if s.day != day.Weekday() {
				continue
			}
			from := time.Date(dy, dm, dd, 0, s.from, 0, 0, loc)
			to := time.Date(dy, dm, dd, 0, s.to, 0, 0, loc)
			if !now.Before(from) && now.Before(to) {
				return from, to, true
			}
		}
	}
	return time.Time{}, time.Time{}, false
}
