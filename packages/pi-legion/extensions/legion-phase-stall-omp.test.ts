import { afterEach, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { symlink, writeFile } from "node:fs/promises";
import * as path from "node:path";
import { roleTopic } from "@legion/contracts";
import { LEGION_PHASES, type LegionOwed, type LegionPhase } from "@legion/contracts/legion-api";
import { OWED_READ_BOUND_MS } from "@legion/pi-shared/phase-reminder";
import { PHASE_STALL_ENTRY } from "@legion/pi-shared/phase-stall";
import {
  type Cleanup,
  call,
  type LegionPane,
  type LegionPaneOptions,
  ompNatsGuard,
  type PaneStep,
  type RelayConnection,
  type Reply,
  type Request,
  roleLaneEnvelope,
  runLegionPane,
  sendRelayFrame,
} from "@legion/pi-shared/test/omp-harness";
import { connect, headers, StringCodec } from "nats";

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
// It is also the only check that the run-end nudge's hidden self-check starts no run: the nudge
// treats any `agent_start` after a settle as a newer run and withholds its steer, so a host that
// counted the side turn as a run would silence the nudge with every unit test still green.
// The WAITING self-check case below fails if that ever changes.
const { skip, setup } = ompNatsGuard();

/** The phase-worker pane the phase-stall cases run, with what its stand-in daemon recorded and
 * its transcript's phase-stall entries. */
interface StallPane extends Omit<LegionPane, "resume"> {
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
  /** Each answer the stand-in's owed route gave from its record (`owedOf`), in order. An answer a
   * case gives through its own `answer` is not among them. */
  readonly owedAnswers: () => LegionOwed[];
  /** How many times the pane read STALL-2's record (`GET /legion/v1/issues/STALL-2/record`). */
  readonly recordReads: () => number;
  /** `LegionPane.resume`, with this file's steps (`StallStep`) among the prompts. */
  readonly resume: (
    replies: readonly Reply[],
    prompt: string | readonly (string | StallStep)[]
  ) => Promise<void>;
}

/** `POST /legion/v1/claims/activity`, the session's own report of what it is doing
 * (packages/pi-shared/src/session-activity.ts): the plugin sends it after each turn and whenever
 * the report's content changes, so how many a case sees is the host's timing and no case here is
 * about it. What those reports hold is asserted in extensions/legion-session-activity-omp.test.ts. */
const ACTIVITY_ROUTE = "/legion/v1/claims/activity";

/** Every daemon route the pane called, in order, the activity heartbeat left out. */
function daemonRoutes(pane: Pick<LegionPane, "requests">): string[] {
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

/** The handoff the worker wrote to the stand-in Dispatch with `dispatch handoff-write` before
 * completing: version 1 of the record of the phase its role works (`ROLE_PHASE`), the newest
 * record its `handoff_complete` reads and names. Its body names the head the round pushed, which
 * the workspace this file builds decides, so the body is written once that push has happened. */
const STALL_RECORD = {
  id: "0c9e7d2a-41b6-4f58-9a3e-6d1b2c8f5e70",
  issue_key: "STALL-2",
  phase: "implement",
  version: 1,
  author: { kind: "agent", session_id: "ses_stall" },
  created_at: "2026-10-09T08:00:00Z",
};

/** One handoff record of STALL-2 as the stand-in Dispatch holds it: `phase` is the phase's word,
 * and `body` the text the writer sent. */
interface HandoffRecord {
  readonly id: string;
  readonly issue_key: string;
  readonly phase: string;
  readonly version: number;
  readonly author: unknown;
  readonly created_at: string;
  readonly body: string;
}

/** Runs `argv` in `cwd` as the pane's identity, failing loudly on a non-zero exit, and answers
 * its stdout. */
async function runAsPane(argv: readonly string[], cwd: string): Promise<string> {
  const child = Bun.spawn([...argv], {
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
  if (code !== 0) throw new Error(`${argv.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

/** What the stand-in daemon calls the work it answers the implementer owes
 * (`POST /legion/v1/claims/owed`), which the reminder and the architect notice name. */
const STALL_WORK = "the implementing phase of STALL-2";

/** The stand-in daemon's answer that the claim owes nothing: its record has moved on. */
const OWES_NOTHING = { owes: false, work: "" };

/** The daemon's state as the pane's `handoff_complete` reads it, with STALL-2 at the phase the
 * stand-in's record holds: at implementing, the phase whose handoff is the `implement` record,
 * unless a case moves it. */
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

/** The roles a stall pane's claim holds in these cases. Each works one phase of STALL-2 and ends
 * it with a handoff record of that phase's word: the daemon's `phase.Handoff` table, which
 * `FILE_BACKED` in packages/pi-shared/src/handoff-record.ts mirrors. */
const ROLE_PHASE = {
  planner: { phase: "planning", word: "plan" },
  implementer: { phase: "implementing", word: "implement" },
  tester: { phase: "testing", word: "test" },
  reviewer: { phase: "reviewing", word: "review" },
} as const satisfies Readonly<
  Record<string, { readonly phase: LegionPhase; readonly word: string }>
>;

type StallRole = keyof typeof ROLE_PHASE;

/** The phase a completion the stand-in takes moves STALL-2 to: the one after the phase it took
 * the completion of, as the workflow moves an issue on. */
const NEXT_PHASE: Partial<Record<LegionPhase, LegionPhase>> = {
  planning: "implementing",
  implementing: "testing",
  testing: "reviewing",
  reviewing: "accepting",
};

/**
 * The stand-in daemon's record of STALL-2. A case owns it and may change it while its pane runs:
 * a change is the daemon moving the issue, and the next owed answer and state read follow it.
 */
interface StallRecord {
  /** The phase STALL-2 stands in. */
  phase: LegionPhase;
}

/**
 * The stand-in's owed answer from its record, as `owedBy` (packages/daemon/internal/api/owed.go)
 * answers in the shapes this file runs: the claim owes its phase while STALL-2 stands in the
 * phase its role works, and owes nothing while the issue stands in a phase of another role.
 */
function owedOf(role: StallRole, record: StallRecord): LegionOwed {
  return record.phase === ROLE_PHASE[role].phase
    ? { owes: true, work: `the ${record.phase} phase of STALL-2` }
    : OWES_NOTHING;
}

/** The stand-in's answer to a completion it takes, its default: the workflow takes the completion
 * of the phase the issue stands in, and STALL-2 moves on to its next phase. */
function takeCompletion(record: StallRecord): Response {
  record.phase = NEXT_PHASE[record.phase] ?? record.phase;
  return Response.json({});
}

/** The daemon's refusal of a completion of a phase the issue no longer stands in
 * (`HANDOFF_NOT_CURRENT_PHASE`, packages/daemon/internal/api/handoff.go), which records nothing
 * and moves nothing. */
function refuseNotCurrentPhase(record: StallRecord): Response {
  return Response.json(
    {
      error: `STALL-2 stands in ${record.phase}, a phase this claim's role does not work`,
      code: "HANDOFF_NOT_CURRENT_PHASE",
    },
    { status: 409 }
  );
}

/**
 * How long the stand-in takes to answer STALL-2's record (`GET /legion/v1/issues/STALL-2/record`):
 * past the owed read's bound, as INFRA's daemon answered the whole state the `legion` tool read
 * before dispatch://INFRA-2694 in 9.2 s to 9.6 s (the measurement under `OWED_READ_BOUND_MS` in
 * packages/pi-shared/src/phase-reminder.ts). Only the `legion` tool's own calls read it
 * (`recordReadingCalls`), and the both-sides rows assert that nothing else did: a reminder that
 * read the daemon's record of the issue, the whole state then, is what reminded every finished
 * role on INFRA (D6 of dispatch://INFRA-2746/artifact/plan-md).
 */
const RECORD_ANSWER_MS = OWED_READ_BOUND_MS + 1_000;

/** How long a pane whose runs make a `legion` call that reads the issue's record waits for the
 * gateway to fall quiet: past that read and the tool's other requests after it, so the call is
 * not taken for a run that has settled. */
const QUIET_PAST_RECORD_MS = RECORD_ANSWER_MS + 4_000;

/** What a step of a stall pane's life (`StallStep`) is handed. */
interface StallScene {
  /** The stand-in daemon's record of STALL-2, which the step may move. */
  readonly record: StallRecord;
  /** The bare repository the pane's workspace pushes to, which holds `legion/STALL-2`. */
  readonly origin: string;
  /** Sends the session one message published to its claim's role topic, as the listener's role
   * lane forwards it to the session's own subject, and resolves once the session has taken it.
   * The message starts a turn of an idle session. */
  readonly roleMessage: (summary: string) => Promise<void>;
}

/** One thing a case does to its running stall pane in place of the daemon's prompt (`PaneStep`). */
type StallStep = (scene: StallScene) => Promise<void>;

/** The session id the pane registered with the listener: the one its newest registration names. */
function sessionOf(requests: readonly Request[]): string {
  const named = requests
    .filter((request) => request.path === "/v1/interests/subscribe")
    .map((request) => request.body.session_id)
    .filter((id): id is string => typeof id === "string" && id !== "")
    .at(-1);
  if (named === undefined) throw new Error("the pane has not registered with the listener");
  return named;
}

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
 * `legion`, `quietMs`, `selfCheck` and environment, what the stand-in answers ahead of this
 * file's own routes, the stand-in's open-ask snapshot, the claim's role, the stand-in daemon's
 * record and completion answer, and the prompts and steps of the pane's first life. */
interface PaneOptions
  extends Pick<LegionPaneOptions, "legion" | "quietMs" | "selfCheck" | "env" | "answer"> {
  /** The count the stand-in Dispatch's open-ask snapshot answers with. */
  readonly openAsks?: number;
  /** The held ask questions the stand-in returns in the session's open-ask snapshot. */
  readonly openAskQuestions?: readonly string[];
  /** The role the pane's claim holds; the implementer by default. */
  readonly role?: StallRole;
  /** The stand-in daemon's record of STALL-2, which the case may move while the pane runs;
   * STALL-2 in the phase `role` works by default. */
  readonly record?: StallRecord;
  /** What the stand-in answers a completion (`POST /legion/v1/handoff/complete`), given its
   * record; `takeCompletion` by default. */
  readonly complete?: (record: StallRecord) => Response;
  /** The daemon's prompts and the case's steps of the pane's first life, in order
   * (`LegionPaneOptions.prompt`); the implementer's assignment by default. */
  readonly prompt?: string | readonly (string | StallStep)[];
}

/**
 * Runs one phase-worker pane on the real Oh My Pi (`runLegionPane`), the stand-in answering the
 * daemon's state, owed, completion, backward-move and update-answer routes and, as Dispatch, the
 * issue's handoff records and the open-ask snapshot, until its run settles.
 *
 * The stand-in daemon answers from one record of STALL-2 (`StallRecord`), which the case may
 * own and move while the pane runs. Its owed route answers `owedOf` that record: `STALL_WORK` for
 * the implementer while the issue stands in implementing, and nothing once a completion has
 * moved it on, as the daemon's own record does once the workflow takes a completion (`owedBy` in
 * `packages/daemon/internal/api/owed.go`); a case that answers `claims/owed` itself decides
 * otherwise. Its record route for STALL-2 answers after `RECORD_ANSWER_MS`. A
 * `request_backward_move` moves the record to the phase it names.
 *
 * The pane's workspace is a jj repository whose change is committed and pushed to a bare
 * origin's `legion/STALL-2`, and the stand-in Dispatch holds the record of the role's phase
 * written to it, as a worker leaves both before completing, so the pane's `handoff_complete`
 * finds the record and the pushed tip. The Dispatch takes a `dispatch handoff-write` as the next
 * version of the phase it names.
 */
async function stallPane(
  run: { readonly binary: string; readonly nats: string },
  replies: readonly Reply[],
  options: PaneOptions = {}
): Promise<StallPane> {
  const role = options.role ?? "implementer";
  const record: StallRecord = options.record ?? { phase: ROLE_PHASE[role].phase };
  const complete = options.complete ?? takeCompletion;
  let pushedTip = "";
  let origin = "";
  /** The stand-in Dispatch's handoff records of STALL-2, oldest first. */
  const records: HandoffRecord[] = [];
  const owedAnswers: LegionOwed[] = [];
  /** The connection the case's role messages are relayed on, opened by the first of them. */
  let connection: RelayConnection | undefined;
  /** The stream sequence of the last frame relayed: one stream for the whole case. */
  let relayed = 0;
  /** The claim's role topic: the harness names the claim `legion-<name>-<issue>-<role>`. */
  const topic = roleTopic(`legion-stall-stall-2-${role}`);
  /** Sends the session one message published to its claim's role topic
   * (`StallScene.roleMessage`), relayed on the connection the first of them opens. `requests`
   * are the pane's requests so far, which name the session the listener relays to. */
  const roleMessage = async (requests: readonly Request[], summary: string): Promise<void> => {
    if (connection === undefined) {
      const opened = await connect({ servers: run.nats });
      cleanup.push(() => opened.close());
      connection = opened;
    }
    relayed += 1;
    const sessionID = sessionOf(requests);
    await sendRelayFrame({
      connection,
      newHeaders: headers,
      sessionID,
      subject: `notifications.agent.${sessionID}`,
      seq: relayed,
      payload: StringCodec().encode(
        roleLaneEnvelope(topic, summary, `stall-${relayed}-${randomUUID()}`)
      ),
    });
  };
  const stepsOf = (
    prompt: string | readonly (string | StallStep)[]
  ): string | readonly (string | PaneStep)[] =>
    typeof prompt === "string"
      ? prompt
      : prompt.map((step) =>
          typeof step === "string"
            ? step
            : (requests) =>
                step({ record, origin, roleMessage: (summary) => roleMessage(requests, summary) })
        );
  const pane = await runLegionPane(
    run.binary,
    replies,
    {
      name: "stall",
      nats: run.nats,
      role,
      tree: "STALL-1",
      issue: "STALL-2",
      prompt: stepsOf(options.prompt ?? "Implement STALL-2."),
      dispatch: true,
      legion: options.legion,
      quietMs: options.quietMs,
      selfCheck: options.selfCheck,
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
        origin = path.join(path.dirname(workspace), "origin.git");
        if ((await Bun.spawn(["git", "init", "--bare", "--quiet", origin]).exited) !== 0) {
          throw new Error(`git init --bare ${origin} failed`);
        }
        await runAsPane([jj, "git", "init", workspace], path.dirname(workspace));
        // From here on, every command runs in the workspace, as the pane's own shell does.
        const jjIn = (...args: string[]) => runAsPane([jj, ...args], workspace);
        await jjIn("git", "remote", "add", "origin", origin);
        await writeFile(path.join(workspace, "main.py"), 'print("stall")\n');
        await jjIn("describe", "-m", "implement: the change");
        await jjIn("new");
        await jjIn("bookmark", "set", "legion/STALL-2", "-r", "@-");
        await jjIn("git", "push", "--bookmark", "legion/STALL-2");
        pushedTip = await jjIn("log", "-r", "@-", "--no-graph", "-T", "commit_id");
        // The worker's `dispatch handoff-write --issue STALL-2 --phase <word>`, whose body names
        // the tip it just pushed.
        const { word } = ROLE_PHASE[role];
        records.push({
          ...STALL_RECORD,
          phase: word,
          body: JSON.stringify({
            schemaVersion: 1,
            phase: word,
            issue: "STALL-2",
            head: pushedTip,
          }),
        });
      },
      answer: (url, body) => {
        const own = options.answer?.(url, body);
        if (own !== undefined) return own;
        // `handoff_complete` and `read_record` read one issue's record (dispatch://INFRA-2694), here
        // STALL-2's as the stand-in's record holds it now; nothing of this pane asks for the whole
        // project body.
        if (url.pathname === "/legion/v1/issues/STALL-2/record") {
          const issue = { ...STALL_STATE.issues["STALL-2"], phase: record.phase };
          // A real clock, deliberately: the reader is a real Oh My Pi process, whose own clock
          // decides whether an answer this late is inside any bound it reads with.
          return Bun.sleep(RECORD_ANSWER_MS).then(() => Response.json(issue));
        }
        if (url.pathname === "/legion/v1/claims/owed") {
          const owed = owedOf(role, record);
          owedAnswers.push(owed);
          return Response.json(owed);
        }
        if (url.pathname === "/legion/v1/handoff/complete") return complete(record);
        // A phase worker's own `request_backward_move`, which the daemon takes: STALL-2 goes back
        // to the phase it names.
        if (url.pathname === "/legion/v1/phase/backward") {
          const to = LEGION_PHASES.find((phase) => phase === body.to);
          if (to === undefined) {
            return Response.json({ error: `no phase ${String(body.to)}` }, { status: 400 });
          }
          record.phase = to;
          return Response.json({});
        }
        // The worker's answer to an update round (`ready_for_update`), which the daemon
        // acknowledges with no body.
        if (url.pathname === "/legion/v1/claims/ready-for-update") {
          return new Response(null, { status: 204 });
        }
        const handoff = /^\/api\/v1\/issues\/STALL-2\/handoffs\/([a-z]+)(\/latest)?$/.exec(
          url.pathname
        );
        if (handoff !== null) {
          const word = handoff[1] ?? "";
          const ofWord = records.filter((written) => written.phase === word);
          if (handoff[2] === undefined) {
            // A `dispatch handoff-write`: the next version of the phase it names, written by the
            // actor it names.
            const written = {
              id: randomUUID(),
              issue_key: "STALL-2",
              phase: word,
              version: ofWord.length + 1,
              author: body.actor,
              created_at: new Date().toISOString(),
              body: String(body.body),
            };
            records.push(written);
            const { body: _, ...summary } = written;
            return Response.json(summary, { status: 201 });
          }
          const newest = ofWord.at(-1);
          return newest === undefined
            ? Response.json(
                { error: `no ${word} handoff on STALL-2`, code: "HANDOFF_NOT_FOUND" },
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
    owedAnswers: () => [...owedAnswers],
    recordReads: () =>
      pane.requests.filter((request) => request.path === "/legion/v1/issues/STALL-2/record").length,
    resume: (more, prompt) => pane.resume(more, stepsOf(prompt)),
  };
}

/**
 * How many `legion` calls the pane's conversation holds that read the issue's record:
 * `handoff_complete` and `read_record` read it once each (`executePhaseOp`,
 * packages/pi-shared/src/legion-phase-ops.ts), and nothing else a pane runs reads it. A pane that
 * read it more often than this read it for something else.
 */
function recordReadingCalls(pane: StallPane): number {
  // Every turn carries the whole conversation so far, the lives before a resume included, so a
  // call is counted once by its id.
  const calls = new Set<string>();
  for (const turn of pane.turns()) {
    const messages: unknown[] = Array.isArray(turn.body.messages) ? turn.body.messages : [];
    for (const message of messages) {
      if (typeof message !== "object" || message === null || !("content" in message)) continue;
      if (!Array.isArray(message.content)) continue;
      for (const block of message.content as unknown[]) {
        if (typeof block !== "object" || block === null) continue;
        if (!("type" in block) || block.type !== "tool_use") continue;
        if (!("name" in block) || block.name !== "legion" || !("id" in block)) continue;
        const input = "input" in block ? block.input : undefined;
        const op = typeof input === "object" && input !== null && "op" in input ? input.op : "";
        if (op === "handoff_complete" || op === "read_record") calls.add(String(block.id));
      }
    }
  }
  return calls.size;
}

/**
 * What every both-sides row asserts of a role that owes nothing
 * (dispatch://INFRA-2746/spec#b-r3 and #b-r5): no reminder reached it, as a turn's text, as a
 * timer's message or as a count its transcript records; its architect was told nothing; and the
 * pane read the issue's record only for its own `legion` calls, never for the reminder
 * (`RECORD_ANSWER_MS`).
 */
async function expectRemindedOfNothing(pane: StallPane): Promise<void> {
  for (const turn of pane.turns()) {
    expect(JSON.stringify(turn.body.messages ?? null)).not.toContain(
      "repeats at the end of every turn"
    );
  }
  expect(await pane.transcriptEntries("legion-phase-reminder")).toEqual([]);
  expect(stallCounts(await pane.phaseEntries()).filter((count) => count !== 0)).toEqual([]);
  expect(pane.publishes()).toEqual([]);
  expect(pane.recordReads()).toBe(recordReadingCalls(pane));
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

    // The worker registered through the daemon's routes; the reminder asked the daemon what the
    // claim owes before it was sent, and the handoff_complete read the issue's record, read the
    // newest implement record from the pane's Dispatch, held the head that record names to the
    // commit the pane's workspace stands on, pushed to legion/STALL-2@origin, with the pane's jj,
    // minted a grant in-process and posted the completion at that head with the record: no grant
    // file, no `legion` command, and no handoff file. Each of the two runs the host settled also
    // reported the turn it ended to the daemon (`src/turn-outcome.ts`); the turn that ended on the
    // tool call ends no run of its own, so it earns no report.
    expect(daemonRoutes(pane)).toEqual([
      "/legion/v1/claims/register",
      "/legion/v1/claims/ready",
      "/legion/v1/claims/owed",
      "/legion/v1/claims/turn-outcome",
      "/legion/v1/issues/STALL-2/record",
      "/legion/v1/grants",
      "/legion/v1/handoff/complete",
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
    // Every role is offered the same tool text, and it gives a verdict to the tester alone.
    expect(legionToolDescription(turns[0] as Request)).toContain(
      "`verdict` pass|fail from the tester alone"
    );
    expect(userText(turns[0] as Request)).not.toContain("handoff_complete");
    // The sentence the settling message earned still leads the reminder it earned.
    expect(userText(turns[1] as Request)).toContain("written as text");
    expect(userText(turns[1] as Request)).toContain(
      `Your turn ended with ${STALL_WORK} still owed`
    );
    expect(userText(turns[1] as Request)).toContain("WAITING");
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
  "a worker the daemon says owes work and that keeps settling is reminded at the end of every turn, past the architect threshold, and of nothing once the daemon answers that the claim owes nothing",
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
        quietMs: QUIET_PAST_RECORD_MS,
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

test.skipIf(skip)(
  "a WAITING reply buys its quiet wait, and the reminder comes again when the wait is over",
  async () => {
    const run = setup();
    // Every request the stand-in served, with the moment it was served: the proposition is that
    // nothing reached the session until the wait was over, and only elapsed time shows it.
    const seen: { readonly path: string; readonly at: number }[] = [];
    // The quiet wait a WAITING line buys, and the bound every assertion below measures against.
    // The bound allows the timer a little early, since a host timer may fire slightly short.
    const waitMs = 1_500;
    const waitedAtLeastMs = waitMs - 100;
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "Pushed the change.\nWAITING: CI on the pull request." }],
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
        env: { LEGION_STALL_WAITING_MS: String(waitMs) },
        quietMs: QUIET_PAST_RECORD_MS,
        answer: (url) => {
          seen.push({ path: url.pathname, at: Date.now() });
          return undefined;
        },
      }
    );

    const turns = pane.turns();
    expect(turns).toHaveLength(3);
    const [waitingTurn = 0, reminderTurn = 0] = seen
      .filter((request) => request.path === "/anthropic/v1/messages")
      .map((request) => request.at);
    // The WAITING line bought the wait: no turn started while it ran, and the reminder's did
    // only once it was over.
    expect(reminderTurn - waitingTurn).toBeGreaterThanOrEqual(waitedAtLeastMs);
    // Nothing arrived from outside in between: in that gap the pane spoke only to the daemon,
    // for the reminder's own question of what the claim owes, the outcome of the turn that bought
    // the wait and the activity report its settle sends, and to the listener for its session
    // registration. The stand-in also serves as Dispatch, which holds the handoff record, so the
    // session reads its people's answers before every model request (pi-envoy's answers line),
    // the reminder's included. That read belongs to the reminder's turn, so it comes only once the
    // wait is over.
    const between = seen.filter((request) => request.at > waitingTurn && request.at < reminderTurn);
    const answersRead = /^\/api\/v1\/agents\/[^/]+\/answers$/;
    for (const request of between.filter(({ path }) => answersRead.test(path))) {
      expect(request.at - waitingTurn).toBeGreaterThanOrEqual(waitedAtLeastMs);
    }
    expect(
      between
        .map((request) => request.path)
        .filter(
          (path) =>
            path !== "/legion/v1/claims/owed" &&
            path !== "/legion/v1/claims/turn-outcome" &&
            path !== ACTIVITY_ROUTE &&
            path !== "/v1/interests/subscribe" &&
            !answersRead.test(path)
        )
    ).toEqual([]);
    // What the worker is told once its wait is over: the reminding repeats, rather than this
    // being the last it hears until something arrives.
    expect(userText(turns[1] as Request)).toContain(STALL_WORK);
    expect(userText(turns[1] as Request)).toContain("handoff_complete");
    expect(userText(turns[1] as Request)).toContain("repeats at the end of every turn");
    expect(userText(turns[1] as Request)).not.toContain(
      "nothing more will be sent until a new event arrives"
    );
    expect(userText(turns[1] as Request)).not.toContain("written as text");
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1, 0]);
    expect(entries.at(-1)).toMatchObject({ state: "active", count: 0 });
    expect(pane.completions()).toHaveLength(1);
  },
  120_000
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
        quietMs: QUIET_PAST_RECORD_MS,
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
        [{ type: "text", text: "WAITING: CI on the pull request." }],
      ],
      {
        env: {
          LEGION_STALL_REMINDER_MS: "500",
          LEGION_STALL_ARCHITECT_AFTER: "3",
          LEGION_STALL_WAITING_MS: "600000",
        },
        quietMs: 4_000,
      }
    );

    expect(pane.turns()).toHaveLength(6);
    // Two reminders, the tool call putting the count back to zero, then two more: never three
    // unanswered in a row, so the threshold is never reached.
    expect(stallCounts(await pane.phaseEntries())).toEqual([0, 1, 2, 0, 1, 2]);
    expect(pane.publishes()).toEqual([]);
  },
  120_000
);

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
          // The first question is the continuation reminder's own; by the timer's the daemon's
          // record has moved on.
          return Response.json(asked === 1 ? { owes: true, work: STALL_WORK } : OWES_NOTHING);
        },
      }
    );

    // Two turns: the prompt's and the continuation reminder's. The timer the second settle armed
    // was answered that nothing is owed and sent the reminder it carried to nobody.
    expect(pane.turns()).toHaveLength(2);
    expect(asked).toBe(2);
    expect(pane.publishes()).toEqual([]);
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries)).toEqual([0, 1]);
    expect(entries.at(-1)).toMatchObject({ state: "active", count: 1 });
  },
  120_000
);

