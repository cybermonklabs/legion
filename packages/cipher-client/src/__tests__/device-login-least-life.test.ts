// packages/cipher-client/src/__tests__/device-login-least-life.test.ts
//
// The least life a device login's kept access token must have left for a trade to spend it
// without a refresh first (`leastAccessLifeMs`, `DeviceLoginIdentity.spend`): for a read trade its
// ask, capped at half the life the last refresh gave and never under the minute every trade keeps,
// and for a plain trade the minute alone. The device login's other rows live in
// device-login.test.ts.

import { afterEach, expect, setSystemTime, test } from "bun:test";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { stubDeviceLoginBroker, writeDeviceLoginFile } from "../../test/device-logins";
import { DeviceLoginIdentity, leastAccessLifeMs } from "../device-login";
import { DEFAULT_RENEW_BEFORE_MS, EXPIRES_IN_STEP_MS, TRADE_ATTEMPT_MS } from "../token-client";

const makeDirectory = temporaryDirectories("device-login-least-life");

const stops: (() => void)[] = [];
afterEach(() => {
  setSystemTime();
  for (const stop of stops.splice(0)) stop();
});

/** A stand-in broker stopped after the test. */
function broker(options: Parameters<typeof stubDeviceLoginBroker>[0] = {}) {
  const stub = stubDeviceLoginBroker(options);
  stops.push(stub.stop);
  return stub;
}

/** A device login file's path in a directory of this test's own. */
function loginPath(): string {
  return join(makeDirectory(), "legion", "device-login.json");
}

/** What a read trade asks a device login for: its client's window and a second (`#trade`). */
const READ_ASK_MS = DEFAULT_RENEW_BEFORE_MS + EXPIRES_IN_STEP_MS;

// The least life a kept access token must have left for a read trade to spend it without a
// refresh: the ask, capped at half the life S the last refresh gave, never under the minute every
// trade keeps.
test.each([
  ["S = 1 s: the minute", 1, 60_000],
  ["S = 30 s: the minute", 30, 60_000],
  ["S = 59 s: the minute", 59, 60_000],
  ["S = 60 s: the minute", 60, 60_000],
  ["S = 61 s: the minute", 61, 60_000],
  ["S = 120 s: half of S, the minute", 120, 60_000],
  ["S = 301 s: half of S, under the ask", 301, 150_500],
  ["S = 302 s: half of S, under the ask", 302, 151_000],
  ["S = 3600 s: the ask, under half of S", 3600, READ_ASK_MS],
  // An older build's file: no cap is known, and the ask uncapped would refresh at every read
  // trade wherever S is under it, so the minute stands until a refresh keeps S.
  ["no S kept, as an older build writes the file: the minute alone", null, 60_000],
] as const)("a read trade's least life at %s", (_, seconds, least) => {
  expect(leastAccessLifeMs(READ_ASK_MS, seconds)).toBe(least);
});

// `spend` refreshes a kept access token with strictly less than its least life left
// (`expires - now < least`). At S = 60 s a read trade's least life is the minute, all a token
// fresh from a refresh has left: one with exactly that left is spent as it is, and one with a
// millisecond less is refreshed first, so the strict `<` is what keeps such a token from being
// refreshed at every trade. The clock stands still, so the time left is the row's to the
// millisecond.
test("a read trade spends a kept access token with exactly its least life left, and refreshes one with a millisecond less", async () => {
  const seconds = 60;
  const least = leastAccessLifeMs(READ_ASK_MS, seconds);
  const now = Date.now();
  setSystemTime(new Date(now));
  for (const [left, refreshes] of [
    [least, 0],
    [least - 1, 1],
  ] as const) {
    const stub = broker();
    const path = loginPath();
    const kept = fixtureSecret("access");
    await writeDeviceLoginFile(path, stub.url, {
      access_token: kept,
      access_expires_at: new Date(now + left).toISOString(),
      access_life_seconds: seconds,
    });
    const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
    const spent = await identity.spend(async (subject) => subject.token, undefined, READ_ASK_MS);
    const what = `a kept access token with ${left} ms left of a least life of ${least} ms`;
    expect(stub.refreshes, what).toHaveLength(refreshes);
    expect(spent, what).toBe(refreshes === 0 ? kept : (stub.loginTokens[0] as string));
  }
});

test("a plain trade asks for no least life and takes the minute alone, whatever S is kept", () => {
  for (const seconds of [null, 1, 60, 302, 3600]) {
    expect(leastAccessLifeMs(undefined, seconds)).toBe(2 * TRADE_ATTEMPT_MS);
  }
});
