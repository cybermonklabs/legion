import { afterEach, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { symlink, writeFile } from "node:fs/promises";
import * as path from "node:path";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import { PHASE_STALL_ENTRY } from "@legion/pi-shared/phase-stall";
import {
  type Cleanup,
  call,
  type LegionPane,
  type LegionPaneOptions,
  ompNatsGuard,
  type Reply,
  type Request,
  type Rpc,
  runLegionPane,
  sendRelayFrame,
} from "@legion/pi-shared/test/omp-harness";
<<<<<<< 7fa22de66d
import { UPDATE_ASK_KEY, waitStandIn } from "@legion/pi-shared/test/wait-stand-in";
import { connect, headers, StringCodec } from "nats";
=======
import { namesMissing } from "@legion/test-fixtures/names-whole";
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253

// The phase-stall follow-up on the real Oh My Pi (src/phase-stall.ts): only the real binary
// shows when the host fires `session_stop`, how it turns the returned follow-up into the next turn,
// what the model is sent, and that the transcript keeps the state a resumed worker restores.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which
// CI's pi-envoy job installs; on the devbox, `mise where <pin>`/bin/omp. LEGION_TEST_NATS_URL
// names the NATS server each pane runs against: a session's only inbound is the consumer the
// listener builds for it, relayed on its own relay subject, so a pane with no NATS server
// registers nothing and claims nothing (INFRA-771). A run without both skips, except on GitHub
// Actions, where a skip would hide the only run of the check on the host that ships it
// (`ompNatsGuard`).
// It is also the check that a wait the worker declares holds the reminder in a real host
// (dispatch://INFRA-2040): the worker calls the `legion` tool's `declare_wait` as a real tool
// call, a real Envoy delivery reaches the session over that NATS server, and the stand-in daemon
// receives the plugin's report of news with the key the card carries, which only the real card
// path (pi-envoy's `handCard`) can show.
// It is also the only check that the run-end nudge's hidden self-check starts no run: the nudge
// treats any `agent_start` after a settle as a newer run and withholds its steer, so a host that
// counted the side turn as a run would silence the nudge with every unit test still green.
// The WAITING self-check case below fails if that ever changes.
const { skip, setup } = ompNatsGuard();

/** The implementer pane the phase-stall cases run, with what its stand-in daemon recorded and
 * its transcript's phase-stall entries. */
interface StallPane extends LegionPane {
  /** The body of each `POST /legion/v1/handoff/complete` the pane made, in order. */
  readonly completions: () => unknown[];
  /** The body of each `POST /v1/messages/publish` the pane made, in order: the architect notice
   * a run of unanswered reminders earns, and every retry of one the listener refused. */
  readonly publishes: () => Record<string, unknown>[];
  /** The persisted transcript's phase-stall entries, in order. */
  readonly phaseEntries: () => Promise<unknown[]>;
  /** The commit `legion/STALL-2@origin` names in the pane's workspace: the pushed tip its
   * `handoff_complete` reports beside the record. */
  readonly pushedTip: string;
  /** The body of each `POST /legion/v1/claims/wait` the pane made, in order: each wait the
   * worker declared. */
  readonly declarations: () => Record<string, unknown>[];
  /** The body of each `POST /legion/v1/claims/wait-news` the pane made, in order: each report of
   * news after a declared wait. */
  readonly newsReports: () => Record<string, unknown>[];
}

/** `POST /legion/v1/claims/activity`, the session's own report of what it is doing
 * (packages/pi-shared/src/session-activity.ts): the plugin sends it after each turn and whenever
 * the report's content changes, so how many a case sees is the host's timing and no case here is
 * about it. What those reports hold is asserted in extensions/legion-session-activity-omp.test.ts. */
const ACTIVITY_ROUTE = "/legion/v1/claims/activity";

/** Every daemon route the pane called, in order, the activity heartbeat left out. */
function daemonRoutes(pane: LegionPane): string[] {
  return pane.requests
    .map((request) => request.path)
    .filter((requested) => requested.startsWith("/legion/") && requested !== ACTIVITY_ROUTE);
}

/**
 * The reminder count each phase-stall entry recorded, with a repeat of the value before it
 * dropped: what is left is the count's own climb, one step per reminder the host put in front of
 * the worker. The repeats are the writes that carry no count of their own — the architect notice
 * minting its key and then marking itself sent — and the closing entry, which follows a
 * `message_end` that already put the count back to zero, in whichever order the host delivers
 * the two.
 */
function stallCounts(entries: readonly unknown[]): number[] {
  const counts = entries.map((entry) =>
    typeof entry === "object" && entry !== null && "count" in entry
      ? Number(entry.count)
      : Number.NaN
  );
  return counts.filter((count, index) => index === 0 || count !== counts[index - 1]);
}

/** The listener's answer to a publish it accepted: an envelope, which the client parses before
 * it answers its caller (`EnvelopeSchema`, packages/contracts/src/envelope.ts). */
function published(body: Record<string, unknown>): Response {
  const key = String(body.idempotency_key ?? "");
  return Response.json({
    event_id: `evt-${key}`,
    source: "agent",
    source_event_id: key,
    topic: String(body.topic ?? ""),
    dedupe_key: `publish.${key}`,
    issued_at: 1,
    payload_summary: "a worker has let its phase reminders go unanswered",
    trace_id: `trace-${key}`,
  });
}

/** The identity the daemon puts on every pane (runtime.GitIdentity), which the pane's jj commits
 * as. */
const PANE_IDENTITY = { JJ_USER: "Legion Test", JJ_EMAIL: "legion-test@example.invalid" };

/** The implement handoff the worker wrote to the stand-in Dispatch with `dispatch handoff-write`
 * before completing: the newest record of STALL-2's implement phase, which its `handoff_complete`
 * reads and names. Its body names the head the round pushed, which the workspace this file builds
 * decides, so the body is written once that push has happened. */
const STALL_RECORD = {
  id: "0c9e7d2a-41b6-4f58-9a3e-6d1b2c8f5e70",
  issue_key: "STALL-2",
  phase: "implement",
  version: 1,
  author: { kind: "agent", session_id: "ses_stall" },
  created_at: "2026-10-09T08:00:00Z",
};

/** Runs `jj` in `cwd` as the pane's identity, failing loudly on a non-zero exit, and answers its
 * stdout. */
async function runJj(jj: string, cwd: string, args: readonly string[]): Promise<string> {
  const child = Bun.spawn([jj, ...args], {
    cwd,
    env: { ...process.env, ...PANE_IDENTITY },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

/** What the stand-in daemon calls the work it answers the implementer owes
 * (`POST /legion/v1/claims/owed`), which the reminder and the architect notice name. */
const STALL_WORK = "the implementing phase of STALL-2";

/** The daemon's state as the pane's `handoff_complete` reads it: STALL-2 at implementing, the
 * phase whose handoff is the `implement` record. */
const STALL_STATE = {
  daemon: {
    project: "STALL",
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-09-18T14:03:27Z",
    startedAt: "2026-09-22T09:15:02Z",
  },
  admission: { cap: 1, active: ["STALL-1"], waiting: [], trees: ["STALL-1"], blocked: [] },
  issues: {
    "STALL-2": {
      key: "STALL-2",
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
    live: [{ repo: "acme/widgets", apps: { implement: true, review: true } }],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { review_round_cap: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
};

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The text of every user message in a Messages request, joined. */
function userText(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(
    messages.filter(
      (message: unknown) =>
        typeof message === "object" &&
        message !== null &&
        "role" in message &&
        message.role === "user"
    )
  );
}

/** The names of the tools a Messages request offers the model. */
function toolNames(request: Request): string[] {
  const tools = Array.isArray(request.body.tools) ? request.body.tools : [];
  return tools.map((tool: unknown) =>
    typeof tool === "object" && tool !== null && "name" in tool ? String(tool.name) : ""
  );
}

/** The description of the `legion` tool a Messages request offers the model. */
function legionToolDescription(request: Request): string {
  const tools = Array.isArray(request.body.tools) ? request.body.tools : [];
  const tool = tools.find(
    (candidate: unknown) =>
      typeof candidate === "object" &&
      candidate !== null &&
      "name" in candidate &&
      candidate.name === "legion"
  );
  return String(tool?.description ?? "");
}

/** How one pane differs from the implementer pane the phase-stall cases run: the runner's
 * `legion`, `quietMs`, `selfCheck`, environment, frame callback and idle step, what the stand-in
 * answers ahead of this file's own routes, and the stand-in's open-ask snapshot. */
interface PaneOptions
  extends Pick<
    LegionPaneOptions,
    "legion" | "quietMs" | "selfCheck" | "env" | "answer" | "onFrame" | "whileUp"
  > {
  /** The count the stand-in Dispatch's open-ask snapshot answers with. */
  readonly openAsks?: number;
  /** The held ask questions the stand-in returns in the session's open-ask snapshot. */
  readonly openAskQuestions?: readonly string[];
}

/**
 * Runs one implementer pane on the real Oh My Pi (`runLegionPane`), the stand-in answering the
 * daemon's state, owed, completion and wait routes and, as Dispatch, the issue's handoff records
 * and the open-ask snapshot, until its run settles. The stand-in's record holds `STALL_WORK` owed
 * until a completion lands and nothing after it, as the daemon's own record does once the workflow
 * takes a completion (`owedBy` in `packages/daemon/internal/api/owed.go`); a case that answers
 * `claims/owed` itself decides otherwise. The record also holds the wait the worker declared and
 * answers the two wait routes as the shared stand-in does (`waitStandIn`): each declaration
 * mints `wait-<n>` in place of the one standing, the owed answer gives it while it stands, and a
 * report of news is answered as the daemon answers it. The pane's workspace is a jj repository
 * whose change is committed and pushed to a bare origin's `legion/STALL-2`, and the stand-in
 * Dispatch holds the implement record written to it, as a worker leaves both before completing,
 * so the pane's `handoff_complete` finds the record and the pushed tip.
 */
async function stallPane(
  run: { readonly binary: string; readonly nats: string },
  replies: readonly Reply[],
  options: PaneOptions = {}
): Promise<StallPane> {
  let pushedTip = "";
  /** The stand-in Dispatch's handoff records of STALL-2's implement phase, oldest first. */
  const records: Array<typeof STALL_RECORD & { body: string }> = [];
  /** Whether the stand-in's record still holds the implementer's work owed. */
  let owes = true;
  /** The wait the stand-in's record holds, and its answers to the two wait routes. */
  const waits = waitStandIn(STALL_WORK);
  const pane = await runLegionPane(
    run.binary,
    replies,
    {
      name: "stall",
      nats: run.nats,
      role: "implementer",
      tree: "STALL-1",
      issue: "STALL-2",
      prompt: "Implement STALL-2.",
      dispatch: true,
      legion: options.legion,
      quietMs: options.quietMs,
      selfCheck: options.selfCheck,
      onFrame: options.onFrame,
      whileUp: options.whileUp,
      env: {
        ...PANE_IDENTITY,
        // The daemon puts the project token on every pane, and the architect notice's role topic
        // is built from it (`roleToken`).
        ...(options.legion === false ? {} : { LEGION_PROJECT: "stall" }),
        ...options.env,
      },
      prepare: async ({ workspace, bin }) => {
        // The pane's PATH is the harness's fixed one plus `bin`: the jj this test process finds is
        // linked into `bin`, so the pane's `handoff_complete` runs the one that made its workspace.
        const jj = Bun.which("jj");
        if (jj === null) throw new Error("jj is not on PATH");
        await symlink(jj, path.join(bin, "jj"));
        const origin = path.join(path.dirname(workspace), "origin.git");
        if ((await Bun.spawn(["git", "init", "--bare", "--quiet", origin]).exited) !== 0) {
          throw new Error(`git init --bare ${origin} failed`);
        }
        await runJj(jj, path.dirname(workspace), ["git", "init", workspace]);
        // From here on, every command runs in the workspace, as the pane's own shell does.
        const jjIn = (...args: string[]) => runJj(jj, workspace, args);
        await jjIn("git", "remote", "add", "origin", origin);
        await writeFile(path.join(workspace, "main.py"), 'print("stall")\n');
        await jjIn("describe", "-m", "implement: the change");
        await jjIn("new");
        await jjIn("bookmark", "set", "legion/STALL-2", "-r", "@-");
        await jjIn("git", "push", "--bookmark", "legion/STALL-2");
        pushedTip = await jjIn("log", "-r", "@-", "--no-graph", "-T", "commit_id");
        // The worker's `dispatch handoff-write --issue STALL-2 --phase implement`, whose body
        // names the tip it just pushed.
        records.push({
          ...STALL_RECORD,
          body: JSON.stringify({
            schemaVersion: 1,
            phase: "implement",
            issue: "STALL-2",
            head: pushedTip,
          }),
        });
      },
      answer: (url, body) => {
        const own = options.answer?.(url, body);
        if (own !== undefined) return own;
        // `handoff_complete` reads one issue's record (dispatch://INFRA-2694), and the reminder
        // asks what the claim owes; nothing of this pane asks for the whole project body.
        if (url.pathname === "/legion/v1/issues/STALL-2/record") {
          return Response.json(STALL_STATE.issues["STALL-2"]);
        }
        if (url.pathname === "/legion/v1/claims/owed") {
          if (owes) return waits.owed();
          const wait = waits.state.wait;
          return Response.json({ owes: false, work: "", ...(wait === undefined ? {} : { wait }) });
        }
        if (url.pathname === "/legion/v1/handoff/complete") {
          owes = false;
          return Response.json({});
        }
        const waitAnswer = waits.answer(url.pathname, body);
        if (waitAnswer !== undefined) return waitAnswer;
        if (url.pathname === "/api/v1/issues/STALL-2/handoffs/implement/latest") {
          const newest = records.at(-1);
          return newest === undefined
            ? Response.json(
                { error: "no implement handoff on STALL-2", code: "HANDOFF_NOT_FOUND" },
                { status: 404 }
              )
            : Response.json(newest);
        }
        // The Envoy listener takes the architect notice unless a case refuses it.
        if (url.pathname === "/v1/messages/publish") return published(body);
        if (url.pathname !== "/api/v1/asks/open") return undefined;
        const questions = options.openAskQuestions ?? [];
        return Response.json({
          session_id: "",
          as_of: new Date().toISOString(),
          opened_since: false,
          count: options.openAsks ?? questions.length,
          waiting_on_human: 0,
          waiting_on_agent: 0,
          asks: questions.map((question, index) => ({
            id: `ask-${index}`,
            ref: `/issues/LEGION-${index}#ask-${index}`,
            question,
            kind: "question",
            urgency: "normal",
            created_at: "2026-09-13T00:00:00Z",
            age_seconds: 0,
            priority: null,
            owner: { issue: { key: `LEGION-${index}`, title: "Test" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          })),
        });
      },
    },
    cleanup
  );
  return {
    ...pane,
    completions: () =>
      pane.requests
        .filter((request) => request.path === "/legion/v1/handoff/complete")
        .map((request) => request.body),
    publishes: () =>
      pane.requests
        .filter((request) => request.path === "/v1/messages/publish")
        .map((request) => request.body),
    phaseEntries: () => pane.transcriptEntries(PHASE_STALL_ENTRY),
    pushedTip,
    declarations: () =>
      pane.requests
        .filter((request) => request.path === "/legion/v1/claims/wait")
        .map((request) => request.body),
    newsReports: () =>
      pane.requests
        .filter((request) => request.path === "/legion/v1/claims/wait-news")
        .map((request) => request.body),
  };
}

test.skipIf(skip)(
  "a turn that ends on a legion tool call written as text is reminded that it did not run, and the next turn's real legion tool call posts the completion",
  async () => {
    const pane = await stallPane(setup(), [
      [
        {
          type: "text",
          text: 'court\n<invoke name="legion">\n<parameter name="op">handoff_complete</parameter>\n<parameter name="summary">Stall proof done.</parameter>\n</invoke>',
        },
      ],
      [
        {
          type: "tool_use",
          name: "legion",
          input: { op: "handoff_complete", summary: "Stall proof done." },
        },
      ],
      [{ type: "text", text: "Reported." }],
    ]);

    // The worker registered through the daemon's routes, and its boot asked the daemon what the
    // claim owes before it armed its resume reminder; the reminder asked again before it was sent,
    // and the handoff_complete read the issue's record, read the newest implement record from the
    // pane's Dispatch, held the head that record names to the commit the pane's workspace stands
    // on, pushed to legion/STALL-2@origin, with the pane's jj, minted a grant in-process and posted
    // the completion at that head with the record: no grant file, no `legion` command, and no
    // handoff file. The last turn's end asked once more before it armed anything, since a wait or
    // the design gate's hold would arm nothing (dispatch://INFRA-2040). Each of the two runs the
    // host settled also reported the turn it ended to the daemon (`src/turn-outcome.ts`); the
    // turn that ended on the tool call ends no run of its own, so it earns no report.
    expect(daemonRoutes(pane)).toEqual([
      "/legion/v1/claims/register",
      "/legion/v1/claims/ready",
      "/legion/v1/claims/owed",
      "/legion/v1/claims/owed",
      "/legion/v1/claims/turn-outcome",
      "/legion/v1/issues/STALL-2/record",
      "/legion/v1/grants",
      "/legion/v1/handoff/complete",
      "/legion/v1/claims/owed",
      "/legion/v1/claims/turn-outcome",
    ]);
    expect(pane.pushedTip).toMatch(/^[0-9a-f]{40}$/);
    expect(
      pane.requests
        .filter((request) => request.path.startsWith("/api/v1/issues/STALL-2/handoffs/"))
        .map((request) => request.path)
    ).toEqual(["/api/v1/issues/STALL-2/handoffs/implement/latest"]);
    expect(pane.completions()).toEqual([
      {
        grantId: "stall-grant-1",
        summary: "Stall proof done.",
        verdict: "",
        commit: pane.pushedTip,
        handoffId: STALL_RECORD.id,
        handoffVersion: STALL_RECORD.version,
      },
    ]);
    const turns = pane.turns();
    // Three turns in one run: the text-only one, the reminder's continuation, and the reply to
    // the tool result. None after: the run had had its one continuation, so the last settle armed
    // the ordinary timer, minutes away.
    expect(turns).toHaveLength(3);
    // A Legion pane, both entries loaded: the host offers the model the `legion` tool.
    expect(toolNames(turns[0] as Request)).toContain("legion");
    // The tool text the model is offered names the report's `verdict` and its two values. The
    // tester, the role the daemon takes it from, is not held: the text names the tester in other
    // sentences too, so a check of it could not fail.
    const offered = legionToolDescription(turns[0] as Request);
    expect(namesMissing(offered, ["`verdict`", "pass|fail"])).toEqual([]);
    expect(userText(turns[0] as Request)).not.toContain("handoff_complete");
    // The sentence the settling message earned still leads the reminder it earned.
    expect(userText(turns[1] as Request)).toContain("written as text");
    expect(userText(turns[1] as Request)).toContain(
      `Your turn ended with ${STALL_WORK} still owed`
    );
    // A worker that waits on something outside its own work is told to declare the wait, and no
    // reminder tells it that a WAITING line buys a quiet wait (dispatch://INFRA-2040).
    expect(userText(turns[1] as Request)).toContain("op `declare_wait`");
    expect(userText(turns[1] as Request)).not.toContain("WAITING");
    expect(userText(turns[1] as Request)).toContain("repeats at the end of every turn");
    // The transcript holds every change, which a worker relaunched with --resume restores: one
    // reminder was counted, and the completion's tool call put the count back to zero. The record
    // holds the rhythm alone: what is owed is the daemon's to answer.
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1, 0]);
    expect(entries.at(-1)).toMatchObject({ state: "active", count: 0 });
  },
  120_000
);

test.skipIf(skip)(
  "a worker the daemon says owes work is reminded at the end of every turn, past the architect threshold, and of nothing once the daemon answers that the claim owes nothing",
  async () => {
    const pane = await stallPane(
      setup(),
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "Nearly there." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "Stall proof done." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500", LEGION_STALL_ARCHITECT_AFTER: "2" },
        quietMs: 6_000,
      }
    );

    const turns = pane.turns();
    // One turn per reminder: the prompt's own, three the reminders started — the first the
    // `session_stop` continuation, the two after it the host's own timer, each of which lets the
    // run settle first — and the reply to the tool result.
    expect(turns).toHaveLength(5);
    for (const index of [1, 2, 3]) {
      expect(userText(turns[index] as Request)).toContain(STALL_WORK);
      expect(userText(turns[index] as Request)).toContain("handoff_complete");
      expect(userText(turns[index] as Request)).toContain("repeats at the end of every turn");
    }
    // One entry per delivered reminder is what proves each landed at a turn boundary of its own,
    // which the continuation alone does not show: the count climbs 1, 2, 3, and the completion
    // puts it back to 0. The reply to the tool result settled a fresh run, whose continuation
    // asked the daemon again and was answered that nothing is owed: no sixth turn.
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1, 2, 3, 0]);
    expect(entries.at(-1)).toMatchObject({ state: "active", count: 0 });
    // The run that settled reported its turn's end too (`src/turn-outcome.ts`), which is no
    // question of what is owed: past those reports, the daemon's last word was the owed answer.
    expect(
      daemonRoutes(pane)
        .filter((route) => route !== "/legion/v1/claims/turn-outcome")
        .at(-1)
    ).toBe("/legion/v1/claims/owed");
    // The threshold was crossed at the second reminder; the notice the architect took there
    // stopped nothing, and the third reminder earned no second notice.
    expect(pane.publishes()).toHaveLength(1);
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
);

/** The shortened reminder delay the wait's cases run under (`LEGION_STALL_REMINDER_MS`): a reminder
 * a timer carries comes this long after the turn that armed it settled. */
const REMINDER_MS = 500;

/** The time the declared wait names. The stand-in records it as given: the daemon holds the
 * window, from 1 minute to 24 hours after the declaration. */
const WAKE_AT = "2026-11-22T09:00:00Z";

/** The claim token `runLegionPane` registers the implementer pane under. */
const STALL_CLAIM = "legion-stall-stall-2-implementer";

// c-real-omp's negative control (dispatch://INFRA-2040): a WAITING line buys nothing. A turn that
// ends with one, and with no wait declared, gets the reminder a turn without it gets: the run's
// continuation first, and after the next such turn the reminder's own timer, at the shortened
// reminder delay, though the quiet wait background work buys is ten minutes here.
test.skipIf(skip)(
  "a turn that ends with a WAITING line and no declared wait gets the reminder within the shortened reminder delay",
  async () => {
    const run = setup();
    // Every request the stand-in served, with the moment it was served: only elapsed time shows
    // which delay the reminder came after.
    const seen: { readonly path: string; readonly at: number }[] = [];
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "Pushed the change.\nWAITING: CI on the pull request." }],
        [{ type: "text", text: "WAITING: CI on the pull request." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "CI passed." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: {
          LEGION_STALL_REMINDER_MS: String(REMINDER_MS),
          LEGION_STALL_WAITING_MS: "600000",
        },
        quietMs: 4_000,
        answer: (url) => {
          seen.push({ path: url.pathname, at: Date.now() });
          return undefined;
        },
      }
    );

    // Four turns: the prompt's, the continuation's, the timer's reminder's, and the reply to the
    // tool result. A WAITING line that bought a quiet wait would have left the prompt's alone.
    const turns = pane.turns();
    expect(turns).toHaveLength(4);
    const [, waitingTurn = 0, reminderTurn = 0] = seen
      .filter((request) => request.path === "/anthropic/v1/messages")
      .map((request) => request.at);
    // The second WAITING turn armed the reminder's own timer, not the quiet wait: the reminder's
    // turn came the shortened delay after it, a host timer firing a little early allowed for, and
    // inside the 4 s the pane would have settled in, ten minutes short of the quiet wait.
    expect(reminderTurn - waitingTurn).toBeGreaterThanOrEqual(REMINDER_MS - 100);
    expect(reminderTurn - waitingTurn).toBeLessThan(4_000);
    for (const index of [1, 2]) {
      expect(userText(turns[index] as Request)).toContain(
        `Your turn ended with ${STALL_WORK} still owed`
      );
    }
    expect(stallCounts(await pane.phaseEntries())).toEqual([0, 1, 2, 0]);
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
);

// c-real-omp (dispatch://INFRA-2040): the worker declares a wait with the `legion` tool's
// `declare_wait`, as a real tool call, and ends its turn. While the stand-in's owed answer gives
// that wait, no reminder comes, though the reminder delay is short, and the activity report says
// that none is due. A real Envoy delivery then reaches the session over NATS, as the listener's
// relay hands it: first the update round's ask, under the fixture's key behind the role-forward
// prefix, then another delivery under a key of its own. The plugin reports each to the stand-in
// with the key the card carries, which shows that the real card path keeps the key, and the
// update ask's answer `ended: false` keeps the wait standing. A prompt of the daemon's sent
// between the two reports nothing. The second report ends the wait, so the owed answer gives owed
// work and no wait, and that turn's end gets the reminder.
test.skipIf(skip)(
  "a declared wait holds every reminder; each real Envoy delivery is reported with the key its card carries, a daemon prompt is not, and once the answer gives no wait the turn's end is reminded",
  async () => {
    const run = setup();
    const otherKey = `envoy.role.forward.publish.${randomUUID().replaceAll("-", "")}`;
    /** The session the listener registered, whose deliver subject each card is relayed on. */
    let sessionID = "";
    /** When each turn the gateway served arrived, every activity report and every report of
     * news, in order, and how many runs of the session have ended. */
    const turnsAt: number[] = [];
    const activity: Record<string, unknown>[] = [];
    let reports = 0;
    let runsEnded = 0;
    /** The checks of every wait still open, run after each frame omp prints and each request the
     * stand-in serves. */
    const waits = new Set<() => void>();
    const recheck = (): void => {
      for (const check of [...waits]) check();
    };
    /** What the case read during the idle life, asserted once the pane has settled. */
    const during: {
      quietForMs: number;
      turnsAfterQuiet: number;
      activityBeforeNews: Record<string, unknown> | undefined;
      reportsAfterAsk: number;
      reportsAfterPrompt: number;
    } = {
      quietForMs: 0,
      turnsAfterQuiet: 0,
      activityBeforeNews: undefined,
      reportsAfterAsk: 0,
      reportsAfterPrompt: 0,
    };
    /** Resolves once `holds` does, read after each frame and each request; rejects naming `what`
     * once 60 s pass without it, and with omp's stderr once omp has closed its RPC stream. */
    const until = (rpc: Rpc, what: string, holds: () => boolean): Promise<void> => {
      if (holds()) return Promise.resolve();
      const { promise, resolve, reject } = Promise.withResolvers<void>();
      // The one clock of the wait: it bounds only a pane that never does what is waited for.
      const giveUp = setTimeout(() => {
        waits.delete(check);
        reject(new Error(`the pane never did this within 60 s: ${what}`));
      }, 60_000);
      const check = (): void => {
        if (!holds()) return;
        waits.delete(check);
        clearTimeout(giveUp);
        resolve();
      };
      waits.add(check);
      return Promise.race([promise, rpc.closed]);
    };

    const pane = await stallPane(
      run,
      [
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "declare_wait", waiting_on: "CI on the pull request", wake_at: WAKE_AT },
          },
        ],
        [{ type: "text", text: "Declared the wait on CI." }],
        // The update ask's turn, then the daemon prompt's, then the other delivery's.
        [{ type: "text", text: "That is the update round's ask; CI still runs." }],
        [{ type: "text", text: "CI still runs." }],
        [{ type: "text", text: "Read the review." }],
        // The reminder that turn's end earned, answered with the completion.
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "The review is answered." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: String(REMINDER_MS) },
        // Longer than three reminder delays, so the declaring run settles on a quiet gateway only
        // once a reminder its turn's end armed would have come.
        quietMs: 4 * REMINDER_MS,
        answer: (url, body) => {
          if (url.pathname === "/anthropic/v1/messages") turnsAt.push(Date.now());
          if (url.pathname === "/v1/interests/subscribe" && typeof body.session_id === "string") {
            sessionID = body.session_id;
          }
          if (url.pathname === ACTIVITY_ROUTE) activity.push(body);
          if (url.pathname === "/legion/v1/claims/wait-news") reports += 1;
          recheck();
          return undefined;
        },
        onFrame: (frame) => {
          if (
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false)
          ) {
            runsEnded += 1;
          }
          recheck();
        },
        whileUp: async (rpc) => {
          // No reminder within three times the shortened delay of the declaring turn's end. A real
          // clock, deliberately: the reminder's timer runs in the omp process, which no test can
          // move, and a host that sends nothing emits no signal, so only elapsed time shows it.
          const quietFrom = turnsAt.at(-1) ?? Date.now();
          await Bun.sleep(Math.max(0, quietFrom + 3 * REMINDER_MS + 100 - Date.now()));
          during.quietForMs = Date.now() - quietFrom;
          during.turnsAfterQuiet = turnsAt.length;
          during.activityBeforeNews = activity.at(-1);

          if (sessionID === "") throw new Error("the pane registered no Envoy session");
          const nc = await connect({ servers: run.nats });
          cleanup.push(() => nc.close());
          const codec = StringCodec();
          /** Relays one Envoy delivery under `key` to the session, as the listener's relay of its
           * consumer does, and resolves once the run it started has ended. */
          const deliver = async (seq: number, key: string, summary: string): Promise<void> => {
            const ended = runsEnded;
            await sendRelayFrame({
              connection: nc,
              newHeaders: headers,
              sessionID,
              subject: `notifications.agent.${sessionID}`,
              seq,
              payload: codec.encode(
                JSON.stringify({
                  event_id: `evt-${randomUUID()}`,
                  source: "envoy",
                  source_event_id: `source-${seq}`,
                  // The role lane of the implementer's claim.
                  topic: `${ROLE_TOPIC_PREFIX}${STALL_CLAIM}`,
                  dedupe_key: key,
                  issued_at: Date.now(),
                  payload_summary: summary,
                  trace_id: `trace-${seq}`,
                })
              ),
            });
            await until(rpc, `end the run delivery ${seq} started`, () => runsEnded > ended);
          };

          await deliver(
            1,
            UPDATE_ASK_KEY,
            "Your pod is being updated: push what you keep, then answer with ready_for_update."
          );
          during.reportsAfterAsk = reports;

          const ended = runsEnded;
          rpc.send({ type: "prompt", message: "Look at CI on STALL-2's pull request again." });
          await until(rpc, "end the daemon prompt's run", () => runsEnded > ended);
          during.reportsAfterPrompt = reports;

          await deliver(2, otherKey, "The review of STALL-2 is in.");
        },
      }
    );

    // The declaration went to the daemon as a real tool call, with the claim, and its answer
    // reached the model.
    expect(pane.declarations()).toEqual([
      {
        claimToken: STALL_CLAIM,
        sessionId: expect.any(String),
        secret: expect.any(String),
        waitingOn: "CI on the pull request",
        wakeAt: WAKE_AT,
      },
    ]);
    expect(pane.toolResults()[0]?.text).toContain('"wait-1"');
    // The wait held the reminder: no turn started for three reminder delays after the declaring
    // turn, and the turn's end reported that no reminder is due.
    expect(during.quietForMs).toBeGreaterThanOrEqual(3 * REMINDER_MS);
    expect(during.turnsAfterQuiet).toBe(2);
    expect(during.activityBeforeNews).toMatchObject({
      activity: { phase: { nextReminderAt: null } },
    });
    // Seven turns: the prompt's and the one its declaration's result continued, the update
    // ask's, the daemon prompt's, the other delivery's, the reminder's, and the reply to the
    // completion's result.
    const turns = pane.turns();
    expect(turns).toHaveLength(7);
    // Each delivery was reported with the key its card carries, against the wait, and the
    // daemon prompt between them was not reported at all.
    expect(pane.newsReports()).toEqual([
      expect.objectContaining({
        claimToken: STALL_CLAIM,
        waitId: "wait-1",
        deliveryKey: UPDATE_ASK_KEY,
      }),
      expect.objectContaining({
        claimToken: STALL_CLAIM,
        waitId: "wait-1",
        deliveryKey: otherKey,
      }),
    ]);
    expect(during.reportsAfterAsk).toBe(1);
    expect(during.reportsAfterPrompt).toBe(1);
    // The update ask's `ended: false` kept the wait standing through its turn and the daemon
    // prompt's: no reminder reached the worker before the other delivery's turn ended.
    expect(userText(turns[4] as Request)).not.toContain("Your turn ended with");
    // That delivery ended the wait, and the owed answer then gave owed work and no wait: the
    // turn's end got the reminder, which names the operation that declares a wait.
    expect(userText(turns[5] as Request)).toContain(
      `Your turn ended with ${STALL_WORK} still owed`
    );
    expect(userText(turns[5] as Request)).toContain("op `declare_wait`");
    expect(pane.completions()).toHaveLength(1);
  },
  180_000
);

