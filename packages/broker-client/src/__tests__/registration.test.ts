// packages/broker-client/src/__tests__/registration.test.ts
//
// A pod's trade can reach the broker before the registration that lets that pod write under its
// session name does: its daemon registers at the claim, a session's watch at the pod's creation,
// and either registrar can be down for minutes. The broker refuses that trade `invalid_grant` with
// an `error_description` beginning `pod_session_not_registered:`, and that one refusal is never
// final: `token()` trades again, 15 s doubling to 5 minutes, for as long as its caller waits, and
// says so once, naming the session, and again every 30 minutes while it lasts. Every other refusal
// stays as final as it was. The Go half is `registration_test.go` in
// `packages/envoy/internal/brokerclient`.

import { expect, test } from "bun:test";
import { newDpopKey } from "../dpop";
import type { SubjectToken, TokenSource } from "../source";
import {
  BrokerRefusedError,
  type BrokerToken,
  BrokerTokenClient,
  isFinalRefusal,
  RegistrationPendingError,
} from "../token-client";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted, type StubAnswer, type StubTrade, stubBroker } from "./stub";

/** The session name a pod of these tests trades under. */
const SESSION = "implementer-g1";

/** The broker's refusal of a pod whose session no registration names yet, in the broker's own
 *  words (`registeredPodSession`, packages/envoy/internal/broker/oauth/exchange.go). */
const UNREGISTERED = `pod_session_not_registered: No registration says this pod may write under the session name '${SESSION}'. A pod writes under a session name its deployment registered for it.`;

function unregistered(): StubAnswer {
  return { status: 400, body: { error: "invalid_grant", error_description: UNREGISTERED } };
}

/** A pod's projected token, read afresh at each trade. */
const POD: TokenSource = {
  read: async (): Promise<SubjectToken> => ({
    token: "projected.service.account.token",
    tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES,
  }),
  describe: () => "a pod's projected service-account token",
};

/** What one pod's wait for its registration did: the trades the broker saw, the waits between
 *  them, the lines the client logged with the clock at each, and how `token()` ended. */
interface Waited {
  readonly trades: StubTrade[];
  readonly waits: number[];
  readonly logged: { readonly at: number; readonly line: string }[];
  readonly token: BrokerToken | undefined;
  readonly refusal: unknown;
}

/**
 * A pod's client against a broker that refuses its session `refusals` times as unregistered and
 * then answers it. Every wait the client asks for passes at once and moves the clock by its
 * length, so a wait of hours runs at the test's speed.
 */
async function waitForRegistration(refusals: number): Promise<Waited> {
  const broker = stubBroker((_trade, count) =>
    count <= refusals ? unregistered() : { body: accepted("token-once-registered", 600) }
  );
  const clock = { now: Date.now() };
  const waits: number[] = [];
  const logged: { at: number; line: string }[] = [];
  const client = new BrokerTokenClient({
    brokerUrl: broker.url,
    key: await newDpopKey(),
    subject: POD,
    session: SESSION,
    now: () => clock.now,
    sleep: async (ms: number) => {
      waits.push(ms);
      clock.now += ms;
    },
    log: (line: string) => logged.push({ at: clock.now, line }),
  });
  try {
    let token: BrokerToken | undefined;
    let refusal: unknown;
    try {
      token = await client.token();
    } catch (error) {
      refusal = error;
    }
    return { trades: broker.trades, waits, logged, token, refusal };
  } finally {
    client.close();
    broker.stop();
  }
}

test("a pod trade waits for its registration however long it takes", async () => {
  const refusals = 7;
  const { trades, waits, logged, token, refusal } = await waitForRegistration(refusals);
  expect(refusal, `the pod's session ${SESSION} was refused for good: ${refusal}`).toBeUndefined();
  expect(token?.accessToken).toBe("token-once-registered");
  // Each refusal, and the trade that landed, each under the pod's session and naming no
  // projects: a pod's entry decides its project.
  expect(trades.length).toBe(refusals + 1);
  for (const trade of trades) {
    expect(trade.form.get("session")).toBe(SESSION);
    expect(trade.form.has("projects")).toBe(false);
  }
  // 15 s doubling to a cap of 5 minutes, across more than the 5 minutes a client that gave up
  // then would have lost.
  expect(waits).toEqual([15_000, 30_000, 60_000, 120_000, 240_000, 300_000, 300_000]);
  expect(waits.reduce((sum, wait) => sum + wait, 0)).toBeGreaterThan(5 * 60_000);
  // Said once, naming the session, within the half hour.
  expect(logged.length).toBe(1);
  expect(logged[0]?.line).toContain(SESSION);
});

