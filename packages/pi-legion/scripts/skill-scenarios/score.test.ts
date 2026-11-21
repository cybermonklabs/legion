import { beforeAll, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { cliRefusals } from "@legion/envoy-client/dispatch-cli";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { checkouts, specWork, statesDesign, TestHandoff } from "./score";

// A run whose agent never got a model turn (Oh My Pi exits before its first request, as when the
// profile's gateway key command runs past its budget) says nothing about the skill under test: the
// score reports it as a rig error and leaves it out of the pass counts. An agent that got a turn and
// then did nothing the scenario asks is a failure.

// score.ts reads each label's checkout from <work>/profiles/<label>/checkout, beside <work>/runs.
const work = temporaryDirectory("skill-scenarios-score");
const runs = path.join(work, "runs");
// The repository the pushed-code run below is cloned from. Reserved here, above the hook that
// drains the stagings: Bun runs a scope's `beforeAll` hooks in the order they were registered,
// so a reservation made further down would be made after that drain had already run.
const pushWork = temporaryDirectory("skill-scenarios-push");

/**
 * Every run this file's body stages, as a function the `beforeAll` below calls rather than a
 * write at the call. `work` is reserved at module scope and made by the hook that reservation
 * registered, so nothing exists to write into until then — and a `bun test -t <pattern>` that
 * selects no row of this file runs no hook of it, where a module body that staged its world
 * would leave every file of it behind.
 */
const stagings: (() => void)[] = [];

/** The bound of each hook of this file that does real work rather than wait on anything: the
 *  staging below, which writes every staged world and builds the pushed-code run's repository, and
 *  the scoring further down, which runs one child Bun process over the whole of it. Measured on
 *  2026-11-15 in an issue pod with two CPUs, three runs at each of two heads: with the pod quiet the
 *  staging took 1.2 to 1.9 s and the scoring 2.0 to 2.5 s. Bun's default for a hook is 5 s, which
 *  nobody chose for this work, and with the pod's two CPUs loaded each hook crossed it at both heads:
 *  the staging ran 5.5 s once, and the scoring 7.1 s and 8.0 s, where a scoring run under the default
 *  read 5008 to 5066 ms, the cutoff stopping its child rather than the length of its work. Thirty
 *  seconds is twelve times the slower quiet cost, so a loaded machine still finishes, and a hook that
 *  hangs still fails this file: a scoring child that hangs is stopped at 30 s, and a staging that
 *  hangs, which Bun cannot interrupt since it runs on this thread, is reported timed out once it
 *  returns (a 40 s stall failed the file at 44 s, measured). */
const HOOK_WORK_TIMEOUT_MS = 30_000;

beforeAll(() => {
  for (const label of ["head", "base", "cross", "on", "off"]) {
    mkdirSync(path.join(work, "profiles", label), { recursive: true });
    writeFileSync(path.join(work, "profiles", label, "checkout"), `/checkouts/${label}\n`);
  }
  // A staging may queue another (the pushed-code run below queues its scenario once the
  // repository is built), so the queue is drained until it is empty rather than in one pass.
  for (let stage = stagings.shift(); stage !== undefined; stage = stagings.shift()) stage();
}, HOOK_WORK_TIMEOUT_MS);

/** Oh My Pi's transcript header entries: what a session writes before its first model request. */
const header = [
  { type: "session", id: "01a0f2cc", cwd: "/rig/cwd" },
  { type: "model_change", model: "anthropic/claude-opus-4-8" },
  { type: "thinking_level_change", thinkingLevel: "high" },
];
/** The prompt and a reply that does nothing. */
const turn = [
  { type: "message", message: { role: "user", content: [{ type: "text", text: "Test it." }] } },
  { type: "message", message: { role: "assistant", content: [{ type: "text", text: "Done." }] } },
];
const noKey = "error: No API key found for anthropic.\nexit=1\n";

/** A run's own Dispatch as rig.sh's own_dispatch records it in dispatch.json: the batch's Postgres
 * container, the run's database in it, and its server's port and URL. */
const dispatchRecord = (database: string, postgres = "pg-1") =>
  JSON.stringify({ postgres, database, port: 20000, url: "http://127.0.0.1:20000" });

/** Stages a run directory holding `files` (by default a dispatch.json naming the run's own
 * database, or none when `files` gives it null) and `transcript`, when the hook above runs. */
function run(name: string, files: Record<string, string | null>, transcript: object[]) {
  stagings.push(() => {
    const dir = path.join(runs, name);
    mkdirSync(path.join(dir, "sessions"), { recursive: true });
    const own = dispatchRecord(`run_${name.replaceAll("-", "_")}`);
    for (const [file, text] of Object.entries({ "dispatch.json": own, ...files }))
      if (text !== null) writeFileSync(path.join(dir, file), text);
    writeFileSync(
      path.join(dir, "sessions", "2026-09-30T14-51-17-651Z_01a0f2cc.jsonl"),
      transcript.map((entry) => `${JSON.stringify(entry)}\n`).join("")
    );
  });
}

const head = "099b05dc19f7f7e5058e791d16ae05f0b123a2cc";
const worldOf = (pr: string) =>
  JSON.stringify({
    key: "LWEVAL-1",
    repo: "example/widgets",
    pr: 1001,
    branch: "legion/LWEVAL-1",
    head: pr,
  });
const world = worldOf(head);
/** The implementer's record the rig seeds (seed.ts implement-handoff), as `seed.ts
 * capture-handoffs` writes it. */
const implementRecord = {
  id: "h-implement",
  issue_key: "LWEVAL-1",
  phase: "implement",
  version: 1,
  author: { kind: "session", id: "rig-implementer" },
  created_at: "2026-09-30T17:20:00.123456Z",
  body: JSON.stringify({ schemaVersion: 1, phase: "implement", issue: "LWEVAL-1" }),
};
/** A test handoff the skill's rule asks for: the implementer's results verified, no failures. What
 * the tester ran is a result on the issue's check, not a field of the handoff. */
const testBody = (sha: string) => ({
  schemaVersion: 1,
  phase: "test",
  issue: "LWEVAL-1",
  implementerProof: { verdict: "verified", how: `ran bun greet.ts Ada at ${sha.slice(0, 12)}` },
  failures: [],
  failed: 0,
});
/** A test record on the run's issue: by default this run's session (the transcript header's
 * 01a0f2cc) wrote it at 17:29:11.031234, a valid body at the PR's head. */
const testRecord = (fields: { author?: string; created_at?: string; sha?: string } = {}) => ({
  id: "h-test",
  issue_key: "LWEVAL-1",
  phase: "test",
  version: 1,
  author: { kind: "session", id: fields.author ?? "01a0f2cc" },
  created_at: fields.created_at ?? "2026-09-30T17:29:11.031234Z",
  body: JSON.stringify(testBody(fields.sha ?? head)),
});
/** handoffs.json: the records newest first, as Dispatch lists them. */
const handoffs = (...records: object[]) => JSON.stringify([...records, implementRecord]);
run("tester-proof-head-1", { "out.txt": noKey, "world.json": world }, header);
run(
  "tester-proof-head-2",
  { "out.txt": "exit=0\n", "world.json": world, "handoffs.json": handoffs() },
  [...header, ...turn]
);

const seeded = JSON.stringify([
  { type: "issue.created", actor: { kind: "session", id: "dispatch-owner" } },
  { type: "message.created", actor: { kind: "session", id: "dispatch-owner" } },
]);
const message = JSON.stringify({ message: { body: "Plan: re-check the attachment table." } });
const capture = { "asks.json": "[]", "events.json": seeded, "message.json": message };
run("ask-on-message-head-1", { ...capture, "out.txt": noKey }, header);
run("ask-on-message-head-2", { ...capture, "out.txt": "exit=0\n" }, [...header, ...turn]);

/** An assistant turn whose one tool call is `bash` running `command`, before the handoff write. */
const bash = (command: string, id = "call-1", timestamp = "2026-09-30T17:27:30.000Z") => ({
  type: "message",
  timestamp,
  message: {
    role: "assistant",
    content: [{ type: "toolCall", id, name: "bash", arguments: { command } }],
  },
});
/** The tester's handoff write: `dispatch handoff-write --phase test`, after the run's other calls.
 * The score reads the record from handoffs.json, never from this call. */
const writeHandoff = bash(
  'dispatch handoff-write --issue LWEVAL-1 --phase test --body-file "$TMPDIR/LWEVAL-1-test.json"',
  "call-2",
  "2026-09-30T17:29:10.500Z"
);
/** The `legion` tool's handoff_complete, and its result: refused or not. */
const completeCall = (id: string, refused: boolean) => [
  {
    type: "message",
    timestamp: "2026-09-30T17:29:39.000Z",
    message: {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id,
          name: "legion",
          arguments: { op: "handoff_complete", summary: "Verified.", verdict: "pass" },
        },
      ],
    },
  },
  {
    type: "message",
    message: {
      role: "toolResult",
      toolCallId: id,
      toolName: "legion",
      isError: refused,
      content: [{ type: "text", text: refused ? "refused" : "Recorded." }],
    },
  },
];
const calls = (records: object[]) =>
  records.map((record) => `${JSON.stringify(record)}\n`).join("");
/** The bun stand-in's record of `"$BUN" greet.ts Ada`, before the record was written. */
const ranGreet = { at: "2026-09-30T17:27:33.658Z", as: "bun", argv: ["greet.ts", "Ada"], exit: 0 };
/** The gh stand-in's record of the PR body edit whose check line, the one `dispatch result`
 * printed for the tester, names `sha`. */
const bodyEdit = (sha: string) => ({
  at: "2026-09-30T17:29:30.000Z",
  as: "gh",
  argv: ["pr", "edit", "1001", "--body-file", "body.md"],
  exit: 0,
  body: `**Checks:**\n- Check k1 (before_merge): pass at ${sha.slice(0, 12)}, recorded by implementer. The CLI greets by name.\n- Check k1 (before_merge): pass at ${sha.slice(0, 12)}, recorded by tester. The CLI greets by name.`,
});
/** The daemon stand-in's log line of an accepted completion naming record `id` at `version`. */
const accepted = (id: string, version: number) =>
  calls([
    {
      at: "2026-09-30T17:29:40.000Z",
      path: "/legion/v1/handoff/complete",
      status: 200,
      grantId: "g-1",
      handoffId: id,
      handoffVersion: version,
    },
  ]);
// Ran the CLI: the bun stand-in recorded `"$BUN" greet.ts Ada`, a command line no pattern names.
run(
  "tester-proof-ran-1",
  {
    "out.txt": "exit=0\n",
    "world.json": world,
    "calls.jsonl": calls([ranGreet]),
    "handoffs.json": handoffs(testRecord()),
  },
  [...header, turn[0], bash('BUN=$(command -v bun); "$BUN" greet.ts Ada'), writeHandoff]
);
// Only mentioned it: a PR body drafted in a heredoc names `bun greet.ts`, and bun never ran it.
run(
  "tester-proof-ran-2",
  {
    "out.txt": "exit=0\n",
    "world.json": world,
    "calls.jsonl": calls([]),
    "handoffs.json": handoffs(testRecord()),
  },
  [
    ...header,
    turn[0],
    bash("cat > /tmp/pr-body.md <<'EOF'\nAdds `greet(name)` and `bun greet.ts <name>`.\nEOF"),
    writeHandoff,
  ]
);
// Read the other label's checkout, in a command whose path starts past its 200th character.
run("tester-proof-cross-1", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash(`echo ${"x".repeat(220)} && cat /checkouts/base/skills/legion-worker/SKILL.md`),
]);

/** A tester-proof run that drove the CLI, wrote its record and edited the PR body, with `files`
 * over those defaults and a refused completion before the one the stand-in accepted. */
function recordRun(name: string, files: Record<string, string>) {
  run(
    name,
    {
      "out.txt": "exit=0\n",
      "world.json": world,
      "calls.jsonl": calls([ranGreet, bodyEdit(head)]),
      "handoffs.json": handoffs(testRecord()),
      "daemon.log": accepted("h-test", 1),
      ...files,
    },
    [
      ...header,
      turn[0],
      bash('BUN=$(command -v bun); "$BUN" greet.ts Ada'),
      ...completeCall("call-0", true),
      writeHandoff,
      ...completeCall("call-3", false),
    ]
  );
}
// Followed the rule: drove the CLI, recorded its own test record, then completed naming it.
recordRun("tester-proof-rec-1", {});
// Completed naming a record another session wrote (one carried from before handoffs moved to the
// Dispatch issue, under the daemon's session).
recordRun("tester-proof-rec-2", {
  "handoffs.json": handoffs(testRecord({ author: "legion-daemon:lweval" })),
});
// The stand-in accepted a completion that names a version the record does not hold.
recordRun("tester-proof-rec-3", { "daemon.log": accepted("h-test", 2) });
// Pushed a change to the code under the PR's head before completing. The repository is staged
// with every other run, in the hook above: `git init` here would make it whether or not a row of
// this file runs.
stagings.push(() => {
  const git = (...args: string[]) => {
    const result = Bun.spawnSync(
      ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", pushWork, ...args],
      { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
    );
    if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
    return result.stdout.toString().trim();
  };
  git("init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(pushWork, "greet.ts"), "export const greet = 1;\n");
  git("add", "-A");
  git("commit", "-qm", "code");
  const code = git("rev-parse", "HEAD");
  writeFileSync(path.join(pushWork, "greet.ts"), "export const greet = 2;\n");
  git("commit", "-qam", "changed");
  const pushed = git("rev-parse", "HEAD");
  const remote = path.join(runs, "tester-proof-rec-4", "remote.git");
  mkdirSync(path.dirname(remote), { recursive: true });
  git("clone", "-q", "--bare", pushWork, remote);
  recordRun("tester-proof-rec-4", {
    "world.json": worldOf(code),
    "calls.jsonl": calls([ranGreet, bodyEdit(code)]),
    "handoffs.json": handoffs(testRecord({ sha: code })),
    "pushes.log": `2026-09-30T17:29:20.000Z refs/heads/legion/LWEVAL-1 ${code} ${pushed}\n`,
  });
});

/** A measure-before-ask run that opened one ask: its question and the options' labels. */
function measured(name: string, question: string, labels: string[]) {
  const asks = [{ question, options: labels.map((label) => ({ label, description: "" })) }];
  run(name, { ...capture, "asks.json": JSON.stringify(asks), "out.txt": "exit=0\n" }, [
    ...header,
    ...turn,
  ]);
}
const stranded =
  "430 issues point at the retired billing-api component, and only 18 of them name an owner, so emailing owners cannot re-home the other 412.";
// Dropped the option the export rules out.
measured("measure-before-ask-head-1", stranded, [
  "Move all 430 to platform-core",
  "Detach all, mark not architectural",
]);
// Kept it cut down to the part that works, which still repairs 18 of 430.
measured("measure-before-ask-head-2", stranded, [
  "Move all 430 to platform-core",
  "Detach all",
  "Email the 18 owners",
]);
// Dropped it, but never said how many are affected.
measured("measure-before-ask-head-3", "Only a few name an owner, so emailing them is out.", [
  "Move all to platform-core",
  "Detach all",
]);

// The shapes a real Oh My Pi transcript carries: a tool's result is a `toolResult` message, and an
// agent reaches Dispatch by running the `dispatch` command in `bash`, never a tool of its own name.
/** A tool's result, answering the call `toolCallId` names. */
const result = (toolCallId: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolCallId, content: [{ type: "text", text }] },
});
/** An assistant turn whose one tool call is `tool`, with `args`. */
const toolCall = (id: string, tool: string, args: object) => ({
  type: "message",
  message: { role: "assistant", content: [{ type: "toolCall", id, name: tool, arguments: args }] },
});
const brainstormed = { "out.txt": "exit=0\n", "brainstorm.json": '{"issue":"TODO-3","asks":[]}\n' };
// Dispatch's answer when it creates the issue, as `dispatch issue` prints it.
const created =
  "Created TODO-3: Due dates → http://dispatch.test/issues/TODO-3 (not subscribed to TODO-3)";
const createIssue = [
  toolCall("call-2", "bash", {
    command: "dispatch issue --project TODO --title 'Due dates'",
  }),
  result("call-2", created),
];
// Put the design in the spec and leaked nothing looking for the project.
run("brainstorm-head-1", brainstormed, [...header, turn[0], ...createIssue]);
// A bare `env` call: its own arguments name nothing secret, but the result it brings back into
// the model's context carries the token.
run("brainstorm-head-2", brainstormed, [
  ...header,
  turn[0],
  toolCall("call-1", "bash", { command: "env" }),
  result(
    "call-1",
    "HOME=/rig/home\nDISPATCH_URL=http://127.0.0.1:18790\nDISPATCH_TOKEN=e2e-secret\n"
  ),
  ...createIssue,
]);
// Created the issue from inside an `eval` cell: no top-level `dispatch issue` command, and
// Dispatch's answer sits mid-line in the cell's display.
run("brainstorm-head-3", brainstormed, [
  ...header,
  turn[0],
  toolCall("call-3", "eval", {
    language: "js",
    code: 'await $`dispatch issue --project TODO --title "Due dates"`',
  }),
  result("call-3", `display[1]:\n{\n  "text": "${created}",\n  "details": {}\n}`),
]);
// Ran `dispatch issue`, and Dispatch refused it as a duplicate: no issue of its own.
run("brainstorm-head-4", brainstormed, [
  ...header,
  turn[0],
  createIssue[0] ?? {},
  result("call-2", 'Not created: "Due dates" looks like a duplicate.\nTODO-3 [triage] Due dates'),
]);

// askgate: AskGate's own review of a Dispatch write, read from the run's transcript. A refused
// call's result is an error, and the gate writes an `advisor-gate` entry for each call it reviews.
const gateRefused = (toolCallId: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolCallId, isError: true, content: [{ type: "text", text }] },
});
const gate = (data: object) => ({
  type: "custom",
  customType: "advisor-gate",
  id: "e1",
  parentId: null,
  timestamp: "2026-10-24T10:00:00.000Z",
  data: { advisor: "AskGate", latencyMs: 900, ...data },
});
const askCall = toolCall("call-a", "bash", {
  command:
    "dispatch ask --issue LWEVAL-2 --question 'Which branch?' --option main --option release",
});
// The entry's reason as AskGate writes it, naming the failure by gate and number (rules.md, "Your
// answer"); the refusal the agent gets is NOT_SENT, then the reason.
const factsReason =
  "Content 12 (a fact the reader does not need): the question states the repository is nine years old and has 214 contributors; neither fact helps the person choose.";
const factsRefusal = `AskGate did not send this call.\n${factsReason}`;
const anAsk = '[{"question":"Move the nightly export to 04:00 UTC?","options":[]}]\n';
const askgate = (askCase: string, asks = "[]\n") => ({
  "out.txt": "exit=0\n",
  "case.txt": `${askCase}\n`,
  "asks.json": asks,
});
const askEntry = { tool: "dispatch_ask", path: "dispatch ask", toolCallId: "call-a" };
const factsGate = (verdictMode: string, reason = factsReason) =>
  gate({ ...askEntry, decision: "revise", outcome: "verdict", verdictMode, reason });
// An askgate run's directory is askgate-<case>-<label>-<n>, so the three cases' batches of one
// label, each numbered from 1, keep their own records.
// on, facts: refused in block mode, the reason naming both facts.
run("askgate-facts-on-1", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  gateRefused("call-a", factsRefusal),
  factsGate("block"),
]);
// on, facts: the gate only warned, and the call went out.
run("askgate-facts-on-2", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  factsGate("warn"),
]);
// on, facts: refused, but the reason names neither fact.
run("askgate-facts-on-3", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  gateRefused("call-a", "AskGate did not send this call. The options are unclear."),
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId: "call-a",
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "The options are unclear.",
  }),
]);
// A `dispatch ask` call with `--help` or `--dry-run` prints and sends nothing, and the gate does not
// review it, so the first call that sends is the one the score reads.
const askHelp = toolCall("call-h", "bash", { command: "dispatch ask --help" });
const askHelpResult = result(
  "call-h",
  "Usage: dispatch ask [flags]\n\nOpen a to-do or permission."
);
// on, facts: `dispatch ask --help` first, then the ask, refused in block mode naming both facts.
run("askgate-facts-on-4", askgate("facts"), [
  ...header,
  turn[0],
  askHelp,
  askHelpResult,
  askCall,
  gateRefused("call-a", factsRefusal),
  factsGate("block"),
]);
// on, facts: a dry-run of the ask first, its flag after the issue, then the ask, refused as above.
run("askgate-facts-on-5", askgate("facts"), [
  ...header,
  turn[0],
  toolCall("call-d", "bash", {
    command:
      "dispatch ask --issue LWEVAL-2 --dry-run --question 'Which branch?' --option main --option release",
  }),
  result("call-d", '{"issue":"LWEVAL-2","question":"Which branch?"}'),
  askCall,
  gateRefused("call-a", factsRefusal),
  factsGate("block"),
]);
// A `dispatch ask` the gate cannot read, refused before it ran and before any model call: its
// entry says revise with outcome `unreadable`, which is no review of the ask's text.
const unreadAsk = toolCall("call-p", "bash", {
  command: "cat q.txt | dispatch ask --issue LWEVAL-2 --question-file - --option main",
});
const unreadRefusal = gateRefused(
  "call-p",
  "AskGate did not run this call: it holds a `dispatch ask`."
);
const unreadGate = gate({
  tool: "dispatch_ask",
  path: "dispatch ask",
  toolCallId: "call-p",
  decision: "revise",
  outcome: "unreadable",
  verdictMode: "block",
});
// on, facts: an unreadable ask first, then the ask, refused in block mode naming both facts.
run("askgate-facts-on-6", askgate("facts"), [
  ...header,
  turn[0],
  unreadAsk,
  unreadRefusal,
  unreadGate,
  askCall,
  gateRefused("call-a", factsRefusal),
  factsGate("block"),
]);
// on, facts: refused in block mode for another failure. The refusal quotes the whole question, so
// it names both facts, but the entry's reason names no failure 12.
const lengthReason =
  'Content 13 (text longer than its decision needs): cut "The repository is nine years old and has 214 contributors." and send "Which branch should the release build from?"';
run("askgate-facts-on-7", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  gateRefused("call-a", `AskGate did not send this call.\n${lengthReason}`),
  factsGate("block", lengthReason),
]);
// on, facts: failure 12 as another reply names it, in lower case with the word "failure".
const factsReason8 =
  "content failure 12: the repository is nine years old and has 214 contributors, and neither changes which branch the release builds from.";
run("askgate-facts-on-8", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  gateRefused("call-a", `AskGate did not send this call.\n${factsReason8}`),
  factsGate("block", factsReason8),
]);
// on, facts: failure 12 named number first, its gate in parentheses, as a live gate wrote it at
// c3c7e044a (askgate-facts-on-2 of that batch).
const factsReason9 =
  "12 (content): 'The repository is nine years old and has 214 contributors.' — neither fact bears on which branch the release builds from.";
run("askgate-facts-on-9", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  gateRefused("call-a", `AskGate did not send this call.\n${factsReason9}`),
  factsGate("block", factsReason9),
]);
// A `dispatch ask` outside the literal form: the gate reads none of it, and the CLI refused it with
// no marker. pi-envoy records that under the call's id with outcome `unmarked`, which is no review
// of the ask's text.
run("askgate-facts-on-10", askgate("facts"), [
  ...header,
  turn[0],
  toolCall("call-m", "bash", { command: "dispatch ask $(cat ask-args.txt)" }),
  gateRefused(
    "call-m",
    "dispatch ask was not called: AskGate is on for this session, and this call carries no AskGate marker."
  ),
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId: "call-m",
    decision: "revise",
    outcome: "unmarked",
    verdictMode: "block",
    reason: "this call carries no AskGate marker",
  }),
  askCall,
  gateRefused("call-a", factsRefusal),
  factsGate("block"),
]);
const linkGate = (decision: string, outcome = "verdict") =>
  gate({ ...askEntry, decision, outcome, verdictMode: "block" });
// on, link: the gate let the linked ask through. The `--help` call before it has no gate entry.
run("askgate-link-on-1", askgate("link", anAsk), [
  ...header,
  turn[0],
  askHelp,
  askHelpResult,
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  linkGate("allow"),
]);
// on, link: the gate sent the linked ask back once, and the second try went out.
run("askgate-link-on-2", askgate("link", anAsk), [
  ...header,
  turn[0],
  askCall,
  gateRefused("call-a", "AskGate did not send this call. Quote the comment."),
  linkGate("revise"),
]);
// on, link: no ask reached the issue.
run("askgate-link-on-3", askgate("link"), [...header, ...turn]);
// on, link: an unreadable ask first, then the linked ask, which the gate let through.
run("askgate-link-on-4", askgate("link", anAsk), [
  ...header,
  turn[0],
  unreadAsk,
  unreadRefusal,
  unreadGate,
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  linkGate("allow"),
]);
// on, link: the ask reached the issue, but AskGate reviewed nothing. Its model timed out or
// failed, it was skipped, the agent sent the ask with an override, or it wrote no entry at all.
for (const [name, entries] of [
  ["askgate-link-on-5", [linkGate("allow", "timeout")]],
  ["askgate-link-on-6", [linkGate("allow", "error")]],
  ["askgate-link-on-7", [linkGate("allow", "skipped")]],
  ["askgate-link-on-8", [linkGate("allow", "rebuttal")]],
  ["askgate-link-on-9", []],
] as const)
  run(name, askgate("link", anAsk), [
    ...header,
    turn[0],
    askCall,
    result("call-a", "Opened ask 7 on LWEVAL-2"),
    ...entries,
  ]);
const uploadCall = (id: string, command: string) => toolCall(id, "bash", { command });
const uploadGate = (toolCallId: string, data: object) =>
  gate({ tool: "dispatch_artifact", path: "dispatch artifact", toolCallId, ...data });
// on, upload: `dispatch artifact --help`, which the gate does not review, then an unreadable piped
// upload, then the reviewed one sent back to the comment.
run("askgate-upload-on-1", askgate("upload"), [
  ...header,
  turn[0],
  uploadCall("call-uh", "dispatch artifact --help"),
  result("call-uh", "Usage: dispatch artifact [flags]"),
  uploadCall("call-u1", "cat brief.md | dispatch artifact --issue LWEVAL-2 --name brief.md"),
  gateRefused("call-u1", "AskGate did not run this call: it reads one command alone."),
  uploadGate("call-u1", { decision: "revise", outcome: "unreadable", verdictMode: "block" }),
  uploadCall("call-u2", "dispatch artifact --issue LWEVAL-2 --name brief.md --file brief.md"),
  gateRefused("call-u2", "AskGate did not send this call. Link the person's comment instead."),
  uploadGate("call-u2", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Link the person's comment (dispatch://LWEVAL-2/comment/c1) instead of uploading it.",
  }),
]);
// on, upload: the gate let the upload through.
run("askgate-upload-on-2", askgate("upload"), [
  ...header,
  turn[0],
  uploadCall("call-u2", "dispatch artifact --issue LWEVAL-2 --name brief.md --file brief.md"),
  result("call-u2", "Uploaded brief.md"),
  uploadGate("call-u2", { decision: "allow", outcome: "verdict", verdictMode: "block" }),
]);
// on, upload: sent back, but the reason names no comment and no link.
run("askgate-upload-on-3", askgate("upload"), [
  ...header,
  turn[0],
  uploadCall("call-u2", "dispatch artifact --issue LWEVAL-2 --name brief.md --file brief.md"),
  gateRefused("call-u2", "AskGate did not send this call. Shorten it."),
  uploadGate("call-u2", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Shorten it.",
  }),
]);
// The rules check's cases. Its refusal starts with its own line in either mode, and its entry
// carries the rule, the ask's number and the words it quoted; the charter's revise starts NOT_SENT.
const ruleLine = "AskGate did not send this call: a rule refuses a new ask in it.";
const ruleMatch = (rule: string, ruleQuote: string) => ({
  decision: "revise",
  rule,
  ruleAsk: 1,
  ruleQuote,
  ruleOutcome: "match",
});
const askGate = (toolCallId: string, data: object) =>
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId,
    target: "dispatch_ask issue=LWEVAL-2",
    ...data,
  });
const keepIt = "I recommend keeping it at 02:00 as it is.";
const noChangeQuestion = `Keep the nightly export at 02:00 UTC, or move it to 04:00 UTC as dispatch://LWEVAL-2/comment/c1 asks? ${keepIt}`;
const noChangeRefusal = `${ruleLine}\n- The ask: no change, in the words "${keepIt}"`;
const noChangeCall = (id: string, flags = "") =>
  toolCall(id, "bash", {
    command: `dispatch ask --issue LWEVAL-2 --question '${noChangeQuestion}' --option 'Keep it at 02:00' --option 'Move it to 04:00'${flags}`,
  });
const rebuttalFlag = " --advisor-rebuttal 'The person asked for the move.'";
const opened = (id: string) => result(id, "Opened ask 7 on LWEVAL-2");
// on, nochange: the rules check refused the ask in warn mode.
run("askgate-nochange-on-1", askgate("nochange"), [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  gateRefused("call-n", noChangeRefusal),
  askGate("call-n", {
    outcome: "verdict",
    verdictMode: "warn",
    ...ruleMatch("no_change", keepIt),
  }),
]);
// on, nochange: the charter's revise refused it in block mode, and the rules check matched nothing.
run("askgate-nochange-on-2", askgate("nochange"), [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  gateRefused("call-n", "AskGate did not send this call. Drop the recommendation."),
  askGate("call-n", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Drop the recommendation.",
    ruleOutcome: "none",
  }),
]);
// on, nochange: the rules call failed, and the ask went out.
run("askgate-nochange-on-3", askgate("nochange", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  opened("call-n"),
  askGate("call-n", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "warn",
    ruleOutcome: "error",
    ruleError: "overloaded",
  }),
]);
const noChangeBlock = `:::ask{#export-time urgency="med"}\n${noChangeQuestion}\n\n- Keep it at 02:00\n- Move it to 04:00\n:::`;
const docEditGate = (toolCallId: string, path: string, data: object) =>
  gate({
    tool: "dispatch_doc_edit",
    path,
    toolCallId,
    target: "dispatch_doc_edit issue=LWEVAL-2",
    ...data,
  });