test.skipIf(skip)(
  "a run of unanswered reminders tells the issue's architect over Envoy, retried under its own key until the listener takes it, and never again",
  async () => {
    const run = setup();
    let publishes = 0;
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "The architect has not answered my question yet." }],
        [{ type: "tool_use", name: "legion", input: { op: "read_record", issue: "STALL-2" } }],
        [{ type: "text", text: "The record still says implementing." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "Stall proof done." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500", LEGION_STALL_ARCHITECT_AFTER: "2" },
        quietMs: 6_000,
        // No live session holds the architect's role the first time, as the listener answers
        // while that architect is between sessions; the second is taken.
        answer: (url) => {
          if (url.pathname !== "/v1/messages/publish") return undefined;
          publishes += 1;
          return publishes === 1
            ? Response.json({ reason: "unclaimed" }, { status: 404 })
            : undefined;
        },
      }
    );

    const notices = pane.publishes();
    expect(notices).toHaveLength(2);
    // The refused notice is sent again under the key it was minted with, so a listener that took
    // one whose answer was lost does not deliver it twice.
    expect(notices[1]?.idempotency_key).toBe(String(notices[0]?.idempotency_key));
    // The stand-in's owed answer names no architect as the claim's owner, so the tree's architect
    // is told.
    expect(notices[1]?.topic).toBe("notifications.role.legion-stall-stall-1-architect");
    expect(String(notices[1]?.message)).toContain("STALL-2");
    // The notice names the work the daemon answered was owed.
    expect(String(notices[1]?.message)).toContain(
      `its turns keep ending with ${STALL_WORK} still owed`
    );
    expect(String(notices[1]?.message)).toContain(
      "The architect has not answered my question yet."
    );
    expect(JSON.parse(String(notices[1]?.payload))).toMatchObject({
      issue: "STALL-2",
      role: "implementer",
      work: STALL_WORK,
      reminders: 3,
    });
    // The worker then called a tool, which put the count back to zero: the reminder after it is
    // a run's first again, so it told the architect nothing.
    expect(pane.turns()).toHaveLength(7);
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
);

