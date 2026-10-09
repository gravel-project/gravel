# Contributing to gravel

Thanks for helping. This page takes you from a clone to a pull request that CI accepts. Everyone
who takes part agrees to the [code of conduct](CODE_OF_CONDUCT.md). To report a vulnerability, do
not open an issue: follow [SECURITY.md](SECURITY.md).

## Before you start

- Find or open an issue for the change and say you are taking it. One issue, one branch, one pull
  request; a small fix can skip the issue.
- Read [README.md](README.md). It is the distilled design and decides every architectural
  question; `docs/adr/` records the decisions that changed it.

## Build and test

You need Git, GNU Make, Go (the version in `go.mod`) and podman. `make check` dry-runs the quadlet
units with podman's quadlet generator, and `make up` runs the stack with `podman compose`. Every
other tool is pinned at the top of the `Makefile` and installed into `.bin/` by `make tools`.

Fork the repository on GitHub, then:

```sh
git clone https://github.com/<you>/gravel.git
cd gravel
git switch -c fix/short-name   # a branch per pull request
make tools        # the pinned CLIs into .bin/ (buf, golangci-lint, govulncheck, ko, templ, ...)
make check        # gofmt, vet, buf lint, golangci-lint, govulncheck, generate drift, unit tests, quadlet dry-run
```

`make check` is what CI runs, except the jobs below; run it before every push. `make help` lists
every target.

**Integration tests** run every package against a real Postgres. Packages that need one skip in
`make test`; `make test-integration` insists on `GRAVEL_TEST_DATABASE_URL`, a server where the user
may create databases (each package makes its own). If port 55432 is taken, pick another in both
places:

```sh
podman run -d --name gravel-test-pg -p 127.0.0.1:55432:5432 -e POSTGRES_USER=gravel -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gravel public.ecr.aws/docker/library/postgres:17
until podman exec gravel-test-pg pg_isready -q -h 127.0.0.1 -U gravel; do sleep 1; done
GRAVEL_TEST_DATABASE_URL='postgres://gravel:test@127.0.0.1:55432/gravel?sslmode=disable' make test-integration
podman rm -f gravel-test-pg
```

Also, when they apply:

- `make generate` after editing a `.proto` file, a `.templ` page or the dashboard definitions
  (`internal/tools/dashboards/defs.go`). The generated code is committed and CI fails on drift.
- `make a11y` when a page changes (axe over the pages; needs Node.js and Chrome).
- `gravel-hub wardogs record` when War Dogs ships a new server build: the fixtures under
  `games/wardogs/testdata/<build>/` are recorded, reviewed and committed by a person
  ([games/wardogs/README.md](games/wardogs/README.md)).
- `make up` to run the hub with Postgres, then `make down`. [docs/hub.md](docs/hub.md) is the
  operator reference.

CI also builds the images, runs the backup restore drill and the dashboards against a live hub,
checks the release configuration and runs `buf breaking` against `main`. Those need more than a
laptop usually has; CI tells you if one fails.

## Design rules

CLAUDE.md holds the full list; these are the ones a change most often meets:

- **Don't reinvent the wheel.** Build on the projects README names (Goth, Connect/buf, WAL-G,
  Agones, SPIFFE/SPIRE and the rest) instead of hand-rolling what they do.
- **Every seam is a provider behind a Go interface**, written in domain terms, with a boring
  default implementation. No backend's features leak into an interface: identity providers sit
  behind `identity.Provider`, a cloud's SDK is imported only under `cloud/`, SQL lives only in
  `internal/store`.
- **API-only clients.** The web UI, the bot and the CLI go through the hub's Connect API, never
  to Postgres or Kubernetes, so a business rule lives in the API exactly once. A page gets its
  data from a procedure called in process; a new page uses or adds a procedure.
- **The `games/` boundary.** A game's client library under `games/<game>/` imports nothing from
  gravel (depguard in `make lint` enforces it), because each becomes a standalone Go module.
  `drivers/<game>/` and `adapters/<game>/` adapt it to gravel's interfaces.
- **Host preferences are data.** A host-configurable knob goes in the Organization settings
  document (ADR-0007), not in a new configuration key or table.
- **Metric names are a public interface.** A new or renamed metric goes on a dashboard in the same
  pull request (ADR-0009); a test fails otherwise.
- **Secrets** reach containers as podman secrets, never in plaintext config, the database or a
  bind mount.

## Pull requests

- **Title:** a [Conventional Commit](https://www.conventionalcommits.org/en/v1.0.0/) with a
  lower-case subject, such as `fix(hub): refuse an expired claim token`. The types are `feat`,
  `fix`, `docs`, `refactor`, `perf`, `chore`, `ci`, `build`, `test` and `style`; a scope is
  optional. CI checks it.
- **Changelog:** add a line to [CHANGELOG.md](CHANGELOG.md) under `## [Unreleased]`, in the right
  section (Added, Changed, Fixed, ...). A change nobody using gravel would notice (CI, tests, a
  typo) carries the `no-changelog` label instead; ask a maintainer to add it. CI checks it.
- **Sign-off:** every commit carries a `Signed-off-by:` line with your name and email, matching the
  commit's author, under the [Developer Certificate of Origin](https://developercertificate.org)
  ([DCO](DCO)). By signing off you certify that you wrote the change or otherwise have the right to
  submit it under the project's license. `git commit -s` adds the line. To fix commits that lack
  it, `git rebase --signoff HEAD~N` for the last N commits (or `git commit --amend -s` for one), then
  `git push --force-with-lease`. CI checks every commit in the pull request.
- **Description:** what changes and why, and how you verified it (the template has both sections),
  ending with `Closes #N` for the issue.
- **Merging:** pull requests are squash-merged once every check is green. The title becomes the
  commit's subject and your commit messages, sign-offs included, its body.

## Versions

Before 1.0, a change that breaks a public surface (the `discord/` Go packages, the API, the
`hub.yaml` and `bot.yaml` keys, metric names, unit and secret names, migrations, the command
lines) gets a CHANGELOG entry starting **Breaking:** that says how to move, and makes the next
release a minor one (0.Y.0); everything else ships in a patch release. `docs/releasing.md`
("Versions") lists what breaks each surface. When unsure, treat it as breaking.

## License

gravel is licensed under the [Apache License 2.0](LICENSE). Your contributions are licensed under
the same terms (section 5 of the license).
