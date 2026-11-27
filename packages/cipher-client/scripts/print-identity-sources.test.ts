// packages/cipher-client/scripts/print-identity-sources.test.ts
//
// print-identity-sources.ts as the broker's TestClientSourcesAgree runs it: one JSON line holding
// this client's registry, each entry its name and the broker provider whose tokens it makes.

import { expect, test } from "bun:test";
import { join } from "node:path";
import { IDENTITY_SOURCES } from "../src/environment";

const SCRIPT = join(import.meta.dir, "print-identity-sources.ts");

test("print-identity-sources prints the registry", async () => {
  const run = Bun.spawn(["bun", SCRIPT], { stdout: "pipe", stderr: "pipe" });
  const [out, error, status] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
  expect({ status, error }).toEqual({ status: 0, error: "" });
  expect(out.endsWith("\n")).toBe(true);
  expect(JSON.parse(out)).toEqual(
    IDENTITY_SOURCES.map(({ name, provider }) => ({ name, provider }))
  );
  // An empty registry would make this pass against itself, so the printed list is held to the
  // names the client takes as well.
  expect(JSON.parse(out).map((source: { name: string }) => source.name)).toEqual([
    "kubernetes",
    "metadata",
    "pool",
    "github-actions",
    "device-login",
  ]);
});
