// packages/cipher-client/src/__tests__/renewal-cycle.test.ts
//
// How often a client trades, and a device login refreshes, across several lives of the broker's
// access token, whatever life the broker gives it (`BROKER_ACCESS_TOKEN_SECONDS`, S here) and
// whatever its subject has left. Each row moves the process's clock (`setSystemTime`) a fixed step
// at a time, calls `token()` at each step, and fails at the first call after which the client has
// traded, or the login has refreshed, more often in the last life than the bounds the row states:
// a token is held to the moment the renewal corpus names (`testdata/renewal.json`, never before
// half its life), and a device login's read trade asks for no more than half the life a refresh
// gives (the cap, `DeviceLoginIdentity.spend`). The clients are a device login's read client and
// its plain client, and a person's read client whose sign-in credential nears its end, which a
// read trade does not move.
//
// The clock stands still between steps, so a row's counts are those of a stepped clock: a trade
// made at exactly the moment a rule names is made there, where real time would have moved on a few
// milliseconds. This file takes from the package's source only what a build before these rules
// had too, so it runs against such a build as a control.

import { afterEach, expect, setSystemTime, test } from "bun:test";
import { readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { stubDeviceLoginBroker, writeDeviceLoginFile } from "../../test/device-logins";
import { serveMetadataIdentity } from "../../test/metadata-server";
import { boxes, CLIENT_ID, signedIn, stubIssuer } from "../../test/sign-ins";
import { DEVICE_LOGIN_FILE_VAR } from "../device-login-file";
import {
  BROKER_URL_VAR,
  type BrokerEnvironment,
  brokerTokenClient,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
} from "../environment";
import type { BrokerTokenClient } from "../token-client";
import { FORM_SCOPE, SCOPE_READ } from "../wire";
import { stubBroker } from "./stub";

const makeDirectory = temporaryDirectories("renewal-cycle");

/** This file's person's machine environments, each in a directory of its own ({@link boxes}). */
const box = boxes(makeDirectory);

const PERSON = "ada@example.com";

/** How far the clock moves between two calls. */
const STEP_SECONDS = 10;

/** How many lives of the access token a row runs across. */
const LIVES = 6;

/** What a row holds its client to in every life of the access token, as the row states it. */
interface Bounds {
  readonly tradesPerLife: number;
  readonly refreshesPerLife: number;
  /** Refreshes come more than this many seconds apart. */
  readonly refreshGap: number;
}

/** One row's client and what it is held to. */
interface Cycle extends Bounds {
  readonly client: BrokerTokenClient;
  readonly trades: () => number;
  readonly refreshes: () => number;
  /** Run after each call: a row whose file an older build rewrote takes out what it keeps. */
  readonly afterCall?: () => Promise<void>;
}

/** What a row's client did over its span. */
interface Counted {
  readonly trades: number;
  readonly refreshes: number;
}

/**
 * Builds `client`'s cycle at an access-token life of `life` seconds from `start`, held to
 * `bounds`, calls its client's `token()` every {@link STEP_SECONDS} across {@link LIVES} lives,
 * and after each call holds the trades and the refreshes of the last life, and the time since the
 * last refresh, to the row's bounds, naming the client, S and the call that broke one.
 */
async function run(client: Client, life: number, start: number, bounds: Bounds): Promise<Counted> {
  const cycle = await client.cycle(life, start, bounds);
  const name = `${client.name} at S = ${life} s`;
  const lifeMs = life * 1000;
  const traded: number[] = [];
  const refreshed: number[] = [];
  for (let at = 0; at < LIVES * lifeMs; at += STEP_SECONDS * 1000) {
    setSystemTime(new Date(start + at));
    await cycle.client.token();
    await cycle.afterCall?.();
    while (traded.length < cycle.trades()) traded.push(at);
    while (refreshed.length < cycle.refreshes()) {
      const last = refreshed.at(-1);
      if (last !== undefined) {
        expect(
          (at - last) / 1000,
          `${name}: a refresh at ${at / 1000} s comes ${(at - last) / 1000} s after the last`
        ).toBeGreaterThan(cycle.refreshGap);
      }
      refreshed.push(at);
    }
    const inLife = (times: readonly number[]): number =>
      times.filter((time) => time > at - lifeMs).length;
    expect(
      inLife(traded),
      `${name}: the trades in the life up to the call at ${at / 1000} s`
    ).toBeLessThanOrEqual(cycle.tradesPerLife);
    expect(
      inLife(refreshed),
      `${name}: the refreshes in the life up to the call at ${at / 1000} s`
    ).toBeLessThanOrEqual(cycle.refreshesPerLife);
  }
  cycle.client.close();
  return { trades: traded.length, refreshes: refreshed.length };
}

const stops: (() => void)[] = [];
afterEach(() => {
  setSystemTime();
  for (const stop of stops.splice(0)) stop();
});

/** The metadata server a person's machine asks for its own Google identity token, the actor of a
 *  person's trade. */
serveMetadataIdentity(fixtureSecret("machine"));

/**
 * A device login whose broker mints access tokens of `life` seconds, as a laptop's first
 * `dispatch` command meets it (no access token kept yet), and its read or plain client, held to
 * `bounds`. Where `olderBuild` is set, an older build rewrites the login's file after every call,
 * leaving out the life a refresh gave the access token, as that build's refreshes and saves do.
 */
async function deviceLoginCycle(
  life: number,
  read: boolean,
  bounds: Bounds,
  olderBuild = false
): Promise<Cycle> {
  const stub = stubDeviceLoginBroker({ accessSeconds: life });
  stops.push(stub.stop);
  const path = join(makeDirectory(), "legion", "device-login.json");
  await writeDeviceLoginFile(path, stub.url);
  const env: BrokerEnvironment = {
    [BROKER_URL_VAR]: stub.url,
    [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
    [DEVICE_LOGIN_FILE_VAR]: path,
  };
  const made = await brokerTokenClient(env, "ses-laptop-1", read ? {} : undefined);
  if (made === undefined) throw new Error("the factory made no client for a device login");
  const afterCall = async (): Promise<void> => {
    const json = JSON.parse(await readFile(path, "utf8")) as Record<string, unknown>;
    if (!("access_life_seconds" in json)) return;
    delete json["access_life_seconds"];
    await writeFile(path, JSON.stringify(json), { mode: 0o600 });
  };
  return {
    client: made.client,
    trades: () => stub.trades.length,
    refreshes: () => stub.refreshes.length,
    ...bounds,
    ...(olderBuild ? { afterCall } : {}),
  };
}

/**
 * A person's read client on a machine whose broker mints tokens of `life` seconds, and whose
 * sign-in credential lapses at `start` and {@link LIVES} lives, held to `bounds`: the broker
 * answers a read trade with the credential's remaining life where that is shorter, in whole
 * seconds, truncated, as `clampNarrowed` bounds it, and a read trade moves the credential not at
 * all, as `signInCredential` takes it. The row's last call comes a step before the credential
 * lapses. Its refreshes are the refresh grants the broker is sent: a person's client refreshes
 * nothing there, so its rows allow none.
 */
async function personCycle(life: number, start: number, bounds: Bounds): Promise<Cycle> {
  const ends = start + LIVES * life * 1000;
  const broker = stubBroker((trade) => {
    const left = Math.floor((ends - Date.now()) / 1000);
    if (trade.form.get(FORM_SCOPE) !== SCOPE_READ || left <= 0) {
      return { status: 400, body: { error: "invalid_grant" } };
    }
    return {
      body: {
        access_token: fixtureSecret("read"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: Math.min(life, left),
        scope: SCOPE_READ,
      },
    };
  });
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON });
  stops.push(broker.stop, issuer.stop);
  const env = await box(broker.url, issuer.url, [await signedIn(PERSON)]);
  const made = await brokerTokenClient(env, "ses_cycle", {});
  if (made === undefined) throw new Error("the factory made no client for a person");
  return {
    client: made.client,
    trades: () => broker.trades.length,
    refreshes: () =>
      broker.trades.filter((trade) => trade.form.get("grant_type") === "refresh_token").length,
    ...bounds,
  };
}

/** How long a row may take: every call is a turn of the event loop, and a trade a request. */
const ROW_MS = 120_000;

/** A row's client: its name, which the row's title and `run`'s failures carry, and the builder of
 *  its cycle at a life from a start, held to the row's bounds. */
interface Client {
  readonly name: string;
  readonly cycle: (life: number, start: number, bounds: Bounds) => Promise<Cycle>;
}

/** A device login's read or plain client ({@link deviceLoginCycle}), whose file an older build
 *  rewrites where `olderBuild` is set. */
function deviceLoginClient(read: boolean, olderBuild = false): Client {
  return {
    name: `a device login's ${read ? "read" : "plain"} client${olderBuild ? " whose file an older build rewrote" : ""}`,
    cycle: (life, _start, bounds) => deviceLoginCycle(life, read, bounds, olderBuild),
  };
}

const READ = deviceLoginClient(true);
const PLAIN = deviceLoginClient(false);
const PERSON_CLIENT: Client = {
  name: "a person's read client whose sign-in credential nears its end",
  cycle: personCycle,
};

// Fifteen rows: each access-token life S against each of the three clients, with the bounds each
// row states and the rule it follows. A token of S s renews after the corpus's moment m for S
// under the client's 300 s window. A device login spends a kept access token with at least its
// least life left (`leastAccessLifeMs`): the read ask of 301 s, capped at S/2 and never under the
// minute, and for a plain trade the minute alone; a refresh leaves S, so refreshes come more than
// S less that least life apart, ceil(S / (S - least)) a life. A plain token lives S and is held to
// m: ceil(S / m) trades a life. A read token traded from a fresh access token lives S and is held
// to m; one traded with exactly the least life left, which `spend`'s strict `<` spends, lives that
// and brings one more trade, refreshed at the next, wherever S - m is under twice the least life:
// ceil(S / m) + 1 a life. A person's read token is held to m while the credential has S or more
// left, and then lives what is left and is held to half of it, so calls 10 s apart see at most
// floor(log2(S / 10)) + 1 more: ceil(S / m) + floor(log2(S / 10)) + 1 a life.
test.each([
  // Corpus 120 → 60 s; least 60 s (S/2): trades 2 + 1, refreshes ceil(120/60), gap 120 - 60.
  { client: READ, life: 120, tradesPerLife: 3, refreshesPerLife: 2, refreshGap: 60 },
  // Corpus 120 → 60 s; least 60 s: trades ceil(120/60), refreshes ceil(120/60), gap 120 - 60.
  { client: PLAIN, life: 120, tradesPerLife: 2, refreshesPerLife: 2, refreshGap: 60 },
  // Corpus 120 → 60 s: trades ceil(120/60) + floor(log2(12)) + 1 = 2 + 3 + 1; no refresh.
  { client: PERSON_CLIENT, life: 120, tradesPerLife: 6, refreshesPerLife: 0, refreshGap: 0 },
  // Corpus 300 → 150 s; least 150 s (S/2): trades 2 + 1, refreshes ceil(300/150), gap 300 - 150.
  { client: READ, life: 300, tradesPerLife: 3, refreshesPerLife: 2, refreshGap: 150 },
  // Corpus 300 → 150 s; least 60 s: trades ceil(300/150), refreshes ceil(300/240), gap 300 - 60.
  { client: PLAIN, life: 300, tradesPerLife: 2, refreshesPerLife: 2, refreshGap: 240 },
  // Corpus 300 → 150 s: trades ceil(300/150) + floor(log2(30)) + 1 = 2 + 4 + 1; no refresh.
  { client: PERSON_CLIENT, life: 300, tradesPerLife: 7, refreshesPerLife: 0, refreshGap: 0 },
  // Corpus 302 → 151 s; least 151 s (S/2): trades 2 + 1, refreshes ceil(302/151), gap 302 - 151.
  { client: READ, life: 302, tradesPerLife: 3, refreshesPerLife: 2, refreshGap: 151 },
  // Corpus 302 → 151 s; least 60 s: trades ceil(302/151), refreshes ceil(302/242), gap 302 - 60.
  { client: PLAIN, life: 302, tradesPerLife: 2, refreshesPerLife: 2, refreshGap: 242 },
  // Corpus 302 → 151 s: trades ceil(302/151) + floor(log2(30.2)) + 1 = 2 + 4 + 1; no refresh.
  { client: PERSON_CLIENT, life: 302, tradesPerLife: 7, refreshesPerLife: 0, refreshGap: 0 },
  // Corpus 600 → 300 s; least 300 s (S/2): trades 2 + 1, refreshes ceil(600/300), gap 600 - 300.
  { client: READ, life: 600, tradesPerLife: 3, refreshesPerLife: 2, refreshGap: 300 },
  // Corpus 600 → 300 s; least 60 s: trades ceil(600/300), refreshes ceil(600/540), gap 600 - 60.
  { client: PLAIN, life: 600, tradesPerLife: 2, refreshesPerLife: 2, refreshGap: 540 },
  // Corpus 600 → 300 s: trades ceil(600/300) + floor(log2(60)) + 1 = 2 + 5 + 1; no refresh.
  { client: PERSON_CLIENT, life: 600, tradesPerLife: 8, refreshesPerLife: 0, refreshGap: 0 },
  // Corpus 900 → 600 s; least 301 s (ask): trades 2 + 1, refreshes ceil(900/599), gap 900 - 301.
  { client: READ, life: 900, tradesPerLife: 3, refreshesPerLife: 2, refreshGap: 599 },
  // Corpus 900 → 600 s; least 60 s: trades ceil(900/600), refreshes ceil(900/840), gap 900 - 60.
  { client: PLAIN, life: 900, tradesPerLife: 2, refreshesPerLife: 2, refreshGap: 840 },
  // Corpus 900 → 600 s: trades ceil(900/600) + floor(log2(90)) + 1 = 2 + 6 + 1; no refresh.
  { client: PERSON_CLIENT, life: 900, tradesPerLife: 9, refreshesPerLife: 0, refreshGap: 0 },
])(
  "$client.name trades and refreshes within its bounds at an access-token life of $life s",
  async ({ client, life, ...bounds }) => {
    await run(client, life, Date.now(), bounds);
  },
  ROW_MS
);

// Under a minute of life every trade refreshes, plain or read, by the minute every trade keeps
// alone: the read trade's ask may add no refresh to the plain client's. Both are held to the
// same bounds: the least life, the minute, is over S, so every trade refreshes; corpus 30 → 15 s:
// trades ceil(30/15), refreshes as many, and no gap (30 - 60).
test(
  "at an access-token life of 30 s a device login's read client refreshes no more than its plain client",
  async () => {
    const start = Date.now();
    const bounds = { tradesPerLife: 2, refreshesPerLife: 2, refreshGap: -30 };
    const plain = await run(PLAIN, 30, start, bounds);
    const read = await run(READ, 30, start, bounds);
    expect(read.refreshes).toBeLessThanOrEqual(plain.refreshes);
  },
  ROW_MS
);

// A file an older build rewrote keeps no life, so a read trade spends a token with the minute
// left, as a plain one does, until a refresh of this build keeps a life again. At 120 s the read
// client may still refresh more often than the plain client: a read token is clamped to the
// login's access token, so it renews at its half life with less of that token left, while a plain
// token is not clamped. Both are held to the bounds of one life, with the minute as the least
// life: at 120 s that is S/2 too, so the bounds are those of the rows above at 120 s.
test.each([
  // Corpus 120 → 60 s; least 60 s (the minute): trades 2 + 1, refreshes ceil(120/60), gap 120 - 60.
  { client: deviceLoginClient(true, true), tradesPerLife: 3, refreshesPerLife: 2, refreshGap: 60 },
  // Corpus 120 → 60 s; least 60 s: trades ceil(120/60), refreshes ceil(120/60), gap 120 - 60.
  { client: deviceLoginClient(false, true), tradesPerLife: 2, refreshesPerLife: 2, refreshGap: 60 },
])(
  "at an access-token life of 120 s $client.name trades and refreshes within its bounds",
  async ({ client, ...bounds }) => {
    await run(client, 120, Date.now(), bounds);
  },
  ROW_MS
);
