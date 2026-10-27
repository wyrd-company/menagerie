#!/usr/bin/env bash
# Tests for the release and CI scripts. Each case runs the real script: the
# release cases in scratch clones of HEAD with the working tree's scripts
# committed into them. Nothing leaves the scratch folder.
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

# The tools these cases run; a missing one fails here by name, not later
# inside a hook.
for tool in git intentional jq yq node task lefthook gofmt rustfmt rumdl; do
  command -v "$tool" >/dev/null || { echo "FAIL $tool is not installed; scripts_test.sh needs it"; exit 1; }
done

git_test() { git -c user.name="Example User" -c user.email=user@example.invalid "$@"; }

# clone_at_head <dir>: a clone of HEAD carrying the working tree's scripts,
# with no release tags, no changelog and one fixture intent in place of the
# repository's own, so every case starts from the same first-release state
# whatever the repository's pending intents, tags and changelog are.
clone_at_head() {
  git clone -q --no-local "$repo" "$1"
  git -C "$1" checkout -q --detach "$(git -C "$repo" rev-parse HEAD)"
  git -C "$1" tag --list | xargs -r git -C "$1" tag -d >/dev/null
  git -C "$1" for-each-ref --format='%(refname:short)' refs/heads | xargs -r git -C "$1" branch -q -D
  cp "$repo"/scripts/*.sh "$1/scripts/"
  git -C "$1" add scripts
  git -C "$1" ls-files -z '.intentional/intents/*.md' | xargs -r -0 git -C "$1" rm -q
  git -C "$1" rm -q --ignore-unmatch CHANGELOG.md
  (cd "$1" && intentional add --release-unit menagerie:minor --message "A fixture change." >/dev/null)
  git -C "$1" add .intentional/intents
  git_test -C "$1" commit -q -m "test: scripts and a fixture intent"
}

# The release commit takes the checkout's Git identity; a runner has none.
export GIT_AUTHOR_NAME="Example User" GIT_AUTHOR_EMAIL=user@example.invalid
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"

# A first release from main.
r=$scratch/release
clone_at_head "$r"
base=$(git -C "$r" rev-parse HEAD)
expect "the rehearsal releases and checks CD's view of the tag" 0 'tag 0\.1\.0 is a release of commit' "$r/scripts/release.sh" rehearse
expect "the rehearsal leaves its clone's source untagged" 0 '^$' git -C "$r" tag --list
expect "release refuses a checkout off main" 1 'release from main, not' "$r/scripts/release.sh" release
git -C "$r" switch -q -c main
echo "a local edit" >>"$r/README.md"
expect "release refuses a checkout with changes" 1 'has changes' "$r/scripts/release.sh" release
git -C "$r" checkout -q -- README.md
expect "release on main tags the release and its baseline" 0 'released 0\.1\.0 at [0-9a-f]{40}; tags: 0\.0\.0 0\.1\.0$' \
  bash -o pipefail -c "RELEASE_PLAN='$scratch/first.json' '$r/scripts/release.sh' release 2>&1 | tee '$scratch/release.log'"
expect "release names the push that delivers it" 0 '^push it: git push --atomic --follow-tags origin main$' cat "$scratch/release.log"
release=$(git -C "$r" rev-parse HEAD)
expect "the release commit writes the changelog and deletes the fixture intent, nothing else" 0 '^D:\.intentional/intents/[a-z0-9-]+\.md A:CHANGELOG\.md $' \
  sh -c "git -C '$r' diff --name-status HEAD~1 HEAD | sort -k2 | awk '{ printf \"%s:%s \", \$1, \$2 }'; echo"
expect "the release commit's subject names its version" 0 '^chore\(release\): 0\.1\.0$' git -C "$r" log -1 --format=%s
expect "the release tag is on the release commit" 0 "^$release$" git -C "$r" rev-parse '0.1.0^{commit}'
expect "the baseline is on the release commit's parent" 0 "^$base$" git -C "$r" rev-parse '0.0.0^{commit}'
cd_check() { (cd "$r" && GITHUB_OUTPUT="$scratch/cd-output" RELEASE_TAG=$1 "$r/scripts/release-tag.sh" && cat "$scratch/cd-output"); }
: >"$scratch/cd-output"
expect "CD delivers the release tag" 0 '^release=true$' cd_check 0.1.0
: >"$scratch/cd-output"
expect "CD delivers nothing for the baseline tag" 0 '^release=false$' cd_check 0.0.0
expect "the rehearsal after the release finds nothing to release" 0 'nothing to release' "$r/scripts/release.sh" rehearse
expect "release refuses when no intent is pending" 1 'no pending intents' \
  env RELEASE_PLAN="$scratch/none.json" "$r/scripts/release.sh" release
if [ -z "${SCRIPTS_TEST_NESTED:-}" ]; then
  expect "these script tests pass at a release commit" 0 'scripts: [0-9]+ passed, 0 failed' \
    env SCRIPTS_TEST_NESTED=1 "$r/scripts/scripts_test.sh"
fi
git_test -C "$r" commit -q --allow-empty -m "test: an ordinary commit after the release"
expect "the rehearsal at a later commit without intents finds nothing to release" 0 'nothing to release' "$r/scripts/release.sh" rehearse

# A second release has no new baseline.
(cd "$r" && intentional add --release-unit menagerie:minor --message "A second fixture change." >/dev/null)
git -C "$r" add .intentional/intents
git_test -C "$r" commit -q -m "test: a second fixture intent"
expect "a second release tags only its own version" 0 'released 0\.2\.0 at [0-9a-f]{40}; tags: 0\.2\.0$' \
  env RELEASE_PLAN="$scratch/second.json" "$r/scripts/release.sh" release

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
  # Rust: a parent module partially staged, one of its child modules with
  # only an unstaged edit, and another child module staged.
  printf '\nfn   sample_staged() {}\n' >>"$d/template-service/src/lib.rs"
  printf 'fn   sample_staged_child() {}\n' >>"$d/template-service/src/numbers.rs"
  git -C "$d" add template-service/src/lib.rs template-service/src/numbers.rs
  printf '\nfn   sample_unstaged() {}\n' >>"$d/template-service/src/lib.rs"
  printf '\nfn   sample_unstaged_child() {}\n' >>"$d/template-service/src/config.rs"
  cp "$d/template-service/src/config.rs" "$scratch/child.before"
  if (cd "$d" && lefthook run pre-commit --no-auto-install >"$scratch/hook.log" 2>&1); then
    echo "hook=passed"
  else
    echo "hook=failed"
    sed "s/^/hook: /" "$scratch/hook.log"
  fi
  cmp -s "$scratch/child.before" "$d/template-service/src/config.rs" && echo "rust-child=unchanged" || echo "rust-child=changed"
  git -C "$d" diff --cached --quiet -- template-service/src/config.rs && echo "rust-child-index=unchanged" || echo "rust-child-index=changed"
  git -C "$d" show :template-service/src/lib.rs | grep -qx 'fn sample_staged() {}' && echo "rust-staged=formatted" || echo "rust-staged=unformatted"
  git -C "$d" show :template-service/src/numbers.rs | grep -qx 'fn sample_staged_child() {}' && echo "rust-staged-child=formatted" || echo "rust-staged-child=unformatted"
  grep -qx 'fn   sample_unstaged() {}' "$d/template-service/src/lib.rs" && echo "rust-unstaged-hunk=kept" || echo "rust-unstaged-hunk=lost"
  git -C "$d" show :template-service/src/lib.rs | grep -q sample_unstaged && echo "rust-unstaged-hunk-staged=yes" || echo "rust-unstaged-hunk-staged=no"
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
expect "the hook leaves an unstaged Rust child module's bytes" 0 '^rust-child=unchanged$' echo "$hook_out"
expect "the hook leaves that child module unstaged" 0 '^rust-child-index=unchanged$' echo "$hook_out"
expect "the hook formats a staged Rust parent module's staged hunk" 0 '^rust-staged=formatted$' echo "$hook_out"
expect "the hook formats a staged Rust child module" 0 '^rust-staged-child=formatted$' echo "$hook_out"
expect "the hook keeps the Rust parent's unstaged hunk as it was" 0 '^rust-unstaged-hunk=kept$' echo "$hook_out"
expect "the hook does not stage the Rust parent's unstaged hunk" 0 '^rust-unstaged-hunk-staged=no$' echo "$hook_out"
printf 'fn   sample_lint() {}\n' >>"$h/template-service/src/numbers.rs"
expect "lint:staged refuses an unformatted Rust file" 1 'not rustfmt-formatted: template-service/src/numbers.rs' "$h/scripts/staged.sh" lint template-service/src/numbers.rs
expect "lint:staged passes a formatted Rust file" 0 '^$' "$h/scripts/staged.sh" lint template-service/src/create.rs

# check-workflows.mjs refuses a job that runs a task before its checkout,
# and a job that lacks a tool its task needs.
wfs=$scratch/workflows
mkdir -p "$wfs"
yq '.jobs.tag.steps |= ([.[] | select(.uses == null or (.uses | test("^actions/checkout") | not))])' \
  "$repo/.github/workflows/cd.yml" >"$wfs/cd.yml"
yq '.jobs.test.steps |= ([.[] | select(.run != "task tools:install:linters")])' "$repo/.github/workflows/ci.yml" >"$wfs/ci.yml"
expect "the workflow check passes this repository's workflows" 0 'checked [0-9]+ task steps in 5 workflows' \
  sh -c "cd '$repo' && node scripts/check-workflows.mjs"
expect "the workflow check refuses a task before checkout" 1 'tag step [0-9]+ runs task release:tag before a step checks out' \
  sh -c "cd '$repo' && node scripts/check-workflows.mjs '$wfs/cd.yml'"
expect "the workflow check refuses a job missing a tool its task needs" 1 'ci.yml test runs task test without an earlier task:tools:install:linters' \
  sh -c "cd '$repo' && node scripts/check-workflows.mjs '$wfs/ci.yml'"

# check-docs.mjs refuses an image a published page shows but docs.yml does
# not list, an asset outside docs/, and a docs folder with no published page.
docs=$scratch/docs
expect "the docs check passes this repository's docs" 0 'checked [0-9]+ pages and [0-9]+ assets' \
  sh -c "cd '$repo' && node scripts/check-docs.mjs"
cp -r "$repo/docs" "$docs-unlisted"
yq -i 'del(.assets[] | select(. == "assets/list.png"))' "$docs-unlisted/docs.yml"
expect "the docs check refuses an unlisted image" 1 'overview.md shows assets/list.png, which docs.yml does not list' \
  sh -c "cd '$repo' && node scripts/check-docs.mjs '$docs-unlisted'"
cp -r "$repo/docs" "$docs-outside"
echo "a file beside the docs folder" >"$scratch/outside.md"
yq -i '.assets += ["../outside.md"]' "$docs-outside/docs.yml"
expect "the docs check refuses an asset outside docs" 1 'asset ../outside.md is outside' \
  sh -c "cd '$repo' && node scripts/check-docs.mjs '$docs-outside'"
mkdir -p "$docs-unpublished"
cp "$repo/docs/docs.yml" "$docs-unpublished/"
cp -r "$repo/docs/assets" "$docs-unpublished/"
printf -- '---\ntitle: Draft\n---\n\nNot published.\n' >"$docs-unpublished/draft.md"
expect "the docs check refuses a folder with no published page" 1 'no page in .* is marked docs: true' \
  sh -c "cd '$repo' && node scripts/check-docs.mjs '$docs-unpublished'"

# The live suite's Docker proxy profile is the shipped one with test paths,
# the runner's uid and a marked stand-in per pending fork change only.
render() { "$repo/scripts/proxy-config.sh" 0 /work/tmp /work/tmp/proxy/workspaces "$@"; }
expect "the proxy profile takes the runner's uid" 0 '^      uids: \[0\]$' render
expect "the proxy profile allows only the test bind root" 0 '^3$' sh -c "'$repo/scripts/proxy-config.sh' 0 /work/tmp /work/tmp/proxy/workspaces | grep -c '^            - /work/tmp\$'"
expect "the proxy profile's exceptions root is the test workspace root" 0 '^  workspace_root: /work/tmp/proxy/workspaces$' render
expect "the proxy profile keeps exec's allow_any_command" 0 '^          allow_any_command: true$' render 1571 1814
expect "pending 1815, merged, has no stand-in" 2 'no stand-in is known for pending issue 1815' render 1815
expect "the proxy profile keeps its ownership block" 0 '^ownership:$' render 1571 1814
expect "pending 1819, merged, has no stand-in" 2 'no stand-in is known for pending issue 1819' render 1819
expect "an unknown pending change is refused" 2 'no stand-in is known for pending issue 9999' render 9999
expect "the proxy profile takes a list of uids" 0 '^      uids: \[0, 1000\]$' "$repo/scripts/proxy-config.sh" 0,1000 /work/tmp /work/tmp/p
expect "a non-numeric uid is refused" 2 'is not a number' "$repo/scripts/proxy-config.sh" root /work/tmp /work/tmp/p
expect "an empty uid in the list is refused" 2 'is not a number' "$repo/scripts/proxy-config.sh" 0,,1000 /work/tmp /work/tmp/p
expect "a relative bind root is refused" 2 'is not absolute' "$repo/scripts/proxy-config.sh" 0 work/tmp /work/tmp/p
cp -r "$repo/deploy" "$scratch/deploy-moved" && mkdir -p "$scratch/proxy-moved/scripts" && cp "$repo/scripts/proxy-config.sh" "$scratch/proxy-moved/scripts/" && cp -r "$scratch/deploy-moved" "$scratch/proxy-moved/deploy"
sed -i 's|^  workspace_root: /srv/menagerie/workspaces$|  workspace_root: /srv/menagerie/workspaces/|' "$scratch/proxy-moved/deploy/sockguard.yml"
expect "a profile whose workspace_root line moved is refused" 1 "workspace_root line moved" "$scratch/proxy-moved/scripts/proxy-config.sh" 0 /work/tmp /work/tmp/p
cp "$repo/deploy/sockguard.yml" "$scratch/proxy-moved/deploy/sockguard.yml"
sed -i 's|^\(  path: /health\)$|\1\n  extra: /srv/menagerie/other|' "$scratch/proxy-moved/deploy/sockguard.yml"
expect "a profile host path the renderer does not replace is refused" 1 'holds a host path this script does not replace' "$scratch/proxy-moved/scripts/proxy-config.sh" 0 /work/tmp /work/tmp/p

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