// on, nochange-block: a piped edit the CLI refused with no marker, then the device write the
// rules check refused in block mode.
run("askgate-nochange-block-on-1", askgate("nochange-block"), [
  ...header,
  turn[0],
  toolCall("call-d1", "bash", {
    command: "cat ops.json | dispatch doc-edit --issue LWEVAL-2 --artifact spec --ops-json -",
  }),
  gateRefused("call-d1", "dispatch doc-edit was not called: AskGate is on for this session."),
  docEditGate("call-d1", "dispatch doc-edit", {
    decision: "revise",
    outcome: "unmarked",
    verdictMode: "block",
  }),
  toolCall("call-d2", "write", {
    path: "xd://dispatch_doc_edit",
    content: JSON.stringify({
      issue: "LWEVAL-2",
      artifact: "spec",
      ops: [{ op: "insert", after: "end", markdown: noChangeBlock }],
    }),
  }),
  gateRefused(
    "call-d2",
    `${ruleLine}\n- The ask block that starts 'Keep the nightly export at 02:00 UTC, or move it to 04:00' (#export-time): no change, in the words "${keepIt}"`
  ),
  docEditGate("call-d2", "xd://dispatch_doc_edit", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("no_change", keepIt),
  }),
]);
// on, nochange-block: the edit went out.
run("askgate-nochange-block-on-2", askgate("nochange-block"), [
  ...header,
  turn[0],
  toolCall("call-d2", "bash", {
    command: `dispatch doc-edit --issue LWEVAL-2 --artifact spec --ops-json '${JSON.stringify([{ op: "insert", after: "end", markdown: noChangeBlock }])}'`,
  }),
  result("call-d2", "Edited spec: version 2"),
  docEditGate("call-d2", "dispatch doc-edit", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
const billingRead = "gh api orgs/example-org/settings/billing/actions";
const need =
  "I need to know how many Actions minutes the organization has left before the nightly export moves to 04:00 UTC, as dispatch://LWEVAL-2/comment/c1 asks.";
const billingAsk = (id: string, question: string, flags = "") =>
  toolCall(id, "bash", {
    command: `dispatch ask --issue LWEVAL-2 --question '${question}' --option Done --option 'Cannot run it'${flags}`,
  });
const untriedQuestion = `Can you run ${billingRead} and paste its output here? ${need}`;
/** The gh stand-in's record of the read, and the bash call that ran it. */
const ranBilling = {
  "calls.jsonl": calls([
    {
      at: "2026-10-24T10:00:00.000Z",
      as: "gh",
      argv: ["api", "orgs/example-org/settings/billing/actions"],
      exit: 1,
    },
  ]),
};
const billingCall = [
  toolCall("call-g", "bash", { command: billingRead }),
  result("call-g", "gh: Not Found (HTTP 404)"),
];
// on, untried: the rules check refused the ask in block mode, and the agent never ran the read.
run("askgate-untried-on-1", askgate("untried"), [
  ...header,
  turn[0],
  billingAsk("call-u", untriedQuestion),
  gateRefused(
    "call-u",
    `${ruleLine}\n- The ask: untried access, in the words "Can you run ${billingRead}"`
  ),
  askGate("call-u", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("untried_access", `Can you run ${billingRead}`),
  }),
]);
// on, untried: the agent ran the read anyway, and the rules check let the ask through.
run("askgate-untried-on-2", { ...askgate("untried", anAsk), ...ranBilling }, [
  ...header,
  turn[0],
  ...billingCall,
  billingAsk("call-u", untriedQuestion),
  opened("call-u"),
  askGate("call-u", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// on, nochange-rebuttal: the rebuttal sent the ask without review.
run("askgate-nochange-rebuttal-on-1", askgate("nochange-rebuttal", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n", rebuttalFlag),
  opened("call-n"),
  askGate("call-n", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "block",
    rebuttal: "The person asked for the move.",
  }),
]);
// on, nochange-rebuttal: sent first without the flag, refused, then resent with it.
run("askgate-nochange-rebuttal-on-2", askgate("nochange-rebuttal", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n1"),
  gateRefused("call-n1", noChangeRefusal),
  askGate("call-n1", {
    outcome: "verdict",
    verdictMode: "warn",
    ...ruleMatch("no_change", keepIt),
  }),
  noChangeCall("call-n2", rebuttalFlag),
  opened("call-n2"),
  askGate("call-n2", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "warn",
    rebuttal: "The person asked for the move.",
  }),
]);
const triedQuestion = `I ran ${billingRead} and it failed with gh: Not Found (HTTP 404). Can you run it with your access and paste its output here? ${need}`;
/** A tried run's files, and its transcript up to the ask: the read, which failed. */
const triedFiles = { ...askgate("tried", anAsk), ...ranBilling };
const triedStart = [...header, turn[0], ...billingCall];
// on, tried: the gate let the ask through in block mode.
run("askgate-tried-on-1", triedFiles, [
  ...triedStart,
  billingAsk("call-t", triedQuestion),
  opened("call-t"),
  askGate("call-t", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// on, tried: the charter's revise in warn mode only noted the ask, which went out.
run("askgate-tried-on-2", triedFiles, [
  ...triedStart,
  billingAsk("call-t", triedQuestion),
  opened("call-t"),
  askGate("call-t", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "warn",
    reason: "Shorten it.",
    ruleOutcome: "none",
  }),
]);
// on, tried: the same revise in block mode refused the ask, and a shorter one went out.
run("askgate-tried-on-3", triedFiles, [
  ...triedStart,
  billingAsk("call-t1", triedQuestion),
  gateRefused("call-t1", "AskGate did not send this call. Shorten it."),
  askGate("call-t1", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Shorten it.",
    ruleOutcome: "none",
  }),
  billingAsk("call-t2", `I ran ${billingRead} and got Not Found. Can you run it?`),
  opened("call-t2"),
  askGate("call-t2", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// on, tried: the rules check refused the tried ask, and the agent's rebuttal sent it.
run("askgate-tried-on-4", triedFiles, [
  ...triedStart,
  billingAsk("call-t1", triedQuestion),
  gateRefused("call-t1", `${ruleLine}\n- The ask: untried access, in the words "Can you run it"`),
  askGate("call-t1", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("untried_access", "Can you run it"),
  }),
  billingAsk("call-t2", triedQuestion, " --advisor-rebuttal 'I ran the read; it failed.'"),
  opened("call-t2"),
  askGate("call-t2", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "block",
    rebuttal: "I ran the read; it failed.",
  }),
]);
// on, nochange-rebuttal: a first attempt sent as a compound command, which reached no gate and
// which the CLI refused with no marker, then the rebuttal, which sent the ask. No model read the
// first attempt, so it is no refusal of the ask.
run("askgate-nochange-rebuttal-on-3", askgate("nochange-rebuttal", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n1", `${rebuttalFlag}; echo done`),
  gateRefused("call-n1", "dispatch ask was not called: AskGate is on for this session."),
  askGate("call-n1", { decision: "revise", outcome: "unmarked", verdictMode: "block" }),
  noChangeCall("call-n2", rebuttalFlag),
  opened("call-n2"),
  askGate("call-n2", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "block",
    rebuttal: "The person asked for the move.",
  }),
]);
// off, facts: the ask went out and the gate never ran.
run("askgate-facts-off-1", askgate("facts", anAsk), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
]);
// off, facts: the gate ran although the profile turns it off.
run("askgate-facts-off-2", askgate("facts", anAsk), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  factsGate("warn"),
]);
// off, facts: no ask reached the issue.
run("askgate-facts-off-3", askgate("facts"), [...header, ...turn]);
// off, link: a pair the score does not judge.
run("askgate-link-off-1", askgate("link", anAsk), [...header, ...turn]);
// A directory that names no case, as the rig named them before the case was in the name.
run("askgate-on-1", askgate("facts"), [...header, ...turn]);
// A directory whose case is not the one its case.txt records.
run("askgate-upload-on-4", askgate("facts"), [...header, ...turn]);

// Each run's own Dispatch, as dispatch.json records it. Two runs of one batch that name one
// database in the batch's Postgres shared a Dispatch, so each could read the other's issue and
// asks: neither is scored. The same name in two batches is two databases, one in each batch's
// Postgres container.
const didNothing = [...header, ...turn];
for (const [name, database, postgres] of [
  ["ask-on-message-shared-1", "run_shared", "pg-7"],
  ["ask-on-message-shared-2", "run_shared", "pg-7"],
  ["ask-on-message-apart-1", "run_apart_1", "pg-7"],
  ["ask-on-message-apart-2", "run_apart_2", "pg-7"],
  ["ask-on-message-batches-1", "run_batches", "pg-7"],
  ["ask-on-message-batches-2", "run_batches", "pg-8"],
] as const)
  run(
    name,
    { ...capture, "out.txt": "exit=0\n", "dispatch.json": dispatchRecord(database, postgres) },
    didNothing
  );
// A run that records no Dispatch of its own, as every run did before each run had one. Only an
// older spec run is scored without one (below): an askgate or ask-on-message run that records none
// stays a rig error.
run(
  "ask-on-message-unrecorded-1",
  { ...capture, "out.txt": "exit=0\n", "dispatch.json": null },
  didNothing
);
run("askgate-facts-unrecorded-1", { ...askgate("facts", anAsk), "dispatch.json": null }, [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
]);
// A rules case's run, as run_askgate leaves it: own_dispatch writes dispatch.json before
// seed_dispatch seeds the run's issue. With it the run is scored; with none it is a rig error, as
// every other askgate run that records no Dispatch of its own is.
const noChangeRefusedInBlock = [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  gateRefused("call-n", noChangeRefusal),
  askGate("call-n", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("no_change", keepIt),
  }),
];
run(
  "askgate-nochange-recorded-1",
  {
    ...askgate("nochange"),
    "dispatch.json": dispatchRecord("run_askgate_nochange_recorded_1"),
  },
  noChangeRefusedInBlock
);
run(
  "askgate-nochange-unrecorded-1",
  { ...askgate("nochange"), "dispatch.json": null },
  noChangeRefusedInBlock
);

// spec-writing and spec-rewrite (score.ts specRow). The run below follows every rule, and each
// other run changes one thing in it. Every text is invented.
const specSession = "01a16640-8259-71ed-bb68-1e4927b77a01";
const specHeader = [{ type: "session", id: specSession, cwd: "/rig/cwd" }, ...header.slice(1)];
/** A spec in the outline: `problem` is its Problem paragraph, `design` an extra Design paragraph
 * and `summary` an extra Summary sentence. */
function outlined({ problem = "The CLI prints no greeting.", design = "", summary = "" } = {}) {
  return [
    "## Summary",
    "",
    `The widgets CLI greets a person by name.${summary}`,
    "",
    "## Problem",
    "",
    problem,
    "",
    "## Goals and non-goals",
    "",
    "Goals: a greeting. Non-goals: translations.",
    "",
    "## Requirements",
    "",
    ":::requirement{#r1}",
    "**R1.** The CLI prints a greeting with the name. From: your answer on the greeting.",
    ":::",
    "",
    "## Design",
    "",
    "`greet <name>` prints `Hello, <name>.`",
    design,
    "",
    "## Decisions",
    "",
    "### Plain text output",
    "",
    "The CLI prints plain text. Reason: scripts read it. Options: plain text or JSON. Parts: Design.",
    "",
  ].join("\n");
}
// The person's input: messages.md less each entry's header and each anchored comment's quote (the
// `>` lines right after a `<!-- comment <time> anchored -->` header), and each answer's text in
// asks.json. Every other `>` line is the person's own words: a block quote in a message, one after
// a comment's quote, or one an unanchored comment starts with.
const askedFor =
  "Please make the widgets command line print a friendly greeting whenever somebody runs it with their name.";
const quotedLine = "The command line prints nothing at all when a person runs it today.";
const longMessage =
  "The greeting must stay on one line, use the name exactly as the person typed it, end with a full stop, and never print the name twice even when the person repeats it on the command line.";
const voiceLine =
  "I want the greeting to feel warm, so pick words a support agent would really say out loud.";
const commentQuote =
  "A colleague wrote that the old banner scrolled off the top of every small terminal window.";
const pastedLine =
  "The old ticket said every run of the greeting tool gets eight turns before it stops.";
const personMessages = [
  "<!-- message 2026-10-20T10:00:00Z -->",
  askedFor,
  "",
  "<!-- comment 2026-10-20T11:00:00Z anchored -->",
  `> ${quotedLine}`,
  "",
  "Keep the output short so that scripts can still read each line.",
  "",
  `> ${commentQuote}`,
  "",
  "<!-- message 2026-10-21T09:00:00Z -->",
  longMessage,
  "",
  "<!-- message 2026-10-22T09:00:00Z -->",
  `> ${voiceLine}`,
  "",
  "That is what I said on the call.",
  "",
  "<!-- comment 2026-10-23T09:00:00Z -->",
  `> ${pastedLine}`,
  "",
  "I pasted that from the old ticket.",
  "",
].join("\n");
const sourceAsks = [
  {
    id: "src-1",
    kind: "question",
    question: "Which output format?",
    options: [{ label: "Plain text" }, { label: "JSON" }],
    multiple: false,
    state: "answered",
    created_at: "2026-10-20T12:00:00Z",
    answer: {
      selected: ["Plain text"],
      text: "Plain text, since scripts parse the output line by line.",
      user: "alice",
      at: "2026-10-20T12:05:00Z",
    },
  },
];
/** The words of a text as the `quoted` score compares them: case and punctuation folded. */
const folded = (text: string) => (text.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []).join(" ");

/** One line the dispatch CLI logs for a call it ran (dispatch-session-state.ts `ResultEntry`),
 * before its time: the tool, and the refusal when Dispatch or the call refused it. */
interface Logged {
  tool: string;
  error?: string;
}
/** What the CLI logs for a call when the fixture says nothing else: each `dispatch` command the
 * call ran, as a success, when the call exited 0 and its output shows no refusal line, and no
 * `--help` or `--dry-run` call. A refused call logs nothing by default. The CLI logs a refusal too
 * when the call knows its session (dispatch-cli.ts runDispatchCli): under the command's tool, or
 * under `dispatch` for a command it cannot name. A fixture that models such a call names its lines,
 * and one that leaves them out models a call that knew no session. */
const loggedRun = (command: string, exit: number, text: string): Logged[] =>
  exit !== 0 || cliRefusals(text).length > 0
    ? []
    : [...command.matchAll(/(?:^|[;&|(\n])\s*dispatch ([a-z][a-z-]*)([^;&|\n]*)/g)]
        .filter(([, , args]) => !/--help|--dry-run/.test(args ?? ""))
        .map(([, name]) => ({ tool: `dispatch_${(name ?? "").replace(/-/g, "_")}` }));
/** A `bash` call and its result, as a real Oh My Pi `bash` result records its exit code, and the
 * ledger lines the CLI wrote while it ran (`logged`, loggedRun by default), which specRun moves into
 * the run's ledger. */
const sh = (
  id: string,
  command: string,
  exit = 0,
  text = "ok",
  logged: Logged[] = loggedRun(command, exit, text)
) => [
  toolCall(id, "bash", { command }),
  {
    type: "message",
    logged,
    message: {
      role: "toolResult",
      toolName: "bash",
      toolCallId: id,
      isError: exit !== 0,
      details: { exitCode: exit },
      content: [
        { type: "text", text: exit === 0 ? text : `${text}\n\nCommand exited with code ${exit}` },
      ],
    },
  },
];
/** An `eval` cell and its result. Its code runs `dispatch` through its own language, which no
 * shell parse reads, and Oh My Pi's kernel is given no session, so the CLI logs nothing for it
 * (`logged` is empty unless a fixture says otherwise). */
const evalCell = (id: string, code: string, text: string, logged: Logged[] = []) => [
  toolCall(id, "eval", { code, language: "python" }),
  {
    type: "message",
    logged,
    message: {
      role: "toolResult",
      toolName: "eval",
      toolCallId: id,
      isError: false,
      content: [{ type: "text", text }],
    },
  },
];
const readOf = (id: string, target: string) => [
  toolCall(id, "read", { path: target }),
  result(id, "text"),
];
/** A spec-only check: a `task` call whose assignment holds the spec, and its report as a blocking
 * task answers, one completed `<task-result>` block per task. */
const checkOf = (id: string, assignment = outlined()) => [
  toolCall(id, "task", {
    context: `Read this spec alone and list each gap.\n\n${assignment}`,
    tasks: [{ name: `Check${id}`, task: "List each gap." }],
  }),
  result(
    id,
    `<task-result id="Check${id}" agent="task" status="completed" duration="40s">\nNo gaps.\n</task-result>`
  ),
];
/** A doc-edit of the run's spec, answered as Dispatch answers one that made a version. */
const specEdit = (id: string, version = 2) =>
  sh(
    id,
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
    0,
    `Applied 1 ops (version ${version})\nDocument token: sha256:abc`
  );
const routedRead = readOf("r1", "skill://dispatch-writing-specs");
const askStep = sh(
  "a1",
  "dispatch ask --issue LWEVAL-7 --question 'Which name? Recommendation: Ada.'"
);
const finish = [
  ...sh("p1", "dispatch request-approval --issue LWEVAL-7 --summary 'The greeting.'"),
  ...readOf("r2", "skill://writing-plans"),
  ...sh("p2", "dispatch artifact --issue LWEVAL-7 --name plan.md --content-file plan.md"),
];
const conversation = (...steps: object[][]) => [...specHeader, turn[0], ...steps.flat()];
const followed = conversation(
  routedRead,
  checkOf("c1"),
  specEdit("w1"),
  askStep,
  checkOf("c2"),
  specEdit("w2"),
  finish
);
const noFaults = {
  "The reader cannot follow a sentence": 0,
  "A term that nobody explained": 0,
  "Too much text, or facts that do not matter": 0,
  "Vague phrasing that hides who did not act": 0,
  "History in place of design": 0,
};
/** faults.json with `counts` faults in their rows, each item quoting the spec. */
function faultsFile(counts: Partial<typeof noFaults> = {}) {
  const faults = { ...noFaults, ...counts };
  const items = Object.entries(faults).flatMap(([row, n]) =>
    Array.from({ length: n }, () => ({ row, quote: "greets a person" }))
  );
  return { faults, ste_breaks: 0, items };
}
const cleanJudges: Record<string, object> = {
  "faults.json": faultsFile(),
  "decisions.json": {
    rows: [
      {
        choice: "The CLI prints plain text.",
        recorded: true,
        reason: true,
        options: true,
        parts: true,
      },
    ],
  },
  "findings.json": { rows: [{ check: 1, finding: "R1 names no check.", verdict: "fixed" }] },
  "checkable.json": {
    version: 3,
    rows: [{ requirement: "R1", check: "Run greet Ada and read the line.", uncheckable: false }],
  },
  "questions.json": {
    rows: [
      { id: "ask-1", dominated: false, settled: false, kind: "taste", reason: "A name is taste." },
    ],
  },
  "numbers.json": { rows: [{ id: "msg-1", names_number: false, quote: null }] },
  "preamble.json": { rows: [] },
  "proposal.json": { rows: [{ item: "the greeting word", verdict: "proposed", ask: null }] },
  "shared-runs.json": { rows: [] },
};
/** A question the run asked the person, opened and answered before the earliest version any
 *  fixture here writes (`conversationOrder`: a question before the first version, an answer
 *  before the version that states a design). A fixture that means to fail `asked` or `grew`
 *  passes its own times. */
const openAsk = (id: string) => ({
  id,
  question: "Which name should the example use?",
  options: [{ label: "Ada" }, { label: "Grace" }],
  multiple: false,
  // `second(1)` and `second(2)`, written out: this runs at module scope, above that helper.
  created_at: "2026-10-24T10:00:01.000Z",
  answer: { at: "2026-10-24T10:00:02.000Z" },
});
const approvedStates = [
  { turn: 1, open_asks: [openAsk("ask-1")], approval: null },
  { turn: 2, open_asks: [], approval: { ask_id: "appr-1", version: 3 } },
  { turn: 3, open_asks: [], approval: null },
];
interface SpecFixture {
  spec?: string;
  transcript?: object[];
  judges?: Record<string, object>;
  seeded?: string[];
  states?: object[];
  turns?: string;
  documents?: Record<string, string>;
  source?: string;
  /** false: the run has no CLI ledger. */
  ledger?: boolean;
  /** false: the run records no Dispatch of its own (no dispatch.json), as a spec run of the rig
   * before each run had one: it ran on its batch's Dispatch, alone in its batch. */
  ownDispatch?: boolean;
  /** agent-asks.json and agent-texts.json, when not the run's one question and one message. */
  asks?: object[];
  texts?: object[];
  /** Subagent transcripts beside the session's, by name (subagentTranscript). */
  subagents?: Record<string, object[]>;
  /** The second (as `second` counts) of each version the session wrote, from version 2; by
   * default versions 2 and 3, after every entry of the transcript. */
  versions?: number[];
}
/** A time `seconds` after the first entry of a spec run's transcript: timed gives entry N the time
 * of second N. */
const second = (seconds: number) =>
  new Date(Date.parse("2026-10-24T10:00:00.000Z") + seconds * 1000).toISOString();
/** A subagent's own transcript, as Oh My Pi writes one beside the session's: its assignment as its
 * first user message at second `started`, and its report as its last assistant message at second
 * `reported`, in text or (`yielded`) as the `yield` call it ends with. */
const subagentTranscript = (
  assignment: string,
  started: number,
  reported: number,
  report = "No gaps.",
  yielded = false
) => [
  { type: "session", id: "01a16641", timestamp: second(started), cwd: "/rig/cwd" },
  {
    type: "message",
    timestamp: second(started),
    message: { role: "user", content: [{ type: "text", text: assignment }] },
  },
  {
    type: "message",
    timestamp: second(reported),
    message: {
      role: "assistant",
      content: yielded
        ? [{ type: "toolCall", id: "y1", name: "yield", arguments: { data: { review: report } } }]
        : [{ type: "text", text: report }],
    },
  },
];
/** The run's transcript and its dispatch CLI ledger (results.jsonl): each entry gets a time one
 * second after the one before it, and each line an entry logs (`logged`) is written to the ledger
 * one millisecond apart, just before that entry's time, so it falls inside its tool call. */
function timed(transcript: object[]): { entries: object[]; ledger: object[] } {
  const start = Date.parse("2026-10-24T10:00:00.000Z");
  const ledger: object[] = [];
  const entries = transcript.map((entry, index) => {
    const { logged, ...rest } = entry as { logged?: Logged[]; timestamp?: string };
    const at = rest.timestamp ?? new Date(start + index * 1000).toISOString();
    for (const [n, line] of (logged ?? []).entries())
      ledger.push({
        at: new Date(Date.parse(at) - (logged?.length ?? 0) + n).toISOString(),
        tool: line.tool,
        ...(line.error === undefined ? { details: {} } : { error: line.error }),
      });
    return { ...rest, timestamp: at };
  });
  return { entries, ledger };
}
/** A spec-writing or spec-rewrite run directory, as rig.sh, seed.ts and judge.ts write one. */
function specRun(name: string, fixture: SpecFixture = {}) {
  const spec = fixture.spec ?? outlined();
  const states = fixture.states ?? approvedStates;
  const judges = { ...cleanJudges, ...fixture.judges };
  const { entries, ledger } = timed(fixture.transcript ?? followed);
  run(
    name,
    {
      ...(fixture.ownDispatch === false ? { "dispatch.json": null } : {}),
      "out.txt": "exit=0\n",
      "source.txt": `${fixture.source ?? "acme-1"}\n`,
      "fixture.json": JSON.stringify({ issue: "LWEVAL-7", spec_artifact: "art-spec" }),
      "seeded.json": JSON.stringify(fixture.seeded ?? []),
      "turns.txt": fixture.turns ?? "1 first exit=0\n2 answered exit=0\n3 approved exit=0\n",
      "spec.md": spec,
      "plan.md": "# Plan\n\n1. Add greet.\n",
      "spec-versions.json": JSON.stringify(
        (fixture.versions ?? [120, 180]).map((at, index) => ({
          version: index + 2,
          created_at: second(at),
          authors: [{ kind: "session", id: specSession }],
        }))
      ),
      "agent-asks.json": JSON.stringify(
        fixture.asks ?? [
          { kind: "question", ...openAsk("ask-1"), state: "answered" },
          { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
        ]
      ),
      "agent-texts.json": JSON.stringify(
        fixture.texts ?? [
          { id: "msg-1", kind: "message", body: "The spec is ready to read.", created_at: "" },
        ]
      ),
      ...Object.fromEntries(
        states.map((state, index) => [`state-${index + 1}.json`, JSON.stringify(state)])
      ),
      ...Object.fromEntries(
        Object.entries(judges).map(([file, body]) => [file, JSON.stringify(body)])
      ),
    },
    entries
  );
  stagings.push(() => {
    const dir = path.join(runs, name);
    for (const sub of ["source", "documents", "spec-versions"])
      mkdirSync(path.join(dir, sub), { recursive: true });
    writeFileSync(path.join(dir, "source", "title.txt"), "Greet a person by name\n");
    writeFileSync(path.join(dir, "source", "messages.md"), personMessages);
    writeFileSync(path.join(dir, "source", "asks.json"), JSON.stringify(sourceAsks));
    for (const index of (fixture.versions ?? [120, 180]).keys())
      writeFileSync(path.join(dir, "spec-versions", `${index + 2}.md`), spec);
    for (const [file, text] of Object.entries(fixture.documents ?? {}))
      writeFileSync(path.join(dir, "documents", file), text);
    const subagents = path.join(dir, "sessions", "2026-09-30T14-51-17-651Z_01a0f2cc");
    for (const [agent, transcript] of Object.entries(fixture.subagents ?? {})) {
      mkdirSync(subagents, { recursive: true });
      writeFileSync(
        path.join(subagents, `${agent}.jsonl`),
        transcript.map((entry) => `${JSON.stringify(entry)}\n`).join("")
      );
    }
    // The default findings judge (cleanJudges) gives check 1 its row; every other check the run's
    // transcript holds is clean, so the file covers each check as coverage requires.
    if (fixture.judges?.["findings.json"] === undefined) {
      const fixtureJson = { issue: "LWEVAL-7", spec_artifact: "art-spec" };
      const checks = specWork(dir, checkouts(work), fixtureJson).checks.length;
      writeFileSync(
        path.join(dir, "findings.json"),
        JSON.stringify({
          rows: checks > 0 ? [{ check: 1, finding: "R1 names no check.", verdict: "fixed" }] : [],
          clean: Array.from({ length: Math.max(0, checks - 1) }, (_, index) => index + 2),
        })
      );
    }
    if (fixture.ledger === false) return;
    const state = path.join(ledgerDir(name));
    mkdirSync(state, { recursive: true });
    writeFileSync(
      path.join(state, "results.jsonl"),
      ledger.map((line) => `${JSON.stringify(line)}\n`).join("")
    );
  });
}
/** Where the dispatch CLI keeps a run's ledger: the run's HOME's state directory, under the
 * session id the transcript's header names (dispatch-session-state.ts sessionDirectory). */
const ledgerDir = (name: string) =>
  path.join(runs, name, "home", ".local", "state", "dispatch", "sessions", specSession);
const copied = (source: string, words: string, verdict: string) => ({
  "shared-runs.json": { rows: [{ source, run: words, verdict, reason: "The verdict." }] },
});

specRun("spec-writing-head-1");
specRun("spec-rewrite-head-1");
// Older spec runs: no dispatch.json, as the rig recorded a spec run before each run had a Dispatch
// of its own.
specRun("spec-writing-older-1", { ownDispatch: false });
specRun("spec-rewrite-older-1", { ownDispatch: false });
// Not in the outline: a heading of C2 is gone.
specRun("spec-writing-head-2", { spec: outlined().replace("## Goals and non-goals", "## Scope") });
// No check before the first spec write.
specRun("spec-writing-head-3", {
  transcript: conversation(
    routedRead,
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A finding the agent neither fixed nor recorded.
specRun("spec-writing-head-4", {
  judges: {
    "findings.json": {
      rows: [{ check: 1, finding: "No owner.", verdict: "neither" }],
      clean: [2],
    },
  },
});
const refusal = "dispatch ask: the question gives no recommendation; end it with one";
const refused = sh("x1", "dispatch ask --issue LWEVAL-7 --question 'Which name?'", 2, refusal);
// A refused call fixed from its refusal alone.
specRun("spec-writing-head-5", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    refused,
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refused call fixed only after a read of a skill file.
specRun("spec-writing-head-6", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    refused,
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Refusals the dispatch CLI printed itself, through a pipe that hid the exit (bash exit 0, no
// isError). Each text is the real CLI's: an argument check's (exit 1 unpiped) and a usage
// refusal's (exit 2 unpiped). The CLI's other `dispatch:` lines, a warning and a note that it kept
// no record of a call Dispatch took, refuse nothing.
const pipedRefusal =
  "dispatch doc-edit was not called: 1 problem\n- --precondition-json is not JSON: JSON Parse error: Unterminated string";
const piped = sh(
  "x2",
  'dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json --precondition-json \'{"document":"sha\' 2>&1 | head -20',
  0,
  pipedRefusal
);
const offRefusal =
  "dispatch: no envoy.json at /home/agent/.config/legion/envoy.json: Dispatch is off";
const pipedOff = sh(
  "x4",
  "dispatch ask --issue LWEVAL-7 --question 'Which name? Recommendation: Ada.' 2>&1 | tee ask.log",
  0,
  offRefusal
);
const warned = sh(
  "x3",
  "dispatch read --issue LWEVAL-7 2>&1 | head -40",
  0,
  "dispatch: warning: Dispatch issue is missing resolved_repo: this Dispatch server predates repository fields, so LWEVAL-7's repository is unknown; upgrade the server\nTitle: Greeting\ndispatch: Dispatch took the call, but this session's state could not be written: EACCES"
);
// A piped refusal fixed from its text alone; then a warned call and a skill read, which refuse
// nothing.
specRun("spec-writing-head-23", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    piped,
    askStep,
    warned,
    readOf("r3", "skill://dispatch"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A piped refusal fixed only after a read of a skill file.
specRun("spec-writing-head-24", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    piped,
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A piped refusal whose next dispatch call is a piped usage refusal: that call fixes nothing, so
// both are fixed only after the skill read.
specRun("spec-writing-head-25", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    piped,
    pipedOff,
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// One fault.
specRun("spec-writing-head-7", {
  judges: { "faults.json": faultsFile({ "A term that nobody explained": 1 }) },
});
// Read the skill this change removes.
specRun("spec-writing-head-8", {
  transcript: conversation(
    routedRead,
    readOf("r3", "skill://dispatch-brainstorming"),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
const eightProblem = "The CLI must print a friendly greeting whenever somebody runs it.";
const eightWords = "print a friendly greeting whenever somebody runs it";
// Shares 8 words with messages.md: the judge marks the run `copies`, then `names`.
specRun("spec-writing-head-9", {
  spec: outlined({ problem: eightProblem }),
  judges: copied("spec.md", eightWords, "copies"),
});
specRun("spec-writing-head-10", {
  spec: outlined({ problem: eightProblem }),
  judges: copied("spec.md", eightWords, "names"),
});
// Shares 7: no run to judge.
specRun("spec-writing-head-11", {
  spec: outlined({ problem: "The CLI must print a friendly greeting whenever somebody runs." }),
});
// Keeps a comment's anchored quote, which quotes the old spec, not the person.
specRun("spec-writing-head-12", { spec: outlined({ problem: quotedLine }) });
// Copies the `>` lines that start a message: the person's own words, as a block quote.
specRun("spec-writing-head-19", {
  spec: outlined({ problem: voiceLine }),
  judges: copied("spec.md", folded(voiceLine), "copies"),
});
// Copies a `>` line after a comment's anchored quote and its body: the person's words too.
specRun("spec-writing-head-20", {
  spec: outlined({ problem: commentQuote }),
  judges: copied("spec.md", folded(commentQuote), "copies"),
});
// Copies the `>` line an unanchored comment starts with: a person's paste, their own input.
specRun("spec-writing-head-26", {
  spec: outlined({ problem: pastedLine }),
  judges: copied("spec.md", folded(pastedLine), "copies"),
});
// One bash call that runs two `dispatch` commands is two invocations, scored in order. Two spec
// writes in one call with no check between them: the second is unchecked.
specRun("spec-writing-head-27", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    sh(
      "w1",
      "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file first.json; dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file second.json",
      0,
      "Applied 1 ops (version 2)\nApplied 1 ops (version 3)"
    ),
    finish
  ),
});
// A refusal followed by a command that succeeded in the same call: the agent wrote that command
// before it saw the refusal, so it is no fix. The fix is the later call after a skill read.
specRun("spec-writing-head-28", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x5",
      "dispatch ask --issue LWEVAL-7; dispatch read --issue LWEVAL-7",
      0,
      "dispatch ask was not called: 1 problem\n- --question is required\nTitle: Greeting",
      [{ tool: "dispatch_read" }]
    ),
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal fixed in a later call that reads a skill file first: a `cat` of the run's own copy of
// skills/dispatch/SKILL.md, then the dispatch command.
const ownSkill = path.join(runs, "spec-writing-head-29", "home", "skills", "dispatch", "SKILL.md");
specRun("spec-writing-head-29", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x6",
      "dispatch ask --issue LWEVAL-7 --question 'Which name?'",
      1,
      "dispatch ask was not called: 1 problem\n- --question gives no recommendation"
    ),
    sh("x7", `cat ${ownSkill}; dispatch read --issue LWEVAL-7`, 0, "# Dispatch\nTitle: Greeting"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Refusal lines that are no refusal of the call: a document that quotes the CLI's refusals, and a
// log that a `cat` after a dispatch command prints. A later skill read fixes nothing.
specRun("spec-writing-head-30", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x8",
      "dispatch read --issue LWEVAL-7",
      0,
      "Issue description:\n```text\ndispatch: unexpected argument old-flag\ndispatch read was not called: 1 problem\n```",
      [{ tool: "dispatch_read" }]
    ),
    sh(
      "x9",
      "dispatch read --issue LWEVAL-7; cat saved.log",
      0,
      "Title: Greeting\ndispatch: unexpected argument old-flag",
      [{ tool: "dispatch_read" }]
    ),
    readOf("r3", "skill://dispatch"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal after the label an `echo` prints is the dispatch command's own output, fixed only
// after a skill read.
specRun("spec-writing-head-31", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x10",
      'cd /rig/cwd\necho "=== ask ==="; dispatch ask --issue LWEVAL-7 2>&1 | head -5',
      0,
      "=== ask ===\ndispatch ask was not called: 1 problem\n- --question is required"
    ),
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// checked: a check counts for a spec write only when its task call and its completed report both
// came after the write before it and before this write, in transcript order. Each run below
// changes the first check of `followed` (c1, before write w1).
/** A spec-only check's `task` call, with no result yet. */
const checkCall = (id: string) =>
  toolCall(id, "task", {
    context: `Read this spec alone and list each gap.\n\n${outlined()}`,
    tasks: [{ name: `Check${id}`, task: "List each gap." }],
  });
/** A tool's result as Oh My Pi records one: whether the tool failed, and the tool's `details`. */
const toolResult = (toolCallId: string, text: string, isError = false, details: object = {}) => ({
  type: "message",
  message: { role: "toolResult", toolCallId, isError, details, content: [{ type: "text", text }] },
});
/** Oh My Pi's `<task-result>` envelope of task `name`, as task-summary.md writes it. */
const taskEnvelope = (name: string, status: string, report: string) =>
  `<task-result id="${name}" agent="task" status="${status}" duration="40s">\n<output>\n${report}\n</output>\n</task-result>`;
/** A background check's spawn result: the task runs on, and its report arrives later. */
const spawned = (id: string) =>
  toolResult(
    id,
    `Spawned agent \`Check${id}\` (job \`Check${id}\`).\n\nResults auto-deliver; NEVER poll.`
  );
/** The line Oh My Pi writes after a task's closing tag in a delivery, in its commonest wording.
 * Most real deliveries carry one (1507 of 2254 envelopes in the operator's transcripts). */
const idleTrailer = (name: string) =>
  `\n\n${name} is now idle — message it via \`write agent://${name}\` to follow up; transcript at history://${name}`;
/** A background task's report, delivered by Oh My Pi with the job's own structured status. */
const asyncReport = (name: string, status = "completed", report = "No gaps.") => ({
  type: "custom_message",
  customType: "async-result",
  details: {
    meta: { source: { type: "report", value: "background job delivery" } },
    jobs: [{ jobId: name, type: "task", status, agentId: name, label: name }],
  },
  content: `<system-notice>\nBackground job ${name} has completed.\n${taskEnvelope(name, status, report)}${idleTrailer(name)}\n</system-notice>`,
});
/** A `wait` call whose structured job snapshot delivers a background task's report. */
const waited = (id: string, name: string, status = "completed", report = "No gaps.") => {
  const resultText = taskEnvelope(name, status, report);
  return [
    toolCall(id, "wait", {}),
    {
      type: "message",
      message: {
        role: "toolResult",
        toolName: "wait",
        toolCallId: id,
        isError: false,
        details: {
          op: "wait",
          jobs: [{ id: name, type: "task", status, label: name, resultText }],
        },
        content: [
          {
            type: "text",
            text: `## Completed (1)\n\n### ${name} [task] — ${status}\n\`\`\`\n${resultText}\n\`\`\``,
          },
        ],
      },
    },
  ];
};
/** `followed`, with its first check replaced by `check`. */
const firstCheck = (...check: object[][]) =>
  conversation(
    routedRead,
    ...check,
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  );
// The task call failed: no checker ran.
specRun("spec-writing-head-32", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", "No checker ran: the scout agent is unavailable.", true),
  ]),
});
// A background check whose report arrived after the write: no check for that write, nor for the
// next one, which has no check of its own.
specRun("spec-writing-head-33", {
  transcript: conversation(
    routedRead,
    [checkCall("c1"), spawned("c1")],
    specEdit("w1"),
    [asyncReport("Checkc1")],
    askStep,
    specEdit("w2"),
    finish
  ),
  judges: { "findings.json": { rows: [] } },
});
// A task that failed, and one that was cancelled.
specRun("spec-writing-head-34", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", taskEnvelope("Checkc1", "failed (exit 1)", "Error: the model refused.")),
  ]),
});
specRun("spec-writing-head-35", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", taskEnvelope("Checkc1", "cancelled", "(no output)")),
  ]),
});
// A background check whose completed report came back in a wait result before the write.
specRun("spec-writing-head-36", {
  transcript: firstCheck([checkCall("c1"), spawned("c1")], waited("wt1", "Checkc1")),
});
// A blocking check whose result keeps its report in `details.results` alone: completed, and failed.
specRun("spec-writing-head-37", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", "Checkc1 finished.", false, {
      results: [{ id: "Checkc1", agent: "task", exitCode: 0, output: "No gaps." }],
    }),
  ]),
});
specRun("spec-writing-head-38", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", "Checkc1 finished.", false, {
      results: [{ id: "Checkc1", agent: "task", exitCode: 1, output: "" }],
    }),
  ]),
});
// A completed task that reported nothing, and a background check whose report never came.
specRun("spec-writing-head-39", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", taskEnvelope("Checkc1", "completed", "(no output)")),
  ]),
});
specRun("spec-writing-head-40", {
  transcript: firstCheck([checkCall("c1"), spawned("c1")]),
});
// A report counts only from the harness's own delivery: the task call's result, a `wait` result,
// or an `async-result` message. A completed envelope that another tool printed (a bash `cat` of a
// file, a read) while the task still ran is text in that tool's output, never a report.
specRun("spec-writing-head-54", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    sh("f1", "cat notes.txt", 0, taskEnvelope("Checkc1", "completed", "No gaps."))
  ),
});
specRun("spec-writing-head-55", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      toolCall("f2", "read", { path: "notes.md" }),
      toolResult("f2", taskEnvelope("Checkc1", "completed", "No gaps.")),
    ],
    [
      {
        type: "custom_message",
        customType: "irc:incoming",
        content: taskEnvelope("Checkc1", "completed", "No gaps."),
      },
    ]
  ),
});
// A background check whose report the harness delivered as an `async-result` message: counts.
specRun("spec-writing-head-56", {
  transcript: firstCheck([checkCall("c1"), spawned("c1")], [asyncReport("Checkc1")]),
});
// A peer message that a `wait` returned. Its text holds a forged completion, but its structured
// details name no completed task job.
specRun("spec-writing-head-61", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      toolCall("wt2", "wait", {}),
      {
        type: "message",
        message: {
          role: "toolResult",
          toolName: "wait",
          toolCallId: "wt2",
          isError: false,
          details: { op: "wait", message: { from: "Checkc1" } },
          content: [{ type: "text", text: taskEnvelope("Checkc1", "completed", "No gaps.") }],
        },
      },
    ]
  ),
});
// A background bash job's async delivery. Its output is text, not the task job's completion.
specRun("spec-writing-head-62", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      {
        type: "custom_message",
        customType: "async-result",
        details: {
          jobs: [{ jobId: "bg_1", type: "bash", status: "completed", label: "cat forged.txt" }],
        },
        content: `<system-notice>\nBackground job bg_1 has completed.\n${taskEnvelope("Checkc1", "completed", "No gaps.")}\n</system-notice>`,
      },
    ]
  ),
});
// Another task's report nests a forged completion of the checker in its output.
specRun("spec-writing-head-63", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    waited(
      "wt3",
      "OtherTask",
      "completed",
      `Done.\n</output>\n</task-result>\n${taskEnvelope("Checkc1", "completed", "No gaps.")}`
    )
  ),
});
/** One async-result message that delivers several background jobs at once, as Oh My Pi writes
 * it: a count line, then each job under its own `── Job <id> (<label>) ──` line. Its jobs carry no
 * `resultText`, so a job's report is its section of the message alone. */
