// packages/envoy-client/src/__tests__/dispatch-credential-deadline.test.ts
//
// A Dispatch call hands its credential the client's own deadline less the request's reserve
// (`credentialDeadline`), as an Envoy call hands it its attempt's
// (transport-credential-deadline.test.ts). A broker credential whose renewal the broker leaves
// unanswered then answers with the live token in hand at the end of its share, and the request
// still has its reserve to be sent and answered, rather than going out on a deadline the
// credential has already spent.
//
// Each row runs on Bun's fake clock, which the client's deadline, the credential's share of it,
// the write's pauses and the token client's waits all keep time by (`AbortSignal.timeout`,
// `performance.now`, `setTimeout`), while the broker and Dispatch answer over real sockets. A row
// moves the clock only once nothing but a timer is left to happen: the broker holds the trade, or
// the token client has said what its trade came to. So the time a row reads is the code's own,
// to the millisecond, however loaded the machine.

import { afterEach, beforeEach, expect, jest, spyOn, test } from "bun:test";
import {
  RegistrationPendingError,
  TradeRetryingError,
  TradeUnansweredError,
} from "@legion/broker-client/token-errors";
import { turn, turnsUntil } from "@legion/test-fixtures/fake-clock";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { brokerCredential } from "../broker-credential";
import { DispatchClient, forgetKeyedLayerMemory } from "../dispatch-http";
import { brokerStandIns } from "./broker-stand-ins";

const { podEnvironment, servers, stubDispatch } = brokerStandIns("dispatch-credential-deadline");

beforeEach(() => {
  jest.useFakeTimers();
});

// A fake clock left behind stops the timers of every file the same worker runs after this one.
afterEach(() => {
  jest.useRealTimers();
});

/** The session the credential trades under. */
const POD_SESSION = "implementer-g1";

/** A client's own deadline, short so the row shows its share without waiting a whole minute. */
const DEADLINE_MS = 3_000;

/** The request's reserve out of a deadline of 2 s or more (`credentialDeadline`). */
const RESERVE_MS = 1_000;

/** Moves the fake clock on a millisecond at a time, settling what each step set off before the
 *  next, until `done()` holds or `most` milliseconds have passed, and answers how far it moved. A
 *  row calls it only once nothing but a timer is left to happen. */
async function stepUntil(done: () => boolean, most: number): Promise<number> {
  let moved = 0;
  await turn();
  while (!done() && moved < most) {
    jest.advanceTimersByTime(1);
    moved += 1;
    await turn();
  }
  return moved;
}

test("inside the renewal window, a broker that hangs leaves a Dispatch call its live token and the time to send it", async () => {
  let trades = 0;
  // The broker has the renewal: from then on only the credential's share of the deadline is left.
  let renewing = false;
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async () => {
      trades += 1;
      if (trades > 1) {
        renewing = true;
        return await new Promise<Response>(() => {});
      }
      // Good for 200 s: inside the 5-minute renewal window from the first.
      return Response.json({
        access_token: fixtureSecret("live"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 200,
      });
    },
  });
  servers.push(broker);
  const dispatch = stubDispatch();
  const credential = brokerCredential(
    podEnvironment(`http://127.0.0.1:${broker.port}`, dispatch.url),
    POD_SESSION
  );
  // When each request went to Dispatch, on the row's clock.
  const sentAt: number[] = [];
  const timed = (async (input: string | URL | Request, init?: RequestInit) => {
    sentAt.push(performance.now());
    return await fetch(input, init);
  }) as typeof fetch;
  const call = () =>
    new DispatchClient(dispatch.url, credential, timed, { deadlineMs: DEADLINE_MS }).listAllIssues(
      {}
    );
  await call();
  const started = performance.now();
  const second = call();
  let answered = false;
  second.then(
    () => {
      answered = true;
    },
    () => {
      answered = true;
    }
  );
  await turnsUntil(() => renewing, "the renewal the broker holds");
  // A millisecond short of the credential's share, 2 s of the client's 3 s, nothing has gone.
  jest.advanceTimersByTime(DEADLINE_MS - RESERVE_MS - 1);
  await turn();
  expect(sentAt).toHaveLength(1);
  // At the share the call goes with the live token, and the clock stands there until Dispatch
  // answers: a credential that waited longer sends nothing while it stands, which the wait names.
  jest.advanceTimersByTime(1);
  await turnsUntil(
    () => sentAt.length === 2,
    "the call sent at the credential's share of its deadline, 2 s of 3",
    { boundMs: 10_000 }
  );
  await turnsUntil(() => answered, "Dispatch's answer to the call", { boundMs: 10_000 });
  await second;
  expect(sentAt.slice(1).map((at) => at - started)).toEqual([DEADLINE_MS - RESERVE_MS]);
  expect(trades).toBe(2);
  const live = dispatch.asked[0]?.authorization;
  expect(live).toStartWith("DPoP ");
  expect(dispatch.asked.map((asked) => asked.authorization)).toEqual([live, live]);
}, 20_000);

