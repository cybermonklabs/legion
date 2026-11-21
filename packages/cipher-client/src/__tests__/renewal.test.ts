// packages/cipher-client/src/__tests__/renewal.test.ts
//
// What the client does between trades: it holds a token while that token is live, trades again
// before it lapses with nothing of the caller's restarting, keeps writing through a broker or an
// issuer that is merely down while its retry loop trades again, tells a caller whose deadline ends
// what that loop's last trade came to rather than handing out no identity, stops with a plain
// error on one that has been ended, and trades afresh when a caller is told the token in hand is
// dead. The loop's whole table of answers is `retry-loop.test.ts`.

import { expect, test } from "bun:test";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { settled, until } from "@legion/test-fixtures/wait";
import { signedIn, stubIssuer } from "../../test/sign-ins";
import { newDpopKey } from "../dpop";
import { personIdentity } from "../person-identity";
import { SIGNIN_COMMAND } from "../signins";
import type { SubjectToken, TokenSource } from "../source";
import { BrokerTokenClient } from "../token-client";
import {
  BrokerRefusedError,
  isFinalRefusal,
  SignInEndedError,
  TradeRetryingError,
} from "../token-errors";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, stubBroker } from "./stub";

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
const CLIENT_SECRET = fixtureSecret("client");

/** A source that answers a token of its own at each read, so a renewal is visibly a fresh one.
 *  `tokens` is each token it answered, in order. */
function countingSource(): TokenSource & { tokens: string[] } {
  const source = {
    tokens: [] as string[],
    read: async (): Promise<SubjectToken> => {
      const token = fixtureSecret("subject");
      source.tokens.push(token);
      return { token, tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES };
    },
    describe: () => "a scripted identity",
  };
  return source;
}

/** A client of `broker` whose clock this test moves. Its retry loop logs nowhere, and waits on
 *  `sleep` where a row gives one. */
async function clientAt(
  url: string,
  clock: { now: number },
  source: TokenSource,
  renewBeforeMs = 60_000,
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>
): Promise<BrokerTokenClient> {
  return new BrokerTokenClient({
    brokerUrl: url,
    key: await newDpopKey(),
    subject: source,
    renewBeforeMs,
    now: () => clock.now,
    random: () => 1,
    log: () => {},
    ...(sleep === undefined ? {} : { sleep }),
  });
}

/** What `call` rejected with, or null where it resolved. */
async function told(call: Promise<unknown>): Promise<unknown> {
  return await call.then(
    () => null,
    (error: unknown) => error
  );
}

/** What a fail-fast caller of `client` is told, which the retry loop tells it as soon as the trade
 *  it waits on has failed: what that trade came to. Waited for through the shared wait, so a call
 *  that never answers fails at the row's deadline, naming it. */
function answerOf(client: BrokerTokenClient): Promise<unknown> {
  return told(settled("the retry loop's answer to a fail-fast caller", client.token()));
}

test("the token in hand is reused until the renewal window, then traded again", async () => {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`token-${count}`, 600) }));
  const clock = { now: Date.now() };
  const source = countingSource();
  const client = await clientAt(broker.url, clock, source);
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    // Well inside the token's life: the same token, and no second trade.
    clock.now += 500_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(1);
    // Inside the renewal window: a fresh subject token traded for a fresh access token, with
    // nothing of the caller's restarted.
    clock.now += 60_000;
    expect((await client.token()).accessToken).toBe("token-2");
    expect(broker.trades.length).toBe(2);
    expect(broker.trades[1]?.form.get("subject_token")).toBe(source.tokens[1] as string);
    expect(source.tokens).toHaveLength(2);
  } finally {
    broker.stop();
  }
});

test("two callers that need a token at once make one trade", async () => {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`token-${count}`, 600) }));
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    const [first, second] = await Promise.all([client.token(), client.token()]);
    expect(first.accessToken).toBe("token-1");
    expect(second.accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(1);
  } finally {
    broker.stop();
  }
});

