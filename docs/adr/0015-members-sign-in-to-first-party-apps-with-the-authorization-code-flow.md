# ADR-0015: Members sign in to first-party apps with the authorization-code flow, and apps verify tokens by introspection

**Status:** accepted (2026-10-10; John merged it) · **Changes:** how a first-party app's web pages sign a member in, and how a service verifies a gravel token · **Tracked by:** #6; hidden-token-gaming/htg#34 and #35 (Muster, the first consumer, htg ADR-0002)

## Context

ADR-0008 shipped the client-credentials slice of #6: registered apps, `POST /oauth/token`, opaque bearer tokens stored as hashes. What #6 still owes is the part for people:
- an app's web pages sign a member in with their gravel account;
- the app acts as that member;
- a service the app talks to can check a token it is handed.

The first consumer is Muster (hidden-token-gaming/htg ADR-0002). Its pages sign members in through the hub, and every Muster caller presents a gravel token that Muster verifies.

#6 names `zitadel/oidc` "rather than hand-rolling". Taking that seriously:

| Option | What it brings | What it costs here |
|---|---|---|
| `zitadel/oidc` as the provider | A certified OpenID Provider: discovery, JWKS, ID tokens, introspection | **A signing key at rest.** ID tokens are JWTs, so the hub must keep a private key, publish its JWKS and rotate it. Gravel has avoided that twice: sessions have no key (ADR-0004), and Linked Roles refresh tokens were deferred because they need one (ADR-0008). The library's `Storage` interface also wants clients, auth requests, tokens and keys in its shapes, beside the stores gravel already has. |
| `ory/fosite` | A mature OAuth2 framework | The same storage duplication. Ory is already rejected as too heavy (#6). |
| Extend gravel's token endpoint | One token store, the existing `apps` and `app_tokens` rows, opaque tokens checked against their hash | Gravel writes the authorization endpoint itself, so it must follow the specs exactly, and its tests must prove it |

Muster needs no ID token. Its pages learn who signed in from `IdentityService.GetMe` called with the member's token, and every service verifies a token by asking the hub. A JWT would also verify offline, but that saves a call that a cached introspection answer already saves. It costs a key, rotation and revocation lag.

## Decision

1. **OAuth 2.0 authorization code with PKCE, not OpenID Connect.**
   - Gravel issues no ID token and keeps no signing key. "Sign in with gravel" is the code flow plus `GetMe` with the member's token.
   - What it implements, it follows to the letter:
     - RFC 6749 §4.1 (the code grant);
     - RFC 7636 (PKCE, `S256` only, required for every client);
     - RFC 7662 (introspection);
     - the OAuth 2.0 Security BCP (RFC 9700).
   - It leaves out the implicit grant, the password grant, `plain` PKCE and partial redirect-URI matching. Discovery waits until a client needs it.
2. **One token store.**
   - **Redirect URIs:** an app registration gains its redirect URIs (exact strings, https except `http://127.0.0.1`/`localhost` for development). `gravel-hub apps create --redirect-uri …` records them.
   - **Codes:** each authorization code is a row: hashed, single use, 60 seconds, bound to the client, the redirect URI, the PKCE challenge and the member. Replaying a code revokes the tokens issued from it (RFC 6749 §4.1.2).
   - **Member tokens:** they live in `app_tokens` with the member's user id. They are opaque, hashed and hold scopes no wider than the app's. Lifetime: one hour, with no refresh token. The app sends the member through the flow again, which is silent while their gravel session lasts.
3. **The authorization endpoint is a page of the hub** (`GET /oauth/authorize`).
   - It requires a gravel session; a visitor without one goes through the existing login first, then comes back.
   - **Consent:** the first time a member signs in to an app, the hub shows the app's name and the scopes it asks for, with Allow and Cancel. The grant is remembered per member and app, and asked again only when the app asks for more. A member's account page lists the apps they allowed and revokes any of them, along with its tokens.
   - **Errors:** a bad client or redirect URI shows an error page and never redirects (RFC 6749 §4.1.2.1). Any other refusal redirects with `error` and the client's `state`.
4. **A member's token acts as that member, within its scopes.**
   - The API's checks are the member's own: an owner's token can do what the owner can, inside the token's scopes.
   - **Signing in needs no scope:** a member's token may always call `GetMe` for its own member. Every other procedure needs its usual scope (`stats:read`, `servers:moderate`, …) as well as the member's own right to it.
   - **Logging:** the audit and identity logs name the member and the app.
5. **`POST /oauth/introspect` (RFC 7662), for apps only.**
   - It is authenticated with the calling app's client credentials and answers `{active, sub, client_id, scope, exp, token_type}`.
   - `sub` is the member's user id for a member token, absent for an app token.
   - It answers `active: false` for an expired, revoked or unknown token, and for a token that belongs to another organization.
   - An app may cache an answer until `exp` or for at most 60 seconds, whichever is sooner. Revoking an app, a grant or a member's sessions takes effect within that window.

## Consequences

- **No key to keep, rotate or leak.** The cost is a call to the hub per token per minute, per service that checks tokens.
- **Muster** signs members in with `golang.org/x/oauth2` (PKCE built in) and verifies every caller by introspection (htg ADR-0002 §4).
- **Tests are the specification.** Each rule above is a test:
  - codes: an expired code, a replayed code (with its tokens revoked), the wrong PKCE verifier, a redirect URI that is close but not exact;
  - consent: remembered, then asked again for a wider scope;
  - access: a revoked grant, and an owner's token without the scope for an owner action;
  - introspection: another organization's token, and an app token.

  A conformance run with an independent client (`golang.org/x/oauth2`) is part of CI.
- **Going to full OIDC later is additive:** discovery, an ID token and a JWKS, when a client that can't call `GetMe` needs one. That is the point at which gravel takes on a key.
- **Privacy:** a remembered grant (member, app, scopes, when) is new data about a member. The account page shows it, and deleting the account deletes it.

## As built (#6)

The five decisions stand. Where the ADR left a choice open, or reality added one:

- **Public procedures need no scope.** "Every other procedure needs its usual scope": the procedures anyone may call anonymously and that do not look at the caller (`GetOrganization`, `GetOrganizationSettings`, `ListGames`, `ListServers`, `GetServerStatus`) have no usual scope, so a member's token calls them with none. The stats reads (`GetBoard`, `ListMatches`, `GetMemberProfile`, `GetParticipationBoard`) need `stats:read` even while stats are public, because they can answer differently for the member. Every procedure is in one table (`internal/api/auth.go`), checked by an interceptor before the procedure runs; one missing from it is refused, and a test fails until each new procedure is classified.
- **What a member does in person stays in person.** No scope admits unlinking, logging out, roles, settings, the owner claim, the leaderboard name, or the apps a member allowed (`AppService`), whatever the member's rights: those are the account's own controls.
- **App-only stays app-only.** A scope only an app holds (`participation:write`, ADR-0014, which landed after this ADR) is never granted by a member: the authorization endpoint answers `invalid_scope`. A member's token is a separate principal in the request context, never an app, so an app-only procedure (`RecordParticipation`) and `on_behalf_of` refuse it even when its row carries the scope.
- **The identity log is untouched.** A member's token cannot call anything that writes the identity log, so only the audit log names the member and the app (an audit row may now carry both; migration 14 relaxes its one-actor constraint).
- **Logging out everywhere** (`RevokeSessions`) also deletes the member's tokens in apps, as §5 implies; the grants stay, so the next sign-in needs a login but no new consent.
- **Small rules the specs leave to the server:** `redirect_uri` is required in every request (OAuth 2.1's rule, so there is never a default); an empty `scope` asks for sign-in only (`GetMe`); a code is spent by its first presentation whatever the outcome, so a refused exchange cannot be retried with another verifier; a used code is kept until no token issued from it can be alive, so a late replay still revokes; requests with a member's token are rate-limited per member; any app of the organization may introspect, since the service checking a token may be registered apart from the app that obtained it.
