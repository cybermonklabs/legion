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
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import {
  clearVerdict,
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
  readonly gate: (who: "main" | "sub") => Promise<{ block?: boolean } | undefined>;
  /** The real CLI, against the fake Dispatch. */
  readonly cli: () => Promise<{ code: number; out: string; posts: number }>;
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
    gate: (who: "main" | "sub") =>
      handlers.get("tool_call")?.(
        {
          type: "tool_call",
          toolName: "bash",
          toolCallId: `t-${Math.random()}`,
          input: { command: COMMAND },
        },
        who === "main" ? main : sub
      ) as Promise<{ block?: boolean } | undefined>,
    /** The real CLI, against the fake Dispatch. */
    cli: async () => {
      let out = "";
      const before = sent.length;
      const code = await runDispatchCli(BASE, env, {
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
async function resend(s: Fixture, who: "main" | "sub") {
  const hook = await s.gate(who);
  if (hook?.block === true) return { code: 1, out: "held at the gate", posts: 0 };
  return s.cli();
}

describe("every remedy the CLI prints, followed", () => {
  /** The six states that print a resend, each arranged on a live session. */
  const states: Array<[string, (s: Fixture) => void]> = [
    [
      "a recorded refusal of this write",
      () => {
        // Arranged by the review itself, in the test below.
      },
    ],
    [
      "a marker of this call that cannot be read",
      (s) => {
        clearVerdict(s.dir(), identity());
        const file = mintMarker(
          s.store,
          { argv: BASE, args: ARGS, files: [], toolCallId: "x", outcome: "verdict" },
          Date.now()
        );
        writeFileSync(file, "{not-json");
      },
    ],
    [
      "a marker bound to other arguments",
      (s) => {
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
    ],
    [
      "no verdict capability",
      (s) => {
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
    ],
    [
      "a foreign verdict store",
      (s) => {
        clearVerdict(s.dir(), identity());
        const older = join(s.dir(), "askgate-verdicts-v9");
        mkdirSync(older, { recursive: true });
        writeFileSync(join(older, "deadbeef"), "{}");
      },
    ],
    [
      "a lost verdict",
      (s) => {
        clearVerdict(s.dir(), identity());
        markVerdictsLost(s.dir(), Date.now());
      },
    ],
  ];

  for (const [state, arrange] of states) {
    for (const who of ["main", "sub"] as const) {
      test(`${state}, resent by ${who === "main" ? "the main agent" : "a task subagent"}`, async () => {
        const s = session();
        try {
          // The review runs and refuses, which both records the verdict and arranges the first
          // state. Every other state clears it and puts its own in place.
          expect((await s.gate("main"))?.block).toBe(true);
          expect(readVerdict(s.dir(), identity()).kind).toBe("refused");
          arrange(s);
          // The write is held, and the refusal prints its advice.
          const held = await s.cli();
          expect({ state, who, code: held.code }).toEqual({ state, who, code: 1 });
          expect(held.out).toContain("a task subagent's literal write");
          // Now follow it. The review allows from here, so only the marker it mints decides.
          s.allow();
          const first = await resend(s, who);
          expect({ state, who, posted: first.posts }).toEqual({
            state,
            who,
            posted: who === "main" ? 1 : 0,
          });
          // And a subagent's second attempt is no better than its first, which is the cost the
          // caveat exists to spare it.
          if (who === "sub") {
            const second = await resend(s, who);
            expect({ state, posts: second.posts, code: second.code }).toEqual({
              state,
              posts: 0,
              code: 1,
            });
          }
        } finally {
          s.stop();
        }
      }, 30_000);
    }
  }

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
