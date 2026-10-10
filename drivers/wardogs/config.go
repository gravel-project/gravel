package wardogs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/games/wardogs"
)

// What the hub owns in a War Dogs configuration document. The deployment's document leaves these
// out: the driver copies the RCON block from the server's own document (it holds the password the
// hub authenticates with and AllowedHosts, which can lock the hub out), writes the feed section
// from the hub's servers.yaml when the server has a feed (ADR-0011; copied too when it has none),
// and writes the ban list from the hub's (John, 2026-10-10: the hub owns DefaultBannedPlayerIds).
const (
	sectionGameSession = "/Script/WDGame.WDGameSession"
	keyBannedIDs       = "DefaultBannedPlayerIds"
	sectionFeed        = "WDServerFeed"
)

var ownedSections = []string{"/Script/WDRCON.WDRCONSettings", sectionFeed}

func owned(section string) bool {
	for _, s := range ownedSections {
		if strings.EqualFold(s, section) {
			return true
		}
	}
	return false
}

// Config is the server's document, redacted, with its schema and its ban list.
func (d *Driver) Config(ctx context.Context) (drivers.ConfigDocument, error) {
	cur, err := d.c.Config(ctx)
	if err != nil {
		return drivers.ConfigDocument{}, mapConfigError(err)
	}
	doc := wardogs.ParseDoc(cur.Text)
	out := drivers.ConfigDocument{
		Revision: cur.Revision, Text: wardogs.RedactConfig(cur.Text), Writable: cur.Writable,
		Warnings: cur.Warnings, Bans: bansOf(doc),
	}
	for _, s := range cur.Sections {
		cs := drivers.ConfigSection{Name: s.Section, AppliesWhen: s.AppliesWhen, Keys: s.AllowedKeys}
		for _, o := range s.KeyOverrides {
			if o.Writable != nil && !*o.Writable {
				lock := o.Key
				if o.LockedBy != "" {
					lock += ": " + o.LockedBy
				}
				cs.Locked = append(cs.Locked, lock)
			}
		}
		out.Sections = append(out.Sections, cs)
	}
	return out, nil
}

// PlanConfig merges the draft, checks it and has the server validate it.
func (d *Driver) PlanConfig(ctx context.Context, draft drivers.ConfigDraft) (drivers.ConfigPlan, error) {
	cur, err := d.c.Config(ctx)
	if err != nil {
		return drivers.ConfigPlan{}, mapConfigError(err)
	}
	merged, err := merge(draft, cur.Text)
	if err != nil {
		return drivers.ConfigPlan{}, err
	}
	res, err := d.c.ValidateConfig(ctx, merged.String())
	if err != nil && !errors.Is(err, wardogs.ErrConfigRejected) {
		return drivers.ConfigPlan{}, mapConfigError(err)
	}
	return drivers.ConfigPlan{
		Revision: cur.Revision,
		Changes:  changes(cur.Text, merged.String()),
		Result:   result(res),
	}, nil
}

// ApplyConfig writes the merged draft against revision.
func (d *Driver) ApplyConfig(ctx context.Context, draft drivers.ConfigDraft, revision string) (drivers.ConfigResult, error) {
	cur, err := d.c.Config(ctx)
	if err != nil {
		return drivers.ConfigResult{}, mapConfigError(err)
	}
	if strings.Trim(cur.Revision, `"`) != strings.Trim(revision, `"`) {
		return drivers.ConfigResult{}, fmt.Errorf("%w: planned against %s, the server is at %s", drivers.ErrConfigConflict, revision, cur.Revision)
	}
	merged, err := merge(draft, cur.Text)
	if err != nil {
		return drivers.ConfigResult{}, err
	}
	res, err := d.c.PutConfig(ctx, merged.String(), cur.Revision, wardogs.ApplyOptions{})
	return result(res), mapConfigError(err)
}

