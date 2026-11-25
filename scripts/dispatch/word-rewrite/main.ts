import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { type ParseArgsOptionsConfig, parseArgs } from "node:util";
import {
  type Category,
  ISSUE_KEY,
  readCommandLine,
  runScript,
  UsageError,
} from "../production-check-runtime.ts";
import { Changes } from "./changes.ts";
import {
  type Ancestry,
  COMMIT,
  type DaemonStates,
  daemonAtLeast,
  describeReading,
  dispatchBuild,
  gitAncestry,
  liveDaemonStates,
  readDaemons,
} from "./daemons.ts";
import {
  type DispatchReads,
  type DispatchWrites,
  liveDispatch,
  ownerName,
  Project,
  ReadError,
  Whoami,
} from "./dispatch-api.ts";
import { DEFAULT_MODEL, gatewayModel, type Model } from "./gateway.ts";
import {
  type Context,
  type Examined,
  examine,
  type Item,
  inPool,
  projectItems,
  READ_CONCURRENCY,
  readItems,
  readTree,
  type Wanted,
} from "./inventory.ts";
import {
  builtInMap,
  type MapEntry,
  MapFileError,
  parseExtraMap,
  readContract,
} from "./name-map.ts";
import { Pace, type PacedWrite, PaceLog, runTimeMs } from "./pace.ts";
import { Tally } from "./report.ts";
import {
  appendTo,
  type Clock,
  loggingFetch,
  RequestRate,
  requestLog,
  SYSTEM_CLOCK,
} from "./request-log.ts";
import { runSettle, type SettleOptions } from "./settle.ts";
import {
  emptyState,
  eventsAfter,
  gateReached,
  type HeldWrite,
  keepRefused,
  keepWaiting,
  type LoggedEvent,
  newestEventId,
  PROJECT_KEY,
  parseState,
  parseStateDocument,
  type RunState,
  readLog,
  readStateDocument,
  STATE_BOUND_BYTES,
  type StateDocument,
  StateTooLarge,
  StateWriteError,
  serializeState,
  stateBytes,
  VERDICT_BYTES,
  writeStateDocument,
} from "./state.ts";
import {
  heldWrite,
  type JudgeCalls,
  type WriteContext,
  writeItem,
  writerRefusal,
} from "./write.ts";

/**
 * The Dispatch rewrite tool of INFRA-2017 (`scripts/dispatch/README.md`, "Word rewrite"). Report
 * mode reads every project's open issues and project documents, lists each occurrence of the word
 * with the place that leaves it, and writes nothing but its local `--state` copy. Write mode asks
 * the model about every occurrence no place leaves and writes each rewrite through Dispatch's own
 * edit routes, keeping its state in a project document between runs. The settle command is write
 * mode with the tester's answers in place of the model's, for the occurrences whose two answers
 * failed (`settle.ts`).
 */

export const USAGE = `bun scripts/dispatch/word-rewrite/main.ts report|write --tree <KEY> --merge-commit <sha> [flags]
bun scripts/dispatch/word-rewrite/main.ts settle --state-document <ref> --answers <file> --summary <text> --merge-commit <sha> [flags]

report and write:
  --tree <KEY>                 the root whose issues name the word as the subject of the change
  --merge-commit <sha>         the merge the Dispatch API names wait for (before the merge: the head)
  --project <KEY>              a project to read; repeat; every project when none is named
  --include-closed             read closed issues too, and list them
  --state-document <ref>       dispatch://<PROJECT>/artifact/<slug>: the state kept between runs
  --state <file>               a local copy of the state, written at the end of every run
  --extra-map <file>           more map entries, one JSON object per line
  --request-log <file>         append one "METHOD path status" line per HTTP request
  --daemon-state <KEY>=<url>   a project's daemon state address, where Dispatch names none; repeat
  --no-daemon <KEY>            a project that has no Legion daemon; repeat
  --stop-at <time>             an RFC 3339 time at which the run stops: it starts no new read,
                               judgement or write, saves its state and exits 0
  --stop-after <duration>      the same, a duration after the run starts, such as 90m or 1h30m
  --whole                      read every project again from the start, with the state's verdicts,
                               rather than only the evented, held-back and waiting items; report
                               mode then writes no state, so it takes no --state
write mode only:
  --summary <text>             the summary of each document version the tool writes (required)
  --model <id>                 the model the judge asks (default ${DEFAULT_MODEL})
  --concurrency <n>            model calls at once (default 4)
  --max-writes <n>             stop writing after n writes; the rest wait for the next run
  --pause-before-write <file>  wait before each write until the file exists
settle (write mode with the tester's answers in place of the model's):
  --state-document <ref>       the state that keeps the occurrences as judge-refused (required)
  --answers <file>             one JSON object: under the key of a judge-refused occurrence, as a
                               report lists it, the answer for its paragraph in the model's shape:
                               {"rewrite": "<paragraph>"}, that with "left": {"<at>": "<place>"},
                               or {"allowed": "<place>"}; a place is quote, word-as-word,
                               other-team-technical, third-party-name or record
  --summary <text>             the summary of each document version it writes (required)
  --merge-commit <sha>         as for report and write
  --daemon-state, --no-daemon, --extra-map, --request-log: as for report and write
  --help                       print this text`;