// Where the daemon's answer does not arrive the worker is reminded of nothing (`UNREAD_CHECK` in
// packages/pi-shared/src/phase-reminder.ts): a stalled worker is sent its task again by the
// daemon's own silence sweep. The stand-in answers `claims/owed` as a proxy does for a daemon it
// cannot reach: a 503 whose body is not the daemon's (`daemonUnreachable` in
// packages/pi-shared/src/daemon-client.ts). The worker's WAITING reply arms a timer, whose read
// fails as well: the timer clears the wake it armed, and the session reports at once that no
// reminder is due, so no wake past its time holds the pod from its idle release.
test.skipIf(skip)(
  "with the daemon unreachable the worker is reminded of nothing, and its session reports no reminder wake",
  async () => {
    const run = setup();
    const pane = await stallPane(
      run,
      [
        [{ type: "text", text: "WAITING: the daemon to come back." }],
        [{ type: "text", text: "A turn that should never start." }],
      ],
      {
        env: { LEGION_STALL_WAITING_MS: "500" },
        quietMs: 4_000,
        answer: (url) =>
          url.pathname === "/legion/v1/claims/owed"
            ? new Response("upstream connect error", { status: 503 })
            : undefined,
      }
    );

    // One turn, the prompt's own: the timer the WAITING reply armed asked the daemon, read no
    // answer, and sent nothing.
    expect(pane.turns()).toHaveLength(1);
    expect(daemonRoutes(pane)).toContain("/legion/v1/claims/owed");
    expect(pane.publishes()).toEqual([]);
    expect(stallCounts(await pane.phaseEntries())).toEqual([0]);

    // The settle reported the wake the WAITING reply armed, and the timer, once its read failed,
    // reported that no reminder is due.
    const reports = pane.requests
      .filter((request) => request.path === ACTIVITY_ROUTE)
      .map((request) => request.body);
    expect(reports).toContainEqual(
      expect.objectContaining({
        activity: expect.objectContaining({ phase: { nextReminderAt: expect.any(String) } }),
      })
    );
    expect(reports.at(-1)).toMatchObject({ activity: { phase: { nextReminderAt: null } } });
  },
  120_000
);

