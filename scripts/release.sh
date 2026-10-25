#!/usr/bin/env bash
# Prepare and publish a Menagerie release with Intentional.
#
#   release.sh rehearse   in a scratch clone of HEAD, then discarded: with
#                         pending intents, prepare, squash-merge and publish;
#                         at a merged release commit, publish; otherwise
#                         check that there is nothing to release
#   release.sh prepare    at main's head: apply the pending intents and commit
#                         them alone on release/<version>
#   release.sh propose    push release/<version> and open its pull request
#   release.sh publish    at the merged release commit: prove it is the apply
#                         of its parent's plan, then tag it
#   release.sh push-tags  push the tags publish made
#
# A release is reviewed as a pull request holding only the apply commit;
# merging it approves the release. Tags are made only by publish, after the
# merge. Intentional reads the version from tags, so before the first release
# prepare plans against a 0.0.0 baseline it makes and deletes locally, and
# publish makes that baseline for real at the release commit's parent.
#
# prepare and publish write the plan to RELEASE_PLAN (default:
# release-plan.json beside the checkout). prepare commits as
# RELEASE_ACTOR_NAME <RELEASE_ACTOR_EMAIL>. publish records the tags it made
# in RELEASE_PLAN.tags for push-tags.
set -euo pipefail

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
plan=${RELEASE_PLAN:-$repo/../release-plan.json}
unit=menagerie

usage() {
  sed -n '2,15p' "$0" >&2
  exit 2
}

fail() {
  echo "$1" >&2
  exit 1
}

has_release_tags() { [ -n "$(git tag --list '[0-9]*.[0-9]*.[0-9]*')" ]; }

has_pending_intents() { [ -n "$(git ls-files '.intentional/intents/*.md')" ]; }

# release_commit_version: the version HEAD's subject releases, if it is a
# release commit.
release_commit_version() { git log -1 --format=%s | sed -n 's/^chore(release): \([0-9][0-9.]*\)$/\1/p'; }

clean() { [ -z "$(git status --porcelain)" ] || fail "the checkout at $(pwd) has changes"; }

# plan_version <plan>: the unit's next version, which is also its tag name.
plan_version() {
  local version tag
  version=$(jq -er ".release_units[] | select(.id == \"$unit\") | .new_version" "$1")
  tag=$(jq -er ".tags[] | select(.release_unit == \"$unit\") | .name" "$1")
  [ "$tag" = "$version" ] || fail "tag $tag does not name version $version"
  echo "$version"
}

# prepare_steps: in the current checkout, at the commit to release from.
prepare_steps() {
  local base version baseline=
  clean
  has_pending_intents || fail "no pending intents at $(git rev-parse HEAD): nothing to release"
  base=$(git rev-parse HEAD)
  if ! has_release_tags; then
    # Only for planning: publish makes the real baseline after review.
    intentional tag --baseline --version "$unit=0.0.0" >/dev/null
    baseline=0.0.0
  fi
  intentional check
  intentional plan >"$plan"
  version=$(plan_version "$plan")
  intentional apply
  if [ -n "$baseline" ]; then git tag -d "$baseline" >/dev/null; fi
  git switch -q -c "release/$version"
  git add --all
  git -c user.name="${RELEASE_ACTOR_NAME:?}" -c user.email="${RELEASE_ACTOR_EMAIL:?}" \
    commit -q -m "chore(release): $version"
  echo "prepared $version on release/$version: $(git rev-parse HEAD), parent $base"
}