export interface Options {
  readonly mode: "report" | "write";
  readonly tree: string;
  readonly mergeCommit: string;
  readonly projects: readonly string[];
  readonly includeClosed: boolean;
  readonly stateDocument: StateDocument | undefined;
  readonly stateFile: string | undefined;
  readonly extraMap: string | undefined;
  readonly requestLog: string | undefined;
  readonly stateAddress: ReadonlyMap<string, string>;
  readonly noDaemon: ReadonlySet<string>;
  readonly summary: string | undefined;
  readonly model: string;
  readonly concurrency: number;
  readonly maxWrites: number | undefined;
  readonly pauseBeforeWrite: string | undefined;
  /** When the run stops, as `--stop-at` names it (milliseconds since the epoch). */
  readonly stopAt: number | undefined;
  /** How long after its start the run stops, as `--stop-after` names it. */
  readonly stopAfterMs: number | undefined;
  /** Read every project again from the start (`--whole`). */
  readonly whole: boolean;
}

const DAEMON_FLAGS = {
  "daemon-state": { type: "string", multiple: true },
  "no-daemon": { type: "string", multiple: true },
  "extra-map": { type: "string" },
  "request-log": { type: "string" },
} as const;

const FLAGS = {
  ...DAEMON_FLAGS,
  tree: { type: "string" },
  "merge-commit": { type: "string" },
  project: { type: "string", multiple: true },
  "include-closed": { type: "boolean" },
  "state-document": { type: "string" },
  state: { type: "string" },
  summary: { type: "string" },
  model: { type: "string" },
  concurrency: { type: "string" },
  "max-writes": { type: "string" },
  "pause-before-write": { type: "string" },
  "stop-at": { type: "string" },
  "stop-after": { type: "string" },
  whole: { type: "boolean" },
} as const;

const SETTLE_FLAGS = {
  ...DAEMON_FLAGS,
  "state-document": { type: "string" },
  answers: { type: "string" },
  summary: { type: "string" },
  "merge-commit": { type: "string" },
} as const;

type FlagTable = Readonly<Record<string, { readonly type: string }>>;
const WRITE_ONLY = ["summary", "model", "concurrency", "max-writes", "pause-before-write"];

function refuse(reason: string): never {
  throw new UsageError(`${reason}\n${USAGE}`);
}

/** The first argument that names no flag of `table`, so a refusal can name it. */
function unknownFlag(argv: readonly string[], table: FlagTable): string | undefined {
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index] ?? "";
    if (!argument.startsWith("-")) continue;
    const name = argument.replace(/^--?/, "").split("=")[0] ?? "";
    if (!argument.startsWith("--") || !Object.hasOwn(table, name)) {
      return argument.split("=")[0];
    }
    if (table[name]?.type === "string" && !argument.includes("=")) index += 1;
  }
  return undefined;
}

/** The mode a command line names: its first argument that is neither a flag nor a flag's value. */
function modeOf(argv: readonly string[]): string | undefined {
  const table: FlagTable = { ...FLAGS, ...SETTLE_FLAGS };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index] ?? "";
    if (!argument.startsWith("-")) return argument;
    const name = argument.replace(/^--?/, "").split("=")[0] ?? "";
    if (table[name]?.type === "string" && !argument.includes("=")) index += 1;
  }
  return undefined;
}

