import { afterEach, expect, jest, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { ServerWebSocket } from "bun";

import {
  type FakeDispatchServer,
  startFakeDispatch,
} from "../../../packages/envoy-client/test/fake-dispatch";

import { readAgentReplay } from "./agent-replay";
import { libRowBoundMs, rowWaits } from "./row-waits";

// readAgentReplay runs in the page in dispatch-user-turns.sh's `count`, where Playwright's
// `page.evaluate` sends the page its source alone (`toString()`) and the page evaluates that. These
// rows run it the same way: its source evaluated in Bun's global scope, where nothing of this
// module or agent-replay.ts is reachable, so a helper it names from outside the function fails a
// row here as it would fail in the page. They run it against a stand-in for Dispatch's live socket
// that answers the agent view's subscribe as the script tells it to. Its two waits are its own
// timers, read against real socket traffic: each row gives both the end of the row's waits
// (lib/row-waits.ts), so a row ends on the frame it waits for however late a loaded host delivers
// it, and gives a short one only to the timer that is its subject: `emptyAfterMs` 300 ms where the
// empty replay is the point, and a 1 s timeout on jest's fake clock where the timeout is, moved only
// once the reader has taken the verdict the row names (endedByTimeout). Each timer ends the read
// with a message of its own, so the message says which ended it.
const waits = rowWaits(libRowBoundMs);

const SESSION = "01a0e0c1-0000-7000-8000-00000000beef";
const SUB = "dispatch-user-turns";
const servers: FakeDispatchServer<unknown>[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop();
});

const responder = (responding: boolean) =>
  JSON.stringify({ data: { responding, v: 1 }, event: "responder", sub: SUB, type: "agent" });
const replay = { frames: [{ kind: "message", seq: 1, v: 1 }], session_id: SESSION, v: 1 };
const replayFrame = JSON.stringify({ data: replay, event: "replay", sub: SUB, type: "agent" });

/** A Dispatch whose live socket answers each subscribe with `answer`, recording every frame. */
function dispatchAnswering(
  answer: (socket: ServerWebSocket<undefined>) => void
): FakeDispatchServer<unknown> {
  const dispatch = startFakeDispatch({ live: answer });
  servers.push(dispatch);
  return dispatch;
}

