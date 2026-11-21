import { afterEach, describe, expect, test } from "bun:test";
import { type AddressInfo, createServer, type Socket } from "node:net";
import {
  BrokerRefusedError,
  RegistrationPendingError,
  SignInEndedError,
  TradeRetryingError,
  TradeUnansweredError,
} from "@legion/cipher-client/token-errors";
import {
  type CommentWriteResponse,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_KEY_PATTERN,
} from "@legion/contracts";
import { bearerCredential, type RequestCredential } from "../bearer";
import {
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  DispatchGatewayError,
  DispatchServiceError,
  DispatchWriteLandedError,
  type DispatchWriteRetry,
  DispatchWriteUnsettledError,
} from "../dispatch-http";

// Every write the `dispatch` command makes (a POST or a PATCH) carries one `Idempotency-Key` for
// that call, and asks Dispatch again under it while an attempt gets no definitive answer, so a
// dropped answer never makes the session's write land twice and a short outage never fails it.
// Every Dispatch that carries `b1c42d1d7d` runs the keyed layer on every write route, so the write
// asks again without asking anything first. `dispatch-keyed-resend.test.ts` uses a Bun stand-in;
// `dispatch-keyed-resend-live.test.ts` is the real-server coverage.

interface Sent {
  readonly method: string;
  readonly url: string;
  readonly headers: Record<string, string>;
  readonly body: RequestInit["body"];
}

/** A scripted answer: a response, one made when the request arrives (given its signal), or a
 *  transport failure. */
type Answer =
  | Response
  | ((signal: AbortSignal | null | undefined) => Response | Promise<Response>)
  | Error;

/**
 * A fetch that answers each write with the next of `answers`, recording what it was sent. A JSON
 * answer stands for one of Dispatch's own; an HTML page stands for a gateway's. A request the
 * script has no answer for fails the row.
 */
function scripted(
  answers: readonly Answer[],
  options: { readonly echo?: boolean | ((request: number) => boolean) } = {}
) {
  const sent: Sent[] = [];
  const echo = options.echo ?? true;
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input);
    const headers = { ...(init?.headers as Record<string, string>) };
    sent.push({ method: init?.method ?? "GET", url, headers, body: init?.body });
    const answer = answers[sent.length - 1];
    if (answer === undefined) throw new Error(`unexpected request ${sent.length}: ${url}`);
    if (answer instanceof Error) throw answer;
    const response = typeof answer === "function" ? await answer(init?.signal) : answer;
    const key = headers["Idempotency-Key"];
    const echoes = typeof echo === "function" ? echo(sent.length) : echo;
    if (!echoes || key === undefined || !isDispatchOwn(response)) return response;
    const echoed = new Response(response.body, response);
    echoed.headers.set("Idempotency-Key", key);
    return echoed;
  };
  return { fetchImpl: fetchImpl as typeof fetch, sent };
}

/** Whether a scripted answer stands for Dispatch's own (JSON) rather than a gateway's page. */
function isDispatchOwn(response: Response): boolean {
  return response.headers.get("Content-Type")?.includes("application/json") ?? false;
}

/**
 * The production pacing, with a clock that moves only when the client pauses or the test calls
 * `advance`.
 */
function pacing(policy: Omit<DispatchWriteRetry, "pause" | "now"> = DISPATCH_WRITE_RETRY) {
  const pauses: number[] = [];
  let clock = 0;
  const retry: DispatchWriteRetry = {
    ...policy,
    pause: async (ms) => {
      pauses.push(ms);
      clock += ms;
    },
    now: () => clock,
  };
  return {
    retry,
    pauses,
    advance: (ms: number) => {
      clock += ms;
    },
    clock: () => clock,
  };
}

function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

function refusal(status: number, code: string, error: string, headers = {}): Response {
  return json({ code, error }, status, headers);
}

function page(status: number, statusText: string): Response {
  return new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
    status,
    statusText,
    headers: { "Content-Type": "text/html" },
  });
}

const inFlight = () =>
  refusal(409, IDEMPOTENCY_KEY_IN_FLIGHT, "an earlier request with this key is still running", {
    "Retry-After": "1",
  });

/** An answer that never comes: the request waits until its signal aborts, then fails with its
 *  reason, as fetch does. */
function hang(signal: AbortSignal | null | undefined): Promise<Response> {
  const { promise, reject } = Promise.withResolvers<Response>();
  signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
  return promise;
}

/** A connection Dispatch read the request from and then reset, as Bun 1.4 fails it (measured in
 *  the review of c5ed36708c, `rr-reset-probe.ts`). */
const reset = () =>
  Object.assign(new TypeError("The socket connection was closed"), { code: "ECONNRESET" });
/** A connection nothing took: Bun 1.4's refused port. */
const refused = () => Object.assign(new TypeError("Unable to connect"), { code: "ECONNREFUSED" });

/** A credential whose presentations after the first fail fast, as a broker credential does once
 *  its token lapsed and the broker is down. */
function failingAfterFirst(): RequestCredential {
  let presented = 0;
  return {
    scheme: "Bearer",
    headers: () => {
      presented += 1;
      if (presented > 1) throw new TradeRetryingError("the secrets broker answered 503");
      return { Authorization: "Bearer secret" };
    },
    secret: () => "secret",
  };
}

/** A credential whose presentations after the first wait until their signal ends, then reject
 *  with its reason, as a broker credential waiting on a broker that does not answer does. */
function waitingAfterFirst(): RequestCredential {
  let presented = 0;
  return {
    scheme: "Bearer",
    headers: (_method, _url, _session, signal) => {
      presented += 1;
      if (presented === 1) return { Authorization: "Bearer secret" };
      const { promise, reject } = Promise.withResolvers<Readonly<Record<string, string>>>();
      signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
      return promise;
    },
    secret: () => "secret",
  };
}

const actor = { kind: "session" as const, id: "session-1" };
const comment = { body: "Shipped.", actor };
/** What these tests read of a posted comment; the rest of the shape is left out. */
const posted = {
  comment: { id: "comment-1", body: "Shipped." },
} as unknown as CommentWriteResponse;

function client(fetchImpl: typeof fetch, retry: DispatchWriteRetry, signal?: AbortSignal) {
  return new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl, {
    signal,
    writeRetry: retry,
  });
}

function failure(call: Promise<unknown>): Promise<Error> {
  return call.then(
    () => {
      throw new Error("expected the call to fail");
    },
    (error: Error) => error
  );
}

