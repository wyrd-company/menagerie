// @vitest-environment node
//
// Holds the backups screen's picker to the Git URL forms the service reads
// (docs/specifications/assets/workspace-api/credential-url-cases.json): the
// kind of credential each URL takes, and whether an App on the table's
// server serves it. The service's gitcreds tests read the same table, so a
// rule that drifts on either side fails here or there.
import { readFileSync } from "node:fs";
import { expect, it } from "vitest";

import { servingApps, urlKind } from "../../src/ui/credentialUrls.ts";

const table = JSON.parse(
  readFileSync(
    new URL(
      "../../../docs/specifications/assets/workspace-api/credential-url-cases.json",
      import.meta.url,
    ),
    "utf8",
  ),
) as { server: string; cases: { url: string; kind: "ssh" | "https" | null; served: boolean }[] };

it.each(table.cases)("URL table: reads $url as the service does", ({ url, kind, served }) => {
  expect(urlKind(url)).toBe(kind);
  expect(servingApps(url, [{ name: "sample-org", server: table.server }]).length > 0).toBe(served);
});