test("a broker that does not answer leaves the token in hand working until it lapses", async () => {
  const broker = stubBroker((_trade, count) =>
    count === 1 ? { body: accepted("token-1", 600) } : { status: 503, body: { error: "down" } }
  );
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    // In the renewal window with the broker down: the token in hand is still live, so the caller
    // keeps writing with it.
    clock.now += 560_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(2);
    // Past its expiry there is nothing live left, so the caller is told what the retry loop's
    // last trade came to, rather than writing with no identity.
    clock.now += 60_000;
    const refusal = await answerOf(client);
    expect(refusal).toBeInstanceOf(TradeRetryingError);
    expect((refusal as Error).message).toContain("503");
    expect(isFinalRefusal(refusal)).toBe(false);
  } finally {
    client.close();
    broker.stop();
  }
});

// A failed trade's text names the broker and what went wrong, and a caller prints it: a plain
// broker URL by itself, and one that carries user info (AGENT_SECRETS_URL can) by its fixed phrase
// alone, no part of it; and no error keeps the library's as its cause, since the body the library
// read hangs off it. The retry loop trades again on such an answer, so a fail-fast caller is told
// the text once the trade failed.
test.each([
  // The library refuses a 200 with no token_type, and hangs the body it read off its error.
  ["the library refuses", (token: string) => ({ access_token: token, expires_in: 600 })],
  // This client refuses a token_type that is not DPoP, naming the broker it asked.
  [
    "this client refuses",
    (token: string) => ({
      access_token: token,
      issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
      token_type: "Bearer",
      expires_in: 600,
    }),
  ],
] as const)("an answer %s names the broker only where its URL is plain, and keeps no cause", async (_, body) => {
  const password = fixtureSecret("pw");
  const leaked = `canary-token-${crypto.randomUUID()}`;
  const broker = stubBroker(() => ({ body: body(leaked) }));
  const withUser = broker.url.replace("http://", `http://operator:${password}@`);
  try {
    for (const [url, named] of [
      [broker.url, `${broker.url}/`],
      [withUser, `the broker URL ${URL_NOT_PRINTED}`],
    ] as const) {
      const client = await clientAt(url, { now: Date.now() }, countingSource());
      const refusal = await answerOf(client);
      client.close();
      if (!(refusal instanceof TradeRetryingError)) throw new Error("the trade was answered");
      expect(refusal.message).toContain(named);
      expect(refusal.cause).toBeUndefined();
      expect(Bun.inspect(refusal)).not.toContain(leaked);
      expect(Bun.inspect(refusal)).not.toContain(password);
    }
  } finally {
    broker.stop();
  }
});

// A broker URL that is no http or https URL may be a token pasted where the URL goes, so the
// client refuses it before any trade and names no part of it.
test("a token pasted as the broker's URL is refused, and named in no part", async () => {
  const pasted = `ya29.canary-pasted-${crypto.randomUUID()}`;
  const refusal = await clientAt(pasted, { now: Date.now() }, countingSource()).then(
    () => undefined,
    (error: unknown) => error
  );
  if (!(refusal instanceof Error)) throw new Error("the client was made");
  expect(refusal.message).toBe("broker client: the broker URL is not an http or https URL");
  expect(Bun.inspect(refusal)).not.toContain(pasted);
});

// A trade that got no answer is named by the failure's class: the runtime's message repeats the
// host it was handed, and the broker is named by `urlForText` already.
test("a trade that got no answer names the failure by its class", async () => {
  const client = await clientAt("http://127.0.0.1:1", { now: Date.now() }, countingSource());
  const refusal = await answerOf(client);
  client.close();
  if (!(refusal instanceof TradeRetryingError)) throw new Error("the trade was answered");
  expect(refusal.message).toBe(
    "trade a scripted identity at http://127.0.0.1:1/v1/oauth/token: ECONNREFUSED, and the trade is being retried in the background"
  );
  expect(refusal.cause).toBeUndefined();
});