# publish_steps: in the current checkout, at the merged release commit.
publish_steps() {
  local release parent version subject applied scratch
  clean
  release=$(git rev-parse HEAD)
  parent=$(git rev-parse HEAD~1)
  : >"$plan.tags"
  git checkout -q --detach "$parent"
  if ! has_release_tags; then
    intentional tag --baseline --version "$unit=0.0.0" >/dev/null
    echo 0.0.0 >>"$plan.tags"
  fi
  intentional check
  intentional plan >"$plan"
  version=$(plan_version "$plan")
  # The merged commit must be exactly the apply of its parent's plan.
  scratch=$(mktemp -d)
  git worktree add -q --detach "$scratch/apply" "$parent"
  (cd "$scratch/apply" && intentional apply >/dev/null && git add --all && git write-tree) >"$scratch/tree"
  applied=$(cat "$scratch/tree")
  git worktree remove --force "$scratch/apply"
  rm -rf "$scratch"
  git checkout -q --detach "$release"
  [ "$applied" = "$(git rev-parse "HEAD^{tree}")" ] ||
    fail "$release is not the apply of $parent's plan for $version"
  subject=$(git log -1 --format=%s)
  [ "$subject" = "chore(release): $version" ] || fail "$release's subject is '$subject', not 'chore(release): $version'"
  intentional tag --plan "$plan"
  echo "$version" >>"$plan.tags"
  [ "$(git rev-parse "$version^{commit}")" = "$release" ] || fail "tag $version is not on $release"
  echo "published $version at $release; tags: $(tr '\n' ' ' <"$plan.tags")"
}

[ $# -eq 1 ] || usage
case "$1" in
  rehearse)
    scratch=$(mktemp -d)
    trap 'rm -rf "$scratch"' EXIT
    head=$(git -C "$repo" rev-parse HEAD)
    git clone -q --no-local "$repo" "$scratch/clone"
    cd "$scratch/clone"
    git checkout -q --detach "$head"
    # Only HEAD matters here; a branch the clone guessed from it must not
    # collide with the release branch prepare makes.
    git for-each-ref --format='%(refname:short)' refs/heads | xargs -r git branch -q -D
    export RELEASE_ACTOR_NAME="Release rehearsal" RELEASE_ACTOR_EMAIL="rehearsal@example.invalid"
    plan=$scratch/release-plan.json
    released=$(release_commit_version)
    if has_pending_intents; then
      echo "rehearsing the release of $head in $scratch/clone"
      prepare_steps
      git diff --stat HEAD~1 HEAD
      # Squash merge: one new commit on main with the pull request's tree and title.
      merged=$(git -c user.name="$RELEASE_ACTOR_NAME" -c user.email="$RELEASE_ACTOR_EMAIL" \
        commit-tree "HEAD^{tree}" -p "$head" -m "$(git log -1 --format=%s)")
      git checkout -q --detach "$merged"
      publish_steps
    elif [ -n "$released" ] && [ -z "$(git tag --list "$released" --points-at HEAD)" ]; then
      echo "$head is the merged release commit for $released: rehearsing publish in $scratch/clone"
      publish_steps
    else
      if ! has_release_tags; then intentional tag --baseline --version "$unit=0.0.0" >/dev/null; fi
      intentional check
      echo "nothing to release at $head: no pending intents"
    fi
    ;;
  prepare)
    cd "$repo"
    if [ -n "${RELEASE_SHA:-}" ] && [ "$(git rev-parse HEAD)" != "$RELEASE_SHA" ]; then
      fail "the checkout is at $(git rev-parse HEAD), not $RELEASE_SHA"
    fi
    prepare_steps
    ;;
  propose)
    cd "$repo"
    version=$(plan_version "$plan")
    [ "$(git branch --show-current)" = "release/$version" ] || fail "not on release/$version"
    git push origin "release/$version"
    notes=$(jq -er ".release_units[] | select(.id == \"$unit\") | .release_notes" "$plan")
    body=$(
      printf 'This pull request holds only the release commit for %s: the applied intents and the changelog.\n\n' "$version"
      printf 'Merging it approves the release. Then dispatch the Release workflow with action publish and the merged commit to tag it; the tag starts delivery.\n\n'
      printf '%s\n' "$notes"
    )
    gh pr create --base main --head "release/$version" --title "chore(release): $version" --body "$body"
    ;;
  publish)
    cd "$repo"
    if [ -n "${RELEASE_SHA:-}" ] && [ "$(git rev-parse HEAD)" != "$RELEASE_SHA" ]; then
      fail "the checkout is at $(git rev-parse HEAD), not $RELEASE_SHA"
    fi
    publish_steps
    ;;
  push-tags)
    cd "$repo"
    mapfile -t tags <"$plan.tags"
    [ ${#tags[@]} -gt 0 ] || fail "$plan.tags names no tags"
    git push origin "${tags[@]/#/refs/tags/}"
    ;;
  *) usage ;;
esac
