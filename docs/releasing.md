# Releasing

A release is a signed tag on `main`. The pipeline (`.github/workflows/release.yml`, config in
`.goreleaser.yaml`) does the rest: binaries, checksums and SBOMs on the GitHub release, and the hub
image on ghcr, signed. `v0.1.0` (2026-10-08) was the first run.

## Versions

gravel follows [Semantic Versioning](https://semver.org/). Before 1.0, SemVer itself promises
nothing, so gravel uses the common 0.x rule (#68):

- **0.Y.0** when the release breaks something a host depends on, any of the public surfaces
  below. Each such CHANGELOG entry starts with **Breaking:** and says how to move.
- **0.Y.Z** for everything else: features, fixes, docs, a new metric, a new optional key.

Adding is never breaking: a new package, route, metric, command or optional key with a default.
Removing or renaming is, and so is changing what an existing one means. When unsure, it is
breaking. The public surfaces:

| Surface | Breaking when |
|---|---|
| Go packages a host imports: `discord/…` (the bot runtime, `cli`, `stock`, the modules, `hubclient`, `rolemeta`). `internal/` is not public | an exported name is removed, renamed or changes its signature or behaviour |
| The hub's API: `proto/gravel/hub/v1` (CI's `proto-breaking` guards the wire), the routes in `docs/hub.md` "Endpoints", `/oauth/token` | a procedure, field, route or scope goes or changes meaning |
| Configuration: `hub.yaml` and `bot.yaml` (parsed strictly, so a removed key stops a host's start), the Organization settings document | a key is removed or renamed, a default changes what a host gets, a value is newly refused |
| Metric names and labels (ADR-0009) | a series is renamed, loses a label or changes meaning; the dashboards change with it |
| Deployment: quadlet unit, volume, network and secret names, compose service names, image names | a name a host's units or scripts use changes |
| The database: migrations | a migration a host must run by hand, or one that can't be rolled back from |
| The command lines of `gravel-hub` and `gravel-bot` | a command, flag or exit code a host's scripts rely on changes |

0.1.0 to 0.6.0 were cut as minors under strict SemVer, one per feature; the rule above applies
from the release after 0.6.0. What 1.0 means is a separate decision.

## Cut a release

1. Pick the version by the rule above: 0.Y.0 if any `[Unreleased]` entry is **Breaking:**,
   else 0.Y.Z. On `main`, move the `## [Unreleased]` entries in `CHANGELOG.md` under
   `## [X.Y.Z] - date` and add the comparison links at the end, by PR.
2. Tag that commit: `git tag -s vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. The tag must point at a
   commit that contains the workflow you expect to run: a workflow runs at the tag's commit, not
   at `main`.
3. Watch the `release` run. It needs `contents: write` (release), `packages: write` (ghcr) and
   `id-token: write` (keyless signing), all granted in the workflow.

If the run fails before publishing, fix `main`, delete the tag and its empty release, and tag
again; a tag can't be re-run against a different workflow file.

## What a release contains

| Artifact | Where |
|---|---|
| `gravel-hub_X.Y.Z_linux_{amd64,arm64}.tar.gz` + `.sbom.json` each, `checksums.txt` | the GitHub release |
| `ghcr.io/gravel-project/gravel-hub:X.Y.Z` and `:latest` (multi-arch index; note: no `v` in the tag) | ghcr, public |
| cosign signature (keyless, GitHub OIDC) and an SBOM, attached to the image | ghcr, beside the image |
| `ghcr.io/gravel-project/gravel-postgres:X.Y.Z` and `:latest` (Postgres 17 plus WAL-G, multi-arch; `deploy/postgres/Containerfile`), cosign-signed, **only when `deploy/postgres/` changed since the previous release tag** (below) | ghcr, public |
| `gravel-bot_X.Y.Z_linux_{amd64,arm64}.tar.gz` + `.sbom.json` each, and `ghcr.io/gravel-project/gravel-bot:X.Y.Z` and `:latest` (multi-arch, cosign-signed): the Discord bot with the stock modules (docs/bot.md) | the GitHub release; ghcr, public |

### The Postgres image has its own version line

The Postgres image's base is pinned by digest, so rebuilding an unchanged `deploy/postgres/` makes
the same image under a new hash. The release job therefore builds it only when that directory
changed since the previous `v*` tag, and otherwise skips the build and names the current version in
the run's summary. The current `gravel-postgres` is the gravel version that last changed it, and
`:latest` points at it; there is no `gravel-postgres:X.Y.Z` for a release that didn't.

Postgres patches still arrive: Dependabot watches the `FROM` digest in
`deploy/postgres/Containerfile` and opens a PR that changes the directory, so the next planned
release publishes a new image. It ignores major versions; moving to Postgres 18 is a data upgrade
and a deliberate PR. A change under `deploy/postgres/` (or to `release.yml`) runs the same
multi-arch build in CI's `release-config` job, without pushing.

## Verify an image

```sh
skopeo inspect docker://ghcr.io/gravel-project/gravel-hub:X.Y.Z --format '{{.Digest}}'   # the index digest
cosign verify ghcr.io/gravel-project/gravel-hub@sha256:… \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/gravel-project/gravel/\.github/workflows/release\.yml@refs/tags/vX\.Y\.Z$'
cosign tree ghcr.io/gravel-project/gravel-hub:X.Y.Z   # shows the attached SBOM
```

The identity is the release workflow at that tag; anything else means the image did not come
from this repository's pipeline.

Use cosign 3 or later. The release signs with `sigstore/cosign-installer@v4`, which installs cosign 3
and stores the signature in the newer bundle format; cosign 2 does not look there and answers
`no signatures found` for an image that is signed. Without a local install,
`podman run --rm ghcr.io/sigstore/cosign/cosign:v3.1.2 verify …` takes the same arguments.

## Move the digest pin

`deploy/quadlet/gravel-hub.container` pins the hub image by the index digest, and
`deploy/quadlet/backup/` pins the Postgres image the same way, so a deployment never changes
under a floating tag. A cut moves the backup set's pin only when the release published a new
Postgres image. `deploy/quadlet/observability/` and the `observability` profile in
`deploy/compose.yaml` pin Prometheus and Grafana by index digest too; they are upstream images, so
moving them is a deliberate PR after reading their release notes, not part of every release. After verifying a new release, put its digests in `Image=` by PR;
consumers (Hidden Token Gaming's `deploy` repository) pin the same way. The Postgres image's
identity for `cosign verify` is the same workflow at the tag that published it.

## One-time setup, done for v0.1.0

- The ghcr package was private after the first push; it was made public and linked to the
  repository in the package settings (the API cannot change visibility).
- `sigstore/cosign-installer` publishes no floating `v4` tag; the workflow pins `v4.1.2`. Check
  every `uses:` ref against the repository's tags before bumping one.