function count(value: string | undefined, flag: string, least: number): number | undefined {
  if (value === undefined) return undefined;
  const number = Number(value);
  if (!/^[0-9]+$/.test(value) || number < least) refuse(`--${flag} takes a whole number`);
  return number;
}

const RFC_3339 = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/;
const DURATION = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/;

/** `--stop-at`'s time, in milliseconds since the epoch. */
function stopTime(value: string | undefined): number | undefined {
  if (value === undefined) return undefined;
  const at = Date.parse(value);
  if (!RFC_3339.test(value) || Number.isNaN(at)) refuse("--stop-at takes an RFC 3339 time");
  return at;
}

/** `--stop-after`'s duration (hours, minutes and seconds, such as `1h30m`), in milliseconds. */
function stopDuration(value: string | undefined): number | undefined {
  if (value === undefined) return undefined;
  const parts = DURATION.exec(value);
  const [hours = "0", minutes = "0", seconds = "0"] =
    parts?.slice(1).map((part) => part ?? "0") ?? [];
  const ms = ((Number(hours) * 60 + Number(minutes)) * 60 + Number(seconds)) * 1_000;
  if (parts === null || ms <= 0) refuse("--stop-after takes a duration such as 90m or 1h30m");
  return ms;
}

/** The flags as `parseArgs` reads them, once every argument names a flag of the mode. */
function readFlags<T extends ParseArgsOptionsConfig>(argv: readonly string[], options: T) {
  try {
    return parseArgs({ args: [...argv], options, allowPositionals: true, strict: true });
  } catch {
    // parseArgs's own message is not repeated: every flag it can refuse here is named already.
    return refuse("a flag is missing its value");
  }
}

/** `--merge-commit`, which every mode needs. */
function mergeCommitOf(value: string | undefined): string {
  if (value === undefined || !COMMIT.test(value)) refuse("--merge-commit takes a commit sha");
  return value;
}

/** `--daemon-state` and `--no-daemon`, which every mode reads alike. */
function daemonOptions(
  pairs: readonly string[] | undefined,
  none: readonly string[] | undefined
): { stateAddress: Map<string, string>; noDaemon: Set<string> } {
  const stateAddress = new Map<string, string>();
  for (const pair of pairs ?? []) {
    const [project, url] = [pair.slice(0, pair.indexOf("=")), pair.slice(pair.indexOf("=") + 1)];
    if (!pair.includes("=") || !PROJECT_KEY.test(project) || !/^https?:\/\/[^/]+\/?$/.test(url)) {
      refuse("--daemon-state takes <PROJECT>=<http(s) URL with no path>");
    }
    stateAddress.set(project, url);
  }
  const noDaemon = new Set(none ?? []);
  for (const project of noDaemon) {
    if (!PROJECT_KEY.test(project)) refuse(`--no-daemon takes a project key, not ${project}`);
    if (stateAddress.has(project)) refuse(`--no-daemon ${project} and --daemon-state ${project}`);
  }
  return { stateAddress, noDaemon };
}

/** `--state-document`, as a document reference, where it is given. */
function stateDocumentOf(ref: string | undefined): StateDocument | undefined {
  const stateDocument = ref === undefined ? undefined : parseStateDocument(ref);
  if (ref !== undefined && stateDocument === undefined) {
    refuse("--state-document takes dispatch://<PROJECT>/artifact/<slug>");
  }
  return stateDocument;
}

