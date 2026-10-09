// Package record writes a War Dogs server's read-only answers as per-build fixtures
// (ADR-0010): every GET route the server advertises, with the configuration document redacted
// and every person and address replaced with a stand-in, under <dir>/<build>/ as files that
// mirror the route paths (GET /v1/catalog/maps/{map}/experiences for the map Kavkazi lands in
// v1/catalog/maps/Kavkazi/experiences.json). A human commits them in a pull request; the client's
// contract tests run against every recorded build.
//
// It sends one token and never retries a 401 (three bad tokens lock the server's protected
// routes), waits out a 429 once, and writes nothing unless every file passed a final check that
// no secret, SteamID, name or address it replaced is left in it.
package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gravel-project/gravel/games/wardogs"
)

// Options configure a recording.
type Options struct {
	// Dir is the testdata directory; the recording goes in Dir/<build>/, replacing one there.
	Dir string
	// Token is the client's token, so the recording can prove it is in no file. Empty records
	// the public routes only.
	Token string
	// AuditLimit is the number of admin-log entries recorded (the route's ?limit=); 0 means 20.
	AuditLimit int
	// Recorder names the tool in recording.json ("gravel-hub wardogs record 0.6.1").
	Recorder string
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// Sleep waits out a 429 (tests); nil means a context-aware timer.
	Sleep func(context.Context, time.Duration) error
}

// File is one recorded answer.
type File struct {
	Route string `json:"route"` // the request, parameters filled: "GET /v1/catalog/maps/Kavkazi/experiences"
	Path  string `json:"file"`  // relative to the build's directory
}

// Skip is an advertised route the recording did not fetch, and why.
type Skip struct {
	Route  string `json:"route"`
	Reason string `json:"reason"`
}

// Manifest is recording.json: what was recorded, from which build, by what.
type Manifest struct {
	Build      string `json:"build"`
	APIVersion string `json:"apiVersion"`
	RecordedAt string `json:"recordedAt"`
	Recorder   string `json:"recorder,omitempty"`
	Files      []File `json:"files"`
	Skipped    []Skip `json:"skipped,omitempty"`
	Scrubbed   Counts `json:"scrubbed"`
}

// ManifestName is the recording's own file in a build's directory.
const ManifestName = "recording.json"

// Report is what a recording did.
type Report struct {
	Manifest
	// Dir is the build's directory.
	Dir string
	// Previous is the newest other recorded build in Options.Dir ("" when there is none), and
	// Diff its routes against this one's.
	Previous string
	Diff     wardogs.RouteDiff
}

// buildDirPattern pulls the changelist out of a build string ("++Wardogs+Live-CL-509546").
var buildDirPattern = regexp.MustCompile(`CL-\d+$`)

// BuildDir is the directory name for a build: its changelist, or the build string made safe.
func BuildDir(build string) string {
	if m := buildDirPattern.FindString(build); m != "" {
		return m
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '_'
	}, strings.Trim(build, "+"))
	if safe == "" {
		return "unknown-build"
	}
	return safe
}

