// Check docs/docs.yml against the pages wyrd.foo publishes from docs/: the
// descriptor has the fields the publish action requires, every listed asset
// is a file inside docs/, and every image a published page (front matter
// `docs: true`) shows is a listed asset. The action copies only listed
// assets, so an unlisted image would be missing on the site.
// Usage: node scripts/check-docs.mjs [docs folder]; the default is docs.
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join, normalize } from "node:path";

const dir = process.argv[2] ?? "docs";
const failures = [];
const manifest = JSON.parse(execFileSync("yq", ["-o=json", join(dir, "docs.yml")], { encoding: "utf8" }));
if (!/^[a-z0-9][a-z0-9-]*$/.test(manifest.name ?? "")) failures.push("docs.yml name is not a URL-safe slug");
for (const field of ["tagline", "category"]) if (!manifest[field]) failures.push(`docs.yml has no ${field}`);
if (!["stable", "preview", "planned"].includes(manifest.status)) failures.push(`docs.yml status ${manifest.status}`);
const assets = manifest.assets ?? [];
for (const asset of assets) {
  const path = normalize(join(dir, asset));
  if (asset.startsWith("/") || !path.startsWith(normalize(dir) + "/")) failures.push(`asset ${asset} is outside ${dir}`);
  else if (!existsSync(path) || !statSync(path).isFile()) failures.push(`asset ${asset} is not a file`);
}
let pages = 0;
for (const file of readdirSync(dir).filter((f) => f.endsWith(".md"))) {
  const text = readFileSync(join(dir, file), "utf8");
  const front = text.match(/^---\n([\s\S]*?)\n---\n/);
  if (!front || !/^docs:\s*true\s*$/m.test(front[1])) continue;
  pages++;
  for (const m of text.matchAll(/!\[[^\]]*\]\(([^)\s]+)\)/g)) {
    const target = m[1];
    if (/^[a-z]+:/.test(target)) continue;
    if (!assets.includes(normalize(target))) failures.push(`${file} shows ${target}, which docs.yml does not list`);
  }
}
if (pages === 0) failures.push(`no page in ${dir} is marked docs: true`);
if (failures.length > 0) {
  console.error(failures.join("\n"));
  process.exit(1);
}
console.log(`checked ${pages} pages and ${assets.length} assets`);