/** The command line of report or write mode. */
export function parseArguments(argv: readonly string[]): Options {
  const unknown = unknownFlag(argv, FLAGS);
  if (unknown !== undefined) refuse(`unknown flag ${unknown}`);
  const { values, positionals } = readFlags(argv, FLAGS);
  const [mode, ...rest] = positionals;
  if (mode !== "report" && mode !== "write") refuse("name the mode: report, write or settle");
  if (rest.length > 0) refuse(`unexpected argument ${rest[0]}`);
  if (mode === "report") {
    const given = WRITE_ONLY.find((flag) => values[flag as keyof typeof values] !== undefined);
    if (given !== undefined) refuse(`flag --${given} is for write mode`);
  }
  if (values.whole === true && mode === "report" && values.state !== undefined) {
    refuse("report mode's --whole writes no state, so it takes no --state");
  }
  const tree = values.tree;
  if (tree === undefined || !ISSUE_KEY.test(tree)) refuse("--tree takes an issue key");
  const mergeCommit = mergeCommitOf(values["merge-commit"]);
  const projects = values.project ?? [];
  const badProject = projects.find((project) => !PROJECT_KEY.test(project));
  if (badProject !== undefined) refuse(`--project takes a project key, not ${badProject}`);
  const stateDocument = stateDocumentOf(values["state-document"]);
  const summary = values.summary;
  if (mode === "write") {
    if (summary === undefined || summary.trim() === "") refuse("write mode needs --summary");
    if (stateDocument === undefined) refuse("write mode needs --state-document");
  }
  return {
    mode,
    tree,
    mergeCommit,
    projects,
    includeClosed: values["include-closed"] === true,
    stateDocument,
    stateFile: values.state,
    extraMap: values["extra-map"],
    requestLog: values["request-log"],
    ...daemonOptions(values["daemon-state"], values["no-daemon"]),
    summary,
    model: values.model ?? DEFAULT_MODEL,
    concurrency: count(values.concurrency, "concurrency", 1) ?? 4,
    maxWrites: count(values["max-writes"], "max-writes", 0),
    pauseBeforeWrite: values["pause-before-write"],
    stopAt: stopTime(values["stop-at"]),
    stopAfterMs: stopDuration(values["stop-after"]),
    whole: values.whole === true,
  };
}

/** The command line of the settle command. */
export function parseSettle(argv: readonly string[]): SettleOptions {
  const unknown = unknownFlag(argv, SETTLE_FLAGS);
  if (unknown !== undefined) {
    const name = unknown.replace(/^--?/, "");
    refuse(
      Object.hasOwn(FLAGS, name) ? `flag ${unknown} is not for settle` : `unknown flag ${unknown}`
    );
  }
  const { values, positionals } = readFlags(argv, SETTLE_FLAGS);
  const [mode, ...rest] = positionals;
  if (mode !== "settle") refuse("name the mode: report, write or settle");
  if (rest.length > 0) refuse(`unexpected argument ${rest[0]}`);
  const stateDocument = stateDocumentOf(values["state-document"]);
  if (stateDocument === undefined) refuse("settle needs --state-document");
  const answers = values.answers;
  if (answers === undefined) refuse("settle needs --answers");
  const summary = values.summary;
  if (summary === undefined || summary.trim() === "") refuse("settle needs --summary");
  return {
    mode: "settle",
    stateDocument,
    answers,
    summary,
    mergeCommit: mergeCommitOf(values["merge-commit"]),
    extraMap: values["extra-map"],
    requestLog: values["request-log"],
    ...daemonOptions(values["daemon-state"], values["no-daemon"]),
  };
}

/** The command line of any mode. */
export function parseCommandLine(argv: readonly string[]): Options | SettleOptions {
  return modeOf(argv) === "settle" ? parseSettle(argv) : parseArguments(argv);
}

/** What a run calls out to, which a test replaces with fakes. */
export interface Deps {
  readonly reads: DispatchReads;
  readonly writes: DispatchWrites | undefined;
  readonly model: Model | undefined;
  readonly daemonStates: DaemonStates;
  readonly ancestry: Ancestry;
  readonly clock: Clock;
  /** The daemon API contract of the checkout the tool runs from. */
  readonly contract: number;
  readonly extraEntries: readonly MapEntry[];
  readonly out: (line: string) => void;
  /** How many requests the run made so far, for its last lines. */
  readonly requests: () => number;
  /** The session the process acts as (`DISPATCH_SESSION_ID`): write mode and the settle command
   *  write as it and keep its id in the state, so that a report sets their edits aside. */
  readonly session?: string | undefined;
}

/** How many items are read before their turn of writing, so a project's texts are not all held
 *  at once. */
const CHUNK = 16;

/** Report mode's lines of the pace: each owner whose items wait for the judge, whose subscribers
 *  no credential of the tool can read; the run time the three bounds give for one write per
 *  waiting item; and the same for the settle command's writes, one per item that holds a
 *  judge-refused occurrence the report lists. */
