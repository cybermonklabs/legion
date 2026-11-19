import { describe, expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

import type { HarnessListener } from "../harness-listeners";

const module = (name: string) => fileURLToPath(new URL(`../${name}`, import.meta.url));
const runServer = module("run-server.sh");
const runAccessIssuer = module("run-access-issuer.sh");

async function run(argv: string[], env: Record<string, string>) {
  const child = Bun.spawn(argv, {
    // TMPDIR: the child writes under its temporary directory, which on a machine whose TMPDIR
    // names the only writable volume is not /tmp.
    env: { PATH: process.env.PATH ?? "", TMPDIR: tmpdir(), ...env },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stderr, stdout };
}

/** The listener table as an importer in `env` builds it: the table is built at evaluation, from
 *  the environment, so each environment needs a process of its own. */
async function listenersIn(env: Record<string, string>): Promise<HarnessListener[]> {
  const script =
    `const { harnessListeners } = await import(${JSON.stringify(module("harness-listeners.ts"))});` +
    "console.log(JSON.stringify(harnessListeners));";
  const { code, stderr, stdout } = await run(["bun", "-e", script], env);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  return JSON.parse(stdout) as HarnessListener[];
}

const ids = (listeners: HarnessListener[]) => listeners.map((listener) => listener.id);
const on = { DISPATCH_E2E_ACCESS_ISSUER: "1" };

describe("DISPATCH_E2E_ACCESS_ISSUER", () => {
  test("1 starts the access issuer before the server, keeping its token in the exported file", async () => {
    const listeners = await listenersIn(on);
    const issuer = listeners.find((listener) => listener.id === "access-issuer");
    expect(issuer).toMatchObject({
      argv: ["bash", runAccessIssuer],
      deployed: false,
      fileVariable: "DISPATCH_E2E_ACCESS_TOKEN_FILE",
      key: "accessIssuer",
    });
    expect(ids(listeners).indexOf("access-issuer")).toBeLessThan(
      ids(listeners).indexOf("dispatch")
    );
    // Two-process mode: before the proxy, so both servers start with the issuer's port.
    const two = ids(await listenersIn({ ...on, DISPATCH_E2E_PROCESSES: "2" }));
    expect(two.indexOf("access-issuer")).toBeGreaterThan(-1);
    expect(two.indexOf("access-issuer")).toBeLessThan(two.indexOf("round-robin-proxy"));
  });

  test("unset, any other value and a deployed run start no access issuer and keep no file", async () => {
    for (const env of [
      {},
      { DISPATCH_E2E_ACCESS_ISSUER: "" },
      { DISPATCH_E2E_ACCESS_ISSUER: "true" },
      { ...on, PLAYWRIGHT_BASE_URL: "http://127.0.0.1:1" },
    ] as Record<string, string>[]) {
      const listeners = await listenersIn(env);
      expect(ids(listeners)).not.toContain("access-issuer");
      expect(listeners.filter((listener) => listener.fileVariable !== undefined)).toEqual([]);
    }
  });

  test("run-server.sh refuses the switch without the issuer's port", async () => {
    const { code, stderr } = await run(["bash", runServer], {
      ...on,
      DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
      DISPATCH_E2E_PORT: "9041",
      // Were the refusal missing, the script would exec this instead of building and serving.
      DISPATCH_E2E_SERVER_BIN: "/bin/false",
      FAKE_BROKER_PORT: "9042",
      FAKE_ENVOY_PORT: "9043",
      FAKE_GITHUB_PORT: "9044",
    });
    expect(code).not.toBe(0);
    expect(stderr).toContain(
      "DISPATCH_E2E_ACCESS_ISSUER_PORT must name the dev OIDC issuer (e2e/run-access-issuer.sh)"
    );
  });

  test("run-access-issuer.sh refuses a port it cannot bind, and no token file", async () => {
    const file = { DISPATCH_E2E_ACCESS_TOKEN_FILE: `${tmpdir()}/never-written` };
    const ported = await run(["bash", runAccessIssuer], {
      ...file,
      DISPATCH_E2E_ACCESS_ISSUER_PORT: "9045",
    });
    expect(ported.code).toBe(2);
    expect(ported.stderr).toContain("unset DISPATCH_E2E_ACCESS_ISSUER_PORT");
    const fileless = await run(["bash", runAccessIssuer], {});
    expect(fileless.code).not.toBe(0);
    expect(fileless.stderr).toContain(
      "DISPATCH_E2E_ACCESS_TOKEN_FILE must name the file the dev OIDC issuer keeps"
    );
  });

  test("the issuer's port joins the collision check under the switch only", async () => {
    const resolve = [
      "bun",
      "-e",
      `(await import(${JSON.stringify(module("harness-ports.ts"))})).harnessPorts()`,
    ];
    const clash = { DISPATCH_E2E_ACCESS_ISSUER_PORT: "9046", FAKE_ENVOY_PORT: "9046" };
    const switched = await run(resolve, { ...clash, ...on });
    expect(switched.code).not.toBe(0);
    expect(switched.stderr).toContain(
      "DISPATCH_E2E_ACCESS_ISSUER_PORT and FAKE_ENVOY_PORT name port 9046"
    );
    expect((await run(resolve, clash)).code).toBe(0);
  });
});