test.skipIf(skip)(
  "a real tool call between reminders puts the count back to zero, so the architect of a working worker is not told",
  async () => {
    const run = setup();
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "Pushed." }],
        [{ type: "text", text: "Still pushing." }],
        // The timer's reminder is answered with a real tool call: the worker is working.
        [{ type: "tool_use", name: "bash", input: { command: "echo ran" } }],
        [{ type: "text", text: "Ran the check." }],
        [{ type: "text", text: "Nothing new." }],
        // The second run of reminders ends with a declared wait, which holds the next one, so
        // the run settles on the scripted turns alone.
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "declare_wait", waiting_on: "CI on the pull request", wake_at: WAKE_AT },
          },
        ],
        [{ type: "text", text: "Waiting on CI." }],
      ],
      {
        env: {
          LEGION_STALL_REMINDER_MS: "500",
          LEGION_STALL_ARCHITECT_AFTER: "3",
        },
        quietMs: 4_000,
      }
    );

    expect(pane.turns()).toHaveLength(7);
    // Two reminders, the tool call putting the count back to zero, then two more, and the
    // declaration's tool call putting it back again: never three unanswered in a row, so the
    // threshold is never reached.
    expect(stallCounts(await pane.phaseEntries())).toEqual([0, 1, 2, 0, 1, 2, 0]);
    expect(pane.publishes()).toEqual([]);
  },
  120_000
);