// Record fetches and writes one recording.
func Record(ctx context.Context, c *wardogs.Client, opt Options) (Report, error) {
	if opt.Dir == "" {
		return Report{}, errors.New("record: no testdata directory")
	}
	if opt.AuditLimit <= 0 {
		opt.AuditLimit = 20
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Sleep == nil {
		opt.Sleep = sleep
	}

	capsRoute, _ := wardogs.ParseRoute("GET /v1/capabilities")
	capsBody, err := fetch(ctx, c, opt, capsRoute, nil)
	if err != nil {
		return Report{}, fmt.Errorf("record: capabilities: %w", err)
	}
	var caps wardogs.Capabilities
	if err := json.Unmarshal(capsBody, &caps); err != nil {
		return Report{}, fmt.Errorf("record: capabilities: %w", err)
	}
	if caps.Build == "" {
		return Report{}, errors.New("record: the capabilities name no build")
	}
	if _, err := c.Capabilities(ctx); err != nil { // the client checks routes against these
		return Report{}, fmt.Errorf("record: capabilities: %w", err)
	}

	type answer struct {
		file File
		body []byte
	}
	answers := []answer{{File{Route: capsRoute.String(), Path: fileFor(capsRoute.Segments)}, capsBody}}
	var skipped []Skip

	// The map ids fill {map} parameters; they come from the catalog, when the server has one.
	// Its answer is kept, so the route is fetched once.
	cache := map[string][]byte{}
	var mapIDs []string
	if caps.Supports(wardogs.CapCatalogMaps) && c.HasToken() {
		r, _ := caps.RouteOf(wardogs.CapCatalogMaps)
		body, err := fetch(ctx, c, opt, r, nil)
		if err != nil {
			return Report{}, fmt.Errorf("record: %s: %w", r, err)
		}
		var cat wardogs.MapCatalog
		if err := json.Unmarshal(body, &cat); err != nil {
			return Report{}, fmt.Errorf("record: %s: %w", r, err)
		}
		for _, m := range cat.Maps {
			if m.ID != "" {
				mapIDs = append(mapIDs, m.ID)
			}
		}
		slices.Sort(mapIDs)
		cache[filled(r, nil)] = body
	}

	routes := slices.Clone(caps.Routes)
	slices.Sort(routes)
	for _, s := range routes {
		r, err := wardogs.ParseRoute(s)
		if err != nil {
			skipped = append(skipped, Skip{Route: s, Reason: "not a route this recorder can parse"})
			continue
		}
		if r.Method != "GET" || r.Shape() == capsRoute.Shape() {
			continue // only reads are recorded, and the capabilities already are
		}
		if !c.HasToken() && !isPublic(r) {
			skipped = append(skipped, Skip{Route: s, Reason: "needs the token"})
			continue
		}
		fills, reason := fillsFor(r, mapIDs)
		if reason != "" {
			skipped = append(skipped, Skip{Route: s, Reason: reason})
			continue
		}
		var query url.Values
		if r.Shape() == "GET /v1/audit" {
			query = url.Values{"limit": {fmt.Sprint(opt.AuditLimit)}}
		}
		for _, params := range fills {
			body, err := cache[filled(r, params)], error(nil)
			if body == nil {
				body, err = fetch(ctx, c, opt, r, query, params...)
			}
			if err != nil {
				var apiErr *wardogs.APIError
				if errors.As(err, &apiErr) {
					skipped = append(skipped, Skip{Route: filled(r, params), Reason: apiErr.Error()})
					continue
				}
				return Report{}, fmt.Errorf("record: %s: %w", filled(r, params), err)
			}
			segs := strings.Split(strings.Trim(mustPath(r, params), "/"), "/")
			req := filled(r, params)
			if query != nil {
				req += "?" + query.Encode()
			}
			answers = append(answers, answer{File{Route: req, Path: fileFor(segs)}, body})
		}
	}

	// Scrub: learn every player name first, then rewrite every answer.
	sc := newScrubber(opt.Token)
	decoded := make([]any, len(answers))
	for i, a := range answers {
		dec := json.NewDecoder(bytes.NewReader(a.body))
		dec.UseNumber()
		if err := dec.Decode(&decoded[i]); err != nil {
			return Report{}, fmt.Errorf("record: %s: the answer is not JSON: %w", a.file.Route, err)
		}
		sc.collect(decoded[i])
	}
	files := make(map[string][]byte, len(answers)+1)
	man := Manifest{
		Build:      caps.Build,
		APIVersion: caps.APIVersion,
		RecordedAt: opt.Now().UTC().Format(time.RFC3339),
		Recorder:   opt.Recorder,
		Skipped:    skipped,
	}
	for i, a := range answers {
		out, err := encode(sc.scrub(decoded[i]))
		if err != nil {
			return Report{}, fmt.Errorf("record: %s: %w", a.file.Route, err)
		}
		files[a.file.Path] = out
		man.Files = append(man.Files, a.file)
	}
	man.Scrubbed = sc.counts()
	if files[ManifestName], err = encode(man); err != nil {
		return Report{}, fmt.Errorf("record: %w", err)
	}

	build := BuildDir(caps.Build)
	final := filepath.Join(opt.Dir, build)
	if err := writeAll(opt.Dir, final, files, sc.originals()); err != nil {
		return Report{}, err
	}
	rep := Report{Manifest: man, Dir: final}
	if prev, prevCaps, ok := previous(opt.Dir, build); ok {
		rep.Previous, rep.Diff = prev, wardogs.DiffRoutes(prevCaps, caps)
	}
	return rep, nil
}

func isPublic(r wardogs.Route) bool {
	s := r.Shape()
	return s == "GET /v1/health" || s == "GET /v1/capabilities"
}

// fillsFor is every set of parameters to fetch a route with, or why it is skipped. A {map}
// parameter takes every map id; any other parameter has no values to try.
func fillsFor(r wardogs.Route, mapIDs []string) ([][]string, string) {
	params := r.Params()
	if len(params) == 0 {
		return [][]string{nil}, ""
	}
	if len(params) > 1 {
		return nil, "more than one parameter"
	}
	if !strings.Contains(strings.ToLower(params[0]), "map") {
		return nil, "no values for {" + params[0] + "}"
	}
	if len(mapIDs) == 0 {
		return nil, "no map ids (the catalog is missing or empty)"
	}
	out := make([][]string, len(mapIDs))
	for i, id := range mapIDs {
		out[i] = []string{id}
	}
	return out, ""
}

// fetch is one GET that waits out a single 429 and gives up on the second.
func fetch(ctx context.Context, c *wardogs.Client, opt Options, r wardogs.Route, query url.Values, params ...string) ([]byte, error) {
	body, err := c.Get(ctx, r, query, params...)
	var rl *wardogs.RateLimitedError
	if errors.As(err, &rl) {
		if err := opt.Sleep(ctx, rl.RetryAfter); err != nil {
			return nil, err
		}
		body, err = c.Get(ctx, r, query, params...)
	}
	return body, err
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func filled(r wardogs.Route, params []string) string {
	return r.Method + " " + mustPath(r, params)
}

func mustPath(r wardogs.Route, params []string) string {
	p, err := r.Path(params...)
	if err != nil {
		return r.String()
	}
	return p
}

// fileFor is the fixture file for a request path: its segments, unescaped, as directories, and
// the last as <segment>.json.
func fileFor(segs []string) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
		parts[i] = strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(s)
	}
	return filepath.ToSlash(filepath.Join(parts...)) + ".json"
}

