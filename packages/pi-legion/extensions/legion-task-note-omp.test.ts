import { afterEach, expect, test } from "bun:test";
import { mkdir, readdir, readFile } from "node:fs/promises";
import * as path from "node:path";
import { taskWaitingNote } from "@legion/pi-shared/task-notes";
import type { TaskValue } from "@legion/pi-shared/task-take";
import {
  type Block,
  type Cleanup,
  call,
  deliveryFor,
  isSelfCheck,
  messageStream,
  ompNatsGuard,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import {
  publishTaskFrame,
  taskRelayConnection,
  taskStandIn,
} from "@legion/pi-shared/test/task-stand-in";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

// dispatch://INFRA-4130 R1 on the real Oh My Pi. The worker's run is busy when the daemon writes
// the claim's next task, so the plugin notes the task to the model with the next tool result
// (`additionalContext` of `tool_result`) and takes it only when the run ends. Only the real
// binary can say the three things this rests on: that the host hands that context to the model in
// its next request as a `developer` message; that the note queues nothing the host would start a
// turn for (a steer, an aside, a follow-up, each of which a run that did not read it turns into a
// turn of its own at its end, `#strandedRecordWakes`); and that the reminder's continuation does
// not come between the run's end and the task's turn. LEGION_TEST_OMP names the binary and
// LEGION_TEST_NATS_URL the NATS server the pane runs against, which the task value is relayed on
// (`ompNatsGuard`, as `legion-phase-stall-omp.test.ts` runs them); without both the case skips,
// except on GitHub Actions, where it fails.
//
// The pane is built from the harness's own parts (`runLegionPane` gives a case neither the RPC
// frames nor a value relayed mid-run): the stand-in model, daemon and listener on one server,
// and the take's stand-in record (`taskStandIn`).
const { skip, setup } = ompNatsGuard();

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

const ISSUE = "NOTE-2";
const ROLE = "implementer";
const CLAIM_TOKEN = `legion-note-${ISSUE.toLowerCase()}-${ROLE}`;

/** The route the first tool call reaches: the stand-in relays the claim's next value while that
 * call runs, and answers it once the session has reported the value seen, so the take holds the
 * value before the call's result comes back. */
const FIRST_CALL_ROUTE = "/test/first-call";

/** The daemon's state as the phase reminder's record check reads it: NOTE-2 at implementing, the
 * implementer's phase, so a run that settles with its phase open earns the reminder's
 * continuation wherever no take is pending. */
const NOTE_STATE = {
  daemon: {
    project: "NOTE",
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-09-18T14:03:27Z",
    startedAt: "2026-09-22T09:15:02Z",
  },
  admission: { cap: 1, active: ["NOTE-1"], waiting: [], trees: ["NOTE-1"], blocked: [] },
  issues: {
    [ISSUE]: {
      key: ISSUE,
      generation: 1,
      phase: "implementing",
      status: "in_progress",
      repo: "acme/widgets",
      defaultBranch: "main",
      workers: {},
    },
  },
  pendingStatusWrites: [],
  heldOutboxRows: [],
  strandedClaims: [],
  wakesWaiting: [],
  capabilities: [],
  repositories: {
    owners: ["acme"],
    mapped: [{ repo: "acme/widgets", default: true }],
    refused: [],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { review_round_cap: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
};

/** An RPC frame that says a run started or ended, or what the host's queues hold. */
type RunFrame =
  | { readonly type: "agent_start" }
  | { readonly type: "agent_end"; readonly isTerminal: boolean }
  | { readonly type: "queue_update"; readonly steering: unknown; readonly followUp: unknown };

/** The messages of a Messages request whose text holds `text`, each as the request carries it. */
function messagesHolding(request: Request, text: string): unknown[] {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages.filter((message) => JSON.stringify(message).includes(text));
}

/** The text of the last user message of a Messages request. */
function lastUserText(request: Request): string {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  const last = messages.findLast(
    (message) =>
      typeof message === "object" &&
      message !== null &&
      "role" in message &&
      message.role === "user"
  );
  return JSON.stringify(last ?? null);
}

/** Every entry of the pane's persisted transcripts under `sessions` whose JSON holds `text`, in
 * order. */
async function transcriptEntriesHolding(sessions: string, text: string): Promise<unknown[]> {
  const files = (await readdir(sessions, { recursive: true }))
    .filter((file) => file.endsWith(".jsonl"))
    .sort();
  const entries: unknown[] = [];
  for (const file of files) {
    for (const line of (await readFile(path.join(sessions, file), "utf8")).split("\n")) {
      if (line.includes(text)) entries.push(JSON.parse(line));
    }
  }
  return entries;
}

test.skipIf(skip)(
  "a real Oh My Pi run reads the note at its next step boundary, and no run starts between the note and the task's turn",
  async () => {
    const { binary, nats } = setup();
    const firstTask = "Implement NOTE-2.";
    const secondTask = "Implement NOTE-2's review fixes.";
    // The daemon's record of the claim: the first task is the registration's value, which the
    // pane takes once its ready has answered; the second is written while that task's run goes on.
    const tasks = taskStandIn();
    tasks.give(firstTask);
    const registered: Record<string, unknown> = JSON.parse(
      await readFile(
        path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
        "utf8"
      )
    );
    const { root, home, workspace, sessions } = await ompRoot(binary, "legion-note-", cleanup);
    const state = path.join(root, "state");
    await mkdir(state, { recursive: true, mode: 0o700 });
    const relay = taskRelayConnection(nats, cleanup);

    let paneSession = "";
    let second: TaskValue | undefined;
    const secondSeen = Promise.withResolvers<void>();
    // The model's run of the first task: two tool calls, then the run's last word; then the turn
    // of the second task, which waits on CI so that its own settle earns no reminder.
    const replies: readonly ((base: string) => readonly Block[])[] = [
      (base) => [call("bash", { command: `curl -fsS ${base}${FIRST_CALL_ROUTE}` })],
      () => [call("bash", { command: "echo second-call" })],
      () => [{ type: "text", text: "The first task's change is in." }],
      () => [{ type: "text", text: "WAITING: CI on the review fixes." }],
    ];
    const turns: Request[] = [];
    // When the stand-in last saw the model or the host's runs move, for the quiet wait below.
    let lastActivityAt = Date.now();
    const { requests, base } = serveStandin(cleanup, async (url, body) => {
      if (url.pathname === "/anthropic/v1/messages") {
        const request: Request = { path: url.pathname, body };
        // pi-envoy's run-end self-check is a side turn, not a turn of the conversation.
        if (isSelfCheck(request)) {
          return new Response(messageStream([{ type: "text", text: "PROCEEDING" }], "btw"), {
            headers: { "content-type": "text/event-stream" },
          });
        }
        turns.push(request);
        lastActivityAt = Date.now();
        const reply = replies[turns.length - 1];
        if (reply === undefined) {
          return Response.json(
            {
              type: "error",
              error: { type: "invalid_request_error", message: "no reply scripted" },
            },
            { status: 400 }
          );
        }
        return new Response(messageStream(reply(base), `msg_${turns.length}`), {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
      if (url.pathname === FIRST_CALL_ROUTE) {
        second = tasks.give(secondTask);
        publishTaskFrame(await relay(), paneSession, CLAIM_TOKEN, second);
        // A real clock, deliberately: the call is a real process's, and a session that never
        // reports the value seen would hold it for ever. The bound answers it anyway, and the
        // assertions below then say what did not happen.
        await Promise.race([secondSeen.promise, Bun.sleep(20_000)]);
        return new Response("the claim's next task value was relayed\n");
      }
      if (
        url.pathname === "/legion/v1/claims/task-seen" &&
        second !== undefined &&
        body.number === second.number
      ) {
        secondSeen.resolve();
      }
      const taskAnswer = tasks.answer(url.pathname, body);
      if (taskAnswer !== undefined) return taskAnswer;
      switch (url.pathname) {
        case "/legion/v1/claims/register":
          paneSession = typeof body.sessionId === "string" ? body.sessionId : "";
          return Response.json({
            ...registered,
            claimToken: CLAIM_TOKEN,
            tree: "NOTE-1",
            issue: ISSUE,
            role: ROLE,
            generation: 1,
            secret: fixtureSecret("claim"),
            ...tasks.registered(),
          });
        case "/legion/v1/claims/ready":
        case "/legion/v1/claims/activity":
          return new Response(null, { status: 204 });
        case "/legion/v1/claims/turn-outcome":
          return Response.json({ refusedRun: 0, failed: false });
        case "/legion/v1/state":
          return Response.json(NOTE_STATE);
      }
      if (url.pathname.startsWith("/legion/")) {
        return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
      }
      // The Envoy listener: the session's registration, which hands it its consumer, and its role.
      const sessionID = typeof body.session_id === "string" ? body.session_id : "";
      return Response.json({
        session_id: sessionID,
        machine_id: "note-machine",
        dir: workspace,
        topics: [],
        ...deliveryFor(url.pathname, body, sessionID),
      });
    });
    await writeStandinProfile(home, base);

    const frames: RunFrame[] = [];
    const rpc = spawnRpc(
      binary,
      {
        extensions: [
          path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
          path.resolve(import.meta.dir, "./legion.ts"),
        ],
        home,
        workspace,
        sessions,
        env: {
          ENVOY_URL: base,
          ENVOY_NATS_URL: nats,
          LEGION_DAEMON_URL: base,
          LEGION_PROJECT: "note",
          LEGION_ROLE: ROLE,
          LEGION_TREE: "NOTE-1",
          LEGION_ISSUE: ISSUE,
          LEGION_GENERATION: "1",
          LEGION_BOOT_TOKEN: fixtureSecret("boot"),
          LEGION_STATE_DIR: state,
          LEGION_WORKSPACE: workspace,
        },
        onFrame: (frame) => {
          if (!("type" in frame)) return;
          if (frame.type === "agent_start") {
            frames.push({ type: "agent_start" });
          } else if (frame.type === "agent_end") {
            frames.push({
              type: "agent_end",
              isTerminal: !("isTerminal" in frame && frame.isTerminal === false),
            });
          } else if (frame.type === "queue_update") {
            frames.push({
              type: "queue_update",
              steering: "steering" in frame ? frame.steering : undefined,
              followUp: "followUp" in frame ? frame.followUp : undefined,
            });
          } else {
            return;
          }
          lastActivityAt = Date.now();
        },
      },
      cleanup
    );

    // Both runs end, and then nothing more happens for a while: the absence of a third run is
    // the proposition, and only elapsed time shows an absence.
    const quietMs = 4_000;
    const deadline = Date.now() + 90_000;
    await Promise.race([
      (async () => {
        for (;;) {
          const ends = frames.filter((frame) => frame.type === "agent_end" && frame.isTerminal);
          if (ends.length >= 2 && Date.now() - lastActivityAt >= quietMs) return;
          if (Date.now() > deadline) {
            throw new Error(`the pane never settled: ${JSON.stringify(frames)}`);
          }
          await Bun.sleep(200);
        }
      })(),
      rpc.closed,
    ]);
    await rpc.end();

    const note = second === undefined ? "" : taskWaitingNote(second.number);
    expect(second?.number).toBe(2);
    // Four requests: the first task's two tool calls and its last word, then the second task.
    expect(turns).toHaveLength(4);
    expect(lastUserText(turns[0] as Request)).toContain(firstTask);
    expect(messagesHolding(turns[0] as Request, note)).toEqual([]);
    // The model's second request, the first after the tool result the note went with, holds the
    // note as a message of its own, which the Messages API carries as a user message; the
    // transcript says what the host made it: a `developer` message. The note carries none of the
    // task's text.
    expect(messagesHolding(turns[1] as Request, note)).toMatchObject([
      { role: "user", content: [{ type: "text", text: note }] },
    ]);
    expect(await transcriptEntriesHolding(sessions, note)).toMatchObject([
      { type: "message", message: { role: "developer", content: [{ type: "text", text: note }] } },
    ]);
    expect(messagesHolding(turns[1] as Request, secondTask)).toEqual([]);

    const runs = frames.filter((frame) => frame.type !== "queue_update");
    // Two runs, each ended terminally: the host had nothing queued to go on with — no steer,
    // aside or follow-up — and the first run to start after the first run's end is the second.
    expect(runs).toEqual([
      { type: "agent_start" },
      { type: "agent_end", isTerminal: true },
      { type: "agent_start" },
      { type: "agent_end", isTerminal: true },
    ]);
    const firstRunEnd = frames.findIndex((frame) => frame.type === "agent_end");
    expect(
      frames
        .slice(0, firstRunEnd)
        .filter(
          (frame) =>
            frame.type === "queue_update" &&
            JSON.stringify([frame.steering, frame.followUp]) !== "[[],[]]"
        )
    ).toEqual([]);
    // That second run is the take's: its request carries the second task's text as the turn's
    // user message, and the turns' reports name the first task, then the second.
    expect(lastUserText(turns[3] as Request)).toContain(secondTask);
    expect(tasks.takes().map((take) => take.number)).toEqual([1, 2]);
    expect(
      requests
        .filter((request) => request.path === "/legion/v1/claims/turn-outcome")
        .map((request) => request.body.task)
    ).toEqual([1, 2]);
    // The take of the second task waited for the first run's end: it was asked after that run's
    // last request to the model, and before the second run's first.
    const turnAt = requests.flatMap((request, index) =>
      request.path === "/anthropic/v1/messages" && !isSelfCheck(request) ? [index] : []
    );
    const secondTake = requests.findLastIndex(
      (request) => request.path === "/legion/v1/claims/take"
    );
    expect(secondTake).toBeGreaterThan(turnAt[2] ?? Number.POSITIVE_INFINITY);
    expect(secondTake).toBeLessThan(turnAt[3] ?? Number.NEGATIVE_INFINITY);
  },
  120_000
);
