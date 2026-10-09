# Security policy

gravel's hub is a network service that holds identities, sessions and service credentials, so
security reports are welcome and are handled privately until a fix ships.

## Supported versions

gravel is before 1.0. Only the latest 0.y release line gets security fixes: a fix lands on `main`
and ships as the next patch release of that line (0.Y.Z). Older lines are not patched; upgrade to
the latest release. Before 1.0 a patch release never breaks a host (`docs/releasing.md`, "Versions").

| Version | Supported |
|---|---|
| The latest 0.y release line (today 0.6.x) | Yes |
| Earlier 0.y lines | No |
| Unreleased `main` | Fixes land here first |

## Reporting a vulnerability

**Do not open a public issue, pull request or discussion for a vulnerability.** Report it through
GitHub's private vulnerability reporting:

<https://github.com/gravel-project/gravel/security/advisories/new>

(or **Security** → **Report a vulnerability** on the repository). Only the maintainers can read
the report. GitHub is the only contact channel; there is no security email address.

## What to include

- The version (`gravel-hub version`, an image digest, or a commit) and how it was deployed
  (compose, quadlets, something else).
- The component: the hub (`cmd/gravel-hub`, its API, pages or `/auth/` flows), the bot
  (`cmd/gravel-bot`, the `discord/` packages), the deployment files under `deploy/`, or the
  release images.
- Steps to reproduce, or a proof of concept, and the configuration it needs.
- The impact as you understand it: what an attacker gains, and from where (anonymous, a logged-in
  member, a registered app, the host).
- Any fix or mitigation you have in mind.

Never include real tokens, passwords, session cookies or a member's personal data; redact them.

## What counts

In scope: the code and deployment files in this repository and the images and binaries its
release workflow publishes, including a default configuration that is unsafe. Out of scope: a
host's own deployment of gravel (report it to that host), and vulnerabilities in a dependency
that gravel does not expose (report those upstream; do tell us if gravel is affected).

## What to expect

gravel has one maintainer, so these are targets, not guarantees:

- An acknowledgement within 7 days.
- An assessment (accepted or not, and how severe) within 14 days, with questions if we need more.
- For an accepted report: a fix on `main`, a patch release, and a published GitHub security
  advisory (with a CVE where it warrants one) that credits you unless you ask not to be named. We
  agree a disclosure date with you; please keep the details private until the advisory is out.
