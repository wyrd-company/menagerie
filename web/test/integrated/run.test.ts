// @vitest-environment node
//
// run.sh down stops only the run whose state is in the scratch folder it is
// given. These cases run the real script against two stand-in control
// endpoints on different ports and record every request each receives.
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

const script = new URL("./run.sh", import.meta.url).pathname;

interface Endpoint {
  url: string;
  port: number;
  requests: string[];
  server: Server;
}

const endpoints: Endpoint[] = [];
const scratches: string[] = [];

/** A stand-in control endpoint that serves `root` as its identity. */
async function endpoint(root: string): Promise<Endpoint> {
  const requests: string[] = [];
  const server = createServer((req, res) => {
    requests.push(`${req.method} ${req.url}`);
    res.setHeader("Content-Type", "application/json");
    res.end(req.url === "/identity" ? JSON.stringify({ root }) : '{"ok":true}');
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  const e = { url: `http://127.0.0.1:${port}`, port, requests, server };
  endpoints.push(e);
  return e;
}

/** A scratch folder holding a run's saved state. */
function scratch(info?: object): string {
  const dir = mkdtempSync(join(tmpdir(), "run-down-"));
  scratches.push(dir);
  mkdirSync(join(dir, "logs"));
  if (info) writeFileSync(join(dir, "info.json"), JSON.stringify(info));
  return dir;
}

/** Runs `run.sh down <dir>`; the environment names the other endpoint's ports. */
function down(dir: string, overrides: Record<string, string | undefined> = {}) {
  const env: Record<string, string> = {};
  for (const [k, v] of Object.entries({ ...process.env, ...overrides }))
    if (v !== undefined) env[k] = v;
  return new Promise<{ code: number | null; stderr: string }>((resolve) => {
    const child = spawn("bash", [script, "down", dir], { env });
    let stderr = "";
    child.stderr.on("data", (d) => (stderr += d));
    child.on("close", (code) => resolve({ code, stderr }));
  });
}

afterEach(() => {
  for (const e of endpoints.splice(0)) e.server.close();
  for (const d of scratches.splice(0)) rmSync(d, { recursive: true, force: true });
});

describe("run.sh down", () => {
  it("shuts down the saved control endpoint, not the one the environment names", async () => {
    const own = scratch();
    const sibling = await endpoint("/elsewhere/sibling/root/workspaces");
    const mine = await endpoint(join(own, "root/workspaces"));
    writeFileSync(
      join(own, "info.json"),
      JSON.stringify({ control: mine.url, root: join(own, "root/workspaces") }),
    );
    const result = await down(own, { MENAGERIE_BROWSER_CONTROL_PORT: String(sibling.port) });
    expect(result.code).toBe(0);
    expect(mine.requests).toEqual(["GET /identity", "POST /shutdown"]);
    expect(sibling.requests).toEqual([]);
  });

  it("uses the saved endpoint with no port in the environment", async () => {
    const own = scratch();
    const mine = await endpoint(join(own, "root/workspaces"));
    writeFileSync(
      join(own, "info.json"),
      JSON.stringify({ control: mine.url, root: join(own, "root/workspaces") }),
    );
    const unset = { MENAGERIE_BROWSER_CONTROL_PORT: undefined, MENAGERIE_BROWSER_PORT: undefined };
    expect((await down(own, unset)).code).toBe(0);
    expect(mine.requests).toEqual(["GET /identity", "POST /shutdown"]);
  });

  it("does not shut down an endpoint that serves another run", async () => {
    const own = scratch();
    const sibling = await endpoint("/elsewhere/sibling/root/workspaces");
    // Saved state that now points at a port another run took over.
    writeFileSync(
      join(own, "info.json"),
      JSON.stringify({ control: sibling.url, root: join(own, "root/workspaces") }),
    );
    const result = await down(own);
    expect(result.code).toBe(0);
    expect(result.stderr).toContain("does not serve this run");
    expect(sibling.requests).toEqual(["GET /identity"]);
  });

  it("sends nothing anywhere when no run state was saved", async () => {
    const sibling = await endpoint("/elsewhere/sibling/root/workspaces");
    const result = await down(scratch(), { MENAGERIE_BROWSER_CONTROL_PORT: String(sibling.port) });
    expect(result.code).toBe(0);
    expect(sibling.requests).toEqual([]);
  });

  it("stops with an error, without waiting, when its fixture was not shut down", async () => {
    const own = scratch({ control: "http://127.0.0.1:9", root: "/never/served" });
    const process_ = spawn("sleep", ["300"]);
    try {
      writeFileSync(join(own, "fixture.pid"), String(process_.pid));
      const result = await down(own);
      expect(result.code).toBe(1);
      expect(result.stderr).toContain(`fixture process ${process_.pid} is still running`);
      expect(spawnSync("kill", ["-0", String(process_.pid)]).status).toBe(0);
    } finally {
      process_.kill();
    }
  });
});
