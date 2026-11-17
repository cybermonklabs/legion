// packages/broker-client/src/__tests__/membership-held.test.ts
//
// What the client does when the broker answers that Dispatch holds the person a credential
// names: the token endpoint's `membership_held`. A hold is not a revocation. It ends at the
// person's next Dispatch sign-in, or the next refresh that confirms them, and the identity the
// client trades is the one the broker serves again then, with no new credential. So the refusal
// is not final: the client keeps its token source and trades again.

import { expect, test } from "bun:test";
import { newDpopKey } from "../dpop";
import { BrokerRefusedError, BrokerTokenClient, isFinalRefusal } from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

/** The broker's description of its `membership_held` refusal, with the person's login in it. */
const HELD =
  "ada@example.com's credentials are held by Dispatch; they work again when ada@example.com signs in to Dispatch, or when Dispatch can confirm ada@example.com with Google again";

test("membership_held is not a final refusal", async () => {
  const refused = new BrokerRefusedError(400, "membership_held", HELD, "ada@example.com's sign-in");
  expect(
    refused.permanent,
    "membership_held read as permanent: Dispatch releases the hold at the person's next sign-in, and the identity in hand is served again then"
  ).toBe(false);
  expect(isFinalRefusal(refused), "membership_held read as a final refusal").toBe(false);

  // The same refusal from a broker, then the release: the client trades again with the same
  // token source and is served.
  let held = true;
  const broker = stubBroker((_trade, count) =>
    held
      ? { status: 400, body: { error: "membership_held", error_description: HELD } }
      : { body: accepted(`token-${count}`, 600) }
  );
  const client = new BrokerTokenClient({
    brokerUrl: broker.url,
    key: await newDpopKey(),
    subject: {
      read: async () => ({ token: "subject", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
      describe: () => "a scripted identity",
    },
  });
  try {
    const during = await refusalOf(() => client.token());
    expect(during).toBeInstanceOf(BrokerRefusedError);
    expect((during as BrokerRefusedError).code).toBe("membership_held");
    expect(isFinalRefusal(during), "the trade's membership_held read as a final refusal").toBe(
      false
    );

    held = false;
    expect((await client.token()).accessToken).toBe("token-2");
    expect(broker.trades.length).toBe(2);
  } finally {
    client.close();
    broker.stop();
  }
});

test("membership_held answers the token in hand while it is live", async () => {
  // The hold comes inside the renewal window of a live token: the trade is refused, and the
  // token in hand still answers, as it does for a 429. Past its expiry nothing live is left and
  // the refusal is thrown, still not final; the release then brings a new token to the same
  // client, from the same token source.
  let held = false;
  const broker = stubBroker((_trade, count) =>
    held
      ? { status: 400, body: { error: "membership_held", error_description: HELD } }
      : { body: accepted(`token-${count}`, 600) }
  );
  const clock = { now: Date.now() };
  const client = new BrokerTokenClient({
    brokerUrl: broker.url,
    key: await newDpopKey(),
    subject: {
      read: async () => ({ token: "subject", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
      describe: () => "a scripted identity",
    },
    renewBeforeMs: 60_000,
    now: () => clock.now,
  });
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
    expect(lapsed).toBeInstanceOf(BrokerRefusedError);
    expect((lapsed as BrokerRefusedError).code).toBe("membership_held");
    expect(isFinalRefusal(lapsed), "the lapsed token's membership_held read as final").toBe(false);

    held = false;
    expect((await client.token()).accessToken).toBe("token-4");
  } finally {
    client.close();
    broker.stop();
  }
});
