// The pane the stall watch's live proof drives: one Legion phase-worker session on the real Oh My
// Pi, with the stand-in that is its model gateway, its daemon and its Envoy listener, and the
// helpers a case reads it with. The cases are in `extensions/legion-stall-watchdog-omp.test.ts`,
// and what was measured to build them is in that file's own header; nothing here is a case, so
// this module runs nothing on import but the constants below.
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import {
  type Block,
  type Cleanup,
  call,
  messageStream,
  ompRoot,
  type Request,
  refusingNats,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { STALL_RESTART_MESSAGE } from "../src/stall-abort";
import {
  STALL_WATCH_ENTRY,
  type StallEvidenceProcess,
  type StallEvidenceRecord,
  type StallWatchRecord,
} from "../src/stall-evidence";
import { stallExitsFile } from "../src/stall-exits";

const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
export const skip = omp === undefined && !onActions;

/** The binary the case runs against; throws on GitHub Actions without it. */
function setup(): string {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  return omp;
}

const PROJECT = "watch";
export const ROLE = "implementer";
const TREE = "WATCH-1";
export const ISSUE = "WATCH-2";

/** The daemon's assignment, sent as the RPC prompt. Every turn of the pane's own conversation
 * carries it, which is what tells a turn from a side request. */
const ASSIGNMENT = `Implement ${ISSUE}. WATCH-ROLE-ASSIGNMENT.`;

/** Who a phase worker's stuck call is told about: the tree's architect, on the role topic
 * `roleToken` builds (`packages/contracts/src/legion-roles.ts`). */
export const ARCHITECT_TOPIC = `notifications.role.legion-${PROJECT}-${TREE.toLowerCase()}-architect`;

/** The bound for a call no deadline judges (`LEGION_STALL_CALL_BOUND_MS`): six seconds, so a
 * wedged cell is overdue one sweep after it starts rather than four hours later. */
export const BOUND_MS = 6_000;

/** The gap between the ladder's steps (`LEGION_STALL_STEP_MS`): four seconds, so each step lands
 * on the sweep after the one before it. */
export const STEP_MS = 4_000;

/** The deadline the backgrounded-call case's shell command declares, which is the rule that
 * would judge that call were its record still open, and also when the host kills the job it
 * backgrounded: twenty seconds, long enough that the steer's background always lands inside it
 * and short enough that the rule comes due well within the case. */
export const SHELL_DEADLINE_MS = 20_000;

/** How long a case waits for something the pane owes it before failing: the pane's boot and the
 * three steps of the ladder, each a 5-second sweep apart. */
export const DEADLINE_MS = 120_000;

/** How long a case watches for something that must not happen: the bound, two of the plugin's
 * 5-second sweeps, and a margin. */
export const QUIET_MS = BOUND_MS + 12_000;

export const cleanup: Cleanup = [];

/** A Messages request's whole conversation, as one string to find texts in. */
export function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** A Messages request's last message, which is what started the turn it answers. */
export function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

/**
 * Whether a process is still running, asked of `/proc` rather than read from a log line. A
 * process the kernel holds as a zombie has ended and is waiting for its parent to reap it, which
 * a signal of 0 still answers for: a cell that was killed while looping never reaps the child the
 * first step ended, so the state letter is what says the child is gone.
 */
export async function runs(pid: number): Promise<boolean> {
  if (!Number.isInteger(pid) || pid <= 0) throw new Error(`${pid} is no process to ask about`);
  let stat: string;
  try {
    stat = await readFile(`/proc/${pid}/stat`, "utf8");
  } catch {
    return false;
  }
  // `/proc/<pid>/stat` holds the executable's name in parentheses, which may itself hold spaces
  // and parentheses, and the state letter is the field after the last of them.
  return (
    stat
      .slice(stat.lastIndexOf(")") + 1)
      .trim()
      .charAt(0) !== "Z"
  );
}

/** A directory of this case's own: the claim's state directory, or where a cell writes the pid of
 * the child it started. */
export async function scratchDir(name: string): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), `legion-watch-${name}-`));
  cleanup.push(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

/** The counter file one claim counts its own stall exits in, under `stateDir`. */
export function exitsFile(stateDir: string): string {
  return stallExitsFile(stateDir, { kind: "phase-worker", role: ROLE, tree: TREE, issue: ISSUE });
}

/**
 * The cell a case wedges its turn with. It waits three seconds before it starts its child, so the
 * host's `tool_execution_start` has reached the plugin and the child is a process rule 2 can see
 * (measurement 2); with `goOn` it keeps running after that child has died, which is what carries
 * the ladder to its second step, and without it the cell is waiting on the child and returns the
 * moment the first step ends it.
 */
function wedgeCell(pidFile: string, options: { readonly goOn: boolean }) {
  return [
    "import pathlib, subprocess, time",
    "time.sleep(3)",
    'p = subprocess.Popen(["/bin/sleep", "3000"])',
    `pathlib.Path(${JSON.stringify(pidFile)}).write_text(str(p.pid))`,
    "print('the child is', p.pid)",
    ...(options.goOn
      ? ["while True:", "    time.sleep(0.5)"]
      : ["p.wait()", "print('the child is gone')"]),
  ].join("\n");
}

/** The cell that warms this session's `eval` kernel: a call of its own that starts the kernel and
 * returns, so the kernel is older than every call made after it (measurement 2). */
export const WARM_CELL = call("eval", {
  language: "py",
  code: "print('the kernel is ready')",
  timeout: 600,
});

/**
 * The turns of a case whose kernel must outlive the call that wedges: the warming cell, then the
 * wedging cell, then `then` for every turn after. The warming cell is what keeps the first step
 * about the cell's own child. A kernel the wedged call itself started is a descendant of the agent
 * that started inside that call, so rule 2 of `src/stall-recovery.ts` is right to take it — and on
 * a host slower than this one to start `python3`, takes it, ending the cell with it (measurement
 * 2). A case about what becomes of the cell must therefore be a case whose kernel is older.
 */
export function warmedWedge(
  pidFile: string,
  options: { readonly goOn: boolean },
  then: string
): (index: number) => readonly Block[] {
  return (index) => {
    if (index === 1) return [WARM_CELL];
    if (index === 2) {
      return [call("eval", { language: "py", code: wedgeCell(pidFile, options), timeout: 600 })];
    }
    return [{ type: "text", text: then }];
  };
}

/** The daemon's state with the issue past this pane's phase: the reviewer works it now, so the
 * phase-stall follow-up (`src/phase-stall.ts`) has no reminder for this role and every turn a case
 * sees is one it asked for. */
const MOVED_ON = {
  daemon: {
    project: PROJECT.toUpperCase(),
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-10-30T14:03:27Z",
    startedAt: "2026-10-31T09:15:02Z",
  },
  admission: { cap: 1, active: [TREE], waiting: [], blocked: [] },
  issues: {
    [ISSUE]: {
      key: ISSUE,
      generation: 1,
      phase: "reviewing",
      status: "in_progress",
      repo: "acme/widgets",
      defaultBranch: "main",
      workers: {},
    },
  },
  pendingStatusWrites: [],
  heldOutboxRows: [],
  strandedClaims: [],
  capabilities: [],
  repositories: {
    owners: ["acme"],
    mapped: [{ repo: "acme/widgets", default: true }],
    refused: [],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { max_fix_attempts: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
};

/**
 * The one-handler extension the last step's cases load beside the plugin. `abort` is not an own
 * property of the session context: it lives on the object every context is made from, so dropping
 * it there leaves every extension of that run — this plugin included — with the host
 * `src/stall-abort.ts` documents, the one whose `ctx.abort()` is not a function. The plugin's own
 * second step still runs; it fails for the host's reason and the ladder goes on to the step that
 * ends the process.
 */
const NO_ABORT_EXTENSION = `export default function noAbort(pi) {
  pi.on("session_start", (_event, context) => {
    const host = Object.getPrototypeOf(context);
    if (host !== null && Object.getOwnPropertyDescriptor(host, "abort") !== undefined) {
      Object.defineProperty(host, "abort", { value: undefined, configurable: true });
    }
  });
}
`;

/** What the steering delivery below carries, and what the pane's answer to it must contain, so a
 * case can find the turn the steer started among the pane's own. */
export const STEER_TEXT = "WATCH-STEER: your architect asks how the work is going.";

/** The file the extension below writes one line into for each run end of its session, under the
 * pane's own state directory. */
const AGENT_ENDS_FILE = "agent-ends.jsonl";

/** What an extension of the pane is handed at one run end: the two fields a continuing end sets
 * and the one the host answers for a steered one, as the entry's `agent_end` handler could read
 * them. */
export interface AgentEndSeen {
  readonly willContinue: boolean | null;
  readonly awaitingAsyncWork: boolean | null;
  readonly pendingMessages: boolean | null;
}

/**
 * The one-handler extension a steering case loads beside the plugin. It sends one steering
 * delivery as the call whose arguments carry `marker` begins — the case's wedging call — with the
 * options an Envoy role delivery is sent with
 * (`pi.sendMessage(…, { deliverAs: "steer", triggerTurn: false })` in
 * `packages/pi-envoy/extensions/envoy.ts`), so what lands on the running turn is what a message
 * from this worker's architect lands as. Nothing of the plugin is replaced: this extension only
 * does what the listener's delivery would have done had this case run a NATS of its own.
 *
 * It also writes down what each run end hands an extension, which is how a case shows that the
 * entry's close at either end is a choice: the host does tell them apart, and the watch reads
 * none of it.
 */
const steerExtension = (
  text: string,
  marker: string,
  endsFile: string
): string => `import { appendFileSync } from "node:fs";
export default function steer(pi) {
  let sent = false;
  pi.on("tool_execution_start", (event) => {
    if (sent || !JSON.stringify(event.args ?? {}).includes(${JSON.stringify(marker)})) return;
    sent = true;
    pi.sendMessage(${JSON.stringify(text)}, { deliverAs: "steer", triggerTurn: false });
  });
  pi.on("agent_end", (event, context) => {
    const seen = {
      willContinue: event.willContinue ?? null,
      awaitingAsyncWork: event.awaitingAsyncWork ?? null,
      pendingMessages:
        typeof context?.hasPendingMessages === "function" ? context.hasPendingMessages() : null,
    };
    appendFileSync(${JSON.stringify(endsFile)}, JSON.stringify(seen) + "\\n");
  });
}
`;

/** How one pane differs from the others. */
export interface PaneOptions {
  /** The scratch root's prefix. */
  readonly name: string;
  /** `LEGION_STATE_DIR`: where this claim's stall exit count lives, shared by its launches. */
  readonly stateDir: string;
  /** Answers turn `index` (1 for the first) of the pane's own conversation. */
  readonly reply: (index: number, request: Request) => readonly Block[];
  /** `LEGION_STALL_CALL_BOUND_MS` for this pane, `BOUND_MS` by default. A case whose
   * proposition is about the deadline rule raises it, so the bound judges nothing of its own
   * while it waits. */
  readonly boundMs?: number;
  /** Loads the extension above, so the host has no `abort()` and the ladder reaches its last
   * step. A pane that loads it may end its own process, which `until` then expects. */
  readonly denyAbort?: boolean;
  /** Loads the steering extension above: `text` is delivered as the call whose arguments carry
   * `when` begins. */
  readonly steer?: { readonly text: string; readonly when: string };
}

/** One running pane, and what a case may ask of it. */
export interface StallPane {
  /** Every request the stand-in served: the gateway's, the daemon's and the listener's. */
  readonly requests: Request[];
  /** The turns of the pane's own conversation, in order. */
  readonly turns: () => Request[];
  /** Every `POST /v1/messages/publish` the pane made: the stuck call's notice, and the notice
   * that the ladder stopped short of a step. */
  readonly publishes: () => Record<string, unknown>[];
  /** This launch's stall-watch records, in the order its transcript holds them. */
  readonly records: () => Promise<StallWatchRecord[]>;
  /** The text of each message the worker sent its own session to start the work again. */
  readonly restarts: () => Promise<string[]>;
  /** Every RPC frame omp printed, in order: what the host says it did, beside what the plugin
   * recorded that it did. */
  readonly frames: () => Record<string, unknown>[];
  /** What each run end of this session handed an extension, in order; empty for a pane that
   * loads no steering extension (`steerExtension`). */
  readonly agentEnds: () => Promise<AgentEndSeen[]>;
  /** The status omp exited with, once it has. */
  readonly exited: Promise<number>;
  /** Whether omp is still running. */
  readonly running: () => boolean;
  /** Polls `probe` until it answers, failing on `what` at the deadline. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
}

/**
 * Starts one Legion phase-worker pane on the real Oh My Pi: pi-envoy's extension and this
 * package's, the role's `LEGION_*` environment against a stand-in that is the model gateway, the
 * daemon's claim, grant, state and activity routes and the Envoy listener, and the three stall
 * numbers set low. Nothing here takes a NATS delivery, so the Envoy extension is given an address
 * that refuses every connection (`refusingNats`). A side request — the run-end self-check, a
 * routing hint — is answered at once with one word, since none of them is a turn; a turn of the
 * pane's own conversation is answered from `reply`.
 */
export async function startPane(options: PaneOptions): Promise<StallPane> {
  const binary = setup();
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { root, home, workspace, sessions } = await ompRoot(
    binary,
    `legion-${options.name}-`,
    cleanup
  );

  const turns: Request[] = [];
  let sides = 0;
  let grants = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      const own =
        conversation(request).includes("WATCH-ROLE-ASSIGNMENT") &&
        !lastMessage(request).includes("<btw>");
      if (!own) {
        sides += 1;
        return new Response(
          messageStream([{ type: "text", text: "PROCEEDING" }], `side_${sides}`),
          {
            headers: { "content-type": "text/event-stream" },
          }
        );
      }
      turns.push(request);
      return new Response(
        messageStream(options.reply(turns.length, request), `msg_${turns.length}`),
        { headers: { "content-type": "text/event-stream" } }
      );
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: `legion-${PROJECT}-${ISSUE.toLowerCase()}-${ROLE}`,
        tree: TREE,
        issue: ISSUE,
        role: ROLE,
        generation: 1,
        secret: `${PROJECT}-secret`,
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/claims/activity") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/state") return Response.json(MOVED_ON);
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `${PROJECT}-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    if (url.pathname === "/v1/messages/publish") {
      // The listener's answer to a publish it took: an envelope, which the client parses before it
      // answers the worker (`EnvelopeSchema`, packages/contracts/src/envelope.ts).
      const key = String(body.idempotency_key ?? "");
      return Response.json({
        event_id: `evt-${key}`,
        source: "agent",
        source_event_id: key,
        topic: String(body.topic ?? ""),
        dedupe_key: `publish.${key}`,
        issued_at: 1,
        payload_summary: "a worker has a tool call that has not returned",
        trace_id: `trace-${key}`,
      });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: `${PROJECT}-machine`,
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);

  const extensions = [
    path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
    path.resolve(import.meta.dir, "../extensions/legion.ts"),
  ];
  if (options.denyAbort === true) {
    const file = path.join(root, "no-abort.ts");
    await writeFile(file, NO_ABORT_EXTENSION);
    extensions.push(file);
  }
  const endsFile = path.join(options.stateDir, AGENT_ENDS_FILE);
  if (options.steer !== undefined) {
    const file = path.join(root, "steer.ts");
    await writeFile(file, steerExtension(options.steer.text, options.steer.when, endsFile));
    extensions.push(file);
  }
  // The `eval` kernel is written under this pane's own TMPDIR, as it is under a pod's, rather than
  // into the machine's shared /tmp.
  const temporary = path.join(root, "tmp");
  await mkdir(temporary, { recursive: true });

  const frames: Record<string, unknown>[] = [];
  const rpc = spawnRpc(
    binary,
    {
      extensions,
      home,
      workspace,
      sessions,
      env: {
        TMPDIR: temporary,
        ENVOY_URL: base,
        ENVOY_NATS_URL: refusingNats(cleanup),
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: ROLE,
        LEGION_TREE: TREE,
        LEGION_ISSUE: ISSUE,
        LEGION_GENERATION: "1",
        LEGION_PROJECT: PROJECT,
        LEGION_BOOT_TOKEN: `${PROJECT}-boot`,
        LEGION_STATE_DIR: options.stateDir,
        LEGION_WORKSPACE: workspace,
        LEGION_STALL_CALL_GRACE_MS: "1000",
        LEGION_STALL_CALL_BOUND_MS: String(options.boundMs ?? BOUND_MS),
        LEGION_STALL_STEP_MS: String(STEP_MS),
      },
      onFrame: (frame) => frames.push(frame as Record<string, unknown>),
    },
    cleanup
  );
  // The stream closing is how a case learns the pane ended itself; the rejection is handled here,
  // so a case reads `exited` and `running` rather than racing a promise.
  rpc.closed.catch(() => undefined);
  let running = true;
  void rpc.exited.then(() => {
    running = false;
  });
  rpc.send({ type: "prompt", message: ASSIGNMENT });

  /** This launch's transcript entries of `customType`. The file is appended to as the session
   * runs, so a case reads it while the pane is still running — and after it has ended itself. */
  const entries = async (customType: string): Promise<Record<string, unknown>[]> => {
    const files = (await readdir(sessions, { recursive: true })).filter((file) =>
      file.endsWith(".jsonl")
    );
    if (files.length !== 1) return [];
    return (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
      .split("\n")
      .filter(Boolean)
      .map((line): unknown => JSON.parse(line))
      .flatMap((entry) =>
        typeof entry === "object" &&
        entry !== null &&
        "customType" in entry &&
        entry.customType === customType
          ? [entry as Record<string, unknown>]
          : []
      );
  };

  const pane: StallPane = {
    requests,
    turns: () => turns,
    publishes: () =>
      requests
        .filter((request) => request.path === "/v1/messages/publish")
        .map((request) => request.body),
    records: async () =>
      (await entries(STALL_WATCH_ENTRY)).map((entry) => entry.data as StallWatchRecord),
    restarts: async () =>
      (await entries(STALL_RESTART_MESSAGE)).map((entry) => String(entry.content ?? "")),
    frames: () => frames,
    agentEnds: async () => {
      let written: string;
      try {
        written = await readFile(endsFile, "utf8");
      } catch {
        return [];
      }
      return written
        .split("\n")
        .filter(Boolean)
        .map((line): AgentEndSeen => JSON.parse(line) as AgentEndSeen);
    },
    exited: rpc.exited,
    running: () => running,
    async until(what, probe) {
      const deadline = Date.now() + DEADLINE_MS;
      for (;;) {
        const value = await probe();
        if (value !== undefined) return value;
        if (options.denyAbort !== true && !running) {
          throw new Error(`omp exited while this case waited for ${what}`);
        }
        if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
        // A poll, because what is awaited is a line in a transcript the pane writes and a request
        // it makes, neither of which this process can be woken by.
        await Bun.sleep(100);
      }
    },
  };
  await pane.until("the pane's registration with the Envoy listener", () =>
    requests.find((request) => request.path === "/v1/interests/subscribe")
  );
  return pane;
}

/** The pid the pane's cell wrote to `pidFile`, once it has; the case ends that process at
 * cleanup, so no `sleep` of a case outlives its run. */
export async function childOf(pane: StallPane, pidFile: string): Promise<number> {
  const pid = await pane.until("the pid of the child the cell started", async () => {
    try {
      const read = Number(await readFile(pidFile, "utf8"));
      return Number.isInteger(read) && read > 0 ? read : undefined;
    } catch {
      return undefined;
    }
  });
  cleanup.push(async () => {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // The step under test ended it, which is what most of these cases are about.
    }
  });
  return pid;
}

/** The records of one kind this launch wrote. */
export function ofKind<K extends StallWatchRecord["kind"]>(
  records: readonly StallWatchRecord[],
  kind: K
): Extract<StallWatchRecord, { kind: K }>[] {
  return records.filter(
    (record): record is Extract<StallWatchRecord, { kind: K }> => record.kind === kind
  );
}

/** The step of `kind` this launch recorded, if it has recorded one. */
export function stepOf(
  records: readonly StallWatchRecord[],
  step: "end-processes" | "abort-turn" | "exit-process"
) {
  return ofKind(records, "step").find((record) => record.step === step);
}

/** This session's `eval` kernel, as the evidence found it in the agent's tree: the one process
 * whose command is the Python runner (`STALL_TOOL_RUNNERS`, `src/stall-recovery.ts`). */
export function kernelOf(evidence: StallEvidenceRecord): StallEvidenceProcess {
  if (!evidence.tree.read) throw new Error(`the tree was not read: ${evidence.tree.reason}`);
  const kernel = evidence.tree.processes.find((process) =>
    process.command.includes("omp-python-runner-")
  );
  if (kernel === undefined) throw new Error("no `eval` kernel was in the agent's tree");
  return kernel;
}
