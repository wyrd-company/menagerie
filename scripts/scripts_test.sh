#!/usr/bin/env bash
# Tests for the release and CI scripts. Each case runs the real script: the
# release cases in scratch clones of HEAD with the working tree's scripts
# committed into them, the evidence cases with a stub gh on PATH. Nothing
# leaves the scratch folder.
set -uo pipefail

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
passed=0 failed=0

pass() { passed=$((passed + 1)); echo "ok   $1"; }
failure() { failed=$((failed + 1)); echo "FAIL $1"; }
# expect <name> <status> <pattern> <command...>: the exit status and an output line.
expect() {
  local name=$1 status=$2 pattern=$3 out code
  shift 3
  out=$("$@" 2>&1)
  code=$?
  if [ "$code" -ne "$status" ]; then
    failure "$name: exit $code"
    printf '%s\n' "$out" | sed 's/^/     /'
  elif ! grep -Eq -- "$pattern" <<<"$out"; then
    failure "$name: no line matches /$pattern/"
    printf '%s\n' "$out" | sed 's/^/     /'
  else
    pass "$name"
  fi
}

git_test() { git -c user.name="Example User" -c user.email=user@example.invalid "$@"; }

# clone_at_head <dir>: a clone of HEAD carrying the working tree's scripts,
# with no release tags, no changelog and one fixture intent in place of the
# repository's own, so every case starts from the same first-release state
# whatever the repository's pending intents, tags and changelog are.
clone_at_head() {
  git clone -q --no-local "$repo" "$1"
  git -C "$1" checkout -q --detach "$(git -C "$repo" rev-parse HEAD)"
  git -C "$1" tag --list | xargs -r git -C "$1" tag -d >/dev/null
  cp "$repo"/scripts/*.sh "$1/scripts/"
  git -C "$1" add scripts
  git -C "$1" ls-files -z '.intentional/intents/*.md' | xargs -r -0 git -C "$1" rm -q
  git -C "$1" rm -q --ignore-unmatch CHANGELOG.md
  (cd "$1" && intentional add --release-unit menagerie:minor --message "A fixture change." >/dev/null)
  git -C "$1" add .intentional/intents
  git_test -C "$1" commit -q -m "test: scripts and a fixture intent"
}

export RELEASE_ACTOR_NAME="Release test" RELEASE_ACTOR_EMAIL=release@example.invalid

# A prepared release, squash-merged onto its base.
r=$scratch/release
clone_at_head "$r"
base=$(git -C "$r" rev-parse HEAD)
expect "prepare commits the release alone on its branch" 0 'prepared 0\.1\.0 on release/0\.1\.0' \
  env RELEASE_PLAN="$scratch/prepare.json" RELEASE_SHA="$base" "$r/scripts/release.sh" prepare
expect "prepare leaves no tag" 0 '^$' git -C "$r" tag --list
expect "the release commit writes the changelog and deletes the fixture intent, nothing else" 0 '^D:\.intentional/intents/[a-z0-9-]+\.md A:CHANGELOG\.md $' \
  sh -c "git -C '$r' diff --name-status HEAD~1 HEAD | sort -k2 | awk '{ printf \"%s:%s \", \$1, \$2 }'; echo"
expect "prepare refuses another commit than RELEASE_SHA" 1 'not 0{40}' \
  env RELEASE_PLAN="$scratch/other.json" RELEASE_SHA=0000000000000000000000000000000000000000 "$r/scripts/release.sh" prepare
release_tree=$(git -C "$r" rev-parse 'HEAD^{tree}')
merge() { # merge <tree> <subject>: a squash commit on base, checked out
  git -C "$r" checkout -q --detach "$(git_test -C "$r" commit-tree "$1" -p "$base" -m "$2")"
  git -C "$r" tag --list '[0-9]*' | xargs -r git -C "$r" tag -d >/dev/null
}

merge "$release_tree" "chore(release): 0.1.0"
# CI runs on the merged release commit before publish tags it.
expect "the rehearsal at a merged release commit rehearses publish" 0 'merged release commit for 0\.1\.0' "$r/scripts/release.sh" rehearse
expect "the rehearsal at a merged release commit publishes in its clone only" 0 'published 0\.1\.0 .* tags: 0\.0\.0 0\.1\.0' "$r/scripts/release.sh" rehearse
expect "the rehearsal leaves the merged release untagged" 0 '^$' git -C "$r" tag --list
if [ -z "${SCRIPTS_TEST_NESTED:-}" ]; then
  expect "these script tests pass at a merged release commit" 0 'scripts: [0-9]+ passed, 0 failed' \
    env SCRIPTS_TEST_NESTED=1 "$r/scripts/scripts_test.sh"
fi
expect "publish tags a reviewed release and its baseline" 0 'published 0\.1\.0 .* tags: 0\.0\.0 0\.1\.0' \
  env RELEASE_PLAN="$scratch/publish.json" "$r/scripts/release.sh" publish
expect "publish puts the baseline on the release commit's parent" 0 "^$base$" git -C "$r" rev-parse '0.0.0^{commit}'
published=$(git -C "$r" rev-parse HEAD)
expect "the rehearsal after publish finds nothing to release" 0 'nothing to release' "$r/scripts/release.sh" rehearse
expect "prepare refuses when no intent is pending" 1 'no pending intents' \
  env RELEASE_PLAN="$scratch/none.json" "$r/scripts/release.sh" prepare
git_test -C "$r" commit -q --allow-empty -m "test: an ordinary commit after the release"
expect "the rehearsal at a later commit without intents finds nothing to release" 0 'nothing to release' "$r/scripts/release.sh" rehearse
git -C "$r" checkout -q --detach "$published"

# A merged release that someone edited after review.
git -C "$r" checkout -q --detach "$(git -C "$r" rev-parse HEAD)"
echo "edited after review" >>"$r/CHANGELOG.md"
git -C "$r" add CHANGELOG.md
edited_tree=$(git -C "$r" write-tree)
git -C "$r" checkout -q -- . 2>/dev/null
git -C "$r" reset -q --hard
merge "$edited_tree" "chore(release): 0.1.0"
expect "publish refuses a release commit that is not the plan's apply" 1 'is not the apply of' \
  env RELEASE_PLAN="$scratch/edited.json" "$r/scripts/release.sh" publish
expect "a refused publish makes no release tag" 0 '^$' sh -c "git -C '$r' tag --list 0.1.0"

merge "$release_tree" "chore: an ordinary title"
expect "publish refuses a release commit with another subject" 1 "not 'chore\(release\): 0\.1\.0'" \
  env RELEASE_PLAN="$scratch/subject.json" "$r/scripts/release.sh" publish

merge "$(git -C "$r" rev-parse "$base^{tree}")" "chore(release): 0.1.0"
expect "publish refuses a commit that applies nothing" 1 'is not the apply of' \
  env RELEASE_PLAN="$scratch/nothing.json" "$r/scripts/release.sh" publish

# release-tag.sh: which tags cd.yml delivers.
t=$scratch/tags
git init -q "$t"
git_test -C "$t" commit -q --allow-empty -m "test: tagged"
record() { printf 'intentional release record\n\ncontract: contract-1\nversion: %s\n%s' "$1" "${2:-}"; }
git_test -C "$t" tag -a 0.0.0 -m "$(record 0.0.0 'baseline: true')"
git_test -C "$t" tag -a 0.1.0 -m "$(record 0.1.0 'baseline: false')"
git_test -C "$t" tag -a 0.2.0 -m "a hand-made tag"
git_test -C "$t" tag -a 0.3.0 -m "$(record 0.4.0)"
git_test -C "$t" tag -a v0.5.0 -m "$(record v0.5.0)"
git -C "$t" tag 0.6.0
tag_check() { (cd "$t" && GITHUB_OUTPUT="$scratch/output" RELEASE_TAG=$1 "$repo/scripts/release-tag.sh" && cat "$scratch/output"); }
: >"$scratch/output"
expect "a release record is delivered" 0 '^release=true$' tag_check 0.1.0
: >"$scratch/output"
expect "the baseline record is not delivered" 0 '^release=false$' tag_check 0.0.0
expect "a tag Intentional did not write is refused" 1 'not an Intentional release record' tag_check 0.2.0
expect "a record of another version is refused" 1 'records another version' tag_check 0.3.0
expect "a v-prefixed tag is refused" 1 'not a bare X\.Y\.Z' tag_check v0.5.0
expect "a lightweight tag is refused" 1 'not an annotated tag' tag_check 0.6.0

# release-evidence.sh with a stub gh that answers from files.
mkdir -p "$scratch/bin"
cat >"$scratch/bin/gh" <<'STUB'
#!/usr/bin/env sh
case "$2" in
  */commits/main) cat "$STUB_DIR/main" ;;
  */runs\?*) cat "$STUB_DIR/runs" ;;
  *) echo "unexpected gh $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$scratch/bin/gh"