// Both sides on a real Oh My Pi (dispatch://INFRA-2746/spec#b-r3, #b-r5, #b-r8): a role that owes
// nothing ends its turns with no reminder, and its architect is told nothing, in each shape in
// which INFRA's finished roles were reminded at every turn end, while the control above, a role
// whose phase is current and whose work is undone, is still reminded. In every row the stand-in
// daemon answers what the claim owes from its record (`owedOf`), which the row sets and moves as
// the daemon would, and it answers the issue's record late (`RECORD_ANSWER_MS`), so a reminder that
// read the record would show as a record read that the pane's own calls do not explain.

/** The limits the both-sides rows run on: the reminder after a reminder and the wait a WAITING
 * reply buys each last half a second, and the architect is told at the second reminder of a run,
 * the least the limit takes. So a reminder, a timer's reminder or an architect notice that a row
 * earned would come inside its quiet wait. */
const QUICK_LIMITS = {
  LEGION_STALL_REMINDER_MS: "500",
  LEGION_STALL_WAITING_MS: "500",
  LEGION_STALL_ARCHITECT_AFTER: "2",
};

/** The worker's completion: the `legion` tool's `handoff_complete`. */
const COMPLETE: Reply = [
  {
    type: "tool_use",
    name: "legion",
    input: { op: "handoff_complete", summary: "Stall proof done." },
  },
];

