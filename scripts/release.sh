#!/usr/bin/env bash
# Release Menagerie with Intentional.
#
#   release.sh release    on main with pending intents: apply them, commit
#                         chore(release): <version> and tag it (the first
#                         time, tag its parent as the 0.0.0 baseline)
#   release.sh rehearse   in a scratch clone of HEAD, then discarded: with
#                         pending intents, release and check that CD would
#                         deliver the tag; otherwise check that there is
#                         nothing to release
#
# A release ends with: git push --atomic --follow-tags origin main. The tag
# push starts cd.yml. Intentional reads the version from tags; the release
# commit uses the checkout's Git identity, its tags carry Intentional's
# release record. The plan is written to RELEASE_PLAN (default:
# release-plan.json beside the checkout).
#
# A plan whose only intents are for the docs unit releases no image: it tags
# the docs unit alone, and CD, which delivers bare X.Y.Z tags, has nothing to
# deliver.
set -euo pipefail

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
plan=${RELEASE_PLAN:-$repo/../release-plan.json}
unit=menagerie

usage() {
  sed -n '2,16p' "$0" >&2
  exit 2
}

fail() {
  echo "$1" >&2
  exit 1
}

has_release_tags() { [ -n "$(git tag --list '[0-9]*.[0-9]*.[0-9]*')" ]; }

has_pending_intents() { [ -n "$(git ls-files '.intentional/intents/*.md')" ]; }

clean() { [ -z "$(git status --porcelain)" ] || fail "the checkout at $(pwd) has changes"; }

# plan_version <plan>: the unit's next version, which is also its tag name;
# empty when the plan does not release the unit (a docs-only plan).
plan_version() {
  local version tag
  version=$(jq -r ".release_units[] | select(.id == \"$unit\") | .new_version" "$1")
  [ -n "$version" ] || return 0
  tag=$(jq -er ".tags[] | select(.release_unit == \"$unit\") | .name" "$1")
  [ "$tag" = "$version" ] || fail "tag $tag does not name version $version"
  echo "$version"
}

# plan_tags <plan>: the names of every tag the plan creates.
plan_tags() { jq -r '[.tags[].name] | join(" ")' "$1"; }

# release_steps: in the current checkout, at the commit to release from.
release_steps() {
  local version tag tags='' released
  clean
  has_pending_intents || fail "no pending intents at $(git rev-parse HEAD): nothing to release"
  if ! has_release_tags; then
    intentional tag --baseline --version "$unit=0.0.0" >/dev/null
    tags="0.0.0 "
  fi
  intentional check
  intentional plan >"$plan"
  version=$(plan_version "$plan")
  [ -n "$(plan_tags "$plan")" ] || fail "the pending intents release nothing"
  if [ -n "$version" ]; then
    released=$version
  else
    released=$(jq -r '[.release_units[] | "\(.id) \(.new_version)"] | join(", ")' "$plan")
  fi
  intentional apply
  git add --all
  git commit -q -m "chore(release): ${version:-$released}"
  intentional tag --plan "$plan"
  for tag in $(plan_tags "$plan"); do
    [ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] || fail "tag $tag is not on the release commit"
  done
  echo "released $released at $(git rev-parse HEAD); tags: $tags${version:-$(plan_tags "$plan")}"
}

[ $# -eq 1 ] || usage
case "$1" in
  release)
    cd "$repo"
    [ "$(git branch --show-current)" = main ] || fail "release from main, not $(git branch --show-current || true)"
    release_steps
    echo "push it: git push --atomic --follow-tags origin main"
    ;;
  rehearse)
    scratch=$(mktemp -d)
    trap 'rm -rf "$scratch"' EXIT
    head=$(git -C "$repo" rev-parse HEAD)
    git clone -q --no-local "$repo" "$scratch/clone"
    cd "$scratch/clone"
    git checkout -q --detach "$head"
    export GIT_AUTHOR_NAME="Release rehearsal" GIT_AUTHOR_EMAIL="rehearsal@example.invalid"
    export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
    plan=$scratch/release-plan.json
    if has_pending_intents; then
      echo "rehearsing the release of $head in $scratch/clone"
      release_steps
      git diff --stat HEAD~1 HEAD
      version=$(plan_version "$plan")
      if [ -n "$version" ]; then
        RELEASE_TAG=$version GITHUB_OUTPUT='' "$repo/scripts/release-tag.sh"
      else
        echo "a docs-only release: CD has no tag to deliver"
      fi
    else
      if ! has_release_tags; then intentional tag --baseline --version "$unit=0.0.0" >/dev/null; fi
      intentional check
      echo "nothing to release at $head: no pending intents"
    fi
    ;;
  *) usage ;;
esac
