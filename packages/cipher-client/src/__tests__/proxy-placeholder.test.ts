// packages/cipher-client/src/__tests__/proxy-placeholder.test.ts
//
// The trade of a person's sign-in for a placeholder of Cipher's proxy (`proxyPlaceholder`): the
// one exchange that asks for no access token, whose answer is a placeholder the broker's proxy
// swaps a token of its own for, and how the session reaches that proxy. The client reads that
// answer strictly and never takes anything else for a placeholder. A 503 with `Retry-After`, the
// broker's own answer through a key set that cannot be read (`keySetOutage`), is waited out on
// the client's injected clock, so no row sleeps.

import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { until } from "@legion/test-fixtures/wait";
import { newDpopKey } from "../dpop";
import type { SubjectToken, TokenSource } from "../source";
import { BrokerTokenClient, type BrokerTokenClientOptions } from "../token-client";
import { BrokerRefusedError, BrokerTradeFailedError, isFinalRefusal } from "../token-errors";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  FORM_REQUESTED_TOKEN_TYPE,
  GRANT_TYPE_TOKEN_EXCHANGE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_PERSON,
  TOKEN_TYPE_PROXY_PLACEHOLDER,
} from "../wire";
import { keySetOutage, refusalOf, stubBroker } from "./stub";

const PERSON = "ada@example.com";
const PLACEHOLDER = `agent-secrets-placeholder-approve-${"A".repeat(32)}`;
const AUTHORITY = "-----BEGIN CERTIFICATE-----\nproxy\n-----END CERTIFICATE-----\n";

/** A source answering `token` under `tokenType` at every read. */
function fixed(token: string, tokenType: string, person?: string): TokenSource {
  return {
    read: async (): Promise<SubjectToken> => ({ token, tokenType }),
    describe: () => `a scripted ${tokenType}`,
    ...(person === undefined ? {} : { person }),
  };
}

/** The broker's answer to a placeholder trade, with `change` applied to it. */
function placeholderAnswer(
  change: (answer: Record<string, unknown>) => void = () => {}
): Record<string, unknown> {
  const answer: Record<string, unknown> = {
    access_token: PLACEHOLDER,
    issued_token_type: TOKEN_TYPE_PROXY_PLACEHOLDER,
    token_type: "N_A",
    expires_in: 900,
    grant_id: "approve:5f070414-1a7c-43a3-8818-cf76eded9806",
    proxy: {
      proxy_url: "http://proxy.example:3128",
      ca_certificate: AUTHORITY,
      proxy_password: PLACEHOLDER,
      names: {
        DISPATCH_APPROVE_TOKEN: {
          value: PLACEHOLDER,
          slots: { value: PLACEHOLDER },
          hosts: ["dispatch.example"],
        },
      },
    },
  };
  change(answer);
  return answer;
}

/** A person's client of the broker at `url`: their identity token as the subject, the machine's as
 *  the actor, under `session`, on the clock and the waits of `seams` where a row gives them. */
async function personClient(
  url: string,
  session = "ada-box-1",
  seams: Pick<BrokerTokenClientOptions, "now" | "sleep"> = {}
): Promise<BrokerTokenClient> {
  return new BrokerTokenClient({
    brokerUrl: url,
    key: await newDpopKey(),
    subject: fixed(fixtureSecret("person"), SUBJECT_TOKEN_TYPE_PERSON, PERSON),
    actor: fixed(fixtureSecret("machine"), SUBJECT_TOKEN_TYPE_GOOGLE),
    session,
    log: () => {},
    ...seams,
  });
}