sha=1111111111111111111111111111111111111111
evidence() {
  PATH="$scratch/bin:$PATH" STUB_DIR=$scratch GITHUB_REPOSITORY=example-org/example RELEASE_SHA=$1 \
    "$repo/scripts/release-evidence.sh"
}
echo "$sha" >"$scratch/main"
echo "123 456" >"$scratch/runs"
expect "main's head with a successful push run passes" 0 'successful push CI runs: 123 456' evidence "$sha"
expect "an abbreviated SHA is refused" 1 'not a full lower-case commit SHA' evidence 1111111
echo 2222222222222222222222222222222222222222 >"$scratch/main"
expect "a commit that is not main's head is refused" 1 "is not main's head" evidence "$sha"
echo "$sha" >"$scratch/main"
: >"$scratch/runs"
expect "a commit without a successful push run is refused" 1 'no successful push-event CI run' evidence "$sha"

# require-env.sh and check-commit-subject.sh.
expect "set variables pass" 0 '^$' env SAMPLE_ONE=1 SAMPLE_TWO=2 sh "$repo/scripts/require-env.sh" SAMPLE_ONE SAMPLE_TWO
expect "an empty variable is named, not shown" 1 'Not configured for this workflow: SAMPLE_TWO$' \
  env SAMPLE_ONE=1 SAMPLE_TWO= sh "$repo/scripts/require-env.sh" SAMPLE_ONE SAMPLE_TWO