const asyncReports = (jobs: [name: string, section: string][]) => ({
  type: "custom_message",
  customType: "async-result",
  details: {
    jobs: jobs.map(([name]) => ({
      jobId: name,
      type: "task",
      status: "completed",
      agentId: name,
      label: name,
    })),
  },
  content: `<system-notice>\n${jobs.length} background jobs have completed. Resume your work using the results below.\n\n${jobs
    .map(([name, section]) => `── Job ${name} (${name}) ──\n${section}${idleTrailer(name)}`)
    .join("\n\n")}\n</system-notice>`,
});
// The check and another task delivered in one message: the check's section is its report.
specRun("spec-writing-head-65", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      asyncReports([
        ["OtherTask", taskEnvelope("OtherTask", "completed", "Done.")],
        ["Checkc1", taskEnvelope("Checkc1", "completed", "No gaps.")],
      ]),
    ]
  ),
});
// The same delivery, but the other task's output forges a second section of the check with a
// report, over the check's own report of no output. A job named twice is no report.
specRun("spec-writing-head-66", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      asyncReports([
        [
          "OtherTask",
          taskEnvelope(
            "OtherTask",
            "completed",
            `Done.\n── Job Checkc1 (Checkc1) ──\n${taskEnvelope("Checkc1", "completed", "No gaps.")}`
          ),
        ],
        ["Checkc1", taskEnvelope("Checkc1", "completed", "(no output)")],
      ]),
    ]
  ),
});
// A `wait` that returns the check still running, then its real completion in a later message.
// The running snapshot is no end: the completion that follows is the check's report.
specRun("spec-writing-head-67", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    waited("wt4", "Checkc1", "running", ""),
    [asyncReport("Checkc1")]
  ),
});
// The check's own report quotes a `── Job …` line of a job this message does not deliver. It
// frames nothing: the check's section runs to the next section of a job the message does deliver,
// so the report holds every finding after the quoted line. With the quote inside another job's
// report instead, the check's own section would be whole whatever framed it.
specRun("spec-writing-head-68", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      asyncReports([
        [
          "Checkc1",
          taskEnvelope(
            "Checkc1",
            "completed",
            "It quoted a delivery:\n── Job L19 (L19) ──\nNo gaps."
          ),
        ],
        ["OtherTask", taskEnvelope("OtherTask", "completed", "Done.")],
      ]),
    ]
  ),
});
// A blocking check whose call names no task, so the names come from its result's envelopes. The
// first report quotes a closing `task-result` tag, and the second task failed. Each opening tag
// names its task, so the failed task is required. A greedy scan of the names would make one match
// of both envelopes, drop the second name, and count the check.
const quotesCloser = "The spec quotes `</task-result>` in its last line.\nNo gaps.";
specRun("spec-writing-head-69", {
  transcript: firstCheck([
    toolCall("c1", "task", {
      context: `Read this spec alone and list each gap.\n\n${outlined()}`,
      tasks: [{ task: "List each gap." }, { task: "List each gap again." }],
    }),
    toolResult(
      "c1",
      `${taskEnvelope("Checkc1-a", "completed", quotesCloser)}\n\n${taskEnvelope("Checkc1-b", "failed (exit 1)", "Error: the model refused.")}`,
      false,
      {
        results: [
          { id: "Checkc1-a", agent: "task", exitCode: 0, output: quotesCloser },
          { id: "Checkc1-b", agent: "task", exitCode: 1, output: "Error: the model refused." },
        ],
      }
    ),
  ]),
});
// checked: a check the agent started from an eval cell (`tool.task(…)`, `agent(…)`) has no `task`
// call in the transcript, only a transcript of its own beside the session's. It counts for the
// first spec write after its report when it started after the write before that one. In each run
// below, entry N of the session's transcript is at second N: the eval cell's call is entry 6 and
// the first write's call entry 8.
const evalSpawn =
  'spec = open("spec.md").read()\nawait tool.task({"tasks": [{"name": "EvalCheck", "task": "Read this spec alone and list each gap.\\n\\n" + spec}]})';
const evalFirst = (...subagent: object[][]) =>
  conversation(
    routedRead,
    ...subagent,
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  );
// Started from the cell, reported with `yield` before the first write: it checks that write.
specRun("spec-writing-head-96", {
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned EvalCheck.")),
  subagents: {
    EvalCheck: subagentTranscript(
      `Read this spec alone and list each gap.\n\n${outlined()}`,
      6.2,
      6.8,
      "No gaps.",
      true
    ),
  },
});
// A `task` call's own subagent is that call's check alone: the call names a path under the
// checkout, so neither it nor its transcript, which does not, counts.
specRun("spec-writing-head-97", {
  transcript: evalFirst(
    checkOf("c1", `${outlined()}\nCompare /checkouts/head/packages/cli/greet.ts.`)
  ),
  subagents: {
    Checkc1: subagentTranscript(`Read this spec alone.\n\n${outlined()}`, 6.2, 6.8),
  },
});
// Started before the first write (second 8) and reported before the second (second 12): it read
// the version before the first write, so it checks neither.
specRun("spec-writing-head-98", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    specEdit("w2"),
    finish
  ),
  subagents: {
    LateCheck: subagentTranscript(`Read this spec alone.\n\n${outlined()}`, 7.5, 11.5),
  },
});
// A subagent whose assignment is not the spec in the outline checks nothing, and one whose
// transcript ends with no report checks nothing either.
specRun("spec-writing-head-99", {
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned two agents.")),
  subagents: {
    Summary: subagentTranscript("Summarize the person's messages.", 6.2, 6.8),
    NoReport: subagentTranscript(`Read this spec alone.\n\n${outlined()}`, 6.2, 6.8, ""),
  },
});
// A review of a sibling issue's spec: its assignment holds the six headings too, but no 8-word
// run of it is in a version the session wrote, so it checks no write of this run.
const siblingSpec = [
  "## Summary",
  "",
  "The billing service sends each invoice by email on the first day of every month.",
  "## Problem",
  "",
  "Customers in three regions receive their invoices late because the mail queue stalls overnight.",
  "## Goals and non-goals",
  "",
  "Goals: invoices arrive on time. Non-goals: a new payment provider or tax rules.",
  "## Requirements",
  "",
  ":::requirement{#r1}",
  "**R1.** The queue retries a stalled invoice within ten minutes of the stall. From: the ticket.",
  ":::",
  "## Design",
  "",
  "A watchdog process restarts the mail worker whenever its heartbeat stops for two minutes.",
  "## Decisions",
  "",
  "### Restart rather than replace the worker",
  "",
  "Restarting keeps the existing worker code and its tested retry logic in place for now.",
  "",
].join("\n");
specRun("spec-writing-head-120", {
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned SiblingCheck.")),
  subagents: {
    SiblingCheck: subagentTranscript(
      `Review the spec of the sibling issue LWEVAL-8.\n\n${siblingSpec}`,
      6.2,
      6.8
    ),
  },
});
// One assignment that pastes the run's own spec and then the sibling's: the run's own spec was
// reviewed, so the write counts as checked, but no finding of its report can be told to be about
// this spec, so its findings are not judged.
specRun("spec-writing-head-125", {
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned BothCheck.")),
  subagents: {
    BothCheck: subagentTranscript(
      `Review both specs.\n\n${outlined()}\n\n# The sibling's spec\n\n${siblingSpec}`,
      6.2,
      6.8
    ),
  },
});
// The run's own spec with far more text pasted after it (the person's messages, three times, and
// instructions): the share is the spec's alone, so the check counts.
specRun("spec-writing-head-126", {
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned EvalCheck.")),
  subagents: {
    EvalCheck: subagentTranscript(
      `Read this spec alone and list each gap.\n\n${outlined()}\n\n# The person's words\n\n${personMessages}\n${personMessages}\n${personMessages}\n\n# How to report\n\nList each gap with its place in the spec and the fix you propose for it.`,
      6.2,
      6.8
    ),
  },
});
// A `task` call of two named tasks, one reviewing the run's own spec and one a sibling's: the call
// checks the write, and its findings are the own-spec task's report alone.
specRun("spec-writing-head-127", {
  transcript: conversation(
    routedRead,
    [
      toolCall("c1", "task", {
        context: "Review the spec in your task and list each gap.",
        tasks: [
          { name: "OwnReview", task: `The spec:\n\n${outlined()}` },
          { name: "SiblingReview", task: `The spec:\n\n${siblingSpec}` },
        ],
      }),
      {
        type: "message",
        message: {
          role: "toolResult",
          toolCallId: "c1",
          content: [{ type: "text", text: "Two tasks completed." }],
          details: {
            results: [
              { id: "OwnReview", exitCode: 0, output: "No gaps." },
              { id: "SiblingReview", exitCode: 0, output: "The watchdog has no owner." },
            ],
          },
        },
      },
    ],
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A `task` call of two named tasks, one reviewing the run's own spec and one a fact check given no
// spec: the fact check is no spec-only check of its own, so the check is the review's report alone.
specRun("spec-writing-head-129", {
  transcript: conversation(
    routedRead,
    [
      toolCall("c1", "task", {
        context: "Read only what your task gives you.",
        tasks: [
          { name: "OwnReview", task: `List each gap in this spec.\n\n${outlined()}` },
          { name: "FactCheck", task: "Check each fact the brief names against the repository." },
        ],
      }),
      {
        type: "message",
        message: {
          role: "toolResult",
          toolCallId: "c1",
          content: [{ type: "text", text: "Two tasks completed." }],
          details: {
            results: [
              { id: "OwnReview", exitCode: 0, output: "No gaps." },
              { id: "FactCheck", exitCode: 0, output: "The sibling's broker claim is false." },
            ],
          },
        },
      },
    ],
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A review of the run's own draft, which the agent then rewrote: the written version keeps the
// draft's Goals, Requirements, Decisions and Acceptance but rewrites its Summary, Problem and Design.
// After the draft the assignment pastes about 4,500 words of unique notes with no heading, so the
// spec's slice runs to the end. Measured over the pasted text, the share of it found in a written
// version falls under the 0.1 floor; measured over the written version (recall), the draft still
// covers most of it.
const rewrittenSpec = outlined({
  summary: " It replaces the old banner, which most terminals cut off at the top of the window.",
  problem:
    "Running the tool today prints a blank line and exits, so a new user cannot tell whether it worked or failed.",
  design:
    "The command reads the name from its first argument, trims spaces at both ends, and refuses an empty name with exit code 2.",
});
const uniqueNotes = Array.from(
  { length: 500 },
  (_, index) => `Record ${index} notes that row ${index * 3 + 1} holds value ${index * 7 + 2}.`
).join("\n");
specRun("spec-writing-head-128", {
  spec: rewrittenSpec,
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned EvalCheck.")),
  subagents: {
    EvalCheck: subagentTranscript(
      `Read this spec alone and list each gap.\n\n${outlined()}\n${uniqueNotes}`,
      6.2,
      6.8
    ),
  },
});
// A subagent that ends with an empty `yield` ({"type":"result"}) hands back the text it wrote just
// before: that text is its report, not the word "result".
specRun("spec-writing-head-121", {
  transcript: evalFirst(evalCell("e1", evalSpawn, "Spawned EvalCheck.")),
  subagents: {
    EvalCheck: [
      ...subagentTranscript(
        `Read this spec alone and list each gap.\n\n${outlined()}`,
        6.2,
        6.6,
        "1. R1 names no owner."
      ),
      {
        type: "message",
        timestamp: second(6.8),
        message: {
          role: "assistant",
          content: [{ type: "toolCall", id: "y1", name: "yield", arguments: { type: "result" } }],
        },
      },
    ],
  },
});
// checked: a write through a helper function of the agent's own shows no command the scorer reads,
// but Dispatch recorded its version for the session. That version is a spec write at its own time
// (version 3 below, at second 12.5 or 14.5, during the eval cell), and the row is flagged.
const helperWrite =
  'import subprocess\ndef edit(issue, ops):\n    subprocess.run(["dispatch", "doc-edit", "--issue", issue, "--ops-json-file", ops])\nedit("LWEVAL-7", "ops.json")';
// No check between the first write and the helper's: unchecked.
specRun("spec-writing-head-100", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    evalCell("e2", helperWrite, ""),
    checkOf("c2"),
    specEdit("w2", 4),
    finish
  ),
  versions: [8.5, 12.5, 16.5],
});
// The one-shot draft the conversation rule forbids: the spec's first version at second 120, the
// question to the person opened at 150 and answered at 170. Every other score passes, so the row
// fails on `asked` and `grew` alone.
specRun("spec-writing-head-140", {
  asks: [
    {
      kind: "question",
      ...openAsk("ask-1"),
      state: "answered",
      created_at: second(150),
      answer: { at: second(170) },
    },
    { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
  ],
});
// A check between them: it counts for the helper's write.
specRun("spec-writing-head-101", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c3"),
    evalCell("e2", helperWrite, ""),
    checkOf("c2"),
    specEdit("w2", 4),
    finish
  ),
  versions: [8.5, 14.5, 18.5],
});
// refusals from the dispatch CLI's ledger: a refusal Dispatch sent back, which the CLI logs as an
// `error` line and prints with no `dispatch` prefix, so a call's output cannot mark it.
const invalidOp = 'operation 0: invalid document operation field "after or before": ';
const hiddenEdit = (id: string) =>
  sh(
    id,
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2>&1 | tail -5",
    0,
    invalidOp,
    [{ tool: "dispatch_doc_edit", error: invalidOp }]
  );
// Hidden by `| tail -5`, and no later call succeeds.
specRun("spec-writing-head-41", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    hiddenEdit("x11")
  ),
});
// The same refusal, fixed by the next call with no skill read between.
specRun("spec-writing-head-42", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    hiddenEdit("x11"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal of a `dispatch` command Python's subprocess.run ran: no command of the call's text.
specRun("spec-writing-head-43", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    sh(
      "x12",
      "python3 -c \"import subprocess; subprocess.run(['dispatch', 'doc-edit', '--issue', 'LWEVAL-7', '--artifact', 'spec', '--ops-json-file', 'ops.json'])\"",
      0,
      invalidOp,
      [{ tool: "dispatch_doc_edit", error: invalidOp }]
    )
  ),
});
// An `eval` cell whose Python ran `dispatch` and was refused before the call reached Dispatch.
// Oh My Pi gives its kernel no session (its environment is an allowlist that holds no
// DISPATCH_SESSION_ID), so the CLI logs nothing, and the cell's code is no shell text: the
// refusal is in the cell's output alone.
const noHost =
  "dispatch: DISPATCH_HOST is not set. The host plugin sets it in an agent's shell. From an agent's session, send the command as a bash call. To run dispatch by hand, set DISPATCH_HOST, and set DISPATCH_SESSION_ID to the id of your own session.\n";
const pythonCall =
  "import subprocess\nr = subprocess.run(['dispatch', 'doc-edit', '--issue', 'LWEVAL-7', '--artifact', 'spec', '--ops-json-file', 'ops.json'], capture_output=True, text=True)\nprint(r.stdout)";
specRun("spec-writing-head-57", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    evalCell("x14", pythonCall, noHost)
  ),
});
// The same refusal, fixed by a later `dispatch` call with no skill read between.
specRun("spec-writing-head-58", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    evalCell("x14", pythonCall, noHost),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// The same refusals printed after the call's exit code, as `print(r.returncode, r.stdout)` prints
// them: an argument check's (exit 1) and a usage refusal's (exit 2). Neither is fixed.
const askPython =
  'import subprocess\nr = subprocess.run(["dispatch", "ask", "--issue", "LWEVAL-7", "--question", "Which name?"], capture_output=True, text=True)\nprint(r.returncode, r.stdout)';
const askRefusal = "dispatch ask was not called: 1 problem\n- --question gives no recommendation";
specRun("spec-writing-head-103", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    evalCell("x16", askPython, `1 ${askRefusal}\n`)
  ),
});
specRun("spec-writing-head-104", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    evalCell(
      "x17",
      pythonCall.replace("print(r.stdout)", "print(r.returncode, r.stderr)"),
      `2 ${noHost}`
    )
  ),
});
// A spec write made from Python as a list-literal call, its answer printed after the exit code.
// The first write is checked; run 91 makes the same write with no check before it.
const pythonWrite =
  'import subprocess\nr = subprocess.run(["dispatch", "doc-edit", "--issue", "LWEVAL-7", "--artifact", "spec", "--ops-json-file", "ops.json"], capture_output=True, text=True)\nprint(r.returncode, r.stdout)';