test("a refusal of the identity stops at once, whatever is in hand", async () => {
  const broker = stubBroker((_trade, count) =>
    count === 1
      ? { body: accepted("token-1", 600) }
      : {
          status: 400,
          body: {
            error: "invalid_grant",
            error_description: "no entry lists the account this token names",
          },
        }
  );
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    clock.now += 560_000;
    const refusal = await client.token().then(
      () => null,
      (error: unknown) => error
    );
    expect(refusal).toBeInstanceOf(BrokerRefusedError);
    // It names what was traded and what the broker said: a known refusal as the broker wrote it
    // (`KNOWN_BROKER_REFUSALS`).
    expect((refusal as Error).message).toBe(
      "the secrets broker refused a scripted identity: HTTP 400 invalid_grant: no entry lists the account this token names"
    );
    // Nothing is held after a refusal: a call before the retry loop's next slot is told the same
    // refusal, with no trade, rather than handed a token the broker has disowned.
    expect(client.token()).rejects.toThrow("no entry lists the account this token names");
    expect(broker.trades.length).toBe(2);
  } finally {
    client.close();
    broker.stop();
  }
});

test("the broker's rate limit leaves the token in hand working until it lapses", async () => {
  // The broker's token endpoint spends a bucket per source address, and a bucket refills: a 429
  // is one of the refusals a later trade may be answered differently, so a client that holds a
  // live token keeps writing with it rather than failing every call through the renewal window.
  const broker = stubBroker((_trade, count) =>
    count === 1
      ? { body: accepted("token-1", 600) }
      : {
          status: 429,
          body: {
            error: "invalid_request",
            error_description: "too many token requests; try again later",
          },
        }
  );
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    clock.now += 560_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades.length).toBe(2);
    // Past its expiry there is nothing live left, so the caller is told rather than writing with
    // no identity, and what it is told says the trade goes on.
    clock.now += 60_000;
    const refusal = await answerOf(client);
    expect(refusal).toBeInstanceOf(TradeRetryingError);
    expect(isFinalRefusal(refusal)).toBe(false);
    expect((refusal as Error).message).toContain("too many token requests; try again later");
  } finally {
    client.close();
    broker.stop();
  }
});

test("a broker asking for a nonce is answered with one in the next proof", async () => {
  const broker = stubBroker((trade, count) => {
    if (count === 1) {
      return { status: 400, body: { error: "use_dpop_nonce" }, nonce: "nonce-from-the-broker" };
    }
    return trade.proofClaims.nonce === "nonce-from-the-broker"
      ? { body: accepted("token-after-the-nonce", 600) }
      : { status: 400, body: { error: "use_dpop_nonce" }, nonce: "nonce-from-the-broker" };
  });
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-after-the-nonce");
    expect(broker.trades.length).toBe(2);
    expect(broker.trades[0]?.proofClaims.nonce).toBeUndefined();
  } finally {
    broker.stop();
  }
});

test("a final refusal is the broker's permanent one or a sign-in the issuer ended", () => {
  const identity = "a scripted identity";
  expect(isFinalRefusal(new BrokerRefusedError(400, "invalid_grant", "revoked", identity))).toBe(
    true
  );
  expect(isFinalRefusal(new BrokerRefusedError(429, "invalid_request", "", identity))).toBe(false);
  expect(isFinalRefusal(new BrokerRefusedError(400, "invalid_dpop_proof", "", identity))).toBe(
    false
  );
  expect(isFinalRefusal(new SignInEndedError("https://accounts.example", PERSON))).toBe(true);
  expect(isFinalRefusal(new Error("the broker answered HTTP 503"))).toBe(false);
});