test("an unregistered session is logged once, and again every 30 minutes while it lasts", async () => {
  // Seventeen refusals across 3765 s: logged at the first, at the first 30 minutes after that,
  // and at the first 30 minutes after that again.
  const { token, logged } = await waitForRegistration(17);
  expect(token?.accessToken).toBe("token-once-registered");
  expect(logged.length).toBe(3);
  for (const [i, { line, at }] of logged.entries()) {
    expect(line).toContain(SESSION);
    if (i > 0) expect(at - (logged[i - 1]?.at as number)).toBeGreaterThanOrEqual(30 * 60_000);
  }
});

/**
 * A pod's client against a broker that answers the session once `registered()` says so, whose
 * waits pass only when the client cuts them short: a call arriving, or `close()`. Nothing passes
 * on a timer, so what a test sees is what the client did, at the test's own pace.
 */
async function heldClient(registered: () => boolean) {
  const broker = stubBroker(() =>
    registered() ? { body: accepted("token-once-registered", 600) } : unregistered()
  );
  const waits: number[] = [];
  const client = new BrokerTokenClient({
    brokerUrl: broker.url,
    key: await newDpopKey(),
    subject: POD,
    session: SESSION,
    sleep: (ms: number, signal?: AbortSignal) => {
      waits.push(ms);
      const { promise, resolve } = Promise.withResolvers<void>();
      if (signal?.aborted) resolve();
      signal?.addEventListener("abort", () => resolve(), { once: true });
      return promise;
    },
    log: () => {},
  });
  return { broker, client, waits };
}

/** Turns of the event loop until `ready()` holds: no timer, so the test runs at the client's pace. */
async function until(ready: () => boolean): Promise<void> {
  while (!ready()) await new Promise((resolve) => setImmediate(resolve));
}

/** What `call` settled with by the next turn of the event loop, or "pending": the test's bound. */
async function outcome(call: Promise<BrokerToken>): Promise<unknown> {
  const bound = Promise.withResolvers<"pending">();
  setImmediate(() => bound.resolve("pending"));
  return await Promise.race([
    call.then(
      (token) => token,
      (error: unknown) => error
    ),
    bound.promise,
  ]);
}

test("a call past its deadline is told its session's registration is pending, and the next call takes the token once it lands", async () => {
  let registered = false;
  const { broker, client, waits } = await heldClient(() => registered);
  try {
    const deadline = new AbortController();
    const call = client.token(deadline.signal);
    await until(() => waits.length === 1);
    deadline.abort();
    const refusal = await outcome(call);
    expect(refusal).toBeInstanceOf(RegistrationPendingError);
    expect((refusal as RegistrationPendingError).session).toBe(SESSION);
    expect((refusal as Error).message).toContain(SESSION);
    expect((refusal as Error).message).toContain("retried");
    expect(isFinalRefusal(refusal)).toBe(false);
    // The trade goes on: the client is still waiting, and asked nothing more.
    expect(broker.trades.length).toBe(1);
    registered = true;
    const next = await client.token(new AbortController().signal);
    expect(next.accessToken).toBe("token-once-registered");
    expect(broker.trades.length).toBe(2);
  } finally {
    client.close();
    broker.stop();
  }
});