/** Scripted after a row's last expected turn, so a turn that should not happen shows up as a turn
 * rather than as the gateway's "no reply scripted" refusal. */
const NEVER: Reply = [{ type: "text", text: "A turn that should never start." }];

/** What the daemon tells a role's next process of the let-go that ended its last one while it was
 * in no turn (`RelaunchSentence`, packages/daemon/internal/supervise/relaunch.go): the prompt a
 * role relaunched with no task is given. */
function relaunched(cause: string): string {
  return (
    `Your previous process ended because ${cause}. You were not in a turn then. ` +
    "No result of that work will come to you. Your transcript does not say so. " +
    "Before you start any of that work again, check what it already did."
  );
}

/** The update round the daemon asks STALL-2's roles about: a pod round (`PodRound`,
 * packages/daemon/internal/supervise/machine.go). */
const UPDATE_ROUND = "pod:5b2e9d41-7c3a-4f0e-9a61-2d8c4b7e1f03";

/** The daemon's ask of that round, as it publishes it to each role's topic (`UpdateTask`,
 * packages/daemon/internal/supervise/table.go). */
const UPDATE_ASK = [
  "The daemon must replace the pod that runs this issue's roles. The reasons are: the worker image changed.",
  "",
  `The round is ${UPDATE_ROUND}. The round ends at 2026-11-19T20:30:00Z (UTC). When the round ends, the daemon deletes the pod and the disk. The disk holds your workspace. Only your own push keeps your work. Work that you do not push before the pod goes is lost.`,
  "",
  "If every role answers, the round can end before that moment. The round then ends when no role is in a turn. If a role does not answer, the round still ends at 2026-11-19T20:30:00Z. A turn in progress does not delay that moment.",
  "",
  "This message can arrive while you are in a turn. Do these four steps now:",
  "",
  "1. Push every commit that you want to keep to this issue's branch.",
  "2. If you need a scratch file as evidence, put it on Dispatch.",
  `3. Call the \`legion\` tool with op: "ready_for_update" and round: "${UPDATE_ROUND}".`,
  "4. End your turn.",
  "",
  'If your `legion` tool has no "ready_for_update" operation, do step 1 and step 2 now. Then end your turn. The round ends at 2026-11-19T20:30:00Z whatever you do.',
  "",
  "These things go with the pod when the round ends:",
  "",
  "- The turn that is in progress.",
  "- The background jobs of that turn.",
  "- Each commit and each file that you did not push.",
  "- The pod's test database.",
  "- The pod's caches.",
  "",
  "The daemon starts your process again on the new pod, in this same session. The daemon holds your next task until your process runs again. Do not start new work now.",
].join("\n");

