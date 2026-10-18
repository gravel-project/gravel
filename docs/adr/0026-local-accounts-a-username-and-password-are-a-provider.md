# ADR-0026: Local accounts: a username and a password are a provider

**Status:** accepted (2026-10-13, with #186 in #232) · **Changes:** who can sign in to a hub with no provider configured; amends ADR-0004 §3 and §6, and supersedes README's "provider-only identity … an accepted, documented v1 tradeoff" and "shared verified email … absorb" · **Decided in:** John's design session *gravel — Design Session: Hub, Spines & Dashboard* (2026-10-10 to 12), handbook D35 · **Built in two steps:** #186 (accounts, login, registration, linking), #187 (set-password links, the host-side bootstrap, mailed resets) · **Part of:** H1 (#182), the unplug test (ADR-0025)

## Context

Every way into a hub was an OAuth or OpenID provider behind Goth (ADR-0004): Discord to log in, Steam to link. A hub with none configured had no members, so nobody could claim it, so it had no owner: the unplug test's first step failed before it started. The old-school floor (ADR-0025, Neckbeard) is a solo operator with no Discord, Steam or Xbox, and GRVL hands a customer a hub before the customer has a Discord application.

The design session: gravel mints its own id and every provider is a credential hanging off it; local accounts are first-class, a username and a password, with email optional and only for recovery; a credential is linked only from inside a session; no automatic email matching and no silent merges; unlinking never loses the account.

## Decision

1. **A local account is an identity like any other:** `("local", <canonical username>)` in `identities`, unique across the hub, with the same events, linking and unlink rules. Its credential (`local_credentials`: the hash, an optional recovery email, the lockout state) sits beside it and goes with it when it is unlinked. There is no redirect, so it is not an `identity.Provider`: the login form posts to the identity service, which owns the rules. Turning it on is `hub.yaml auth.local.enabled` (off by default).
2. **Usernames** are 3 to 32 of `a-z 0-9 . _ -`, starting with a letter or a digit, compared after NFKC and lower case; a few names that read as the hub speaking are reserved. The username is also the member's first display name.
3. **Passwords** follow NIST SP 800-63B: a minimum length the host sets (`local_accounts.min_password_length`, 12 by default, 8 to 128), at most 1024 bytes, never the username, NFKC before hashing, and no composition rules. They are hashed with **argon2id** at OWASP's minimum (19 MiB, 2 passes, 1 lane) as a PHC string, salted, no pepper; a hash with older parameters is replaced at the next good login.
4. **Refusals say nothing.** An unknown username, a wrong password and a locked account get one answer and one hash verification each. Five failures in a row lock an account for 15 minutes, each lockout since the last good login twice the one before, up to a day; a good login clears it. Logins and registrations are limited per client address on top of the pages' limit.
5. **Registration is closed by default.** The host opens it in the Organization settings (`local_accounts.registration: open`); closed, staff make accounts and hand over a one-time set-password link (#187), and the first owner is made from the host with `gravel-hub users create-local` (#187). Hidden Token Gaming turns local accounts on at H1's gate with registration closed (John, 2026-10-12).
6. **One rule for linking:** a member adds a username and password only from inside their session, one per member. A username another member holds is refused. There is no email matching and no merge: an account found twice stays two accounts. Email is optional and only ever for recovery (#187).

## Consequences

- New: migration 00023 (`local_credentials`), `identity.LocalLogin`, `LocalRegister`, `LocalLink`, `CanonicalUsername`, argon2id hashing (`golang.org/x/crypto/argon2`), `/auth/local/login`, `/auth/local/register`, `/account/local`, `hub.yaml auth.local`, `org.Settings.local_accounts`, `gravel_local_logins_total{result}`, and `local` among `gravel_identities`.
- A hub with only local accounts offers a login form, so the owner claim works with nothing plugged in; the unplug test (#198) relies on it.
- gravel now owns hashing, lockout and (with #187) resets: that is the cost the design session accepted. The lockout lets anyone who knows a username lock it for a while; the doubling caps at a day, and staff can reset it (#187).
- README's "provider-only identity … v1 tradeoff" and its "shared verified email; absorb" merge path are withdrawn.