/** The stand-in daemon's answer that the claim owes nothing: its record has moved on. */
const OWES_NOTHING = { owes: false, work: "" };

test.skipIf(skip)(
  "a worker the daemon says owes nothing is reminded of nothing: its run settles where it is",
  async () => {
    const run = setup();
    const pane = await stallPane(
      run,
      // A second reply is scripted so a turn that should not happen shows up as a turn rather
      // than as the gateway's "no reply scripted" refusal.
      [
        [{ type: "text", text: "I completed this from the shell." }],
        [{ type: "text", text: "Understood." }],
      ],
      {
        quietMs: 4_000,
        answer: (url) =>
          url.pathname === "/legion/v1/claims/owed" ? Response.json(OWES_NOTHING) : undefined,
      }
    );

    // One turn, the prompt's own, and the wait proves no second: the daemon answered that the
    // claim owes nothing, so `session_stop` ran no continuation at all.
    expect(pane.turns()).toHaveLength(1);
    expect(daemonRoutes(pane)).toContain("/legion/v1/claims/owed");
    expect(pane.publishes()).toEqual([]);
    expect(pane.completions()).toEqual([]);
    // Nothing was put in front of the worker, so nothing was counted.
    expect(stallCounts(await pane.phaseEntries())).toEqual([0]);
  },
  120_000
);

