// Starts the fixture server: node test/fixture-server/main.ts [port]
import { fileURLToPath } from "node:url";
import { createFixture } from "./server.ts";

const port = Number(process.argv[2] ?? process.env.FIXTURE_PORT ?? 4318);
const distDir = fileURLToPath(new URL("../../dist/", import.meta.url));
const fixture = createFixture({ distDir });
const url = await fixture.listen(port);
console.log(`fixture server on ${url}`);