describe("DispatchClient's writes", () => {
  const keyedTail =
    ". Dispatch finishes an attempt it received whatever this command does, so read the write " +
    "back before you send it again: a new command sends it under a new key.";

  test("send one Idempotency-Key per write call and none on a read, PUT or DELETE", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 8 }, () => json({})));
    const dispatch = client(fetchImpl, pacing().retry);

    await dispatch.comment("DSP-1", comment);
    await dispatch.comment("DSP-1", comment);
    await dispatch.updateIssue("DSP-1", { status: "testing", actor });
    await dispatch.syncArchitectureSource("DSP", "acme/infra");
    await dispatch.writeHandoff("DSP-1", "implement", { actor, body: '{"schemaVersion":1}' });
    await dispatch.getIssue("DSP-1");
    await dispatch.followAsk("ask-1", "session-1", actor);
    await dispatch.unfollowAsk("ask-1", "session-1", actor);

    const keys = sent.map((request): string | undefined => request.headers["Idempotency-Key"]);
    expect(sent.map((request) => request.method)).toEqual([
      "POST",
      "POST",
      "PATCH",
      "POST",
      "POST",
      "GET",
      "PUT",
      "DELETE",
    ]);
    for (const key of keys.slice(0, 5)) expect(key).toMatch(IDEMPOTENCY_KEY_PATTERN);
    // Two presses are two writes: the same comment posted twice is two intents.
    expect(new Set(keys.slice(0, 5)).size).toBe(5);
    expect(keys.slice(5)).toEqual([undefined, undefined, undefined]);
  });

  // A caller whose intent outlives the call names the key itself: AskGate's decision document is
  // created once per session, whatever process reaches for it and however many times, and each of
  // its records is appended once (packages/pi-envoy/extensions/askgate/decision-record.ts). The
  // key it gives goes on every attempt of that write, and on every call it is given to, so
  // Dispatch runs that intent once and replays it rather than writing a second time.
  test("send a caller's own key on every attempt of its write, and mint one only where it gives none", async () => {
    const { fetchImpl, sent } = scripted([
      new TypeError("fetch failed"),
      refusal(503, "UNAVAILABLE", "dispatch is restarting"),
      json({ artifact: { id: "doc-1" } }),
      json({ applied: 1 }),
      json({}),
    ]);
    const dispatch = client(fetchImpl, pacing().retry);
    const create = "askgate-doc.3f7a91";
    const append = "askgate-rec.0c5e22";

    await dispatch.projectArtifact(
      "DSP",
      { name: "askgate-decisions-sess-1.md", content: "# Decisions", actor },
      create
    );
    await dispatch.docEdit(
      "doc-1",
      { ops: [{ op: "insert", markdown: "```json\n{}\n```", after: "end" }], actor },
      append
    );
    await dispatch.comment("DSP-1", comment);

    const keys = sent.map((request): string | undefined => request.headers["Idempotency-Key"]);
    // The three attempts of the create are one intent, and the key the caller chose is each one's.
    expect(keys.slice(0, 4)).toEqual([create, create, create, append]);
    expect(keys[4]).toMatch(IDEMPOTENCY_KEY_PATTERN);
    expect(keys[4]).not.toBe(create);
  });

  test("ask again under the same key through a transport failure, a 5xx, a 429 and 409 KEY_IN_FLIGHT, answering the first definitive answer", async () => {
    const { fetchImpl, sent } = scripted([
      new TypeError("fetch failed"),
      page(502, "Bad Gateway"),
      page(429, "Too Many Requests"),
      inFlight(),
      json(posted, 201, { "Idempotency-Replayed": "?1" }),
    ]);
    const { retry, pauses } = pacing();

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(5);
    const key = sent[0]?.headers["Idempotency-Key"];
    expect(key).toBeString();
    for (const request of sent) {
      expect(request.method).toBe("POST");
      expect(request.url).toBe("http://dispatch.test/api/v1/issues/DSP-1/comments");
      expect(request.headers["Idempotency-Key"]).toBe(key as string);
      expect(request.body).toBe(JSON.stringify(comment));
    }
    // After KEY_IN_FLIGHT the write is asked at most 3 s later, not on the schedule's 8 s.
    expect(pauses).toEqual([1_000, 2_000, 4_000, 3_000]);
  });

  // Dispatch's keyed layer stores a write once per key and answers a repeat under it with the
  // first answer, on every write route of every Dispatch that carries `b1c42d1d7d`, so a failure is
  // asked again whatever it says of that layer: Dispatch's own refusal from outside it, which
  // carries no key back, a gateway's page and a transport failure alike.
  test("ask again under the key whatever the failure says of the keyed layer", async () => {
    for (const answer of [
      refusal(500, "INTERNAL", "internal server error"),
      refusal(429, "RATE_LIMITED", "slow down"),
      refusal(503, "STORE_UNAVAILABLE", "database unavailable"),
      page(502, "Bad Gateway"),
      new TypeError("fetch failed"),
    ] as const) {
      const { fetchImpl, sent } = scripted([answer, json(posted, 201)], {
        echo: (request) => request > 1,
      });
      const { retry, pauses } = pacing();

      await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

      expect(sent).toHaveLength(2);
      expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
      expect(pauses).toEqual([1_000]);
    }
  });

  test("ask again after a gateway's 408 and Dispatch's own 5xx", async () => {
    const { fetchImpl, sent } = scripted([
      page(408, "Request Timeout"),
      refusal(500, "INTERNAL", "internal server error"),
      refusal(503, "UNAVAILABLE", "database unavailable"),
      json({ key: "DSP-1", status: "testing" }),
    ]);

    await expect(
      client(fetchImpl, pacing().retry).updateIssue("DSP-1", { status: "testing", actor })
    ).resolves.toEqual({ key: "DSP-1", status: "testing" } as never);
    expect(sent.map((request) => request.method)).toEqual(["PATCH", "PATCH", "PATCH", "PATCH"]);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
  });

  test("answer any other refusal at once, KEY_REUSED and other 409s included", async () => {
    for (const answer of [
      refusal(422, "KEY_REUSED", "this key was used for another request"),
      refusal(409, "ISSUE_CLAIMED", "session-2 holds DSP-1"),
      refusal(400, "INVALID_COMMENT", "body must not be blank"),
      refusal(404, "ISSUE_NOT_FOUND", "no such issue"),
      page(404, "Not Found"),
    ]) {
      const { fetchImpl, sent } = scripted([answer]);
      const { retry, pauses } = pacing();

      const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(error).toBeInstanceOf(DispatchServiceError);
      expect(error).toEqual(expect.objectContaining({ status: answer.status }));
      expect(sent).toHaveLength(1);
      expect(pauses).toEqual([]);
    }
  });

  // A status that carries no body - a 304 - cannot be rebuilt around one: the Fetch standard's
  // `new Response` refuses a body for it, as Node's and every browser's does. Bun's accepts one, so
  // the test holds the client to the standard's constructor; the answer is handed on as it came,
  // and the call fails with Dispatch's status rather than with the client's own TypeError.
  test("answer a bodiless refusal at once, as it came", async () => {
    const { fetchImpl, sent } = scripted([
      new Response(null, { status: 304, statusText: "Not Modified" }),
    ]);
    const { retry, pauses } = pacing();
    const BunResponse = globalThis.Response;
    globalThis.Response = class extends BunResponse {
      constructor(body?: BodyInit | null, init?: ResponseInit) {
        if (
          body !== null &&
          body !== undefined &&
          [101, 204, 205, 304].includes(init?.status ?? 200)
        ) {
          throw new TypeError(`Response constructor: a ${init?.status} response has no body`);
        }
        super(body, init);
      }
    } as typeof Response;
    try {
      const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(error).not.toBeInstanceOf(TypeError);
      expect(error).toEqual(expect.objectContaining({ status: 304 }));
      expect(sent).toHaveLength(1);
      expect(pauses).toEqual([]);
    } finally {
      globalThis.Response = BunResponse;
    }
  });

  test("a stored 409 after a retry keeps its code, candidates and message", async () => {
    const candidates = [{ from: 1, to: 2, context: "same" }];
    const { fetchImpl } = scripted([
      page(502, "Bad Gateway"),
      json({ error: "quote is ambiguous", code: "TARGET_AMBIGUOUS", candidates }, 409),
    ]);

    await expect(
      client(fetchImpl, pacing().retry).docEdit("artifact-1", {
        ops: [{ op: "delete", find: "same" }],
        actor,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "TARGET_AMBIGUOUS",
        status: 409,
        message: "quote is ambiguous",
        candidates,
      })
    );
  });

  // R1-8710 (the review of 87105ed7e8): a 5xx can come after Dispatch committed the write, so a
  // write that runs out of attempts on 5xx answers ends unsettled, carrying its key, with the last
  // answer as `last`. It used to throw that answer as it stood, which a caller reads as a refusal
  // ("try again in a minute") and resends under a new key.
  test("stop after five attempts and end unsettled, the last attempt's refusal as its cause", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => page(502, "Bad Gateway")));
    const { retry, pauses } = pacing();

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(5);
    expect(pauses).toEqual([1_000, 2_000, 4_000, 8_000]);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect((error as DispatchWriteUnsettledError).last).toBeInstanceOf(DispatchGatewayError);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 502 Bad Gateway with a " +
        `body that is not Dispatch's error JSON ("502 Bad Gateway"), which looks like a proxy or ` +
        "gateway page rather than Dispatch's own answer (the last of 5 attempts), and an attempt " +
        "of the write may have reached Dispatch, so it may or may not have taken effect. Dispatch " +
        "finishes an attempt it received whatever this command does, so read the write back " +
        "before you send it again: a new command sends it under a new key."
    );
  });

  // The reviewer's R1-8710 case: a silence cut, then Dispatch's broker-token gate answering 503
  // BROKER_UNAVAILABLE (before its keyed layer reads the key) on every later attempt, echoed or
  // not. The write ends unsettled with its key, never "try again in a minute".
  test.each([
    ["un-echoed", false],
    ["echoed", true],
  ] as const)("a silence cut then %s 503s to the end leaves the write unsettled", async (_, echoes) => {
    const { retry, stall } = stalling();
    const unavailable = () =>
      refusal(
        503,
        "BROKER_UNAVAILABLE",
        "the secrets broker cannot be reached; try again in a minute"
      );
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 30 }, unavailable)], {
      echo: (request) => echoes && request > 1,
    });

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent.length).toBeGreaterThan(2);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect((error as DispatchWriteUnsettledError).last).toEqual(
      expect.objectContaining({ code: "BROKER_UNAVAILABLE", status: 503 })
    );
  });

  // The last attempt's failure is the client's own error: the URL as `urlForText` names it and the
  // failure by its class, since the runtime's message repeats the host it dialed (#4140, B1). A
  // Dispatch that is down refuses the connection (`ECONNREFUSED`, as Bun 1.4 names it), which
  // sends nothing, so no attempt can have landed and the write is told Dispatch is unreachable.
  test("a refused connection on every attempt throws the last one, named by its class", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => refused()));

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(5);
    expect(error).not.toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toBe(
      "Dispatch at http://dispatch.test is unreachable: ECONNREFUSED. If the " +
        "Dispatch URL changed, restart this agent process so it picks up the new configuration."
    );
  });

  // c2 (the review of c5ed36708c): a connection reset after Dispatch read the whole request may
  // follow a write Dispatch received, and its keyed layer runs that write to its end whatever the
  // client does, so a reset on every attempt may have landed: unsettled, never "unreachable".
  test("a connection reset on every attempt leaves the write unsettled", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => reset()));

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(5);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toStartWith(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments failed on its last attempt " +
        "(Dispatch at http://dispatch.test is unreachable: ECONNRESET."
    );
  });

  // c1 and its twin: after a reset the write may have landed, so a credential that then cannot
  // present is unsettled; after a refused connection nothing went out, so the broker's own error
  // stands.
  test.each([
    ["reset", () => reset(), true],
    ["refused", () => refused(), false],
  ] as const)("a credential that fails after a %s connection", async (_, first, unsettled) => {
    const { fetchImpl } = scripted([first()]);
    const error = await failure(
      new DispatchClient("http://dispatch.test", failingAfterFirst(), fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    );
    if (unsettled) {
      expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
      expect((error as DispatchWriteUnsettledError).last).toBeInstanceOf(TradeRetryingError);
    } else {
      expect(error).toBeInstanceOf(TradeRetryingError);
    }
  });

  // c3 and its twin: the same, where the second credential waits out the client's own deadline.
  test.each([
    ["reset", () => reset(), true],
    ["refused", () => refused(), false],
  ] as const)(
    "a credential that waits out the deadline after a %s connection",
    async (_, first, unsettled) => {
      const { fetchImpl } = scripted([first()]);
      const error = await failure(
        new DispatchClient("http://dispatch.test", waitingAfterFirst(), fetchImpl, {
          writeRetry: pacing().retry,
          deadlineMs: 300,
        }).comment("DSP-1", comment)
      );
      if (unsettled) {
        expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
        expect(error.message).toContain("had no definitive answer when its deadline ended it");
      } else {
        expect(error).not.toBeInstanceOf(DispatchWriteUnsettledError);
        expect(error.name).toBe("TimeoutError");
      }
    },
    10_000
  );

  test("wait a Retry-After longer than the pause, and stop rather than start past 30 s", async () => {
    const { fetchImpl, sent } = scripted([
      refusal(429, "RATE_LIMITED", "slow down", { "Retry-After": "5" }),
      page(503, "Service Unavailable"),
      refusal(429, "RATE_LIMITED", "slow down", { "Retry-After": "40" }),
    ]);
    const { retry, pauses } = pacing();

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    // 5 s honours the first Retry-After; 2 s is the schedule's; 40 s would start past 30 s.
    expect(pauses).toEqual([5_000, 2_000]);
    expect(sent).toHaveLength(3);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).last).toEqual(
      expect.objectContaining({ code: "RATE_LIMITED", status: 429 })
    );
  });

  test("stop at the 30 s bound however few attempts that leaves", async () => {
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => page(503, "Unavailable")));
    const { retry, pauses } = pacing({
      ...DISPATCH_WRITE_RETRY,
      pausesMs: [10_000, 15_000, 20_000],
    });

    await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(pauses).toEqual([10_000, 15_000]);
    expect(sent).toHaveLength(3);
  });

  test("say a write may have landed when the last refusal alone says it did not", async () => {
    const after502 = scripted([
      page(502, "Bad Gateway"),
      ...Array.from({ length: 4 }, () => page(429, "Too Many Requests")),
    ]);
    const landed = await failure(
      client(after502.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );

    expect(landed).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(landed).not.toBeInstanceOf(DispatchServiceError);
    expect((landed as DispatchWriteUnsettledError).attempts).toBe(5);
    expect((landed as DispatchWriteUnsettledError).last).toBeInstanceOf(DispatchGatewayError);
    expect(landed.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 429 Too Many Requests " +
        `with a body that is not Dispatch's error JSON ("429 Too Many Requests"), which looks like ` +
        "a proxy or gateway page rather than Dispatch's own answer (the last of 5 attempts), and an " +
        "earlier attempt of the write may have reached Dispatch, so it may or may not have taken " +
        "effect. Dispatch finishes an attempt it received whatever this command does, so read the " +
        "write back before you send it again: a new command sends it under a new key."
    );

    // KEY_IN_FLIGHT says Dispatch may still be running the write, so it is asked every 3 s at most
    // until 55 s: first at 0 s, then at 1 and 3 s on the schedule, then every 3 s from 6 to 54 s.
    const running = scripted([
      new TypeError("fetch failed"),
      ...Array.from({ length: 30 }, () => inFlight()),
    ]);
    const stillRunning = await failure(
      client(running.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );

    expect(running.sent).toHaveLength(20);
    expect(stillRunning).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(stillRunning.message).toBe(
      "KEY_IN_FLIGHT (the last of 20 " +
        "attempts), and Dispatch was still running an earlier attempt of the write, so it may or " +
        "may not have taken effect. Dispatch finishes an attempt it received whatever this " +
        "command does, so read the write back before you send it again: a new command sends it " +
        "under a new key."
    );
  });

  test("a write no attempt of which can have reached Dispatch keeps the last refusal", async () => {
    const { fetchImpl } = scripted(Array.from({ length: 5 }, () => page(429, "Too Many Requests")));

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(error).toBeInstanceOf(DispatchGatewayError);
    expect(error).toEqual(expect.objectContaining({ mayHaveReachedDispatch: false }));
  });

  // The call's deadline is the caller's limit, never a reason to ask again. A write it ends while
  // an attempt is out, which may have reached Dispatch, may or may not have taken effect, and says
  // so; so does the caller's own cancellation once an attempt may have landed, while one before any
  // attempt went out is thrown as it is (the transition table, write-outcome.ts).
  test("the deadline or the caller's cancellation ends a write's attempts", async () => {
    const hung = scripted([hang]);
    const timeout = await failure(
      client(hung.fetchImpl, pacing().retry, AbortSignal.timeout(30)).comment("DSP-1", comment)
    );
    expect(timeout).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((timeout as DispatchWriteUnsettledError).last.name).toBe("TimeoutError");
    expect(timeout.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments had no definitive answer when its " +
        "deadline ended it (its only attempt), and an attempt of the write may have reached " +
        `Dispatch, so it may or may not have taken effect${keyedTail}`
    );
    expect(hung.sent).toHaveLength(1);

    // A timeout the fetch reports of its own is a deadline too.
    const timedOut = scripted([
      () => {
        throw new DOMException("The operation timed out.", "TimeoutError");
      },
    ]);
    const thrown = await failure(
      client(timedOut.fetchImpl, pacing().retry).comment("DSP-1", comment)
    );
    expect(thrown).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(timedOut.sent).toHaveLength(1);

    // The caller's stop during the pause after a 502, which may have landed: unsettled, with the
    // stop as its cause, and nothing sent after it.
    const controller = new AbortController();
    const cancelled = scripted([page(502, "Bad Gateway"), json(posted, 201)]);
    const paused: DispatchWriteRetry = {
      ...DISPATCH_WRITE_RETRY,
      pause: (_ms, signal) => {
        controller.abort(new DOMException("The operation was aborted.", "AbortError"));
        return Promise.reject(signal.reason);
      },
    };
    const aborted = await failure(
      client(cancelled.fetchImpl, paused, controller.signal).comment("DSP-1", comment)
    );
    expect(aborted).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((aborted as DispatchWriteUnsettledError).last.name).toBe("AbortError");
    expect(cancelled.sent).toHaveLength(1);

    // The caller's stop before any attempt went out is thrown as it is.
    const early = new AbortController();
    early.abort(new DOMException("The operation was aborted.", "AbortError"));
    const none = scripted([json(posted, 201)]);
    const stopped = await failure(
      client(none.fetchImpl, pacing().retry, early.signal).comment("DSP-1", comment)
    );
    expect(stopped.name).toBe("AbortError");
    expect(none.sent).toHaveLength(0);
  });

  // The second half of this row proves the caller's deadline ends a pause rather than waiting it
  // out, and it reads no clock to do it: the pause it must not wait out is 30 s, well above the
  // row's own 20 s limit, so a pause that ran would fail the row by that limit. The limit is that
  // discriminator rather than a bound on work: measured in a 2-CPU pod the row costs 0.10 s idle
  // and, under 16 busy loops (8 x nproc), 0.52 s at most in a whole-suite run and 0.43 s with this
  // file alone. It stays on the row because the suite's 30 s default would equal that pause.
  test("the production pause waits out its time and stops when the signal aborts", async () => {
    const { fetchImpl, sent } = scripted([page(502, "Bad Gateway"), json(posted, 201)]);
    const started = performance.now();

    await expect(
      client(fetchImpl, { ...DISPATCH_WRITE_RETRY, pausesMs: [50], withinMs: 1_000 }).comment(
        "DSP-1",
        comment
      )
    ).resolves.toEqual(posted);
    expect(performance.now() - started).toBeGreaterThanOrEqual(45);
    expect(sent).toHaveLength(2);

    // The deadline ends the pause after a 502 that may have reached Dispatch.
    const stalled = scripted([page(502, "Bad Gateway")]);
    const stop = AbortSignal.timeout(50);
    const error = await failure(
      client(
        stalled.fetchImpl,
        { ...DISPATCH_WRITE_RETRY, pausesMs: [30_000], withinMs: 120_000 },
        stop
      ).comment("DSP-1", comment)
    );
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).last.name).toBe("TimeoutError");
  }, 20_000);

  // An attempt that hears nothing for silenceMs is cut off and asked again under its key, so a
  // write Dispatch is slow to finish gets its KEY_IN_FLIGHT and then its replay inside the call's
  // deadline, rather than a bare timeout at its end. The bound is a real timer aborting the fetch's
  // own signal, so these two run it for tens of milliseconds, as the pause test above does.
  test("cut off an attempt silent for silenceMs and ask again under its key", async () => {
    const { fetchImpl, sent } = scripted([
      hang,
      inFlight(),
      json(posted, 201, { "Idempotency-Replayed": "?1" }),
    ]);
    const { retry, pauses } = pacing({ ...DISPATCH_WRITE_RETRY, silenceMs: 50 });
    const started = performance.now();

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(performance.now() - started).toBeGreaterThanOrEqual(45);
    expect(sent).toHaveLength(3);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
    expect(pauses).toEqual([1_000, 2_000]);
  });

  /**
   * The production pacing for a write whose attempts stall. The silence cut is a real timer, so it
   * fires here after 20 ms, and the client's clock moves by the production `silenceMs` when it
   * does, as 20 s of real silence would move it; every window and pause is production's.
   */
  function stalling() {
    const paced = pacing({ ...DISPATCH_WRITE_RETRY, silenceMs: 20 });
    const stall = (signal: AbortSignal | null | undefined): Promise<Response> => {
      signal?.addEventListener("abort", () => paced.advance(DISPATCH_WRITE_RETRY.silenceMs), {
        once: true,
      });
      return hang(signal);
    };
    return { ...paced, stall };
  }

  const replayed = () => json(posted, 201, { "Idempotency-Replayed": "?1" });

  // Measured on 2026-11-02 from the transcript store: one write cut off for silence took 52 s
  // when it was sent again, and succeeded. Dispatch finishes a keyed write whatever its client
  // does, so the call keeps asking under its key, each repeat answered 409 KEY_IN_FLIGHT, until
  // the first run's stored answer comes back. Before, it stopped asking 30 s after it began.
  test("collect under its key a write Dispatch finishes 52 s after it began, past a silence cut", async () => {
    const { retry, pauses, clock, stall } = stalling();
    const finished = () => (clock() < 52_000 ? inFlight() : replayed());
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => finished)]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
    // Cut at 20 s, then asked 1 s later and at most every 3 s while the run lives: the replay is
    // collected at 53 s, inside the 55 s the call keeps for a write Dispatch may still be running.
    expect(pauses).toEqual([1_000, 2_000, ...Array.from({ length: 10 }, () => 3_000)]);
    expect(clock()).toBe(53_000);
  });

  test("ask again under its key after one silent attempt", async () => {
    const { retry, pauses, clock, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, replayed()]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(2);
    expect(pauses).toEqual([1_000]);
    expect(clock()).toBe(21_000);
  });

  test("ask again under its key after two silent attempts in a row", async () => {
    const { retry, pauses, clock, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, stall, replayed()]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(3);
    expect(pauses).toEqual([1_000, 2_000]);
    expect(clock()).toBe(43_000);
  });

  test("a write whose every attempt is silent may or may not have taken effect", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted(Array.from({ length: 5 }, () => stall));

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    // Cut at 20, 41 and 63 s; no ask starts past 55 s.
    expect(sent).toHaveLength(3);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments made no progress for 0.02 s, so it " +
        "was cut off (the last of 3 attempts), and an attempt of the write may have reached " +
        "Dispatch, so it may or may not have taken effect. Dispatch finishes an attempt it " +
        "received whatever this command does, so read the write back before you send it again: " +
        "a new command sends it under a new key."
    );
  });

  // An attempt cut off for silence may still be running in Dispatch, so a write whose later
  // attempts then fail in transport may have landed: unsettled, not "unreachable", which reads as
  // nothing written.
  test("a transport failure after a silent attempt leaves the write unsettled", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([
      stall,
      ...Array.from({ length: 20 }, () => new TypeError("fetch failed")),
    ]);

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent.length).toBeGreaterThan(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toStartWith(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments failed on its last attempt " +
        "(Dispatch at http://dispatch.test is unreachable: TypeError (message withheld)."
    );
    expect(error.message).toContain("so it may or may not have taken effect");
  });

  // A credential that cannot present a later attempt sends nothing, but an earlier attempt that
  // was cut off for silence may still have landed: the write is unsettled, never the broker's
  // error, which reads as nothing having been written and invites a resend under a new key.
  test("a credential that fails after a silent attempt leaves the write unsettled, naming the credential", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, replayed()]);
    // The write's presentations: the first goes out with attempt 1, every later one fails, as a
    // fail-fast broker credential does once its token lapsed and the broker is down.
    let presented = 0;
    const credential: RequestCredential = {
      scheme: "Bearer",
      headers: () => {
        presented += 1;
        if (presented > 1) {
          throw new TradeRetryingError(
            "the secrets broker answered 503 temporarily_unavailable to the trade"
          );
        }
        return { Authorization: "Bearer secret" };
      },
      secret: () => "secret",
    };

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).last).toBeInstanceOf(TradeRetryingError);
    expect(error.message).toBe(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments could not present its credential " +
        "for another attempt (the secrets broker answered 503 temporarily_unavailable to the " +
        "trade, and the trade is being retried in the background) (its only attempt), and an " +
        "attempt of the write may have reached Dispatch, so it may or may not have taken effect. " +
        "Dispatch finishes an attempt it received whatever this command does, so read the write " +
        "back before you send it again: a new command sends it under a new key."
    );
  });

  // A write that never sent anything has nothing to settle: the credential's own error stands.
  test("a credential that fails before any attempt is sent throws its own error", async () => {
    const { retry } = stalling();
    const { fetchImpl, sent } = scripted([]);
    const credential: RequestCredential = {
      scheme: "Bearer",
      headers: () => {
        throw new TradeRetryingError("the secrets broker refused the connection");
      },
      secret: () => "",
    };

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(0);
    expect(error).toBeInstanceOf(TradeRetryingError);
  });

  // A1. A 5xx may have come after Dispatch committed the write, so a write whose later attempts
  // then fail in transport may have landed: unsettled, never "unreachable", which reads as nothing
  // written and invites a resend under a new key.
  test("a transport failure after a 5xx that may have reached Dispatch leaves the write unsettled", async () => {
    const { fetchImpl, sent } = scripted([
      page(502, "Bad Gateway"),
      ...Array.from({ length: 4 }, () => new TypeError("fetch failed")),
    ]);

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(5);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toStartWith(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments failed on its last attempt " +
        "(Dispatch at http://dispatch.test is unreachable: TypeError (message withheld)."
    );
  });

  // A1 for an upload: its silence cut keeps the five-attempt schedule rather than the in-flight
  // asking, but the cut attempt may still have landed all the same.
  test("a transport failure after an upload's silence cut leaves the upload unsettled", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([
      stall,
      ...Array.from({ length: 4 }, () => new TypeError("fetch failed")),
    ]);

    const error = await failure(
      client(fetchImpl, retry).artifact("DSP-1", {
        name: "build.tar",
        file: new Blob([new Uint8Array(1 << 20)]),
        actor,
      })
    );

    expect(sent.length).toBeGreaterThan(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toContain("failed on its last attempt (Dispatch at");
  });

  // A2. Dispatch answers a dead token 401 BROKER_TOKEN_NOT_ACTIVE before any handler runs, and the
  // client presents afresh and sends again. An earlier attempt of that write cut off for silence
  // may still have landed, so a credential that then cannot present is unsettled too.
  test("a dead token's re-send whose credential fails after a silent attempt leaves the write unsettled", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([
      stall,
      refusal(
        401,
        "BROKER_TOKEN_NOT_ACTIVE",
        "the secrets broker does not hold this token as live"
      ),
    ]);
    let stale = false;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        if (stale) {
          throw new TradeRetryingError("the secrets broker refused the connection to the trade");
        }
        return { Authorization: "DPoP live-token" };
      },
      stale: () => {
        stale = true;
      },
      secret: () => "live-token",
    };

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(2);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).last).toBeInstanceOf(TradeRetryingError);
  });

  // A3. While a release rolls out, an attempt that reached the new build and lost its answer can be
  // followed by an old build's no-route answer. The no-route retry sends the write again under the
  // key it already carries, so the new build answers it from that key rather than writing twice.
  test("a no-route retry after a silent attempt sends the write again under its own key", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted(
      [
        stall,
        refusal(404, "NOT_FOUND", "no route for POST /api/v1/issues/DSP-1/comments"),
        replayed(),
      ],
      { echo: (request) => request !== 2 }
    );

    await expect(
      new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl, {
        writeRetry: retry,
        sleep: async () => {},
      }).comment("DSP-1", comment)
    ).resolves.toEqual(posted);

    expect(sent).toHaveLength(3);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
  });

  // A3, exhausted: every later send meets an old build, so the write may have landed on the new
  // one and the call cannot say: unsettled, never the rolling release's "try again".
  test("a write whose later sends all meet no route after a silent attempt is unsettled", async () => {
    const { retry, stall } = stalling();
    const noRoute = () =>
      refusal(404, "NOT_FOUND", "no route for POST /api/v1/issues/DSP-1/comments");
    const { fetchImpl } = scripted([stall, ...Array.from({ length: 10 }, noRoute)], {
      echo: (request) => request === 1,
    });

    const error = await failure(
      new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl, {
        writeRetry: retry,
        sleep: async () => {},
      }).comment("DSP-1", comment)
    );

    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toContain("may or may not have taken effect");
  });

  // F4 (the review of 215510405b): once an attempt may have landed, only a 2xx read whole or a
  // refusal Dispatch decided settles it. A page something in Dispatch's place wrote, or a refusal
  // Dispatch's auth made before its keyed layer ran (a 401, `403 READ_ONLY_TOKEN`), says nothing of
  // the earlier attempt, so the write is unsettled, carrying its key, never a refusal that reads as
  // nothing written.
  const unjudged: readonly (readonly [string, () => Response, number])[] = [
    ["a gateway's 403 page", () => page(403, "Forbidden"), 403],
    ["a gateway's 404 page", () => page(404, "Not Found"), 404],
    ["Dispatch's auth 401", () => refusal(401, "UNAUTHORIZED", "a token is required"), 401],
    [
      "Dispatch's 403 READ_ONLY_TOKEN",
      () => refusal(403, "READ_ONLY_TOKEN", "this token may only read"),
      403,
    ],
  ];
  test.each(
    unjudged
  )("%s after a silent attempt leaves the write unsettled, carrying its key", async (_, answer, status) => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, answer()], { echo: false });

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(2);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    const unsettled = error as DispatchWriteUnsettledError;
    expect(unsettled.key).toBe(sent[0]?.headers["Idempotency-Key"] as string);
    expect((unsettled.last as DispatchServiceError).status).toBe(status);
    expect(error.message).toContain("so it may or may not have taken effect");
  });

  // A handler's 403 runs inside keyedWrites and is stored under the key. When its first answer is
  // lost, the repeat's replay is the final refusal, not an auth refusal before the keyed layer.
  test("a keyed handler 403 replayed after a silent attempt stands as Dispatch's refusal", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([
      stall,
      refusal(
        403,
        "ACTOR_SESSION_MISMATCH",
        "the actor this write names is not the session its credential proved",
        { "Idempotency-Replayed": "?1" }
      ),
    ]);

    const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(2);
    expect(error).toBeInstanceOf(DispatchServiceError);
    expect(error).not.toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error).toEqual(expect.objectContaining({ status: 403, code: "ACTOR_SESSION_MISMATCH" }));
  });

  // A second 401 BROKER_TOKEN_NOT_ACTIVE, after the client presented afresh and sent again, is
  // Dispatch's auth refusing before its keyed layer ran: unsettled too.
  test("a second dead-token answer after a silent attempt leaves the write unsettled, carrying its key", async () => {
    const { retry, stall } = stalling();
    const notActive = () =>
      refusal(
        401,
        "BROKER_TOKEN_NOT_ACTIVE",
        "the secrets broker does not hold this token as live"
      );
    const { fetchImpl, sent } = scripted([stall, notActive(), notActive()], { echo: false });
    let stale = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => ({ Authorization: "DPoP live-token" }),
      stale: () => {
        stale += 1;
      },
      secret: () => "live-token",
    };

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: retry,
      }).comment("DSP-1", comment)
    );

    expect(stale).toBe(1);
    expect(sent).toHaveLength(3);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
  });

  test.each(unjudged)("%s with nothing landed stands as the refusal", async (_, answer, status) => {
    const { fetchImpl, sent } = scripted([answer()], { echo: false });

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(sent).toHaveLength(1);
    expect(error).toBeInstanceOf(DispatchServiceError);
    expect((error as DispatchServiceError).status).toBe(status);
  });

  // The deadline that ends a no-route retry's wait after an attempt that may have landed leaves the
  // write unsettled too, never the runtime's bare "The operation timed out." (the red team of the
  // review of c5ed36708c, F3).
  test("the deadline during a no-route wait after a silent attempt leaves the write unsettled", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl } = scripted(
      [stall, refusal(404, "NOT_FOUND", "no route for POST /api/v1/issues/DSP-1/comments")],
      { echo: (request) => request === 1 }
    );

    const error = await failure(
      new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl, {
        writeRetry: retry,
        deadlineMs: 300,
        // The wait between no-route answers ends only when the client's deadline does.
        sleep: (_ms, signal) =>
          new Promise<void>((_resolve, reject) => {
            signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
          }),
      }).comment("DSP-1", comment)
    );

    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect(error.message).toContain("had no definitive answer when its deadline ended it");
  }, 10_000);

  // R1-81d8 (the review of 81d860a8f7): a 2xx settles only once its body is read whole, inside the
  // attempt. Before, the body was read after the write had settled, outside every table event, so
  // a deadline during it after a landing threw the runtime's TimeoutError with no key.
  /** A 201 that carries the key back and whose body begins, then meets `then` as the client reads
   *  on: `controller`'s abort (the call's deadline or stop, which ends the body as fetch ends one
   *  its signal ends), or a broken connection. */
  const partBody =
    (then: "broken" | AbortController, reason?: unknown) =>
    (signal: AbortSignal | null | undefined): Response => {
      const whole = JSON.stringify(posted);
      const body = new ReadableStream<Uint8Array>({
        start(stream) {
          signal?.addEventListener("abort", () => stream.error(signal.reason), { once: true });
          stream.enqueue(new TextEncoder().encode(whole.slice(0, 11)));
        },
        pull(stream) {
          if (then === "broken") stream.error(reset());
          else then.abort(reason);
        },
      });
      return new Response(body, { status: 201, headers: { "Content-Type": "application/json" } });
    };

  test.each([
    ["deadline", new DOMException("The operation timed out.", "TimeoutError")],
    ["caller's stop", new DOMException("The operation was aborted.", "AbortError")],
  ] as const)("a 201 whose body the %s ends after a landing leaves the write unsettled, carrying its key", async (_, reason) => {
    const controller = new AbortController();
    const { fetchImpl, sent } = scripted([
      refusal(503, "UNAVAILABLE", "the database is restarting"),
      partBody(controller, reason),
    ]);

    const error = await failure(
      client(fetchImpl, pacing().retry, controller.signal).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(2);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect((error as DispatchWriteUnsettledError).last.name).toBe(reason.name);
  });

  // The same body cut short by the deadline before anything else of the write: a 201's headers came,
  // so it may have landed.
  test("a 201 whose body the deadline ends on the first attempt leaves the write unsettled", async () => {
    const controller = new AbortController();
    const { fetchImpl, sent } = scripted([
      partBody(controller, new DOMException("The operation timed out.", "TimeoutError")),
    ]);

    const error = await failure(
      client(fetchImpl, pacing().retry, controller.signal).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
  });

  // A body that breaks off is asked again under the key, and Dispatch's replay of what it stored is
  // the write's answer.
  test("a 201 whose body breaks off is asked again under its key, and the replay settles it", async () => {
    const { fetchImpl, sent } = scripted([partBody("broken"), replayed()]);

    await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
      posted
    );

    expect(sent).toHaveLength(2);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
  });

  // A 201 whose body, read whole, is not the JSON it says (cut short on the way) is asked again the
  // same way, where it used to settle the write with the text as its payload.
  test("a 201 whose body is not the JSON it says is asked again under its key", async () => {
    const cut = new Response(JSON.stringify(posted).slice(0, 11), {
      status: 201,
      headers: { "Content-Type": "application/json" },
    });
    const { fetchImpl, sent } = scripted([cut, replayed()]);

    await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
      posted
    );

    expect(sent).toHaveLength(2);
  });

  // R2-81d8: a fail-fast broker credential tells the write its trade's last failure at once while
  // the trade goes on (`TradeRetryingError`); nothing was sent, so the write presents again, and the
  // presentation after it can succeed. Before, a refused connection and then one such failure
  // ended the write, where the write before the table posted it.
  test("a credential that fails once while its trade goes on is presented again, and the write goes", async () => {
    let presented = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presented += 1;
        if (presented === 2) {
          throw new TradeRetryingError("the secrets broker answered 503 temporarily_unavailable");
        }
        return { Authorization: "DPoP live" };
      },
      stale: () => {},
      secret: () => "live",
    };
    const { fetchImpl, sent } = scripted([refused(), json(posted, 201)]);

    await expect(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    ).resolves.toEqual(posted);

    expect(sent).toHaveLength(2);
    expect(presented).toBe(3);
  });

  // A broker credential can fail fast while its trade goes on. Nothing of the write is sent until
  // a presentation succeeds, so the write keeps presenting on its pacing.
  test("a fail-fast credential failing every presentation for a time is presented again, and the write goes", async () => {
    let presented = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presented += 1;
        if (presented <= 2) {
          throw new TradeRetryingError("the secrets broker answered 503 temporarily_unavailable");
        }
        return { Authorization: "DPoP live" };
      },
      stale: () => {},
      secret: () => "live",
    };
    const { fetchImpl, sent } = scripted([json(posted, 201)]);

    await expect(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    ).resolves.toEqual(posted);

    expect(sent).toHaveLength(1);
    expect(presented).toBe(3);
  });

  // A credential whose trade never comes back within the call. The fail-fast `TradeRetryingError`
  // is presented again on the write's pacing, and the write ends with it once the pacing is spent;
  // a trade still unanswered and a registration still pending are told only once the
  // presentation has waited out its share of the call's deadline, so the write ends with them at
  // once (R3-99b1). Nothing is sent; after a landing each ends unsettled with its key.
  const goingOn = [
    [
      "TradeRetryingError",
      () => new TradeRetryingError("the secrets broker answered 503 temporarily_unavailable"),
    ],
    ["TradeUnansweredError", () => new TradeUnansweredError("this machine's pool identity")],
    [
      "RegistrationPendingError",
      () =>
        new RegistrationPendingError(
          "ses_pod",
          new BrokerRefusedError(
            400,
            "pod_session_not_registered",
            "pod_session_not_registered: No registration says this pod may write as ses_pod",
            "this pod"
          )
        ),
    ],
  ] as const;
  const failing = (make: () => Error) => {
    const presentations: string[] = [];
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: (_method, url) => {
        presentations.push(url);
        throw make();
      },
      secret: () => "",
    };
    return { credential, writes: () => presentations };
  };

  test("a credential that keeps failing with TradeRetryingError ends the write with it once its pacing is spent", async () => {
    const [, make] = goingOn[0];
    const { credential, writes } = failing(make);
    const { retry, clock } = pacing();
    const { fetchImpl, sent } = scripted([]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: retry,
      }).comment("DSP-1", comment)
    );

    expect(error).toBeInstanceOf(TradeRetryingError);
    expect(sent).toHaveLength(0);
    expect(writes().length).toBe(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
    expect(clock()).toBeLessThanOrEqual(DISPATCH_WRITE_RETRY.withinMs);
  });

  test.each(
    goingOn.slice(1)
  )("a credential that fails with %s, told after its share of the deadline, ends the write at once with it", async (_, make) => {
    const { credential, writes } = failing(make);
    const { retry, pauses } = pacing();
    const { fetchImpl, sent } = scripted([]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: retry,
      }).comment("DSP-1", comment)
    );

    expect(error).toBeInstanceOf(make().constructor as new (...args: never[]) => Error);
    expect(sent).toHaveLength(0);
    expect(writes()).toHaveLength(1);
    expect(pauses).toEqual([]);
  });

  test.each(
    goingOn
  )("a credential that keeps failing with %s after a landing ends the write unsettled, carrying its key", async (_, make) => {
    let calls = 0;
    const { credential: never } = failing(make);
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: (method, url, body, signal) => {
        calls += 1;
        return calls === 1
          ? { Authorization: "DPoP live" }
          : never.headers(method, url, body, signal);
      },
      secret: () => "",
    };
    const { fetchImpl, sent } = scripted([
      refusal(503, "UNAVAILABLE", "the database is restarting"),
    ]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(1);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect((error as DispatchWriteUnsettledError).last).toBeInstanceOf(
      make().constructor as new (
        ...args: never[]
      ) => Error
    );
  });

  // Every Dispatch write asks for JSON, so a 2xx whose body is never the JSON it says is asked
  // again under its key until the asking is spent, and then ends unsettled with the key: never a
  // success, and never a loop past the write's limits.
  test("a 201 whose body is never the JSON it says ends unsettled with its key once the asking is spent", async () => {
    const cut = () =>
      new Response(JSON.stringify(posted).slice(0, 11), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      });
    const { fetchImpl, sent } = scripted(Array.from({ length: 20 }, cut));

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect(sent.length).toBe(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
    expect(new Set(sent.map((request) => request.headers["Idempotency-Key"])).size).toBe(1);
  });

  // A 2xx that carries no body (a 204, a 205) has nothing to read whole, so it settles at once.
  test.each([204, 205])("a %i settles the write", async (status) => {
    const { fetchImpl, sent } = scripted([new Response(null, { status })]);

    const answer: unknown = await client(fetchImpl, pacing().retry).comment("DSP-1", comment);

    expect(answer).toBe("");
    expect(sent).toHaveLength(1);
  });

  test("a 201 Dispatch replayed from the key settles once its body is read", async () => {
    const { fetchImpl, sent } = scripted([replayed()]);

    await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
      posted
    );
    expect(sent).toHaveLength(1);
  });

  // Every Dispatch write answers JSON or a bodiless 204 or 205 (`WriteJSON`, `api/server.go`), so
  // a 2xx with an empty body or a body that is not JSON is something in Dispatch's place, a captive
  // portal's 200 for one, and ends unsettled with its key, never as a success.
  test.each([
    ["a portal's 200 page", () => page(200, "OK")],
    [
      "an empty 200 that says JSON",
      () => new Response("", { status: 200, headers: { "Content-Type": "application/json" } }),
    ],
    ["a 201 in plain text", () => new Response("created", { status: 201 })],
  ] as const)("%s leaves the write unsettled with its key", async (_, answer) => {
    const { fetchImpl, sent } = scripted([answer()], { echo: false });

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect((error as DispatchWriteUnsettledError).last.message).toContain(
      "though every Dispatch write answers JSON, so something in Dispatch's place answered"
    );
    expect(sent).toHaveLength(1);
  });

  // A 2xx whose body breaks off is asked again on its Retry-After, and the replay settles it.
  test("a 201 whose body breaks off is asked again on its Retry-After", async () => {
    const broken = (signal: AbortSignal | null | undefined) => {
      const response = partBody("broken")(signal);
      response.headers.set("Retry-After", "3");
      return response;
    };
    const { fetchImpl, sent } = scripted([broken, replayed()]);
    const { retry, pauses } = pacing();

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(2);
    expect(pauses).toEqual([3_000]);
  });

  // A credential refused for good (a sign-in its issuer ended) ends the write at once, in its own
  // words: no presentation after it can succeed, and nothing was sent.
  test("a credential refused for good ends the write at once with its refusal", async () => {
    let presented = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presented += 1;
        throw new SignInEndedError("https://issuer.test", "person@example.test");
      },
      secret: () => "",
    };
    const { fetchImpl, sent } = scripted([]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    );

    expect(error).toBeInstanceOf(SignInEndedError);
    expect(sent).toHaveLength(0);
    expect(presented).toBe(1);
  });

  // A5 (the review of be17859eca): Dispatch keys a write per login, the person or the service and
  // never the session (`requestKeyLogin`, request_keys.go), so a repeat as another person or as
  // the machine is a new key there and can run the write a second time. The client's pin is
  // stricter on purpose: it cannot tell whether another session trades as the same person. Once
  // an attempt may have landed, a presentation as another identity than the write's ends the
  // write unsettled with its key, and nothing is presented again; before that, a repeat reaches
  // no handler, and the write goes as the new identity.
  const switching = () => {
    let login = "person-a";
    let presented = 0;
    const byHeaders = new WeakMap<object, string>();
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presented += 1;
        const headers = { Authorization: `DPoP ${login}-token`, DPoP: "proof" };
        byHeaders.set(headers, login);
        return headers;
      },
      identity: (presentedHeaders) => byHeaders.get(presentedHeaders),
      secret: () => `${login}-token`,
    };
    return {
      credential,
      switchTo: (next: string) => {
        login = next;
      },
      presentations: () => presented,
    };
  };

  test("a repeat that would present as another login after an attempt that may have landed ends the write unsettled, carrying its key", async () => {
    const { credential, switchTo, presentations } = switching();
    const { fetchImpl, sent } = scripted([
      () => {
        switchTo("person-b");
        return refusal(503, "UNAVAILABLE", "the database is restarting");
      },
      json(posted, 201),
    ]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(1);
    expect(presentations()).toBe(2);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
    expect((error as DispatchWriteUnsettledError).key).toBe(
      sent[0]?.headers["Idempotency-Key"] as string
    );
    expect((error as DispatchWriteUnsettledError).last.message).toContain(
      "would present as another identity than the attempt before it"
    );
  });

  test("before anything may have landed, a repeat as another login goes as that login", async () => {
    const { credential, switchTo } = switching();
    const { fetchImpl, sent } = scripted([
      () => {
        switchTo("person-b");
        throw refused();
      },
      json(posted, 201),
    ]);

    await expect(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    ).resolves.toEqual(posted);

    expect(sent.map((request) => request.headers.Authorization)).toEqual([
      "DPoP person-a-token",
      "DPoP person-b-token",
    ]);
  });

  // T2 (the review of 4dad905e68): the identity a write is pinned to is the one its attempts
  // last went out as. A switch before anything may have landed moves it, so once the new
  // identity's attempt may have landed, a repeat as that identity is the same write and goes on.
  test("a switch before a landing moves the pin, so a repeat as the new identity after its landing goes on", async () => {
    const { credential, switchTo } = switching();
    const { fetchImpl, sent } = scripted([
      () => {
        switchTo("person-b");
        throw refused();
      },
      refusal(503, "UNAVAILABLE", "the database is restarting"),
      json(posted, 201),
    ]);

    await expect(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    ).resolves.toEqual(posted);

    expect(sent.map((request) => request.headers.Authorization)).toEqual([
      "DPoP person-a-token",
      "DPoP person-b-token",
      "DPoP person-b-token",
    ]);
  });

  // F1 (the review of be17859eca): the failure a write ends with is the latest of its
  // credential's, never an older one: a presentation that the caller's stop ends while it throws
  // its own error ends the write with that error, and one that answered before the stop leaves no
  // earlier failure behind.
  test("a stop during a later presentation ends the write with that presentation's own error, not an earlier one", async () => {
    const controller = new AbortController();
    let presented = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presented += 1;
        if (presented === 1) {
          throw new TradeRetryingError("the secrets broker answered 503 temporarily_unavailable");
        }
        controller.abort(new DOMException("The operation was aborted.", "AbortError"));
        throw new RegistrationPendingError(
          "ses_pod",
          new BrokerRefusedError(
            400,
            "pod_session_not_registered",
            "pod_session_not_registered: No registration says this pod may write as ses_pod",
            "this pod"
          )
        );
      },
      secret: () => "",
    };
    const { fetchImpl, sent } = scripted([]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        signal: controller.signal,
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(0);
    expect(error).toBeInstanceOf(RegistrationPendingError);
  });

  test("a stop right after a presentation answered ends the write with the stop, not an earlier credential failure", async () => {
    const controller = new AbortController();
    let presented = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presented += 1;
        if (presented === 1) {
          throw new TradeRetryingError("the secrets broker answered 503 temporarily_unavailable");
        }
        controller.abort(new DOMException("The operation was aborted.", "AbortError"));
        return { Authorization: "DPoP live", DPoP: "proof" };
      },
      secret: () => "live",
    };
    const { fetchImpl, sent } = scripted([]);

    const error = await failure(
      new DispatchClient("http://dispatch.test", credential, fetchImpl, {
        signal: controller.signal,
        writeRetry: pacing().retry,
      }).comment("DSP-1", comment)
    );

    expect(sent).toHaveLength(0);
    expect(error).not.toBeInstanceOf(TradeRetryingError);
    expect((error as Error).name).toBe("AbortError");
  });

  // F4 (the review of be17859eca): a JSON answer is one whose media type is `application/json` or
  // ends in `+json`, letter case aside, as Dispatch's own rule reads it (`compressibleAnswer`,
  // response_compression.go); so a proxy that rewrites the type's case leaves Dispatch's answer
  // Dispatch's.
  test.each([
    "Application/JSON; charset=utf-8",
    "application/vnd.dispatch+json",
  ])("a 201 typed %s settles the write as Dispatch's answer", async (type) => {
    const { fetchImpl, sent } = scripted([
      (signal) => {
        void signal;
        const key = sent[0]?.headers["Idempotency-Key"] ?? "";
        return new Response(JSON.stringify(posted), {
          status: 201,
          headers: { "Content-Type": type, "Idempotency-Key": key },
        });
      },
    ]);

    await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
      posted
    );
    expect(sent).toHaveLength(1);
  });

  // While no answer of the call carried the key back, a 2xx with a valid JSON body settles the
  // write without one: nothing in this call shows that Dispatch's keyed layer answered it.
  test("a 2xx without the key settles the write while no answer of the call carried the key", async () => {
    const { fetchImpl } = scripted([json(posted, 201)], { echo: false });

    await expect(client(fetchImpl, pacing().retry).comment("DSP-1", comment)).resolves.toEqual(
      posted
    );
  });

  // Dispatch's keyed layer answers every keyed write with its key, so once an answer of the call
  // carried it back, a 2xx without exactly that key is something in Dispatch's place (a gateway's
  // own JSON 201), which says nothing of whether the write ran: unsettled, carrying the key. The
  // evidence is the call's own answers: no probe, and nothing kept across calls.
  test.each([
    {
      name: "without the key leaves the write unsettled",
      echoes: false,
      outcome: (key: string) => ({
        error: expect.objectContaining({
          name: "DispatchWriteUnsettledError",
          key,
          message: expect.stringContaining(
            "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 201 without carrying " +
              "back its Idempotency-Key, though an earlier answer of this write carried it and " +
              "Dispatch answers every keyed write with it, so something in Dispatch's place answered"
          ),
        }),
      }),
    },
    { name: "with the key settles the write", echoes: true, outcome: () => ({ value: posted }) },
  ])("an echoed 503, then a JSON 201 $name", async ({ echoes, outcome }) => {
    const { fetchImpl, sent } = scripted(
      [refusal(503, "UNAVAILABLE", "the database is restarting"), json(posted, 201)],
      { echo: (request) => request === 1 || echoes }
    );

    const settled = await client(fetchImpl, pacing().retry)
      .comment("DSP-1", comment)
      .then(
        (value) => ({ value }),
        (error: unknown) => ({ error })
      );

    expect(sent).toHaveLength(2);
    const key = sent[0]?.headers["Idempotency-Key"] as string;
    expect(sent[1]?.headers["Idempotency-Key"]).toBe(key);
    expect(settled).toEqual(outcome(key));
  });

  // F5: a write behind a redirect names the origin the redirect pointed at, which is what tells a
  // Dispatch URL to correct; the redirect is not followed.
  test("a redirect's refusal names the origin it points at", async () => {
    const { fetchImpl } = scripted([
      new Response("moved", {
        status: 301,
        statusText: "Moved Permanently",
        headers: { Location: "https://user:secret@elsewhere.test:8443/api/v1/x?token=abc" },
      }),
    ]);

    const error = await failure(client(fetchImpl, pacing().retry).comment("DSP-1", comment));

    expect(error).toBeInstanceOf(DispatchGatewayError);
    expect(error.message).toStartWith(
      "POST http://dispatch.test/api/v1/issues/DSP-1/comments answered 301 Moved Permanently (a " +
        "redirect to https://elsewhere.test:8443, which this client does not follow) with a body"
    );
    expect(error.message).not.toContain("secret");
    expect(error.message).not.toContain("token=abc");
  });

  test("a silent attempt is asked again under its key without a probe", async () => {
    const { retry, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, replayed()]);

    await expect(client(fetchImpl, retry).comment("DSP-1", comment)).resolves.toEqual(posted);

    expect(sent).toHaveLength(2);
  });

  // The client's 60 s deadline counts from when it was made, so a write that begins late in the
  // client's life asks no later than 55 s after that, not 55 s after its own first attempt.
  test("no ask starts more than 55 s after the client was made", async () => {
    const { retry, advance, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => inFlight())]);
    const dispatch = client(fetchImpl, retry);
    // Earlier calls of the same client took 30 s of its deadline.
    advance(30_000);

    const error = await failure(dispatch.comment("DSP-1", comment));

    // Cut at 50 s, then asked at 51 and 53 s; the next ask would start at 56 s.
    expect(sent).toHaveLength(3);
    expect(error.message).toBe(
      "KEY_IN_FLIGHT (the last of 3 " +
        "attempts), and Dispatch was still running an earlier attempt of the write, so it may or " +
        `may not have taken effect${keyedTail}`
    );
  });

  // A timer can fire late on a loaded host, so a pause that ends past the bound starts no ask.
  test("a pause that ends past the bound starts no ask", async () => {
    const { retry, advance, clock, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => inFlight())]);
    const late: DispatchWriteRetry = {
      ...retry,
      pause: async (ms) => {
        advance(ms);
        if (clock() > 52_000) advance(3_000);
      },
    };

    await failure(client(fetchImpl, late).comment("DSP-1", comment));

    // Asked at 0, 21, 23, then every 3 s to 50 s; the pause after 50 s ends at 56 s.
    expect(sent).toHaveLength(12);
    expect(clock()).toBe(56_000);
  });

  // Each repeat of an upload sends its whole body again, so an upload keeps the schedule alone.
  test("an upload Dispatch may still be running keeps the five-attempt schedule", async () => {
    const { retry, pauses, stall } = stalling();
    const { fetchImpl, sent } = scripted([stall, ...Array.from({ length: 20 }, () => inFlight())]);

    const error = await failure(
      client(fetchImpl, retry).artifact("DSP-1", {
        name: "build.tar",
        file: new Blob([new Uint8Array(1 << 20)]),
        actor,
      })
    );

    // Cut at 20 s, then asked at 21, 23 and 27 s; 8 s more would pass 30 s.
    expect(sent).toHaveLength(4);
    expect(pauses).toEqual([1_000, 2_000, 4_000]);
    expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
  });

  test("a retried upload sends the bytes and boundary its first attempt sent", async () => {
    const { fetchImpl, sent } = scripted([
      page(502, "Bad Gateway"),
      json({ artifact: { id: "artifact-1" } }, 201),
    ]);

    await client(fetchImpl, pacing().retry).artifact("DSP-1", {
      name: "spec.md",
      file: new Blob(["# Spec"], { type: "text/markdown" }),
      summary: "Initial spec",
      actor,
    });

    expect(sent).toHaveLength(2);
    const [first, second] = sent as [Sent, Sent];
    expect(second.headers["Content-Type"]).toBe(first.headers["Content-Type"] as string);
    expect(first.headers["Content-Type"]).toStartWith("multipart/form-data; boundary=");
    expect(second.headers["Idempotency-Key"]).toBe(first.headers["Idempotency-Key"] as string);
    expect(first.body).toBeInstanceOf(Uint8Array);
    expect(second.body).toBe(first.body);
    const form = await new Response(first.body, {
      headers: { "Content-Type": first.headers["Content-Type"] as string },
    }).formData();
    expect(form.get("name")).toBe("spec.md");
    expect(form.get("summary")).toBe("Initial spec");
    expect(form.get("actor")).toBe(JSON.stringify(actor));
    expect(await (form.get("file") as Blob).text()).toBe("# Spec");
  });

  // A write Dispatch made whose answer is gone is no refusal: a replay of a first answer larger
  // than Dispatch keeps (2xx ANSWER_NOT_KEPT), or 409 KEY_COMMITTED (the write landed before its
  // answer was lost, and Dispatch will not run it again). It is never asked again, and its one line
  // says what was made and to read it back.
  test("a write whose answer is gone is a landing, never resent", async () => {
    const url = "http://dispatch.test/api/v1/issues/DSP-1/comments";
    for (const [answer, code, status, lost] of [
      [
        json({ code: IDEMPOTENCY_ANSWER_NOT_KEPT, error: "not kept" }, 201, {
          "Idempotency-Replayed": "?1",
        }),
        IDEMPOTENCY_ANSWER_NOT_KEPT,
        201,
        "Dispatch kept no copy of its answer, which was too large",
      ],
      [
        refusal(409, IDEMPOTENCY_KEY_COMMITTED, "the write landed and its answer was lost"),
        IDEMPOTENCY_KEY_COMMITTED,
        409,
        "its answer was lost on the way back",
      ],
    ] as const) {
      const { fetchImpl, sent } = scripted([
        refusal(503, "UNAVAILABLE", "database unavailable"),
        answer,
        json(posted, 201),
      ]);
      const { retry, pauses } = pacing();

      const error = await failure(client(fetchImpl, retry).comment("DSP-1", comment));

      expect(sent).toHaveLength(2);
      expect(pauses).toEqual([1_000]);
      expect(error).toBeInstanceOf(DispatchWriteLandedError);
      expect(error).not.toBeInstanceOf(DispatchServiceError);
      expect(error).toEqual(expect.objectContaining({ code, status }));
      expect(error.message).toBe(
        `POST ${url} was made, but ${lost} (${code}), so this command cannot print what it made: ` +
          "read it back with `dispatch read`, or with the matching list for something it created."
      );
    }
  });

  test("the production pacing is five attempts within 30 s, each cut off after 20 s of silence, and asks every 3 s until 55 s while Dispatch may be running the write", () => {
    expect(DISPATCH_WRITE_RETRY).toEqual({
      pausesMs: [1_000, 2_000, 4_000, 8_000],
      withinMs: 30_000,
      silenceMs: 20_000,
      inFlightPauseMs: 3_000,
      inFlightWithinMs: 55_000,
    });
  });
});

