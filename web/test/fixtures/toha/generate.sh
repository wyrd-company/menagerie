#!/usr/bin/env bash
# Regenerates the Toha interview documents under ./documents from a real Toha
# build, so the UI fixtures are producer output rather than hand-written JSON.
#
#   test/fixtures/toha/generate.sh <path-to-toha-binary> <empty-scratch-dir>
#
# Test tooling only: Menagerie application code never runs the Toha CLI. The
# scratch directory holds an isolated HOME/XDG tree, a git-versioned copy of
# the sample template (v1, then v2 adding one question) and the targets.
# Absolute scratch paths in each document's `context` are rewritten to generic
# values; nothing else in a document is changed.
set -euo pipefail

toha=$(realpath "$1")
scratch=$(realpath "$2")
here=$(cd "$(dirname "$0")" && pwd)
out="$here/documents"

if [ -n "$(ls -A "$scratch")" ]; then
  echo "scratch directory must be empty: $scratch" >&2
  exit 1
fi

export HOME="$scratch/home" XDG_CONFIG_HOME="$scratch/config" XDG_DATA_HOME="$scratch/data"
export XDG_CACHE_HOME="$scratch/cache" XDG_STATE_HOME="$scratch/state"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_CACHE_HOME" "$XDG_STATE_HOME" "$out"
rm -f "$out"/*.json "$out/PRODUCER"

g() { git -c user.name=sample -c user.email=sample@example.test -c init.defaultBranch=main "$@"; }

# The template repository: v1 is the fixture folder, v2 adds the `region` question.
repo="$scratch/sample-kit"
mkdir -p "$repo"
cp -r "$here/sample-kit-v1/." "$repo/"
g -C "$repo" init -q
g -C "$repo" add -A
g -C "$repo" commit -qm v1
g -C "$repo" tag v1
cp "$here/sample-kit-v2.template.yml" "$repo/template.yml"
g -C "$repo" commit -qam v2
g -C "$repo" tag v2
v1="file://$repo@v1"
v2="file://$repo@v2"

# A Toha result document is printed first, followed by instructions.
extract() { node -e '
  const text = require("fs").readFileSync(0, "utf8") + "\n";
  const end = text.indexOf("\n}\n");
  if (!text.startsWith("{") || end < 0) { console.error(text); process.exit(1); }
  process.stdout.write(JSON.stringify(JSON.parse(text.slice(0, end + 2)), null, 2) + "\n");
'; }

formal=""
envelope() { node -e '
  const [template, answers] = process.argv.slice(1);
  process.stdout.write(JSON.stringify({ template, answers: JSON.parse(answers) }));
' "$formal" "$1"; }

target="$scratch/sample-space"
mkdir -p "$target"
g -C "$target" init -q
g -C "$target" commit -q --allow-empty -m init

# Create: first batch, a rejected submission, the second batch, completion.
"$toha" stage --async "$scratch/stage-1.json" "$v1" "$target" >/dev/null || [ $? = 4 ]
extract <"$scratch/stage-1.json" >"$out/create-batch-1.json"
formal=$(node -p 'require(process.argv[1]).context.template' "$out/create-batch-1.json")

# Only Toha can judge these: a label pattern and the document's own schema.
envelope '{"title":"Sample project","labels":["Bad Label"],"settings":{"$schema":"https://schemas.example.test/sample-settings/1","count":2}}' >"$scratch/rejected.json"
"$toha" continue "$target" "$scratch/rejected.json" | extract >"$out/create-batch-1-rejected.json" || [ "${PIPESTATUS[0]}" = 4 ]

envelope '{"title":"Sample project","notes":"First line\nSecond line","remote":"https://example.test/sample.git","enabled":true,"verbose":false,"size":"small","palette":"green","features":["alpha","gamma"],"labels":["one","two"],"settings":{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample","count":2}}' >"$scratch/batch-1.json"
"$toha" continue "$target" "$scratch/batch-1.json" | extract >"$out/create-batch-2.json" || [ "${PIPESTATUS[0]}" = 4 ]

envelope '{}' >"$scratch/batch-2.json"
"$toha" continue "$target" "$scratch/batch-2.json" >/dev/null
"$toha" apply "$target" >/dev/null
g -C "$target" add -A
g -C "$target" commit -qm applied

# Upgrade to v2: only the new question is asked.
"$toha" stage --update --async "$scratch/stage-upgrade.json" "$v2" "$target" >/dev/null || [ $? = 4 ]
extract <"$scratch/stage-upgrade.json" >"$out/upgrade-batch-1.json"

# The CLI's staged update re-asks every question with the recorded answers as
# defaults. Menagerie's upgrade asks only the questions the recorded answers do
# not cover, so this document keeps only the properties that no create batch
# asked. Everything else in the document is the producer's output.
node -e '
  const fs = require("fs");
  const [dir] = process.argv.slice(1);
  const read = (n) => JSON.parse(fs.readFileSync(dir + "/" + n, "utf8"));
  const asked = new Set(["create-batch-1.json", "create-batch-2.json"].flatMap((n) => Object.keys(read(n).schema.properties)));
  const doc = read("upgrade-batch-1.json");
  for (const id of Object.keys(doc.schema.properties)) if (asked.has(id)) delete doc.schema.properties[id];
  doc.schema.required = doc.schema.required.filter((id) => !asked.has(id));
  fs.writeFileSync(dir + "/upgrade-batch-uncovered.json", JSON.stringify(doc, null, 2) + "\n");
' "$out"

# A flow stop ends a second create interview.
ended="$scratch/ended-space"
mkdir -p "$ended"
"$toha" stage --async "$scratch/stage-ended.json" "$v1" "$ended" >/dev/null || [ $? = 4 ]
envelope '{"title":"Sample project","enabled":false,"size":"halt","settings":{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample"}}' >"$scratch/halt.json"
"$toha" continue "$ended" "$scratch/halt.json" | extract >"$out/create-ended.json"

# Rewrite scratch paths in `context` to generic values.
node -e '
  const fs = require("fs"), path = require("path");
  const [dir, scratch] = process.argv.slice(1);
  for (const name of fs.readdirSync(dir).filter((n) => n.endsWith(".json"))) {
    const file = path.join(dir, name);
    const doc = JSON.parse(fs.readFileSync(file, "utf8"));
    if (doc.context) {
      doc.context.target = doc.context.target.replace(scratch, "/srv/workspaces");
      doc.context.template = doc.context.template.replace("file://" + scratch, "https://example.test/templates");
    }
    fs.writeFileSync(file, JSON.stringify(doc, null, 2) + "\n");
  }
' "$out" "$scratch"

printf "%s\nsource commit: %s\n" "$("$toha" --version)" "${TOHA_SOURCE_COMMIT:-unrecorded}" >"$out/PRODUCER"
ls "$out"