// SetConfigBans rewrites the ban list alone; a document that moved in between is read again
// once.
func (d *Driver) SetConfigBans(ctx context.Context, bans []drivers.Identity) (drivers.ConfigResult, error) {
	ids, err := steamIDs(bans)
	if err != nil {
		return drivers.ConfigResult{}, err
	}
	var res wardogs.ApplyResult
	for attempt := 0; attempt < 2; attempt++ {
		cur, err := d.c.Config(ctx)
		if err != nil {
			return drivers.ConfigResult{}, mapConfigError(err)
		}
		doc := wardogs.ParseDoc(cur.Text)
		s := doc.Section(sectionGameSession)
		if s == nil {
			doc.Sections = append(doc.Sections, wardogs.DocSection{Name: sectionGameSession})
			s = &doc.Sections[len(doc.Sections)-1]
		}
		s.SetArray(keyBannedIDs, ids)
		res, err = d.c.PutConfig(ctx, doc.String(), cur.Revision, wardogs.ApplyOptions{})
		if !errors.Is(err, wardogs.ErrRevisionMismatch) {
			return result(res), mapConfigError(err)
		}
	}
	return result(res), drivers.ErrConfigConflict
}

// merge builds the document to send: the draft's sections, the owned sections from the server's
// current document, secrets the draft left redacted taken from there too, and the hub's ban list.
// It refuses a draft that sets what the hub owns or leaves a band.
func merge(draft drivers.ConfigDraft, current string) (wardogs.Doc, error) {
	want := wardogs.ParseDoc(draft.Text)
	cur := wardogs.ParseDoc(current)
	var problems []drivers.ConfigProblem
	for _, s := range want.Sections {
		if owned(s.Name) {
			problems = append(problems, drivers.ConfigProblem{Section: s.Name, Code: drivers.ProblemHubOwned,
				Message: "the hub owns this section (the RCON block from the server's own document, the feed from servers.yaml); leave it out"})
		}
		if strings.EqualFold(s.Name, sectionGameSession) && s.HasKey(keyBannedIDs) {
			problems = append(problems, drivers.ConfigProblem{Section: s.Name, Key: keyBannedIDs, Code: drivers.ProblemHubOwned,
				Message: "the hub owns the ban list; ban through the hub and leave the key out"})
		}
	}
	if len(problems) > 0 {
		return wardogs.Doc{}, &drivers.InvalidConfigError{Problems: problems}
	}
	// Secrets the draft redacted keep the server's value.
	for i := range want.Sections {
		s := &want.Sections[i]
		cs := cur.Section(s.Name)
		for j, l := range s.Lines {
			if l.Key == "" || !wardogs.IsSecretKey(s.Name, l.Key) || l.Value != wardogs.Redacted {
				continue
			}
			v := ""
			if cs != nil {
				v, _ = cs.Value(l.Key)
			}
			s.Lines[j] = wardogs.DocLine{Op: l.Op, Key: l.Key, Value: v}
		}
	}
	for _, name := range ownedSections {
		if cs := cur.Section(name); cs != nil {
			want.SetSection(*cs)
		}
	}
	if f := draft.Feed; f != nil {
		// The game appends /api/ingest/events to Url.
		want.SetSection(wardogs.DocSection{Name: sectionFeed, Lines: []wardogs.DocLine{
			{Key: "Url", Value: strings.TrimRight(f.URL, "/")},
			{Key: "Token", Value: f.Token},
		}})
	}
	if draft.Bans != nil {
		ids, err := steamIDs(draft.Bans)
		if err != nil {
			return wardogs.Doc{}, err
		}
		s := want.Section(sectionGameSession)
		if s == nil {
			want.Sections = append(want.Sections, wardogs.DocSection{Name: sectionGameSession})
			s = &want.Sections[len(want.Sections)-1]
		}
		s.SetArray(keyBannedIDs, ids)
	} else if cs := cur.Section(sectionGameSession); cs != nil && cs.HasKey(keyBannedIDs) {
		s := want.Section(sectionGameSession)
		if s == nil {
			want.Sections = append(want.Sections, wardogs.DocSection{Name: sectionGameSession})
			s = &want.Sections[len(want.Sections)-1]
		}
		s.SetArray(keyBannedIDs, cs.Array(keyBannedIDs))
	}
	if problems := checkBands(want, draft.Bands); len(problems) > 0 {
		return wardogs.Doc{}, &drivers.InvalidConfigError{Problems: problems}
	}
	return want, nil
}

