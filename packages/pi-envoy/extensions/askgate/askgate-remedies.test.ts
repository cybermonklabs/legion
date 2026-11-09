// Every remedy the CLI prints, followed rather than read, from a main agent and from a task
// subagent, in the state that prints it. A real gate, a real review, the real CLI.
//
// The harness is a reviewer's (ReviewGate11A), kept rather than reinvented: its fidelity is the
// point. The author's own control simulated the mint and reached three of the six states, and
// its name claimed the coverage — which is how three remedies stayed false through a round that
// was spent fixing remedies.
//
// A message is as much an artefact of a head as a test is, and it has no suite to fail. These
// are that suite.

import { describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { join, resolve } from "node:path";
import {
  clearVerdict,
  MARKER_MAX_AGE_MS,
  markVerdictsLost,
  mintMarker,
  pidNamespace,
  processStart,
  readVerdict,
  setRecordMode,
  writeAskgateRecord,
  writeIdentity,
} from "@legion/envoy-client/askgate-marker";
import { runDispatchCli } from "@legion/envoy-client/dispatch-cli";
import { createAskGate, type Deps, type Pi, spawnDryRun } from "./askgate-core";
import { createDecisionRecorder } from "./decision-record";

const BASE = ["comment", "--issue", "DSP-1", "--body", "rejected-content"];
const COMMAND = "dispatch comment --issue DSP-1 --body rejected-content";
const ARGS = { issue: "DSP-1", body: "rejected-content" };
const identity = () => writeIdentity("dispatch_comment", ARGS);

/** One session under test: a live gate, a fake Dispatch, and the real CLI against both. */
interface Fixture {
  readonly work: string;
  readonly store: string;
  readonly dir: () => string;
  readonly stop: () => void;
  /** Makes every later review allow, so only the marker it mints decides. */
  readonly allow: () => void;
  /**
   * Switches the session's chosen mode after the opening review has run. A warn-mode revise
   * records no verdict — it neither records nor clears, which is the model's `leave` — so a
   * state that is a recorded refusal can only be seeded in block and then switched.
   */
  readonly setMode: (mode: "warn" | "block") => void;
  /** The gate reading one literal call, as the host would hand it over. */
  readonly gate: (
    who: "main" | "sub",
    command?: string
  ) => Promise<{ block?: boolean } | undefined>;
  /** The real CLI, against the fake Dispatch. */
  readonly cli: (argv?: readonly string[]) => Promise<{ code: number; out: string; posts: number }>;
}

function session(answer: "allow" | "revise" = "revise"): Fixture {
  const work = mkdtempSync("/tmp/askgate-remedies-");
  const sent: string[] = [];
  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      if (request.method === "POST" || request.method === "PATCH") {
        await request.text();
        sent.push(new URL(request.url).pathname);
      }
      return Response.json({
        id: "write-1",
        issue_key: "DSP-1",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    },
  });
  // The gate's dry run execs whatever `dispatch` is on PATH, which without this is the
  // released plugin build rather than the tree under test: every row would measure a CLI this
  // branch does not change. A shim at the head of PATH runs the entry point from source.
  const bin = join(work, "bin");
  mkdirSync(bin, { recursive: true });
  const entry = join(import.meta.dir, "..", "..", "..", "envoy-client", "bin", "dispatch.ts");
  writeFileSync(
    join(bin, "dispatch"),
    `#!/bin/sh\nexec ${process.execPath} ${JSON.stringify(entry)} "$@"\n`,
    { mode: 0o755 }
  );
  const env = {
    PATH: `${bin}:${process.env.PATH}`,
    HOME: work,
    TMPDIR: "/tmp",
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: "ses-1",
    DISPATCH_STATE_DIR: join(work, "state"),
    DISPATCH_URL: `http://127.0.0.1:${server.port}`,
    DISPATCH_TOKEN: "test-token",
    OMP_ASKGATE: "block",
  };
  const store = join(work, "store");
  const dir = () => join(env.DISPATCH_STATE_DIR, "sessions", "ses-1");
  const owner = { pid: process.pid, start: processStart(process.pid), ns: pidNamespace(), store };
  writeAskgateRecord(dir(), owner, "block");
  let verdict = answer;
  const handlers = new Map<string, (event: unknown, ctx: unknown) => unknown>();
  createAskGate({
    env,
    home: work,
    now: Date.now,
    logger: { warn: () => {} },
    runtimeLogger: { warn: () => {} },
    appendFile: () => {},
    complete: async () => {
      return {
        text:
          verdict === "allow"
            ? '{"decision":"allow"}'
            : '{"decision":"revise","reason":"fixture rejection of this exact write"}',
      };
    },
    fallbackModels: () => [],
    sharedAccountProviders: () => [],
    configFile: join(work, "envoy.json"),
    contextMessages: () => [
      { role: "user", content: [{ type: "text", text: "Post this." }], timestamp: Date.now() },
    ],
    handlerCeilingMs: () => 120_000,
    dispatchClient: () => undefined,
    recordDecision: createDecisionRecorder({ env, now: Date.now, dispatchClient: () => undefined }),
    dryRun: spawnDryRun,
    markerStore: store,
    sessionDir: dir,
    recordCheckMode: (mode: "warn" | "block") => setRecordMode(dir(), owner, mode),
  } as unknown as Deps)({
    on: (event: string, handler: (event: unknown, ctx: unknown) => unknown) =>
      handlers.set(event, handler),
    appendEntry: () => {},
    // The gate uses two of the host's members; the rest of `Pi` is nothing this exercises.
  } as unknown as Pi);
  const model = { provider: "anthropic", id: "claude-fable-5-1" };
  const main = {
    agent: { kind: "main", id: "Main", name: "main", depth: 0 },
    hasUI: false,
    ui: { notify: () => {} },
    getSystemPrompt: () => ["review fixture"],
    models: { resolve: () => model },
    model,
    sessionManager: { getSessionId: () => "ses-1", getEntries: () => [] },
    cwd: work,
  };
  const sub = { ...main, agent: { kind: "task", id: "Sub", name: "sub", depth: 1 } };
  return {
    work,
    store,
    dir,
    stop: () => server.stop(true),
    allow: () => {
      verdict = "allow";
    },
    setMode: (mode) => {
      setRecordMode(dir(), owner, mode);
      env.OMP_ASKGATE = mode;
    },
    /** The gate reading one literal call, as the host would hand it over. */
    gate: (who: "main" | "sub", command: string = COMMAND) =>
      handlers.get("tool_call")?.(
        {
          type: "tool_call",
          toolName: "bash",
          toolCallId: `t-${Math.random()}`,
          input: { command },
        },
        who === "main" ? main : sub
      ) as Promise<{ block?: boolean } | undefined>,
    /** The real CLI, against the fake Dispatch. */
    cli: async (argv: readonly string[] = BASE) => {
      let out = "";
      const before = sent.length;
      const code = await runDispatchCli([...argv], env, {
        stdout: (text: string) => {
          out += text;
        },
        stderr: (text: string) => {
          out += text;
        },
        readText: (path: string) => (path === "-" ? "" : readFileSync(resolve(work, path), "utf8")),
        cwd: work,
      });
      return { code, out: out.replace(/\s+/g, " ").trim(), posts: sent.length - before };
    },
  };
}

