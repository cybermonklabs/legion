import { beforeAll, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// `e2e/run-server.sh` passes the server the root of The Spec from `DISPATCH_E2E_SPEC_ROOTS`, read
// before the script removes every inherited `DISPATCH_*` variable, as it reads
// `DISPATCH_E2E_SERVER_BIN`. A stand-in server binary prints what it got.
const runServer = fileURLToPath(new URL("../run-server.sh", import.meta.url));
const directory = temporaryDirectory("run-server-spec-roots");
const binary = join(directory, "envoy-dispatch");
beforeAll(() => {
  writeFileSync(
    binary,
    '#!/usr/bin/env bash\necho "DISPATCH_SPEC_ROOTS=$(printenv DISPATCH_SPEC_ROOTS || echo unset)"\n',
    { mode: 0o755 }
  );
});

async function specRootsGiven(env: Record<string, string>): Promise<string> {
  const child = Bun.spawn(["bash", runServer], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: directory,
      TMPDIR: tmpdir(),
      DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
      DISPATCH_E2E_PORT: "8777",
      DISPATCH_E2E_SERVER_BIN: binary,
      FAKE_BROKER_PORT: "9024",
      FAKE_ENVOY_PORT: "9021",
      FAKE_GITHUB_PORT: "9022",
      ...env,
    },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  expect(code, stderr).toBe(0);
  return stdout;
}

test("run-server.sh names STE-1 as the root of The Spec unless DISPATCH_E2E_SPEC_ROOTS names others, and an empty value names none", async () => {
  expect(await specRootsGiven({})).toBe("DISPATCH_SPEC_ROOTS=STE-1\n");
  expect(await specRootsGiven({ DISPATCH_E2E_SPEC_ROOTS: "ACME-1,ACME-7" })).toBe(
    "DISPATCH_SPEC_ROOTS=ACME-1,ACME-7\n"
  );
  expect(await specRootsGiven({ DISPATCH_E2E_SPEC_ROOTS: "" })).toBe("DISPATCH_SPEC_ROOTS=\n");
  // The server's own setting in the caller's shell does not reach it: the sweep removes it.
  expect(await specRootsGiven({ DISPATCH_SPEC_ROOTS: "ACME-9" })).toBe(
    "DISPATCH_SPEC_ROOTS=STE-1\n"
  );
});