func checkBands(doc wardogs.Doc, bands []drivers.Band) []drivers.ConfigProblem {
	var out []drivers.ConfigProblem
	for _, b := range bands {
		s := doc.Section(b.Section)
		if s == nil {
			continue
		}
		raw, ok := s.Value(b.Key)
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(raw), `"`), 10, 64)
		if err != nil {
			out = append(out, drivers.ConfigProblem{Section: b.Section, Key: b.Key, Code: drivers.ProblemNotNumber,
				Message: fmt.Sprintf("%q is not a whole number", raw)})
			continue
		}
		if (b.Min != nil && v < *b.Min) || (b.Max != nil && v > *b.Max) {
			out = append(out, drivers.ConfigProblem{Section: b.Section, Key: b.Key, Code: drivers.ProblemOutOfBand,
				Message: fmt.Sprintf("%d is outside the band %s", v, bandText(b))})
		}
	}
	return out
}

func bandText(b drivers.Band) string {
	lo, hi := "", ""
	if b.Min != nil {
		lo = strconv.FormatInt(*b.Min, 10)
	}
	if b.Max != nil {
		hi = strconv.FormatInt(*b.Max, 10)
	}
	return "[" + lo + ", " + hi + "]"
}

func bansOf(doc wardogs.Doc) []drivers.Identity {
	s := doc.Section(sectionGameSession)
	if s == nil {
		return nil
	}
	var out []drivers.Identity
	for _, id := range s.Array(keyBannedIDs) {
		out = append(out, drivers.Identity{Provider: "steam", Subject: strings.TrimSpace(id)})
	}
	return out
}

func steamIDs(bans []drivers.Identity) ([]string, error) {
	out := make([]string, 0, len(bans))
	for _, b := range bans {
		id, err := steamID(b)
		if err != nil {
			return nil, err
		}
		out = append(out, string(id))
	}
	return out, nil
}

// changes is the key-by-key diff of two documents, both redacted first.
func changes(before, after string) []drivers.ConfigChange {
	diff := wardogs.DiffDocs(wardogs.ParseDoc(wardogs.RedactConfig(before)), wardogs.ParseDoc(wardogs.RedactConfig(after)))
	out := make([]drivers.ConfigChange, 0, len(diff))
	for _, c := range diff {
		out = append(out, drivers.ConfigChange{Section: c.Section, Key: c.Key, Before: c.Before, After: c.After})
	}
	return out
}

func result(r wardogs.ApplyResult) drivers.ConfigResult {
	out := drivers.ConfigResult{OK: r.OK, Revision: r.Revision, Warnings: r.Warnings}
	if r.Error != nil && r.Error.Code != "" {
		out.Problems = append(out.Problems, drivers.ConfigProblem{Code: r.Error.Code, Message: r.Error.Message})
	}
	for _, e := range r.Errors {
		out.Problems = append(out.Problems, drivers.ConfigProblem{Section: e.Section, Key: e.Key, Code: e.Code, Message: e.Message})
	}
	for _, c := range r.Changed {
		out.Changed = append(out.Changed, c.Section)
	}
	for _, o := range r.Outcomes {
		out.Outcomes = append(out.Outcomes, drivers.ConfigOutcome{Section: o.Section, State: o.State, Detail: o.Detail})
	}
	for _, s := range r.Shadowed {
		out.Shadowed = append(out.Shadowed, drivers.ConfigShadowed{Section: s.Section, Key: s.Key, Declared: s.Declared, Effective: s.Effective, Branch: s.Branch})
	}
	for _, s := range r.Stripped {
		out.Stripped = append(out.Stripped, drivers.ConfigStripped{Section: s.Section, Key: s.Key, Reason: s.Reason})
	}
	return out
}

func mapConfigError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, wardogs.ErrRevisionMismatch):
		return fmt.Errorf("%w: %w", drivers.ErrConfigConflict, err)
	case errors.Is(err, wardogs.ErrConfigRejected):
		return fmt.Errorf("%w: %w", drivers.ErrConfigRejected, err)
	}
	return mapError(err)
}