// The trade goes through openid-client, as every trade of this client does, so this answer is also
// the library's reading of a token endpoint answer whose token_type is N_A (RFC 8693 Section
// 2.2.1, the type of a token that is no access token).
test("a sign-in is traded for the proxy placeholder and how the session reaches the proxy", async () => {
  const broker = stubBroker(() => ({ body: placeholderAnswer() }));
  try {
    const client = await personClient(broker.url);
    const held = await client.proxyPlaceholder();
    expect(held).toEqual({
      placeholder: PLACEHOLDER,
      proxyUrl: "http://proxy.example:3128",
      caCertificate: AUTHORITY,
      names: { DISPATCH_APPROVE_TOKEN: { hosts: ["dispatch.example"] } },
    });

    expect(broker.trades).toHaveLength(1);
    const form = broker.trades[0]?.form;
    expect(form?.get(FORM_REQUESTED_TOKEN_TYPE)).toBe(TOKEN_TYPE_PROXY_PLACEHOLDER);
    expect(form?.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
    expect(form?.get("actor_token_type")).toBe(ACTOR_TOKEN_TYPE_MACHINE);
    expect(form?.get("session")).toBe("ada-box-1");
    expect(form?.get("grant_type")).toBe(GRANT_TYPE_TOKEN_EXCHANGE);
    expect(broker.trades[0]?.proofClaims["htm"]).toBe("POST");
  } finally {
    broker.stop();
  }
});

test("an answer that is not a proxy placeholder is refused and never handed out", async () => {
  const rows: Record<string, (answer: Record<string, unknown>) => void> = {
    "an access token's type": (answer) => {
      answer["issued_token_type"] = "urn:ietf:params:oauth:token-type:access_token";
    },
    "a DPoP token_type": (answer) => {
      answer["token_type"] = "DPoP";
    },
    "no expires_in": (answer) => {
      delete answer["expires_in"];
    },
    "a placeholder without the placeholder prefix": (answer) => {
      answer["access_token"] = "a-real-looking-token";
    },
    "a refresh token": (answer) => {
      answer["refresh_token"] = "never-issued";
    },
    "no grant id": (answer) => {
      delete answer["grant_id"];
    },
    "no proxy": (answer) => {
      delete answer["proxy"];
    },
    "a proxy password other than the placeholder": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["proxy_password"] = "another";
    },
    "a proxy URL that is no http URL": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["proxy_url"] = "ftp://proxy.example";
    },
    "no authority": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["ca_certificate"] = "";
    },
    "a name with no host": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["names"] = {
        DISPATCH_APPROVE_TOKEN: { value: PLACEHOLDER, hosts: [] },
      };
    },
    "a name whose value is not the placeholder": (answer) => {
      (answer["proxy"] as Record<string, unknown>)["names"] = {
        DISPATCH_APPROVE_TOKEN: { value: "another", hosts: ["dispatch.example"] },
      };
    },
  };
  for (const [row, change] of Object.entries(rows)) {
    const broker = stubBroker(() => ({ body: placeholderAnswer(change) }));
    try {
      const refusal = await refusalOf(
        async () => await (await personClient(broker.url)).proxyPlaceholder()
      );
      if (!(refusal instanceof BrokerTradeFailedError)) {
        throw new Error(`${row}: answered ${String(refusal)}; want BrokerTradeFailedError`);
      }
      expect(refusal.message).not.toContain(PLACEHOLDER);
    } finally {
      broker.stop();
    }
  }
});

test("a broker's refusal of the placeholder trade is thrown as the trade's refusal", async () => {
  const broker = stubBroker(() => ({
    status: 400,
    body: {
      error: "invalid_request",
      error_description:
        "Only a person's sign-in is traded for the approve capability's proxy placeholder, on a broker that serves it.",
    },
  }));
  try {
    const refusal = await refusalOf(
      async () => await (await personClient(broker.url)).proxyPlaceholder()
    );
    expect(refusal).not.toBeNull();
    expect(String(refusal)).toContain("invalid_request");
  } finally {
    broker.stop();
  }
});

/** The broker's answer to a trade whose subject or actor token it could not check, because the key
 *  set of that token's issuer could not be read: 503 with `Retry-After: 5`, as its real server
 *  wrote it. */
const KEY_SET_OUTAGE = await keySetOutage();

/** A clock a row moves, starting where these rows' calls start. */
const OUTAGE_START = 1_700_000_000_000;

