// @vitest-environment node
//
// Holds the backups screen's picker to the Git URL forms the service reads
// (docs/specifications/assets/workspace-api/credential-url-cases.json): the
// kind of credential each URL takes, and whether an App on the case's
// server, or else the table's, serves it. A URL the service refuses
// outright the picker may read more loosely: it is never to be stricter
// than the service, whose refusal on save explains the URL. The service's gitcreds tests read the same table, so a
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
) as {
  server: string;
  cases: {
    url: string;
    server?: string;
    refused?: boolean;
    kind: "ssh" | "https" | null;
    served: boolean;
  }[];
};

it.each(table.cases.filter((c) => !c.refused))(
  "URL table: reads $url as the service does",
  ({ url, server, kind, served }) => {
    expect(urlKind(url)).toBe(kind);
    const apps = [{ name: "sample-org", server: server ?? table.server }];
    expect(servingApps(url, apps).length > 0).toBe(served);
  },
);
