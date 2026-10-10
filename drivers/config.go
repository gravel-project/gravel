package drivers

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Configuration errors.
var (
	// ErrConfigConflict is a server whose configuration changed since the revision a write was
	// planned against. Nothing was applied; plan again.
	ErrConfigConflict = errors.New("drivers: the configuration changed since it was planned")
	// ErrConfigRejected is a document the server refused (ConfigResult.Problems). Nothing was
	// applied.
	ErrConfigRejected = errors.New("drivers: the server rejected the configuration")
	// ErrInvalidConfig is a document the driver refuses before sending it: a value outside a
	// band, a key or section the hub owns. InvalidConfigError carries the problems.
	ErrInvalidConfig = errors.New("drivers: the configuration is not acceptable")
)

// Problem codes a driver reports itself, beside the server's own.
const (
	ProblemOutOfBand = "out_of_band" // a value outside the game's band
	ProblemHubOwned  = "hub_owned"   // a section or key the hub keeps (secrets, the ban list)
	ProblemNotNumber = "not_a_number"
)

// InvalidConfigError is ErrInvalidConfig with the problems, each naming its section and key.
type InvalidConfigError struct {
	Problems []ConfigProblem
}

func (e *InvalidConfigError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, fmt.Sprintf("%s %s: %s", p.Section, p.Key, p.Message))
	}
	return "drivers: the configuration is not acceptable: " + strings.Join(parts, "; ")
}

// Unwrap makes the error ErrInvalidConfig.
func (e *InvalidConfigError) Unwrap() error { return ErrInvalidConfig }

// Band is the range a configuration key may take (a publisher guardrail, the Game spec's); a nil
// bound is open.
type Band struct {
	Section string
	Key     string
	Min     *int64
	Max     *int64
}

// ConfigDraft is what a deployment wants a server's configuration to be.
type ConfigDraft struct {
	// Text is the desired document as the deployment keeps it: secrets redacted, the sections and
	// keys the hub owns left out. The driver merges those in from the server and the hub.
	Text string
	// Bans is the hub's ban list for the server; the driver writes it into the document. Nil
	// keeps the server's list as it is.
	Bans []Identity
	// Bands are checked before anything is sent.
	Bands []Band
	// Feed is where the server should push its events and the token it posts with (ADR-0011);
	// the driver writes it into the game's feed settings. Nil keeps the server's own.
	Feed *Feed
}

// Feed is a server's event feed: the origin it posts to and its token.
type Feed struct {
	URL   string
	Token string
}

// ConfigSection is the server's schema for one section.
type ConfigSection struct {
	Name string
	// AppliesWhen is when a change takes effect: "applied", "next-match", "next-restart".
	AppliesWhen string
	Keys        []string
	// Locked are keys that may not be written, with what locks them ("Port: RCONPort").
	Locked []string
}

// ConfigDocument is a server's configuration as the hub may show it.
type ConfigDocument struct {
	Revision string
	// Text is the document with every secret redacted.
	Text     string
	Writable bool
	Sections []ConfigSection
	Warnings []string
	// Bans are the identities the document bans.
	Bans []Identity
}

// ConfigChange is one key whose lines a plan changes; values are redacted.
type ConfigChange struct {
	Section string
	Key     string
	// Before and After are the key's lines in order (array lines keep their prefix); empty when
	// absent on that side.
	Before, After []string
}

// ConfigProblem is one reason a document is refused.
type ConfigProblem struct {
	Section, Key, Code, Message string
}

// ConfigOutcome is when a section's change takes effect.
type ConfigOutcome struct {
	Section, State, Detail string
}

// ConfigShadowed is a key whose value in force comes from elsewhere.
type ConfigShadowed struct {
	Section, Key, Declared, Effective, Branch string
}

// ConfigStripped is a key (or a section, with no key) the server drops.
type ConfigStripped struct {
	Section, Key, Reason string
}

// ConfigResult is what a server says of a document it validated or applied.
type ConfigResult struct {
	OK       bool
	Revision string
	Problems []ConfigProblem
	// Changed are the sections the document changes.
	Changed  []string
	Outcomes []ConfigOutcome
	Shadowed []ConfigShadowed
	Stripped []ConfigStripped
	Warnings []string
}

// ConfigPlan is a draft merged, checked and validated against the server's current document.
type ConfigPlan struct {
	// Revision is the server's revision the plan was made against; ApplyConfig needs it.
	Revision string
	Changes  []ConfigChange
	Result   ConfigResult
}

// Configurer reads and writes a server's configuration (ADR-0010 §6). Writes are whole
// documents, made only against the revision they were planned on.
type Configurer interface {
	// Config is the current document, redacted.
	Config(ctx context.Context) (ConfigDocument, error)
	// PlanConfig merges the draft with what the hub owns, checks the bands and has the server
	// validate it, sending nothing that would change it. A document the server would refuse
	// comes back as a plan whose result is not OK.
	PlanConfig(ctx context.Context, draft ConfigDraft) (ConfigPlan, error)
	// ApplyConfig writes the merged draft if the server is still at revision (ErrConfigConflict
	// otherwise).
	ApplyConfig(ctx context.Context, draft ConfigDraft, revision string) (ConfigResult, error)
	// SetConfigBans rewrites the document's ban list alone, against the current revision.
	SetConfigBans(ctx context.Context, bans []Identity) (ConfigResult, error)
}