/** Waits that pass at once, each moving `clock` by its length, kept in `waits`. */
function passingWaits(clock: { now: number }, waits: number[]): BrokerTokenClientOptions["sleep"] {
  return async (ms: number) => {
    waits.push(ms);
    clock.now += ms;
  };
}

/** Fails unless `error` is the broker's outage through a key set that cannot be read as a call
 *  that stopped trying answers it: never a refusal, and in words that name the outage, the
 *  status and how long the call tried. */
function expectOutage(error: Error | null): void {
  expect(error).toBeInstanceOf(BrokerTradeFailedError);
  expect(error).not.toBeInstanceOf(BrokerRefusedError);
  expect(isFinalRefusal(error)).toBe(false);
  expect((error as BrokerTradeFailedError).retryAfterMs).toBe(5_000);
  for (const words of [
    `could not check a scripted ${SUBJECT_TOKEN_TYPE_PERSON}, because its issuer's key set could not be read`,
    "HTTP 503 temporarily_unavailable",
    "; tried for ",
  ]) {
    expect(error?.message).toContain(words);
  }
}

test("Retry-After: a placeholder answered 503 asking 5 s is traded again after 5 s", async () => {
  const clock = { now: OUTAGE_START };
  const arrivals: number[] = [];
  const broker = stubBroker((_trade, count) => {
    arrivals.push(clock.now);
    return count === 1 ? KEY_SET_OUTAGE : { body: placeholderAnswer() };
  });
  const waits: number[] = [];
  try {
    const client = await personClient(broker.url, "ada-box-1", {
      now: () => clock.now,
      sleep: passingWaits(clock, waits),
    });
    expect((await client.proxyPlaceholder()).placeholder).toBe(PLACEHOLDER);
    expect(waits).toEqual([5_000]);
    expect(arrivals).toEqual([OUTAGE_START, OUTAGE_START + 5_000]);
  } finally {
    broker.stop();
  }
});

test("Retry-After: with no signal, a broker that answers 503 every time fails within 2 minutes naming the outage", async () => {
  const clock = { now: OUTAGE_START };
  const arrivals: number[] = [];
  const broker = stubBroker(() => {
    arrivals.push(clock.now);
    return KEY_SET_OUTAGE;
  });
  const waits: number[] = [];
  try {
    const client = await personClient(broker.url, "ada-box-1", {
      now: () => clock.now,
      sleep: passingWaits(clock, waits),
    });
    const error = await refusalOf(() => client.proxyPlaceholder());
    expectOutage(error);
    expect(error?.message).toContain("a call with no signal tries for");
    expect(clock.now - OUTAGE_START).toBeLessThanOrEqual(120_000);
    // A trade 5 s after each 503, the wait each asked for, until no 5 s is left inside the
    // 2 minutes.
    expect(arrivals).toEqual(Array.from({ length: 24 }, (_, n) => OUTAGE_START + n * 5_000));
    expect(waits.every((ms) => ms === 5_000)).toBe(true);
  } finally {
    broker.stop();
  }
});

test("Retry-After: with a signal, the wait ends when the signal does", async () => {
  const clock = { now: OUTAGE_START };
  const broker = stubBroker(() => KEY_SET_OUTAGE);
  // The wait holds until the call's own signal ends it, moving no clock.
  const waits: number[] = [];
  const held = (ms: number, signal?: AbortSignal): Promise<void> => {
    waits.push(ms);
    const { promise, resolve } = Promise.withResolvers<void>();
    signal?.addEventListener("abort", () => resolve(), { once: true });
    return promise;
  };
  try {
    const client = await personClient(broker.url, "ada-box-1", {
      now: () => clock.now,
      sleep: held,
    });
    const life = new AbortController();
    const call = refusalOf(() => client.proxyPlaceholder(life.signal));
    await until("the call waiting out the 503's Retry-After", () => waits.length === 1);
    expect(waits).toEqual([5_000]);
    life.abort();
    const error = await call;
    expectOutage(error);
    expect(error?.message).toContain("until the call's signal ended");
    expect(broker.trades).toHaveLength(1);
  } finally {
    broker.stop();
  }
});