expect "a name that is not a variable is refused" 2 'not a variable name' sh "$repo/scripts/require-env.sh" 'SAMPLE;true'
expect "a conventional subject passes" 0 '^$' sh "$repo/scripts/check-commit-subject.sh" "feat(image): add a sample"
expect "another subject fails" 1 'not a conventional commit subject' sh "$repo/scripts/check-commit-subject.sh" "Add a sample"
printf 'fix: a sample\n\nBody text.\n' >"$scratch/message"
expect "a message file's first line is the subject" 0 '^$' sh "$repo/scripts/check-commit-subject.sh" "$scratch/message"
printf 'Sample\n\nfix: not the first line\n' >"$scratch/message"
expect "a message file with another first line fails" 1 'not a conventional commit subject: Sample' \
  sh "$repo/scripts/check-commit-subject.sh" "$scratch/message"

# Every Task suite on the disposable daemon passes one per-invocation owner
# token to up and to its deferred down, so a refused up cannot clean up
# another invocation's run.
wrappers=$(yq -r '.tasks | to_entries[] | select([.value.cmds[]? | select(type == "!!str" and test("isolated-docker.sh up"))] | length > 0) | .key' "$repo/taskfile.yml")
expect "the disposable-daemon suites are found" 0 '^test:docker$' echo "$wrappers"
# owner_wired <task>: OWNER comes from urandom, the first command is the
# deferred down with it, and the first up passes it.
owner_wired() {
  local down up
  down="scripts/isolated-docker.sh down '{{.STATE}}' '{{.OWNER}}'"
  up="scripts/isolated-docker.sh up '{{.STATE}}' '{{.OWNER}}'"
  TASK=$1 DOWN=$down UP=$up yq -e '.tasks[strenv(TASK)] | (
    (.vars.OWNER.sh | test("urandom")) and (.cmds[0].defer == strenv(DOWN)) and
    ([.cmds[] | select(type == "!!str" and test("isolated-docker.sh up"))][0] == strenv(UP)))' "$repo/taskfile.yml"
}
for name in $wrappers; do
  expect "$name passes its owner token to up and to the deferred down" 0 '^true$' owner_wired "$name"
done

