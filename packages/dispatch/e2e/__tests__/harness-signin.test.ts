import { afterAll, describe, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
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

// `env` itself as the server run-server.sh executes (DISPATCH_E2E_SERVER_BIN): run with no
// argument it prints the environment it was given, which is what run-server.sh hands
// envoy-dispatch, read without building or starting a server.
const printEnvironment = Bun.which("env") ?? "/usr/bin/env";

/** The environment run-server.sh, run in `env`, hands the server. Each variable starts a line of
 *  `env`'s output; the PEM key's own lines name none of the variables read here. */
async function serverEnvironment(env: Record<string, string>): Promise<Record<string, string>> {
  const { code, stderr, stdout } = await run(["bash", runServer], {
    ...env,
    DISPATCH_E2E_SERVER_BIN: printEnvironment,
  });
  if (code !== 0) throw new Error(`run-server.sh exited ${code}: ${stderr}`);
  return Object.fromEntries(
    stdout.split("\n").flatMap((line) => {
      const match = /^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/.exec(line);
      return match === null ? [] : [[match[1], match[2]]];
    })
  );
}

const twoProcesses = { DISPATCH_E2E_PROCESSES: "2" };
const oidc = { ...twoProcesses, DISPATCH_E2E_SIGNIN: "oidc" };
const google = { DISPATCH_E2E_SIGNIN: "google" };

/** What every run-server.sh run here is handed besides its sign-in: a database it never opens,
 *  since the server it executes is `env`, and the fakes' ports. */
const serverInputs = {
  DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
  FAKE_BROKER_PORT: "9032",
  FAKE_ENVOY_PORT: "9033",
  FAKE_GITHUB_PORT: "9034",
};

/** What the harness hands each server under google (e2e/harness-listeners.ts). */
const googleServerInputs = {
  ...serverInputs,
  ...google,
  DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
  DISPATCH_E2E_SIGNIN_CLIENT_SECRET: "secret",
  DISPATCH_E2E_SIGNIN_DOMAIN: "example.com",
  DISPATCH_E2E_SIGNIN_ISSUER_PORT: "9035",
  DISPATCH_E2E_SIGNIN_OUTSIDE_HOST: "localhost",
  DISPATCH_E2E_SIGNING_KEY: "signing-key",
};

// A directory of stand-ins for commands a script runs, each made by `stub` below.
const stubs = mkdtempSync(join(tmpdir(), "harness-signin-stubs-"));
afterAll(() => rmSync(stubs, { force: true, recursive: true }));

/** Writes an executable `name` into the stub directory that says it ran and exits `code`. */
function stub(name: string, code: number): void {
  const path = join(stubs, name);
  writeFileSync(path, `#!/bin/sh\necho "${name} stub reached" >&2\nexit ${code}\n`);
  chmodSync(path, 0o755);
}

/** `count` subprocess runs one after another, each under the short bound. */
const shortRuns = (count: number) => Array.from({ length: count }, () => "short" as const);

describe("DISPATCH_E2E_SIGNIN", () => {
  // Each value is one import of its own, so the row's limit is read from the two lists.
  const refusedSignins = ["dev", "OIDC", " oidc", "1", "Google", "google "];
  const acceptedSignins: Record<string, string>[] = [{}, { DISPATCH_E2E_SIGNIN: "" }, oidc];
  test(
    "every harness importer refuses a value other than oidc, google or unset, naming dev, oidc and google",
    async () => {
      for (const value of refusedSignins) {
        const { code, stderr } = await importPorts({ ...twoProcesses, DISPATCH_E2E_SIGNIN: value });
        expect(code).not.toBe(0);
        expect(stderr).toContain(
          `DISPATCH_E2E_SIGNIN must be "oidc", "google" or unset, not ${JSON.stringify(value)}`
        );
        expect(stderr).toContain("unset to sign people in through dev sign-in");
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

  // Each count is one import of its own, so the row's limit is read from the list.
  const googleProcesses = ["", "1", "2"];
  test(
    "google is accepted with one process and with two",
    async () => {
      for (const processes of googleProcesses) {
        const { code, stderr } = await importPorts({
          ...google,
          DISPATCH_E2E_PROCESSES: processes,
        });
        expect(stderr).toBe("");
        expect(code).toBe(0);
      }
    },
    rowLimit(...googleProcesses.map(() => "short" as const))
  );

  // The unknown value, oidc and google without the issuer, and google without each of its three
  // other inputs: six runs of run-server.sh.
  const googleRequired = [
    "DISPATCH_E2E_SIGNIN_DOMAIN",
    "DISPATCH_E2E_SIGNIN_OUTSIDE_HOST",
    "DISPATCH_E2E_SIGNING_KEY",
  ] as const;
  test(
    "run-server.sh refuses an unknown value, and oidc or google without its inputs",
    async () => {
      const base = { ...serverInputs, ...twoProcesses, DISPATCH_E2E_PORT: "9031" };
      const unknown = await run(["bash", runServer], { ...base, DISPATCH_E2E_SIGNIN: "dev" });
      expect(unknown.code).toBe(1);
      expect(unknown.stderr).toContain(
        "DISPATCH_E2E_SIGNIN must be oidc, google or unset, not 'dev'"
      );
      // Under oidc and google the dev sign-in key is not asked for; the issuer is.
      for (const signIn of ["oidc", "google"]) {
        const issuerless = await run(["bash", runServer], { ...base, DISPATCH_E2E_SIGNIN: signIn });
        expect(issuerless.code).not.toBe(0);
        expect(issuerless.stderr).not.toContain("DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY");
        expect(issuerless.stderr).toContain(
          "DISPATCH_E2E_SIGNIN_ISSUER_PORT must name the dev OIDC issuer"
        );
      }
      // Each input google's server needs is asked for by name.
      for (const input of googleRequired) {
        const { [input]: _, ...without } = googleServerInputs;
        const missing = await run(["bash", runServer], {
          ...without,
          DISPATCH_E2E_PORT: "9031",
          DISPATCH_E2E_SERVER_BIN: printEnvironment,
        });
        expect(missing.code).not.toBe(0);
        expect(missing.stderr).toContain(`${input} must`);
      }
    },
    rowLimit(...shortRuns(1 + 2 + googleRequired.length))
  );

  // The dev control and the three google readings: four runs of run-server.sh.
  test(
    "run-server.sh hands a google server Google's domain, passkey sign-in and a second origin on its own port",
    async () => {
      // The control: under dev sign-in the same reading sees dev sign-in and no second origin, so
      // the reading below can tell the two apart.
      const dev = await serverEnvironment({ ...serverInputs, DISPATCH_E2E_PORT: "9031" });
      expect(dev.DISPATCH_DEV_SIGNIN).toBe("1");
      expect(dev.DISPATCH_SERVER_URL).toBe("http://127.0.0.1:9031");
      expect(dev).not.toHaveProperty("DISPATCH_OUTSIDE_ORIGIN");
      expect(dev).not.toHaveProperty("DISPATCH_SIGNIN_DOMAIN");

      const signIn = {
        DISPATCH_SIGNIN_CLIENT_ID: "dispatch-e2e",
        DISPATCH_SIGNIN_CLIENT_SECRET: "secret",
        DISPATCH_SIGNIN_DOMAIN: "example.com",
        DISPATCH_SIGNIN_ISSUER: "http://127.0.0.1:9035",
        DISPATCH_SIGNIN_PASSKEY: "1",
        DISPATCH_SIGNING_KEY: "signing-key",
        DISPATCH_TEST_HOOKS: "1",
        DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER: "1",
      };
      const read = async (env: Record<string, string>) => {
        const given = await serverEnvironment({ ...googleServerInputs, ...env });
        expect(given).toMatchObject(signIn);
        for (const absent of [
          "DISPATCH_DEV_SIGNIN",
          "DISPATCH_DEV_SIGNIN_SHARED_KEY",
          "DISPATCH_SIGNIN_GROUP",
        ]) {
          expect(given).not.toHaveProperty(absent);
        }
        return given;
      };

      // One process on the port it was given: both origins on that port.
      const given = await read({ DISPATCH_E2E_PORT: "9031" });
      expect(given.DISPATCH_SERVER_URL).toBe("http://127.0.0.1:9031");
      expect(given.DISPATCH_OUTSIDE_ORIGIN).toBe("http://localhost:9031");

      // One process on a port the OS chose: both origins on the port held for it.
      const held = await read({});
      expect(held.DISPATCH_PORT).toMatch(/^[1-9][0-9]*$/);
      expect(held.DISPATCH_SERVER_URL).toBe(`http://127.0.0.1:${held.DISPATCH_PORT}`);
      expect(held.DISPATCH_OUTSIDE_ORIGIN).toBe(`http://localhost:${held.DISPATCH_PORT}`);

      // Two processes: both origins on the round-robin proxy's port, not on the process's own.
      const behindProxy = await read({
        ...twoProcesses,
        DISPATCH_E2E_PORT: "9031",
        DISPATCH_E2E_SERVER_PORT: "9037",
      });
      expect(behindProxy.DISPATCH_PORT).toBe("9037");
      expect(behindProxy.DISPATCH_SERVER_URL).toBe("http://127.0.0.1:9031");
      expect(behindProxy.DISPATCH_OUTSIDE_ORIGIN).toBe("http://localhost:9031");
    },
    rowLimit(...shortRuns(4))
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

  // No mode, oidc without a group, and google stopped at a stand-in for flock: three runs.
  test(
    "run-signin-issuer.sh asks for a person and a group under oidc alone",
    async () => {
      const client = {
        DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
        DISPATCH_E2E_SIGNIN_CLIENT_SECRET: "secret",
      };
      const modeless = await run(["bash", runSignInIssuer], client);
      expect(modeless.code).toBe(2);
      expect(modeless.stderr).toContain(
        "run-signin-issuer.sh: DISPATCH_E2E_SIGNIN must be oidc or google, not ''"
      );
      const groupless = await run(["bash", runSignInIssuer], {
        ...client,
        DISPATCH_E2E_SIGNIN: "oidc",
        DISPATCH_E2E_SIGNIN_AS: "alice@example.com",
      });
      expect(groupless.code).not.toBe(0);
      expect(groupless.stderr).toContain(
        "DISPATCH_E2E_SIGNIN_GROUP must name the sign-in pool group the Dispatch servers admit"
      );
      // Under google it asks for neither and goes on to build the issuer, which a stand-in for
      // `flock` stops at: the run gets past every input it reads.
      stub("flock", 97);
      const personless = await run(["bash", runSignInIssuer], {
        ...client,
        ...google,
        PATH: `${stubs}:${process.env.PATH ?? ""}`,
      });
      expect(personless.stderr).toContain("flock stub reached");
      expect(personless.code).toBe(97);
    },
    rowLimit(...shortRuns(3))
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

  // One listener table for one process and one for two.
  const googleRuns = [
    [google, ["dispatch"]],
    [{ ...google, ...twoProcesses }, ["dispatch-a", "dispatch-b"]],
  ] as const;
  test(
    "google starts the issuer, with no person of its own, before every server, and hands each the client, Google's domain and the outside host",
    async () => {
      for (const [env, serverIds] of googleRuns) {
        const listeners = await listenersIn(env);
        const ids = listeners.map((listener) => listener.id);
        const issuer = listeners.find((listener) => listener.id === "signin-issuer");
        const servers = listeners.filter(
          (listener) => listener.key.startsWith("dispatch") && listener.env
        );
        expect(servers.map((server) => server.id)).toEqual([...serverIds]);
        expect(ids.indexOf("signin-issuer")).toBeGreaterThan(-1);
        expect(ids.indexOf("signin-issuer")).toBeLessThan(
          Math.min(...servers.map((server) => ids.indexOf(server.id)))
        );
        // The people come from the issuer's queue, so it is handed none, and no group. Each value
        // is compared as a string: bun 1.4's `toMatchObject` writes an asymmetric matcher over the
        // received property it matched, so a value read after one would be the matcher.
        const secret = issuer?.env?.DISPATCH_E2E_SIGNIN_CLIENT_SECRET ?? "";
        expect(secret).toMatch(/^[A-Za-z0-9_-]{43}$/);
        expect(issuer?.env).toEqual({
          DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
          DISPATCH_E2E_SIGNIN_CLIENT_SECRET: secret,
        });
        const keys = new Set<string>();
        for (const server of servers) {
          const given = server.env ?? {};
          const key = given.DISPATCH_E2E_SIGNING_KEY ?? "";
          expect(key).toMatch(/^[A-Za-z0-9_-]{43}$/);
          expect(key).not.toEqual(secret);
          expect(given).toEqual({
            DISPATCH_E2E_SIGNIN_CLIENT_ID: "dispatch-e2e",
            DISPATCH_E2E_SIGNIN_CLIENT_SECRET: secret,
            DISPATCH_E2E_SIGNIN_DOMAIN: "example.com",
            DISPATCH_E2E_SIGNIN_OUTSIDE_HOST: "localhost",
            DISPATCH_E2E_SIGNING_KEY: key,
          });
          keys.add(key);
        }
        expect(keys.size).toBe(1);
      }
    },
    rowLimit(...googleRuns.map(() => "short" as const))
  );

  // The control's listener table and google's.
  test(
    "google starts no real broker, which a dev sign-in run of one process does",
    async () => {
      const broker = ["dev-oidc-issuer", "real-broker"];
      // The control: the same reading of a one-process dev sign-in run finds both.
      const underDev = (await listenersIn({})).map((listener) => listener.id);
      expect(underDev).toEqual(expect.arrayContaining(broker));
      const underGoogle = (await listenersIn(google)).map((listener) => listener.id);
      for (const id of broker) expect(underGoogle).not.toContain(id);
    },
    rowLimit("short", "short")
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

  // A refused resolution under oidc and under google, and dev sign-in's accepted one.
  const clashingSignIns = [oidc, google];
  test(
    "the issuer's port joins the collision check under oidc and google only",
    async () => {
      const resolve = [
        "bun",
        "-e",
        `(await import(${JSON.stringify(module("harness-ports.ts"))})).harnessPorts()`,
      ];
      const clash = { DISPATCH_E2E_SIGNIN_ISSUER_PORT: "9036", FAKE_ENVOY_PORT: "9036" };
      for (const signIn of clashingSignIns) {
        const refused = await run(resolve, { ...clash, ...signIn });
        expect(refused.code).not.toBe(0);
        expect(refused.stderr).toContain(
          "FAKE_ENVOY_PORT and DISPATCH_E2E_SIGNIN_ISSUER_PORT name port 9036"
        );
      }
      expect((await run(resolve, { ...clash, ...twoProcesses })).code).toBe(0);
    },
    rowLimit(...shortRuns(clashingSignIns.length + 1))
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
