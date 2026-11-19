// packages/broker-client/src/__tests__/held-tokens.test.ts
//
// How long the client holds what a trade sends and gets (`@legion/contracts/held-secrets`): a
// session trades all its life, so each token is held an hour past its lapse, not for the
// process's life; and the retry loop's own log goes through the scrub where no caller replaces it.

import { afterEach, expect, test } from "bun:test";
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

test("a trade holds its tokens an hour past their lapse, not for the process's life", async () => {
  const minted = fixtureSecret("access");
  const broker = stubBroker(() => ({ body: accepted(minted, 600) }));
  const exp = Math.floor(Date.now() / 1000) + 300;
  const subject = jwtLapsingAt(exp);
  const opaque = fixtureSecret("opaque");
  // When each trade began and ended, by the subject token it traded.
  const traded = new Map<string, { readonly before: number; readonly after: number }>();
  try {
    for (const read of [subject, opaque]) {
      const client = new BrokerTokenClient({
        brokerUrl: broker.url,
        key: await newDpopKey(),
        subject: sourceOf(read),
        log: () => {},
      });
      const before = Date.now();
      await client.token();
      const after = Date.now();
      traded.set(read, { before, after });
      // The token it got: an hour past its own expiry.
      const token = forgetsAt(minted) as number;
      expect(token).toBeGreaterThanOrEqual(before + 600_000 + HELD_PAST_LAPSE_MS);
      expect(token).toBeLessThanOrEqual(after + 600_000 + HELD_PAST_LAPSE_MS);
      client.close();
    }
    // A JWT subject token: an hour past its own `exp`.
    expect(forgetsAt(subject)).toBe(exp * 1000 + HELD_PAST_LAPSE_MS);
    // One whose lapse it cannot read: an hour past the trade that sent it, which a broker that
    // echoes it answers within.
    const held = forgetsAt(opaque) as number;
    const trade = traded.get(opaque) ?? { before: Number.NaN, after: Number.NaN };
    expect(held).toBeGreaterThanOrEqual(trade.before + HELD_PAST_LAPSE_MS);
    expect(held).toBeLessThanOrEqual(trade.after + HELD_PAST_LAPSE_MS);
  } finally {
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