/** The daemon's task for the implementer of STALL-2, released and woken by a message on its role
 * topic (`WakeTask` with `pods.WakeRoleMessage`, packages/daemon/internal/workflow/wake.go, which
 * names the role's claim): a start that names no phase, so it is no work of any phase. */
const ROLE_MESSAGE_WAKE =
  "Your issue's pod had been released while it was idle, and this wake brought it back. " +
  "Your session, your workspace and any work you had not pushed are as you left them. " +
  "The wake is a message somebody published to your role topic (legion-stall-stall-2-implementer) " +
  "while your pod was gone. Envoy keeps a role message for one hour: a message kept for your role " +
  "reaches this session as an Envoy card that shows kept_at, possibly before this task, unless its " +
  "hour ended before you came back, and Envoy tells its sender when it drops one. " +
  "A message sent with keep: false is not kept, and its sender sends it again.";

/**
 * Pushes one commit to `legion/STALL-2` at `origin` from a clone of its own, as another role of
 * the issue pushes from its own checkout: a child of the branch's tip, made as the pane's
 * identity. Answers the commit it pushed.
 */
async function pushAsAnotherRole(origin: string): Promise<string> {
  const clone = path.join(path.dirname(origin), "another-role");
  await runAsPane(
    ["git", "clone", "--quiet", "--branch", "legion/STALL-2", origin, clone],
    path.dirname(origin)
  );
  await writeFile(path.join(clone, "test_main.py"), "import main\n");
  const git = [
    "git",
    "-c",
    `user.name=${PANE_IDENTITY.JJ_USER}`,
    "-c",
    `user.email=${PANE_IDENTITY.JJ_EMAIL}`,
  ];
  await runAsPane([...git, "add", "test_main.py"], clone);
  await runAsPane([...git, "commit", "--quiet", "-m", "test: the stall"], clone);
  await runAsPane([...git, "push", "--quiet", "origin", "HEAD:legion/STALL-2"], clone);
  return runAsPane(["git", "rev-parse", "HEAD"], clone);
}