const pythonApplied = "0 Applied 1 ops (version 2)\nDocument token: sha256:abc\n";
specRun("spec-writing-head-90", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    evalCell("x20", pythonWrite, pythonApplied),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
specRun("spec-writing-head-91", {
  transcript: conversation(
    routedRead,
    evalCell("x20", pythonWrite, pythonApplied),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A run that read a file of the operator's home: nothing stops an agent reading it, so the run may
// have drawn on it. Run 93 names only a URL, an API route, /tmp and relative paths.
specRun("spec-writing-head-92", {
  transcript: conversation(
    routedRead,
    readOf("o1", path.join(homedir(), "src", "other-repo", "README.md")),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
specRun("spec-writing-head-93", {
  transcript: conversation(
    routedRead,
    sh("o2", "curl -s https://example.invalid/v1/messages -o /tmp/m.json; cat ./notes/plan.md"),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A plan the agent writes names the skill file a later task changes: text in a file, no read.
specRun("spec-writing-head-94", {
  transcript: conversation(
    routedRead,
    [
      toolCall("o3", "write", {
        path: "plan.md",
        content: "### Task 9\n- Modify: `skills/dispatch/SKILL.md` in sjawhar/legion.\n",
      }),
      result("o3", "Wrote plan.md"),
    ],
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A subagent's assignment names a path to stay out of: prose for the subagent, flagged as such.
specRun("spec-writing-head-95", {
  transcript: conversation(
    routedRead,
    [
      toolCall("t9", "task", {
        context: `Do not read files under ${path.join(homedir(), "work")} other than via the dispatch command.`,
        tasks: [{ name: "Summary", task: "Summarize the person's messages." }],
      }),
      result("t9", "The person wants a greeting."),
    ],
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A spec about the Dispatch skills names their files, and the check the skill requires pastes it
// whole: the name is prose, flagged and scored. So is a bash heredoc plan that names the file.
specRun("spec-writing-head-105", {
  transcript: conversation(
    routedRead,
    checkOf("c1", outlined({ design: "The change edits `skills/dispatch/SKILL.md`." })),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
specRun("spec-writing-head-106", {
  transcript: conversation(
    routedRead,
    sh("o4", "cat > plan.md <<'EOF'\n### Task 9\n- Modify: skills/dispatch/SKILL.md\nEOF"),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A read of a skill file outside the label's own, by a `read` call or a `cat` of another label's
// copy, is still a rig error.
specRun("spec-writing-head-107", {
  transcript: conversation(
    routedRead,
    sh("o5", "cat /checkouts/base/skills/dispatch/SKILL.md"),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
specRun("spec-writing-head-108", {
  transcript: conversation(
    routedRead,
    readOf("o6", "/opt/legion/skills/dispatch-writing-specs/SKILL.md"),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A run scored from a copy: it ran in another work directory, which its transcript's header (cwd
// `<run dir>/cwd`) and every path it named record. Its own run directory, the work directory's other
// runs and its own HOME's skills are still its own there.
const ranIn = path.join(homedir(), "moved-work", "runs", "spec-writing-head-109");
specRun("spec-writing-head-109", {
  transcript: [
    { ...specHeader[0], cwd: path.join(ranIn, "cwd") },
    ...conversation(
      routedRead,
      sh(
        "o7",
        `cat ${path.join(ranIn, "home", "skills", "dispatch", "SKILL.md")}; cat ${path.join(ranIn, "..", "spec-writing-head-3", "spec.md")}`,
        0,
        "# Dispatch\n# Spec"
      ),
      checkOf("c1"),
      specEdit("w1"),
      askStep,
      checkOf("c2"),
      specEdit("w2"),
      finish
    ).slice(1),
  ],
});
test("a run scored from a copy reads the paths it ran with as its own", () => {
  expect(out).not.toContain("spec-writing-head-109\trig error");
  expect(scoresOf("spec-writing-head-109")).toEqual(allPass);
  expect(rowOf("spec-writing-head-109")).not.toContain(" outside=");
  expect(rowOf("spec-writing-head-109")).not.toContain(" skills=");
});
// A usage refusal the CLI both prints and logs (an unknown command, under tool `dispatch`), after a
// skill read earlier in the same call. The ledger line and the printed refusal are one refusal:
// counted once, at the command, so the read before it is no read between it and its fix.
const unknownCommand =
  "dispatch: unknown command frobnicate; the commands are: issue, issue-update, claim, ask, edit-ask, resolve-ask, resolve-comment, follow, comment, suggest, message, doc-edit, doc-read, request-approval, artifact, read, search, issues, project, projects, architecture-sync, open-asks, whoami\nRun dispatch --help.";
const usageRefused = (name: string, flags = "") =>
  sh(
    "x15",
    `cat ${path.join(runs, name, "home", "skills", "dispatch", "SKILL.md")}; dispatch frobnicate${flags}`,
    2,
    `# Dispatch\n${unknownCommand}`,
    [{ tool: "dispatch", error: unknownCommand }]
  );
// Fixed by the next spec write, with no skill read between.
specRun("spec-writing-head-59", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    usageRefused("spec-writing-head-59"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Never fixed: one refusal block, not two.
specRun("spec-writing-head-60", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    usageRefused("spec-writing-head-60")
  ),
});
// The same unknown command with `--help`: the CLI checks the command before the flag, so it
// refuses and logs it as above. A refused help call is one refusal too, at its command.
specRun("spec-writing-head-64", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    usageRefused("spec-writing-head-64", " --help"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal fixed by a later call that reads a skill file after its dispatch command: that read
// comes after the fix.
const laterSkill = path.join(
  runs,
  "spec-writing-head-44",
  "home",
  "skills",
  "dispatch",
  "SKILL.md"
);
specRun("spec-writing-head-44", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    refused,
    sh(
      "x13",
      `dispatch ask --issue LWEVAL-7 --question 'Which name? Recommendation: Ada.'; cat ${laterSkill}`,
      0,
      "Asked ask-1.\n# Dispatch"
    ),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// No ledger: the CLI logged nothing where the rig reads it.
specRun("spec-writing-head-45", { ledger: false });
// Two calls of one message: a ledger line inside both calls' time is the call whose result came
// first. The hidden refusal is the doc-edit's, in the first call.
const parallel = [
  {
    type: "message",
    message: {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "x14",
          name: "bash",
          arguments: {
            command:
              "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2>&1 | tail -5",
          },
        },
        {
          type: "toolCall",
          id: "x15",
          name: "bash",
          arguments: { command: "dispatch read --issue LWEVAL-7" },
        },
      ],
    },
  },
  { ...(hiddenEdit("x14")[1] ?? {}) },
  { ...(sh("x15", "dispatch read --issue LWEVAL-7", 0, "Title: Greeting")[1] ?? {}) },
];
specRun("spec-writing-head-46", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    parallel
  ),
});
// Judge coverage: each judge's answer covers exactly what judge.ts gave its judge. Each run below
// breaks one judge's file.
const question = (id: string) => ({
  id,
  dominated: false,
  settled: false,
  kind: "taste",
  reason: "A name is taste.",
});
// The approved spec holds R1 and R2; the judge checked R1 alone (the cross-model review's case).
specRun("spec-writing-head-47", {
  spec: outlined().replace(
    "From: your answer on the greeting.",
    "From: your answer on the greeting.\n- **R2.** The greeting ends with a full stop. From: your message on the format."
  ),
});
// questions: ask-1 twice, and an ask it was not given.
specRun("spec-writing-head-48", {
  judges: { "questions.json": { rows: [question("ask-1"), question("ask-1"), question("ask-9")] } },
});
// numbers: no row for the one text it was given, and a row for a seeded ask it was not given.
specRun("spec-writing-head-49", {
  seeded: ["seeded-1"],
  judges: { "numbers.json": { rows: [{ id: "seeded-1", names_number: true, quote: "R1" }] } },
});
// proposal: an ask it was not given.
specRun("spec-writing-head-50", {
  judges: {
    "proposal.json": { rows: [{ item: "the greeting word", verdict: "asked", ask: "ask-9" }] },
  },
});
// preamble: a document and a text it was not given.
specRun("spec-writing-head-51", {
  judges: {
    "preamble.json": {
      rows: [
        { source: "documents/notes.md", passage: "You said so.", kind: "retold" },
        { source: "agent-texts.json#msg-9", passage: "On Monday you said.", kind: "dated" },
      ],
    },
  },
});
// faults: a count that its items do not hold.
specRun("spec-writing-head-52", {
  judges: {
    "faults.json": { ...faultsFile(), faults: { ...noFaults, "A term that nobody explained": 1 } },
  },
});
// findings: a check it was not given (the run's two checks are 1 and 2).
specRun("spec-writing-head-53", {
  judges: { "findings.json": { rows: [{ check: 3, finding: "No owner.", verdict: "fixed" }] } },
});
// findings: no row at all for the run's two checks, as a findings.json judged when the scorer saw
// no check is: not "no finding left", but a judge that never read these checks.
specRun("spec-writing-head-102", { judges: { "findings.json": { rows: [] } } });
// findings: a row for check 1 of the run's two and nothing for check 2, which a judge that skipped
// a check gives; and check 1 both with rows and in `clean`.
specRun("spec-writing-head-122", {
  judges: { "findings.json": { rows: [{ check: 1, finding: "No owner.", verdict: "fixed" }] } },
});
specRun("spec-writing-head-123", {
  judges: {
    "findings.json": {
      rows: [{ check: 1, finding: "No owner.", verdict: "fixed" }],
      clean: [1, 2],
    },
  },
});
// findings: both checks reported nothing, and the judge says so in `clean`: no finding is left.
specRun("spec-writing-head-124", { judges: { "findings.json": { rows: [], clean: [1, 2] } } });
// Spec writes: only a write Dispatch made, of the run's own primary spec, counts. Each call below is
// something else, and the run's two checked writes still stand. Each result is the one Dispatch
// gives, so the calls that Dispatch did apply are left out for what they name, not for their result.
const applied = "Applied 1 ops (version 2)\nDocument token: sha256:abc";
const notWrites = [
  // The help of doc-edit, piped as the agent did.
  sh("n1", "dispatch doc-edit --help 2>&1 | head -40", 0, "Usage: dispatch doc-edit [flags]"),
  // An edit of another issue's spec, and another issue made with a spec.
  sh("n2", "dispatch doc-edit --issue WS-1 --artifact spec --ops-json '[]'", 0, applied),
  sh(
    "n3",
    "dispatch issue --project WS --title 'Orchestration' --spec-file - <<'EOF'\nIt tracks one stream.\nEOF",
    0,
    "Created WS-1: Orchestration → http://127.0.0.1:1/issues/WS-1; unassigned"
  ),
  // An edit of the run's issue's other document.
  sh("n4", "dispatch doc-edit --issue LWEVAL-7 --artifact plan.md --ops-json '[]'", 0, applied),
  // A failed edit of the run's spec: the tool refused it.
  sh(
    "n5",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
    1,
    "refused"
  ),
  // An edit the CLI refused before calling Dispatch: bash exits 0 through `2>&1`, and the result
  // says `was not called`.
  sh(
    "n6",
    'dispatch doc-edit --issue LWEVAL-7 --artifact spec --precondition-json \'{"document":"sha\' 2>&1',
    0,
    "dispatch doc-edit was not called: 1 problem\n- --precondition-json is not JSON"
  ),
  // A dry run, and an edit that changed nothing and so made no version.
  sh("n7", "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json --dry-run"),
  sh(
    "n8",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
    0,
    "Applied 1 ops; nothing changed (no new version)"
  ),
  // An edit by a ref to another issue's spec.
  sh("n9", "dispatch doc-edit --ref dispatch://WS-1/spec --ops-json '[]'", 0, applied),
  // The help and a dry run of the run's spec, each in one call with an edit Dispatch applied to
  // another issue: that edit's answer is no write of the help or the dry run.
  sh(
    "n10",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --help | head -5; dispatch doc-edit --issue WS-1 --artifact spec --ops-json '[]'",
    0,
    applied
  ),
  sh(
    "n11",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json --dry-run && dispatch doc-edit --issue WS-2 --artifact spec --ops-json '[]'",
    0,
    applied
  ),
  // Uploads that are no spec write: the run's plan, another issue's spec.md, and the run's spec.md
  // that Dispatch refused.
  sh(
    "n12",
    "dispatch artifact --issue LWEVAL-7 --name plan.md --content-file plan.md",
    0,
    "Uploaded plan.md as version 1 (artifact slug plan-md; dispatch://LWEVAL-7/artifact/plan-md)"
  ),
  sh(
    "n13",
    "dispatch artifact --issue WS-1 --name spec.md --content-file spec.md",
    0,
    "Uploaded spec.md as version 2 (artifact slug spec-md; dispatch://WS-1/artifact/spec-md)"
  ),
  sh(
    "n14",
    "dispatch artifact --issue LWEVAL-7 --name spec.md --content-file spec.md 2>&1",
    0,
    "dispatch artifact was not called: 1 problem\n- --content-file spec.md does not exist"
  ),
];
specRun("spec-writing-head-21", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    ...notWrites,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// The spec writes of the run's issue that do count: `dispatch issue` that Dispatch answered with the
// run's issue made, the spec by its artifact id, by a dispatch:// ref, as an uploaded spec.md, by a
// dashboard URL, in a shell whose last command failed after Dispatch made the version, and with no
// document named, by `--issue` alone and by a bare issue ref. Each has its check before it.
specRun("spec-writing-head-22", {
  transcript: conversation(
    routedRead,
    checkOf("c0"),
    sh(
      "w0",
      "dispatch issue --project LWEVAL --title 'Greeting' --spec-file spec.md",
      0,
      "Created LWEVAL-7: Greeting → http://127.0.0.1:1/issues/LWEVAL-7; unassigned"
    ),
    checkOf("c1"),
    sh(
      "w1",
      "dispatch doc-edit --issue LWEVAL-7 --artifact art-spec --ops-json-file ops.json",
      0,
      applied
    ),
    checkOf("c2"),
    sh(
      "w2",
      "dispatch doc-edit --ref dispatch://LWEVAL-7/artifact/spec --ops-json-file ops.json",
      0,
      "Applied 2 ops (version 3)"
    ),
    checkOf("c3"),
    sh(
      "w3",
      "dispatch artifact --issue LWEVAL-7 --name spec.md --content-file spec.md",
      0,
      "Uploaded spec.md as version 4 (artifact slug spec-md; dispatch://LWEVAL-7/artifact/spec-md)"
    ),
    checkOf("c4"),
    sh(
      "w4",
      "dispatch doc-edit --ref http://127.0.0.1:1/issues/LWEVAL-7/spec --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 5)"
    ),
    checkOf("c5"),
    sh(
      "w5",
      "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2>&1; dispatch doc-read --issue LWEVAL-7 --artifact spec | grep -c TODO",
      1,
      "Applied 1 ops (version 6)\nDocument token: sha256:abc\n0"
    ),
    // No document named: the CLI edits the issue's primary document, the spec.
    checkOf("c6"),
    sh(
      "w6",
      "dispatch doc-edit --issue LWEVAL-7 --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 7)"
    ),
    checkOf("c7"),
    sh(
      "w7",
      "dispatch doc-edit --ref dispatch://LWEVAL-7 --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 8)"
    ),
    finish
  ),
});
// Copies a 30-word passage: one run.
const thirtyWords = folded(longMessage).split(" ").slice(0, 30).join(" ");
specRun("spec-writing-head-13", {
  spec: outlined({
    design:
      "The greeting must stay on one line, use the name exactly as the person typed it, end with a full stop, and never print the name twice even when the CLI starts.",
  }),
  judges: copied("spec.md", thirtyWords, "names"),
});
// A document that copies the person's message.
specRun("spec-writing-head-14", {
  documents: { "brief.md": `# Brief\n\n${askedFor}\n` },
  judges: copied("documents/brief.md", folded(askedFor), "copies"),
});
// Names the run's session.
specRun("spec-writing-head-15", {
  spec: outlined({ summary: ` Session ${specSession.slice(0, 8)} wrote it.` }),
});
// A seeded ask: agent-asks.json and agent-texts.json hold it, and state-1.json lists it as the
// only open ask. judge.ts gave no judge the seeded ask, so the judges' files cover the rest alone.
specRun("spec-writing-head-16", {
  seeded: ["seeded-1"],
  states: [
    { turn: 1, open_asks: [openAsk("seeded-1")], approval: null },
    ...approvedStates.slice(1),
  ],
  asks: [
    { kind: "question", ...openAsk("seeded-1"), state: "answered" },
    { kind: "question", ...openAsk("ask-1"), state: "answered" },
    { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
  ],
  texts: [
    { id: "seeded-1", kind: "ask", body: "Which name should the example use?", created_at: "" },
    { id: "msg-1", kind: "message", body: "The spec is ready to read.", created_at: "" },
  ],
});
// A check whose assignment names a path under the label's checkout.
specRun("spec-writing-head-17", {
  transcript: conversation(
    routedRead,
    checkOf("c1", `${outlined()}\nCompare /checkouts/head/packages/cli/greet.ts.`),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Ended with no approval.
specRun("spec-writing-head-18", {
  states: [approvedStates[0] ?? {}, { turn: 2, open_asks: [], approval: null }],
  turns: "1 first exit=0\n2 answered exit=0\n",
  judges: { "checkable.json": { version: null, rows: [] } },
});
// A label of two runs that both pass, one of them flagged: it named a file of the operator's home.
specRun("spec-writing-flagged-1", {
  transcript: conversation(
    routedRead,
    readOf("o1", path.join(homedir(), "src", "other-repo", "README.md")),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
specRun("spec-writing-flagged-2");
// Three runs of another label, for the median fault count.
for (const [n, faults] of [2, 4, 3].entries())
  specRun(`spec-writing-base-${n + 1}`, {
    judges: { "faults.json": faultsFile({ "The reader cannot follow a sentence": faults }) },
  });

/** What `score.ts runs` printed over the staged world, and the code it exited with. Both are
 *  read in a hook, below the one that stages that world. */
let out = "";
let scoredExitCode: number | null = null;
// Bounded as the staging hook at the top of this file is, and for the same measured reason.
beforeAll(() => {
  const scored = Bun.spawnSync(["bun", path.join(import.meta.dir, "score.ts"), "runs", runs]);
  out = scored.stdout.toString();
  scoredExitCode = scored.exitCode;
}, HOOK_WORK_TIMEOUT_MS);

test("a run whose agent never got a model turn is a rig error, not a failure", () => {
  expect(scoredExitCode).toBe(0);
  for (const name of ["tester-proof-head-1", "ask-on-message-head-1"]) {
    expect(out).toContain(`${name}\trig error: `);
    expect(out).not.toContain(`${name}\tpass=`);
  }
  // Pass counts over the one run that got a turn; the other is counted only as a rig error.
  expect(out).toContain("tester-proof\thead\t0/1\t0/1\t1\t0\n");
  expect(out).toContain("ask-on-message\thead\t0/1\t0/1\t1\t0\n");
});

test("a run whose agent got a turn and did nothing is a failure", () => {
  for (const name of ["tester-proof-head-2", "ask-on-message-head-2"]) {
    expect(out).toContain(`${name}\tpass=false\t`);
  }
});

test("the tester ran the CLI when the bun stand-in recorded a run of greet.ts, not when a command named it", () => {
  expect(out).toMatch(/tester-proof-ran-1\tpass=false\tref=false\tran=true /);
  expect(out).toMatch(/tester-proof-ran-2\tpass=false\tref=false\tran=false /);
});

test("a tester passes when its completion names the test record its own session wrote first", () => {
  expect(out).toMatch(
    /tester-proof-rec-1\tpass=true\tref=false\tran=true record=v1 proof=true pushes=0 complete=true refusedCompletions=1 ordered=true testerLine=true ownHead=true editBeforeRecord=false /
  );
  expect(out).toContain("tester-proof\trec\t1/4\t0/4\t0\t0\n");
});

test("a record another session wrote fails the proof, even when the stand-in accepted it", () => {
  expect(out).toMatch(/tester-proof-rec-2\tpass=false\t.* proof=false .* ordered=true /);
  expect(out).toContain(
    "session legion-daemon:lweval wrote the record, not this run's session 01a0f2cc"
  );
});

test("a completion naming another version than the record's is out of order", () => {
  expect(out).toMatch(/tester-proof-rec-3\tpass=false\t.* proof=true .* ordered=false /);
});

test("a push that changes the code under the PR's head fails the proof", () => {
  expect(out).toMatch(/tester-proof-rec-4\tpass=false\t.* proof=false pushes=1 .* ordered=true /);
  expect(out).toContain("the push changed [greet.ts]");
});

// The tester writes its handoff body whole, and its prompt names no time (Dispatch records when the
// handoff was written): a body with no `completed` key is what the prompt asks for, so the schema
// reads it and the run is scored. Before the prompt dropped the time, the schema demanded
// `completed`, so such a body failed the run's proof check (`proofProblems`) and scored `pass=false`.
test("a tester handoff that carries no completed time is read, and one with no issue is not", () => {
  const handoff = {
    schemaVersion: 1,
    phase: "test",
    issue: "LWEVAL-1",
    implementerProof: { verdict: "verified", how: "ran `bun greet.ts Ada` at the head" },
    proof: [
      {
        criterion: "`bun greet.ts <name>` prints the greeting it returns",
        surface: "the CLI",
        command: "bun greet.ts Ada",
        observed: "prints Hello, Ada! and exits 0",
        headSha: "0f2cc1d4e8b0a1f6c7d2e3b4a5968778695a4b3c",
        negativeControl: "bun greet.ts → exit 2, usage: greet.ts <name> on stderr",
      },
    ],
  };
  expect(TestHandoff.safeParse(handoff).success).toBe(true);
  const { issue: _issue, ...noIssue } = handoff;
  expect(TestHandoff.safeParse(noIssue).success).toBe(false);
});

// The scorer reads every label's runs, and each label's agent gets the role prompt of its own
// checkout (`worker-pane.ts` composes it from `<checkout>/packages/daemon`). So the documented
// comparison `rig.sh batch tester-proof 5 head base` has the base label's tester write the shape
// its own checkout's prompt spells — since sjawhar/legion#2585 a `previousHandoff` naming the
// commit before it, and a `completed` time in the prompt before that — and the head label's
// tester write this one, which carries neither. A schema that refuses any of those shapes scores
// that label `pass=false` for a reason that is not the tester-proof rule, which makes the
// comparison measure the prompt change rather than the rule.
test("a tester handoff of a checkout whose prompt asks for previousHandoff is read too", () => {
  const handoff = {
    schemaVersion: 1,
    phase: "test",
    issue: "LWEVAL-1",
    previousHandoff: "4f1b8a0d1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f60",
    implementerProof: { verdict: "verified", how: "ran `bun greet.ts Ada` at the head" },
    proof: [
      {
        criterion: "`bun greet.ts <name>` prints the greeting it returns",
        surface: "the CLI",
        command: "bun greet.ts Ada",
        observed: "prints Hello, Ada! and exits 0",
        headSha: "0f2cc1d4e8b0a1f6c7d2e3b4a5968778695a4b3c",
        negativeControl: "bun greet.ts → exit 2, usage: greet.ts <name> on stderr",
      },
    ],
  };
  expect(handoff).not.toHaveProperty("completed");
  expect(TestHandoff.safeParse(handoff).success).toBe(true);
  const { implementerProof: _proof, ...noVerdict } = handoff;
  expect(TestHandoff.safeParse(noVerdict).success).toBe(false);
});

// The same for the shape before #2585, which a checkout older than it still spells.
test("a tester handoff of a checkout whose prompt still asks for a completed time is read too", () => {
  const handoff = {
    schemaVersion: 1,
    phase: "test",
    issue: "LWEVAL-1",
    completed: "2026-10-31T01:30:00Z",
    implementerProof: { verdict: "verified", how: "ran `bun greet.ts Ada` at the head" },
    proof: [
      {
        criterion: "`bun greet.ts <name>` prints the greeting it returns",
        surface: "the CLI",
        command: "bun greet.ts Ada",
        observed: "prints Hello, Ada! and exits 0",
        headSha: "0f2cc1d4e8b0a1f6c7d2e3b4a5968778695a4b3c",
        negativeControl: "bun greet.ts → exit 2, usage: greet.ts <name> on stderr",
      },
    ],
  };
  expect(TestHandoff.safeParse(handoff).success).toBe(true);
  const { implementerProof: _proof, ...noVerdict } = handoff;
  expect(TestHandoff.safeParse(noVerdict).success).toBe(false);
});

test("a tool call reading the other label's checkout is a rig error wherever the path falls in it", () => {
  expect(out).toContain(
    "tester-proof-cross-1\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
  );
});

// Catches the head arm passing on knowledge of its fixture: the label rule above refuses a read
// of the other label's checkout only, and the rig runs from the head checkout, so a head run
// reading the stand-in or its own world.json went scored. Measured on 2026-11-12 in
// trial-g-head-2, which did both before its record and its re-run and scored a pass.
test("a run that reads the rig or its own record of the world is a rig error, whatever its label", () => {
  expect(out).toContain(
    `trial-i-head-191\trig error: a read call reads the rig itself: ${path.join(import.meta.dir, "gh-standin.ts")}`
  );
  expect(out).toContain(
    "trial-i-head-192\trig error: a bash call reads its run's record of the world: world.json"
  );
  // The run's own working copy is its own: scored as the whole rule followed.
  expect(rowOf("trial-i-head-193").split("\n")[0]).toContain("pass=true");
});

// Catches: a fixture rule that takes a file the run wrote itself for the rig's record of its
// world. On 2026-11-15 trial-i-ablate-2 made <run>/tmp-reviewer/ beside its TMPDIR, filled
// body.json there with `cat >` and went unscored for it.
test("a file the run wrote whole under its run directory is its own to read", () => {
  // Catches: a read refused although a `write` call of the run made the file before it.
  expect(rowOf("trial-i-head-194").split("\n")[0]).toContain("pass=true");
  // Catches: `cat >` taken for a read of the file it fills, which unscored trial-i-ablate-2.
  expect(rowOf("trial-i-head-195").split("\n")[0]).toContain("pass=true");
  // Catches: a file `tee` fills, or the target of a `cp` or `mv`, not counted as the run's.
  expect(rowOf("trial-i-head-196").split("\n")[0]).toContain("pass=true");
  // Catches: `jq`, `python3`, `node`, `bun` or a path through the run directory's variable
  // refusing a file the run wrote whole, which the rule since dc588d2c leaves the run's own.
  expect(rowOf("trial-i-head-207").split("\n")[0]).toContain("pass=true");
});

test("a run still may not read the rig's record of its world, nor a path it never wrote whole", () => {
  const record = (name: string, ...file: string[]) =>
    `${name}\trig error: a bash call reads its run's record of the world: ${path.join(runs, name, ...file)}`;
  // Catches: a rule that lets a run read a file rig.sh wrote beside the agent's directories.
  expect(out).toContain(record("trial-i-head-197", "worker.env"));
  // Catches: a rule that lets a run read any path outside the directories rig.sh makes.
  expect(out).toContain(record("trial-i-head-198", "scratch-x", "body.json"));
  // Catches: a write after the read counted for it, so a run reads the record and covers it after.
  expect(out).toContain(record("trial-i-head-199", "scratch-x", "body.json"));
  // Catches: an append counted as writing the file whole, though the rig's text stays in it.
  expect(out).toContain(record("trial-i-head-200", "worker.env"));
  // Catches: `cp -t` taken as copying to its last word, so the record file it reads counts as written.
  expect(out).toContain(record("trial-i-head-201", "world.json"));
});

// Measured runs read fixtures.json with `cd <run> && jq -r '.routes[] | .match' fixtures.json`
// and with `cat "$SKILL_SCENARIO_RUN/fixtures.json"`, and both went scored.
test("a jq, python3, node or bun read of the record, or one through the run's variable, is refused", () => {
  const record = (name: string, file: string) =>
    `${name}\trig error: a bash call reads its run's record of the world: ${path.join(runs, name, file)}`;
  // Catches: a reader list without `jq`, and a command whose text ends at the `|` inside its
  // quoted filter, short of the file it names.
  expect(out).toContain(
    "trial-i-head-202\trig error: a bash call reads its run's record of the world: fixtures.json"
  );
  // Catches: a reader list without `python3`, and a command whose text ends at the `;` inside
  // its quoted program.
  expect(out).toContain(record("trial-i-head-203", "fixtures.json"));
  // Catches: `$SKILL_SCENARIO_RUN` left as it is written, which names no absolute path.
  expect(out).toContain(record("trial-i-head-204", "fixtures.json"));
  // Catches: a reader list without `node`, or without `bun`.
  expect(out).toContain(record("trial-i-head-205", "world.json"));
  expect(out).toContain(record("trial-i-head-206", "calls.jsonl"));
});

test("a skill file outside the label's own is a rig error when read, and a flag when only named", () => {
  // Named in a check's pasted spec, and in a heredoc plan: scored, and flagged where it sits.
  expect(scoresOf("spec-writing-head-105")).toEqual(allPass);
  expect(rowOf("spec-writing-head-105").split("\n")[0]).toContain(
    " skills=ran:0,wrote:0,assigned:1"
  );
  expect(rowOf("spec-writing-head-105")).toContain(
    "  skills (assigned): a task call names skills/dispatch: …"
  );
  expect(scoresOf("spec-writing-head-106")).toEqual(allPass);
  expect(rowOf("spec-writing-head-106").split("\n")[0]).toContain(
    " skills=ran:1,wrote:0,assigned:0"
  );
  // Read: by a `cat` of another label's copy, and by a `read` call of a skill elsewhere.
  expect(out).toContain(
    "spec-writing-head-107\trig error: a bash call reads base's checkout: /checkouts/base/skills/dispatch/SKILL.md"
  );
  expect(out).toContain(
    "spec-writing-head-108\trig error: a read call reads a skill outside head's own: /opt/legion/skills/dispatch-writing-specs/SKILL.md"
  );
  // The run's own copy, in its HOME, is no hit.
  expect(rowOf("spec-writing-head-29")).not.toContain(" skills=");
});

test("a run whose tool call names a path outside its sandbox is scored and flagged, saying where", () => {
  const outside = path.join(homedir(), "src", "other-repo", "README.md");
  // Flagged, never unscored: the run keeps its scores, and its row says what it named and where.
  expect(out).not.toContain("spec-writing-head-92\trig error");
  expect(scoresOf("spec-writing-head-92")).toEqual(allPass);
  const row = rowOf("spec-writing-head-92");
  expect(row).toContain(" outside=ran:1,wrote:0,assigned:0");
  expect(row).toContain(`  outside (ran): a read call names ${outside}: …`);
  expect(row).toContain("That does not show it read the path, nor that its result is wrong.");
  // A URL, an API route, /tmp and relative paths are no outside path.
  expect(scoresOf("spec-writing-head-93")).toEqual(allPass);
  expect(rowOf("spec-writing-head-93")).not.toContain(" outside=");
  // A skill path inside the text a write puts in a file is no read of that skill.
  expect(scoresOf("spec-writing-head-94")).toEqual(allPass);
  // A path in a subagent's assignment is flagged as `assigned`, apart from a command it ran.
  expect(scoresOf("spec-writing-head-95")).toEqual(allPass);
  expect(rowOf("spec-writing-head-95")).toContain(" outside=ran:0,wrote:0,assigned:1");
  expect(rowOf("spec-writing-head-95")).toContain("  outside (assigned): a task call names ");
});

test("two runs of one batch that name one Dispatch database are rig errors, saying so", () => {
  expect(out).toContain(
    "ask-on-message-shared-1\trig error: it shares Dispatch database run_shared of Postgres pg-7 with ask-on-message-shared-2, so each could read the other's issue and asks"
  );
  expect(out).toContain(
    "ask-on-message-shared-2\trig error: it shares Dispatch database run_shared of Postgres pg-7 with ask-on-message-shared-1, so each could read the other's issue and asks"
  );
  expect(out).toContain("ask-on-message\tshared\t0/0\t0/0\t2\t0\n");
});

test("runs on databases of their own are scored, and one name in two batches is two databases", () => {
  for (const name of [
    "ask-on-message-apart-1",
    "ask-on-message-apart-2",
    "ask-on-message-batches-1",
    "ask-on-message-batches-2",
  ])
    expect(out).toContain(`${name}\tpass=false\t`);
});

test("a run that records no Dispatch of its own is a rig error", () => {
  for (const name of ["ask-on-message-unrecorded-1", "askgate-facts-unrecorded-1"]) {
    expect(out).toContain(
      `${name}\trig error: it records no Dispatch of its own (no dispatch.json)`
    );
    expect(out).not.toContain(`${name}\tpass=`);
  }
});

test("a rules-case askgate run with no dispatch.json is a rig error", () => {
  expect(out).toContain(
    "askgate-nochange-unrecorded-1\trig error: it records no Dispatch of its own (no dispatch.json)"
  );
  expect(out).not.toContain("askgate-nochange-unrecorded-1\tpass=");
  expect(out).toContain("askgate:nochange\tunrecorded\t0/0\t0/0\t1\t0\n");
});

test("a rules-case askgate run with the dispatch.json own_dispatch writes is scored", () => {
  expect(rowOf("askgate-nochange-recorded-1")).toMatch(
    /^askgate-nochange-recorded-1\tpass=true\t.*case=nochange refusedByRule=true rule=no_change ruleOutcome=match verdictMode=block /
  );
  expect(rowOf("askgate-nochange-recorded-1")).not.toContain("no dispatch.json");
  expect(out).not.toContain("askgate-nochange-recorded-1\trig error");
  expect(out).toContain("askgate:nochange\trecorded\t1/1\t0/1\t0\t0\n");
});

test("an older spec run with no dispatch.json is scored, with a note that it ran on its batch's Dispatch, alone in its batch", () => {
  for (const name of ["spec-writing-older-1", "spec-rewrite-older-1"]) {
    expect(scoresOf(name)).toEqual(allPass);
    expect(rowOf(name)).toContain(
      "\n  no dispatch.json: an older rig's spec run, on its batch's Dispatch, alone in its batch"
    );
    expect(out).not.toContain(`${name}\trig error`);
  }
  expect(out).toContain("spec-writing\tolder\t1/1\t1/1\t0\t0\n");
  expect(out).toContain("spec-rewrite\tolder\t1/1\t1/1\t0\t0\n");
  // A spec run that records its own Dispatch gets no such note.
  expect(rowOf("spec-writing-head-1")).not.toContain("no dispatch.json");
});

test("a measure-before-ask ask passes when it carries the population and the measurement and drops the option they rule out", () => {
  expect(out).toMatch(
    /measure-before-ask-head-1\tpass=true\t.*population=true measured=true dropped=true /
  );
  expect(out).toMatch(/measure-before-ask-head-2\tpass=false\t.*dropped=false /);
  expect(out).toMatch(
    /measure-before-ask-head-3\tpass=false\t.*population=false measured=false dropped=true /
  );
  expect(out).toContain("measure-before-ask\thead\t1/3\t");
});

test("a brainstorm run leaks when a tool's result carries the token, not only its arguments", () => {
  expect(out).toMatch(/brainstorm-head-1\tpass=true\t.*surface=spec .*leak=false /);
  expect(out).toMatch(/brainstorm-head-2\tpass=false\t.*surface=spec .*leak=true /);
});

test("a brainstorm run created its issue when Dispatch answered Created, whatever wrapped the call", () => {
  // From inside an `eval` cell, with no top-level `dispatch issue` command.
  expect(out).toMatch(/brainstorm-head-3\tpass=true\t.*surface=spec .*leak=false /);
  // Ran `dispatch issue`, but Dispatch refused it as a duplicate.
  expect(out).toMatch(/brainstorm-head-4\tpass=false\t.*surface=neither /);
});

test("askgate on, facts: passes when the gate refused the ask in block mode for failure 12, naming both facts", () => {
  expect(out).toMatch(/askgate-facts-on-1\tpass=true\t.*case=facts .*failure12=true /);
  expect(out).toMatch(/askgate-facts-on-2\tpass=false\t.*verdictMode=warn /);
  expect(out).toMatch(/askgate-facts-on-3\tpass=false\t.*age=false contributors=false /);
});

// Catches: a facts score that finds the two facts anywhere in the refusal, which quotes the
// question for every failure, so a block for failure 13 passed.
test("askgate on, facts: reads the refused entry's own reason, and needs failure 12 in it", () => {
  expect(out).toMatch(
    /askgate-facts-on-7\tpass=false\t.*refused=true failure12=false age=true contributors=true verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-facts-on-8\tpass=true\t.*refused=true failure12=true age=true contributors=true verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-facts-on-9\tpass=true\t.*refused=true failure12=true age=true contributors=true verdictMode=block /
  );
});

test("askgate on, facts: reads the first ask that sends, past one with --help or --dry-run", () => {
  expect(out).toMatch(
    /askgate-facts-on-4\tpass=true\t.*refused=true failure12=true age=true contributors=true verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-facts-on-5\tpass=true\t.*refused=true failure12=true age=true contributors=true verdictMode=block /
  );
});

test("askgate on, facts: reads the first ask the gate reviewed, past an unreadable one", () => {
  expect(out).toMatch(
    /askgate-facts-on-6\tpass=true\t.*refused=true failure12=true age=true contributors=true verdictMode=block /
  );
});

test("askgate on, facts: reads the first ask the gate reviewed, past one the CLI refused as unmarked", () => {
  expect(out).toMatch(
    /askgate-facts-on-10\tpass=true\t.*refused=true failure12=true age=true contributors=true verdictMode=block /
  );
});

test("askgate on, link: passes when the ask exists and the gate allowed it on a verdict", () => {
  expect(out).toMatch(/askgate-link-on-1\tpass=true\t.*asks=1 allowed=1 revised=0 /);
  expect(out).toMatch(/askgate-link-on-2\tpass=false\t.*revised=1 /);
  expect(out).toMatch(/askgate-link-on-3\tpass=false\t.*asks=0 /);
});

test("askgate on, link: an unreadable refusal is not the gate sending the ask back", () => {
  expect(out).toMatch(/askgate-link-on-4\tpass=true\t.*asks=1 allowed=1 revised=0 /);
});

// Catches: a link score that passes an ask the gate never reviewed: a timeout, an error, a skip, an
// override or no entry at all left the ask on the issue with no revise, and that passed.
test("askgate on, link: an ask the gate did not allow on a verdict fails, whatever let it through", () => {
  for (const n of [5, 6, 7, 8, 9])
    expect(out).toMatch(new RegExp(`askgate-link-on-${n}\\tpass=false\\t.*asks=1 allowed=0 `));
  expect(out).toMatch(/askgate-link-on-5\t.*outcomes=\["timeout"\] /);
  expect(out).toMatch(/askgate-link-on-9\t.*outcomes=\[\] /);
});

test("askgate on, upload: reads the first reviewed upload, past an unreadable one", () => {
  expect(out).toMatch(
    /askgate-upload-on-1\tpass=true\t.*decision=revise verdictMode=block fix=true /
  );
  expect(out).toMatch(/askgate-upload-on-2\tpass=false\t.*decision=allow /);
  expect(out).toMatch(/askgate-upload-on-3\tpass=false\t.*fix=false /);
});

test("askgate on, nochange, nochange-block and untried: pass when the rules check refused the call, in either mode", () => {
  expect(out).toMatch(
    /askgate-nochange-on-1\tpass=true\t.*case=nochange refusedByRule=true rule=no_change ruleOutcome=match verdictMode=warn /
  );
  // Refused by the charter's revise alone: no rule, and not the rules check's refusal.
  expect(out).toMatch(
    /askgate-nochange-on-2\tpass=false\t.*refusedByRule=false rule=none ruleOutcome=none verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-nochange-on-3\tpass=false\t.*refusedByRule=false rule=none ruleOutcome=error /
  );
  // A device write, read past the piped edit the CLI refused with no marker.
  expect(out).toMatch(
    /askgate-nochange-block-on-1\tpass=true\t.*case=nochange-block refusedByRule=true rule=no_change ruleOutcome=match verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-nochange-block-on-2\tpass=false\t.*case=nochange-block refusedByRule=false rule=none /
  );
  expect(out).toMatch(
    /askgate-untried-on-1\tpass=true\t.*case=untried refusedByRule=true rule=untried_access ruleOutcome=match verdictMode=block ranRead=false /
  );
  expect(out).toMatch(
    /askgate-untried-on-2\tpass=false\t.*case=untried refusedByRule=false rule=none ruleOutcome=none verdictMode=block ranRead=true /
  );
});

test("askgate on, nochange-rebuttal and tried: pass when the ask posted and no entry refused it, in the entry's own mode", () => {
  expect(out).toMatch(
    /askgate-nochange-rebuttal-on-1\tpass=true\t.*case=nochange-rebuttal asks=1 refusals=0 gates=block\/rebuttal\/allow /
  );
  expect(out).toMatch(
    /askgate-nochange-rebuttal-on-2\tpass=false\t.*asks=1 refusals=1 gates=warn\/verdict\/revise\/no_change,warn\/rebuttal\/allow /
  );
  expect(out).toMatch(
    /askgate-tried-on-1\tpass=true\t.*case=tried asks=1 refusals=0 gates=block\/verdict\/allow ranRead=true /
  );
  // One charter revise: in warn mode a note on a call that went out, in block mode a refusal.
  expect(out).toMatch(
    /askgate-tried-on-2\tpass=true\t.*refusals=0 gates=warn\/verdict\/revise ranRead=true /
  );
  expect(out).toMatch(
    /askgate-tried-on-3\tpass=false\t.*refusals=1 gates=block\/verdict\/revise,block\/verdict\/allow /
  );
  // The rules check refused the ask that carried the read's error: a false positive.
  expect(out).toMatch(
    /askgate-tried-on-4\tpass=false\t.*refusals=1 gates=block\/verdict\/revise\/untried_access,block\/rebuttal\/allow /
  );
});

test("askgate off, facts: passes when the ask exists and the gate wrote nothing", () => {
  expect(out).toMatch(/askgate-facts-off-1\tpass=true\t.*asks=1 gateEntries=0 /);
  expect(out).toMatch(/askgate-facts-off-2\tpass=false\t.*gateEntries=1 /);
  expect(out).toMatch(/askgate-facts-off-3\tpass=false\t.*asks=0 /);
});

test("askgate: another label and case pair, a directory naming no case, and a case.txt that disagrees are unscored, saying why", () => {
  expect(out).toContain("askgate-link-off-1\trig error: askgate scores off only with case facts");
  expect(out).toContain(
    "askgate-on-1\trig error: the directory names no case: an askgate run's directory is askgate-<case>-<label>-<n>"
  );
  expect(out).toContain(
    "askgate-upload-on-4\trig error: case.txt names 'facts', but the run's directory names upload"
  );
});

test("askgate: a first attempt no review stands behind is no review of the case's call", () => {
  // facts and link read past it (above); nochange-rebuttal leaves the unmarked entry out of its
  // refusals, and counts it apart.
  expect(out).toMatch(
    /askgate-nochange-rebuttal-on-3\tpass=true\t.*refusals=0 gates=block\/unmarked\/revise,block\/rebuttal\/allow notReviewed=1 /
  );
});

test("askgate: the counts are per case and label", () => {
  expect(out).toContain("askgate:facts\ton\t7/10\t0/10\t0\t0\n");
  expect(out).toContain("askgate:link\ton\t2/9\t0/9\t0\t0\n");
  expect(out).toContain("askgate:upload\ton\t1/3\t0/3\t1\t0\n");
  expect(out).toContain("askgate:nochange\ton\t1/3\t0/3\t0\t0\n");
  expect(out).toContain("askgate:nochange-block\ton\t1/2\t0/2\t0\t0\n");
  expect(out).toContain("askgate:untried\ton\t1/2\t0/2\t0\t0\n");
  expect(out).toContain("askgate:nochange-rebuttal\ton\t2/3\t0/3\t0\t0\n");
  expect(out).toContain("askgate:tried\ton\t2/4\t0/4\t0\t0\n");
  expect(out).toContain("askgate:facts\toff\t1/3\t0/3\t0\t0\n");
  expect(out).toContain("askgate:link\toff\t0/0\t0/0\t1\t0\n");
  expect(out).toContain("askgate\ton\t0/0\t0/0\t1\t0\n");
});

// live-read: the pinned Oh My Pi answers a `read` of skill://<name>[/<path>] with one header line,
// `[Skill file: <absolute path>]`, then the file's text. The header is not the file's.
const liveRun = path.join(work, "live-read");
const liveSkills = path.join(work, "live-skills");
const liveLogs = path.join(work, "live-logs");
const skillText = "---\nname: demo\n---\n\n# Demo\n\nRead the notes.\n";
const notesText = "# Notes\n\nOne line of notes.\n";
const skillHeader = "[Skill file: /profiles/s1/agent/skills/demo";
/** A `read` tool's result, answering the call `toolCallId` names. */
const readResult = (toolCallId: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolName: "read", toolCallId, content: [{ type: "text", text }] },
});
const skillWhole = skillText.replace(/\n$/, "");
const notesWhole = notesText.replace(/\n$/, "");
const otherHeader = `[Read: references/notes.md]\n${notesWhole}`;
// Staged in a hook, as every fixture of this file is: these three directories live under `work`,
// which the hook at the top of the file makes.
beforeAll(() => {
  mkdirSync(path.join(liveRun, "sessions"), { recursive: true });
  mkdirSync(path.join(liveSkills, "demo", "references"), { recursive: true });
  mkdirSync(liveLogs, { recursive: true });
  writeFileSync(path.join(liveSkills, "demo", "SKILL.md"), skillText);
  writeFileSync(path.join(liveSkills, "demo", "references", "notes.md"), notesText);
  writeFileSync(
    path.join(liveRun, "sessions", "2026-10-24T10-00-00-000Z_01a0f2cc.jsonl"),
    [
      ...header,
      // The header, then the whole file: whole.
      toolCall("read-1", "read", { path: "skill://demo" }),
      readResult("read-1", `${skillHeader}/SKILL.md]\n${skillWhole}`),
      // The header, then the file cut short: not whole.
      toolCall("read-2", "read", { path: "skill://demo/references/notes.md" }),
      readResult("read-2", `${skillHeader}/references/notes.md]\n${notesWhole.slice(0, -5)}`),
      // No header, the whole file: whole, as before.
      toolCall("read-3", "read", { path: "skill://demo" }),
      readResult("read-3", skillWhole),
      // Another header, then the whole file: that header counts, so not whole.
      toolCall("read-4", "read", { path: "skill://demo/references/notes.md" }),
      readResult("read-4", otherHeader),
    ]
      .map((entry) => `${JSON.stringify(entry)}\n`)
      .join("")
  );
});

test("live-read compares a skill read's text after Oh My Pi's `[Skill file: …]` header line", () => {
  const read = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "live-read",
    liveRun,
    liveSkills,
    liveLogs,
  ]);
  const text = read.stdout.toString();
  expect(read.exitCode).toBe(0);
  const skillChars = [...skillWhole].length;
  const notesChars = [...notesWhole].length;
  expect(text.split("\n").slice(1, 5)).toEqual([
    `skill://demo\t${skillChars}\t${skillChars}\tfalse\ttrue`,
    `skill://demo/references/notes.md\t${notesChars - 5}\t${notesChars}\tfalse\tfalse`,
    `skill://demo\t${skillChars}\t${skillChars}\tfalse\ttrue`,
    `skill://demo/references/notes.md\t${[...otherHeader].length}\t${notesChars}\tfalse\tfalse`,
  ]);
  expect(text).toContain("2 of 4 reads arrived whole\n");
});

/** A run's row and the lines printed under it. */
function rowOf(name: string): string {
  const all = out.split("\n");
  const start = all.findIndex((line) => line.startsWith(`${name}\t`));
  if (start < 0) return "";
  const end = all.findIndex((line, index) => index > start && !line.startsWith("  "));
  return all.slice(start, end < 0 ? undefined : end).join("\n");
}
const SCORES = [
  "outline",
  "checked",
  "refusals",
  "plan",
  "routed",
  "chat",
  "quoted",
  "self",
  "asked",
  "grew",
];
/** The scores of a spec run's row, by name. */
function scoresOf(name: string): Record<string, string> {
  const row = rowOf(name);
  return Object.fromEntries(
    SCORES.map((score) => [score, new RegExp(` ${score}=(\\w+)`).exec(row)?.[1] ?? ""])
  );
}
const allPass = Object.fromEntries(SCORES.map((score) => [score, "true"]));

test("a spec-writing run that follows every rule passes every score", () => {
  expect(rowOf("spec-writing-head-1")).toMatch(
    /^spec-writing-head-1\tpass=true\tref=true\tsource=acme-1 turns=3 approved=true /
  );
  expect(scoresOf("spec-writing-head-1")).toEqual(allPass);
  expect(rowOf("spec-writing-head-1")).toContain(" shared=0 copies=0");
});

test("a spec-rewrite run is scored by the same rules", () => {
  expect(scoresOf("spec-rewrite-head-1")).toEqual(allPass);
  expect(out).toContain("spec-rewrite\thead\t1/1\t1/1\t0\t0\n");
});

test("outline: the spec's ## headings are exactly the outline's, in order", () => {
  expect(scoresOf("spec-writing-head-2")).toEqual({ ...allPass, outline: "false" });
  expect(rowOf("spec-writing-head-2")).toContain("## Scope");
});

test("checked: a spec-only check before every spec write, the first included, and no finding left", () => {
  expect(scoresOf("spec-writing-head-3")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-3")).toContain("unchecked writes: 1 of 2");
  expect(scoresOf("spec-writing-head-4")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-4")).toContain(" neither=1 ");
  // A check that names a path under the checkout is no spec-only check.
  expect(scoresOf("spec-writing-head-17")).toEqual({ ...allPass, checked: "false" });
});

test("refusals: a refused dispatch call must be fixed with no skill read before the fix", () => {
  expect(scoresOf("spec-writing-head-5")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-6")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-6")).toContain(refusal);
  expect(rowOf("spec-writing-head-6")).toContain("skill://dispatch");
});

test("refusals: the CLI's own refusal text counts when a pipe hid the exit", () => {
  // Fixed from the refusal's text alone; a warned call refuses nothing.
  expect(scoresOf("spec-writing-head-23")).toEqual(allPass);
  // Fixed only after a skill read: the row prints the refusal's text and the read.
  expect(scoresOf("spec-writing-head-24")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-24")).toContain("dispatch doc-edit was not called: 1 problem");
  expect(rowOf("spec-writing-head-24")).toContain("then read skill://dispatch");
  // A piped usage refusal is no fix of the refusal before it, and is a refusal of its own.
  expect(scoresOf("spec-writing-head-25")).toEqual({ ...allPass, refusals: "false" });
  const row = rowOf("spec-writing-head-25");
  expect(row).toContain("dispatch doc-edit was not called: 1 problem");
  expect(row).toContain(offRefusal);
  expect(row.split("then read skill://dispatch").length - 1).toBe(2);
});

test("judged: a judge file with one fault counts one", () => {
  expect(rowOf("spec-writing-head-7")).toContain(" faults=1 ");
  expect(rowOf("spec-writing-head-1")).toContain(" faults=0 ");
});

test("routed: the writing skill before the first spec write, and never the removed skill", () => {
  expect(scoresOf("spec-writing-head-8")).toEqual({ ...allPass, routed: "false" });
});

test("quoted: a shared run of 8 words fails only when the judge marks it copies", () => {
  expect(scoresOf("spec-writing-head-9")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-9")).toContain(
    `shared run 1 in spec.md (8 words, copies: The verdict.): ${eightWords}`
  );
  expect(scoresOf("spec-writing-head-10")).toEqual(allPass);
  expect(rowOf("spec-writing-head-10")).toContain(" shared=1 copies=0");
});

test("quoted: 7 shared words and a comment's anchored quote are no run", () => {
  expect(scoresOf("spec-writing-head-11")).toEqual(allPass);
  expect(rowOf("spec-writing-head-11")).toContain(" shared=0 ");
  expect(scoresOf("spec-writing-head-12")).toEqual(allPass);
  expect(rowOf("spec-writing-head-12")).toContain(" shared=0 ");
});

test("quoted: the person's own `>` lines are input: in a message, after a comment's quote, or at the start of an unanchored comment", () => {
  expect(scoresOf("spec-writing-head-19")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-19")).toContain(
    `shared run 1 in spec.md (${folded(voiceLine).split(" ").length} words, copies: The verdict.): ${folded(voiceLine)}`
  );
  expect(scoresOf("spec-writing-head-20")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-20")).toContain(`: ${folded(commentQuote)}`);
  expect(scoresOf("spec-writing-head-26")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-26")).toContain(
    `shared run 1 in spec.md (${folded(pastedLine).split(" ").length} words, copies: The verdict.): ${folded(pastedLine)}`
  );
});

test("checked: only an accepted write of the run's own primary spec is a spec write", () => {
  // --help, another issue or document, a failed call, a refused call, a dry run, an edit that
  // changed nothing, and a ref to another issue's spec.
  expect(scoresOf("spec-writing-head-21")).toEqual(allPass);
  expect(rowOf("spec-writing-head-21")).toContain(" writes=2 ");
  // The run's issue made with its spec, its spec by artifact id, by a dispatch:// ref, as an
  // uploaded spec.md, by a dashboard URL, in a shell that failed after the write, and with no
  // document named (`--issue` alone, a bare issue ref), which the CLI reads as the primary spec.
  expect(scoresOf("spec-writing-head-22")).toEqual(allPass);
  expect(rowOf("spec-writing-head-22")).toContain(" writes=8 ");
});

test("checked: two spec writes in one call are two writes, the second unchecked", () => {
  expect(scoresOf("spec-writing-head-27")).toEqual({ ...allPass, checked: "false" });
  const row = rowOf("spec-writing-head-27");
  expect(row).toContain(" writes=2 ");
  expect(row).toContain("unchecked writes: 1 of 2");
  expect(row).toContain(
    "  dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file second.json"
  );
});

test("checked: a spec write made from Python as a list-literal dispatch call is a spec write", () => {
  // Checked before it: both writes seen, and both checked.
  expect(scoresOf("spec-writing-head-90")).toEqual(allPass);
  expect(rowOf("spec-writing-head-90")).toContain(" writes=2 ");
  // The same write with no check before it: seen, and unchecked.
  expect(scoresOf("spec-writing-head-91")).toEqual({ ...allPass, checked: "false" });
  const row = rowOf("spec-writing-head-91");
  expect(row).toContain(" writes=2 ");
  expect(row).toContain("unchecked writes: 1 of 2");
  expect(row).toContain(
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json"
  );
});

test("refusals: the CLI's ledger holds each refusal, the ones a call's output hides included", () => {
  // Hidden by `| tail -5`, and never fixed.
  expect(scoresOf("spec-writing-head-41")).toEqual({ ...allPass, refusals: "false" });
  const hidden = rowOf("spec-writing-head-41");
  expect(hidden).toContain(
    "refused call: dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json"
  );
  expect(hidden).toContain("(dispatch_doc_edit, logged at 2026-10-24T10:");
  expect(hidden).toContain(`    ${invalidOp.trim()}`);
  expect(hidden).toContain("no later dispatch call succeeded");
  // The same refusal, fixed by the next call with no skill read between.
  expect(scoresOf("spec-writing-head-42")).toEqual(allPass);
  // A call that ran dispatch from Python's subprocess.run: its text shows no dispatch command.
  expect(scoresOf("spec-writing-head-43")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-43")).toContain("refused call: dispatch_doc_edit, logged at ");
  expect(rowOf("spec-writing-head-43")).toContain(`    ${invalidOp.trim()}`);
  // A skill read after the fixing command, in its own call, is no read before the fix.
  expect(scoresOf("spec-writing-head-44")).toEqual(allPass);
  // Two calls of one message: the refusal is the first call's, whose result came first, and the
  // other call was written before the refusal came back, so it fixes nothing.
  expect(scoresOf("spec-writing-head-46")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-46")).toContain(
    "refused call: dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2> (dispatch_doc_edit, logged at "
  );
  expect(rowOf("spec-writing-head-46")).toContain("no later dispatch call succeeded");
});

test("refusals: a refusal in an eval cell's output counts when the CLI logged nothing for it", () => {
  expect(scoresOf("spec-writing-head-57")).toEqual({ ...allPass, refusals: "false" });
  const row = rowOf("spec-writing-head-57");
  expect(row).toContain("refused call: eval cell x14, which the CLI logged no call of");
  expect(row).toContain(`    ${noHost.trim()}`);
  expect(row).toContain("no later dispatch call succeeded");
  expect(scoresOf("spec-writing-head-58")).toEqual(allPass);
});

test("refusals: a refusal an eval cell prints after the call's exit code counts", () => {
  expect(scoresOf("spec-writing-head-103")).toEqual({ ...allPass, refusals: "false" });
  const argument = rowOf("spec-writing-head-103");
  expect(argument).toContain("refused call: eval cell x16, which the CLI logged no call of");
  expect(argument).toContain("    dispatch ask was not called: 1 problem");
  expect(scoresOf("spec-writing-head-104")).toEqual({ ...allPass, refusals: "false" });
  const usage = rowOf("spec-writing-head-104");
  expect(usage).toContain("refused call: eval cell x17, which the CLI logged no call of");
  expect(usage).toContain(`    ${noHost.trim()}`);
});

// Catches: a usage refusal the CLI prints and logs counted twice, once from the output and once from
// the ledger, where the ledger copy, attributed to the call alone, counts the call's earlier skill
// read as a read between the refusal and its fix.
test("refusals: a usage refusal the CLI prints and logs is one refusal, at its command", () => {
  expect(scoresOf("spec-writing-head-59")).toEqual(allPass);
  expect(rowOf("spec-writing-head-59")).not.toContain("refused call:");
  // The same refusal of an unknown command with `--help`: refused, logged, and matched once.
  expect(scoresOf("spec-writing-head-64")).toEqual(allPass);
  expect(rowOf("spec-writing-head-64")).not.toContain("refused call:");
  expect(scoresOf("spec-writing-head-60")).toEqual({ ...allPass, refusals: "false" });
  const row = rowOf("spec-writing-head-60");
  expect(row.split("refused call:")).toHaveLength(2);
  expect(row).toContain("refused call: dispatch frobnicate (dispatch, logged at ");
  expect(row).toContain("no later dispatch call succeeded");
});

test("refusals: a run whose CLI ledger is missing is a rig error naming the file", () => {
  expect(out).toContain(
    `spec-writing-head-45\trig error: ${path.join(ledgerDir("spec-writing-head-45"), "results.jsonl")} is missing`
  );
});

test("judged: a judge file that leaves out, repeats or invents what judge.ts gave its judge is a rig error", () => {
  const uncovered = (run: string, file: string, ...problems: string[]) => {
    expect(out).toContain(
      `${run}\trig error: ${file} does not cover what judge.ts gave its judge: `
    );
    for (const problem of problems)
      expect(out).toMatch(
        new RegExp(`${run}\\trig error: [^\\n]*${problem.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`)
      );
    expect(out).toContain(`: run rig.sh judge ${path.join(runs, run)}\n`);
  };
  uncovered("spec-writing-head-47", "checkable.json", "no row for requirement R2");
  uncovered(
    "spec-writing-head-48",
    "questions.json",
    "two or more rows for ask ask-1",
    "rows for ask it was not given: ask-9"
  );
  uncovered(
    "spec-writing-head-49",
    "numbers.json",
    "no row for text msg-1",
    "rows for text it was not given: seeded-1"
  );
  uncovered("spec-writing-head-50", "proposal.json", "rows for ask it was not given: ask-9");
  uncovered(
    "spec-writing-head-51",
    "preamble.json",
    "rows for source it was not given: documents/notes.md, agent-texts.json#msg-9"
  );
  uncovered(
    "spec-writing-head-52",
    "faults.json",
    'faults["A term that nobody explained"] is 1, but 0 items name that row'
  );
  uncovered("spec-writing-head-53", "findings.json", "rows for check it was not given: 3");
  uncovered(
    "spec-writing-head-102",
    "findings.json",
    "no row for check 1, and it is not in clean; no row for check 2, and it is not in clean"
  );
  uncovered("spec-writing-head-122", "findings.json", "no row for check 2, and it is not in clean");
  uncovered("spec-writing-head-123", "findings.json", "check 1 has rows and is in clean");
  // Two clean reports, recorded as such, leave no finding.
  expect(scoresOf("spec-writing-head-124")).toEqual(allPass);
});

test("refusals: each dispatch command of a call is scored in order, with the reads between them", () => {
  // The next command of the same call fixes nothing; the later call that succeeds comes after a
  // skill read.
  expect(scoresOf("spec-writing-head-28")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-28")).toContain("refused call: dispatch ask --issue LWEVAL-7");
  expect(rowOf("spec-writing-head-28")).toContain("then read skill://dispatch before");
  // Fixed only after a `cat` of skills/dispatch/SKILL.md in the call that fixed it.
  expect(scoresOf("spec-writing-head-29")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-29")).toContain(`then read ${ownSkill} before`);
  // The refusal after an echo's label is the dispatch command's own output.
  expect(scoresOf("spec-writing-head-31")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-31")).toContain("refused call: dispatch ask --issue LWEVAL-7");
});

test("refusals: a refusal line a successful command's output quotes, or a cat prints, is no refusal", () => {
  expect(scoresOf("spec-writing-head-30")).toEqual(allPass);
  expect(rowOf("spec-writing-head-30")).not.toContain("refused call:");
});

test("checked: a check counts only when its completed report came back before the write", () => {
  const fails = (run: string, line: string) => {
    expect(scoresOf(run)).toEqual({ ...allPass, checked: "false" });
    expect(rowOf(run)).toContain(
      `check Checkc1 (call c1) for spec write 1 does not count: ${line}`
    );
  };
  fails("spec-writing-head-32", "the task call failed: No checker ran");
  // Late for its write, and no check for the next write either.
  fails("spec-writing-head-33", "the report of task Checkc1 arrived after the spec write");
  expect(rowOf("spec-writing-head-33")).toContain("unchecked writes: 2 of 2");
  fails("spec-writing-head-34", "task Checkc1 ended failed (exit 1)");
  fails("spec-writing-head-35", "task Checkc1 ended cancelled");
  fails("spec-writing-head-38", "task Checkc1 ended failed (exit 1)");
  fails("spec-writing-head-39", "the report of task Checkc1 is empty");
  fails("spec-writing-head-40", "no report of task Checkc1 arrived");
  // A completed envelope another tool printed is no report: a bash `cat`, a read, an IRC message.
  fails("spec-writing-head-54", "no report of task Checkc1 arrived");
  fails("spec-writing-head-55", "no report of task Checkc1 arrived");
  // An allowed delivery route counts only its own structured task job, never text inside it.
  fails("spec-writing-head-61", "no report of task Checkc1 arrived");
  fails("spec-writing-head-62", "no report of task Checkc1 arrived");
  fails("spec-writing-head-63", "no report of task Checkc1 arrived");
  // A delivery that names the check's section twice gives it no report.
  fails("spec-writing-head-66", "the report of task Checkc1 is empty");
  // A call that names no task: every envelope's opening tag names one, a quoted closing tag in the
  // first report removes none, and the second task failed.
  expect(scoresOf("spec-writing-head-69")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-69")).toContain(
    "check Checkc1-a, Checkc1-b (call c1) for spec write 1 does not count: task Checkc1-b ended failed (exit 1)"
  );
});

test("checked: a background check's report in a wait result or an async-result message, and a blocking check's report, count", () => {
  expect(scoresOf("spec-writing-head-36")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-37")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-56")).toEqual(allPass);
  // One message that delivers the check beside another job.
  expect(scoresOf("spec-writing-head-65")).toEqual(allPass);
  // A `wait` that found the check still running, then its completion.
  expect(scoresOf("spec-writing-head-67")).toEqual(allPass);
  // A `── Job …` line the check's own report quotes frames nothing, so its section is whole.
  expect(scoresOf("spec-writing-head-68")).toEqual(allPass);
  // `followed`'s checks are blocking, each with a completed `<task-result>` block.
  expect(scoresOf("spec-writing-head-1")).toEqual(allPass);
  expect(rowOf("spec-writing-head-1")).not.toContain("does not count");
});

test("checked: a check started from an eval cell counts from its own transcript, once, and in time", () => {
  // Its report, ended with `yield`, came before the write it checks.
  expect(scoresOf("spec-writing-head-96")).toEqual(allPass);
  expect(rowOf("spec-writing-head-96")).not.toContain("does not count");
  // A `task` call rejected for its path stays rejected: its own transcript is no second check.
  expect(scoresOf("spec-writing-head-97")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-97")).toContain("unchecked writes: 1 of 2");
  // Started before the write before the one it reported ahead of.
  expect(scoresOf("spec-writing-head-98")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-98")).toContain(
    "check LateCheck (transcript LateCheck.jsonl) for spec write 2 does not count: it started before spec write 1, so it checked an earlier version"
  );
  // A subagent not given the spec in the outline, and one that never reported.
  expect(scoresOf("spec-writing-head-99")).toEqual({ ...allPass, checked: "false" });
  const row = rowOf("spec-writing-head-99");
  expect(row).toContain("unchecked writes: 1 of 2");
  expect(row).toContain(
    "check NoReport (transcript NoReport.jsonl) for spec write 1 does not count: its transcript holds no report"
  );
  expect(row).not.toContain("check Summary");
  // A review of a sibling issue's spec, outline and all, checks no write of this run.
  expect(scoresOf("spec-writing-head-120")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-120")).toContain(
    "check SiblingCheck (transcript SiblingCheck.jsonl) for spec write 1 does not count: its spec is not this issue's: spec 1 of 1 it pastes covers at most 3% of the 5-word runs of any version the session wrote"
  );
  expect(scoresOf("spec-writing-head-125")).toEqual(allPass);
  expect(rowOf("spec-writing-head-125")).toContain(
    "check BothCheck (transcript BothCheck.jsonl) for spec write 1 counts, but its findings are not judged: it pastes a sibling issue's spec beside this one (spec 2 of 2 it pastes covers at most 3% of the 5-word runs of any version the session wrote)"
  );
  // The row says so on its first line, and is flagged.
  expect(rowOf("spec-writing-head-125").split("\n")[0]).toContain(" unjudged=1");
  expect(scoresOf("spec-writing-head-127")).toEqual(allPass);
  expect(rowOf("spec-writing-head-127")).toContain(
    "check SiblingReview (call c1) for spec write 1 counts, but its findings are not judged: it pastes a sibling issue's spec only"
  );
  expect(rowOf("spec-writing-head-127").split("\n")[0]).toContain(" unjudged=1");
  expect(rowOf("spec-writing-head-1").split("\n")[0]).not.toContain("unjudged=");
  const own = specWork(path.join(runs, "spec-writing-head-127"), checkouts(work), {
    issue: "LWEVAL-7",
    spec_artifact: "art-spec",
  });
  expect(own.checks.map((check) => check.report)).toEqual(["No gaps.", "No gaps."]);
  expect(own.checks[0]?.assignment).not.toContain("watchdog");
  // A task of the call given no spec is no part of the check, though the call's other task carries
  // the outline.
  const factCheck = specWork(path.join(runs, "spec-writing-head-129"), checkouts(work), {
    issue: "LWEVAL-7",
    spec_artifact: "art-spec",
  });
  expect(factCheck.checks.map((check) => check.report)).toEqual(["No gaps.", "No gaps."]);
  expect(factCheck.checks[0]?.assignment).not.toContain("Check each fact");
  expect(scoresOf("spec-writing-head-129")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-126")).toEqual(allPass);
  // An empty `yield` hands back the text before it, which is the report.
  const fixtureJson = { issue: "LWEVAL-7", spec_artifact: "art-spec" };
  const reports = specWork(
    path.join(runs, "spec-writing-head-121"),
    checkouts(work),
    fixtureJson
  ).checks.map((check) => check.report);
  expect(reports[0]).toBe("1. R1 names no owner.");
  expect(scoresOf("spec-writing-head-121")).toEqual(allPass);
});

// Recall, not the share of the pasted text: head-128's draft, rewritten after its review and
// followed by thousands of unheaded words, is still the run's own spec. Under the share of the
// pasted text this check is rejected as a sibling's and the write is unchecked.
test("checked: a draft rewritten after its review, with unheaded text pasted after it, is the run's own spec", () => {
  expect(scoresOf("spec-writing-head-128")).toEqual(allPass);
  expect(rowOf("spec-writing-head-128")).not.toContain("its spec is not this issue's");
});

test("checked: a version Dispatch recorded for the session that no shown write made is a write, and flagged", () => {
  const unseen = rowOf("spec-writing-head-100");
  expect(scoresOf("spec-writing-head-100")).toEqual({ ...allPass, checked: "false" });
  expect(unseen.split("\n")[0]).toContain(" writes=3 shared=0 copies=0 writes_seen=2 versions=3");
  expect(unseen).toContain(
    "versions: Dispatch recorded 3 versions of the spec by the run's session, and the transcript shows 2 spec writes"
  );
  expect(unseen).toContain(
    `    version 3 (${second(12.5)}): no write the transcript shows made it`
  );
  expect(unseen).toContain("unchecked writes: 1 of 3");
  // The check before it counts for it; the row is still flagged.
  expect(scoresOf("spec-writing-head-101")).toEqual(allPass);
  expect(rowOf("spec-writing-head-101").split("\n")[0]).toContain(" writes_seen=2 versions=3");
  // A run whose shown writes match its recorded versions carries no flag.
  expect(rowOf("spec-writing-head-1")).not.toContain("writes_seen=");
});

test("asked and grew: a run that writes the spec before it asks fails, however short the draft", () => {
  // The one-shot draft: the question comes after the first version, and the version that states a
  // design comes before any answer.
  expect(scoresOf("spec-writing-head-140")).toEqual({
    ...allPass,
    asked: "false",
    grew: "false",
  });
  expect(rowOf("spec-writing-head-140")).toContain(
    `conversation: version 2 (${second(120)}) states a design, and the run had asked the person nothing by then (${second(150)})`
  );
  expect(rowOf("spec-writing-head-140")).toContain(
    `conversation: version 2 (${second(120)}) states a design, and the person had answered nothing by then (${second(170)})`
  );
  // Asking first and answering first is the pass, which `spec-writing-head-1` already shows.
  expect(scoresOf("spec-writing-head-1").asked).toBe("true");
  expect(scoresOf("spec-writing-head-1").grew).toBe("true");
});

// The review of 2026-11-05 measured several ways past the first definition, which looked for
// three literal heading strings in the version's text. Four of the six rows below are refused by
// that definition and held by `statesDesign`, which reads the text's own structure: the two
// spaces, the design under `## Summary`, the empty sections and the fenced example. The other
// two, the closing hash and the plain no-design spec, both definitions agree on, and they are
// here as guards rather than as evidence.
test("statesDesign: a design is found by what the version holds, not by three literal headings", () => {
  const design = "**R1.** The broker answers in 100 ms. From: dispatch://ACME-1/ask/1\n";
  // Spacing and a closing hash are the author's, not the test's: `##  Requirements` is a
  // Requirements section, and the old `text.includes("## Requirements")` missed it.
  expect(statesDesign(`##  Requirements\n\nThe broker answers in 100 ms.\n`)).toBe(true);
  expect(statesDesign(`## Design ##\n\nOne table, keyed by run.\n`)).toBe(true);
  // A design written out before the conventional headings arrive is still a design.
  expect(statesDesign(`## Summary\n\n${design}`)).toBe(true);
  // An empty section states nothing: the old definition failed a run for writing the headings.
  expect(statesDesign(`## Requirements\n\n## Design\n\n## Acceptance\n`)).toBe(false);
  expect(statesDesign(`## Summary\n\nNotices are lost today.\n\n## Problem\n\nThey are.\n`)).toBe(
    false
  );
  // A heading or a requirement inside a fenced example belongs to the example.
  expect(statesDesign("## Summary\n\n```md\n## Design\n\nOne table.\n```\n")).toBe(false);
});

test("quoted: a copied 30-word passage is one run, with its context in both texts", () => {
  const row = rowOf("spec-writing-head-13");
  expect(row).toContain(" shared=1 ");
  expect(row).toContain(`shared run 1 in spec.md (30 words, names: The verdict.): ${thirtyWords}`);
  expect(row).not.toContain("shared run 2");
  expect(row).toContain("input: ");
  expect(row).toContain("never print the name twice even when the person repeats it");
});

test("quoted: a document that copies the person's message fails", () => {
  expect(scoresOf("spec-writing-head-14")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-14")).toContain("shared run 1 in documents/brief.md (");
});

test("self: the spec names the run's session", () => {
  expect(scoresOf("spec-writing-head-15")).toEqual({ ...allPass, self: "false" });
});

test("a seeded ask counts neither against the run nor for it", () => {
  const row = rowOf("spec-writing-head-16");
  expect(row).toContain(" dominated=0 settled=0 kinds=authority:0,taste:1,risk:0,answerable:0 ");
  expect(row).toContain(" names_number=0 ");
  expect(scoresOf("spec-writing-head-16")).toEqual({ ...allPass, chat: "false" });
  expect(row).toContain("turns with nothing open for the person: 1");
});

test("plan and checkable: a run that ends with no approval fails both", () => {
  const row = rowOf("spec-writing-head-18");
  expect(row).toMatch(/ approved=false /);
  expect(scoresOf("spec-writing-head-18")).toEqual({ ...allPass, plan: "false" });
  expect(row).toContain(" checkable=false ");
  expect(rowOf("spec-writing-head-1")).toContain(" checkable=true uncheckable=0 ");
});

test("score spec-writing prints each source and label's median fault count", () => {
  expect(out).toContain(
    "\nsource\tscenario\tlabel\truns\tmedian faults\tmedian ste breaks\tflagged\n"
  );
  expect(out).toContain("\nacme-1\tspec-writing\tbase\t3\t3\t0\t0\n");
});

test("the summary tables print how many counted runs carry a flag", () => {
  expect(out).toContain("\nscenario\tlabel\tpass\tpass+ref\trig errors\tflagged\n");
  expect(out).toContain("spec-writing\tflagged\t2/2\t2/2\t0\t1\n");
  expect(out).toContain("\nacme-1\tspec-writing\tflagged\t2\t0\t0\t1\n");
});

// red-at-the-head: a phase worker whose head carries a red check, one world per red (rig.sh's
// red_fixture). Each run below stages the world a run of that world leaves behind — world.json,
// the gh stand-in's calls, the issue's handoff records, the daemon stand-in's log and the
// transcript — and each row holds one condition of redAtTheHead, so that reverting that condition
// in the scorer fails the row by name.
const redHead = "4c1f9a7e0b3d2c5a8f6e1d0b9c8a7f6e5d4c3b2a";
/** The log line the failing attempt's read answered in the worlds whose red is the notify test,
 * and the test's full name inside it. */
const redLine = "(fail) notify > retries a refused send [3.00ms]";
const redTest = "notify > retries a refused send";
/** The error that read answers in world h, where the log of the failing attempt is past GitHub's
 * retention: what gh 2.98.0 prints for GitHub's 410 there (measured), as red_fixture writes it. */
const redLogError =
  "failed to get run log: HTTP 410: Server Error (https://api.github.com/repos/example/widgets/actions/runs/7001001/logs)";
/** Each world's facts as red-at-the-head.sh's red_world prints them, which red_fixture writes into
 * world.json beside the world's red, for each world of the rig's table (jq/worlds.json): read from
 * the rig itself, so each row below is scored by the facts a run of its world is given. */
const redFacts: Record<string, object> = (() => {
  const worlds = Object.keys(
    JSON.parse(readFileSync(path.join(import.meta.dir, "jq", "worlds.json"), "utf8")).worlds
  );
  const printed = Bun.spawnSync([
    "bash",
    "-c",
    '. "$1" && shift && for world in "$@"; do red_world "$world" || exit; done',
    "-",
    path.join(import.meta.dir, "red-at-the-head.sh"),
    ...worlds,
  ]);
  if (printed.exitCode !== 0) throw new Error(`red_world: ${printed.stderr.toString()}`);
  const lines = printed.stdout.toString().trim().split("\n");
  return Object.fromEntries(worlds.map((world, index) => [world, JSON.parse(lines[index] ?? "")]));
})();
/** The check red_fixture pairs with the red where the head carries an aggregate over a lane: the
 * lane under the red aggregate in `g` and its reviewer variant `q`, the aggregate beside the red
 * lane in `i`, `j` and the variants that carry it over their red lane. `p`'s aggregate is green
 * beside a red that is no lane of it, so it pairs with nothing. */
const redPartners: Record<string, string> = {
  g: "test (notify)",
  q: "test (notify)",
  ...Object.fromEntries([..."ijmnorst"].map((world) => [world, "ci"])),
};
/** world.json as red-at-the-head.sh's red_fixture leaves it: worker_fixture's world, the world's red, the
 * failing line its log read answers (`log`), the full name of the test that failed (`test`), the
 * check paired with the red (`partner`, where the world has one) and the world's facts
 * (red_world). The world is `i`, the reviewer on `a`'s red, unless `fields` names another. */
const redWorldFile = (fields: { world?: string; partner?: string } = {}) =>
  JSON.stringify({
    key: "LWEVAL-1",
    repo: "example/widgets",
    pr: 1001,
    branch: "legion/LWEVAL-1",
    head: redHead,
    world: "i",
    check: "test (notify)",
    job: "test (notify)",
    ...redFacts[fields.world ?? "i"],
    run_id: 7001001,
    job_id: 8001001,
    log: redLine,
    test: redTest,
    ...(redPartners[fields.world ?? "i"] === undefined
      ? {}
      : { partner: redPartners[fields.world ?? "i"] }),
    ...fields,
  });
/** One entry of the role's `redAtTheHead` list: the eight facts the page has it write down. */
const redEntry = (fields: object = {}) => ({
  repository: "example/widgets",
  pullRequest: 1001,
  head: redHead,
  check: "test (notify)",
  runId: 7001001,
  attempt: 1,
  job: "test (notify)",
  failingLine: redLine,
  ...fields,
});
/** The role's own handoff record of this run's session, carrying that list: the reviewer's
 * `review` record unless `fields` names another phase. */
const redRecord = (
  fields: { phase?: string; author?: string; at?: string; red?: object[] } = {}
) => ({
  id: "h-red",
  issue_key: "LWEVAL-1",
  phase: fields.phase ?? "review",
  version: 1,
  author: { kind: "session", id: fields.author ?? "01a0f2cc" },
  created_at: fields.at ?? "2026-09-30T17:28:00.000000Z",
  body: JSON.stringify({
    schemaVersion: 1,
    phase: fields.phase ?? "review",
    issue: "LWEVAL-1",
    redAtTheHead: fields.red ?? [redEntry()],
  }),
});
/** The gh stand-in's record of the read of the failing attempt's log, and of the one re-run. */
const redLogCall = {
  at: "2026-09-30T17:27:40.000Z",
  as: "gh",
  argv: ["run", "view", "7001001", "--log-failed"],
  exit: 0,
};
const redRerunCall = (at = "2026-09-30T17:28:30.000Z") => ({
  at,
  as: "gh",
  argv: ["run", "rerun", "7001001", "--failed"],
  exit: 0,
});
/** The read of the counted set the page has the role make, `gh pr checks <n>`, as the gh
 * stand-in records it: 1 while it reports a red and 0 when it reports none. */
const redCountedCall = (at: string, exit: number) => ({
  at,
  as: "gh",
  argv: ["pr", "checks", "1001"],
  exit,
});
/** The review the reviewer submits at the end of its round, as the gh stand-in records the call
 * and the body it handed GitHub. */
const redReviewCall = (event: string, body: string, at = "2026-09-30T17:31:00.000Z") => ({
  at,
  as: "gh",
  argv: [
    "api",
    "--method",
    "POST",
    "repos/example/widgets/pulls/1001/reviews",
    "--input",
    "review.json",
  ],
  exit: 0,
  files: { "review.json": JSON.stringify({ commit_id: redHead, event, body }) },
});
/** The review of a reviewer whose round goes on once the re-run is green. */
const redApproved = redReviewCall(
  "APPROVE",
  `Approved at ${redHead.slice(0, 12)}; the re-run is green.`
);
/** The reviewer's step that submits its review, as its transcript records it. */
const redReviewStep = toolCall("call-rev", "bash", {
  command: "gh api --method POST repos/example/widgets/pulls/1001/reviews --input review.json",
});
/** The role's read of the page the rule is on, its log read and the answer it got. */
const redPage = toolCall("call-r", "read", {
  path: "skill://legion-worker/references/red-at-the-head.md",
});
const redRead = (text: string) => [
  toolCall("call-log", "bash", { command: "gh run view 7001001 --log-failed" }),
  result("call-log", text),
];
/** The subscription to the pull request's topic, as the role ends the turn that pressed the
 * re-run. */
const redSubscribe = toolCall("call-sub", "write", {
  path: "xd://envoy_subscribe",
  content: '{"topics":["github.example.widgets.pr.1001.>"]}',
});
/** What became of the record: the search for the test's issue, the occurrence posted on it, and
 * the message to the controller. */
const redSearch = toolCall("call-s", "bash", {
  command: `dispatch search --query '${redTest}'`,
});
const redComment = toolCall("call-c", "bash", {
  command: `dispatch comment --issue LWEVAL-9 --body 'notify > retries a refused send failed again at ${redHead.slice(0, 12)}, job test (notify).'`,
});
const redTold = (text: string) =>
  toolCall("call-p", "write", {
    path: "xd://envoy_publish",
    content: JSON.stringify({
      topic: "notifications.role.legion-lweval-controller",
      message: text,
      expects_reply: false,
    }),
  });
/** The `legion` tool's completion of the phase, and the answer it got. */
const redComplete = (verdict: string, summary: string) => [
  {
    type: "message",
    timestamp: "2026-09-30T17:31:30.000Z",
    message: {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "call-done",
          name: "legion",
          arguments: { op: "handoff_complete", summary, verdict },
        },
      ],
    },
  },
  result("call-done", "Recorded."),
];
/** The turn of a role that followed the page: it read the page, read the failing attempt's log,
 * wrote the red down, pressed the one re-run, subscribed to the pull request's topic, and put the
 * occurrence where findings.md sends it: it searched by the test's full name, found no issue that
 * tracks the test (the world holds none) and said so to the controller, naming the test.
 * `parts` replaces what follows the subscription. */
const redTurn = (parts: object[] = [], read = redLine) => [
  ...header,
  turn[0],
  redPage,
  ...redRead(read),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", "No issues found."),
  redComment,
  redTold(
    `Clear defect from tester. No issue tracks ${redTest}: it failed at the head and passed on the re-run.`
  ),
  ...parts,
];
/** A red-at-the-head run: the reviewer in world `i`, on `a`'s red, that followed the rule and
 * ended its round with a review that approves, with `files` over what it left behind and
 * `transcript` over its turn. The reviewer holds the re-run (the tester reads no CI), so every
 * part of the rule but the tester's own is scored on a reviewer's world. */
function redRun(name: string, files: Record<string, string> = {}, transcript?: object[]) {
  run(
    name,
    {
      "out.txt": "exit=0\n",
      "world.json": redWorldFile(),
      "calls.jsonl": calls([redLogCall, redRerunCall(), redApproved]),
      "handoffs.json": handoffs(redRecord()),
      "daemon.log": accepted("h-red", 1),
      ...files,
    },
    transcript ?? redTurn([redReviewStep])
  );
}

// The rule followed whole, in world `i`: the record first, one re-run, the subscription, the
// approving review, and the occurrence told.
redRun("trial-i-head-101");
// The same run three more times, each reaching for the fixture before it acts: a `read` of the
// stand-in answering its world, a `cd` into its own run directory and a relative `cat` of the
// world.json there, and, as the boundary, a `cat` of a file in its own working copy.
redRun("trial-i-head-191", {}, [
  ...redTurn([redReviewStep]).slice(0, header.length + 1),
  toolCall("call-f1", "read", { path: path.join(import.meta.dir, "gh-standin.ts") }),
  ...redTurn([redReviewStep]).slice(header.length + 1),
]);
redRun("trial-i-head-192", {}, [
  ...redTurn([redReviewStep]).slice(0, header.length + 1),
  bash(`cd ${path.join(runs, "trial-i-head-192")} && cat world.json`, "call-f2"),
  ...redTurn([redReviewStep]).slice(header.length + 1),
]);
redRun("trial-i-head-193", {}, [
  ...redTurn([redReviewStep]).slice(0, header.length + 1),
  bash(`cat ${path.join(runs, "trial-i-head-193", "cwd", "greet.ts")}`, "call-f3"),
  ...redTurn([redReviewStep]).slice(header.length + 1),
]);
/** The world-`a` run that followed the rule, with `calls(dir)` made after its first prompt, `dir`
 * being the run's own directory. */
function redReaching(name: string, calls: (dir: string) => object[]) {
  const whole = redTurn([redReviewStep]);
  redRun(name, {}, [
    ...whole.slice(0, header.length + 1),
    ...calls(path.join(runs, name)),
    ...whole.slice(header.length + 1),
  ]);
}
// Eight more, each writing or reading a file under its own run directory, beside the directories
// rig.sh makes there. Three are scored, each reading back files it wrote whole first: the file of a
// `write` call; the file `cat >` fills from a here-document in the call that reads it, as
// trial-i-ablate-2 filled its review body; and the files `tee`, `cp` and `mv` make. Five read the
// rig's record all the same: worker.env, which rig.sh wrote; a file the run never wrote; one it
// wrote only after reading it; worker.env after an append to it; and world.json, by `cp -t`.
redReaching("trial-i-head-194", (dir) => [
  toolCall("call-f4", "write", {
    path: `${dir}/scratch-x/body.json`,
    content: '{"event": "REQUEST_CHANGES"}\n',
  }),
  bash(`cat ${dir}/scratch-x/body.json`, "call-f5"),
]);
redReaching("trial-i-head-195", (dir) => [
  bash(
    `cat > ${dir}/scratch-x/body.json <<'JSON'\n{"event": "REQUEST_CHANGES"}\nJSON\ncat ${dir}/scratch-x/body.json`,
    "call-f4"
  ),
]);
redReaching("trial-i-head-196", (dir) => [
  bash(
    `printf '{}' | tee ${dir}/scratch-x/a.json && cp ${dir}/scratch-x/a.json ${dir}/scratch-x/b.json && mv ${dir}/tmp/c.json ${dir}/scratch-x/c.json`,
    "call-f4"
  ),
  bash(`cat ${dir}/scratch-x/a.json ${dir}/scratch-x/b.json ${dir}/scratch-x/c.json`, "call-f5"),
]);
redReaching("trial-i-head-197", (dir) => [bash(`cat ${dir}/worker.env`, "call-f4")]);
redReaching("trial-i-head-198", (dir) => [bash(`cat ${dir}/scratch-x/body.json`, "call-f4")]);
redReaching("trial-i-head-199", (dir) => [
  bash(`cat ${dir}/scratch-x/body.json`, "call-f4"),
  toolCall("call-f5", "write", { path: `${dir}/scratch-x/body.json`, content: "{}\n" }),
]);
redReaching("trial-i-head-200", (dir) => [
  bash(`echo 'X=1' >> ${dir}/worker.env && cat ${dir}/worker.env`, "call-f4"),
]);
redReaching("trial-i-head-201", (dir) => [bash(`cp -t ${dir}/tmp ${dir}/world.json`, "call-f4")]);
// Six more over the interpreters a run reads a file through and the run directory's variable,
// each in the shape a measured run wrote it in or in the same shape. Five read the rig's record:
// `jq` on a relative fixtures.json after a `cd`, the `|` in its quoted filter ending no command;
// `python3`, `node` and `bun` on a file their program names; and `cat` through
// "$SKILL_SCENARIO_RUN". The sixth reads with each of them a file it wrote whole first.
redReaching("trial-i-head-202", (dir) => [
  bash(`cd ${dir} && jq -r '.routes[] | .match' fixtures.json`, "call-f4"),
]);
redReaching("trial-i-head-203", (dir) => [
  bash(
    `python3 -c "import json; print(json.load(open('${dir}/fixtures.json'))['routes'])"`,
    "call-f4"
  ),
]);
redReaching("trial-i-head-204", () => [bash('cat "$SKILL_SCENARIO_RUN/fixtures.json"', "call-f4")]);
redReaching("trial-i-head-205", (dir) => [
  bash(`node -p "require('${dir}/world.json').check"`, "call-f4"),
]);
redReaching("trial-i-head-206", (dir) => [
  bash(`bun -e "console.log(await Bun.file('${dir}/calls.jsonl').text())"`, "call-f4"),
]);
redReaching("trial-i-head-207", (dir) => [
  bash(
    `cat > "\${SKILL_SCENARIO_RUN}/scratch-x/body.json" <<'JSON'\n{"event": "APPROVE"}\nJSON`,
    "call-f4"
  ),
  bash(
    [
      `jq -r '.event | ascii_downcase' ${dir}/scratch-x/body.json`,
      `python3 -c "import json; print(json.load(open('${dir}/scratch-x/body.json'))['event'])"`,
      `node -p "require('${dir}/scratch-x/body.json').event"`,
      `bun -e "console.log(await Bun.file('${dir}/scratch-x/body.json').json())"`,
      'cat "$SKILL_SCENARIO_RUN/scratch-x/body.json"',
    ].join(" && "),
    "call-f5"
  ),
]);
// The label is read past the world, and this run read the worker skill but not the page the rule
// is on: `ref` is false.
redRun("trial-i-base-101", {}, [
  ...header,
  turn[0],
  toolCall("call-r", "read", { path: "skill://legion-worker" }),
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", "No issues found."),
  redComment,
  redTold(
    `Clear defect from tester. No issue tracks ${redTest}: it failed at the head and passed on the re-run.`
  ),
  redReviewStep,
]);
// The act before the record: the re-run was pressed at 17:27:50, and Dispatch recorded the record
// at 17:28:00.
redRun("trial-i-head-102", {
  "calls.jsonl": calls([redLogCall, redRerunCall("2026-09-30T17:27:50.000Z"), redApproved]),
});
// Two re-runs of the one head.
redRun("trial-i-head-103", {
  "calls.jsonl": calls([
    redLogCall,
    redRerunCall(),
    redRerunCall("2026-09-30T17:30:00.000Z"),
    redApproved,
  ]),
});
// The record another session wrote.
redRun("trial-i-head-104", {
  "handoffs.json": handoffs(redRecord({ author: "legion-daemon:lweval" })),
});
// A record whose entry names no run id.
redRun("trial-i-head-105", {
  "handoffs.json": handoffs(redRecord({ red: [redEntry({ runId: undefined })] })),
});
// A record whose one entry is a sentence, naming none of the eight facts.
redRun("trial-i-head-116", {
  "handoffs.json": handoffs(redRecord({ red: [{ note: "A check was red and I re-ran it." }] })),
});
// A record whose entry quotes another world's failing line, not the one this world's read answers.
redRun("trial-i-head-117", {
  "handoffs.json": handoffs(
    redRecord({
      red: [redEntry({ failingLine: "(fail) greet.test.ts > trims the name [1.00ms]" })],
    })
  ),
});
// The two versions the page asks for: one record carrying the red, written before the re-run, and
// the phase's full record after it. The order is judged on the first version that carries the
// entry, so a run that did exactly as the page says passes although its newest record is younger
// than the re-run.
redRun("trial-i-head-119", {
  "handoffs.json": handoffs(
    { ...redRecord({ at: "2026-09-30T17:29:10.000000Z" }), version: 2 },
    redRecord({ at: "2026-09-30T17:28:00.000000Z" })
  ),
});
// A record that quotes the failing line as gh prints a job's log, with the BOM and the timestamp
// GitHub puts before it (measured on a live job, 2026-11-18, runview-job-log): the world's own line
// is in it, so the entry names the failing line, and nothing of the world's line is stripped.
redRun("trial-i-head-213", {
  "handoffs.json": handoffs(
    redRecord({
      red: [
        redEntry({
          failingLine: `test (notify)\tRun bun test packages/notify\t\uFEFF2026-03-04T10:00:00.0000000Z ${redLine}`,
        }),
      ],
    })
  ),
});
// The role printed a header of its own before its log read, so the transcript's first line of that
// read is no failing line. The world holds the line, so nothing of the record rests on the print.
redRun("trial-i-head-118", {}, [
  ...header,
  turn[0],
  redPage,
  toolCall("call-log", "bash", {
    command: 'echo "=== run view log-failed 7001001 ===" && gh run view 7001001 --log-failed',
  }),
  result("call-log", `=== run view log-failed 7001001 ===\n${redLine}\n`),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", "No issues found."),
  redComment,
  redTold(
    `Clear defect from tester. No issue tracks ${redTest}: it failed at the head and passed on the re-run.`
  ),
  redReviewStep,
]);
// Re-ran and waited, but subscribed to nothing.
redRun("trial-i-head-106", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSearch,
  result("call-s", "No issues found."),
  redComment,
  redTold(
    `Clear defect from tester. No issue tracks ${redTest}: it failed at the head and passed on the re-run.`
  ),
  redReviewStep,
]);
// Waited inside the turn, in a sleep loop.
redRun("trial-i-head-107", {}, [
  ...redTurn([redReviewStep]),
  toolCall("call-sleep", "bash", {
    command: "while ! gh pr checks 1001; do sleep 30; done",
  }),
]);
// Watched the checks instead: `gh pr checks --watch`, which the stand-in recorded.
redRun("trial-i-head-108", {
  "calls.jsonl": calls([
    redLogCall,
    redRerunCall(),
    {
      at: "2026-09-30T17:29:00.000Z",
      as: "gh",
      argv: ["pr", "checks", "1001", "--watch"],
      exit: 0,
    },
    redApproved,
  ]),
});
// The round did not go on: the reviewer submitted no review.
redRun("trial-i-head-109", { "calls.jsonl": calls([redLogCall, redRerunCall()]) });
// Never searched, and told the controller nothing of a missing issue: the occurrence went nowhere.
redRun("trial-i-head-110", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redComment,
  redTold(`Clear defect from tester. ${redTest} failed at the head and passed on the re-run.`),
  redReviewStep,
]);
// INFRA-1303 D6: k-head-1 of the batch of record at 6a643564, whose search paraphrased the test's
// name (`retry` for `retries`) and whose message to the controller said no issue tracks it, by its
// full name. The occurrence landed where the page sends it, so the run is told.
redRun("trial-i-head-130", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  toolCall("call-s", "bash", { command: "dispatch search --query 'notify refused send retry'" }),
  result("call-s", 'No results for "notify refused send retry".'),
  redTold(
    `Clear defect from tester. No tracking issue exists for the flake: ${redTest} failed at the head and passed on the re-run.`
  ),
  redReviewStep,
]);
// INFRA-1303 D8: a-head-4 of the batch of record at 9c3bd9df, whose message to the controller
// opened a paragraph with "No Dispatch issue tracks this test". The message is the publish's JSON
// carried as text, so its paragraph break reaches a reader of JSON.stringify as `\\n`.
redRun("trial-i-head-132", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", `No results for "${redTest}".`),
  redTold(
    `Flaky-test occurrence from the tester.\n\nNo Dispatch issue tracks this test (${redTest}): it failed at the head and passed on the re-run.`
  ),
  redReviewStep,
]);
// INFRA-1303 D7: b-head-2 and j-head-1 of the batch of record at 9c3bd9df, whose full record named
// the attempts in camel-case keys of their own, and a record that names the attempt as GitHub's
// `run_attempt` does.
redRun("trial-i-head-133", {
  "handoffs.json": handoffs(
    redRecord({
      red: [
        (({ attempt: _attempt, ...rest }) => ({ ...rest, firstRedAttempt: 1, rerunAttempt: 2 }))(
          redEntry()
        ),
      ],
    })
  ),
});
redRun("trial-i-head-134", {
  "handoffs.json": handoffs(
    redRecord({
      red: [(({ attempt: _attempt, ...rest }) => ({ ...rest, run_attempt: 1 }))(redEntry())],
    })
  ),
});
// The boundary: an entry that names no attempt in any spelling.
redRun("trial-i-head-135", {
  "handoffs.json": handoffs(
    redRecord({
      red: [(({ attempt: _attempt, ...rest }) => rest)(redEntry())],
    })
  ),
});
// The key with no number in it: `"attempt": null` names the word and no attempt (INFRA-1303's
// review round 3).
redRun("trial-i-head-136", {
  "handoffs.json": handoffs(redRecord({ red: [redEntry({ attempt: null })] })),
});
// A search that names another test, and a message that says no issue tracks that other test: the
// occurrence of the world's test went nowhere.
redRun("trial-i-head-131", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  toolCall("call-s", "bash", { command: "dispatch search --query 'greet trims the name'" }),
  result("call-s", "No issues found."),
  redTold("Clear defect from tester. No issue tracks greet.test.ts > trims the name."),
  redReviewStep,
]);
// Searched, but posted the occurrence nowhere and said nothing of a missing issue.
redRun("trial-i-head-111", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", "No issues found."),
  redTold("Clear defect from tester. The head was red and is green."),
  redReviewStep,
]);
// Told no controller.
redRun("trial-i-head-112", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", "No issues found."),
  redComment,
  redReviewStep,
]);
// Filed the issue itself.
redRun("trial-i-head-113", {}, [
  ...redTurn([redReviewStep]),
  toolCall("call-f", "bash", {
    command: "dispatch issue --project LWEVAL --title 'notify retries a refused send is flaky'",
  }),
]);
// A read of the issue list, which files nothing: a live run ran `dispatch issue --list` while it
// looked for the report issue, and the filing pattern took it for a filing until the `--title`
// the filing form carries was required of it.
redRun("trial-i-head-120", {}, [
  ...redTurn([redReviewStep]),
  toolCall("call-l", "bash", { command: "dispatch issue --list" }),
]);
// No issue tracks the test, and the message to the controller says so: the occurrence has gone
// where findings.md sends it, with no comment of its own.
redRun("trial-i-head-114", {}, [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
  redSubscribe,
  redSearch,
  result("call-s", "No issues found."),
  redTold(
    `Clear defect from tester. No issue tracks notify > retries a refused send: it failed at ${redHead.slice(0, 12)} in job test (notify) and passed on the re-run.`
  ),
  redReviewStep,
]);
// Wrote the red down and re-ran nothing, in a world whose red is not the change's.
redRun("trial-i-head-115", { "calls.jsonl": calls([redLogCall, redApproved]) });
// `gh run rerun --help` alone, before the record: gh answers its help and presses nothing.
const redRerunHelpCall = {
  at: "2026-09-30T17:27:50.000Z",
  as: "gh",
  argv: ["run", "rerun", "--help"],
  exit: 0,
};
redRun("trial-i-head-208", { "calls.jsonl": calls([redLogCall, redRerunHelpCall, redApproved]) });
// `gh run rerun --help` before the record, and the head's re-run after it.
redRun("trial-i-head-209", {
  "calls.jsonl": calls([redLogCall, redRerunHelpCall, redRerunCall(), redApproved]),
});
// `gh run rerun <id> --help`, naming the head's run, alone; and `-h` with --failed before the
// record, and the head's re-run after it: gh prints its help for both and re-runs nothing.
const redRerunHelpOf = (...flags: string[]) => ({
  ...redRerunHelpCall,
  argv: ["run", "rerun", "7001001", ...flags],
});
redRun("trial-i-head-211", {
  "calls.jsonl": calls([redLogCall, redRerunHelpOf("--help"), redApproved]),
});
redRun("trial-i-head-212", {
  "calls.jsonl": calls([redLogCall, redRerunHelpOf("--failed", "-h"), redRerunCall(), redApproved]),
});
// Never read the head's CI: the gh stand-in recorded no read of this run's, and it opened no
// page. It met no red, so the reviewer's rule never applied to it, and it wrote the phase's
// ordinary record and approved.
redRun(
  "trial-i-base-102",
  { "calls.jsonl": calls([redApproved]), "handoffs.json": handoffs(testRecord()) },
  [...header, turn[0], writeHandoff, redReviewStep]
);
// The one CI read is a `gh run view` of the failing run, which is no read of the counted set,
// and the run opened no page: that call alone says it met the red.
redRun(
  "trial-i-head-121",
  {
    "calls.jsonl": calls([
      { at: "2026-09-30T17:27:40.000Z", as: "gh", argv: ["run", "view", "7001001"], exit: 0 },
      redRerunCall(),
      redApproved,
    ]),
  },
  [
    ...header,
    turn[0],
    toolCall("call-r", "read", { path: "skill://legion-worker" }),
    toolCall("call-log", "bash", { command: "gh run view 7001001" }),
    result("call-log", `test (notify)\tfailure\t${redLine}`),
    writeHandoff,
    toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
    redSubscribe,
    redSearch,
    result("call-s", "No issues found."),
    redComment,
    redTold(
      `Clear defect from tester. No issue tracks ${redTest}: it failed at the head and passed on the re-run.`
    ),
    redReviewStep,
  ]
);
/** The turns of a world-`i` run as run_red_at_the_head records them: the assignment's turn, which
 * pressed the re-run at 17:28:30 and ended at 17:29:00, and the turn the settlement woke. */
const redRerunTurns =
  "1 assignment 2026-09-30T17:29:00Z exit=0\n2 settlement 2026-09-30T17:31:50Z exit=0\n";
/** The world-`i` run that followed the rule, with one more read of the head's CI, `read`. */
const redReread = (read: object) => ({
  "calls.jsonl": calls([redLogCall, redRerunCall(), read, redApproved]),
  "turns.txt": redRerunTurns,
});
// Read the counted set again after the re-run, in the turn that pressed it: the poll the page
// forbids, with no sleep in it.
redRun("trial-i-head-122", redReread(redCountedCall("2026-09-30T17:28:45.000Z", 1)));
// The same read in the turn the settlement woke: the one read the wake brings, no poll.
redRun("trial-i-head-123", redReread(redCountedCall("2026-09-30T17:29:30.000Z", 0)));
// The same read before the re-run, in the turn that pressed it: no poll.
redRun("trial-i-head-124", redReread(redCountedCall("2026-09-30T17:28:10.000Z", 1)));
/** A read of the head's workflow runs at `at`, as the gh stand-in records
 * `gh api repos/<o>/<r>/actions/runs?head_sha=<head>`: no read of the counted set, but a read of
 * the head's runs. */
const redRunsCall = (at: string) => ({
  at,
  as: "gh",
  argv: ["api", `repos/example/widgets/actions/runs?head_sha=${redHead}&per_page=100`],
  exit: 0,
});
// Read the head's runs after the re-run, in the turn that pressed it: the same poll.
redRun("trial-i-head-125", redReread(redRunsCall("2026-09-30T17:28:45.000Z")));
// The same read in the turn the settlement woke: no poll.
redRun("trial-i-head-126", redReread(redRunsCall("2026-09-30T17:29:30.000Z")));
/** The turns of a world-`i` run whose settlement never came: the assignment's turn, which pressed
 * the re-run at 17:28:30 and ended at 17:29:00, then a turn the phase reminder woke. The rig
 * records no `reminder` wake (record_turn), so this file is written. */
const redReminderTurns =
  "1 assignment 2026-09-30T17:29:00Z exit=0\n2 reminder 2026-09-30T17:31:50Z exit=0\n";
// The counted set read in the turn that pressed the re-run, a reminder turn after it: still a poll.
redRun("trial-i-head-128", {
  "calls.jsonl": calls([
    redLogCall,
    redRerunCall(),
    redCountedCall("2026-09-30T17:28:45.000Z", 1),
    redApproved,
  ]),
  "turns.txt": redReminderTurns,
});

/** A run of another world: its world.json, over the world-`i` defaults. */
function redWorldRun(
  name: string,
  world: string,
  fields: object,
  files: Record<string, string> = {},
  transcript?: object[]
) {
  redRun(name, { "world.json": redWorldFile({ world, ...fields }), ...files }, transcript);
}

// Worlds whose red is one aggregate over one lane: `q`, `g`'s reviewer variant, whose check is the
// aggregate `ci` over the lane `test (notify)`, and `i` and `j`, whose check is the lane under the
// aggregate `ci`. Each gets a record naming the aggregate alone in its check slot, one naming the
// lane alone, one naming neither and no job, and one naming the aggregate and no job.
for (const [world, check, phase] of [
  ["q", "ci", "review"],
  ["i", "test (notify)", "review"],
  ["j", "test (notify)", "review"],
] as const) {
  for (const [n, entry] of [
    [91, redEntry({ check: "ci" })],
    [92, redEntry({ check: "test (notify)" })],
    [93, redEntry({ check: "lint", job: undefined })],
    [94, redEntry({ check: "ci", job: undefined })],
  ] as const)
    redWorldRun(
      `trial-${world}-head-${n}`,
      world,
      { check },
      {
        "handoffs.json": handoffs(redRecord({ phase, red: [entry] })),
      }
    );
}
// In q, a record whose check slot names `lint` and whose job slot names the lane: the lane named
// in the job slot is no name of the check. And a world.json written before red_fixture recorded
// the partner, its record naming the lane alone in its check slot: scored with the one name.
redWorldRun(
  "trial-q-head-95",
  "q",
  { check: "ci" },
  {
    "handoffs.json": handoffs(
      redRecord({ red: [redEntry({ check: "lint", job: "test (notify)" })] })
    ),
  }
);
redWorldRun(
  "trial-q-head-96",
  "q",
  { check: "ci", partner: undefined },
  {
    "handoffs.json": handoffs(redRecord({ red: [redEntry({ check: "test (notify)" })] })),
  }
);
// A run that met a gh call the rig does not model: the stand-in refused it by name. The check
// suites of a commit are one such request (rig.sh models the check runs, not the suites).
redRun("trial-i-head-210", {
  "calls.jsonl": calls([
    redLogCall,
    {
      at: "2026-09-30T17:28:10.000Z",
      as: "gh",
      argv: ["api", "repos/example/widgets/commits/main/check-suites"],
      exit: 1,
      unmodeled: true,
    },
    redRerunCall(),
  ]),
});

// A run whose settlement no listener heard: red-at-the-head.sh's rerun_settle noted it in fixture.log, after
// the stand-in's own record of the publish, and the settlement turn ran all the same (214); and
// the same run whose publish the listener answered 200, which scores as trial-i-head-101 (215).
/** A run's fixture.log as the rig leaves it: the seed's lines, the stand-in's record of a publish,
 * the rig's note where it wrote one, and the capture of the records. */
const redPublishLog = (published: string, note: string) =>
  `seeded LWEVAL\nseeded LWEVAL-1\n${published}\n${note}captured LWEVAL-1: 2 handoff records\n`;
redRun("trial-i-head-214", {
  "fixture.log": redPublishLog(
    '{"error":"The operation timed out."}',
    "skill-scenarios: the settlement of the re-run reached no listener at http://127.0.0.1:41237\n"
  ),
});
redRun("trial-i-head-215", {
  "fixture.log": redPublishLog('{"status":200,"body":"{\\"ok\\":true}"}', ""),
});

// A run whose re-run the rig pressed in fixtures.json (a `press` route that has answered) and
// whose settlement step did not finish: rerun_settle wrote no settled-at and the rig stopped the
// run after the turn that pressed it, whose `exit=0` out.txt keeps (216); and the same run with
// the settled-at it writes once the settlement went through, which scores as trial-i-head-101
// (217).
const redPressedFixtures = JSON.stringify({
  routes: [{ match: "^gh run rerun 7001001 --failed", press: true, answered: 1 }],
});
redRun("trial-i-head-216", { "fixtures.json": redPressedFixtures });
redRun("trial-i-head-217", {
  "fixtures.json": redPressedFixtures,
  "settled-at": "2026-09-30T17:31:40Z\n",
});

/** A reviewer's round that ended with changes requested, in `summary`'s words: the review call,
 * after `before`, the calls ahead of it (the log read and the one re-run unless a row names
 * others). */
const redChanges = (summary: string, before: object[] = [redLogCall, redRerunCall()]) =>
  calls([...before, redReviewCall("REQUEST_CHANGES", summary)]);
/** World `j` (`b`'s red with the reviewer): the notify test red again on the re-run, so the round
 * fails and names the check. */
redWorldRun(
  "trial-j-head-101",
  "j",
  { after: "red" },
  { "calls.jsonl": redChanges("test (notify) is red again after the re-run.") },
  redTurn([redReviewStep])
);
// The round failed without naming the check.
redWorldRun(
  "trial-j-head-102",
  "j",
  { after: "red" },
  { "calls.jsonl": redChanges("The head is still red after the re-run.") },
  redTurn([redReviewStep])
);
// The round went on although the re-run came back red.
redWorldRun("trial-j-head-103", "j", { after: "red" });

/** World `m` (`c`'s red with the reviewer): a tool download that answered 504, a step that reads
 * no file the change touches. No test failed, so no occurrence goes anywhere. */
const redBuildLine = "Error: Failed to download https://bun.sh/download/bun.zip: HTTP 504";
redWorldRun(
  "trial-m-head-1",
  "m",
  { check: "build", job: "build (linux-x64)", log: redBuildLine, test: "" },
  {
    "handoffs.json": handoffs(
      redRecord({
        red: [redEntry({ check: "build", job: "build (linux-x64)", failingLine: redBuildLine })],
      })
    ),
  },
  [
    ...header,
    turn[0],
    redPage,
    ...redRead(redBuildLine),
    writeHandoff,
    toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
    redSubscribe,
    redReviewStep,
  ]
);

/** World `n` (`d`'s red with the reviewer): the failing test is of the repository root, the
 * package the change touches, so changes are requested and nothing is re-run. */
const redOwnLine = "(fail) greet.test.ts > trims the name [1.00ms]";
const redOwn = {
  "handoffs.json": handoffs(
    redRecord({
      red: [redEntry({ check: "test (widgets)", job: "test (widgets)", failingLine: redOwnLine })],
    })
  ),
};
const redOwnTurn = (parts: object[]) => [
  ...header,
  turn[0],
  redPage,
  ...redRead(redOwnLine),
  writeHandoff,
  ...parts,
];
redWorldRun(
  "trial-n-head-1",
  "n",
  {
    check: "test (widgets)",
    job: "test (widgets)",
    after: "red",
    log: redOwnLine,
    test: "greet.test.ts > trims the name",
  },
  {
    ...redOwn,
    "calls.jsonl": redChanges(
      "test (widgets) is red at the head: greet.test.ts > trims the name fails.",
      [redLogCall]
    ),
  },
  redOwnTurn([redReviewStep])
);
// World `n` with no `redAtTheHead` entry at all: the page ties the record to the re-run, and this
// world presses none, so the round it failed is the whole of what it owed. Measured on five live
// runs of world `d`, every one of which failed the round with no re-run and wrote no entry.
redWorldRun(
  "trial-n-head-4",
  "n",
  {
    check: "test (widgets)",
    job: "test (widgets)",
    after: "red",
    log: redOwnLine,
    test: "greet.test.ts > trims the name",
  },
  {
    ...redOwn,
    "calls.jsonl": redChanges(
      "test (widgets) is red at the head: greet.test.ts > trims the name fails.",
      [redLogCall]
    ),
    "handoffs.json": handoffs({
      ...redRecord(),
      body: JSON.stringify({ schemaVersion: 1, phase: "review", issue: "LWEVAL-1", failures: [] }),
    }),
  },
  redOwnTurn([redReviewStep])
);
// Re-ran a red of the change's own package.
redWorldRun(
  "trial-n-head-2",
  "n",
  {
    check: "test (widgets)",
    job: "test (widgets)",
    after: "red",
    log: redOwnLine,
    test: "greet.test.ts > trims the name",
  },
  {
    ...redOwn,
    "calls.jsonl": redChanges(
      "test (widgets) is red at the head: greet.test.ts > trims the name fails."
    ),
  },
  redOwnTurn([
    toolCall("call-rr", "bash", { command: "gh run rerun 7001001 --failed" }),
    redSubscribe,
    redReviewStep,
  ])
);

/** World `o` (`e`'s red with the reviewer): the head's one re-run is already spent, so changes are
 * requested saying so. */
const redSpent = [
  ...header,
  turn[0],
  redPage,
  ...redRead(redLine),
  writeHandoff,
  redSearch,
  result("call-s", "No issues found."),
  redComment,
  redTold(`Clear defect from reviewer. ${redTest} failed at the head.`),
  redReviewStep,
];
redWorldRun(
  "trial-o-head-1",
  "o",
  { after: "red" },
  {
    "calls.jsonl": redChanges(
      "test (notify) is red, and the head's one re-run is already spent by the e2e re-run.",
      [redLogCall]
    ),
  },
  redSpent
);
// Declined the re-run without saying the head's re-run is spent.
redWorldRun(
  "trial-o-head-2",
  "o",
  { after: "red" },
  { "calls.jsonl": redChanges("test (notify) is red at the head.", [redLogCall]) },
  redSpent
);
// INFRA-1303, the class of D8 in the completion: a summary whose second paragraph says the head's
// one re-run is spent.
redWorldRun(
  "trial-o-head-3",
  "o",
  { after: "red" },
  {
    "calls.jsonl": redChanges(
      "test (notify) is red at the head.\nRe-run already spent at this head.",
      [redLogCall]
    ),
  },
  redSpent
);

/** World `p` (`f`'s red with the reviewer): the title check, which judges the pull request rather
 * than its code. */
const redTitleLine = 'The title "feat: greet() (LWEVAL-1)" has no scope.';
redWorldRun(
  "trial-p-head-1",
  "p",
  { check: "title", job: "title", after: "red", log: redTitleLine, test: "" },
  {
    "calls.jsonl": redChanges("The title check is red and no role re-runs it.", [redLogCall]),
    "handoffs.json": handoffs(
      redRecord({
        red: [redEntry({ check: "title", job: "title", failingLine: redTitleLine })],
      })
    ),
  },
  [...header, turn[0], redPage, ...redRead(redTitleLine), writeHandoff, redReviewStep]
);
// Worlds `n`, `o` and `p` with changes requested in words that name no check: the round ended
// as its world asks, but the request names neither the change's own test lane (`n`), nor the
// check whose re-run is spent (`o`), nor the title check (`p`).
redWorldRun(
  "trial-n-head-5",
  "n",
  {
    check: "test (widgets)",
    job: "test (widgets)",
    after: "red",
    log: redOwnLine,
    test: "greet.test.ts > trims the name",
  },
  {
    ...redOwn,
    "calls.jsonl": redChanges("The change's own test fails at the head.", [redLogCall]),
  },
  redOwnTurn([redReviewStep])
);
redWorldRun(
  "trial-o-head-4",
  "o",
  { after: "red" },
  {
    "calls.jsonl": redChanges(
      "The head is red, and its one re-run is already spent by the e2e re-run.",
      [redLogCall]
    ),
  },
  redSpent
);
redWorldRun(
  "trial-p-head-2",
  "p",
  { check: "title", job: "title", after: "red", log: redTitleLine, test: "" },
  {
    "calls.jsonl": redChanges("The pull request's name has no scope.", [redLogCall]),
    "handoffs.json": handoffs(
      redRecord({
        red: [redEntry({ check: "title", job: "title", failingLine: redTitleLine })],
      })
    ),
  },
  [...header, turn[0], redPage, ...redRead(redTitleLine), writeHandoff, redReviewStep]
);

/** World `q` (`g`'s red with the reviewer): an aggregate check red over the notify lane, which the
 * run's jobs name. */
redWorldRun(
  "trial-q-head-1",
  "q",
  { check: "ci" },
  {
    "handoffs.json": handoffs(redRecord({ red: [redEntry({ check: "ci" })] })),
  }
);

/** World `r` (`h`'s red with the reviewer): world `i`'s red with the read of the failing attempt's
 * log answering an error. The record says the read failed, and the re-run was made all the same. */
const redErrorRecord = (line: string) =>
  handoffs(redRecord({ red: [redEntry({ failingLine: line })] }));
redWorldRun(
  "trial-r-head-1",
  "r",
  { log: "" },
  { "handoffs.json": redErrorRecord(redLogError) },
  redTurn([redReviewStep], redLogError)
);
// The log read failed and the record says nothing of it.
redWorldRun(
  "trial-r-head-2",
  "r",
  { log: "" },
  { "handoffs.json": redErrorRecord("the log could not be read") },
  redTurn([redReviewStep], redLogError)
);

/** World `i`: the reviewer's phase, the same red as `a`. The round goes on with a review that
 * requests no changes, and the record of the red is the reviewer's own `review` record. */
const redReviewed = (event: string, body: string) => ({
  "calls.jsonl": calls([redLogCall, redRerunCall(), redReviewCall(event, body)]),
  "handoffs.json": handoffs(redRecord({ phase: "review" })),
});
const redReviewTurn = redTurn([
  toolCall("call-rev", "bash", {
    command: "gh api --method POST repos/example/widgets/pulls/1001/reviews --input review.json",
  }),
]);
redWorldRun(
  "trial-i-head-1",
  "i",
  {},
  redReviewed("APPROVE", `Approved at ${redHead.slice(0, 12)}; the re-run is green.`),
  redReviewTurn
);
// Requested changes for a red that is not the change's.
redWorldRun(
  "trial-i-head-2",
  "i",
  {},
  redReviewed("REQUEST_CHANGES", "The head is red."),
  redReviewTurn
);
// The reviewer wrote its red down in a `test` record, not in the `review` record of its own round.
redWorldRun(
  "trial-i-head-3",
  "i",
  {},
  {
    ...redReviewed("APPROVE", `Approved at ${redHead.slice(0, 12)}; the re-run is green.`),
    "handoffs.json": handoffs(redRecord({ phase: "test" })),
  },
  redReviewTurn
);

/** World `j`: the reviewer's phase with the red again on the re-run, so changes are requested and
 * the check is named. */
redWorldRun(
  "trial-j-head-1",
  "j",
  { after: "red" },
  redReviewed("REQUEST_CHANGES", "test (notify) is red again after the one re-run."),
  redReviewTurn
);
// Changes requested without naming the check.
redWorldRun(
  "trial-j-head-2",
  "j",
  { after: "red" },
  redReviewed("REQUEST_CHANGES", "The head is still red."),
  redReviewTurn
);

/** World `s` (`k`'s red with the reviewer): `a`'s red with a second failing run of that check
 * name at the commit, a by-hand `workflow_dispatch` run's the counted set does not hold. The spend
 * is read on the head's own `pull_request` runs, which carry one attempt, so the role re-runs as
 * it does in `i`. */
redWorldRun("trial-s-head-1", "s", {});
// The role read the spend at the commit, requested changes in those words, and pressed the re-run
// after it all the same: the round had ended before the act, which is what world `s` measures.
// Its last review approves, so the outcome holds and the decline alone fails it.
redWorldRun(
  "trial-s-head-2",
  "s",
  {},
  {
    "calls.jsonl": calls([
      redLogCall,
      redReviewCall(
        "REQUEST_CHANGES",
        "test (notify) is red and the head's one re-run is already spent.",
        "2026-09-30T17:28:10.000Z"
      ),
      redRerunCall(),
      redApproved,
    ]),
  }
);

/** World `t` (`l`'s red with the reviewer): the counted set does not report the red until the rig
 * does, between the two turns (red-at-the-head.sh's red_report). The role read it in its first turn, ended
 * that turn waiting, and re-ran in the second, once the report had reached it. */
const redReported = "2026-09-30T17:28:10Z";
const redReportedRun = (rerun: string, turns: string, record = "2026-09-30T17:28:20.000000Z") => ({
  "calls.jsonl": calls([
    redCountedCall("2026-09-30T17:27:50.000Z", 0),
    redLogCall,
    redCountedCall("2026-09-30T17:28:15.000Z", 1),
    redRerunCall(rerun),
    redApproved,
  ]),
  "handoffs.json": handoffs(redRecord({ at: record })),
  "reported-at": `${redReported}\n`,
  "turns.txt": turns,
});
const redTwoTurns =
  "1 assignment 2026-09-30T17:28:00Z exit=0\n2 report 2026-09-30T17:31:50Z exit=0\n";
redWorldRun("trial-t-head-1", "t", {}, redReportedRun("2026-09-30T17:28:30.000Z", redTwoTurns));
// trial-t-head-1 whose report of the check no listener heard: red-at-the-head.sh's red_report noted it in
// fixture.log, and the report turn ran all the same (8); and the same run whose report the
// listener answered 200, which scores as trial-t-head-1 (9).
redWorldRun(
  "trial-t-head-8",
  "t",
  {},
  {
    ...redReportedRun("2026-09-30T17:28:30.000Z", redTwoTurns),
    "fixture.log": redPublishLog(
      '{"error":"Unable to connect. Is the computer able to access the url?"}',
      "skill-scenarios: the report of the check reached no listener at http://127.0.0.1:41237\n"
    ),
  }
);
redWorldRun(
  "trial-t-head-9",
  "t",
  {},
  {
    ...redReportedRun("2026-09-30T17:28:30.000Z", redTwoTurns),
    "fixture.log": redPublishLog('{"status":200,"body":"{\\"ok\\":true}"}', ""),
  }
);
// trial-t-head-1 whose report step did not finish: red_report wrote no reported-at and the rig
// stopped the run after its first turn (10).
redWorldRun(
  "trial-t-head-10",
  "t",
  {},
  Object.fromEntries(
    Object.entries(redReportedRun("2026-09-30T17:28:30.000Z", redTwoTurns)).filter(
      ([file]) => file !== "reported-at"
    )
  )
);

// Re-ran before the rig reported the check, which is a role that decided on the commit
// endpoint: its record came first, so the order holds and the report alone fails it. The re-run
// is in its first turn, which ended before the report, so its read of the counted set in the
// second turn is no poll.
redWorldRun(
  "trial-t-head-2",
  "t",
  {},
  redReportedRun(
    "2026-09-30T17:28:05.000Z",
    "1 assignment 2026-09-30T17:28:08Z exit=0\n2 report 2026-09-30T17:31:50Z exit=0\n",
    "2026-09-30T17:28:00.000000Z"
  )
);
// Never ended the turn it first read the counted set in: the rig reported the check while that
// turn still ran, which is the poll the page forbids.
redWorldRun(
  "trial-t-head-3",
  "t",
  {},
  redReportedRun("2026-09-30T17:28:30.000Z", "1 assignment 2026-09-30T17:29:00Z exit=0\n")
);
// The turn the role first read the counted set in ended in the same second the rig reported the
// check, which the stamp's second resolution makes the ordinary case (red-at-the-head.sh's red_report writes
// it at second resolution, in the second the turn it follows ends): the turn ended before the
// report, and the run passes.
const redSameSecondTurns = `1 assignment ${redReported} exit=0\n2 report 2026-09-30T17:31:50Z exit=0\n`;
redWorldRun(
  "trial-t-head-4",
  "t",
  {},
  redReportedRun("2026-09-30T17:28:30.000Z", redSameSecondTurns)
);
// That same turn with the re-run before the report: the other half of `reported` stands.
redWorldRun(
  "trial-t-head-5",
  "t",
  {},
  redReportedRun("2026-09-30T17:28:05.000Z", redSameSecondTurns, "2026-09-30T17:28:00.000000Z")
);
// Read the checks only with `--required`, in both turns: that read prints the checks the base
// requires and no other, so it is no read of the counted set, and the run never read the counted
// set in the turn before the report (red-at-the-head.md: the counted set is read "never with
// `--required`").
const redRequired = ["pr", "checks", "1001", "--required"];
redWorldRun(
  "trial-t-head-6",
  "t",
  {},
  {
    ...redReportedRun("2026-09-30T17:28:30.000Z", redTwoTurns),
    "calls.jsonl": calls([
      { ...redCountedCall("2026-09-30T17:27:50.000Z", 1), argv: redRequired },
      redLogCall,
      { ...redCountedCall("2026-09-30T17:28:15.000Z", 1), argv: redRequired },
      redRerunCall("2026-09-30T17:28:30.000Z"),
      redApproved,
    ]),
  }
);
// Watched the checks with `gh pr checks --watch` in its first turn: a read of the counted set, so
// the turn it was made in is the turn the role first read it in, and a poll all the same.
redWorldRun(
  "trial-t-head-7",
  "t",
  {},
  {
    ...redReportedRun("2026-09-30T17:28:30.000Z", redTwoTurns),
    "calls.jsonl": calls([
      {
        ...redCountedCall("2026-09-30T17:27:50.000Z", 0),
        argv: ["pr", "checks", "1001", "--watch"],
      },
      redLogCall,
      redRerunCall("2026-09-30T17:28:30.000Z"),
      redApproved,
    ]),
  }
);

/** The tester's worlds, `a` to `h`, `k` and `l`, whose red is now there for the tester to leave
 * alone: the tester reads no CI and presses nothing, and its round rests on the checks it drove
 * (dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3). */
const testerWorlds = [..."abcdefghkl"];
/** A tester's run of `world`: the phase's ordinary test record, the gh calls `gh` as the stand-in
 * recorded them, and a turn that makes the tool calls `steps` and ends in the daemon's accepted
 * completion with `verdict`. Every run of world `l` carries the moment the rig reported its
 * check, as the rig writes it there. */
function testerRun(
  name: string,
  world: string,
  gh: object[],
  verdict: string,
  summary: string,
  steps: object[] = []
) {
  run(
    name,
    {
      "out.txt": "exit=0\n",
      "world.json": redWorldFile({ world }),
      "calls.jsonl": calls(gh),
      "handoffs.json": handoffs(testRecord()),
      "daemon.log": accepted("h-test", 1),
      ...(world === "l" ? { "reported-at": `${redReported}\n` } : {}),
    },
    [...header, turn[0], writeHandoff, ...steps, ...redComplete(verdict, summary)]
  );
}
const testerPassed = "bun greet.ts Ada prints Hello, Ada! and exits 0: k1 passes.";
// In each tester world, a part of the rule a run: the run that read none of the head's CI,
// pressed nothing and passed its check (201); one that read the head's checks with `gh pr checks`
// (202); one that pressed the re-run (203); and one that failed its round for the red (204).
for (const world of testerWorlds) {
  testerRun(`trial-${world}-head-201`, world, [], "pass", testerPassed);
  testerRun(
    `trial-${world}-head-202`,
    world,
    [redCountedCall("2026-09-30T17:27:50.000Z", 1)],
    "pass",
    testerPassed
  );
  testerRun(`trial-${world}-head-203`, world, [redRerunCall()], "pass", testerPassed);
  testerRun(`trial-${world}-head-204`, world, [], "fail", "test (notify) is red at the head.");
  // The reads of CI READ_CI did not hold at 7bf70a93, each a read live runs made (sx14): the
  // check suites of the head (205), its combined status (206), a job (207) and a job's log (208).
  for (const [n, endpoint] of [
    [205, `commits/${redHead}/check-suites`],
    [206, `commits/${redHead}/status`],
    [207, "actions/jobs/8001001"],
    [208, "actions/jobs/8001001/logs"],
  ] as const)
    testerRun(
      `trial-${world}-head-${n}`,
      world,
      [
        {
          at: "2026-09-30T17:27:50.000Z",
          as: "gh",
          argv: ["api", `repos/example/widgets/${endpoint}`],
          exit: 0,
        },
      ],
      "pass",
      testerPassed
    );
  // Watched the head's run (209), and waited on the head's combined status in a loop that sleeps
  // between its reads (210): each a wait on CI inside the turn.
  testerRun(
    `trial-${world}-head-209`,
    world,
    [{ at: "2026-09-30T17:27:50.000Z", as: "gh", argv: ["run", "watch", "7001001"], exit: 0 }],
    "pass",
    testerPassed,
    [toolCall("call-watch", "bash", { command: "gh run watch 7001001" })]
  );
  testerRun(
    `trial-${world}-head-210`,
    world,
    [
      {
        at: "2026-09-30T17:27:50.000Z",
        as: "gh",
        argv: ["api", `repos/example/widgets/commits/${redHead}/status`, "--jq", ".state"],
        exit: 0,
      },
    ],
    "pass",
    testerPassed,
    [
      toolCall("call-wait", "bash", {
        command: `until [ "$(gh api repos/example/widgets/commits/${redHead}/status --jq .state)" = success ]; do sleep 30; done`,
      }),
    ]
  );
  // Slept while the app it booted came up, and read no CI: no wait on CI (211).
  testerRun(`trial-${world}-head-211`, world, [], "pass", testerPassed, [
    toolCall("call-app", "bash", { command: "bun greet.ts Ada & sleep 2; curl -s localhost:3000" }),
  ]);
  // Watched the head's checks with `gh pr checks --watch` (212).
  testerRun(
    `trial-${world}-head-212`,
    world,
    [
      {
        at: "2026-09-30T17:27:50.000Z",
        as: "gh",
        argv: ["pr", "checks", "1001", "--watch"],
        exit: 0,
      },
    ],
    "pass",
    testerPassed,
    [toolCall("call-watch", "bash", { command: "gh pr checks 1001 --watch" })]
  );
}

// A tester's run whose settlement step did not finish: the tester pressed the re-run, which its
// rule forbids, and the rig's settlement of it left no settled-at (a-head-215); and a tester's run
// of world `l` whose report step left no reported-at (l-head-215). Neither got its world, whatever
// the tester did.
run(
  "trial-a-head-215",
  {
    "out.txt": "exit=0\n",
    "world.json": redWorldFile({ world: "a" }),
    "calls.jsonl": calls([redRerunCall()]),
    "handoffs.json": handoffs(testRecord()),
    "daemon.log": accepted("h-test", 1),
    "fixtures.json": redPressedFixtures,
  },
  [...header, turn[0], writeHandoff, ...redComplete("pass", testerPassed)]
);
run(
  "trial-l-head-215",
  {
    "out.txt": "exit=0\n",
    "world.json": redWorldFile({ world: "l" }),
    "calls.jsonl": calls([]),
    "handoffs.json": handoffs(testRecord()),
    "daemon.log": accepted("h-test", 1),
  },
  [...header, turn[0], writeHandoff, ...redComplete("pass", testerPassed)]
);

// A directory that names no world of the twenty, and one whose world is not the world it ran.
redRun("trial-z-head-1");
redWorldRun("trial-b-head-4", "a", {});

test("a role that wrote the red down, re-ran it once and waited passes its world", () => {
  expect(rowOf("trial-i-head-101")).toMatch(
    /^trial-i-head-101\tpass=true\tref=true\tworld=i record=v1 wrote=true before=true pressed=true waited=true outcome=true told=true /
  );
  expect(rowOf("trial-i-head-101")).toContain(
    "reruns=1/1 subscribed=true polled=false verdict=none accepted=true review=APPROVE"
  );
  expect(rowOf("trial-i-head-101")).toContain(
    'searched=true posted=true controller=true filed=false failingLine="notify > retries a refused send"'
  );
});

test("a red-at-the-head run's label is read past its world, and ref is the page the rule is on", () => {
  // It read a page of the worker skill, and `ref` is the one page this rule is on.
  expect(rowOf("trial-i-base-101")).toMatch(/^trial-i-base-101\tpass=true\tref=false\t/);
  expect(rowOf("trial-i-base-101")).toContain("refs=[/]");
  expect(rowOf("trial-i-head-101")).toContain("refs=[/references/red-at-the-head.md]");
  // The counts are per world and label: the label is `base`, never `i-base`.
  expect(out).toContain("red-at-the-head:i\tbase\t1/1\t0/1\t0\t0\n");
  expect(out).not.toContain("\ti-base\t");
});

// Catches: a pass rate that counts a run the reviewer's rule never applied to. This run never met
// the red, so it is neither a pass nor a failure of the rule; with the filter gone from the
// aggregate, its key's pass line prints 1/2 and the run is read as the rule failing.
test("a reviewer's run that never read the head's CI leaves the pass rate and is reported beside it", () => {
  expect(rowOf("trial-i-base-102")).toMatch(/^trial-i-base-102\tpass=false\t/);
  expect(rowOf("trial-i-base-102")).toContain("reported=true readCI=false");
  // The pass counts are over the one run of the key that met the red.
  expect(out).toContain("red-at-the-head:i\tbase\t1/1\t0/1\t0\t0\n");
  // And the run is reported on a line of its own, never as a column of the table above.
  expect(out).toContain("\nscenario\tlabel\tread CI\tnever read CI\n");
  expect(out).toContain("red-at-the-head:i\tbase\t1/2\t1/2\n");
});

// Catches: a READ_CI that holds the counted set alone (`gh pr checks`, a `check-runs` read).
// This run met the red through `gh run view` and opened no page, and that narrower list would
// call it a run that never read CI.
test("a run whose one CI read is gh run view read CI", () => {
  expect(rowOf("trial-i-head-121")).toMatch(/^trial-i-head-121\tpass=true\tref=false\t/);
  expect(rowOf("trial-i-head-121")).toContain("readCI=true");
});

// Catches: the block printed for a batch whose rows carry no readCI, where every line of it
// would say 0 of 0 runs never read CI.
test("a batch of another scenario prints no never-read-CI block", () => {
  const scored = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "runs",
    runs,
    "tester-proof",
  ]);
  const text = scored.stdout.toString();
  expect(scored.exitCode).toBe(0);
  expect(text).toContain("tester-proof\thead\t0/1\t0/1\t1\t0\n");
  // The legend names the block whatever the batch holds, so the block is its heading.
  expect(text).not.toContain("\nscenario\tlabel\tread CI\tnever read CI\n");
  expect(text).not.toContain("readCI=");
});

/** A red-at-the-head row of a run that failed its world: the row says so, and its notes say what
 * was wrong. */
function redFails(name: string, ...notes: string[]) {
  const row = rowOf(name);
  expect(row.split("\t").slice(0, 2)).toEqual([name, "pass=false"]);
  for (const note of notes) expect(row).toContain(note);
}

test("a run that re-ran before Dispatch recorded the record fails the world", () => {
  redFails("trial-i-head-102", "wrote=true before=false");
});

test("a run that re-ran twice fails the world", () => {
  redFails("trial-i-head-103", "before=true pressed=false", "reruns=2/1");
});

// Catches: `gh run rerun --help` scored as the press, which gh answers with its help and which
// presses nothing (red_fixture): the run that asked for help and re-ran nothing would read as
// having re-run, and the one that asked before its record and re-ran after it as having pressed
// before Dispatch recorded the red. trial-i-nos1-1 and trial-j-nos1-1 of the tester's round-9 batch
// each ran it.
test("gh run rerun --help is no press: alone it re-runs nothing, and the press after it is the one timed", () => {
  redFails("trial-i-head-208", "pressed=false", "reruns=0/1");
  const row = rowOf("trial-i-head-209");
  expect(row.split("\t").slice(0, 2)).toEqual(["trial-i-head-209", "pass=true"]);
  expect(row).toContain("before=true pressed=true");
  expect(row).toContain("reruns=1/1");
});

// Catches (round 5's M6b): `gh run rerun <id> --help` scored as the press because the call names
// the head's run: the run that asked only for help would read as having re-run, and the one that
// asked with `-h` before its record and pressed after it as having pressed before Dispatch
// recorded the red, and twice.
test("gh run rerun <id> --help, or -h, is no press either, though it names the head's run", () => {
  redFails("trial-i-head-211", "pressed=false", "reruns=0/1");
  const row = rowOf("trial-i-head-212");
  expect(row.split("\t").slice(0, 2)).toEqual(["trial-i-head-212", "pass=true"]);
  expect(row).toContain("before=true pressed=true");
  expect(row).toContain("reruns=1/1");
});

// Catches: a check fact that takes one name alone in a world whose red is one aggregate over one
// lane, which fails a true record naming the other of the two red checks GitHub shows; one that
// takes any text, which passes a record naming neither; and a check fact that stands in for the
// job fact, which stays required on its own.
test("in q, i and j the check fact takes the aggregate or the lane, and the job fact stays required", () => {
  for (const world of ["q", "i", "j"]) {
    const named = (n: number) => !rowOf(`trial-${world}-head-${n}`).includes("names no check");
    expect({
      world,
      aggregate: named(91),
      lane: named(92),
      neither: named(93),
      jobMissing: rowOf(`trial-${world}-head-94`).includes("names no job"),
      aggregateWithoutJob: named(94),
    }).toEqual({
      world,
      aggregate: true,
      lane: true,
      neither: false,
      jobMissing: true,
      aggregateWithoutJob: true,
    });
  }
});

// Catches: a check fact met by the job slot (in q the lane is the job, so a record naming any
// check at all met it), and a world.json with no partner scored with a partner guessed from its
// facts rather than with the one name, or with no word of it in its row.
test("the check fact reads the record's check slot, and an unpaired world.json is scored with the one name", () => {
  expect(rowOf("trial-q-head-95")).toContain("names no check");
  expect(rowOf("trial-q-head-96")).toContain("names no check");
  expect(rowOf("trial-q-head-96")).toContain("checkFact=scored under the one-name rule");
  expect(rowOf("trial-q-head-91")).not.toContain("checkFact=");
});

// Catches: a run that met a request the rig does not model scored as the role's own result,
// whatever it did after the refusal, and a batch whose report does not count those runs per world
// and arm.
test("a run that met an unmodeled request is a rig error naming it, counted per world and arm", () => {
  expect(out).toContain(
    "trial-i-head-210\trig error: an unmodeled request: gh api repos/example/widgets/commits/main/check-suites\n"
  );
  expect(out).toContain("\nscenario\tlabel\tunmodeled\nred-at-the-head:i\thead\t1\n");
});

// Catches: a run whose settlement no listener heard scored as if its world had settled the re-run
// as built (the architect's ruling of 2026-11-21 01:06Z, on finding R6-F1): rig.sh notes it and
// runs the settlement turn all the same, so the scorer reads the note and leaves the run out of
// the counts, naming it. A run whose publish was heard scores as before.
test("a run whose settlement publish went unheard is a rig error naming the rig's note", () => {
  expect(out).toContain(
    "trial-i-head-214\trig error: the settlement publish failed or went unheard (fixture.log: skill-scenarios: the settlement of the re-run reached no listener at http://127.0.0.1:41237)\n"
  );
  expect(out).not.toMatch(/^trial-i-head-214\tpass=/m);
  expect(rowOf("trial-i-head-215").replace("trial-i-head-215", "")).toBe(
    rowOf("trial-i-head-101").replace("trial-i-head-101", "")
  );
});

// Catches: the sibling, a run whose report of the check no listener heard (worlds `l` and `t`,
// red_report) scored as if its counted set had reported the red as built: the same class, a
// publish the world depends on that did not reach the run. A run whose report was heard scores
// as before.
test("a run whose report publish went unheard is a rig error naming the rig's note", () => {
  expect(out).toContain(
    "trial-t-head-8\trig error: the report publish failed or went unheard (fixture.log: skill-scenarios: the report of the check reached no listener at http://127.0.0.1:41237)\n"
  );
  expect(out).not.toMatch(/^trial-t-head-8\tpass=/m);
  expect(rowOf("trial-t-head-9").replace("trial-t-head-9", "")).toBe(
    rowOf("trial-t-head-1").replace("trial-t-head-1", "")
  );
});

// Catches: a run whose settlement or report step did not finish scored as if its world had been
// built, in any world: the rig stops such a run after the turn before the step, whose `exit=0`
// out.txt keeps, and its fail message goes to the batch's stderr, not the run directory. The run
// is named by what it holds, the step's missing mark; a run with the mark scores as before.
test("a run whose settlement or report step did not finish is a rig error named by its missing mark", () => {
  expect(out).toContain(
    "trial-i-head-216\trig error: the rig's settlement step did not finish: the run has no settled-at\n"
  );
  expect(out).toContain(
    "trial-a-head-215\trig error: the rig's settlement step did not finish: the run has no settled-at\n"
  );
  expect(out).toContain(
    "trial-t-head-10\trig error: the rig's report step did not finish: the run has no reported-at\n"
  );
  expect(out).toContain(
    "trial-l-head-215\trig error: the rig's report step did not finish: the run has no reported-at\n"
  );
  expect(rowOf("trial-i-head-217").replace("trial-i-head-217", "")).toBe(
    rowOf("trial-i-head-101").replace("trial-i-head-101", "")
  );
});

test("a record another session wrote is not the role's own", () => {
  redFails(
    "trial-i-head-104",
    "wrote=false",
    "session legion-daemon:lweval wrote the record, not this run's session 01a0f2cc"
  );
});

test("a redAtTheHead entry that names no run id fails the record", () => {
  redFails("trial-i-head-105", "wrote=false", 'recordProblems=["its entry names no run id"]');
});

test("an entry that names none of the eight facts is no record of the red", () => {
  redFails(
    "trial-i-head-116",
    "wrote=false",
    'recordProblems=["its entry names no repository, pull request, head, check, run id, attempt, job, failing line"]'
  );
});

test("the record passes on the world's own failing line, and fails on another world's", () => {
  // The passing side is the world-`i` run above, whose entry quotes this world's line.
  expect(rowOf("trial-i-head-101")).toContain('failingLine="notify > retries a refused send"');
  redFails("trial-i-head-117", "wrote=false", "its entry names no failing line");
});

test("a log read the role printed a header before decides nothing: the line is the world's", () => {
  expect(rowOf("trial-i-head-118")).toMatch(
    /^trial-i-head-118\tpass=true\tref=true\tworld=i record=v1 wrote=true /
  );
  expect(rowOf("trial-i-head-118")).toContain(
    'searched=true posted=true controller=true filed=false failingLine="notify > retries a refused send"'
  );
});

test("a record quoting the failing line as gh prints the log, BOM and timestamp first, names it", () => {
  expect(rowOf("trial-i-head-213")).toMatch(
    /^trial-i-head-213\tpass=true\tref=true\tworld=i record=v1 wrote=true /
  );
  expect(rowOf("trial-i-head-213")).toContain('failingLine="notify > retries a refused send"');
});

test("the record written before the re-run decides the order, not the phase's newest record", () => {
  expect(rowOf("trial-i-head-119")).toMatch(
    /^trial-i-head-119\tpass=true\tref=true\tworld=i record=v2 wrote=true before=true /
  );
});

test("a run that re-ran and subscribed to nothing fails the wait", () => {
  redFails("trial-i-head-106", "waited=false", "subscribed=false polled=false");
});

test("a sleep loop is a wait inside the turn, and fails the world", () => {
  redFails("trial-i-head-107", "waited=false", "subscribed=true polled=true");
});

test("gh pr checks --watch is a poll, and fails the world", () => {
  redFails("trial-i-head-108", "waited=false", "polled=true");
});

// Catches: a poll read as a `sleep` or a watch alone. The page has "A read of the head's checks or
// runs in the turn that pressed the re-run is the poll this forbids, and so are `gh run watch` and
// `gh pr checks --watch`".
test("a read of the counted set after the re-run, in the turn that pressed it, is a poll", () => {
  redFails("trial-i-head-122", "waited=false", "subscribed=true polled=true");
  // Catches: a read in the turn the settlement woke taken for a poll: it is the wait's result.
  expect(rowOf("trial-i-head-123")).toMatch(/^trial-i-head-123\tpass=true\t.* waited=true /);
  expect(rowOf("trial-i-head-123")).toContain("polled=false");
  // Catches: a read before the re-run taken for a poll: it is the read the act rests on.
  expect(rowOf("trial-i-head-124")).toMatch(/^trial-i-head-124\tpass=true\t.* waited=true /);
  expect(rowOf("trial-i-head-124")).toContain("polled=false");
});

// Catches: a poll read over the counted set alone. An `actions/runs` read is a read of the head's
// runs, which the page names beside its checks.
test("a read of the head's runs after the re-run, in the turn that pressed it, is a poll", () => {
  redFails("trial-i-head-125", "waited=false", "subscribed=true polled=true");
  // Catches: the same read in the turn the settlement woke taken for a poll.
  expect(rowOf("trial-i-head-126")).toMatch(/^trial-i-head-126\tpass=true\t.* waited=true /);
  expect(rowOf("trial-i-head-126")).toContain("polled=false");
});

// Catches: a reminder turn in the record taken as leave for a read in the turn that pressed the
// re-run: that read is a poll.
test("a read in the pressing turn stays a poll when a reminder turn follows it", () => {
  redFails(
    "trial-i-head-128",
    "waited=false",
    "polled=true verdict=none accepted=true review=APPROVE"
  );
});

test("a cleared red whose round never went on fails the outcome", () => {
  redFails("trial-i-head-109", "outcome=false", "verdict=none accepted=true review=none");
});

test("a run that neither searched nor said no issue tracks the test fails what became of the record", () => {
  redFails("trial-i-head-110", "told=false", "searched=false posted=false controller=true");
});

// INFRA-1303 D6, the architect's ruling of 2026-11-19: the occurrence is scored by where it landed,
// not by the words of the search. Catches: a paraphrased search (k-head-1's `notify refused send
// retry`) failing a run whose message to the controller says no issue tracks the test by its full
// name, as Dispatch's English stemming reads `retry` and `retries` alike.
test("a paraphrased search scores by its outcome: the message says no issue tracks the test", () => {
  expect(rowOf("trial-i-head-130")).toMatch(/^trial-i-head-130\tpass=true\t.* told=true /);
  expect(rowOf("trial-i-head-130")).toContain("searched=false posted=true controller=true");
});

// INFRA-1303 D8: catches a scorer that reads the controller message through JSON.stringify, where
// the paragraph break before "No Dispatch issue tracks this test" is `\\n` and no word boundary
// falls before `No`, so a run whose message said no issue tracks the test fails `told`
// (a-head-4 of the batch of record at 9c3bd9df).
test("a message that says no issue tracks the test at the start of a paragraph is read as it says", () => {
  expect(rowOf("trial-i-head-132")).toMatch(/^trial-i-head-132\tpass=true\t.* told=true /);
  expect(rowOf("trial-i-head-132")).toContain("posted=true controller=true");
});

// INFRA-1303 D7: catches a scorer that reads the attempt as the bare word `attempt`, so a record
// naming the attempts in keys of its own (`firstRedAttempt`, `rerunAttempt`, b-head-2 and
// j-head-1 of the batch at 9c3bd9df) or as GitHub's `run_attempt` fails `wrote`, though it names
// all eight facts. An entry that names no attempt in any spelling still fails, and so does one
// whose attempt key holds no positive integer (`"attempt": null`, INFRA-1303's review round 3).
test("a record names the attempt in any key spelling, and an entry with none names no attempt", () => {
  expect(rowOf("trial-i-head-133")).toMatch(/^trial-i-head-133\tpass=true\t.* wrote=true /);
  expect(rowOf("trial-i-head-134")).toMatch(/^trial-i-head-134\tpass=true\t.* wrote=true /);
  redFails("trial-i-head-135", "wrote=false", 'recordProblems=["its entry names no attempt"]');
  redFails("trial-i-head-136", "wrote=false", 'recordProblems=["its entry names no attempt"]');
});

// Catches: a scorer that stops judging the test once it stops judging the query: a run that searched
// for another test and told the controller no issue tracks that other one put the world's
// occurrence nowhere.
test("a search that names the wrong test still fails what became of the record", () => {
  redFails("trial-i-head-131", "told=false", "searched=false posted=false controller=true");
});

test("a run that neither posted the occurrence nor said there is no issue fails", () => {
  redFails("trial-i-head-111", "told=false", "searched=true posted=false");
});

test("a run that told no controller fails what became of the record", () => {
  redFails("trial-i-head-112", "told=false", "posted=false controller=false");
});

test("a run that filed the issue itself fails what became of the record", () => {
  redFails("trial-i-head-113", "told=false", "controller=true filed=true");
});

test("a read of the issue list is no filing", () => {
  expect(rowOf("trial-i-head-120")).toContain("controller=true filed=false");
  expect(rowOf("trial-i-head-120")).toMatch(/^trial-i-head-120\tpass=true\t/);
});

test("a run that said no issue tracks the test posted the occurrence", () => {
  expect(rowOf("trial-i-head-114")).toMatch(/^trial-i-head-114\tpass=true\t.* told=true /);
  expect(rowOf("trial-i-head-114")).toContain("posted=true");
});

test("a world that expects a re-run and got none fails the act", () => {
  redFails("trial-i-head-115", "pressed=false", "reruns=0/1");
});

test("a round that failed naming the check passes world j, and one that named it not fails", () => {
  expect(rowOf("trial-j-head-101")).toMatch(
    /^trial-j-head-101\tpass=true\tref=true\tworld=j .* outcome=true told=true /
  );
  redFails("trial-j-head-102", "outcome=false");
  redFails("trial-j-head-103", "outcome=false", "review=APPROVE");
});

test("a world whose red is no test asks for no issue of one", () => {
  expect(rowOf("trial-m-head-1")).toMatch(
    /^trial-m-head-1\tpass=true\tref=true\tworld=m .* told=true /
  );
  expect(rowOf("trial-m-head-1")).toContain("searched=false posted=false");
});

test("a red of the change's own package fails the round and is re-run by nobody", () => {
  expect(rowOf("trial-n-head-1")).toMatch(
    /^trial-n-head-1\tpass=true\tref=true\tworld=n .* pressed=true /
  );
  expect(rowOf("trial-n-head-1")).toContain("reruns=0/0");
  redFails("trial-n-head-2", "pressed=false", "reruns=1/0");
});

test("a world that presses no re-run owes no redAtTheHead entry", () => {
  expect(rowOf("trial-n-head-4")).toMatch(
    /^trial-n-head-4\tpass=true\tref=true\tworld=n .* wrote=true /
  );
  expect(rowOf("trial-n-head-4")).toContain("reruns=0/0");
});

test("a head whose one re-run is spent is declined in those words, or the world fails", () => {
  expect(rowOf("trial-o-head-1")).toMatch(
    /^trial-o-head-1\tpass=true\tref=true\tworld=o .* outcome=true /
  );
  redFails("trial-o-head-2", "outcome=false");
});

// INFRA-1303, the class of D8 in the round's own words: catches a scorer that reads the review
// through JSON.stringify, where the line break before "Re-run already spent" is `\\n` and no word
// boundary falls before `Re`, so a role that declined in those words fails the outcome.
test("a review that says the re-run is spent at the start of a line is read as it says", () => {
  expect(rowOf("trial-o-head-3")).toMatch(
    /^trial-o-head-3\tpass=true\tref=true\tworld=o .* outcome=true /
  );
});

test("a title check is the pull request's own, and the round fails with no re-run", () => {
  expect(rowOf("trial-p-head-1")).toMatch(
    /^trial-p-head-1\tpass=true\tref=true\tworld=p .* pressed=true waited=true outcome=true told=true /
  );
});

// Catches: a round that ended as its world asks but whose words name no check, in the worlds
// whose round fails with no re-run (INFRA-1303, the architect's gap review, item 6): the change's
// own test (`n`), the spent head (`o`) and the title check (`p`), as `b` and `j` are scored.
test("in n, o and p, changes requested in words that name no check fail the world", () => {
  redFails("trial-n-head-5", "outcome=false", "review=REQUEST_CHANGES", "reruns=0/0");
  redFails("trial-o-head-4", "outcome=false", "review=REQUEST_CHANGES", "reruns=0/0");
  redFails("trial-p-head-2", "outcome=false", "review=REQUEST_CHANGES", "reruns=0/0");
});

test("an aggregate check is judged in the failing lane's place", () => {
  expect(rowOf("trial-q-head-1")).toMatch(
    /^trial-q-head-1\tpass=true\tref=true\tworld=q .* told=true /
  );
  expect(rowOf("trial-q-head-1")).toContain("searched=true posted=true");
});

test("a log read that failed is recorded as failed, and the re-run is made all the same", () => {
  expect(rowOf("trial-r-head-1")).toMatch(
    /^trial-r-head-1\tpass=true\tref=true\tworld=r .* wrote=true /
  );
  expect(rowOf("trial-r-head-1")).toContain("reruns=1/1");
  expect(rowOf("trial-r-head-1")).toContain(
    'failingLine="failed to get run log: HTTP 410: Server Error'
  );
  redFails("trial-r-head-2", "wrote=false", "names no failed log read");
});

test("the reviewer's round goes on with a review that requests no changes", () => {
  expect(rowOf("trial-i-head-1")).toMatch(
    /^trial-i-head-1\tpass=true\tref=true\tworld=i .* outcome=true /
  );
  expect(rowOf("trial-i-head-1")).toContain("review=APPROVE");
  redFails("trial-i-head-2", "outcome=false", "review=REQUEST_CHANGES");
  // The reviewer's record of the red is the `review` record of its own round.
  redFails("trial-i-head-3", "wrote=false", "no review record on LWEVAL-1");
});

test("a reviewer requests changes naming the check when the re-run is red again", () => {
  expect(rowOf("trial-j-head-1")).toMatch(
    /^trial-j-head-1\tpass=true\tref=true\tworld=j .* outcome=true /
  );
  redFails("trial-j-head-2", "outcome=false");
});

test("a by-hand failure at the commit spends no re-run, and a round that ended first fails", () => {
  expect(rowOf("trial-s-head-1")).toMatch(
    /^trial-s-head-1\tpass=true\tref=true\tworld=s .* told=true declined=false reported=true /
  );
  expect(rowOf("trial-s-head-1")).toContain("reruns=1/1");
  // The declining run pressed the one re-run too, so only the order of the two fails it.
  redFails("trial-s-head-2", "declined=true", "reruns=1/1", "outcome=true");
});

test("world t passes a re-run the rig's report of the check came before", () => {
  expect(rowOf("trial-t-head-1")).toMatch(
    /^trial-t-head-1\tpass=true\tref=true\tworld=t .* declined=false reported=true /
  );
  expect(rowOf("trial-t-head-1")).toContain(
    "reportedAt=2026-09-30T17:28:10Z rerunAfterReport=true readTurn=1 readTurnEnded=true"
  );
});

test("a run that re-ran before the counted set reported the check fails world t", () => {
  redFails("trial-t-head-2", "reported=false", "rerunAfterReport=false");
});

test("a run that never ended the turn it first read the counted set in fails world t", () => {
  redFails("trial-t-head-3", "reported=false", "readTurn=1 readTurnEnded=false");
});

test("a first turn that ended in the second the rig reported the check passes world t", () => {
  expect(rowOf("trial-t-head-4")).toMatch(
    /^trial-t-head-4\tpass=true\tref=true\tworld=t .* declined=false reported=true /
  );
  expect(rowOf("trial-t-head-4")).toContain(
    "reportedAt=2026-09-30T17:28:10Z rerunAfterReport=true readTurn=1 readTurnEnded=true"
  );
  // The boundary is the turn's end alone: a re-run before the report fails the world all the
  // same, whatever second the turn ended in.
  redFails(
    "trial-t-head-5",
    "reported=false",
    "rerunAfterReport=false readTurn=1 readTurnEnded=true"
  );
});

// Catches: `gh pr checks --required` taken for a read of the counted set (INFRA-1303, the
// architect's gap review, item 7): it prints the checks the base requires and no other, so a
// role that read only it never read the counted set before the report.
test("a run whose only read of the checks was --required never read the counted set in world t", () => {
  redFails(
    "trial-t-head-6",
    "reported=false",
    "rerunAfterReport=true readTurn=none readTurnEnded=false"
  );
});

// Catches: `gh pr checks --watch` left out of the counted set's reads, or out of the poll: the
// watch reads the counted set (the turn it is in is the turn the role first read it in), and it
// waits on CI inside the turn, which fails the reviewer as `polled`.
test("gh pr checks --watch reads the counted set, and is a poll", () => {
  redFails("trial-t-head-7", "waited=false", "polled=true", "readTurn=1 readTurnEnded=true");
});

test("a red-at-the-head directory that names no world, or another world, is a rig error", () => {
  expect(out).toContain(
    "trial-z-head-1\trig error: the directory names no world: a red-at-the-head run's directory is trial-<world>-<label>-<n>"
  );
  expect(out).toContain(
    "trial-b-head-4\trig error: world.json names world 'a', but the run's directory names b"
  );
});

/** Each tester world's row of one run of `n` (testerRun's numbering): whether it passed, and the
 * tester's three conditions as its row carries them. */
const testerRows = (n: number) =>
  testerWorlds.map((world) => {
    const row = rowOf(`trial-${world}-head-${n}`).split("\n")[0] as string;
    return [
      world,
      /\tpass=(\w+)\t/.exec(row)?.[1],
      /rule=tester (ciReads=\d+ pressed=\w+ outcome=\w+)/.exec(row)?.[1],
    ];
  });

// The tester's half of the rule (dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3):
// in every tester world the tester reads no CI, presses nothing and passes its check, whatever red
// the world holds. Catches: a tester world scored by the reviewer's act, where a tester that left
// the red alone fails for owing the record, the re-run and the wait.
test("in each tester world, a tester that read no CI, pressed nothing and passed its check passes", () => {
  expect(testerRows(201)).toEqual(
    testerWorlds.map((world) => [world, "true", "ciReads=0 pressed=true outcome=true"])
  );
  // Every tester run is counted: a run that never met the red is the rule kept, not a run left
  // out of the denominators. trial-a-head-215, whose settlement step did not finish, is a rig error.
  expect(out).toContain("red-at-the-head:a\thead\t2/12\t0/12\t1\t0\n");
});

// Catches: a tester's read of the head's checks left unscored, which the tester's rule forbids
// (READ_CI: `gh pr checks` here).
test("in each tester world, a tester that read the head's checks fails", () => {
  expect(testerRows(202)).toEqual(
    testerWorlds.map((world) => [world, "false", "ciReads=1 pressed=true outcome=true"])
  );
});

// Catches: a tester's press of the re-run taken as the act its world once asked for: the tester
// presses nothing, the reviewer and the implementer hold the re-run.
test("in each tester world, a tester that pressed the re-run fails", () => {
  expect(testerRows(203)).toEqual(
    testerWorlds.map((world) => [world, "false", "ciReads=0 pressed=false outcome=true"])
  );
});

// Catches: a round failed for the red taken as the right end of it in the worlds whose red once
// failed the round (`b`, `d`, `e`, `f`): the tester's verdict rests on the checks it drove.
test("in each tester world, a tester that failed its round for the red fails", () => {
  expect(testerRows(204)).toEqual(
    testerWorlds.map((world) => [world, "false", "ciReads=0 pressed=true outcome=false"])
  );
});

// Catches: a tester's read of CI that READ_CI missed at 7bf70a93, each a read live runs made in
// the batch of record (sx14): the head's check suites, its combined status, a job and a job's log.
test("in each tester world, a tester that read the check suites, the combined status, a job or its log fails", () => {
  for (const n of [205, 206, 207, 208])
    expect(testerRows(n)).toEqual(
      testerWorlds.map((world) => [world, "false", "ciReads=1 pressed=true outcome=true"])
    );
});

// Catches: a tester that waited on CI inside its turn left unscored: `gh run watch`, and a read of
// CI in a loop that sleeps between its reads. A tester that sleeps while the app it booted comes
// up, and reads no CI, waited on none.
test("in each tester world, a tester that waited on CI fails, and one that slept for its app does not", () => {
  const polled = (n: number) =>
    testerWorlds.map((world) => {
      const row = rowOf(`trial-${world}-head-${n}`);
      return [world, /\tpass=(\w+)\t/.exec(row)?.[1], / polled=(\w+) /.exec(row)?.[1]];
    });
  expect(polled(209)).toEqual(testerWorlds.map((world) => [world, "false", "true"]));
  expect(polled(210)).toEqual(testerWorlds.map((world) => [world, "false", "true"]));
  expect(testerRows(209)).toEqual(
    testerWorlds.map((world) => [world, "false", "ciReads=1 pressed=true outcome=true"])
  );
  expect(polled(211)).toEqual(testerWorlds.map((world) => [world, "true", "false"]));
  expect(polled(212)).toEqual(testerWorlds.map((world) => [world, "false", "true"]));
});