/** How one write's connection to `socketDispatch` takes its body. */
type Intake = "full" | "stall" | { readonly pauses: number; readonly pauseMs: number };

/** One write `socketDispatch` took: the key it carried, its `Content-Length` and what arrived. */
interface Taken {
  readonly key: string | undefined;
  readonly length: number;
  received: number;
}

/** Answers `socket` with `status` and a JSON `body`, carrying back `key`, and closes it. */
function answerOn(socket: Socket, status: number, body: unknown, key?: string): void {
  const text = JSON.stringify(body);
  socket.end(
    `HTTP/1.1 ${status} ${status === 201 ? "Created" : "OK"}\r\n` +
      "Content-Type: application/json\r\n" +
      (key === undefined ? "" : `Idempotency-Key: ${key}\r\n`) +
      `Content-Length: ${Buffer.byteLength(text)}\r\nConnection: close\r\n\r\n${text}`
  );
}

/**
 * A Dispatch on a real socket, so a request's body goes out at the pace the connection takes it.
 * Each write's connection takes its body as the next of `intakes` says - at once, in a fixed number
 * of paced steps, or nothing past the first of it - and answers 201 carrying back its key once it
 * holds all of it.
 */
async function socketDispatch(intakes: readonly Intake[]) {
  const writes: Taken[] = [];
  const sockets = new Set<Socket>();
  const server = createServer((socket) => {
    sockets.add(socket);
    // Closing the server while a client is still writing destroys this socket, and its error would
    // otherwise be an unhandled error that reddens the run and the row after this one.
    socket.on("error", () => {});
    socket.on("close", () => sockets.delete(socket));
    let head = Buffer.alloc(0);
    let taken: Taken | undefined;
    let intake: Intake = "full";
    let paused = 0;
    socket.on("data", (data: Buffer) => {
      let chunk = data;
      if (taken === undefined) {
        head = Buffer.concat([head, chunk]);
        const end = head.indexOf("\r\n\r\n");
        if (end === -1) return;
        const lines = head.subarray(0, end).toString("latin1").split("\r\n");
        const header = (name: string): string | undefined =>
          lines
            .find((line) => line.toLowerCase().startsWith(`${name}:`))
            ?.slice(name.length + 1)
            .trim();
        taken = {
          key: header("idempotency-key"),
          length: Number(header("content-length")),
          received: 0,
        };
        intake = intakes[writes.length] ?? "full";
        writes.push(taken);
        chunk = head.subarray(end + 4);
      }
      taken.received += chunk.length;
      if (intake === "stall") {
        // What the connection took is counted before it stops, so a row can tell an attempt that
        // went out and was cut off part way from one that never started. It stops once the first
        // of the body is in, which is with the headers when they came together.
        if (taken.received > 0) socket.pause();
        return;
      }
      if (taken.received >= taken.length) {
        answerOn(socket, 201, { artifact: { id: "artifact-1" } }, taken.key);
      } else if (typeof intake === "object") {
        // A step of the body, not a rate: what must stay under the write's silence bound is the
        // length of one wait, and a fixed number of waits is a fixed number of chances to pass it.
        // The waits sit in the first half of the body, so however much of it the socket buffers
        // already hold when the last wait ends, the connection is still taking bytes and the client
        // still sees progress.
        const step = taken.length / 2 / intake.pauses;
        if (paused < intake.pauses && taken.received >= (paused + 1) * step) {
          paused++;
          socket.pause();
          const resume = setTimeout(() => socket.resume(), intake.pauseMs);
          socket.once("close", () => clearTimeout(resume));
        }
      }
    });
  });
  const { promise: listening, resolve } = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", resolve);
  await listening;
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    writes,
    close(): void {
      for (const socket of sockets) socket.destroy();
      server.close();
    },
  };
}

