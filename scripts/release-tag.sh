#!/usr/bin/env bash
# Decide whether RELEASE_TAG is a release to deliver. It must be a bare
# X.Y.Z tag that Intentional annotated as a release record. Intentional's
# 0.0.0 baseline record matches the tag pattern but releases nothing, so it
# is reported and not delivered. Writes release=true|false to GITHUB_OUTPUT
# when set.
set -euo pipefail

tag=${RELEASE_TAG:?}
fail() {
  echo "::error::$1" >&2
  exit 1
}
out() { if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "release=$1" >>"$GITHUB_OUTPUT"; fi; }

[[ $tag =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail "tag '$tag' is not a bare X.Y.Z version"
[ "$(git cat-file -t "refs/tags/$tag" 2>/dev/null)" = tag ] || fail "tag $tag is not an annotated tag in this checkout"
record=$(git cat-file -p "refs/tags/$tag")
grep -qx 'intentional release record' <<<"$record" || fail "tag $tag is not an Intentional release record"
grep -qx "version: $tag" <<<"$record" || fail "tag $tag records another version"
if grep -qx 'baseline: true' <<<"$record"; then
  echo "tag $tag is Intentional's baseline; nothing to deliver"
  out false
  exit 0
fi
echo "tag $tag is a release of commit $(git rev-parse "refs/tags/$tag^{commit}")"
out true
