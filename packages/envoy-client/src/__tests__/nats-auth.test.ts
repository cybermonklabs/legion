import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { connect, nkeys } from "nats";
import { natsSeedFromEnvironment } from "../envoy-settings";
import { natsAuthOptions } from "../nats-auth";
import type { BrokerNatsCredential, NatsCredential } from "../nats-credential";
import { type NatsServer, startNatsServer, useNatsServerDirectory } from "./nats-server";

interface TestKeyPair {
  getSeed(): Uint8Array;
  getPublicKey(): string;
}

/** nats.js types its `nkeys` export `any`. */
const testKeys: { createUser(): TestKeyPair; createAccount(): TestKeyPair } = nkeys;

const makeDirectory = temporaryDirectories("nats-seed");
const serverDirectory = useNatsServerDirectory();

function createUser(): { readonly seed: string; readonly publicKey: string } {
  const user = testKeys.createUser();
  return { seed: new TextDecoder().decode(user.getSeed()), publicKey: user.getPublicKey() };
}

function seedFile(contents: string): string {
  const file = path.join(makeDirectory(), "seed");
  writeFileSync(file, contents, { mode: 0o600 });
  return file;
}

/** The credential the environment's seed variables name, or none. */
async function seedCredential(env: NodeJS.ProcessEnv): Promise<NatsCredential> {
  const seed = await natsSeedFromEnvironment(env);
  return seed === undefined ? { kind: "none" } : { kind: "seed", seed };
}

/** Connects as a client of this package does: `servers` plus the authenticator the environment
 * names, and nothing else that bears on authentication. */
async function connectWith(url: string, env: NodeJS.ProcessEnv) {
  const connection = await connect({
    servers: [url],
    name: "nats-auth-test",
    ...natsAuthOptions(await seedCredential(env)),
    reconnect: false,
  });
  await connection.flush();
  return connection;
}

// Docker-backed: CI runs this package's tests with LEGION_E2E=1 after pulling the image.
describe.skipIf(process.env.LEGION_E2E !== "1")("natsAuthOptions against a real NATS", () => {
  const user = createUser();
  let open: NatsServer;
  let authorized: NatsServer;

  beforeAll(async () => {
    [open, authorized] = await Promise.all([
      startNatsServer(serverDirectory),
      startNatsServer(serverDirectory, user.publicKey),
    ]);
  }, 120_000);
  afterAll(() => {
    open?.stop();
    authorized?.stop();
  }, 60_000);

  test("with no seed, connects to a server that asks for no credential", async () => {
    const connection = await connectWith(open.url, {});
    await connection.close();
  });

  test("with the user's seed, from the file (which wins) or the variable, connects to a server that requires it", async () => {
    for (const env of [
      { NATS_NKEY_SEED_FILE: seedFile(`\n  ${user.seed}\n`), NATS_NKEY_SEED: "not a seed" },
      { NATS_NKEY_SEED: user.seed },
    ]) {
      const connection = await connectWith(authorized.url, env);
      await connection.close();
    }
  });

  test("with no seed, is refused by a server that requires one", async () => {
    await expect(connectWith(authorized.url, {})).rejects.toThrow(/authorization violation/i);
  });
});

describe("natsAuthOptions", () => {
  const user = createUser();

  test("is no option for no credential", async () => {
    expect(await natsSeedFromEnvironment({})).toBeUndefined();
    expect(natsAuthOptions({ kind: "none" })).toEqual({});
  });

  test("authenticates with a seed resolved elsewhere, and names its source when it is no user seed", () => {
    const source = "envoy.natsSeed in /tmp/envoy.json";
    expect(natsAuthOptions({ kind: "seed", seed: { value: user.seed, source } })).toHaveProperty(
      "authenticator"
    );
    expect(() => natsAuthOptions({ kind: "seed", seed: { value: "hunter2", source } })).toThrow(
      "envoy.natsSeed in /tmp/envoy.json does not hold a valid nkey seed"
    );
  });

  test("presents the broker credential's own CONNECT credential", () => {
    const asked: (string | undefined)[] = [];
    const presented = "example-session-credential";
    const broker: BrokerNatsCredential = {
      ready: async () => {},
      renew: async () => {},
      expiresAt: () => new Date(0),
      needsReconnect: () => false,
      drop: () => {},
      tokenGeneration: () => 0,
      tokenSession: () => undefined,
      authenticator: (nonce?: string) => {
        asked.push(nonce);
        return { auth_token: presented, sig: "the-proof" };
      },
    };
    const { authenticator } = natsAuthOptions({ kind: "broker", broker });
    if (typeof authenticator !== "function") throw new Error("no authenticator for the broker");
    expect(authenticator("server-nonce")).toEqual({
      auth_token: presented,
      sig: "the-proof",
    });
    expect(asked).toEqual(["server-nonce"]);
  });

  test("refuses an unusable seed naming its variable, never its path, never falling back", async () => {
    const dir = makeDirectory();
    const missing = path.join(dir, "missing");
    const blank = seedFile(" \n");
    const notASeed = seedFile("SUNOTASEED");
    const account = seedFile(new TextDecoder().decode(testKeys.createAccount().getSeed()));
    const cases: [NodeJS.ProcessEnv, string][] = [
      [
        { NATS_NKEY_SEED_FILE: missing, NATS_NKEY_SEED: user.seed },
        "NATS_NKEY_SEED_FILE names a file that could not be read (ENOENT)",
      ],
      [{ NATS_NKEY_SEED_FILE: blank }, "NATS_NKEY_SEED_FILE names a file that is empty"],
      [{ NATS_NKEY_SEED_FILE: "" }, "NATS_NKEY_SEED_FILE is set but empty"],
      [
        { NATS_NKEY_SEED_FILE: notASeed },
        "the file NATS_NKEY_SEED_FILE names does not hold a valid nkey seed",
      ],
      [
        { NATS_NKEY_SEED_FILE: account },
        "the file NATS_NKEY_SEED_FILE names holds an nkey seed that is not a user's",
      ],
      [{ NATS_NKEY_SEED: "  " }, "NATS_NKEY_SEED is set but empty"],
      [{ NATS_NKEY_SEED: "hunter2" }, "NATS_NKEY_SEED does not hold a valid nkey seed"],
    ];
    for (const [env, message] of cases) {
      let thrown: unknown;
      try {
        natsAuthOptions(await seedCredential(env));
      } catch (error) {
        thrown = error;
      }
      if (!(thrown instanceof Error)) throw new Error(`accepted ${JSON.stringify(env)}`);
      const text = thrown.message;
      expect(text).toContain(message);
      expect(text).not.toContain(user.seed);
      expect(text).not.toContain("hunter2");
      expect(text).not.toContain(dir);
    }
  });
});
