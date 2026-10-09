# Releasing

A release is a signed tag on `main`. The pipeline (`.github/workflows/release.yml`, config in
`.goreleaser.yaml`) does the rest: binaries, checksums and SBOMs on the GitHub release, and the hub
image on ghcr, signed. `v0.1.0` (2026-10-08) was the first run.

## Cut a release

1. On `main`, move the `## [Unreleased]` entries in `CHANGELOG.md` under `## [X.Y.Z] - date` and
   add the comparison links at the end, by PR.
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
| `ghcr.io/gravel-project/gravel-postgres:X.Y.Z` and `:latest` (Postgres 17 plus WAL-G, multi-arch; `deploy/postgres/Containerfile`), cosign-signed | ghcr, public |
| `gravel-bot_X.Y.Z_linux_{amd64,arm64}.tar.gz` + `.sbom.json` each, and `ghcr.io/gravel-project/gravel-bot:X.Y.Z` and `:latest` (multi-arch, cosign-signed): the Discord bot with the stock modules (docs/bot.md) | the GitHub release; ghcr, public |

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

## Move the digest pin

`deploy/quadlet/gravel-hub.container` pins the hub image by the index digest, and
`deploy/quadlet/backup/` pins the Postgres image the same way, so a deployment never changes
under a floating tag. After verifying a new release, put its digests in `Image=` by PR;
consumers (Hidden Token Gaming's `deploy` repository) pin the same way. The Postgres image's
identity for `cosign verify` is the same workflow at the same tag.

## One-time setup, done for v0.1.0

- The ghcr package was private after the first push; it was made public and linked to the
  repository in the package settings (the API cannot change visibility).
- `sigstore/cosign-installer` publishes no floating `v4` tag; the workflow pins `v4.1.2`. Check
  every `uses:` ref against the repository's tags before bumping one.
