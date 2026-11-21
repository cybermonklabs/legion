// packages/cipher-client/src/__tests__/membership-held.test.ts
//
// What the client does when the broker answers that Dispatch holds the person a credential
// names: the token endpoint's `membership_held`. A hold is not a revocation. It ends at the
// person's next Dispatch sign-in, or the next refresh that confirms them, and the identity the
// client trades is the one the broker serves again then, with no new credential. So the refusal
// is not final: the client's retry loop keeps its token source and trades again, as it does for a
// 429.

import { expect, test } from "bun:test";
import { turnsUntil } from "@legion/test-fixtures/fake-clock";
import { newDpopKey } from "../dpop";
import { BrokerTokenClient } from "../token-client";
import { BrokerRefusedError, isFinalRefusal, TradeRetryingError } from "../token-errors";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

/** The broker's description of its `membership_held` refusal, with the person's login in it. */
const HELD =
  "ada@example.com's credentials are held by Dispatch; they work again when ada@example.com signs in to Dispatch, or when Dispatch can confirm ada@example.com with Google again";

/** The retry loop's waits, which end when the row says, so it trades again only then. */
function gatedSleep() {
  const waiting: (() => void)[] = [];
  return {
    sleep: (_ms: number, signal?: AbortSignal): Promise<void> => {
      const { promise, resolve } = Promise.withResolvers<void>();
      waiting.push(resolve);
      signal?.addEventListener("abort", () => resolve(), { once: true });
      return promise;
    },
    /** Ends the loop's wait once it is waiting, so the next trade is the release's. Waits on turns
     *  of the event loop for the loop to ask for its wait, and fails naming it at the row's
     *  deadline (`turnsUntil`), so a loaded machine delays the row and never fails it. */
    async wake(): Promise<void> {
      await turnsUntil("waiting before its next trade", () => waiting.length > 0);
      for (const resolve of waiting.splice(0)) resolve();
    },
  };
}

/** A client of `url` that trades `subject`, logs nowhere and waits on `sleep`. */
async function clientOf(
  url: string,
  sleep: (ms: number, signal?: AbortSignal) => Promise<void>,
  clock?: { now: number }
): Promise<BrokerTokenClient> {
  return new BrokerTokenClient({
    brokerUrl: url,
    key: await newDpopKey(),
    subject: {
      read: async () => ({ token: "subject", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
      describe: () => "a scripted identity",
    },
    renewBeforeMs: 60_000,
    log: () => {},
    sleep,
    ...(clock === undefined ? {} : { now: () => clock.now }),
  });
}

test("membership_held is not a final refusal", async () => {
  const refused = new BrokerRefusedError(400, "membership_held", HELD, "ada@example.com's sign-in");
  expect(
    refused.permanent,
    "membership_held read as permanent: Dispatch releases the hold at the person's next sign-in, and the identity in hand is served again then"
  ).toBe(false);
  expect(isFinalRefusal(refused), "membership_held read as a final refusal").toBe(false);
  expect(refused.code).toBe("membership_held");

  // The same refusal from a broker, then the release: the client trades again with the same
  // token source and is served.
  let held = true;
  const broker = stubBroker((_trade, count) =>
    held
      ? { status: 400, body: { error: "membership_held", error_description: HELD } }
      : { body: accepted(`token-${count}`, 600) }
  );
  const gate = gatedSleep();
  const client = await clientOf(broker.url, gate.sleep);
  try {
    // A caller that fails fast is told the refusal at once, while the loop trades on.
    const during = await refusalOf(() => client.token());
    expect(during).toBeInstanceOf(TradeRetryingError);
    expect((during as Error).message).toContain("HTTP 400 membership_held");
    expect(isFinalRefusal(during), "the trade's membership_held read as a final refusal").toBe(
      false
    );

    held = false;
    const released = client.token(undefined, { waitForReady: true });
    await gate.wake();
    expect((await released).accessToken).toBe("token-2");
    expect(broker.trades.length).toBe(2);
  } finally {
    client.close();
    broker.stop();
  }
});

test("membership_held answers the token in hand while it is live", async () => {
  // The hold comes inside the renewal window of a live token: the trade is refused, and the
  // token in hand still answers, as it does for a 429. Past its expiry nothing live is left, and
  // the refusal is told, still not final; the release then brings a new token to the same client,
  // from the same token source.
  let held = false;
  const broker = stubBroker((_trade, count) =>
    held
      ? { status: 400, body: { error: "membership_held", error_description: HELD } }
      : { body: accepted(`token-${count}`, 600) }
  );
  const clock = { now: Date.now() };
  const gate = gatedSleep();
  const client = await clientOf(broker.url, gate.sleep, clock);
  try {
    expect((await client.token()).accessToken).toBe("token-1");

    held = true;
    clock.now += 560_000;
    expect(
      (await client.token()).accessToken,
      "the hold dropped a live token in hand; it is the token the broker serves again after the release"
    ).toBe("token-1");
    expect(broker.trades.length).toBe(2);

    clock.now += 60_000;
    const lapsed = await refusalOf(() => client.token());
    expect(lapsed).toBeInstanceOf(TradeRetryingError);
    expect((lapsed as Error).message).toContain("HTTP 400 membership_held");
    expect(isFinalRefusal(lapsed), "the lapsed token's membership_held read as final").toBe(false);

    held = false;
    clock.now += 300_000;
    const released = client.token(undefined, { waitForReady: true });
    await gate.wake();
    expect((await released).accessToken).toBe("token-3");
  } finally {
    client.close();
    broker.stop();
  }
});