test.skipIf(skip)(
  "a daemon that answers between reminders that the claim owes nothing stops them: the reminder its timer carried is never sent",
  async () => {
    const run = setup();
    let asked = 0;
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "A turn that should never start." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500" },
        quietMs: 4_000,
        answer: (url) => {
          if (url.pathname !== "/legion/v1/claims/owed") return undefined;
          asked += 1;
          // The first three questions are the boot's, the continuation reminder's own and the
          // second turn's end's, which arms the timer (dispatch://INFRA-2040: every turn's end
          // asks before it arms anything); by the timer's the daemon's record has moved on.
          return Response.json(asked <= 3 ? { owes: true, work: STALL_WORK } : OWES_NOTHING);
        },
      }
    );

    // Two turns: the prompt's and the continuation reminder's. The timer the second settle armed
    // was answered that nothing is owed and sent the reminder it carried to nobody.
    expect(pane.turns()).toHaveLength(2);
    expect(asked).toBe(4);
    expect(pane.publishes()).toEqual([]);
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1]);
    expect(entries.at(-1)).toMatchObject({ state: "active", count: 1 });
  },
  120_000
);

// Where the daemon's answer does not arrive the worker is still nudged, and the nudge says
// it could not read the record. It names no phase and no owed work, because it read neither, and
// the one operation it names is `declare_wait`. The stand-in answers `claims/owed` as a proxy
// does for a daemon it cannot reach: a 503 whose body is not the daemon's (`daemonUnreachable` in
// packages/pi-shared/src/daemon-client.ts). The daemon comes back once the third turn has been
// served, and its answer that nothing is owed ends the reminding there.
test.skipIf(skip)(
  "with the daemon unreachable the worker is still nudged, by a reminder that names no phase and no operation but declare_wait",
  async () => {
    const run = setup();
    let served = 0;
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "Pushed the change." }],
        [{ type: "text", text: "Still pushing." }],
        [{ type: "text", text: "Still waiting for the daemon to come back." }],
      ],
      {
        env: { LEGION_STALL_REMINDER_MS: "500" },
        quietMs: 4_000,
        answer: (url) => {
          if (url.pathname === "/anthropic/v1/messages") served += 1;
          if (url.pathname !== "/legion/v1/claims/owed") return undefined;
          return served < 3
            ? new Response("upstream connect error", { status: 503 })
            : Response.json(OWES_NOTHING);
        },
      }
    );

    // Three turns: the prompt's, the continuation's and the timer's, each reminder the unread
    // one. The assignment's own turn named no operation, so what the reminders' turns carry is
    // theirs.
    const turns = pane.turns();
    expect(turns).toHaveLength(3);
    expect(userText(turns[0] as Request)).not.toContain("handoff_complete");
    for (const index of [1, 2]) {
      const text = userText(turns[index] as Request);
      expect(text).toContain("could not read the Legion daemon's record of what your role owes");
      expect(text).not.toContain("handoff_complete");
      expect(text).not.toContain(STALL_WORK);
      expect(text).not.toContain("implementing");
    }
    expect(stallCounts(await pane.phaseEntries())).toEqual([0, 1, 2]);
  },
  120_000
);

