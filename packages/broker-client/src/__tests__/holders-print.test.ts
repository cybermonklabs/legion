// packages/broker-client/src/__tests__/holders-print.test.ts
//
// The box print test of this package's holders (@legion/test-fixtures/print-test): each holds the
// probe in every box field, printed by every printer at every placement, beside its twins, the
// same shape with the probe as a string there.

import { afterAll, expect, test } from "bun:test";
import { type Holder, printTest, REMOTE_WORDS } from "@legion/test-fixtures/print-test";
import { newDpopKey } from "../dpop";
import type { MadeBrokerClient } from "../environment";
import type { TokenSource } from "../source";
import { BrokerTokenClient } from "../token-client";
import { BrokerRefusedError } from "../token-errors";
import { POD_SESSION_NOT_REGISTERED, SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { stubBroker } from "./stub";

const SCRIPTED: TokenSource = {
  read: async () => ({ token: "projected", tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES }),
  describe: () => "a scripted identity",
};

/** A refusal's twin: an Error whose box fields are `#` fields, and whose message quotes them as
 *  they came. */
class RefusedTwin extends Error {
  readonly #code: string;
  readonly #description: string;
  constructor(
    readonly status: number,
    code: string,
    description: string
  ) {
    super(`the secrets broker refused a scripted identity: HTTP ${status} ${code}: ${description}`);
    this.name = "BrokerRefusedError";
    this.#code = code;
    this.#description = description;
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

const description = `${POD_SESSION_NOT_REGISTERED}: ${REMOTE_WORDS}`;

/** A client that holds, as its pending refusal, the broker's refusal of an unregistered pod whose
 *  words quote the probe: it traded once, was refused, and sleeps until closed. */
const broker = stubBroker(() => ({
  status: 400,
  body: { error: "invalid_grant", error_description: description },
}));
const slept = Promise.withResolvers<void>();
const pending = new BrokerTokenClient({
  brokerUrl: broker.url,
  key: await newDpopKey(),
  subject: SCRIPTED,
  session: "session-1",
  log: () => {},
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
  broker.stop();
});

const holders: Holder[] = [
  {
    name: "broker-client.BrokerRefusedError",
    kind: "error",
    boxed: new BrokerRefusedError(400, REMOTE_WORDS, description, "a scripted identity"),
    same: new RefusedTwin(400, REMOTE_WORDS, description),
  },
  {
    name: "broker-client.BrokerTokenClient",
    kind: "class",
    boxed: pending,
    same: new TokenClientTwin(new RefusedTwin(400, "invalid_grant", description)),
  },
  {
    name: "broker-client.MadeBrokerClient",
    kind: "object",
    boxed: { client: pending, signIn: undefined } satisfies MadeBrokerClient,
    same: {
      client: new TokenClientTwin(new RefusedTwin(400, "invalid_grant", description)),
      signIn: undefined,
    },
  },
];

for (const holder of holders) {
  test(`box print test: ${holder.name} prints no secret by any printer at any placement`, () => {
    expect(printTest(holder).map((f) => f.why)).toEqual([]);
  });
}