/** The `dispatch` command's own source, which a pane runs with bun: the plugin's `bin/dispatch`
 * runs the bundle a build makes, and a checkout holds none. */
const DISPATCH_CLI = path.resolve(import.meta.dir, "../../envoy-client/bin/dispatch.ts");

// INFRA-4567's implementer, whose second completion was refused: STALL-2 stands in testing, which
// the implementer does not work, so the daemon refuses its completion HANDOFF_NOT_CURRENT_PHASE
// and answers that it owes nothing. The turn after the refusal ends with a WAITING line, and so
// does the turn an Envoy message starts: each buys a wait of half a second, and each wait ends
// with the daemon asked again and nothing sent.
test.skipIf(skip)(
  "a role whose completion was refused HANDOFF_NOT_CURRENT_PHASE is reminded of nothing, and its WAITING lines after it change nothing",
  async () => {
    const pane = await stallPane(
      setup(),
      [
        COMPLETE,
        [
          {
            type: "text",
            text: "The daemon refused the completion HANDOFF_NOT_CURRENT_PHASE.\nWAITING: the daemon to send my next task.",
          },
        ],
        [{ type: "text", text: "WAITING: the daemon to send my next task." }],
        NEVER,
      ],
      {
        record: { phase: "testing" },
        complete: refuseNotCurrentPhase,
        env: QUICK_LIMITS,
        quietMs: QUIET_PAST_RECORD_MS,
        prompt: [
          "Implement STALL-2.",
          (scene) => scene.roleMessage("The tester has taken STALL-2 and runs its tests now."),
        ],
      }
    );

    // Three turns: the completion's, the reply to its refusal, and the Envoy message's.
    expect(pane.turns()).toHaveLength(3);
    expect(pane.completions()).toHaveLength(1);
    expect(pane.toolResults()).toContainEqual(
      expect.objectContaining({
        is_error: true,
        text: expect.stringContaining("HANDOFF_NOT_CURRENT_PHASE"),
      })
    );
    // Each WAITING line's wait ended with the daemon asked what the claim owes, and both times it
    // answered nothing.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
);

// INFRA-3082's tester and reviewer: a role's completion was taken, STALL-2 moved on to testing,
// and then the tester pushed to `legion/STALL-2` from its own checkout and told the implementer
// so on its role topic. The implementer reads the new head and ends its turn.
test.skipIf(skip)(
  "a role whose completion is recorded while another role pushes the head is reminded of nothing and its architect is told nothing",
  async () => {
    let origin = "";
    let pushed = "";
    const pane = await stallPane(
      setup(),
      [
        COMPLETE,
        [{ type: "text", text: "Completed STALL-2." }],
        [call("bash", { command: "jj git fetch" })],
        [{ type: "text", text: "The tester's commit is on legion/STALL-2; it is the tester's." }],
        NEVER,
      ],
      {
        env: QUICK_LIMITS,
        quietMs: QUIET_PAST_RECORD_MS,
        prompt: [
          "Implement STALL-2.",
          async (scene) => {
            origin = scene.origin;
            pushed = await pushAsAnotherRole(scene.origin);
            await scene.roleMessage("I pushed a test of the change to legion/STALL-2.");
          },
        ],
      }
    );

    // Four turns: the completion's, the reply to it, and the two of the Envoy message's run.
    expect(pane.turns()).toHaveLength(4);
    expect(pane.completions()).toHaveLength(1);
    // The head moved after the completion was taken: the branch at origin is the other role's
    // commit, and not the one the completion named.
    expect(pushed).not.toBe(pane.pushedTip);
    expect(
      await runAsPane(
        ["git", "--git-dir", origin, "rev-parse", "refs/heads/legion/STALL-2"],
        origin
      )
    ).toBe(pushed);
    // Both runs ended with the daemon asked what the claim owes, and both times it answered
    // nothing: the completion had moved STALL-2 on to testing.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
);

// INFRA-4337's tester: the daemon moves STALL-2 back to implementing while the tester's turn runs
// (the move a hold, an architect or a review makes), so the owed answer the tester's turn end reads
// is nothing, though it owed its testing phase when the turn began. Then the daemon's card on the
// tester's role topic starts one more turn.
test.skipIf(skip)(
  "a tester the daemon moves out of its phase while in a turn is reminded of nothing at its next turn end, with no handoff_complete call",
  async () => {
    const record: StallRecord = { phase: "testing" };
    const pane = await stallPane(
      setup(),
      [
        [call("bash", { command: "echo ran the suite" })],
        // The move lands while the turn runs, between its two tool calls.
        () => {
          record.phase = "implementing";
          return [call("bash", { command: "echo read the results" })];
        },
        [{ type: "text", text: "The suite passed; the test report comes next." }],
        [
          {
            type: "text",
            text: "STALL-2 is back in implementing: its testing waits for the change.",
          },
        ],
        NEVER,
      ],
      {
        role: "tester",
        record,
        env: QUICK_LIMITS,
        quietMs: 4_000,
        prompt: [
          "Test STALL-2.",
          (scene) =>
            scene.roleMessage(
              "STALL-2 went back to implementing: the architect asked for a change first."
            ),
        ],
      }
    );

    // Four turns: the three of the task's run, and the card's.
    expect(pane.turns()).toHaveLength(4);
    expect(pane.completions()).toEqual([]);
    // The task's run ended after the move, and so did the card's: the daemon was asked at each end,
    // and both times it answered nothing.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
);

// OSSV-732's reviewer: the reviewer reads the change, finds it wrong and sends STALL-2 back to
// implementing with its own `request_backward_move`, which the daemon takes. It made no
// completion call, and from the move on the daemon answers that it owes nothing.
test.skipIf(skip)(
  "a reviewer whose own request_backward_move moved the issue back is reminded of nothing",
  async () => {
    const record: StallRecord = { phase: "reviewing" };
    const pane = await stallPane(
      setup(),
      [
        [call("bash", { command: "cat main.py" })],
        [
          {
            type: "tool_use",
            name: "legion",
            input: {
              op: "request_backward_move",
              to: "implementing",
              reason: "main.py prints the wrong word.",
            },
          },
        ],
        [
          {
            type: "text",
            text: "Sent STALL-2 back to implementing: main.py prints the wrong word.",
          },
        ],
        NEVER,
      ],
      { role: "reviewer", record, env: QUICK_LIMITS, quietMs: 4_000, prompt: "Review STALL-2." }
    );

    expect(pane.turns()).toHaveLength(3);
    expect(pane.completions()).toEqual([]);
    expect(
      pane.requests
        .filter((request) => request.path === "/legion/v1/phase/backward")
        .map((request) => request.body.to)
    ).toEqual(["implementing"]);
    expect(record.phase).toBe("implementing");
    // The run's end asked the daemon once, after the move, and it answered nothing.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
);

// INFRA-1253's planner, and INFRA-4567's implementer on the other side of the same cause: a role
// whose completion was taken and moved its issue on is relaunched, and its new process ends turns
// in which the issue stands in a phase of another role. The first life completes planning, which
// moves STALL-2 on to implementing. The second is a fresh Oh My Pi over the same transcript
// (`pane.resume`), its claim registered again, which holds the planner's claim while STALL-2
// stands in implementing: the stand-in answers that it owes nothing from that fact alone.
test.skipIf(skip)(
  "a finished role relaunched over its transcript is reminded of nothing",
  async () => {
    const record: StallRecord = { phase: "planning" };
    const pane = await stallPane(
      setup(),
      [COMPLETE, [{ type: "text", text: "Planned STALL-2." }]],
      {
        role: "planner",
        record,
        env: QUICK_LIMITS,
        quietMs: QUIET_PAST_RECORD_MS,
        prompt: "Plan STALL-2.",
      }
    );
    expect(record.phase).toBe("implementing");

    await pane.resume(
      [
        [call("bash", { command: "jj log -r @- --no-graph -T description" })],
        [{ type: "text", text: "My plan stands, and nothing of planning is open." }],
        NEVER,
      ],
      [relaunched("role planner reports generation 5 exited (code 1)")]
    );

    // Two processes, each of which registered its claim: the first life and the relaunch.
    expect(
      pane.requests.filter((request) => request.path === "/legion/v1/claims/register")
    ).toHaveLength(2);
    // Four turns: two in each life.
    expect(pane.turns()).toHaveLength(4);
    expect(pane.completions()).toHaveLength(1);
    // Each life's run ended with the daemon asked what the claim owes, and the answer was nothing
    // in both: the first after its completion moved STALL-2 on, the second in a process that had
    // seen no completion at all.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  360_000
);

// A role whose completion was taken answers the daemon's update round in testing, a phase it does
// not work: it calls `ready_for_update` and ends its turn. The round replaces its process, and the
// replacement, a fresh Oh My Pi over the same transcript, is told of the relaunch and ends its
// turn with no reminder.
test.skipIf(skip)(
  "a role that answered an update round in a phase that is not current runs in the replacement process with no reminder, and its architect gets no notice",
  async () => {
    const pane = await stallPane(
      setup(),
      [
        COMPLETE,
        [{ type: "text", text: "Completed STALL-2." }],
        [
          {
            type: "tool_use",
            name: "legion",
            input: { op: "ready_for_update", round: UPDATE_ROUND },
          },
        ],
        [{ type: "text", text: "Everything of mine is pushed, and the daemon has my answer." }],
      ],
      {
        env: QUICK_LIMITS,
        quietMs: QUIET_PAST_RECORD_MS,
        prompt: ["Implement STALL-2.", (scene) => scene.roleMessage(UPDATE_ASK)],
      }
    );
    expect(
      pane.requests
        .filter((request) => request.path === "/legion/v1/claims/ready-for-update")
        .map((request) => request.body.round)
    ).toEqual([UPDATE_ROUND]);

    await pane.resume(
      [[{ type: "text", text: "STALL-2 stands in testing; nothing of it is mine to do." }], NEVER],
      [relaunched(`the daemon replaced the pod in update round ${UPDATE_ROUND}`)]
    );

    expect(
      pane.requests.filter((request) => request.path === "/legion/v1/claims/register")
    ).toHaveLength(2);
    // Five turns: the completion's run, the update ask's run, and the replacement's one turn.
    expect(pane.turns()).toHaveLength(5);
    expect(pane.completions()).toHaveLength(1);
    // The answer to the round ended its turn in the wait the daemon ends, which asks nothing. The
    // completion's run and the replacement's turn each ended with the daemon asked, and both
    // times it answered nothing.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  360_000
);

// INFRA-4567's implementer: its completion is taken and moves STALL-2 on to testing, and just
// after it a message on its role topic wakes it, as a released role's pod is woken: the kept
// message reaches the session as an Envoy card, and then the daemon's wake task arrives as a
// user message, a task that names no phase.
test.skipIf(skip)(
  "a finished role woken by a role-topic message after its accepted completion, holding a task with no phase, is reminded of nothing and its architect is told nothing",
  async () => {
    const pane = await stallPane(
      setup(),
      [
        COMPLETE,
        [{ type: "text", text: "Completed STALL-2." }],
        [{ type: "text", text: "Noted: the tester has STALL-2." }],
        [{ type: "text", text: "Nothing of STALL-2 is mine to do now." }],
        NEVER,
      ],
      {
        env: QUICK_LIMITS,
        quietMs: QUIET_PAST_RECORD_MS,
        prompt: [
          "Implement STALL-2.",
          (scene) => scene.roleMessage("Thank you for the change: the tester has STALL-2 now."),
          ROLE_MESSAGE_WAKE,
        ],
      }
    );

    // Four turns: the completion's run, the card's and the wake task's.
    expect(pane.turns()).toHaveLength(4);
    expect(pane.completions()).toHaveLength(1);
    // Each of the three runs ended with the daemon asked, and each time it answered nothing.
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
);

// A role whose completion was taken writes a handoff record of its phase after it, with the
// `dispatch handoff-write` command, in the same run: the record is newer than the completion, and
// the daemon answers that the claim owes nothing all the same.
test.skipIf(skip)(
  "a finished role that writes a handoff record after its accepted completion is reminded of nothing",
  async () => {
    const body = JSON.stringify({
      schemaVersion: 1,
      phase: "implement",
      issue: "STALL-2",
      note: "the evidence the completion left out",
    });
    const pane = await stallPane(
      setup(),
      [
        COMPLETE,
        [
          call("bash", {
            command: `printf '%s' '${body}' | ${process.execPath} ${DISPATCH_CLI} handoff-write --issue STALL-2 --phase implement --body-file -`,
          }),
        ],
        [{ type: "text", text: "Recorded version 2 of the implement handoff." }],
        NEVER,
      ],
      { env: QUICK_LIMITS, quietMs: QUIET_PAST_RECORD_MS }
    );

    expect(pane.turns()).toHaveLength(3);
    expect(pane.completions()).toHaveLength(1);
    // The write reached the stand-in Dispatch after the completion, as version 2 of the phase.
    const routes = pane.requests.map((request) => request.path);
    expect(routes.indexOf("/api/v1/issues/STALL-2/handoffs/implement")).toBeGreaterThan(
      routes.indexOf("/legion/v1/handoff/complete")
    );
    expect(pane.toolResults().map((result) => result.text)).toContainEqual(
      expect.stringContaining("Recorded the implement handoff of STALL-2, version 2")
    );
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
);

// INFRA-4011's planner: STALL-2 stands in implementing, long past its planning. The planner is
// relaunched with no task, and then its architect asks it on its role topic for a piece of work:
// it runs real tool calls and ends the turn.
test.skipIf(skip)(
  "a finished planner that ends a turn of work its architect asked for is reminded of nothing",
  async () => {
    const pane = await stallPane(
      setup(),
      [
        [{ type: "text", text: "My plan stands, and nothing of planning is open." }],
        [
          call("bash", {
            command: "printf '%s\\n' 'The parser change' 'The CI time' > plan-risks.md",
          }),
        ],
        [call("bash", { command: "cat plan-risks.md" })],
        [{ type: "text", text: "Wrote the plan's two open risks to plan-risks.md." }],
        NEVER,
      ],
      {
        role: "planner",
        record: { phase: "implementing" },
        env: QUICK_LIMITS,
        quietMs: 4_000,
        prompt: [
          relaunched("role planner reports generation 5 exited (code 1)"),
          (scene) =>
            scene.roleMessage(
              "From the architect: write the plan's two open risks to plan-risks.md for the implementer."
            ),
        ],
      }
    );

    // Four turns: the relaunch's, and the three of the architect's request.
    expect(pane.turns()).toHaveLength(4);
    expect(pane.completions()).toEqual([]);
    expect(pane.toolResults().map((result) => result.text)).toContainEqual(
      expect.stringContaining("The CI time")
    );
    expect(pane.owedAnswers()).toEqual([OWES_NOTHING, OWES_NOTHING]);
    await expectRemindedOfNothing(pane);
  },
  240_000
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
        [{ type: "text", text: "WAITING: nothing more." }],
      ],
      // Longer than the job, so the pane settles on the woken turn rather than before it.
      { quietMs: 12_000 }
    );

    const turns = pane.turns();
    // Three turns: the assignment's, the one its tool result continued, and the one the finished
    // job woke. Nothing prompted the third.
    expect(turns).toHaveLength(3);
    expect(userText(turns[2] as Request)).toContain("background-done");
    // No reminder reached the worker in between: the turn that ended with the job running earned
    // none, because `session_stop` never fired while the job was pending.
    for (const turn of turns) {
      expect(userText(turn as Request)).not.toContain(`Your turn ended with ${STALL_WORK}`);
    }
    const entries = await pane.phaseEntries();
    expect(stallCounts(entries).filter((count) => count !== 0)).toEqual([]);
    // The WAITING line of the woken turn is what quiets the rhythm underneath.
    expect(entries.at(-1)).toMatchObject({ state: "waiting", count: 0 });
    expect(pane.completions()).toHaveLength(0);
  },
  180_000
);
