# Changelog

All notable changes to gravel are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions will follow
[Semantic Versioning](https://semver.org/) from the first release.

## [Unreleased]

### Added

- Every newly opened or reopened issue and pull request is put on the HTG Platform board (hidden-token-gaming's org project, where gravel's issues are sequenced with the rest of the platform work) by that org's reusable `add-to-project` workflow. Without the `PROJECT_ADMIN_TOKEN` repository secret the job logs a notice and skips (hidden-token-gaming/.github#41).
- `docs/adr/0001-hub-first-kubernetes-later.md`: the hub is one Go service with Postgres as the system of record, Kubernetes (the operator plus Agones) is the fully managed driver added with the first managed game, podman quadlets are the first deployment target, adapters and drivers run in-process until the open-source cut, stats start on plain Postgres, and backups belong to the hub. Decided in Hidden Token Gaming's 2026-10-08 strategic review (#11).

### Changed

- README and CLAUDE.md follow ADR-0001: status line, principle 2 (the resource lives in the hub's database), the hub and backup paragraphs, the adapter-repo rule, the local-dev default, and a seven-step build order with the issues behind each step. The launch games are the five Hidden Token Gaming plays, in three shapes, with War Dogs first; the open threads link the design-delta issues #2–#9 (#1).
