// packages/envoy-client/src/__tests__/holders-print.test.ts
//
// The box print test of this package's holders (@legion/test-fixtures/print-test): each holds the
// probe in every box field, printed by every printer at every placement, beside its twin, the same
// shape with the probe as a string there. broker-credential.ts's Made and Held are its own; this
// test builds values of their shapes. The listener's refusal (`EnvoyApiError`) is built from an
// answer whose every word quotes the probe, and Dispatch's refusals (`DispatchServiceError` and the
// gateway answer and no-route refusal that are its kin) as the client builds them from such an
// answer.

import { afterAll, expect, test } from "bun:test";
import { newDpopKey } from "@legion/cipher-client/dpop";
import type { TokenSource } from "@legion/cipher-client/source";
import { BrokerTokenClient } from "@legion/cipher-client/token-client";
import { BrokerRefusedError } from "@legion/cipher-client/token-errors";
import {
  POD_SESSION_NOT_REGISTERED,
  SUBJECT_TOKEN_TYPE_KUBERNETES,
} from "@legion/cipher-client/wire";
import { type Holder, printTest, REMOTE_WORDS } from "@legion/test-fixtures/print-test";
import { bearerCredential } from "../bearer";
import { DispatchClient, DispatchNoRouteError } from "../dispatch-http";
import { EnvoyApiError } from "../transport";

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

/** The listener's refusal whose every word quotes the probe. */
const listenerRefusal = {
  error: REMOTE_WORDS,
  code: REMOTE_WORDS,
  reason: REMOTE_WORDS,
  expected: [REMOTE_WORDS],
  refused: [REMOTE_WORDS],
  holder: REMOTE_WORDS,
  role: REMOTE_WORDS,
  holder_state: "unresponsive",
};
const envoyRefusal = new EnvoyApiError({
  method: "GET",
  url: "http://listener/v1/interests/ses_sender",
  status: 403,
  responseBody: JSON.stringify(listenerRefusal),
});

/** `EnvoyApiError`'s twin: an Error whose words are public strings, as they came, and whose
 *  message quotes them. */
class EnvoyApiErrorTwin extends Error {
  override readonly name = "EnvoyApiError";
  readonly details = {
    method: "GET",
    url: "http://listener/v1/interests/ses_sender",
    status: 403,
    responseBody: JSON.stringify(listenerRefusal),
  };
  readonly expected = [REMOTE_WORDS];
  readonly code = REMOTE_WORDS;
  readonly reason = REMOTE_WORDS;
  readonly holder = REMOTE_WORDS;
  readonly sessionState = "unresponsive";
  readonly refused = [REMOTE_WORDS];
  readonly roleHeld = { role: REMOTE_WORDS, holder: REMOTE_WORDS };
  constructor() {
    super(`${REMOTE_WORDS} (reason: ${REMOTE_WORDS}) Holder ${REMOTE_WORDS} is unresponsive.`);
  }
}

/** The error a DispatchClient read throws when Dispatch, or something in its place, answers
 *  `answer`. */
async function dispatchRefusal(answer: () => Response): Promise<unknown> {
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), (async () =>
    answer()) as unknown as typeof fetch);
  return client.listProjects().catch((caught: unknown) => caught);
}

/** A refusal's data whose every word quotes the probe: candidates, current tokens, mismatches. */
const refusalData = {
  candidates: [{ from: 1, to: 2, context: REMOTE_WORDS }],
  current: { document: REMOTE_WORDS, blocks: [{ id: "block-1", token: REMOTE_WORDS }] },
  mismatches: [{ scope: "block", block_id: "block-1", expected: REMOTE_WORDS }],
};
const serviceRefusal = await dispatchRefusal(() =>
  Response.json({ error: REMOTE_WORDS, code: REMOTE_WORDS, ...refusalData }, { status: 403 })
);
const gatewayRefusal = await dispatchRefusal(
  () =>
    new Response(`<html><body><p>${REMOTE_WORDS}</p></body></html>`, {
      status: 403,
      headers: { "Content-Type": "text/html" },
    })
);

/** `DispatchServiceError`'s twin: an Error whose code is a public string, as it came, and whose
 *  message is Dispatch's words as they came. */
class DispatchServiceErrorTwin extends Error {
  override readonly name = "DispatchServiceError";
  readonly fromDispatch: boolean = true;
  readonly code = REMOTE_WORDS;
  readonly status = 403;
  constructor() {
    super(REMOTE_WORDS);
  }
}

/** `DispatchGatewayError`'s twin: the page's words, as they came, in its answer and message. */
class DispatchGatewayErrorTwin extends DispatchServiceErrorTwin {
  override readonly fromDispatch = false;
  readonly answer = `GET http://dispatch.test/api/v1/projects answered 403 with ${REMOTE_WORDS}`;
  readonly advice = "asking again will not change the answer";
  readonly transient = false;
  readonly mayHaveReachedDispatch = false;
}

/** `DispatchServiceError`'s twin with its data: the candidates, current tokens and mismatches as
 *  they came. */
class DispatchRefusalTwin extends DispatchServiceErrorTwin {
  readonly candidates = refusalData.candidates;
  readonly current = refusalData.current;
  readonly mismatches = refusalData.mismatches;
}

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
  {
    name: "envoy-client.EnvoyApiErrorDetails",
    kind: "object",
    boxed: envoyRefusal.details,
    same: new EnvoyApiErrorTwin().details,
  },
  {
    name: "envoy-client.EnvoyApiError",
    kind: "error",
    boxed: envoyRefusal,
    same: new EnvoyApiErrorTwin(),
  },
  {
    name: "envoy-client.DispatchServiceError",
    kind: "error",
    boxed: serviceRefusal,
    same: new DispatchRefusalTwin(),
  },
  {
    name: "envoy-client.DispatchGatewayError",
    kind: "error",
    boxed: gatewayRefusal,
    same: new DispatchGatewayErrorTwin(),
  },
  {
    // The client's own refusal: its code is a box it builds from its own word, so it holds no
    // probe, and the row shows it prints none.
    name: "envoy-client.DispatchNoRouteError",
    kind: "error",
    boxed: new DispatchNoRouteError(),
    same: new DispatchServiceErrorTwin(),
  },
];

for (const holder of holders) {
  test(`box print test: ${holder.name} prints no secret by any printer at any placement`, () => {
    expect(printTest(holder).map((f) => f.why)).toEqual([]);
  });
}
