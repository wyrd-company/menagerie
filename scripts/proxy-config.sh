#!/usr/bin/env bash
# Renders deploy/sockguard.yml, the shipped Docker proxy profile, for the
# live suites on the disposable daemon.
#
#   proxy-config.sh <uid> <bind-root> <workspace-root> [pending...]
#
# <uid> replaces the profile's Menagerie uid. Every allowed bind root
# becomes <bind-root>, the folder the suites' workspaces, socket folders
# and Compose staging live in, and workspace_exceptions.workspace_root
# becomes <workspace-root>. Nothing else of the profile changes, except a
# stand-in for each pending Sockguard fork change, named by its issue
# number, whose key the pinned proxy does not know yet:
#
#   1815  allow_any_command  -> the global insecure_allow_body_blind_writes,
#         which lifts the same exec argv gate
#   1819  label filters kept under ownership -> no ownership block, so
#         devcontainer-builder's label lookups select the right containers
#
# Pending 1571 and 1814 have no stand-in: the pinned proxy ignores their
# keys, and the suites that need them wait.
set -euo pipefail

[ $# -ge 3 ] || {
  sed -n '2,8p' "$0" >&2
  exit 2
}
uid=$1 bind_root=$2 workspace_root=$3
shift 3
profile=$(dirname "$0")/../deploy/sockguard.yml

case "$uid" in '' | *[!0-9]*) echo "uid $uid is not a number" >&2; exit 2 ;; esac
for path in "$bind_root" "$workspace_root"; do
  case "$path" in /*) ;; *) echo "$path is not absolute" >&2; exit 2 ;; esac
  case "$path" in *[!A-Za-z0-9/._-]*) echo "$path holds a character the profile cannot take" >&2; exit 2 ;; esac
done

script=(
  -e "s|^\(      uids: \)\[1000\]$|\1[$uid]|"
  -e "s|^\(  workspace_root: \)/srv/menagerie/workspaces$|\1$workspace_root|"
  -e "s|^\(            - \)/srv/menagerie/[a-z-]*$|\1$bind_root|"
)
prelude=""
for issue in "$@"; do
  case "$issue" in
    1815)
      script+=(-e '/^          allow_any_command: true$/d')
      prelude="insecure_allow_body_blind_writes: true"
      ;;
    1819) script+=(-e '/^ownership:$/,/^$/d') ;;
    1571 | 1814 | verbosity | missing-roots) ;;
    *) echo "no stand-in is known for pending issue $issue" >&2; exit 2 ;;
  esac
done

rendered=$(sed "${script[@]}" "$profile")
# Each replacement must have happened: the profile's shape is what the
# suites assert.
grep -q "^      uids: \[$uid\]$" <<<"$rendered" || { echo "the profile's uid line moved" >&2; exit 1; }
grep -q "^  workspace_root: $workspace_root$" <<<"$rendered" || { echo "the profile's workspace_root line moved" >&2; exit 1; }
if grep -q "/srv/menagerie/" <<<"$rendered"; then
  echo "the profile holds a host path this script does not replace" >&2
  exit 1
fi
[ -z "$prelude" ] || echo "$prelude"
echo "$rendered"
