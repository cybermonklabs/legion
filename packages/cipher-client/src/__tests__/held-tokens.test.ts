// packages/cipher-client/src/__tests__/held-tokens.test.ts
//
// How long the client holds what a trade sends and gets (`@legion/contracts/held-secrets`): a
// session trades all its life, so each token is held an hour past its lapse, not for the
// process's life; and the retry loop's own log goes through the scrub where no caller replaces it.

import { afterEach, expect, setSystemTime, test } from "bun:test";
import {
  forgetHeldSecrets,
  HELD_PAST_LAPSE_MS,
  holdSecret,
  WITHHELD,
} from "@legion/contracts/held-secrets";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { newDpopKey } from "../dpop";
import type { SubjectToken, TokenSource } from "../source";
import { BrokerTokenClient } from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, stubBroker } from "./stub";

afterEach(forgetHeldSecrets);

/** When the process's one registry forgets `value`: epoch milliseconds, `undefined` for a value
 *  held for life, and `null` for one it does not hold. */
function forgetsAt(value: string): number | undefined | null {
  const registry = (globalThis as Record<symbol, unknown>)[
    Symbol.for("@legion/contracts/held-secrets")
  ] as { values: Set<string>; forgets: Map<string, number> };
  if (!registry.values.has(value)) return null;
  return registry.forgets.get(value);
}

/** A source that answers `token` at each read. */
function sourceOf(token: string): TokenSource {
  return {
    read: async (): Promise<SubjectToken> => ({ token, tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
    describe: () => "a scripted identity",
  };
}

/** A JWT whose payload says it lapses at `exp` (epoch seconds); its signature is never checked. */
function jwtLapsingAt(exp: number): string {
  const part = (value: unknown) => Buffer.from(JSON.stringify(value)).toString("base64url");
  return `${part({ alg: "RS256" })}.${part({ exp, sub: "system:serviceaccount:legion:pod" })}.${fixtureSecret("sig")}`;
}

/** The moment `Date.now()` is held at while the row trades, so each lapse it reads is exact. */
const TRADED_AT = new Date("2026-03-04T05:06:07.000Z").getTime();

test("a trade holds its tokens an hour past their lapse, not for the process's life", async () => {
  const minted = fixtureSecret("access");
  const broker = stubBroker(() => ({ body: accepted(minted, 600) }));
  // Bun holds `Date.now()`, which the client dates each lapse from, at TRADED_AT; timers stay real.
  setSystemTime(TRADED_AT);
  const exp = Math.floor(TRADED_AT / 1000) + 300;
  const subject = jwtLapsingAt(exp);
  const opaque = fixtureSecret("opaque");
  try {
    for (const read of [subject, opaque]) {
      const client = new BrokerTokenClient({
        brokerUrl: broker.url,
        key: await newDpopKey(),
        subject: sourceOf(read),
        log: () => {},
      });
      await client.token();
      // The token it got: an hour past its own expiry, 600 s after the trade.
      expect(forgetsAt(minted)).toBe(TRADED_AT + 600_000 + HELD_PAST_LAPSE_MS);
      client.close();
    }
    // A JWT subject token: an hour past its own `exp`.
    expect(forgetsAt(subject)).toBe(exp * 1000 + HELD_PAST_LAPSE_MS);
    // One whose lapse it cannot read: an hour past the trade that sent it, which a broker that
    // echoes it answers within.
    expect(forgetsAt(opaque)).toBe(TRADED_AT + HELD_PAST_LAPSE_MS);
  } finally {
    setSystemTime();
    broker.stop();
  }
});

test("the retry loop's own log goes through the scrub where no caller replaces it", async () => {
  // Its line names the broker as `urlForText` does, here held as a secret so the line holds one.
  let answers = 0;
  const broker = stubBroker(() => {
    answers += 1;
    return answers === 1
      ? { status: 503, body: { error: "temporarily_unavailable" } }
      : { body: accepted(fixtureSecret("access"), 600) };
  });
  const planted = holdSecret(broker.url.replace("http://", ""));
  const printed: string[] = [];
  const warn = console.warn;
  console.warn = (...args: unknown[]) => {
    printed.push(args.map(String).join(" "));
  };
  const client = new BrokerTokenClient({
    brokerUrl: broker.url,
    key: await newDpopKey(),
    subject: sourceOf(fixtureSecret("subject")),
    sleep: async () => {},
    random: () => 0,
  });
  try {
    await client.token(undefined, { waitForReady: true });
  } finally {
    console.warn = warn;
    client.close();
    broker.stop();
  }
  expect(printed.length).toBeGreaterThan(0);
  expect(printed.join("\n")).toContain(WITHHELD);
  expect(printed.join("\n")).not.toContain(planted);
});