// An attempt's silence bound counts the connection taking its body as progress, so an upload is
// bounded by how long it goes unheard, never by how long it takes; one whose connection stops
// taking it is cut off and sent again under its key. Only a real socket shows that: the pace is
// the kernel's backpressure on the connection, which no fake clock drives, so these run in real
// time.
describe("DispatchClient's uploads on a real connection", () => {
  const opened: { close(): void }[] = [];

  // The row that hits its hang guard never reaches the end of its own body, so this hook is what
  // closes its server and its sockets; left open, their errors would land on the row after it. It
  // is the one owner of the close, which is why no row closes its own server.
  afterEach(() => {
    for (const made of opened.splice(0)) made.close();
  });

  const dispatchOn = async (intakes: readonly Intake[]) => {
    const made = await socketDispatch(intakes);
    opened.push(made);
    return made;
  };

  const upload = (url: string, silenceMs: number, size: number) =>
    new DispatchClient(url, bearerCredential("secret"), fetch, {
      writeRetry: { ...DISPATCH_WRITE_RETRY, silenceMs },
    }).artifact("DSP-1", {
      name: "build.tar",
      file: new Blob([new Uint8Array(size).fill(97)]),
      actor,
    });

  // The body goes out in four paced steps of 2,200 ms, so it is on the connection for about 8,800
  // ms against a silence bound of 8,000 ms and still lands in one attempt. The pace is a fixed
  // number of steps rather than a rate because every step is a wait a busy machine can stretch,
  // and what matters is how far one wait has to stretch before it passes the bound. At 8 KiB a ms
  // this row made 384 waits of 8 ms against a 1,000 ms bound, and one of them passing it cut the
  // attempt off and made the upload land in two. Four waits of 450 ms against a 1,500 ms bound
  // left 1,050 ms, and that was still not enough: in a 2-CPU pod under 16 busy loops (8 x nproc)
  // the row failed 1 whole-suite run of 20 at 5,207.83 ms with `toHaveLength(1)` receiving 2. The
  // overshoot is not the timer, which was late by 224.9 ms at most over 180 rounds under the same
  // loops; it is the suite process's own work while 24 MiB streams through it, which only a
  // whole-suite run has. Four waits of 2,200 ms against 8,000 ms leave 5,800 ms, five and a half
  // times that overshoot, and keep the number of waits at four. The body stays at 24 MiB, and the
  // steps sit in its first half, because what the client counts as progress is the connection
  // taking its bytes: a body the socket buffers swallow whole leaves the client silent for every
  // pause that follows, and measured in this pod a 4 MiB body was swallowed whole and cut off 3
  // runs of 3 under that load. The 60 s limit is a hang guard, not a budget: it fires only if the
  // server's resume or the client's stream never comes. Measured in that pod: 9.01 s idle; under
  // those 16 loops, 9.62 s at most over the three whole-suite runs that sized the step and 11.25 s
  // over the twenty that proved it, since the row's cost is mostly its own pacing.
  test("an upload going out for longer than silenceMs lands in one attempt", async () => {
    const silenceMs = 8_000;
    const dispatch = await dispatchOn([{ pauses: 4, pauseMs: 2_200 }]);
    const started = performance.now();
    await upload(dispatch.url, silenceMs, 24 << 20);

    expect(performance.now() - started).toBeGreaterThan(silenceMs);
    expect(dispatch.writes).toHaveLength(1);
    expect(dispatch.writes[0]?.received).toBe(dispatch.writes[0]?.length as number);
  }, 60_000);

  // The claim is that an upload its connection stops taking is cut off and sent again under its key,
  // not that exactly two attempts do it: a second attempt a busy machine also stalls is cut off too,
  // and the client is right to send a third, which is how this row failed. So it asserts what it
  // means - the first attempt reached the server incomplete, the last one landed whole, every
  // attempt carried the one key - and not the count, which is the machine's answer rather than the
  // client's. The body is 16 MiB rather than 2 MiB, twice `STREAMED_BODY_MIN`, because the
  // socket buffers in this pod took 2 MiB and 512 bytes whole 1 round of 5: at that size the first
  // attempt can leave the client entirely, and what cuts it off is then the silence after the body
  // went rather than a connection that stopped taking it. At 16 MiB the connection stops taking it
  // part way, which is the row's subject. No assertion sees how much of the body left the client,
  // so that size is what keeps the stall in the middle of the upload: `received` shows only that
  // the server read the first of the attempt and then stopped, so it is above nothing and below
  // the body. The silence bound is 1,000 ms so a stall of the machine's own does not cut a healthy
  // attempt. The 30 s limit is a hang guard, not a budget: it fires only if the resend never lands.
  // Measured in a 2-CPU pod: 2.18 s idle; under 16 busy loops (8 x nproc), 3.68 s at most in a
  // whole-suite run.
  test("an upload its connection stops taking is cut off and sent again under its key", async () => {
    const silenceMs = 1_000;
    const dispatch = await dispatchOn(["stall", "full"]);
    const started = performance.now();
    await upload(dispatch.url, silenceMs, 16 << 20);

    expect(performance.now() - started).toBeGreaterThanOrEqual(silenceMs);
    const { writes } = dispatch;
    expect(writes.length).toBeGreaterThan(1);
    const first = writes[0] as Taken;
    const landed = writes.at(-1) as Taken;
    expect(first.received).toBeGreaterThan(0);
    expect(first.received).toBeLessThan(first.length);
    expect(landed.received).toBe(landed.length);
    expect(first.key).toMatch(IDEMPOTENCY_KEY_PATTERN);
    for (const write of writes) expect(write.key).toBe(first.key as string);
  }, 30_000);
});