test("a call that joins a wait under way stops it with its own signal, and a call with no deadline does not return", async () => {
  let registered = false;
  const { broker, client, waits } = await heldClient(() => registered);
  try {
    const waiting = client.token();
    await until(() => waits.length === 1);
    const deadline = new AbortController();
    const joined = client.token(deadline.signal);
    // Joining cuts the wait short: the client asks again at once, is refused, and waits again.
    await until(() => waits.length === 2);
    deadline.abort();
    expect(await outcome(joined)).toBeInstanceOf(RegistrationPendingError);
    // The control: the call that passed no deadline is still waiting.
    expect(await outcome(waiting)).toBe("pending");
    registered = true;
    expect((await client.token()).accessToken).toBe("token-once-registered");
    expect((await waiting).accessToken).toBe("token-once-registered");
    expect(broker.trades.length).toBe(3);
  } finally {
    client.close();
    broker.stop();
  }
});

/** Collects garbage across a few turns of the event loop, so what nothing holds is gone. */
async function collected(): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((resolve) => setImmediate(resolve));
    Bun.gc(true);
  }
  await new Promise((resolve) => setImmediate(resolve));
}

test("calls past their deadline are not kept by the trade they stopped waiting for", async () => {
  // The review's finding (e0-deep-review-t-e-30.md, finding 1): a trade waiting out a missing
  // registration can stay pending for the session's life, and each call that joined it and
  // stopped at its deadline — each listener heartbeat, each Dispatch request — must be free to
  // go once it has its answer. Ten calls stop; the trade keeps waiting.
  const calls = 10;
  const { broker, client, waits } = await heldClient(() => false);
  const waiting = client.token().catch(() => undefined);
  try {
    await until(() => waits.length === 1);
    // Counted by a FinalizationRegistry, which keeps nothing alive: a WeakRef's target is also
    // kept until the job that made it ends, which would count the last call however it is held.
    const collectedCalls = new Set<number>();
    const registry = new FinalizationRegistry<number>((call) => collectedCalls.add(call));
    // Each call is made in a function of its own, whose frame ends with it: the collector reads a
    // live frame's registers conservatively, so one left running would hold its last answer.
    const stopCall = async (call: number): Promise<void> => {
      const deadline = new AbortController();
      const before = waits.length;
      const joined = client.token(deadline.signal);
      await until(() => waits.length === before + 1);
      deadline.abort();
      const refusal = await joined.catch((error: unknown) => error);
      if (!(refusal instanceof RegistrationPendingError)) {
        throw new Error(`stopped call ${call} was answered ${String(refusal)}`);
      }
      registry.register(refusal, call);
    };
    for (let call = 0; call < calls; call++) await stopCall(call);
    await collected();
    const kept = calls - collectedCalls.size;
    expect(
      kept,
      `${kept} of ${calls} calls past their deadline are kept by the trade they stopped waiting for`
    ).toBe(0);
  } finally {
    client.close();
    await waiting;
    broker.stop();
  }
});

test("every other refusal stays permanent", () => {
  const identity = "a pod's projected service-account token";
  const rows: [number, string, string, boolean][] = [
    [400, "invalid_grant", UNREGISTERED, false],
    [429, "invalid_request", "too many token requests", false],
    [400, "invalid_dpop_proof", "", false],
    [400, "use_dpop_nonce", "", false],
    [400, "invalid_grant", "no entry lists the account this token names", true],
    [400, "invalid_grant", `The session name '${SESSION}' belongs to another identity.`, true],
    [400, "invalid_grant", "ada@example.com revoked this machine's sign-in", true],
    // The same words under another code, past the start, or with no colon are not the refusal.
    [400, "invalid_request", UNREGISTERED, true],
    [400, "invalid_grant", `the refusal was: ${UNREGISTERED}`, true],
    [400, "invalid_grant", "pod_session_not_registered", true],
    [401, "invalid_client", "", true],
    [
      400,
      "invalid_request",
      "The subject token was refused: its audience is not this broker's.",
      true,
    ],
  ];
  for (const [status, code, description, permanent] of rows) {
    const refused = new BrokerRefusedError(status, code, description, identity);
    expect(refused.permanent, `HTTP ${status} ${code} ${JSON.stringify(description)}`).toBe(
      permanent
    );
    expect(isFinalRefusal(refused), `HTTP ${status} ${code} ${JSON.stringify(description)}`).toBe(
      permanent
    );
  }
});
