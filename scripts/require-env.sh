#!/usr/bin/env sh
# Fail before any work unless every named variable is non-empty. Names only
# are printed, never values.
set -eu
missing=
for name in "$@"; do
  case "$name" in
    "" | [0-9]* | *[!A-Za-z0-9_]*) echo "not a variable name: $name" >&2; exit 2 ;;
  esac
  eval "value=\${$name:-}"
  [ -n "$value" ] || missing="$missing $name"
done
if [ -n "$missing" ]; then
  echo "::error::Not configured for this workflow:$missing"
  exit 1
fi