// encode writes JSON the same way every time: two-space indent, sorted keys, no HTML escaping,
// a final newline.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeAll writes the files into a fresh directory beside final, checks every byte of them for
// the values that must not be there, and only then swaps it in for final.
func writeAll(dir, final string, files map[string][]byte, forbidden []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	tmp, err := os.MkdirTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	for rel, data := range files {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return fmt.Errorf("record: %w", err)
		}
	}
	if err := leakCheck(tmp, forbidden); err != nil {
		return err
	}
	old := ""
	if _, err := os.Stat(final); err == nil {
		old = final + ".old"
		_ = os.RemoveAll(old)
		if err := os.Rename(final, old); err != nil {
			return fmt.Errorf("record: %w", err)
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		if old != "" {
			_ = os.Rename(old, final)
		}
		return fmt.Errorf("record: %w", err)
	}
	ok = true
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// leakCheck reads back every written file and fails, naming the file but never the value, if
// any forbidden value is in it.
func leakCheck(root string, forbidden []string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if f != "" && bytes.Contains(data, []byte(f)) {
				rel, _ := filepath.Rel(root, p)
				return fmt.Errorf("record: %s still holds a value the scrub replaced; nothing was written", rel)
			}
		}
		return nil
	})
}

// previous is the newest other build recorded in dir, by the changelist number when both have
// one and by name otherwise, with its capabilities.
func previous(dir, current string) (string, wardogs.Capabilities, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", wardogs.Capabilities{}, false
	}
	var builds []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != current && !strings.HasPrefix(e.Name(), ".") {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "v1", "capabilities.json")); err == nil {
				builds = append(builds, e.Name())
			}
		}
	}
	if len(builds) == 0 {
		return "", wardogs.Capabilities{}, false
	}
	slices.SortFunc(builds, CompareBuilds)
	prev := builds[len(builds)-1]
	data, err := os.ReadFile(filepath.Join(dir, prev, "v1", "capabilities.json"))
	if err != nil {
		return "", wardogs.Capabilities{}, false
	}
	var caps wardogs.Capabilities
	if json.Unmarshal(data, &caps) != nil {
		return "", wardogs.Capabilities{}, false
	}
	return prev, caps, true
}

// CompareBuilds orders build directories: by changelist number when both are CL-<n>, else by
// name.
func CompareBuilds(a, b string) int {
	var na, nb int
	_, ea := fmt.Sscanf(a, "CL-%d", &na)
	_, eb := fmt.Sscanf(b, "CL-%d", &nb)
	if ea == nil && eb == nil && na != nb {
		if na < nb {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
