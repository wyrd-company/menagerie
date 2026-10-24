#!/usr/bin/env bash
# Apply .github/repo-settings.yml to REPO with the token in GH_TOKEN. Run only
# by the reconcile-settings workflow.
set -euo pipefail
settings=.github/repo-settings.yml
fail() {
  echo "::error file=$settings::$1"
  exit 1
}
[ -f "$settings" ] || fail "required file is missing"
# Visibility is never reconciled: publishing or hiding a repository stays a
# deliberate manual act.
[ "$(yq -r 'has("visibility")' "$settings")" = false ] || fail "visibility is not reconciled; change it by hand"
read_field() { yq -r "$1" "$settings"; }
args=(
  -f "description=$(read_field '.description')"
  -F "allow_squash_merge=$(read_field '.merge.squash')"
  -F "allow_rebase_merge=$(read_field '.merge.rebase')"
  -F "allow_merge_commit=$(read_field '.merge.merge_commit')"
  -F "delete_branch_on_merge=$(read_field '.merge.delete_branch_on_merge')"
  -F "has_wiki=$(read_field '.features.wiki')"
  -F "has_projects=$(read_field '.features.projects')"
  -F "has_discussions=$(read_field '.features.discussions')"
)
# The API rejects an unknown field, so a typo fails here.
gh api -X PATCH "repos/${REPO:?}" "${args[@]}" --silent
# Topics replace wholesale, so a removed topic is removed.
yq -o=json '{"names": .topics}' "$settings" | gh api -X PUT "repos/$REPO/topics" --input - --silent
echo "applied $settings to $REPO"
