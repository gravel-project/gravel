package wardogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// When a configuration change takes effect, as the server's schema and apply results say.
const (
	AppliesNow         = "applied"
	AppliesNextMatch   = "next-match"
	AppliesNextRestart = "next-restart"
	AppliesPending     = "pending" // still being checked (the sponsor banner's fetch)
)

// Errors of a configuration write that the server answered with a result.
var (
	// ErrRevisionMismatch is a 412: the document changed since the revision the write was based
	// on. Nothing was applied; read it again and re-plan.
	ErrRevisionMismatch = errors.New("wardogs: the configuration changed since that revision")
	// ErrConfigRejected is a 422: the document has errors (ApplyResult.Errors). Nothing was
	// applied.
	ErrConfigRejected = errors.New("wardogs: the server rejected the configuration")
)

// ConfigDocument is GET /v1/config. Text is the whole document in INI form with CRLF line
// endings and the RCON password in clear: pass it through RedactConfig before it is stored,
// logged or shown.
type ConfigDocument struct {
	Revision string    `json:"revision"`
	Writable bool      `json:"writable"`
	Text     string    `json:"text"`
	Sections []Section `json:"sections"`
	Warnings []string  `json:"warnings"`
}

// Section is the schema of one section: the keys the server honours (every other key is
// stripped) and when a change to them takes effect.
type Section struct {
	Section      string        `json:"section"`
	AppliesWhen  string        `json:"appliesWhen"`
	Description  string        `json:"description"`
	AllowedKeys  []string      `json:"allowedKeys"`
	KeyOverrides []KeyOverride `json:"keyOverrides"`
}

// KeyOverride is a key whose apply time or writability differs from its section's.
type KeyOverride struct {
	Key         string `json:"key"`
	AppliesWhen string `json:"appliesWhen"`
	Description string `json:"description"`
	// Writable is nil on builds before CL-501228, which had no per-key lock.
	Writable *bool `json:"writable"`
	// LockedBy names what pins the key ("RCONPort", a command-line switch).
	LockedBy string `json:"lockedBy"`
}

// Schema finds a section by name; ok is false when the server does not honour it.
func (d ConfigDocument) Schema(section string) (Section, bool) {
	for _, s := range d.Sections {
		if s.Section == section {
			return s, true
		}
	}
	return Section{}, false
}

// Key is what the schema says about one key: whether the server honours it, whether it may be
// written, what locks it, and when a change takes effect.
func (d ConfigDocument) Key(section, key string) (KeySchema, bool) {
	s, ok := d.Schema(section)
	if !ok {
		return KeySchema{}, false
	}
	allowed := false
	for _, k := range s.AllowedKeys {
		if strings.EqualFold(k, key) {
			allowed = true
		}
	}
	if !allowed {
		return KeySchema{}, false
	}
	ks := KeySchema{Writable: true, AppliesWhen: s.AppliesWhen}
	for _, o := range s.KeyOverrides {
		if strings.EqualFold(o.Key, key) {
			if o.AppliesWhen != "" {
				ks.AppliesWhen = o.AppliesWhen
			}
			if o.Writable != nil {
				ks.Writable = *o.Writable
			}
			ks.LockedBy = o.LockedBy
		}
	}
	return ks, true
}

// KeySchema is one honoured key.
type KeySchema struct {
	Writable    bool
	LockedBy    string
	AppliesWhen string
}

// ApplyResult is what a validate or a write answers.
type ApplyResult struct {
	OK       bool   `json:"ok"`
	Revision string `json:"revision"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	// Errors are why a 422 applied nothing.
	Errors []KeyError `json:"errors"`
	// Changed are the sections the write changes.
	Changed []SectionChange `json:"changed"`
	// Outcomes say per section when the change takes effect (Applies* constants).
	Outcomes []Outcome `json:"outcomes"`
	// Shadowed are keys a higher configuration layer outranks: the value sent is not in force.
	Shadowed []Shadowed `json:"shadowed"`
	// Stripped are keys this build does not know, dropped.
	Stripped []Stripped `json:"stripped"`
	// Conflict are the deltas behind a 412; their shape is not documented.
	Conflict []json.RawMessage `json:"conflict"`
	// Warnings were applied anyway (commonly a key a command-line switch pins).
	Warnings []string `json:"warnings"`
}

// KeyError is one problem with the document.
type KeyError struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SectionChange is one section a write changes.
type SectionChange struct {
	Section string   `json:"section"`
	Added   bool     `json:"added"`
	Removed bool     `json:"removed"`
	Keys    []string `json:"keys"`
}

// Outcome is when a section's change takes effect.
type Outcome struct {
	Section string `json:"section"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
}