// The run-end ask nudge (extensions/envoy.ts) is a hidden side-turn self-check whose
// WAITING verdict — and nothing else — buys one steered turn. Two host behaviours carry it, and
// only the real binary can say either: an ephemeral call is served as a Messages request over a
// snapshot of the conversation that the transcript never keeps, and a `triggerTurn` continuation
// of an agent-attributed custom message re-enters no `before_agent_start`, so it arms no period.
// The check the period owes is re-armed by the agent's own work, and a continuation that only
// replies calls no tool, so it owes none: those two together are why the nudge cannot nudge
// itself. If either changed, an ordinary session would nudge itself to the per-period cap after
// every settle. This is what a pin bump is re-run against.
test.skipIf(skip)(
  "a PROCEEDING self-check leaves an ask-free stop with no visible turn at all",
  async () => {
    const run = setup();
    const pane = await stallPane(
      run,
      // A second reply is scripted so a turn that should not happen shows up as a turn rather
      // than as the gateway's "no reply scripted" refusal.
      [[{ type: "text", text: "Done." }], [{ type: "text", text: "Understood." }]],
      // An ordinary session, not a Legion pane: a Legion-driven one is excluded from the nudge.
      { legion: false, openAsks: 0, selfCheck: "PROCEEDING", quietMs: 8_000 }
    );

    // One turn, and the wait proves no second: the user's own. The agent said it is not waiting
    // on anyone, so the whole stop cost one hidden call the user never saw.
    expect(pane.turns()).toHaveLength(1);
    // Both entries loaded but no LEGION_* in the environment: a person's own session gets no
    // `legion` tool, and the Legion entry never speaks to the daemon.
    expect(toolNames(pane.turns()[0] as Request)).not.toContain("legion");
    expect(
      pane.requests.map((request) => request.path).filter((p) => p.startsWith("/legion/"))
    ).toEqual([]);
    const selfChecks = pane.selfChecks();
    expect(selfChecks).toHaveLength(1);
    expect(userText(selfChecks[0] as Request)).toContain("WAITING or PROCEEDING");
    const asks = pane.requests.filter((request) => request.path === "/api/v1/asks/open");
    expect(asks).toHaveLength(2);
  },
  180_000
);