/** The advice, followed: the gate reads the resend, then the CLI runs the same argv. */
async function resend(
  s: Fixture,
  who: "main" | "sub",
  call: { command: string; argv: readonly string[] } = { command: COMMAND, argv: BASE }
) {
  const hook = await s.gate(who, call.command);
  if (hook?.block === true) return { code: 1, out: "held at the gate", posts: 0 };
  return s.cli(call.argv);
}

describe("every remedy the CLI prints, followed", () => {
  /**
   * Every state the CLI prints a resend from, enumerated from `dispatch-cli.ts` rather than
   * from a previous round's list. `arrange` puts the state in place; `repair` carries out any
   * clause of the advice that comes before the resend, since a remedy is followed as written or
   * it is not followed at all.
   */
  const states: Array<{
    readonly name: string;
    readonly arrange: (s: Fixture) => void;
    /** The call this state is reached by, where it is not the default one. */
    readonly call?: (s: Fixture) => { command: string; argv: readonly string[] };
    readonly repair?: (s: Fixture) => void;
    /** Whether the state holds the write at all; `false` means it posts with a warning. */
    readonly holds: boolean;
  }> = [
    { name: "a recorded refusal of this write", arrange: () => {}, holds: true },
    {
      name: "a marker of this call that cannot be read",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        writeFileSync(
          mintMarker(
            s.store,
            { argv: BASE, args: ARGS, files: [], toolCallId: "x", outcome: "verdict" },
            Date.now()
          ),
          "{not-json"
        );
      },
      // Clause one of its own advice: remove the file. The resend after it is a different
      // question from the resend before it.
      repair: (s) => {
        for (const name of readdirSync(join(s.store, "markers")))
          rmSync(join(s.store, "markers", name), { force: true });
      },
      holds: true,
    },
    {
      name: "a marker bound to other arguments",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        mintMarker(
          s.store,
          {
            argv: BASE,
            args: { issue: "DSP-1", body: "other" },
            files: [],
            toolCallId: "x",
            outcome: "verdict",
          },
          Date.now()
        );
      },
      holds: true,
    },
    {
      // Built by the mechanism that produces it, not field by field: `sweepMarkers` judges age
      // by the file's mtime and `consumeMarker` by the body's `createdAt`, so a marker aged
      // only in its body is a state the clock cannot reach — and a row constructing one tests
      // nothing. Aged by both, the resend's own mint sweeps it and a subagent's posts.
      name: "a marker past its age",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        const file = mintMarker(
          s.store,
          { argv: BASE, args: null, files: [], toolCallId: "x", outcome: "verdict" },
          Date.now() - 3 * MARKER_MAX_AGE_MS
        );
        const old = (Date.now() - 3 * MARKER_MAX_AGE_MS) / 1000;
        utimesSync(file, old, old);
      },
      holds: true,
    },
    {
      name: "no verdict capability",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        const file = join(
          s.dir(),
          "askgate",
          `${process.pid}-${processStart(process.pid)}-${pidNamespace()}.json`
        );
        const record = JSON.parse(readFileSync(file, "utf8")) as Record<string, unknown>;
        delete record.caps;
        writeFileSync(file, JSON.stringify(record));
      },
      holds: true,
    },
    {
      name: "a foreign verdict store",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        const older = join(s.dir(), "askgate-verdicts-v9");
        mkdirSync(older, { recursive: true });
        writeFileSync(join(older, "deadbeef"), "{}");
      },
      holds: true,
    },
    {
      name: "a lost verdict",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        markVerdictsLost(s.dir(), Date.now());
      },
      holds: true,
    },
    {
      // Three states the enumeration missed for three rounds, each counted by hand against
      // `dispatch-cli.ts` rather than against the previous round's list — and each time the
      // missing row was where a defect lived.
      name: "an upload whose own bytes cannot be read",
      call: () => ({
        command: "dispatch artifact --issue DSP-1 --name plan --path missing.md",
        argv: ["artifact", "--issue", "DSP-1", "--name", "plan", "--path", "missing.md"],
      }),
      arrange: (s) => clearVerdict(s.dir(), identity()),
      repair: (s) => writeFileSync(join(s.work, "missing.md"), "# Plan\n"),
      holds: true,
    },
    {
      name: "a marker store that cannot be listed",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        chmodSync(join(s.store, "markers"), 0o000);
      },
      repair: (s) => chmodSync(join(s.store, "markers"), 0o700),
      holds: true,
    },
    {
      name: "a session record that cannot be read",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        writeFileSync(join(s.dir(), "askgate", "99999999-0-stray.json"), "{not json");
      },
      repair: (s) => rmSync(join(s.dir(), "askgate", "99999999-0-stray.json"), { force: true }),
      holds: true,
    },
    {
      // The state this whole change exists for: no review ever saw the write, so it posts with
      // a warning, and a subagent's posts too.
      name: "nothing recorded against the write",
      arrange: (s) => clearVerdict(s.dir(), identity()),
      holds: false,
    },
    {
      // Reached only by running the argv the marker is keyed to: minting under one argv and
      // running another leaves `consumeMarker` never opening the marker, so the row printed
      // the previous row's message byte for byte and passed because both posted.
      name: "a file changed after the review",
      call: () => ({
        command: "dispatch comment --issue DSP-1 --body-file body.md",
        argv: ["comment", "--issue", "DSP-1", "--body-file", "body.md"],
      }),
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        // The argv the CLI runs, word for word: the marker is keyed by those words, and an
        // absolute path here against a relative one there is a different key.
        writeFileSync(join(s.work, "body.md"), "reviewed");
        mintMarker(
          s.store,
          {
            argv: ["comment", "--issue", "DSP-1", "--body-file", "body.md"],
            args: { issue: "DSP-1", body: "reviewed" },
            // A digest of bytes the file no longer holds, which is the state: AskGate read it
            // before the change.
            files: [{ flag: "--body-file", operand: "body.md", sha256: "0".repeat(64) }],
            toolCallId: "x",
            outcome: "verdict",
          },
          Date.now()
        );
      },
      holds: true,
    },
  ];

  // The rule the renderer applies and the rule this measures must be one fact: the caveat is
  // carried by a resend when, and only when, following it as a task subagent is refused.
  // Asserting the caveat is simply *present* is what let it spread to four states where it is
  // false — including the unreviewed-post warning, which tells the population this change
  // exists to unblock to route every write through its parent.
  for (const { name, arrange, repair, holds, call } of states) {
    test(`${name}: the caveat is there exactly where a subagent's resend is refused`, async () => {
      // A fresh session per agent kind. Running main first in one session repairs some states
      // under the subagent that follows — `no verdict capability` among them — so a shared
      // session can hide a real refusal behind an ordering.
      const followed = async (who: "main" | "sub") => {
        const s = session();
        try {
          expect((await s.gate("main"))?.block).toBe(true);
          expect(readVerdict(s.dir(), identity()).kind).toBe("refused");
          arrange(s);
          const made = call?.(s) ?? { command: COMMAND, argv: BASE };
          const printed = await s.cli(made.argv);
          expect({ name, who, holds: printed.code === 1 }).toEqual({ name, who, holds });
          const caveat =
            /That is the main agent's to send/.test(printed.out) ||
            /None of that works from a task subagent/.test(printed.out);
          repair?.(s);
          // From here the review allows, so only what the resend's marker binds decides.
          s.allow();
          const again = await resend(s, who, made);
          return { caveat, posted: again.posts === 1 };
        } finally {
          s.stop();
        }
      };
      // The main agent's resend is reviewed afresh and gets a bound marker, so it posts from
      // every one of these states; that is the control that makes the subagent row mean
      // something.
      expect({ name, main: (await followed("main")).posted }).toEqual({ name, main: true });
      const sub = await followed("sub");
      expect({ name, caveat: sub.caveat }).toEqual({ name, caveat: !sub.posted });
    }, 60_000);
  }

  /**
   * The inputs `persistent` reads, each arm of each. The derivation is
   * `!capable || uncertainSession(dir)` on one arm and `said.kind === "lost"` on others, so the
   * suite is the product of the faults that reach those arms with the session conditions those
   * inputs range over — generated, not listed.
   *
   * Four rounds running, a hand-listed suite omitted the one cell where the defect lived: a row
   * that never reached its state, one that minted a different argv, one that built a marker no
   * clock produces, and one that omitted the very arm its round's derivation was written for. A
   * product cannot omit the cell nobody thought of, which is the only property that has
   * mattered here. The product of twelve faults, five session conditions and two modes is the right size for that.
   */
  const conditions: Array<[string, (s: Fixture) => void]> = [
    ["a healthy session", () => {}],
    [
      "no verdict capability",
      (s) => {
        const file = join(
          s.dir(),
          "askgate",
          `${process.pid}-${processStart(process.pid)}-${pidNamespace()}.json`
        );
        const record = JSON.parse(readFileSync(file, "utf8")) as Record<string, unknown>;
        delete record.caps;
        writeFileSync(file, JSON.stringify(record));
      },
    ],
    [
      "a foreign verdict store",
      (s) => {
        const older = join(s.dir(), "askgate-verdicts-v9");
        mkdirSync(older, { recursive: true });
        writeFileSync(join(older, "deadbeef"), "{}");
      },
    ],
    ["a lost verdict", (s) => markVerdictsLost(s.dir(), Date.now())],
    [
      "its own verdict store unreadable",
      (s) => {
        mkdirSync(join(s.dir(), "askgate-verdicts-v2"), { recursive: true });
        chmodSync(join(s.dir(), "askgate-verdicts-v2"), 0o000);
      },
    ],
  ];

  // Mode is one of the caveat's inputs and was held at `block` for every row, so dropping
  // the guard that stops a message announcing a **post** from carrying one left the suite
  // green. The axes are what the function reads, not what anyone enumerated.
  for (const { name, arrange, repair, call } of states)
    for (const [condition, apply] of conditions)
      for (const mode of ["block", "warn"] as const)
        test(`${name} with ${condition}, ${mode} mode`, async () => {
          const held = async (who: "main" | "sub") => {
            const s = session();
            try {
              // Seeded in block, since a warn-mode revise records nothing, then switched to
              // the mode under test before the CLI reads anything.
              expect((await s.gate("main"))?.block).toBe(true);
              s.setMode(mode);
              if (name !== "a recorded refusal of this write") clearVerdict(s.dir(), identity());
              arrange(s);
              apply(s);
              const made = call?.(s) ?? { command: COMMAND, argv: BASE };
              const printed = await s.cli(made.argv);
              // The two caveat sentences themselves, not any mention of a subagent: the
              // record-recovery lead says a subagent never rewrites the record, which is its own
              // true statement and not a caveat on a resend.
              // Which caveat, not whether one: a boolean OR cannot tell the all-session
              // sentence from the one-clause sentence, so transposing them, or rendering the
              // wrong one at every persistent site, left the whole tree green.
              const caveat = /None of that works from a task subagent/.test(printed.out)
                ? "all"
                : /That is the main agent's to send/.test(printed.out)
                  ? "clause"
                  : "none";
              repair?.(s);
              s.allow();
              const again = await resend(s, who, made);
              // A refusal of AskGate's, not any non-zero exit: an upload whose file is missing
              // fails at the request in warn mode, where AskGate let it through.
              return {
                printed: / was not called:/.test(printed.out),
                caveat,
                posted: again.posts === 1,
                out: printed.out,
              };
            } finally {
              for (const locked of [
                "askgate-verdicts-v2",
                join("..", "..", "..", "store", "markers"),
              ])
                if (existsSync(join(s.dir(), locked))) chmodSync(join(s.dir(), locked), 0o700);
              if (existsSync(join(s.store, "markers"))) chmodSync(join(s.store, "markers"), 0o700);
              s.stop();
            }
          };
          const sub = await held("sub");
          // Advice that asks nothing of the agent — tell whoever owns the session — has no
          // resend to caveat, and the rule is about resends. Read from the message itself, so a
          // state that gains a resend later is covered without anyone listing it.
          if (!/\bsend (it|the |this |a write)/i.test(sub.out)) {
            expect({ name, condition, caveat: sub.caveat }).toEqual({
              name,
              condition,
              caveat: "none",
            });
            return;
          }
          // The oracle is the behaviour, not a list. Which caveat is true of a state is
          // settled by following the advice: if the write posts, neither sentence belongs;
          // if this write is refused but a different write of the same session posts, only
          // the marker-bound clause is beyond a subagent; if even a different write is
          // refused, nothing in the message works and the all-session sentence is true.
          //
          // A hand-listed oracle can disagree with the system, and the one that stood here
          // did: it called an unparseable verdict file of this write a session-wide
          // uncertainty, so the row agreed with a message telling the agent two false things.
          // A measured oracle cannot disagree, and a cell nobody enumerated is untested
          // rather than tested against a wrong expectation.
          const persistent = !(await corrected(name, arrange, apply, call, repair, mode));
          if (!sub.printed)
            expect({ name, condition, caveat: sub.caveat }).toEqual({
              name,
              condition,
              caveat: "none",
            });
          else
            expect({ name, condition, caveat: sub.caveat }).toEqual({
              name,
              condition,
              caveat: persistent ? "all" : sub.posted ? "none" : "clause",
            });
        }, 60_000);

  /**
   * Whether a *different* write of this session posts from a subagent, which is what tells the
   * one-clause caveat from the all-session one. Measured in a session of its own, so the row
   * it answers for is not disturbed.
   */
  const corrected = async (
    name: string,
    arrange: (s: Fixture) => void,
    apply: (s: Fixture) => void,
    call: ((s: Fixture) => { command: string; argv: readonly string[] }) | undefined,
    repair: ((s: Fixture) => void) | undefined,
    mode: "block" | "warn"
  ): Promise<boolean> => {
    const s = session();
    try {
      expect((await s.gate("main"))?.block).toBe(true);
      // Before the arrange, never after: `setRecordMode` rewrites the owner record, so a
      // state built by editing that record — a gate with no capabilities — is undone by it.
      s.setMode(mode);
      if (name !== "a recorded refusal of this write") clearVerdict(s.dir(), identity());
      arrange(s);
      apply(s);
      call?.(s);
      repair?.(s);
      s.allow();
      const other = await resend(s, "sub", {
        command: "dispatch comment --issue DSP-1 --body a-different-write",
        argv: ["comment", "--issue", "DSP-1", "--body", "a-different-write"],
      });
      return other.posts === 1;
    } finally {
      if (existsSync(join(s.dir(), "askgate-verdicts-v2")))
        chmodSync(join(s.dir(), "askgate-verdicts-v2"), 0o700);
      if (existsSync(join(s.store, "markers"))) chmodSync(join(s.store, "markers"), 0o700);
      s.stop();
    }
  };

  // The clause a message offers that is *not* a resend of the same write: `REFUSAL_STANDS`
  // tells the agent to send a corrected write, which is a different write and meets no
  // refusal. Every row above follows the same-write resend, which is the clause that is not in
  // question — so swapping the two clauses, which would tell a subagent the corrected write
  // needs its parent and the refused one does not, left the tree green.
  for (const [condition, apply] of conditions)
    test(`a recorded refusal with ${condition}: the corrected write is followed, not the resend`, async () => {
      const s = session();
      try {
        expect((await s.gate("main"))?.block).toBe(true);
        apply(s);
        const printed = await s.cli();
        expect(printed.code).toBe(1);
        const corrected = ["comment", "--issue", "DSP-1", "--body", "corrected-content"];
        s.allow();
        const after = await resend(s, "sub", {
          command: "dispatch comment --issue DSP-1 --body corrected-content",
          argv: corrected,
        });
        // In a healthy session the corrected write is a different write and posts from a
        // subagent, so the caveat must be on the same-write clause alone. Where the session's
        // own uncertainty stands, it covers every write of that session and neither works.
        expect({ condition, posted: after.posts === 1 }).toEqual({
          condition,
          posted: condition === "a healthy session",
        });
        expect({
          condition,
          caveat: /None of that works from a task subagent/.test(printed.out)
            ? "all"
            : /That is the main agent's to send/.test(printed.out)
              ? "clause"
              : "none",
        }).toEqual({ condition, caveat: condition === "a healthy session" ? "clause" : "all" });
        // By position, not only by name: swapping the two clauses puts the caveat on the one
        // that works and leaves the refused resend reading as free, which every assertion
        // above passes unchanged.
        if (condition === "a healthy session") {
          const flat = printed.out.replace(/\s+/g, " ");
          expect(flat).toContain("replaces the refusal. That is the main agent's to send");
          expect(flat).not.toContain("meets no refusal. That is the main agent's to send");
        }
      } finally {
        for (const locked of ["askgate-verdicts-v2"])
          if (existsSync(join(s.dir(), locked))) chmodSync(join(s.dir(), locked), 0o700);
        s.stop();
      }
    }, 60_000);

  // The one state no resend clears, for either kind, and the reason it is worse than a refusal:
  // `askgateState` answers `broken`, so no marker is ever reached, and each attempt spends a
  // whole review before being refused anyway. The agent page promised a resend cleared "any of
  // them"; it now says not to resend this one.
  test("an unreadable record is refused however often it is resent, and each resend costs a review", async () => {
    for (const who of ["main", "sub"] as const) {
      const s = session("allow");
      try {
        // A stray owner file, not this owner's own: `writeAskgateRecord` removes only the
        // files it can parse, so the gate rewriting its own record leaves this one, and
        // `askgateState` answers `broken` for the session from then on.
        const record = join(s.dir(), "askgate", "99999999-0-stray.json");
        writeFileSync(record, "{not json");
        for (let attempt = 1; attempt <= 3; attempt += 1) {
          const again = await resend(s, who);
          expect({ who, attempt, code: again.code, posts: again.posts }).toEqual({
            who,
            attempt,
            code: 1,
            posts: 0,
          });
        }
        // And it survives the gate writing its record again, since only an owner file that
        // parses is replaced.
        expect(readFileSync(record, "utf8")).toBe("{not json");
      } finally {
        s.stop();
      }
    }
  }, 30_000);

  // The failure control: with nothing recorded against the write, both kinds post. Without it
  // the rows above would pass on a CLI that refused everything.
  test("with nothing recorded against it, both kinds post", async () => {
    for (const who of ["main", "sub"] as const) {
      const s = session("allow");
      try {
        const posted = await resend(s, who);
        expect({ who, posts: posted.posts, code: posted.code }).toEqual({
          who,
          posts: 1,
          code: 0,
        });
      } finally {
        s.stop();
      }
    }
  }, 30_000);
});
