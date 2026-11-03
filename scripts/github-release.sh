#!/usr/bin/env bash
# The GitHub Release of RELEASE_TAG, through gh and GH_TOKEN.
#
#   github-release.sh publish    after CD delivered the image of a release
#                                tag: publish its Release, with the tag's
#                                CHANGELOG.md section as the notes. A Release
#                                that is already published stays as it is, so
#                                a re-run or a dispatch changes nothing.
#   github-release.sh docs-tag   for a pushed docs tag: write publish=true|false
#                                to GITHUB_OUTPUT when set. A code release tag
#                                on the same commit that has no published
#                                Release gives false, because the publisher
#                                needs that Release and CD starts a publication
#                                of this commit when it succeeds.
set -euo pipefail

scripts=$(cd "$(dirname "$0")" && pwd)
tag=${RELEASE_TAG:?}
release_tags='[0-9]*.[0-9]*.[0-9]*'

fail() {
  echo "::error::$1" >&2
  exit 1
}

# release_state <tag>: absent, draft or published. Any other answer from
# GitHub fails, so that a failed read does not count as a missing Release.
release_state() {
  local answer
  if answer=$(gh release view "$1" --json isDraft --jq .isDraft 2>&1); then
    case $answer in
      true) echo draft ;;
      false) echo published ;;
      *) fail "unexpected answer for the GitHub Release of $1: $answer" ;;
    esac
  elif [ "$answer" = "release not found" ]; then
    echo absent
  else
    fail "cannot read the GitHub Release of $1: $answer"
  fi
}

# notes <tag>: the body of the tag's section in the tag's own CHANGELOG.md.
notes() {
  local changelog text
  changelog=$(git show "refs/tags/$1:CHANGELOG.md") || fail "tag $1 has no CHANGELOG.md"
  text=$(awk -v heading="## $1" '$0 == heading { found = 1; next } /^## / { found = 0 } found' <<<"$changelog" | sed '/./,$!d')
  [ -n "$text" ] || fail "CHANGELOG.md at tag $1 has no notes for $1"
  printf '%s\n' "$text"
}

# is_latest <tag>: true unless a higher release tag exists, so that delivering
# an older tag again does not take Latest from a newer Release.
is_latest() {
  if [ "$(git tag --list "$release_tags" | sort -V | tail -n 1)" = "$1" ]; then echo true; else echo false; fi
}

publish() {
  local state text latest
  state=$(release_state "$tag")
  case $state in
    published)
      echo "the GitHub Release of $tag is already published"
      return
      ;;
    draft)
      latest=$(is_latest "$tag")
      gh release edit "$tag" --draft=false --latest="$latest"
      ;;
    absent)
      text=$(notes "$tag")
      latest=$(is_latest "$tag")
      gh release create "$tag" --verify-tag --title "$tag" --latest="$latest" --notes-file - <<<"$text"
      ;;
  esac
  echo "published the GitHub Release of $tag"
}

# is_code_release <tag>: release-tag.sh decides, as it does for CD.
is_code_release() {
  local output
  output=$(mktemp)
  if RELEASE_TAG=$1 GITHUB_OUTPUT=$output "$scripts/release-tag.sh" >/dev/null 2>&1 &&
    grep -qx 'release=true' "$output"; then
    rm -f "$output"
    return 0
  fi
  rm -f "$output"
  return 1
}

docs_tag() {
  local code state decision=true
  for code in $(git tag --points-at "refs/tags/$tag^{commit}" --list "$release_tags"); do
    is_code_release "$code" || continue
    state=$(release_state "$code")
    if [ "$state" != published ]; then
      echo "::notice::the GitHub Release of $code is not published yet; CD publishes these docs when it succeeds"
      decision=false
    fi
  done
  if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "publish=$decision" >>"$GITHUB_OUTPUT"; fi
  echo "publish=$decision"
}

[ $# -eq 1 ] || fail "usage: github-release.sh publish|docs-tag"
case $1 in
  publish) publish ;;
  docs-tag) docs_tag ;;
  *) fail "usage: github-release.sh publish|docs-tag" ;;
esac