/** What `promise` settles to: its value, or its error's message. */
async function settled(
  promise: Promise<unknown>
): Promise<{ readonly value?: unknown; readonly error?: string }> {
  try {
    return { value: await promise };
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
}

/** readAgentReplay as `page.evaluate` runs it: its source, evaluated in the global scope, where
 *  nothing of this module or of agent-replay.ts is reachable. Evaluating that source is the point
 *  of these rows, and the source is this repository's own, not a value from anywhere else. */
// biome-ignore lint/security/noGlobalEval: these rows exist to run the function's own source as the page does
const evaluateInGlobalScope = eval;
const inPage = evaluateInGlobalScope(`(${readAgentReplay.toString()})`) as typeof readAgentReplay;

// read gives each of the read's timers the end of the row's waits, but for the one a row names.
const read = (url: string, timers: { emptyAfterMs?: number; timeoutMs?: number } = {}) => {
  const left = Math.max(1, waits.end() - performance.now());
  return inPage({
    emptyAfterMs: timers.emptyAfterMs ?? left,
    sessionId: SESSION,
    timeoutMs: timers.timeoutMs ?? left,
    url,
  });
};

test("a responding session's replay is the answer", async () => {
  const dispatch = dispatchAnswering((socket) => {
    socket.send(responder(true));
    socket.send(replayFrame);
  });
  expect(await read(dispatch.wsUrl)).toEqual(replay);
  expect(dispatch.received).toEqual([
    { sub: SUB, type: "subscribe", view: { kind: "agent", session_id: SESSION } },
  ]);
});

test("a responding session with no replay ends on its empty-replay timer, saying its replay is empty", async () => {
  const dispatch = dispatchAnswering((socket) => socket.send(responder(true)));
  const result = await settled(read(dispatch.wsUrl, { emptyAfterMs: 300 }));
  expect(result.error).toBe(
    "the session's replay is empty: Dispatch said the session is responding and sent no replay"
  );
});

test("a refused agent view ends on the refusal, naming it", async () => {
  const dispatch = dispatchAnswering((socket) =>
    socket.send(
      JSON.stringify({
        code: "OAUTH_SCOPE_REQUIRED",
        message:
          "this socket's credential does not hold the person scope, which watching an agent's conversation requires",
        sub: SUB,
        type: "error",
      })
    )
  );
  const result = await settled(read(dispatch.wsUrl));
  expect(result.error).toBe(
    "Dispatch refused the agent view: OAUTH_SCOPE_REQUIRED: this socket's credential does not hold the person scope, which watching an agent's conversation requires"
  );
});

test("a view Dispatch ends before its replay says so", async () => {
  const dispatch = dispatchAnswering((socket) =>
    socket.send(JSON.stringify({ reason: "dropped", sub: SUB, type: "ended" }))
  );
  const result = await settled(read(dispatch.wsUrl));
  expect(result.error).toBe("Dispatch ended the agent view before its replay (dropped)");
});

test("a session that answers a re-arm after not responding still gives its replay", async () => {
  const dispatch = dispatchAnswering((socket) => {
    socket.send(responder(false));
    // Dispatch re-arms the session and asks again; this one answers the second time.
    socket.send(responder(true));
    socket.send(replayFrame);
  });
  expect(await read(dispatch.wsUrl)).toEqual(replay);
});

// uptimeSeconds is the kernel's own count of seconds since boot (/proc/uptime), the one clock
// jest's fake timers do not freeze: under them Bun.nanoseconds(), performance.now(), Date.now() and
// process.hrtime all stand still.
const uptimeSeconds = () => Number(readFileSync("/proc/uptime", "utf8").split(" ")[0]);
// nap blocks the thread for a real 10 ms, which the fake clock does not reach, so the wait below
// looks about a hundred times a second rather than spinning.
const napCell = new Int32Array(new SharedArrayBuffer(4));
const nap = () => Atomics.wait(napCell, 0, 0, 10);

// endedByTimeout reads with a 1 s timeout on jest's fake clock, so no wall clock races the
// stand-in's frames to the reader. `verdict` waits until the reader has taken Dispatch's last
// verdict, through `armed`: the reader arms its empty-replay timer on a responding verdict and
// clears it on the next one, so whether that timer is pending, over the timers pending once the
// read began, says which verdict it took last. Only then does the row move the clock past the
// timeout. The empty-replay timer is given the end of the row's waits, past the 1 s the clock moves,
// and each wait in `armed` gives up at that end, read on the kernel's clock, naming the verdict.
async function endedByTimeout(
  url: string,
  verdict: (armed: (pending: boolean, what: string) => Promise<void>) => Promise<void>
) {
  const left = Math.max(0, waits.end() - performance.now());
  const end = uptimeSeconds() + left / 1000;
  const emptyAfterMs = Math.max(2_000, left);
  jest.useFakeTimers();
  try {
    const reading = settled(inPage({ emptyAfterMs, sessionId: SESSION, timeoutMs: 1_000, url }));
    const base = jest.getTimerCount();
    const armed = async (pending: boolean, what: string) => {
      const want = base + (pending ? 1 : 0);
      while (jest.getTimerCount() !== want) {
        if (uptimeSeconds() >= end) {
          throw new Error(
            `by the end of the row's waits the reader had not taken ${what}: ${jest.getTimerCount()} timers pending, not ${want}`
          );
        }
        nap();
        const tick = Promise.withResolvers<void>();
        setImmediate(tick.resolve);
        await tick.promise;
      }
    };
    await verdict(armed);
    jest.advanceTimersByTime(1_000);
    return await reading;
  } finally {
    jest.useRealTimers();
  }
}

test("a session Dispatch last called not responding ends at the timeout, saying it is not responding", async () => {
  // Dispatch calls the session responding, then, on the row's word, not responding: the reader
  // arms its empty-replay timer on the first verdict and clears it on the second, so the row sees
  // it take each one.
  let session: ServerWebSocket<undefined> | undefined;
  const dispatch = dispatchAnswering((socket) => {
    session = socket;
    socket.send(responder(true));
  });
  const result = await endedByTimeout(dispatch.wsUrl, async (armed) => {
    await armed(true, "Dispatch's verdict that the session is responding");
    session?.send(responder(false));
    await armed(false, "Dispatch's verdict that the session is not responding");
  });
  expect(result.error).toBe(
    "the session did not answer Dispatch's replay request within 1s (responding: false), so the agent view has no replay"
  );
});

// The timeout names Dispatch's last verdict as it was: a session Dispatch called responding, whose
// replay never came before the timeout, is not one that did not answer.
test("a responding session whose replay does not come before the timeout says the session is responding", async () => {
  const dispatch = dispatchAnswering((socket) => socket.send(responder(true)));
  const result = await endedByTimeout(dispatch.wsUrl, (armed) =>
    armed(true, "Dispatch's verdict that the session is responding")
  );
  expect(result.error).toBe(
    "Dispatch said the session is responding and sent no replay within 1s (responding: true), so the agent view has no replay"
  );
});

test("a socket closed before the replay says so", async () => {
  const dispatch = dispatchAnswering((socket) => socket.close(1011, "internal error"));
  const result = await settled(read(dispatch.wsUrl));
  expect(result.error).toBe(
    "the live socket closed before the session's replay (1011 internal error)"
  );
});
