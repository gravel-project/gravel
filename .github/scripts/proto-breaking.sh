#!/usr/bin/env bash
# The API's wire check (buf breaking against main), with one way through for an intentional break:
# the pull request carries the `breaking` label AND adds a CHANGELOG entry starting **Breaking:**
# (docs/releasing.md "Versions": that entry makes the next release 0.Y.0). Then buf still runs and
# its findings are printed, but they don't fail the job. The label alone fails, so a break is never
# waved through without the entry that tells hosts how to move.
#
# Env: PR (the pull request number; empty on a push to main), REPO (owner/name), GH_TOKEN, and
# BASE (the ref to diff the CHANGELOG against; default origin/main). Labels are read when the job
# runs, so after adding the label, re-run this job.
set -euo pipefail

base="${BASE:-origin/main}"
declared=false
if [[ -n "${PR:-}" ]]; then
	labels="$(gh pr view "$PR" --repo "$REPO" --json labels --jq '.labels[].name')"
	if grep -qx breaking <<<"$labels"; then
		declared=true
	fi
fi

if [[ "$declared" != true ]]; then
	exec make proto-breaking
fi

if ! git diff "$base"...HEAD -- CHANGELOG.md | grep -qE '^\+- \*\*Breaking:\*\*'; then
	echo "::error::the breaking label needs a CHANGELOG entry starting **Breaking:** that says how hosts move (docs/releasing.md, Versions)"
	exit 1
fi
if make proto-breaking; then
	echo "::notice::breaking label and a Breaking CHANGELOG entry, but buf finds no wire break; the label may not be needed"
else
	echo "::warning::intentional wire break accepted (the breaking label and a Breaking CHANGELOG entry); the findings above are what hosts' clients lose"
fi