function paceLines(tally: Tally): string[] {
  const lines = tally.owners().map((owner) => `owner ${ownerName(owner)} subscribers=unknown`);
  const writes = tally.waiting.map((item) => ({
    owner: ownerName(item.owner),
    project: item.project,
  }));
  const runTime = (paced: readonly PacedWrite[]) => Math.ceil(runTimeMs(paced) / 1_000);
  lines.push(`pace writes=${writes.length} run-time-s=${runTime(writes)}`);
  const settles = [...tally.settles.values()];
  lines.push(`settle-pace writes=${settles.length} run-time-s=${runTime(settles)}`);
  return lines;
}

/** Reads the state at the run's start: the state document, else the local copy, else none. */
async function startingState(
  options: Options,
  reads: DispatchReads
): Promise<{ state: RunState; token: string | null }> {
  if (options.stateDocument !== undefined) {
    return readStateDocument(reads, options.stateDocument);
  }
  if (options.stateFile !== undefined && existsSync(options.stateFile)) {
    return { state: parseState(readFileSync(options.stateFile, "utf8")), token: null };
  }
  return { state: emptyState(), token: null };
}

/**
 * The items a run reads again after the first whole read: each item an event after the cursor
 * belongs to, in the projects the run reads. The events are read before the run's first read of
 * an item (`runTool`). Dispatch gives an issue document's events to its issue and sets no
 * top-level `artifact_id` on them (`ownerForArtifact`), so an event of an issue names the issue's
 * title and every document of it; an event of a project document names that document.
 */
function wantedOfEvents(events: readonly LoggedEvent[], projects: ReadonlySet<string>): Wanted[] {
  return events.flatMap((event): Wanted[] => {
    if (!projects.has(event.project)) return [];
    if (event.issue_key !== null)
      return [{ project: event.project, issue: event.issue_key, id: null }];
    if (event.artifact_id !== null) {
      return [{ project: event.project, issue: null, id: event.artifact_id }];
    }
    return [];
  });
}

/** What a save compares, so the state is written only when something in it changed. */
function stateMark(state: RunState): string {
  const held = state.held.map((write) => `${write.id}${write.retry ? "!" : ""}`).join(",");
  const waiting = state.waiting.map((entry) => `${entry.id}:${entry.gate}`).join(",");
  const refused = state.refused.map((entry) => entry.key).join(",");
  return `${state.cursor}|${state.writeCursor}|${state.whole}|${state.finished.join(",")}|${state.verdicts.size}|${held}|${waiting}|${refused}|${state.sessions.join(",")}`;
}

/** Write mode saves its state at most once in this time while it holds something unsaved. */
export const SAVE_INTERVAL_MS = 60_000;

/** When the run stops, where its options name a stop time: the earlier of `--stop-at` and
 *  `--stop-after` counted from the run's start. */
function stopTimeOf(options: Options, started: number): number | undefined {
  const after = options.stopAfterMs === undefined ? undefined : started + options.stopAfterMs;
  if (options.stopAt === undefined) return after;
  return after === undefined ? options.stopAt : Math.min(options.stopAt, after);
}

/** The run's model calls, and the occurrences it judged again for a retried refused write. */
function callsLine(calls: JudgeCalls): string {
  return `model-calls first=${calls.first} second=${calls.second} again=${calls.again} judged-again=${calls.againOccurrences}`;
}

