#!/usr/bin/env bash
# Format or lint only the named files, for the pre-commit hook.
#
#   staged.sh format <file>...   apply each file's formatter to that file alone
#   staged.sh lint <file>...     check each file with its file-level linters
#
# Paths are relative to the repository root, as lefthook's {staged_files}
# gives them. A file with no formatter or linter is skipped. Package-wide
# checks (go vet, clippy, tsc) stay in `task lint`, which CI runs.
set -euo pipefail

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$repo"
[ $# -ge 1 ] || { echo "usage: $0 format|lint <file>..." >&2; exit 2; }
mode=$1
shift

go=() rust=() web=() webscript=() shell=() posix=() workflows=() yaml=() markdown=()
for f in "$@"; do
  [ -f "$f" ] || continue
  case "$f" in
    *.go) go+=("$f") ;;
    *.rs) rust+=("$f") ;;
  esac
  case "$f" in
    web/node_modules/* | web/dist/*) ;;
    web/*.ts | web/*.tsx | web/*.js | web/*.mjs | web/*.cjs)
      web+=("${f#web/}")
      webscript+=("${f#web/}")
      ;;
    web/*.json | web/*.css | web/*.md | web/*.yml | web/*.yaml | web/*.html) web+=("${f#web/}") ;;
  esac
  case "$f" in
    *.sh) shell+=("$f") ;;
    image/rootfs/etc/s6-overlay/scripts/* | image/rootfs/etc/s6-overlay/s6-rc.d/*/finish | image/rootfs/etc/s6-overlay/s6-rc.d/*/run | image/rootfs/usr/local/bin/*) posix+=("$f") ;;
  esac
  case "$f" in
    .github/workflows/*.yml) workflows+=("$f") ;;
  esac
  # The YAML `task lint` checks with ryl: the repository's own tooling.
  case "$f" in
    taskfile.yml | */taskfile.yml | deploy/*.yml | .github/*.yml | .config/*.yml | .ryl.yml) yaml+=("$f") ;;
  esac
  case "$f" in
    web/* | .intentional/intents/* | CHANGELOG.md) ;;
    *.md) markdown+=("$f") ;;
  esac
done

# rustfmt_file <file> <out>: the file formatted alone. rustfmt reads it from
# standard input, so it follows no `mod` into child module files; it runs in
# the file's folder so it finds the same rustfmt configuration.
# shellcheck disable=SC2094 # <file> and <out> are different files
rustfmt_file() { (cd "$(dirname "$1")" && rustfmt --edition 2024 --emit stdout) <"$1" >"$2"; }

case "$mode" in
  format)
    if [ ${#go[@]} -gt 0 ]; then gofmt -w "${go[@]}"; fi
    for f in ${rust[@]+"${rust[@]}"}; do
      out=$(mktemp)
      rustfmt_file "$f" "$out" || { rm -f "$out"; exit 1; }
      # Rewrite in place, so the file keeps its mode, only when it changed.
      cmp -s "$out" "$f" || cat "$out" >"$f"
      rm -f "$out"
    done
    if [ ${#web[@]} -gt 0 ]; then (cd web && pnpm exec prettier --write --ignore-unknown "${web[@]}"); fi
    ;;
  lint)
    if [ ${#go[@]} -gt 0 ]; then
      unformatted=$(gofmt -l "${go[@]}")
      [ -z "$unformatted" ] || { echo "not gofmt-formatted: $unformatted" >&2; exit 1; }
    fi
    for f in ${rust[@]+"${rust[@]}"}; do
      out=$(mktemp)
      rustfmt_file "$f" "$out" || { rm -f "$out"; exit 1; }
      cmp -s "$out" "$f" || { rm -f "$out"; echo "not rustfmt-formatted: $f" >&2; exit 1; }
      rm -f "$out"
    done
    if [ ${#web[@]} -gt 0 ]; then (cd web && pnpm exec prettier --check --ignore-unknown "${web[@]}"); fi
    if [ ${#webscript[@]} -gt 0 ]; then (cd web && pnpm exec eslint "${webscript[@]}"); fi
    if [ ${#shell[@]} -gt 0 ]; then shellcheck "${shell[@]}"; fi
    if [ ${#posix[@]} -gt 0 ]; then shellcheck -s sh "${posix[@]}"; fi
    if [ ${#workflows[@]} -gt 0 ]; then actionlint "${workflows[@]}"; fi
    if [ ${#yaml[@]} -gt 0 ]; then ryl check --config-file .ryl.yml "${yaml[@]}"; fi
    if [ ${#markdown[@]} -gt 0 ]; then rumdl check "${markdown[@]}"; fi
    ;;
  *) echo "usage: $0 format|lint <file>..." >&2; exit 2 ;;
esac
