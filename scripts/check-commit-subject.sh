#!/usr/bin/env sh
# Check that a commit subject is a conventional commit. The argument is the
# subject itself (a pull request title in CI) or the path of a commit message
# file (lefthook's commit-msg hook), whose first line is the subject.
set -eu

if [ -f "${1-}" ]; then
  subject=$(head -n 1 "$1")
else
  subject=$(printf '%s\n' "${1-}" | head -n 1)
fi
pattern='^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([a-z0-9._/-]+\))?!?: .+'

if printf '%s\n' "$subject" | grep -Eq "$pattern"; then
  exit 0
fi
echo "not a conventional commit subject: $subject" >&2
echo "expected <type>[(scope)][!]: <description>" >&2
exit 1
