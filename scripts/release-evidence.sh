#!/usr/bin/env bash
# Prove that RELEASE_SHA may be released: it is a full commit SHA, it is the
# head of main, and a push-event run of ci.yml for exactly that commit
# succeeded. Reads GITHUB_REPOSITORY; gh authenticates with GH_TOKEN.
set -euo pipefail

sha=${RELEASE_SHA:?}
repo=${GITHUB_REPOSITORY:?}
fail() {
  echo "::error::$1" >&2
  exit 1
}

[[ $sha =~ ^[0-9a-f]{40}$ ]] || fail "release SHA '$sha' is not a full lower-case commit SHA"
main=$(gh api "repos/$repo/commits/main" --jq .sha)
[ "$main" = "$sha" ] || fail "release SHA $sha is not main's head $main"
runs=$(gh api "repos/$repo/actions/workflows/ci.yml/runs?head_sha=$sha&event=push&branch=main&status=success" \
  --jq "[.workflow_runs[] | select(.head_sha == \"$sha\" and .event == \"push\" and .conclusion == \"success\") | .id] | join(\" \")")
[ -n "$runs" ] || fail "no successful push-event CI run for $sha"
echo "release SHA $sha is main's head; successful push CI runs: $runs"
