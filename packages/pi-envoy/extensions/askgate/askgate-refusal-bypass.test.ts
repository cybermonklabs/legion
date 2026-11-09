import { afterEach, expect, test } from "bun:test";
import * as fs from "node:fs";
import * as path from "node:path";
import {
  type AskgateOwner,
  askgateState,
  openStore,
  pidNamespace,
  processStart,
  setRecordMode,
  writeAskgateRecord,
} from "@legion/envoy-client/askgate-marker";
import { DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { sessionDirectory } from "@legion/envoy-client/dispatch-session-directory";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { temporaryTmpdir } from "@legion/test-fixtures/temporary-tmpdir";
import type { DispatchSession } from "../../src/dispatch-session";
import * as core from "./askgate-core";
import {
  ADVISOR_GATE_ENTRY_TYPE,
  ADVISOR_GATE_RECORD_ENTRY_TYPE,
  type CompleteRequest,
  createAskGate,
  type DryRunOptions,
  type DryRunResult,
  type GateEntry,
  type GateModel,
  type GateRecordEntry,
  type Message,
} from "./askgate-core";
import * as fx from "./askgate-fixtures";
import { createDecisionRecorder } from "./decision-record";
import type { RuleMatch } from "./rules-check";

/** Runs a `dispatch` command's --dry-run as the entry's binding does. */
type DryRunner = (script: string, opts: DryRunOptions) => Promise<DryRunResult>;

/** Dispatch's answer that the project holds no decision document of the session's name. */
const _NO_DOCUMENT = new DispatchServiceError(
  "ARTIFACT_NOT_FOUND",
  404,
  "document not found by slug"
);

/**
 * The suite's own TMPDIR, pointed at this directory before the file's first test and put back
 * when it ends: every scratch directory a test makes through `tmpdir()` lands here, and the
 * fixture's `afterAll` removes it. The fixture makes the directory and writes `TMPDIR` in a
 * `beforeAll`, so a run that selects no row of this file does neither.
 */
const SUITE_TMP = temporaryTmpdir("gate-suite");
/**
 * Directories named as the gate's old snapshots were (`askgate-` and six characters): the gate
 * reads each file in place now and writes no copy of one, which the suite's last test holds.
 */
const _snapshotDirs = () =>
  fs.readdirSync(SUITE_TMP).filter((name) => /^askgate-[A-Za-z0-9]{6}$/.test(name));
/**
 * Ends every call a test's gates were handed, as the runner ends a call's tool, so the gate releases
 * the marker each one took: a test that never ends its call would otherwise leave it behind.
 */
const callEnders: Array<() => Promise<void>> = [];
afterEach(async () => {
  for (const end of callEnders.splice(0)) await end();
});

/** One marker the gate minted, as the CLI reads it: its file name and its JSON. */
type MintedMarker = {
  name: string;
  v: number;
  argv: string;
  args: string | null;
  files: Array<{ flag: string; operand: string; sha256: string }>;
  toolCallId: string;
  outcome: string;
  createdAt: number;
};
/** The markers in a store, each with its file name; none when the store has no markers yet. */
function markersIn(store: string): MintedMarker[] {
  const dir = path.join(store, "markers");
  if (!fs.existsSync(dir)) return [];
  return fs
    .readdirSync(dir)
    .filter((name) => !name.startsWith("."))
    .map((name) => ({ name, ...JSON.parse(fs.readFileSync(path.join(dir, name), "utf8")) }));
}

const HOME = "/home/tester";
/** The session's envoy.json, which the notice for an OMP_ASKGATE the gate does not read names. */
const CONFIG_FILE = `${HOME}/.config/legion/envoy.json`;
const MODEL = { provider: "anthropic", id: "claude-fable-5-1" };
/** The `experiments` entry the experiments extension appends, with its `askgate_block` arm `on`. */
const experimentsEntry = (on: boolean) => ({
  type: "custom",
  customType: "experiments",
  data: {
    v: 1,
    rootSessionId: "01a15300-0000-7000-8000-000000000001",
    features: { askgate_block: { gate: "random", draw: on ? "on" : "off", on } },
  },
});
const USAGE = { input: 1200, output: 40, cacheRead: 300, cacheWrite: 900, cost: 0.0231 };
const ALLOW = '{"decision":"allow"}';
const REVISE =
  '{"decision":"revise","reason":"failure 7 (content gate): \\"see the message above\\" points at nothing; send the text itself"}';
const _bytes = (s: string) => Buffer.byteLength(s, "utf8");
const PARTIAL = { input: 40000, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0.4 };
/** A model call that runs until aborted, then settles as a real aborted request does: an error, with the input it was billed for. */
const _untilAborted = (req: CompleteRequest): Promise<Completion> => {
  const { promise, resolve } = Promise.withResolvers<Completion>();
  req.signal.addEventListener(
    "abort",
    () => resolve({ text: "", error: "aborted", usage: PARTIAL }),
    { once: true }
  );
  return promise;
};

type Completion = { text: string; error?: string; usage?: GateEntry["usage"] };
type Result =
  | {
      block?: boolean;
      reason?: string;
      input?: Record<string, unknown>;
      additionalContext?: string;
      /** A `tool_result` handler's replacement for the call's own result. */
      content?: Array<{ type: string } & Record<string, unknown>>;
    }
  | undefined;
type Handler = (event: unknown, ctx: unknown) => Promise<Result>;

/** What a test changes about the session it binds; every field has a default below. */
interface BindOptions {
  env?: Record<string, string>;
  complete?: (req: CompleteRequest) => Promise<Completion>;
  kind?: "main" | "sub";
  /** The models `@askgate` and `@default` resolve to: the askgate role's MODEL alone unless a test says otherwise. */
  roles?: { askgate?: GateModel; default?: GateModel };
  primary?: { provider: string; id: string };
  /** The primary's context, as the entry's buildSessionContext binding returns it. */
  context?: () => readonly Message[];
  systemPrompt?: string[];
  /** Every appendEntry throws: the session file cannot take the record. */
  recordThrows?: boolean;
  /** extensionHandlers.toolCallTimeoutMs as the entry reads it; 120 000 ms unless a test says otherwise. */
  ceiling?: number;
  /** The session's working directory (ctx.cwd). */
  cwd?: string;
  /** Runs a `dispatch` command's --dry-run; the real runner unless a test says otherwise. */
  dryRun?: DryRunner;
  /** The gate role's fallback chain after its first model, as the entry resolves it; none unless a test says otherwise. */
  fallbacks?: Array<{ provider: string; id: string }>;
  /** `askgate.sharedAccountProviders` as the entry reads it; none unless a test says otherwise. */
  sharedAccounts?: string[];
  /** The session's entries, read on each call: an `experiments` entry whose askgate_block arm is off (warn) unless a test says otherwise. */
  entries?: readonly unknown[] | (() => readonly unknown[]);
  /** Whether the session has a UI, which a notice needs; true unless a test says otherwise. */
  hasUI?: boolean;
  /** The gate's marker store: a fresh directory under the suite's TMPDIR unless a test says otherwise. */
  store?: string;
  /** The session directory whose record the gate writes its mode into; a fresh one unless shared. */
  sessionDir?: string;
  /** The session's Dispatch, as the directory a call runs in configures it; none unless a test says otherwise. */
  dispatch?: (directory: string, signal: AbortSignal) => DispatchSession | undefined;
  /** The clock the gate times itself by; the real one unless a test drives it. */
  now?: () => number;
}

/** One session binding of the gate, driven through a fake `pi` and `ctx`. */
interface BoundGate {
  handlers: Map<string, Handler>;
  entries: GateEntry[];
  /**
   * The `advisor-gate-record` entries the gate appended for records it could not write: the
   * decision record runs after the handler has returned, so its failure rides on its own entry.
   */
  recordEntries: GateRecordEntry[];
  /** What the host's logger was told at bind: a notice at every session start. */
  warnings: string[];
  /** What the host's plain log was told while the session ran (`runtimeLogger`), which no later session repeats. */
  runtimeWarnings: string[];
  notices: string[];
  dumps: Array<[string, string]>;
  calls: CompleteRequest[];
  fallbackRoles: string[];
  dryRuns: Array<{ script: string; cwd: string; timeoutMs: number }>;
  /** Every Dispatch client the gate asked for: the directory it asked for, and that read's signal. */
  dispatchReads: Array<{ directory: string; signal: AbortSignal }>;
  /** The marker store the gate mints into. */
  store: string;
  /** The session directory whose record carries this binding's check mode. */
  sessionDir: string;
  /** The check mode the session's record now shows the CLI, or undefined when the record is off. */
  recordedMode(): "warn" | "block" | undefined;
  write(toolCallId: string, path: string, content: string): Promise<Result>;
  device(toolCallId: string, toolName: string, input: Record<string, unknown>): Promise<Result>;
  bash(toolCallId: string, command: string, extra?: Record<string, unknown>): Promise<Result>;
  /** Any other event the extension listens to, such as session_switch. */
  event(name: string, payload?: Record<string, unknown>): Promise<Result> | undefined;
}

function bind(opts: BindOptions = {}): BoundGate {
  const handlers = new Map<string, Handler>();
  const entries: GateEntry[] = [];
  /** The `advisor-gate-record` follow-ups of records that were never written. */
  const recordEntries: GateRecordEntry[] = [];
  const warnings: string[] = [];
  const runtimeWarnings: string[] = [];
  const notices: string[] = [];
  const dumps: Array<[string, string]> = [];
  const calls: CompleteRequest[] = [];
  /** The model role each fallback-chain read was made under, in order. */
  const fallbackRoles: string[] = [];
  /** Every dry-run the gate started: its script, working directory and time limit. */
  const dryRuns: Array<{ script: string; cwd: string; timeoutMs: number }> = [];
  /** Every Dispatch client the gate asked for: the directory it asked for, and that read's signal. */
  const dispatchReads: Array<{ directory: string; signal: AbortSignal }> = [];
  const store = opts.store ?? fs.mkdtempSync(path.join(SUITE_TMP, "store-"));
  // The session directory the gate records its mode into (one real process keeps a record per
  // session it binds). A test may share a store and a session across binds to drive the switch case.
  const sessionDir = opts.sessionDir ?? fs.mkdtempSync(path.join(SUITE_TMP, "session-"));
  const self: AskgateOwner = {
    pid: process.pid,
    start: processStart(process.pid),
    ns: pidNamespace(),
    store,
  };
  try {
    openStore(store);
    // The bind, as envoy.ts does it: the mode from OMP_ASKGATE, which an arm does not change here.
    writeAskgateRecord(
      sessionDir,
      self,
      opts.env?.OMP_ASKGATE?.trim() === "block" ? "block" : "warn"
    );
  } catch {
    // A broken store, as envoy.ts catches and notifies: the gate's own mint then fails for the call.
  }
  /** Every handler an event has, in registration order: the host's own run before the core's. */
  const registered = new Map<string, Handler[]>();
  const pi = {
    on: (event: string, handler: Handler) => {
      const bound = registered.get(event) ?? [];
      bound.push(handler);
      registered.set(event, bound);
      handlers.set(event, async (payload: unknown, ctx: unknown) => {
        let last: Result;
        for (const each of bound) last = await each(payload, ctx);
        return last;
      });
    },
    appendEntry: (customType: string, data: GateEntry | GateRecordEntry) => {
      expect([ADVISOR_GATE_ENTRY_TYPE, ADVISOR_GATE_RECORD_ENTRY_TYPE]).toContain(customType);
      if (opts.recordThrows) throw new Error("session file unavailable");
      if (customType === ADVISOR_GATE_RECORD_ENTRY_TYPE)
        recordEntries.push(data as GateRecordEntry);
      else entries.push(data as GateEntry);
    },
  };
  const complete = opts.complete ?? (async () => ({ text: ALLOW, usage: USAGE }));
  // The session's Dispatch, which a test that drives a document read or a decision record binds;
  // with none, a document read gives nothing and no decision record is written.
  const dispatchClient = (directory: string, signal: AbortSignal) => {
    dispatchReads.push({ directory, signal });
    return opts.dispatch?.(directory, signal);
  };
  // The host's own recorder, which the entry builds once per process and hands the core
  // (`registerAskGate`): the gate builds none of its own.
  const recordDecision = createDecisionRecorder({
    env: opts.env ?? {},
    now: opts.now ?? Date.now,
    dispatchClient,
  });
  // The host's own `session_shutdown`, as `registerAskGate` registers it at bind time and before
  // the core's: the recorder owns the cut and its delay, and the core's handler waits on what is
  // still pending.
  pi.on("session_shutdown", async () => {
    await recordDecision.shutdown();
    return undefined;
  });
  createAskGate({
    env: opts.env ?? {},
    home: HOME,
    now: opts.now ?? Date.now,
    logger: { warn: (message: string) => warnings.push(message) },
    runtimeLogger: { warn: (message: string) => runtimeWarnings.push(message) },
    appendFile: (p, text) => dumps.push([p, text]),
    complete: (req) => {
      calls.push(req);
      return complete(req);
    },
    fallbackModels: (_ctx, _primary, role) => {
      fallbackRoles.push(role);
      return opts.fallbacks ?? [];
    },
    sharedAccountProviders: () => opts.sharedAccounts ?? [],
    configFile: CONFIG_FILE,
    contextMessages: () => (opts.context ?? (() => [fx.user("Post the comment.")]))(),
    handlerCeilingMs: () => opts.ceiling ?? 120_000,
    dispatchClient,
    recordDecision,
    dryRun: (script, runOpts) => {
      dryRuns.push({ script, cwd: runOpts.cwd, timeoutMs: runOpts.timeoutMs });
      return (opts.dryRun ?? core.spawnDryRun)(script, runOpts);
    },
    markerStore: store,
    sessionDir: () => sessionDir,
    recordCheckMode: (mode) => setRecordMode(sessionDir, self, mode),
  })(pi as never);
  const roles = opts.roles ?? { askgate: MODEL };
  const sessionEntries = opts.entries ?? [experimentsEntry(false)];
  const ctx = {
    agent: { kind: opts.kind ?? "main", id: "Main", name: "main", depth: 0 },
    hasUI: opts.hasUI ?? true,
    ui: { notify: (message: string) => notices.push(message) },
    getSystemPrompt: () => opts.systemPrompt ?? ["PRIMARY SYSTEM RULES", "More rules."],
    models: {
      resolve: (spec: string) =>
        spec === "@askgate" ? roles.askgate : spec === "@default" ? roles.default : undefined,
    },
    /** The session's primary model: on the gate's provider unless a test says otherwise. */
    model: "primary" in opts ? opts.primary : MODEL,
    sessionManager: {
      getSessionId: () => "sess-1",
      getEntries: () => (typeof sessionEntries === "function" ? sessionEntries() : sessionEntries),
    },
    cwd: opts.cwd ?? "/work",
  };
  const handler = handlers.get("tool_call");
  /** Every call id this binding was handed, ended after the test (`callEnders`). */
  const emitted = new Set<string>();
  callEnders.push(async () => {
    for (const toolCallId of emitted)
      await handlers.get("tool_execution_end")?.(
        { type: "tool_execution_end", toolCallId, toolName: "bash", isError: false },
        ctx
      );
  });
  const emit = (event: Record<string, unknown>) => {
    if (!handler) throw new Error("no tool_call handler registered");
    if (typeof event.toolCallId === "string") emitted.add(event.toolCallId);
    return handler({ type: "tool_call", ...event }, ctx);
  };
  return {
    handlers,
    entries,
    recordEntries,
    warnings,
    runtimeWarnings,
    notices,
    dumps,
    calls,
    fallbackRoles,
    dryRuns,
    dispatchReads,
    store,
    sessionDir,
    recordedMode: () => {
      const state = askgateState(sessionDir);
      return state.kind === "on" ? state.mode : undefined;
    },
    write: (toolCallId: string, path: string, content: string) =>
      emit({ toolName: "write", toolCallId, input: { path, content } }),
    device: (toolCallId: string, toolName: string, input: Record<string, unknown>) =>
      emit({ toolName, toolCallId, input }),
    bash: (toolCallId: string, command: string, extra: Record<string, unknown> = {}) =>
      emit({ toolName: "bash", toolCallId, input: { command, ...extra } }),
    /** Any other event the extension listens to, such as session_switch. */
    event: (name: string, payload: Record<string, unknown> = {}) =>
      handlers.get(name)?.({ type: name, ...payload }, ctx),
  };
}

/**
 * Lets the decision record the gate starts as its handler returns run to its end: nothing of the
 * gated call waits on it, so a test that reads what Dispatch was sent drains the microtasks the
 * stand-in client settles on.
 */
const _recorded = async () => {
  for (let i = 0; i < 50; i++) await Promise.resolve();
};

/** The two checks tell themselves apart by the frame each system prompt opens with. */
const _isCharter = (req: CompleteRequest) => req.system.includes("You are AskGate.");
const isRules = (req: CompleteRequest) => req.system.includes("You are AskGate's rule check.");
/** The rules call's answer, as its request asks for it. */
const rulesAnswer = (matches: readonly Partial<RuleMatch>[], reason = "") =>
  JSON.stringify({ matches, reason });
const _RULES_NONE = rulesAnswer([]);
/** A model stand-in that answers each check with its own text. */
const _bySystem =
  (
    charter: string | ((req: CompleteRequest) => Promise<Completion>),
    rules: string | ((req: CompleteRequest) => Promise<Completion>)
  ) =>
  (req: CompleteRequest): Promise<Completion> => {
    const answer = isRules(req) ? rules : charter;
    return typeof answer === "string"
      ? Promise.resolve({ text: answer, usage: USAGE })
      : answer(req);
  };

// The cross-model review of 21146dfe (dispatch://INFRA-2079, row 3.5), kept as written there and
// turned round to assert the fix. AskGate's review really rejects this payload and blocks the
// literal call; the agent then sends the same write through a form the gate does not read. Since
// a rejection minted nothing, the CLI could not tell that write from one no review ever saw, and
// it posted. The gate writes its refusals down now, so each form below meets the same no.
const makeAttackDir = temporaryDirectories("ReviewGateSol-live");
for (const form of ["nested-shell", "variable-command", "compound-command"]) {
  test(`ReviewGateSol actual revise verdict then ${form}`, async () => {
    const work = makeAttackDir();
    const bin = path.join(work, "bin");
    fs.mkdirSync(bin);
    const entry = path.join(
      import.meta.dir,
      "..",
      "..",
      "..",
      "envoy-client",
      "bin",
      "dispatch.ts"
    );
    fs.writeFileSync(
      path.join(bin, "dispatch"),
      `#!/bin/sh\nexec "${process.execPath}" "${entry}" "$@"\n`,
      { mode: 0o755 }
    );
    const sent: unknown[] = [];
    const server = Bun.serve({
      port: 0,
      async fetch(req) {
        if (req.method === "POST")
          sent.push({ path: new URL(req.url).pathname, body: await req.json() });
        return Response.json({
          id: "comment-1",
          issue_key: "DSP-1",
          advice: {
            issue_status: "in_progress",
            session_writes_since_human: 0,
            your_open_asks: [],
          },
        });
      },
    });
    try {
      const env = {
        PATH: `${bin}:${process.env.PATH}`,
        HOME: work,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "reviewed-rejected",
        DISPATCH_STATE_DIR: path.join(work, "state"),
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN: "test-token",
        OMP_ASKGATE: "block",
      };
      const g = bind({
        env,
        cwd: work,
        sessionDir: sessionDirectory(env, env.DISPATCH_SESSION_ID),
        complete: async () => ({ text: REVISE }),
      });
      const command = "dispatch comment --issue DSP-1 --body reviewed-rejected-payload";
      // Failure control: the model really returned revise for this exact payload, and blocked it.
      const refusal = await g.bash("literal-control", command);
      expect(refusal?.block).toBe(true);
      expect(g.calls).toHaveLength(1);
      expect(g.calls[0].user).toContain("reviewed-rejected-payload");
      expect(g.entries).toMatchObject([
        { outcome: "verdict", decision: "revise", verdictMode: "block" },
      ]);
      expect(sent).toHaveLength(0);
      expect(markersIn(g.store)).toEqual([]);
      const wrapped =
        form === "nested-shell"
          ? `bash -c ${JSON.stringify(command)}`
          : form === "variable-command"
            ? "d=dispatch; $d comment --issue DSP-1 --body reviewed-rejected-payload"
            : `${command}; true`;
      expect(await g.bash("bypass", wrapped)).toBeUndefined();
      expect(g.calls).toHaveLength(1);
      expect(markersIn(g.store)).toEqual([]);
      const child = Bun.spawn(["bash", "-c", wrapped], {
        cwd: work,
        env,
        stdout: "pipe",
        stderr: "pipe",
      });
      const [code, stdout, stderr] = await Promise.all([
        child.exited,
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
      ]);
      console.log(
        JSON.stringify({
          tree: path.resolve(import.meta.dir, "../../../.."),
          form,
          original: {
            block: refusal?.block,
            verdict: g.entries[0].decision,
            modelCalls: g.calls.length,
          },
          bypass: {
            code,
            posts: sent,
            warning: stdout.includes("posted without review"),
            refusal: stdout.includes("was not called"),
            stderr,
          },
        })
      );
      expect(sent).toHaveLength(0);
      expect(stdout).toContain("was not called");
      expect(stdout).toContain("AskGate reviewed this write and did not send it");
      expect(stdout).not.toContain("posted without review");
    } finally {
      server.stop(true);
    }
  }, 30000);
}
