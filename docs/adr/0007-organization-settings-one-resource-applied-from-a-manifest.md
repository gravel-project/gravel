# ADR-0007: Organization settings, one resource, applied from a manifest

**Status:** accepted (2026-10-09) · **Changes:** where a host's configurable knobs live and how a deployment sets them · **Tracked by:** #7

## Context

The design puts every host-configurable knob (theme, navigation, the Discord role mapping, token lifetimes, layout overrides) in one Organization settings resource managed through the API, "no scattered ConfigMaps". The pages (ADR-0005) read their theme through a `ThemeSource`. Hidden Token Gaming keeps its desired state in a repository and applies it on converge; the `gravel` CLI that the design has apply such a manifest over the API is build-order step 5, not yet built, and the API's only credential is a browser session.

## Decision

1. **One JSON document on the organization row** (`organizations.settings`, with `settings_updated_at`), versioned by its own `version` key. Theme tokens for both schemes, the font, a logo, a favicon and navigation links are in it now; later knobs join the same document. Zero values mean the default, so a document says only what the host changed.
2. **The API reads it publicly and writes it as the owner.** `GetOrganizationSettings` needs no session: the login page renders the theme before anyone is logged in. `UpdateOrganizationSettings` replaces the whole document, owner only, and `invalid_argument` names every invalid field at once. Validation lives in `internal/org` and is the same for every writer: colours are plain CSS colours, the font a font-family list, the logo and favicon https or site-relative, navigation links bounded and `http(s)` or site-relative, placements and roles from the known sets.
3. **The pages read it through the API.** The hub's `ThemeSource` is an in-process `GetOrganizationSettings`; the web layer still normalizes what it renders, so a document from an older build never injects into the stylesheet.
4. **A manifest applies it until the CLI exists.** `gravel-hub settings export` prints the document as YAML; `gravel-hub settings apply <manifest>` validates and stores it, says "unchanged" or "applied", and with `--dry-run` exits 3 when it would change something. The hub binary runs the same domain code the API handler runs, so the rules hold; a deployment commits the manifest and applies it on converge. When the `gravel` CLI arrives, `gravel settings apply` does the same over HTTP-JSON with a credential, and the hub-side command stays for bootstrapping.

## Consequences

- Hidden Token Gaming's theme is a committed manifest (deploy#45) applied by its infra role after each converge; a change is one PR with the manifest's diff.
- A settings edit UI is not part of this; the owner's page can grow one over `UpdateOrganizationSettings`.
- The role mapping (gravel#15, htg#30) and token lifetimes extend the same document and the same two procedures.