test.skipIf(skip)(
  "a WAITING self-check with a held ask runs exactly one nudge and lists the ask",
  async () => {
    const run = setup();
    const heldQuestion = "Which deployment window should I use?";
    const pane = await stallPane(
      run,
      [[{ type: "text", text: "Done." }], [{ type: "text", text: "Understood." }]],
      {
        legion: false,
        openAskQuestions: [heldQuestion],
        selfCheck: "WAITING",
        quietMs: 8_000,
      }
    );

    const turns = pane.turns();
    // Two, and the wait proves no third: the user's turn, and the one nudge continuation.
    expect(turns).toHaveLength(2);
    expect(userText(turns[0] as Request)).not.toContain("no open ask in Dispatch");
    expect(userText(turns[1] as Request)).toContain(
      "You just said you are waiting on a human for something no open ask in Dispatch covers."
    );
    expect(userText(turns[1] as Request)).toContain("dispatch ask");
    const selfChecks = pane.selfChecks();
    expect(selfChecks).toHaveLength(1);
    expect(userText(selfChecks[0] as Request)).toContain(heldQuestion);
    // The continuation's own stop found the period already fired, so it ran no second
    // self-check and read no third open-ask snapshot: one nudge per period, and no loop.
    const asks = pane.requests.filter((request) => request.path === "/api/v1/asks/open");
    expect(asks).toHaveLength(2);
  },
  180_000
);

