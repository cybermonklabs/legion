import { describe, expect, test } from "bun:test";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import type { HarnessListener } from "../harness-listeners";
import { rowLimit, runBounded } from "./bounded-subprocess";

const module = (name: string) => fileURLToPath(new URL(`../${name}`, import.meta.url));
const runServer = module("run-server.sh");
const runSignInIssuer = module("run-signin-issuer.sh");

function run(argv: string[], env: Record<string, string>) {
  return runBounded({
    argv,
    bound: "short",
    // TMPDIR: the child writes under its temporary directory, which on a machine whose TMPDIR
    // names the only writable volume is not /tmp.
    env: { PATH: process.env.PATH ?? "", TMPDIR: tmpdir(), ...env },
    from: import.meta.url,
  });
}

// A process of its own per import: the refusal is the module's evaluation, which runs once.
const importPorts = (env: Record<string, string>) =>
  run(["bun", "-e", `await import(${JSON.stringify(module("harness-ports.ts"))})`], env);

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

const twoProcesses = { DISPATCH_E2E_PROCESSES: "2" };
const oidc = { ...twoProcesses, DISPATCH_E2E_SIGNIN: "oidc" };

describe("DISPATCH_E2E_SIGNIN", () => {
  // Each value is one import of its own, so the row's limit is read from the two lists.
  const refusedSignins = ["dev", "OIDC", " oidc", "1"];
  const acceptedSignins: Record<string, string>[] = [{}, { DISPATCH_E2E_SIGNIN: "" }, oidc];
  test(
    "every harness importer refuses a value other than oidc or unset, naming those",
    async () => {
      for (const value of refusedSignins) {
        const { code, stderr } = await importPorts({ ...twoProcesses, DISPATCH_E2E_SIGNIN: value });
        expect(code).not.toBe(0);
        expect(stderr).toContain(
          `DISPATCH_E2E_SIGNIN must be "oidc" or unset, not ${JSON.stringify(value)}`
        );
      }
      for (const env of acceptedSignins) {
        expect((await importPorts(env)).code).toBe(0);
      }
    },
    rowLimit(...[...refusedSignins, ...acceptedSignins].map(() => "short" as const))
  );

  // Each value is one import of its own, so the row's limit is read from the list.
  const fewerProcesses = ["", "1"];
  test(
    "oidc is refused without two processes",
    async () => {
      for (const processes of fewerProcesses) {
        const { code, stderr } = await importPorts({
          DISPATCH_E2E_PROCESSES: processes,
          DISPATCH_E2E_SIGNIN: "oidc",
        });
        expect(code).not.toBe(0);
        expect(stderr).toContain("DISPATCH_E2E_SIGNIN=oidc needs DISPATCH_E2E_PROCESSES=2");
      }
    },
    rowLimit(...fewerProcesses.map(() => "short" as const))
  );

  test(
    "run-server.sh refuses an unknown value, and oidc without its inputs",
    async () => {
      const base = {
        ...twoProcesses,
        DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
        DISPATCH_E2E_PORT: "9031",
        FAKE_BROKER_PORT: "9032",
        FAKE_ENVOY_PORT: "9033",
        FAKE_GITHUB_PORT: "9034",
      };
      const unknown = await run(["bash", runServer], { ...base, DISPATCH_E2E_SIGNIN: "dev" });
      expect(unknown.code).toBe(1);
      expect(unknown.stderr).toContain("DISPATCH_E2E_SIGNIN must be oidc or unset, not 'dev'");
      // Under oidc the dev sign-in key is not asked for; the issuer is.
      const issuerless = await run(["bash", runServer], { ...base, DISPATCH_E2E_SIGNIN: "oidc" });
      expect(issuerless.code).not.toBe(0);
      expect(issuerless.stderr).not.toContain("DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY");
      expect(issuerless.stderr).toContain(
        "DISPATCH_E2E_SIGNIN_ISSUER_PORT must name the dev OIDC issuer"
      );
    },
    rowLimit("short", "short")
  );

  test(
    "run-signin-issuer.sh refuses a port it cannot bind, and a missing input",
    async () => {
      const inputs = {
        DISPATCH_E2E_SIGNIN_AS: "alice@example.com",
        DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
        DISPATCH_E2E_SIGNIN_CLIENT_SECRET: "secret",
        DISPATCH_E2E_SIGNIN_GROUP: "people",
      };
      const ported = await run(["bash", runSignInIssuer], {
        ...inputs,
        DISPATCH_E2E_SIGNIN_ISSUER_PORT: "9035",
      });
      expect(ported.code).toBe(2);
      expect(ported.stderr).toContain("unset DISPATCH_E2E_SIGNIN_ISSUER_PORT");
      const { DISPATCH_E2E_SIGNIN_CLIENT_SECRET: _, ...secretless } = inputs;
      const missing = await run(["bash", runSignInIssuer], secretless);
      expect(missing.code).not.toBe(0);
      expect(missing.stderr).toContain(
        "DISPATCH_E2E_SIGNIN_CLIENT_SECRET must give the secret of that client"
      );
    },
    rowLimit("short", "short")
  );

  test(
    "run-signin-issuer.sh execs a handed binary without a build, and refuses one it cannot run",
    async () => {
      const inputs = {
        DISPATCH_E2E_SIGNIN_AS: "alice@example.com",
        DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
        DISPATCH_E2E_SIGNIN_CLIENT_SECRET: "secret",
        DISPATCH_E2E_SIGNIN_GROUP: "people",
      };
      const dir = await mkdtemp(join(tmpdir(), "signin-issuer-bin-"));
      try {
        // A stand-in for the binaries job's dev-oidc-issuer: it prints the arguments it was
        // exec'd with, which is all this row needs to see that the script ran it and built nothing.
        const handed = join(dir, "dev-oidc-issuer");
        await writeFile(handed, '#!/usr/bin/env bash\nprintf "handed %s\\n" "$*"\n');
        await chmod(handed, 0o755);
        const ran = await run(["bash", runSignInIssuer], {
          ...inputs,
          DISPATCH_E2E_SIGNIN_ISSUER_BIN: handed,
        });
        expect(ran.stderr).toBe("");
        expect(ran.code).toBe(0);
        // The client's secret file, then its one callback (main's #5185): the servers'
        // /auth/callback on the loopback dashboard origin.
        expect(ran.stdout).toMatch(
          /^handed --code-flow dispatch-e2e=\/dev\/fd\/\d+=http:\/\/127\.0\.0\.1\/auth\/callback --sign-in-as alice@example\.com --sign-in-group people\n$/
        );

        const plain = join(dir, "not-executable");
        await writeFile(plain, "");
        for (const refused of [plain, dir, join(dir, "missing")]) {
          const result = await run(["bash", runSignInIssuer], {
            ...inputs,
            DISPATCH_E2E_SIGNIN_ISSUER_BIN: refused,
          });
          expect(result.code).toBe(2);
          expect(result.stderr).toContain(
            `DISPATCH_E2E_SIGNIN_ISSUER_BIN=${refused} is not an executable file`
          );
        }
      } finally {
        await rm(dir, { force: true, recursive: true });
      }
    },
    // One bounded run of the handed binary, then one for each of the three paths it refuses.
    rowLimit("short", "short", "short", "short")
  );

  test(
    "oidc starts the issuer before both servers and hands all three one client secret",
    async () => {
      const listeners = await listenersIn(oidc);
      const ids = listeners.map((listener) => listener.id);
      const issuer = listeners.find((listener) => listener.id === "signin-issuer");
      const servers = listeners.filter(
        (listener) => listener.key.startsWith("dispatch") && listener.env
      );
      expect(ids.indexOf("signin-issuer")).toBeGreaterThan(-1);
      expect(ids.indexOf("signin-issuer")).toBeLessThan(ids.indexOf("round-robin-proxy"));
      expect(servers.map((server) => server.id)).toEqual(["dispatch-a", "dispatch-b"]);
      const issuerEnv = issuer?.env ?? {};
      const client = (env: Readonly<Record<string, string>>) => ({
        group: env.DISPATCH_E2E_SIGNIN_GROUP,
        id: env.DISPATCH_E2E_SIGNIN_CLIENT_ID,
        secret: env.DISPATCH_E2E_SIGNIN_CLIENT_SECRET,
      });
      expect(client(issuerEnv)).toEqual({
        group: "dispatch-e2e-people",
        id: "dispatch-e2e",
        secret: expect.stringMatching(/^[A-Za-z0-9_-]{43}$/),
      });
      const [a = {}, b = {}] = servers.map((server) => server.env ?? {});
      for (const env of [a, b]) {
        expect(client(env)).toEqual(client(issuerEnv));
        expect(env).not.toHaveProperty("DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY");
      }
      expect(a.DISPATCH_E2E_SIGNING_KEY).toMatch(/^[A-Za-z0-9_-]{43}$/);
      expect(b.DISPATCH_E2E_SIGNING_KEY).toEqual(a.DISPATCH_E2E_SIGNING_KEY);
      expect(a.DISPATCH_E2E_SIGNING_KEY).not.toEqual(issuerEnv.DISPATCH_E2E_SIGNIN_CLIENT_SECRET);
    },
    rowLimit("short")
  );

  test(
    "dev sign-in starts no issuer and hands both servers one dev sign-in key",
    async () => {
      const listeners = await listenersIn(twoProcesses);
      expect(listeners.map((listener) => listener.id)).not.toContain("signin-issuer");
      const keys = listeners
        .filter((listener) => listener.env !== undefined)
        .map((listener) => listener.env ?? {});
      expect(keys).toHaveLength(2);
      for (const env of keys) {
        expect(env.DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY).toBe(
          keys[0]?.DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY
        );
        expect(env).not.toHaveProperty("DISPATCH_E2E_SIGNING_KEY");
        expect(env).not.toHaveProperty("DISPATCH_E2E_SIGNIN_CLIENT_SECRET");
      }
    },
    rowLimit("short")
  );

  test(
    "the issuer's port joins the collision check under oidc only",
    async () => {
      const resolve = [
        "bun",
        "-e",
        `(await import(${JSON.stringify(module("harness-ports.ts"))})).harnessPorts()`,
      ];
      const clash = { DISPATCH_E2E_SIGNIN_ISSUER_PORT: "9036", FAKE_ENVOY_PORT: "9036" };
      const underOidc = await run(resolve, { ...clash, ...oidc });
      expect(underOidc.code).not.toBe(0);
      expect(underOidc.stderr).toContain(
        "FAKE_ENVOY_PORT and DISPATCH_E2E_SIGNIN_ISSUER_PORT name port 9036"
      );
      expect((await run(resolve, { ...clash, ...twoProcesses })).code).toBe(0);
    },
    rowLimit("short", "short")
  );
});

describe("DISPATCH_E2E_BINARY_B", () => {
  // The first server to start migrates the empty database (e2e/harness-listeners.ts). An earlier
  // build given as B starts first, as production ran it before this tree; with none, A starts first.
  test(
    "an earlier build given as B starts before A, this tree, and A starts first without one",
    async () => {
      const cases: [Record<string, string>, string[]][] = [
        [
          { ...twoProcesses, DISPATCH_E2E_BINARY_B: "previous-build/envoy-dispatch" },
          ["dispatch-b", "dispatch-a"],
        ],
        [twoProcesses, ["dispatch-a", "dispatch-b"]],
      ];
      for (const [env, order] of cases) {
        const ids = (await listenersIn(env)).map((listener) => listener.id);
        expect(ids.filter((id) => id.startsWith("dispatch-"))).toEqual(order);
      }
    },
    rowLimit("short", "short")
  );
});