// Shadowed is a key whose value in force comes from elsewhere.
type Shadowed struct {
	Section   string `json:"section"`
	Key       string `json:"key"`
	Declared  string `json:"declared"`
	Effective string `json:"effective"`
	Branch    string `json:"branch"`
}

// Stripped is a key (or a whole section, with no key) the server dropped.
type Stripped struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Reason  string `json:"reason"`
}

// ApplyOptions are a write's switches.
type ApplyOptions struct {
	// Force applies despite warnings; it does not bypass errors.
	Force bool
	// FullApply runs every applier, not only those for the keys that changed.
	FullApply bool
}

// Config reads the configuration document.
func (c *Client) Config(ctx context.Context) (ConfigDocument, error) {
	var out ConfigDocument
	return out, c.call(ctx, CapConfigRead, nil, &out)
}

// ValidateConfig checks a whole document without writing it. A document with errors comes back
// with its result and ErrConfigRejected.
func (c *Client) ValidateConfig(ctx context.Context, text string) (ApplyResult, error) {
	return c.writeConfig(ctx, CapConfigValidate, text, "", nil)
}

// PutConfig replaces the whole document (a section left out is removed), if it is still at
// revision. On a 412 or a 422 the result comes back with ErrRevisionMismatch or
// ErrConfigRejected, and nothing was applied. The text goes out with CRLF line endings whatever
// it came in with.
func (c *Client) PutConfig(ctx context.Context, text, revision string, opt ApplyOptions) (ApplyResult, error) {
	if revision == "" {
		return ApplyResult{}, errors.New("wardogs: a configuration write needs the revision it is based on")
	}
	q := url.Values{}
	if opt.Force {
		q.Set("force", "true")
	}
	if opt.FullApply {
		q.Set("fullApply", "true")
	}
	return c.writeConfig(ctx, CapConfigWrite, text, revision, q)
}

func (c *Client) writeConfig(ctx context.Context, cap Capability, text, revision string, q url.Values) (ApplyResult, error) {
	r, err := c.route(ctx, cap)
	if err != nil {
		return ApplyResult{}, err
	}
	path, err := r.Path()
	if err != nil {
		return ApplyResult{}, err
	}
	var h http.Header
	if revision != "" {
		h = http.Header{"If-Match": {`"` + strings.Trim(revision, `"`) + `"`}}
	}
	resp, err := c.send(ctx, request{
		route: r, path: path, query: q,
		body: []byte(ToCRLF(text)), ctype: "text/plain",
		header: h,
		pass:   map[int]bool{http.StatusPreconditionFailed: true, http.StatusUnprocessableEntity: true},
	})
	if err != nil {
		return ApplyResult{}, err
	}
	var out ApplyResult
	if err := c.decode(resp.Body, &out, r); err != nil {
		return ApplyResult{}, err
	}
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		return out, fmt.Errorf("%w (%s)", ErrRevisionMismatch, describe(out))
	case http.StatusUnprocessableEntity:
		return out, fmt.Errorf("%w (%s)", ErrConfigRejected, describe(out))
	}
	return out, nil
}

func describe(r ApplyResult) string {
	switch {
	case r.Error != nil && r.Error.Code != "":
		return r.Error.Code + ": " + r.Error.Message
	case len(r.Errors) == 1:
		return fmt.Sprintf("%s %s: %s", r.Errors[0].Section, r.Errors[0].Key, r.Errors[0].Code)
	case len(r.Errors) > 1:
		return fmt.Sprintf("%d errors", len(r.Errors))
	}
	return "no detail"
}