/** One whole run of report or write mode; answers the exit status. */
export async function runTool(options: Options, deps: Deps): Promise<number> {
  const { reads, out } = deps;
  const started = deps.clock.now();
  const stopTime = stopTimeOf(options, started);
  let halted = false;
  /** Whether the stop time has come: from then on the run starts no read, judgement or write. */
  const stopped = () => {
    if (!halted && stopTime !== undefined && deps.clock.now() >= stopTime) halted = true;
    return halted;
  };
  const calls: JudgeCalls = { first: 0, second: 0, again: 0, againOccurrences: 0 };
  /** The state saved once more when a read fails, where the run has a state to save. */
  let saveOnFailure: (() => Promise<void>) | undefined;
  try {
    const whoami = Whoami.parse(await reads.whoami());
    const writing = options.mode === "write";
    if (writing) {
      const refusal = writerRefusal(whoami, deps.session);
      if (refusal !== null) {
        out(`refused: ${refusal}`);
        return 2;
      }
    }
    const scope = whoami.kind === "agent" ? whoami.project : null;
    const build = await dispatchBuild(await reads.healthz(), options.mergeCommit, deps.ancestry);
    out(`dispatch commit=${build.commit ?? "null"} holds-merge=${build.holdsMerge ? "yes" : "no"}`);
    const entries = [...builtInMap(deps.contract), ...deps.extraEntries];
    out(`map entries=${entries.length} contract=${deps.contract}`);
    const known = Project.array()
      .parse(await reads.projects())
      .map((project) => project.key);
    const missing = options.projects.find((project) => !known.includes(project));
    if (missing !== undefined) {
      out(`unavailable: project ${missing} is not in Dispatch`);
      return 2;
    }
    const projects = options.projects.length > 0 ? [...options.projects] : known;
    const daemons = await readDaemons(projects, reads, deps.daemonStates, {
      noDaemon: options.noDaemon,
      stateAddress: options.stateAddress,
    });
    for (const [project, reading] of daemons) out(`daemon ${project} ${describeReading(reading)}`);
    const reachOf = (project: string) => ({
      daemonAtLeast: (contract: number) => daemonAtLeast(daemons.get(project), contract),
      dispatchHoldsMerge: build.holdsMerge,
    });
    let { state, token } = await startingState(options, reads);
    /** Whether the state document already names the session this run writes as. */
    let sessionSaved = !writing || deps.session === undefined;
    if (deps.session !== undefined && !sessionSaved) {
      sessionSaved = state.sessions.includes(deps.session);
      if (!sessionSaved) state.sessions.push(deps.session);
    }
    // The event log is read before the run's first read of an item, and the cursor moves only to
    // the newest event read here (spec version 32): an edit made while the run goes on has a
    // later event, which the next run reads. A report lists apart what an event after the last
    // write-mode run's cursor changed, the edits of the sessions that ran the tool set aside
    // (`changes.ts`); where both cursors are one, the log is read once for both.
    /** The cursor a run after the whole read moves to once it has read the events' items. */
    let eventCursor = state.cursor;
    /** The newest id at the start of a whole write run: its own cursor, for `write_cursor`. */
    let wholeCursor: number | undefined;
    let evented: readonly LoggedEvent[] = [];
    let changes: Changes | undefined;
    if (!stopped()) {
      const listed = writing || state.writeCursor === null ? undefined : state.writeCursor;
      if (listed !== undefined) changes = new Changes(new Set(state.sessions));
      let listedRead = false;
      if (options.whole) {
        if (writing) wholeCursor = await newestEventId(reads);
      } else if (!state.whole) {
        if (state.cursor === null) state.cursor = await newestEventId(reads);
      } else {
        const from = state.cursor ?? 0;
        listedRead = listed === from;
        evented = await eventsAfter(
          reads,
          from,
          listedRead ? (page) => changes?.add(page) : undefined
        );
        eventCursor = evented.at(-1)?.id ?? state.cursor;
      }
      if (listed !== undefined && !listedRead) {
        await readLog(reads, listed, (page) => changes?.add(page));
      }
    }
    const tree = await readTree(reads, options.tree);
    const context: Context = { reads, entries, reachOf, scope, tree };
    const tally = new Tally(changes);
    const seen = new Set<string>();
    let writeContext: WriteContext | undefined;
    if (writing) {
      const model = deps.model;
      if (deps.writes === undefined || model === undefined || options.summary === undefined) {
        throw new Error("write mode runs with writes, a model and a summary");
      }
      const pace = new Pace(deps.clock);
      writeContext = {
        reads,
        writes: deps.writes,
        answerer: { kind: "model", model, concurrency: options.concurrency },
        pace,
        paceLog: new PaceLog(reads, pace, new Set(state.sessions), deps.clock),
        // The session's id is saved before the run's first write (`save` is set below).
        beforeWrite: async () => {
          if (sessionSaved) return;
          sessionSaved = true;
          await save(true, true);
        },
        clock: deps.clock,
        state,
        entries,
        reachOf,
        summary: options.summary,
        remaining: options.maxWrites ?? Number.POSITIVE_INFINITY,
        pauseFile: options.pauseBeforeWrite,
        statusLines: new Map(),
        retried: new Set(state.held.filter((write) => write.retry).flatMap((write) => write.keys)),
        calls,
        written: new Map(),
      };
    }

    // Write mode saves the state when it holds something unsaved and a minute has passed since
    // its last save (or its start), and once more when the run stops; report mode never does.
    let savedAt = started;
    let savedMark = stateMark(state);
    const save = async (final: boolean, force = false) => {
      const writes = writeContext?.writes;
      if (writes === undefined || options.stateDocument === undefined) return;
      const now = deps.clock.now();
      if (!final && now - savedAt < SAVE_INTERVAL_MS) return;
      const mark = stateMark(state);
      if (mark === savedMark && !force) return;
      token = await writeStateDocument({ reads, writes }, options.stateDocument, state, token);
      savedAt = now;
      savedMark = mark;
    };
    /** The held-back writes this run took up, and the items whose turn it has handled: a read
     *  that fails puts back each held-back write whose item it had not handled. */
    let takenHeld: readonly HeldWrite[] = [];
    const handled = new Set<string>();
    saveOnFailure = async () => {
      for (const write of takenHeld) {
        if (handled.has(write.id) || state.held.some((kept) => kept.id === write.id)) continue;
        state.held.push(write);
      }
      await save(true);
    };

    /** Handles each item once; answers the items the stop time left unhandled. */
    const handle = async (items: readonly Item[]): Promise<Item[]> => {
      const fresh = items.filter((item) => !seen.has(item.id));
      for (const item of fresh) seen.add(item.id);
      for (let start = 0; start < fresh.length; start += CHUNK) {
        if (stopped()) return fresh.slice(start);
        const chunk = fresh.slice(start, start + CHUNK);
        const examined: Examined[] = await inPool(chunk, READ_CONCURRENCY, (item) =>
          examine(context, item)
        );
        for (const [index, one] of examined.entries()) {
          if (stopped()) return [...chunk.slice(index), ...fresh.slice(start + CHUNK)];
          keepWaiting(state, one);
          keepRefused(state, one);
          if (writeContext === undefined) {
            tally.add(one, state.verdicts);
            handled.add(one.item.id);
            continue;
          }
          const result = await writeItem(writeContext, one);
          tally.add(one, state.verdicts, writeContext.written);
          handled.add(one.item.id);
          await save(false);
          if (result === "nothing-to-judge") continue;
          const item = one.item;
          out(
            `write kind=${item.kind} project=${item.project} issue=${item.issue?.key ?? "-"} document=${item.artifact?.slug ?? "-"} result=${result}`
          );
        }
      }
      return [];
    };
    /** Write mode keeps for the next run the items a stop time left unhandled. */
    const holdLeft = (left: readonly Item[]) => {
      if (!writing) return;
      for (const item of left) state.held.push(heldWrite(item, []));
    };
    /** The held-back writes first, with `extra`: report mode reads them and leaves the held-back
     *  list as it is; write mode takes them up. */
    const handleHeld = async (extra: readonly Wanted[]) => {
      if (stopped()) return;
      const held = state.held;
      if (writing) {
        state.held = [];
        takenHeld = held;
      }
      holdLeft(await handle(await readItems(reads, [...held, ...extra])));
      takenHeld = [];
    };

    if (options.whole) {
      // The whole read: the held-back writes, then every project from the start, with the
      // state's verdicts. Its cursor and the projects it finished stay as they were, and a project
      // the stop time cuts is left to the next whole read. Write mode saves its state; report mode
      // writes none.
      await handleHeld([]);
      for (const project of projects) {
        if (stopped()) break;
        await handle(await projectItems(reads, project, options.includeClosed));
      }
      if (writing && wholeCursor !== undefined) state.writeCursor = wholeCursor;
    } else {
      // First the held-back writes, and the items whose kept names the project or Dispatch's
      // build has now reached.
      await handleHeld(
        state.waiting.filter(
          (entry) =>
            projects.includes(entry.project) && gateReached(entry.gate, reachOf(entry.project))
        )
      );
      if (!stopped() && !state.whole) {
        for (const project of projects) {
          if (state.finished.includes(project)) continue;
          if (stopped()) break;
          // A project the stop time cuts is read whole again by the next run, so its items are
          // not held back one by one.
          const left = await handle(await projectItems(reads, project, options.includeClosed));
          if (left.length > 0) break;
          state.finished.push(project);
          await save(false);
        }
        if (!stopped()) state.whole = true;
      } else if (!stopped()) {
        holdLeft(await handle(await readItems(reads, wantedOfEvents(evented, new Set(projects)))));
        state.cursor = eventCursor;
      }
      // What a write-mode run has read stands at its cursor: its items' later events are after it.
      if (writing) state.writeCursor = state.cursor;
    }
    // Each occurrence kept as judge-refused is listed under the place a program tells while that
    // place holds it: its item is read, in the projects the run reads, where this run has not
    // read it.
    if (!stopped()) {
      const unread = state.refused.filter(
        (entry) => projects.includes(entry.project) && !seen.has(entry.id)
      );
      await handle(await readItems(reads, unread));
    }
    await save(true);
    if (options.stateFile !== undefined) writeFileSync(options.stateFile, serializeState(state));

    tally.addKeptRefused(state.refused);
    for (const line of tally.lines) out(line);
    for (const line of tally.summary()) out(line);
    for (const line of tally.checkSection()) out(line);
    if (options.mode === "report") {
      for (const line of tally.changedSection()) out(line);
      for (const line of paceLines(tally)) out(line);
    }
    const bytes = stateBytes(state);
    const projected = bytes + (tally.notJudged - tally.judgeRefused) * VERDICT_BYTES;
    out(
      `state verdicts=${state.verdicts.size} held=${state.held.length} waiting=${state.waiting.length} not-judged=${tally.notJudged} bytes=${bytes} projected-bytes=${projected} bound=${STATE_BOUND_BYTES}`
    );
    out(callsLine(calls));
    out(`requests ${deps.requests()}`);
    out(`seconds ${Math.round((deps.clock.now() - started) / 1_000)}`);
    if (halted && stopTime !== undefined) {
      out(`result stopped at its stop time ${new Date(stopTime).toISOString()}`);
    } else {
      out("result done");
    }
    return 0;
  } catch (error) {
    if (error instanceof ReadError) {
      // A read that fails ends the run; what it judged and held back so far is kept.
      try {
        await saveOnFailure?.();
      } catch (saveError) {
        if (saveError instanceof StateTooLarge || saveError instanceof StateWriteError) {
          out(saveError.message);
        } else if (!(saveError instanceof ReadError)) {
          throw saveError;
        }
      }
      out(callsLine(calls));
      out(`unavailable: ${error.route} ${error.category}`);
      return 2;
    }
    if (error instanceof StateTooLarge || error instanceof StateWriteError) {
      out(error.message);
      return 1;
    }
    throw error;
  }
}