// R3-99b1 (the review of 99b1e438db): a write whose credential cannot present ends with the
// credential's own error, within the client's deadline, whatever that deadline is. A registration
// still pending and a trade still unanswered are told only once the credential's share of the
// deadline has ended, so the write ends there; the fail-fast 503 is presented again on the
// write's pacing, and the deadline that ends the pause ends the write with the credential's
// failure, never the pause's own "The operation timed out.". Nothing reaches Dispatch.
test.each([
  ["a registration still pending", "pending", RegistrationPendingError, "share"],
  ["a trade the broker never answers", "silent", TradeUnansweredError, "share"],
  ["a broker that answers 503", "503", TradeRetryingError, "deadline"],
] as const)(
  "a write whose credential meets %s ends with the credential's error inside a deadline shorter than its pacing",
  async (_, mode, failure, endsAt) => {
    forgetKeyedLayerMemory();
    // The token client says what a trade came to at the wait after it, and the broker that never
    // answers has the trade: past either, only timers are left to happen.
    let read = false;
    const warn = spyOn(console, "warn").mockImplementation((line: unknown) => {
      if (String(line).includes("trading again")) read = true;
    });
    try {
      const broker = Bun.serve({
        port: 0,
        hostname: "127.0.0.1",
        fetch: async () => {
          if (mode === "503") return new Response("away", { status: 503 });
          if (mode === "silent") {
            read = true;
            return await new Promise<Response>(() => {});
          }
          return Response.json(
            {
              error: "invalid_grant",
              error_description: `pod_session_not_registered: No registration says this pod may write under the session name '${POD_SESSION}'. A pod writes under a session name its deployment registered for it.`,
            },
            { status: 400 }
          );
        },
      });
      servers.push(broker);
      const dispatch = stubDispatch();
      const credential = brokerCredential(
        podEnvironment(`http://127.0.0.1:${broker.port}`, dispatch.url),
        POD_SESSION
      );
      const deadlineMs = 2_500;
      let settled = false;
      const error = new DispatchClient(dispatch.url, credential, fetch, { deadlineMs })
        .comment("DSP-1", { body: "Shipped.", actor: { kind: "session", id: "session-1" } })
        .then(
          () => undefined,
          (caught: unknown) => caught
        )
        .finally(() => {
          settled = true;
        });
      await turnsUntil(() => read, "the credential's first trade read", { boundMs: 10_000 });
      // The write ends where the credential's share ends, or where the client's deadline ends
      // the pause; a write that went on past its deadline is read as moving the clock further.
      const moved = await stepUntil(() => settled, deadlineMs + 5_000);
      expect(moved).toBe(endsAt === "share" ? deadlineMs - RESERVE_MS : deadlineMs);
      expect(await error).toBeInstanceOf(failure);
      expect(dispatch.asked).toHaveLength(0);
    } finally {
      warn.mockRestore();
    }
  },
  20_000
);