test("a sign-in the issuer ended drops the token in hand, while an issuer that is down keeps it", async () => {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`token-${count}`, 600) }));
  const issuer = await stubIssuer({
    clientId: CLIENT_ID,
    person: PERSON,
    answer: (count) => {
      if (count === 2 || count === 4) return { status: 503, body: "Service Unavailable" };
      if (count === 3) {
        return {
          status: 400,
          body: { error: "invalid_grant", error_description: "Token has been expired or revoked." },
        };
      }
      return undefined;
    },
  });
  const clock = { now: Date.now() };
  const source = personIdentity(await signedIn(PERSON, { client_id: CLIENT_ID }), {
    issuer: issuer.url,
    clientId: CLIENT_ID,
    clientSecret: CLIENT_SECRET,
  });
  // The retry loop's waits pass only when this row lets them, so what follows a failed renewal is
  // the loop's next trade, at its slot.
  const held: (() => void)[] = [];
  const sleep = (_ms: number, signal?: AbortSignal): Promise<void> =>
    new Promise<void>((resolve) => {
      held.push(resolve);
      signal?.addEventListener("abort", () => resolve(), { once: true });
    });
  const client = await clientAt(broker.url, clock, source, 60_000, sleep);
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    expect(broker.trades[0]?.form.get("subject_token")).toBe(issuer.idTokens[0] as string);
    // In the renewal window with the issuer down: the token in hand is still live, so the caller
    // keeps writing with it, and goes on doing so while the loop waits for its slot.
    clock.now += 560_000;
    expect((await client.token()).accessToken).toBe("token-1");
    expect((await client.token()).accessToken).toBe("token-1");
    // At the slot, the issuer no longer honours the sign-in: that is final, whatever is in hand.
    held.shift()?.();
    const ended = await until("a caller told that the sign-in ended", () => told(client.token()));
    expect(ended).toBeInstanceOf(SignInEndedError);
    expect((ended as Error).message).toContain(SIGNIN_COMMAND);
    // Nothing is held after it: past the slot, with the issuer down again, the caller is told
    // what the trade came to rather than handed the token the ended sign-in traded for.
    clock.now += 60_000;
    const down = await answerOf(client);
    expect(down).toBeInstanceOf(TradeRetryingError);
    expect((down as Error).message).toContain("HTTP 503");
    expect(broker.trades.length).toBe(1);
  } finally {
    client.close();
    broker.stop();
    issuer.stop();
  }
});

// The refusal names the person's sign-in; the broker's text is not one of its known refusals, so
// it is withheld (`KNOWN_BROKER_REFUSALS`).
test("a broker's refusal of a person's trade names their sign-in", async () => {
  const broker = stubBroker(() => ({
    status: 400,
    body: {
      error: "invalid_grant",
      error_description: `${PERSON} revoked this machine's sign-in`,
    },
  }));
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON });
  const source = personIdentity(await signedIn(PERSON), {
    issuer: issuer.url,
    clientId: CLIENT_ID,
    clientSecret: CLIENT_SECRET,
  });
  const client = await clientAt(broker.url, { now: Date.now() }, source);
  try {
    await expect(client.token()).rejects.toThrow(
      `the secrets broker refused ${PERSON}'s sign-in on this machine: HTTP 400 invalid_grant: (its text is withheld)`
    );
  } finally {
    broker.stop();
    issuer.stop();
  }
});

test("drop() makes the next token() trade, and leaves a trade under way alone", async () => {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`token-${count}`, 600) }));
  const clock = { now: Date.now() };
  const client = await clientAt(broker.url, clock, countingSource());
  try {
    expect((await client.token()).accessToken).toBe("token-1");
    // Well inside the token's life, a caller told the token is dead drops it, and the next call
    // trades rather than answering it again.
    client.drop();
    expect((await client.token()).accessToken).toBe("token-2");
    expect(broker.trades.length).toBe(2);
    // A drop while a trade is under way leaves that trade alone: its token is a fresh one, and a
    // caller arriving after the drop waits for it rather than trading a second time.
    clock.now += 560_000;
    const trading = client.token();
    client.drop();
    const [first, second] = await Promise.all([trading, client.token()]);
    expect(first.accessToken).toBe("token-3");
    expect(second.accessToken).toBe("token-3");
    expect((await client.token()).accessToken).toBe("token-3");
    expect(broker.trades.length).toBe(3);
  } finally {
    broker.stop();
  }
});
