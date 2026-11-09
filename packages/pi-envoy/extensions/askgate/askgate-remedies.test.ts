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
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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
  const env = {
    PATH: process.env.PATH,
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
      name: "a marker past its age",
      arrange: (s) => {
        clearVerdict(s.dir(), identity());
        mintMarker(
          s.store,
          { argv: BASE, args: null, files: [], toolCallId: "x", outcome: "verdict" },
          Date.now() - 3 * MARKER_MAX_AGE_MS
        );
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
          const caveat = printed.out.includes("a task subagent's literal write");
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