// Background work is on for every role, so a worker can end its turn with a job of its own
// still running. The pinned Oh My Pi holds `session_stop` back until its agent-owned async jobs
// are idle (omp://extensions.md: it "defers until agent-owned background jobs are fully idle"),
// so the phase reminder — which rides that hook — never fires into such a wait, and the job's own
// result wakes the session into the next turn. Nothing in the plugin decides this, which is
// exactly why only the real binary can say it: a pin that stopped deferring would remind a
// working worker that it has done nothing, with every unit test still green.
test.skipIf(skip)(
  "a turn that ends while the session waits on its own background job gets no reminder, and the job's result wakes it",
  async () => {
    const run = setup();
    const pane = await stallPane(
      run,
      [
        [call("bash", { command: "sleep 4; echo background-done", async: true })],
        [{ type: "text", text: "I started the build; I will be woken when it is done." }],
        // The woken turn completes the phase, so the daemon's answer at the next turn's end owes
        // nothing and the run settles on the scripted turns alone.
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "handoff_complete", summary: "The build passed." },
          },
        ],
        [{ type: "text", text: "Reported." }],
      ],
      // Longer than the job, so the pane settles on the woken turn rather than before it.
      { quietMs: 12_000 }
    );

    const turns = pane.turns();
    // Four turns: the assignment's, the one its tool result continued, the one the finished job
    // woke, and the reply to the completion's result. Nothing prompted the third.
    expect(turns).toHaveLength(4);
    expect(userText(turns[2] as Request)).toContain("background-done");
    // No reminder reached the worker in between: the turn that ended with the job running earned
    // none, because `session_stop` never fired while the job was pending.
    for (const turn of turns) {
      expect(userText(turn as Request)).not.toContain(`Your turn ended with ${STALL_WORK}`);
    }
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries).filter((count) => count !== 0)).toEqual([]);
    expect(entries.at(-1)).toMatchObject({ state: "active", count: 0 });
    expect(pane.completions()).toHaveLength(1);
  },
  180_000
);
