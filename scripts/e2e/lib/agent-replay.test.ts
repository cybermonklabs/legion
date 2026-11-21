import { afterEach, expect, test } from "bun:test";
import { join } from "node:path";
import { recordTimers } from "@legion/test-fixtures/timer-record";
import { settled } from "@legion/test-fixtures/wait";
import type { ServerWebSocket } from "bun";

import {
  type FakeDispatchServer,
  startFakeDispatch,
} from "../../../packages/envoy-client/test/fake-dispatch";

import { readAgentReplay } from "./agent-replay";

// readAgentReplay runs in the page in dispatch-user-turns.sh's `count`, where Playwright's
// `page.evaluate` sends the page its source alone (`toString()`) and the page evaluates that. These
// rows run it the same way: its source evaluated in Bun's global scope, where nothing of this
// module or agent-replay.ts is reachable, so a helper it names from outside the function fails a
// row here as it would fail in the page. They run it against a stand-in for Dispatch's live socket
// that answers the agent view's subscribe as the script tells it to. Its two waits are its own
// real timers, read against real socket traffic. A row about a read that ends on what Dispatch
// sent gives it a timeout no row reaches (`NO_TIMEOUT_MS`), so a read that waited for its timeout
// rather than ending on that frame never ends there, and the row's wait for it fails by name; the
// row whose point is the timeout gives it 1 s and holds the read to the timer it armed for that
// second, from the read's own record of its timers (`@legion/test-fixtures/timer-record`), not to
// how long it took. `emptyAfterMs` is 300 ms throughout.

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

/** The longest delay a timer takes (Bun, as Node does, fires a longer one after 1 ms): a timeout
 *  no row reaches. */
const NO_TIMEOUT_MS = 2 ** 31 - 1;

/** How `read` ended, waited for under the row's deadline and named by `what`: its value, or its
 *  error's message. */
async function outcome(
  what: string,
  read: Promise<unknown>
): Promise<{ readonly value?: unknown; readonly error?: string }> {
  return await settled(
    what,
    read.then(
      (value) => ({ value }),
      (error: unknown) => ({ error: error instanceof Error ? error.message : String(error) })
    )
  );
}

/** The file whose function these rows run: the name its evaluated source carries. */
const AGENT_REPLAY_SOURCE = join(import.meta.dir, "agent-replay.ts");

/** readAgentReplay as `page.evaluate` runs it: its source, evaluated in the global scope, where
 *  nothing of this module or of agent-replay.ts is reachable. Evaluating that source is the point
 *  of these rows, and the source is this repository's own, not a value from anywhere else. Its
 *  `sourceURL` names agent-replay.ts and changes nothing the source does. Without it Bun names the
 *  evaluated code's frames by the file that called `eval`, this test, whose timers the timer
 *  record leaves out; with it the record keeps the timers the read arms as the program's. */
// biome-ignore lint/security/noGlobalEval: these rows exist to run the function's own source as the page does
const evaluateInGlobalScope = eval;
const inPage = evaluateInGlobalScope(
  `(${readAgentReplay.toString()})\n//# sourceURL=${AGENT_REPLAY_SOURCE}`
) as typeof readAgentReplay;

const read = (url: string, timeoutMs = NO_TIMEOUT_MS) =>
  inPage({ emptyAfterMs: 300, sessionId: SESSION, timeoutMs, url });

test("a responding session's replay is the answer", async () => {
  const dispatch = dispatchAnswering((socket) => {
    socket.send(responder(true));
    socket.send(replayFrame);
  });
  expect(await settled("the replay Dispatch relays", read(dispatch.wsUrl))).toEqual(replay);
  expect(dispatch.received).toEqual([
    { sub: SUB, type: "subscribe", view: { kind: "agent", session_id: SESSION } },
  ]);
});

test("a responding session with no replay ends at once, saying its replay is empty", async () => {
  const dispatch = dispatchAnswering((socket) => socket.send(responder(true)));
  const result = await outcome(
    "the read to end on its empty-replay wait after the responding verdict",
    read(dispatch.wsUrl)
  );
  expect(result.error).toBe(
    "the session's replay is empty: Dispatch said the session is responding and sent no replay"
  );
});

test("a refused agent view ends at once, naming Dispatch's refusal", async () => {
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
  const result = await outcome("the read to end on Dispatch's refusal", read(dispatch.wsUrl));
  expect(result.error).toBe(
    "Dispatch refused the agent view: OAUTH_SCOPE_REQUIRED: this socket's credential does not hold the person scope, which watching an agent's conversation requires"
  );
});

test("a view Dispatch ends before its replay says so", async () => {
  const dispatch = dispatchAnswering((socket) =>
    socket.send(JSON.stringify({ reason: "dropped", sub: SUB, type: "ended" }))
  );
  const result = await outcome(
    "the read to end on Dispatch's end of the view",
    read(dispatch.wsUrl)
  );
  expect(result.error).toBe("Dispatch ended the agent view before its replay (dropped)");
});

test("a session that answers a re-arm after not responding still gives its replay", async () => {
  const dispatch = dispatchAnswering((socket) => {
    socket.send(responder(false));
    // Dispatch re-arms the session and asks again; this one answers the second time.
    socket.send(responder(true));
    socket.send(replayFrame);
  });
  expect(await settled("the replay after the re-arm", read(dispatch.wsUrl))).toEqual(replay);
});

test("a session that never answers ends at the timeout, saying it is not responding", async () => {
  const dispatch = dispatchAnswering((socket) => socket.send(responder(false)));
  const steps: unknown[] = [];
  const stopRecording = recordTimers([import.meta.dir], (step) => steps.push(step));
  try {
    const result = await outcome("the read to end at its 1 s timeout", read(dispatch.wsUrl, 1_000));
    expect(result.error).toBe(
      "the session did not answer Dispatch's replay request within 1s (responding: false), so the agent view has no replay"
    );
  } finally {
    stopRecording();
  }
  // The read's own record of its timers, to its end: it armed one, its timeout, for the 1 s it was
  // given, and that timer ran, its error the read's end. So the read ended at its timeout: one
  // armed later, or a wait on another timer of its own before the end, shows its delay here.
  expect(steps).toEqual([
    { delayMs: 1_000, from: expect.stringContaining(`${AGENT_REPLAY_SOURCE}:`), timer: 1 },
    { fired: 1 },
  ]);
});

test("a socket closed before the replay says so", async () => {
  const dispatch = dispatchAnswering((socket) => socket.close(1011, "internal error"));
  const result = await outcome("the read to end on the socket's close", read(dispatch.wsUrl));
  expect(result.error).toBe(
    "the live socket closed before the session's replay (1011 internal error)"
  );
});
