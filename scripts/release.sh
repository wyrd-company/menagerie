#!/usr/bin/env bash
# Prepare a Menagerie release with Intentional.
#
#   release.sh rehearse   run the release steps in a scratch clone of HEAD and
#                         discard it; nothing leaves the clone
#   release.sh cut        in this checkout, at the release commit: plan, apply,
#                         commit and tag; pushes nothing
#   release.sh push       push the release commit to main, then its tags
#
# Intentional reads the version from tags. Before the first release there is
# no tag, so the steps first establish the 0.0.0 baseline at the release
# commit. rehearse does that only inside its clone; cut does it for real,
# under the release workflow's evidence gate. Both run the same steps, so a
# passing rehearsal is the first release's plan.
#
# cut uses RELEASE_ACTOR_NAME and RELEASE_ACTOR_EMAIL for the release commit.
# cut writes the plan to RELEASE_PLAN (default: release-plan.json beside the
# checkout) and the tags it made to RELEASE_PLAN.tags, which push reads.
set -euo pipefail

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)

usage() {
  sed -n '2,9p' "$0" >&2
  exit 2
}

# release_steps <checkout> <plan>: baseline when absent, check, plan, apply,
# commit and tag, in <checkout>.
release_steps() {
  local dir=$1 plan=$2 version tag
  cd "$dir"
  [ -z "$(git status --porcelain)" ] || { echo "the checkout has changes" >&2; return 1; }
  : >"$plan.tags"
  if [ -z "$(git tag --list '[0-9]*.[0-9]*.[0-9]*')" ]; then
    echo "no release tag yet: establishing the 0.0.0 baseline at $(git rev-parse HEAD)"
    intentional tag --baseline --version menagerie=0.0.0
    echo 0.0.0 >>"$plan.tags"
  fi
  intentional check
  intentional plan >"$plan"
  version=$(jq -er '.release_units[] | select(.id == "menagerie") | .new_version' "$plan")
  tag=$(jq -er '.tags[] | select(.release_unit == "menagerie") | .name' "$plan")
  [ "$tag" = "$version" ] || { echo "tag $tag does not name version $version" >&2; return 1; }
  intentional apply
  git add --all
  git -c user.name="$RELEASE_ACTOR_NAME" -c user.email="$RELEASE_ACTOR_EMAIL" \
    commit -q -m "chore(release): $version"
  intentional tag --plan "$plan"
  echo "$tag" >>"$plan.tags"
  [ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] || {
    echo "tag $tag is not on the release commit" >&2
    return 1
  }
  echo "release $version: commit $(git rev-parse HEAD), tags $(git tag --points-at HEAD | tr '\n' ' ')"
}

[ $# -eq 1 ] || usage
case "$1" in
  rehearse)
    scratch=$(mktemp -d)
    trap 'rm -rf "$scratch"' EXIT
    head=$(git -C "$repo" rev-parse HEAD)
    git clone -q --no-local "$repo" "$scratch/clone"
    git -C "$scratch/clone" checkout -q --detach "$head"
    echo "rehearsing the release of $head; tags made here stay in $scratch/clone"
    RELEASE_ACTOR_NAME="Release rehearsal" RELEASE_ACTOR_EMAIL="rehearsal@example.invalid" \
      release_steps "$scratch/clone" "$scratch/release-plan.json"
    git -C "$scratch/clone" diff --stat HEAD~1 HEAD
    ;;
  cut)
    : "${RELEASE_ACTOR_NAME:?}" "${RELEASE_ACTOR_EMAIL:?}"
    if [ -n "${RELEASE_SHA:-}" ] && [ "$(git -C "$repo" rev-parse HEAD)" != "$RELEASE_SHA" ]; then
      echo "the checkout is at $(git -C "$repo" rev-parse HEAD), not $RELEASE_SHA" >&2
      exit 1
    fi
    release_steps "$repo" "${RELEASE_PLAN:-$repo/../release-plan.json}"
    ;;
  push)
    cd "$repo"
    plan=${RELEASE_PLAN:-$repo/../release-plan.json}
    mapfile -t tags <"$plan.tags"
    [ ${#tags[@]} -gt 0 ] || { echo "$plan.tags names no tags" >&2; exit 1; }
    # The release commit lands first, so the tags' push starts delivery of a
    # commit that main already holds. If main has moved on, this push is
    # refused and nothing is published. If the tag push fails, recreate the
    # tags at the release commit with `intentional tag --plan` and the
    # release-plan artifact, then push them.
    git push origin HEAD:refs/heads/main
    git push origin "${tags[@]/#/refs/tags/}"
    ;;
  *) usage ;;
esac