# The pre-commit hook formats and lints only the staged files. An unrelated
# unstaged edit keeps its bytes, and a partially staged file keeps its
# unstaged hunk unformatted.
hook_case() { # hook_case <dir>: prints the outcome lines the expectations read
  local d=$1
  cp "$repo/.config/lefthook.yml" "$d/.config/lefthook.yml"
  cp "$repo/taskfile.yml" "$d/taskfile.yml"
  git -C "$d" add .config/lefthook.yml taskfile.yml
  git_test -C "$d" commit -q --allow-empty -m "test: hooks under test"
  printf 'func   unrelated() {}\n' >>"$d/backend/internal/jobs/run.go"
  cp "$d/backend/internal/jobs/run.go" "$scratch/unrelated.before"
  printf 'package jobs\n\nfunc sampleStaged() {  }\n' >"$d/backend/internal/jobs/sample_hook.go"
  printf '\nA staged sentence.\n' >>"$d/README.md"
  git -C "$d" add backend/internal/jobs/sample_hook.go README.md
  printf '\nfunc   sampleUnstaged() {}\n' >>"$d/backend/internal/jobs/sample_hook.go"
  (cd "$d" && lefthook run pre-commit --no-auto-install >"$scratch/hook.log" 2>&1) && echo "hook=passed" || echo "hook=failed"
  cmp -s "$scratch/unrelated.before" "$d/backend/internal/jobs/run.go" && echo "unrelated=unchanged" || echo "unrelated=changed"
  git -C "$d" diff --cached --quiet -- backend/internal/jobs/run.go && echo "unrelated-index=unchanged" || echo "unrelated-index=changed"
  git -C "$d" show :backend/internal/jobs/sample_hook.go | grep -qx 'func sampleStaged() {}' && echo "staged=formatted" || echo "staged=unformatted"
  grep -qx 'func   sampleUnstaged() {}' "$d/backend/internal/jobs/sample_hook.go" && echo "unstaged-hunk=kept" || echo "unstaged-hunk=lost"
  git -C "$d" show :backend/internal/jobs/sample_hook.go | grep -q sampleUnstaged && echo "unstaged-hunk-staged=yes" || echo "unstaged-hunk-staged=no"
}
h=$scratch/hooks
clone_at_head "$h"
hook_out=$(hook_case "$h")
expect "the pre-commit hook passes on staged files" 0 '^hook=passed$' echo "$hook_out"
expect "the hook leaves an unrelated unstaged file's bytes" 0 '^unrelated=unchanged$' echo "$hook_out"
expect "the hook leaves an unrelated file unstaged" 0 '^unrelated-index=unchanged$' echo "$hook_out"
expect "the hook formats the staged hunk" 0 '^staged=formatted$' echo "$hook_out"
expect "the hook keeps the unstaged hunk as it was" 0 '^unstaged-hunk=kept$' echo "$hook_out"
expect "the hook does not stage the unstaged hunk" 0 '^unstaged-hunk-staged=no$' echo "$hook_out"

# Toolchain versions named in several places agree, so a bump moves them all.
go_version=$(sed -n 's/^go //p' "$repo/go.mod")
node_version=$(cat "$repo/.node-version")
rust_versions=$(grep -ho 'toolchain: [0-9.]*' "$repo"/.github/workflows/*.yml | sort -u)
expect "the image builds with go.mod's Go" 0 "golang:$go_version-" grep -o "golang:[^@]*" "$repo/Dockerfile"
expect "the image builds with .node-version's Node.js" 0 "node:$node_version-" grep -o "node:[^@ ]*" "$repo/Dockerfile"
expect "every workflow names one Rust toolchain" 0 '^toolchain: [0-9.]+$' echo "$rust_versions"
rust_version=${rust_versions#toolchain: }
expect "the image builds with the workflows' Rust" 0 "rust:$rust_version-" grep -o "rust:[^@]*" "$repo/Dockerfile"
expect "the devcontainer has the same Go, Node.js and Rust" 0 '^3$' sh -c "grep -Ec '\"version\": \"($go_version|$node_version|$rust_version)\"' '$repo/.devcontainer/devcontainer.json'"

echo "scripts: $passed passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$passed" -gt 0 ]