async function main(argv: readonly string[]): Promise<number> {
  if (argv.includes("--help") || argv.includes("-h")) {
    console.log(`usage: ${USAGE}`);
    return 0;
  }
  const prelude = await readCommandLine(argv, parseCommandLine, "config" satisfies Category);
  if (!prelude.ready) return prelude.code;
  const { options, config } = prelude;
  const root = resolve(import.meta.dir, "../../..");
  let extraEntries: MapEntry[] = [];
  if (options.extraMap !== undefined) {
    try {
      extraEntries = parseExtraMap(readFileSync(options.extraMap, "utf8"));
    } catch (error) {
      console.error(error instanceof MapFileError ? error.message : "--extra-map cannot be read");
      return 2;
    }
  }
  let answers = "";
  if (options.mode === "settle") {
    try {
      answers = readFileSync(options.answers, "utf8");
    } catch {
      console.error("--answers cannot be read");
      return 2;
    }
  }
  const log = requestLog(
    options.requestLog === undefined ? undefined : appendTo(options.requestLog)
  );
  const fetchImpl = loggingFetch(fetch, log, new RequestRate());
  // An empty DISPATCH_SESSION_ID names no session, as an unset one does.
  const session = process.env.DISPATCH_SESSION_ID || undefined;
  const { reads, writes } = liveDispatch(config, fetchImpl, session);
  const deps: Deps = {
    reads,
    writes: options.mode === "report" ? undefined : writes,
    model:
      options.mode === "write"
        ? gatewayModel(options.model, (detail) => console.error(`model fault: ${detail}`))
        : undefined,
    daemonStates: liveDaemonStates(fetchImpl),
    ancestry: gitAncestry(root),
    clock: SYSTEM_CLOCK,
    contract: readContract(root),
    extraEntries,
    out: (line) => console.log(line),
    requests: () => log.count(),
    session,
  };
  return options.mode === "settle" ? runSettle(options, answers, deps) : runTool(options, deps);
}

if (import.meta.main) await runScript(main);
