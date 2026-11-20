// packages/envoy-client/src/__tests__/holders-print.test.ts
//
// The box print test of this package's holders (@legion/test-fixtures/print-test): each holds the
// probe in every box field, printed by every printer at every placement, beside its twin, the same
// shape with the probe as a string there. broker-credential.ts's Made and Held are its own; this
// test builds values of their shapes.

import { afterAll, expect, test } from "bun:test";
import { newDpopKey } from "@legion/broker-client/dpop";
import type { TokenSource } from "@legion/broker-client/source";
import { BrokerTokenClient } from "@legion/broker-client/token-client";
import { BrokerRefusedError } from "@legion/broker-client/token-errors";
import {
  POD_SESSION_NOT_REGISTERED,
  SUBJECT_TOKEN_TYPE_KUBERNETES,
} from "@legion/broker-client/wire";
import { type Holder, printTest, REMOTE_WORDS } from "@legion/test-fixtures/print-test";

const SCRIPTED: TokenSource = {
  read: async () => ({ token: "projected", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
  describe: () => "a scripted identity",
};

const description = `${POD_SESSION_NOT_REGISTERED}: ${REMOTE_WORDS}`;

/** A refusal's twin: an Error whose box fields are `#` fields, and whose message quotes them as
 *  they came. */
class RefusedTwin extends Error {
  readonly #code: string;
  readonly #description: string;
  constructor(
    readonly status: number,
    code: string,
    words: string
  ) {
    super(`the secrets broker refused a scripted identity: HTTP ${status} ${code}: ${words}`);
    this.name = "BrokerRefusedError";
    this.#code = code;
    this.#description = words;
  }
  get said(): string {
    return this.#code + this.#description;
  }
}

/** A client's twin: an object whose `#pending` field holds the refusal's twin. */
class TokenClientTwin {
  readonly #pending: RefusedTwin;
  constructor(pending: RefusedTwin) {
    this.#pending = pending;
  }
  get pending(): RefusedTwin {
    return this.#pending;
  }
}

/** A client that holds, as its pending refusal, the broker's refusal of an unregistered pod whose
 *  words quote the probe: it traded once, was refused, and sleeps until closed. */
const slept = Promise.withResolvers<void>();
const pending = new BrokerTokenClient({
  brokerUrl: "http://127.0.0.1:1",
  key: await newDpopKey(),
  subject: SCRIPTED,
  session: "session-1",
  log: () => {},
  fetch: (async () =>
    Response.json(
      { error: "invalid_grant", error_description: description },
      { status: 400 }
    )) as unknown as typeof fetch,
  sleep: (_ms, signal) => {
    slept.resolve();
    const { promise, resolve } = Promise.withResolvers<void>();
    signal?.addEventListener("abort", () => resolve(), { once: true });
    return promise;
  },
});
const trading = pending.token().catch(() => undefined);
await slept.promise;
afterAll(async () => {
  pending.close();
  await trading;
});

const twinClient = (): TokenClientTwin =>
  new TokenClientTwin(new RefusedTwin(400, "invalid_grant", description));
const refusal = new BrokerRefusedError(400, "invalid_grant", REMOTE_WORDS, "a person's sign-in");
const refusalTwin = new RefusedTwin(400, "invalid_grant", REMOTE_WORDS);

const holders: Holder[] = [
  {
    name: "envoy-client.Made",
    kind: "object",
    boxed: { client: pending, signIn: "person@example.com" },
    same: { client: twinClient(), signIn: "person@example.com" },
  },
  {
    name: "envoy-client.Held",
    kind: "object",
    boxed: {
      making: undefined,
      made: { client: pending, signIn: "person@example.com" },
      refused: new Map<string, unknown>([["person@example.com", refusal]]),
    },
    same: {
      making: undefined,
      made: { client: twinClient(), signIn: "person@example.com" },
      refused: new Map<string, unknown>([["person@example.com", refusalTwin]]),
    },
  },
];

for (const holder of holders) {
  test(`box print test: ${holder.name} prints no secret by any printer at any placement`, () => {
    expect(printTest(holder).map((f) => f.why)).toEqual([]);
  });
}
