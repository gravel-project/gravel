# ADR-0005: The web UI is an API client

**Status:** accepted (2026-10-09) · **Changes:** how the hub's pages get their data, how they are styled and scripted, and how they are checked · **Tracked by:** #14

## Context

The design says the web UI is templ + htmx over the Connect HTTP-JSON API, one deploy artifact, no JS build pipeline, and that every client is API-only: business rules live in the API exactly once, and a later SPA or mobile app is a front-end-only change. The login's first pages (#13) called the domain services directly, which keeps the pages away from the database but lets a page bypass the API's authorization and error mapping. Hosts also need their own look (#7) under a Content-Security-Policy that allows no inline script or style.

## Decision

1. **The pages call the hub's own API, in process.** Every read and write a page needs (`GetOrganization`, `GetMe`, `UnlinkIdentity`, `ClaimOwnership`, `Logout`, `RevokeSessions`) is a Connect HTTP-JSON call through an `http.RoundTripper` that hands the request to the API handler behind the session middleware, without a socket. The page's request lends the call its cookie and request id; cookies the API sets (a logout clearing the session) are relayed to the browser. The API's authorization, error codes and metrics apply to the pages as to any client, and the rate limiter skips the in-process call because the page request was counted. The provider redirect flows (`/auth/{provider}/start`, `/link`, `/callback`) stay outside the API: they are the login mechanism itself, and a session is issued only there. Logging out is a procedure (`Logout`) so the header's form is an API call too.
2. **htmx is served from the binary.** `internal/web/static/vendor/htmx.min.js` is vendored by `make vendor-htmx` with its licence and provenance; no third-party asset is loaded. Forms carry `hx-post` and target the main landmark; an `HX-Request` gets the page's content, a plain request gets a redirect or the full page, so every page works without JavaScript. htmx is configured for this origin only, without eval or script tags from responses, and swaps error fragments so a refusal shows its message.
3. **Templates are templ components, generated and committed.** `make generate` runs `templ generate` beside `buf generate`, and CI fails on drift. The view models live with the templates and know no domain type.
4. **The theme is a stylesheet route.** The CSP forbids inline styles, so a host's tokens (colours for the light and dark schemes, the font), logo, favicon and navigation links (header or footer, optionally owner-only) render as `/theme.css` custom properties and layout attributes, from a `ThemeSource`. gravel#7 stores the theme on the Organization settings and implements the source; until then the defaults apply. Every stored token is validated as a plain CSS colour or replaced by the default, so a theme cannot inject into the stylesheet; logo and favicon URLs must be https or site-relative.
5. **Accessibility is a CI gate.** `make a11y` runs axe (`@axe-core/cli`, pinned) over the login page and fixtures of the account and error pages, served by the real templates and stylesheet; the default tokens meet WCAG AA contrast in both schemes; pages have a skip link, landmarks, labelled forms and distinct button names.

## Consequences

- A page can do only what the API allows; adding a page means adding or using a procedure, never a query.
- Each page costs an extra session lookup per API call it makes (the middleware runs again on the in-process request); negligible at this scale, and the seam to cache it is the transport.
- Hidden Token Gaming's branding is #7's data, not a fork of the templates.
