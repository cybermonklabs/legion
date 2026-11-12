import { beforeAll, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
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

beforeAll(() => {
  for (const label of ["head", "base", "cross", "on", "off"]) {
    mkdirSync(path.join(work, "profiles", label), { recursive: true });
    writeFileSync(path.join(work, "profiles", label, "checkout"), `/checkouts/${label}\n`);
  }
  // A staging may queue another (the pushed-code run below queues its scenario once the
  // repository is built), so the queue is drained until it is empty rather than in one pass.
  for (let stage = stagings.shift(); stage !== undefined; stage = stagings.shift()) stage();
  // Bun's default hook bound is 5s, and this one writes every run of the file and clones a bare
  // repository for each threads-closed run: it ran in about 4s before this round added three
  // runs, and on a loaded pod it then timed out with the clone killed and its stderr empty,
  // which reads as a git failure rather than as the hook's own bound. 120s is the whole file's
  // staging, not one run's.
}, 120_000);

/** The session id of a run's first agent, and of the half the rig boots after it: the score
 * reads who sent a message by the sender Envoy recorded against the session id of that half's
 * own transcript (score.ts's `ownSession`), so the two halves of a run are two sessions. */
const FIRST_SESSION = "01a0f2cc";
const SECOND_SESSION = "01b1f3dd";
/** Oh My Pi's transcript header entries: what a session writes before its first model request. */
const header = [
  { type: "session", id: FIRST_SESSION, cwd: "/rig/cwd" },
  { type: "model_change", model: "anthropic/claude-opus-4-8" },
  { type: "thinking_level_change", thinkingLevel: "high" },
];
/** The header of the half the rig boots second, whose transcript is its own
 * (`<run>/sessions/<role>`). */
const secondHeader = [{ type: "session", id: SECOND_SESSION, cwd: "/rig/cwd" }, ...header.slice(1)];
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
    // The agent's own working directory, where a path it writes and then reads back resolves
    // (rig.sh's worker_boot), so a fixture naming `cwd/<file>` puts the file where the agent
    // would have left it.
    mkdirSync(path.join(dir, "cwd"), { recursive: true });
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
/** A record holding its opening line and nothing else, which is what a run whose agents made no
 * call and no move leaves: the gh stand-in writes that line the first time it is invoked
 * (rig.sh's worker_boot invokes it for the line alone, before the agent starts) and the daemon
 * stand-in when it starts listening, so the file is there and says its writer was there, where
 * a record nothing ever wrote is absent. */
const openedRecord = (file: string) => calls([{ at: "2026-09-30T17:20:00.000Z", opened: file }]);
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
// Read the rig's own harness under the head label's checkout: rig.sh puts the stand-ins and the
// panes of the checkout it runs from on every label's PATH, so the `gh` a base run is given is a
// script naming that checkout's gh-standin.ts, and an agent that reads the gh it was given reads
// a file under the head label's checkout. The harness is the same for every label, so the run is
// scored and carries no flag.
run(
  "tester-proof-base-1",
  { "out.txt": "exit=0\n", "world.json": world, "handoffs.json": handoffs() },
  [
    ...header,
    turn[0],
    bash("cat /checkouts/head/packages/pi-legion/scripts/skill-scenarios/gh-standin.ts"),
  ]
);
// The other label's worker skill, under that same checkout: the text the labels compare, so the
// read is a rig error as it was.
run("tester-proof-base-2", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash("cat /checkouts/head/skills/legion-worker/references/review-threads.md"),
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

// reviewer-text-findings: the round's review submission, as the gh stand-in recorded it. The
// world's two findings are text, so text-only asks for an approval of the head that carries both
// as inline comments, and defect, the same change with a behavioural finding in it, asks for
// REQUEST_CHANGES.
const findings = [
  {
    path: "docs/cli.md",
    line: 7,
    side: "RIGHT",
    body: "The change trims the name, so this sentence is false.",
  },
  {
    path: ".github/workflows/greet.yml",
    line: 1,
    side: "RIGHT",
    body: "The workflow has no push trigger, so this comment names one it does not carry.",
  },
];
/** `gh api --method POST …/pulls/1001/reviews --input body.json`, the submission the reviewer's
 * prompt prescribes, with the body the stand-in read from the file. */
const submission = (review: object) => ({
  at: "2026-09-30T17:29:30.000Z",
  as: "gh",
  argv: [
    "api",
    "--method",
    "POST",
    "repos/example/widgets/pulls/1001/reviews",
    "--input",
    "body.json",
  ],
  exit: 0,
  files: { "body.json": JSON.stringify(review) },
});
const approval = (comments: object[] = findings, commit = head) => ({
  commit_id: commit,
  event: "APPROVE",
  body: "The open threads are the work the merge waits for.",
  comments,
});
/** The review record the round leaves on the issue, whatever it submitted. */
const reviewRecord = {
  id: "h-review",
  issue_key: "LWEVAL-1",
  phase: "review",
  version: 1,
  author: { kind: "session", id: "01a0f2cc" },
  created_at: "2026-09-30T17:29:11.031234Z",
  body: JSON.stringify({
    schemaVersion: 1,
    phase: "review",
    issue: "LWEVAL-1",
    headReviewed: head,
    decision: "approved",
  }),
};
/** One message of `<run>/published.jsonl`: a message Envoy accepted on a role topic of this run,
 * which record-publishes.ts appends from the bus, with the session Envoy recorded as its sender.
 * The score reads what reached a topic and who sent it, and never what an agent's own code says
 * it would send. */
const sentTo = (
  role: string,
  text: string,
  at = "2026-09-30T17:42:00.000Z",
  sender = FIRST_SESSION
) =>
  JSON.stringify({
    at,
    topic: `notifications.role.legion-skills1-lweval-1-${role}`,
    message: text,
    sender,
  });
/** The recorder's own two lines: it subscribed, and the rig stopped it. A record without both is
 * a rig error, since what the recorder missed is unknown. */
const recordingLine = (at = "2026-09-30T17:20:00.000Z") =>
  JSON.stringify({ at, recording: "legion-skills1-lweval-1-" });
const closedLine = (at = "2026-09-30T17:50:00.000Z") => JSON.stringify({ at, closed: "stopped" });
/** A whole record: the recorder's opening line, the run's messages, its closing line. */
const bus = (...lines: string[]) =>
  [recordingLine(), ...lines, closedLine()].map((line) => `${line}\n`).join("");
/** A record exactly as given, for the runs whose record is not whole. */
const busLines = (...lines: string[]) => lines.map((line) => `${line}\n`).join("");
/** A publish an agent's own transcript records: the `envoy_publish` write a session makes. What
 * the agent called carries no weight of its own — a call can be refused, a cell can build a
 * message it never sends — so a fixture that holds this and no line of the record is a run that
 * published nothing. */
const publishCall = (role: string, text: string, at = "2026-09-30T17:42:00.000Z") => ({
  type: "message",
  timestamp: at,
  message: {
    role: "assistant",
    content: [
      {
        type: "toolCall",
        id: "call-publish",
        name: "write",
        arguments: {
          path: "xd://envoy_publish",
          content: JSON.stringify({
            topic: `notifications.role.legion-skills1-lweval-1-${role}`,
            message: text,
          }),
        },
      },
    ],
  },
});
/** The reviewer's own read of the record, which the rule has it make before it sends: the
 * `legion` tool's `read_record`, answered with the issue in `accepting` (review-threads.md step
 * 1). The score reads this call and its own answer, since every other caller of
 * `/legion/v1/state` — `handoff_complete`, the phase reminder — reads the same route and says
 * nothing about the reviewer having read the record. The answer carries its own time
 * (`answeredAt`), which the tool writes when it returned and which is what orders the publish;
 * a real read answers in the same millisecond, which is what the fixture gives both by default. */
const readRecord = (at = "2026-09-30T17:40:30.000Z", phase = "accepting", answeredAt = at) => [
  {
    type: "message",
    timestamp: at,
    message: {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: `call-read-record-${at}`,
          name: "legion",
          arguments: { op: "read_record", issue: "LWEVAL-1" },
        },
      ],
    },
  },
  {
    type: "message",
    timestamp: answeredAt,
    message: {
      role: "toolResult",
      toolCallId: `call-read-record-${at}`,
      content: [{ type: "text", text: `the record of LWEVAL-1, in ${phase}` }],
      // The record as the tool returns it: a typed field of the result, which is what the score
      // reads, rather than the words of the text beside it.
      details: { record: { key: "LWEVAL-1", phase, status: "needs_review" } },
    },
  },
];
/** The same read made through a `write` to `xd://legion`, whose result carries the tool's own
 * answer one level down, and through an `eval` cell, whose result carries whatever it printed.
 * Which tool carried the call is no part of the rule, so the score reads the record from the
 * answer on each route through one schema. `answeredAt` is when the tool returned, the call's
 * own time by default: a cell that does more than read answers later than it was called, and
 * what the cell did in between falls inside that span. */
const readRecordVia = (
  route: "xd" | "eval" | "eval-unreadable",
  at = "2026-09-30T17:40:30.000Z",
  answeredAt = at
) => {
  const record = { key: "LWEVAL-1", phase: "accepting", status: "needs_review" };
  const id = `call-read-${route}`;
  // One entry per route: the call the session made and the answer the tool returned, so a new
  // route is one entry rather than a branch in two chains.
  const routes = {
    xd: {
      call: {
        name: "write",
        arguments: { path: "xd://legion", content: JSON.stringify({ op: "read_record" }) },
      },
      result: { text: "wrote xd://legion", details: { xdev: { inner: { record } } } },
    },
    eval: {
      call: {
        name: "eval",
        arguments: {
          language: "py",
          code: 'print(tool.legion({"op": "read_record", "issue": "LWEVAL-1"}))',
        },
      },
      result: {
        text: `the cell printed ${JSON.stringify({ record })}`,
        details: { cells: [{ language: "py" }], language: "py" },
      },
    },
    "eval-unreadable": {
      call: {
        name: "eval",
        arguments: {
          language: "py",
          code: 'print(tool.legion({"op": "read_record", "issue": "LWEVAL-1"}))',
        },
      },
      result: {
        text: "the cell printed <Record object at 0x7f>",
        details: { cells: [{ language: "py" }], language: "py" },
      },
    },
  } as const;
  const { call, result } = routes[route];
  return [
    {
      type: "message",
      timestamp: at,
      message: {
        role: "assistant",
        content: [{ type: "toolCall", id, name: call.name, arguments: call.arguments }],
      },
    },
    {
      type: "message",
      timestamp: answeredAt,
      message: {
        role: "toolResult",
        toolCallId: id,
        content: [{ type: "text", text: result.text }],
        details: result.details,
      },
    },
  ];
};
/** A tool call and the result it got, each at its own time: the shape the orderings of this
 * scenario are read in, since the score reads a read's time from the answer the tool returned
 * and a result with no time of its own is ordered before everything. `failed` is a call the
 * tool refused, which is no answer of its own whatever its text holds. */
const stamped = (
  id: string,
  tool: string,
  args: object,
  at: string,
  answer: { text: string; details?: object; failed?: boolean; answeredAt?: string }
) => [
  {
    type: "message",
    timestamp: at,
    message: {
      role: "assistant",
      content: [{ type: "toolCall", id, name: tool, arguments: args }],
    },
  },
  {
    type: "message",
    timestamp: answer.answeredAt ?? at,
    message: {
      role: "toolResult",
      toolCallId: id,
      isError: answer.failed === true,
      content: [{ type: "text", text: answer.text }],
      details: answer.details ?? {},
    },
  },
];
/** The reviewer's wake of the implementer, which an approval that leaves text threads open owns:
 * the daemon starts no phase for the fix, so the implementer learns its turn began from this
 * message and from nothing else. The message names the head the fix lands above and each finding
 * it must close, which are the two things the reviewer can name in this world: the stand-in's
 * `reviewThreads` route serves the bot's thread alone, so the threads this round wrote have no
 * node id to give (review-threads.md step 1). */
const toldImplementer = (commit: string, at?: string) =>
  sentTo(
    "implementer",
    `Approved ${commit}: docs/cli.md and .github/workflows/greet.yml are yours to close above it.`,
    at
  );
/** The daemon line a clean round leaves: the completion the stand-in accepted. No term of
 * `reviewerTextFindings` reads this file — the verdict is in the calls, the ordering in the
 * reviewer's own `read_record` and its answer in the transcript — and the run writes it because
 * `unscored` makes a run with no daemon.log a rig error (head-53 below is that run). So the
 * line is what the rig would have logged and is scored nowhere. */
const reviewDaemon = [
  {
    at: "2026-09-30T17:40:00.000Z",
    path: "/legion/v1/handoff/complete",
    status: 200,
    grantId: "g-1",
  },
];
function reviewRun(
  name: string,
  files: Record<string, string | null>,
  transcript: object[] = [...header, ...turn, ...readRecord()]
) {
  run(
    name,
    {
      "out.txt": "exit=0\n",
      "world.json": world,
      "pushes.log": "",
      "handoffs.json": handoffs(reviewRecord),
      "daemon.log": calls(reviewDaemon),
      "published.jsonl": bus(toldImplementer(head)),
      ...files,
    },
    transcript
  );
}
// The submission, and after it the read of the reviews already on the pull request, which a round
// often makes and which submits nothing.
// Catches: a scorer that reads only whether the wake was sent and not when. The rule sends it
// once `read_record` shows the issue in `accepting` (review-threads.md step 1), because the round
// ends on both halves and a push in the gap between an accepted completion and the review's
// webhook voids the approval. head-30 publishes the right message to the right topic with its
// review, before the completion the stand-in accepted, and fails.
reviewRun("reviewer-text-findings-text-only-head-30", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:39:00.000Z")),
});
// head-35: the reviewer published the wake after its completion was accepted, and never called
// `read_record` at all. The stand-in answers `accepting` to every state read after that
// completion, and `handoff_complete` and the phase reminder make those reads themselves, so a
// scorer that took any 200 serving `accepting` would pass a reviewer that never read the record
// — which is the step the rule names (review-threads.md step 1).
reviewRun(
  "reviewer-text-findings-text-only-head-35",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [...header, ...turn]
);
// head-37: the reviewer published the wake and read the record after it. The rule's order is the
// other way round — the record first, because the round ends on both halves and a push between an
// accepted completion and the review's webhook voids the approval — so a run that read it late
// did not take the step, however many reads it made.
reviewRun(
  "reviewer-text-findings-text-only-head-37",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:41:00.000Z")),
  },
  [...header, ...turn, ...readRecord("2026-09-30T17:43:00.000Z")]
);
// The same read on the other two routes a session can reach the tool by, and one whose answer
// this cannot read at all. Which tool carried the call is no part of the rule — head-38 writes to
// `xd://legion`, whose result carries the tool's answer as `details.xdev.inner`, and head-39 runs
// an `eval` cell, whose result carries what it printed — so both pass. head-40's cell printed no
// record this can parse, which is the reader's limit and a rig error, never a round that read no
// record.
reviewRun(
  "reviewer-text-findings-text-only-head-38",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [...header, ...turn, ...readRecordVia("xd")]
);
reviewRun(
  "reviewer-text-findings-text-only-head-39",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [...header, ...turn, ...readRecordVia("eval")]
);
reviewRun(
  "reviewer-text-findings-text-only-head-40",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [...header, ...turn, ...readRecordVia("eval-unreadable")]
);
/** An `eval` cell that publishes and reads no record: the call is written when the session made
 * it and the result when the cell returned, so a message the cell published lands between the
 * two, where Oh My Pi puts every message a cell sends. */
const publishingCell = (at: string, answeredAt: string) =>
  stamped(
    `call-publish-cell-${at}`,
    "eval",
    {
      language: "py",
      code: 'print(tool.envoy_publish({"topic": "notifications.role.legion-skills1-lweval-1-implementer", "message": "Approved."}))',
    },
    at,
    {
      text: "the cell printed {'published': True}",
      details: { cells: [{ language: "py" }], language: "py" },
      answeredAt,
    }
  );
// head-117: the round head-39 passes on, made in one cell: the reviewer reads `accepting` and
// publishes the wake inside the same `eval`, so the message reached the topic at 17:40:35 while
// the cell's own result was written at 17:40:40. The read came first — a cell cannot publish
// what it has not yet run — and the step was taken, so the round passes. head-118 and head-119
// are the orderings that still fail, each from a cell of its own: one that published with no
// accepting read behind it at all, and one that published before the cell that took the read it
// would have had to follow.
reviewRun(
  "reviewer-text-findings-text-only-head-117",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:40:35.000Z")),
  },
  [
    ...header,
    ...turn,
    ...readRecordVia("eval", "2026-09-30T17:40:30.000Z", "2026-09-30T17:40:40.000Z"),
  ]
);
reviewRun(
  "reviewer-text-findings-text-only-head-118",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:40:35.000Z")),
  },
  [...header, ...turn, ...publishingCell("2026-09-30T17:40:30.000Z", "2026-09-30T17:40:40.000Z")]
);
reviewRun(
  "reviewer-text-findings-text-only-head-119",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:40:35.000Z")),
  },
  [
    ...header,
    ...turn,
    ...publishingCell("2026-09-30T17:40:30.000Z", "2026-09-30T17:40:40.000Z"),
    ...readRecordVia("eval", "2026-09-30T17:41:00.000Z", "2026-09-30T17:41:10.000Z"),
  ]
);
// head-42: the reviewer completed, read `accepting`, then submitted its approval and published.
// The read cannot have shown the round ending — the stand-in moves to `accepting` on the
// completion alone, while the daemon needs the review too — so the step is not shown however
// late the publish is. The deep pass of 2026-11-10 built this order out of a recorded run and it
// passed on the read alone.
reviewRun(
  "reviewer-text-findings-text-only-head-42",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([{ ...submission(approval()), at: "2026-09-30T17:41:00.000Z" }]),
  },
  [...header, ...turn, ...readRecord("2026-09-30T17:40:30.000Z")]
);
// head-43: an earlier read answered `reviewing`, and the only read after the approval holds no
// record this can parse. What the reviewer was shown is unknown, so the run is a rig error; a
// reader that asked whether *any* read was readable would score it a failure, which is the
// outcome the rig error exists to rule out.
reviewRun(
  "reviewer-text-findings-text-only-head-43",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [
    ...header,
    ...turn,
    ...readRecord("2026-09-30T17:29:00.000Z", "reviewing"),
    ...readRecordVia("eval-unreadable", "2026-09-30T17:40:30.000Z"),
  ]
);
// head-44: a `handoff_complete` whose summary mentions `read_record`, beside a native read that
// answered `reviewing`. The op is what makes a call a read of the record, never the word, so the
// only read of this run answered `reviewing` and the run fails on the step. A reader that matched
// the word would take the summary for a read whose answer holds no record and make the whole run
// a rig error, which is what the deep pass of 2026-11-10 measured; this fixture is the pin, so the
// row must read `pass=false` and not disappear from the counts.
reviewRun(
  "reviewer-text-findings-text-only-head-44",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [
    ...header,
    ...turn,
    {
      type: "message",
      timestamp: "2026-09-30T17:40:00.000Z",
      message: {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "call-complete",
            name: "legion",
            arguments: {
              op: "handoff_complete",
              summary: "Approved the head. Will read_record before waking anyone.",
            },
          },
        ],
      },
    },
    {
      type: "message",
      timestamp: "2026-09-30T17:40:01.000Z",
      message: {
        role: "toolResult",
        toolCallId: "call-complete",
        content: [{ type: "text", text: "{}" }],
        details: {},
      },
    },
    ...readRecord("2026-09-30T17:40:30.000Z", "reviewing"),
  ]
);
// head-45: a cell and an `xd://legion` write that each mention `read_record` without naming the
// op, the write answering with a record. Neither is a read: the op decides on every route, and
// the write's answer carries a record only because this fixture put one there. So the run has no
// read at all and fails on the step. A reader that matched the word would take the write for the
// accepting read and pass the run.
reviewRun(
  "reviewer-text-findings-text-only-head-45",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [
    ...header,
    ...turn,
    {
      type: "message",
      timestamp: "2026-09-30T17:40:00.000Z",
      message: {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "call-word-eval",
            name: "eval",
            arguments: {
              language: "py",
              code: 'print("the read_record route is the reviewer own")',
            },
          },
        ],
      },
    },
    {
      type: "message",
      timestamp: "2026-09-30T17:40:01.000Z",
      message: {
        role: "toolResult",
        toolCallId: "call-word-eval",
        content: [{ type: "text", text: "the read_record route is the reviewer's own" }],
        details: { cells: [{ language: "py" }], language: "py" },
      },
    },
    {
      type: "message",
      timestamp: "2026-09-30T17:40:10.000Z",
      message: {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "call-word-write",
            name: "write",
            arguments: {
              path: "xd://legion",
              content: JSON.stringify({
                op: "handoff_complete",
                summary: "Approved; read_record comes next.",
              }),
            },
          },
        ],
      },
    },
    {
      type: "message",
      timestamp: "2026-09-30T17:40:11.000Z",
      message: {
        role: "toolResult",
        toolCallId: "call-word-write",
        content: [{ type: "text", text: "wrote xd://legion" }],
        details: {
          xdev: { inner: { record: { key: "LWEVAL-1", phase: "accepting" } } },
        },
      },
    },
  ]
);
// head-46: the same unreadable read as head-40, in a round that also pushed. The run failed on
// evidence this reader did read — a review round pushes nothing — so it stays a failure: a rig
// error says the verdict is unknown, and this verdict is known. Without the condition the throw
// would take a failing run out of both counts.
reviewRun(
  "reviewer-text-findings-text-only-head-46",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "pushes.log": `2026-09-30T17:29:25.000Z refs/heads/legion/LWEVAL-1 ${"0".repeat(40)} ${head}\n`,
  },
  [...header, ...turn, ...readRecordVia("eval-unreadable")]
);
// head-48: the same unreadable read as head-40, in a round that woke nobody. The step is the
// record read and then the message, so a round that published no wake fails whatever that read
// answered: the read could have decided nothing, and a rig error here would take a decided
// failure out of both counts (A1 of the 2026-11-10 audit).
reviewRun(
  "reviewer-text-findings-text-only-head-48",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(),
  },
  [...header, ...turn, ...readRecordVia("eval-unreadable")]
);
// head-47: two approvals, the read between them. The verdict is the submission the round ended
// with, so the read came before it and shows nothing of the round ending; a reader taking the
// round's first APPROVE would find the read after it and pass the run. Every other fixture
// submits once, which is why the revert the deep pass made of that line failed nothing.
reviewRun(
  "reviewer-text-findings-text-only-head-47",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([
      submission(approval()),
      { ...submission(approval()), at: "2026-09-30T17:45:00.000Z" },
    ]),
  },
  [...header, ...turn, ...readRecord()]
);
// head-41: the reviewer completed, read `accepting` and published, and only then submitted its
// approval. The stand-in moves to `accepting` on any accepted completion of `reviewing`, while
// the daemon needs the review as well, so the read alone cannot see that window — and that window
// is the one the rule exists to close. The passing order is every other text-only head run:
// submission, then the read, then the publish.
reviewRun("reviewer-text-findings-text-only-head-41", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([{ ...submission(approval()), at: "2026-09-30T17:45:00.000Z" }]),
});
// head-36: the stand-in answered the read 200 and the agent's own plugin refused the body, which
// is what a label whose contract requires a key the stand-in stopped serving reads on every state
// read (`daemon-standin.ts`'s `settings` line tells that measurement). An agent whose
// `read_record` is refused cannot take the step the rule names, so the run is a rig error naming
// the refused read rather than a row whose wake was never sent.
reviewRun(
  "reviewer-text-findings-text-only-head-36",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
  },
  [
    ...header,
    ...turn,
    {
      type: "message",
      timestamp: "2026-09-30T17:40:30.000Z",
      message: {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "call-read-refused",
            name: "legion",
            arguments: { op: "read_record", issue: "LWEVAL-1" },
          },
        ],
      },
    },
    {
      type: "message",
      timestamp: "2026-09-30T17:40:31.000Z",
      message: {
        role: "toolResult",
        toolCallId: "call-read-refused",
        isError: true,
        content: [
          {
            type: "text",
            text: "GET /legion/v1/state answered 200 with a body the daemon API contract does not describe: settings.max_fix_attempts: Invalid input: expected number, received undefined",
          },
        ],
      },
    },
  ]
);
reviewRun("reviewer-text-findings-text-only-head-1", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(approval()),
    {
      at: "2026-09-30T17:29:40.000Z",
      as: "gh",
      argv: ["api", "repos/example/widgets/pulls/1001/reviews"],
      exit: 0,
    },
  ]),
});
// Only one of the two findings carries an inline comment.
reviewRun("reviewer-text-findings-text-only-head-2", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval(findings.slice(0, 1)))]),
});
// A text finding sent back as REQUEST_CHANGES: the rule's own failure.
reviewRun("reviewer-text-findings-text-only-head-3", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission({ ...approval(), event: "REQUEST_CHANGES", decision: "changes_requested" }),
  ]),
});
// The approval the rule asks for, with the bot thread the round adjudicated resolved: one call,
// one thread, which review-threads.md has the reviewer make and the score reports.
reviewRun("reviewer-text-findings-text-only-head-4", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:20.000Z",
      as: "gh",
      argv: [
        "api",
        "graphql",
        "-f",
        'query=mutation{resolveReviewThread(input:{threadId:"PRRT_kwDOrig0001"}){thread{id}}}',
      ],
      exit: 1,
    },
    submission(approval()),
  ]),
});
// Another head than the one the fixture serves: the approval names a commit this run has not.
reviewRun("reviewer-text-findings-text-only-head-5", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval(findings, "7".repeat(40)))]),
});
// `gh pr review --approve`: the event reads, and the call can carry neither head nor comment.
reviewRun("reviewer-text-findings-text-only-head-6", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: ["pr", "review", "1001", "--approve", "--body", "Looks good."],
      exit: 0,
    },
  ]),
});
// head-50: the same submission posted to another repository's pull request. The stand-in matches
// its routes on the pull request's number alone, so it answered this call as GitHub would answer
// the right one, but a review of sjawhar/legion-smoke approves nothing of the world's own
// example/widgets, and a reader that took it counted a verdict the pull request never got —
// recorded runs submitted to a repository their world has not got, some of them to this one
// (rig.sh's worker world comment holds that story).
reviewRun("reviewer-text-findings-text-only-head-50", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...submission(approval()),
      argv: [
        "api",
        "--method",
        "POST",
        "repos/sjawhar/legion-smoke/pulls/1001/reviews",
        "--input",
        "body.json",
      ],
    },
  ]),
});
// A review round pushes nothing at all.
reviewRun("reviewer-text-findings-text-only-head-7", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "pushes.log": `2026-09-30T17:29:25.000Z refs/heads/legion/LWEVAL-1 ${"0".repeat(40)} ${head}\n`,
});
// The same resolution sent as the body of `gh api graphql --input`, which the call's arguments
// never name.
reviewRun("reviewer-text-findings-text-only-head-8", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:20.000Z",
      as: "gh",
      argv: ["api", "graphql", "--input", "resolve.json"],
      exit: 1,
      files: {
        "resolve.json": JSON.stringify({
          query: 'mutation{resolveReviewThread(input:{threadId:"PRRT_kwDOrig0001"}){thread{id}}}',
        }),
      },
    },
    submission(approval()),
  ]),
});
// The approval the rule asks for, and no message to the implementer: the round left the two text
// threads open and woke nobody to close them.
reviewRun("reviewer-text-findings-text-only-head-10", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(),
});
// The approval the rule asks for, and the wake the implementer needs sent to the architect's
// topic instead. Catches: a reader of the wake that takes any message of the run for it. The
// message is the one `toldImplementer` writes, word for word — the head and both findings' paths
// — so the topic is the one thing wrong with this round, and the implementer, which reads its
// own topic alone, learns nothing. Until this round the message named neither path, so the run
// failed on the names and the topic rule it was written for was pinned by nothing.
reviewRun("reviewer-text-findings-text-only-head-11", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo(
      "architect",
      `Approved ${head}: docs/cli.md and .github/workflows/greet.yml are yours to close above it.`
    )
  ),
});
// head-49: the approval the rule asks for, and a wake that names the head alone. Step 1 has the
// reviewer name the approved head and each open thread; the threads of its own submission have no
// node id in this world, so their paths are what it can name, and a message naming neither leaves
// the implementer the whole diff to search for the work the merge waits for.
reviewRun("reviewer-text-findings-text-only-head-49", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo("implementer", `Approved ${head}: the two text threads are yours to close above it.`)
  ),
});
// head-51, head-52 and head-53: the round that passes as head-1 does, each missing one of the
// three records a worker run leaves. Nothing in the rig creates them: each is written by the
// thing that keeps it, so an absent one is a record nothing ever wrote. It reads as an empty
// one (`lines`), which says no call, no push and no move — all three what a passing run shows
// — so each run is a rig error naming the file it has not got, and head-52, whose only fault is
// a push log nothing wrote, would otherwise score a pass.
for (const [name, file] of [
  ["head-51", "calls.jsonl"],
  ["head-52", "pushes.log"],
  ["head-53", "daemon.log"],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    [file]: null,
  });
// head-54: the three records there and holding no line of the run's own, which is the other
// reading — it made no call, pushed nothing and moved nothing — so it scores, its row names
// each of them, and it fails for having submitted no review. This is the state a run leaves:
// each stand-in opens its record with one line of its own, which `Call` and `DaemonLine` pass
// over, and the push log is the one rig.sh empties. Every other review run here holds an empty
// pushes.log, which is what a review round leaves, and names it the same way.
reviewRun("reviewer-text-findings-text-only-head-54", {
  "case.txt": "text-only\n",
  "calls.jsonl": openedRecord("calls.jsonl"),
  "pushes.log": "",
  "daemon.log": openedRecord("daemon.log"),
});
// head-55: an approval whose one inline comment is on the workflow, with the document the change
// makes false left uncommented. head-2 is this run's mirror — the document commented, the
// workflow left out — and a reader that asked for one finding rather than both would pass
// whichever of the two it looked at.
reviewRun("reviewer-text-findings-text-only-head-55", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval(findings.slice(1)))]),
});
// head-56: the recorder subscribed and the rig's stop never reached it, so the record has no
// closing line and what it missed is unknown. threads-closed's head-27 is that gap in the other
// recorded scenario; a reader that held only that scenario to a whole record would read this
// round's wake off a record that may have dropped it.
reviewRun("reviewer-text-findings-text-only-head-56", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": busLines(recordingLine(), toldImplementer(head)),
});
// head-57: a transcript with no session header, which Oh My Pi writes before its first model
// request. Who sent each message of the record is read by the session id Envoy recorded against
// it, so a run naming none can have no message read as its agent's and is a rig error. A reader
// that returned a blank in its place would find no message of this record sent by "" and score
// the round as one that woke the implementer not at all.
reviewRun(
  "reviewer-text-findings-text-only-head-57",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [...turn, ...readRecord()]
);
// head-58: an accepting read after the approval and an unreadable one beside it, both before the
// wake. One read after the approval answered `accepting`, so the round took the step and the run
// is decided; the unreadable read decides nothing. A reader that made a rig error of any
// unreadable read after the approval would take this passing round out of both counts.
reviewRun(
  "reviewer-text-findings-text-only-head-58",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...readRecord(),
    ...readRecordVia("eval-unreadable", "2026-09-30T17:40:40.000Z"),
  ]
);
// head-59 and head-60: a `read_record` the tool refused for want of the issue, which is the
// refusal the comment on `reads` names. A refused call is no answer of its own, so head-59, which
// made no other read, took no step and fails on it: a reader counting the refusal would find an
// answer holding no record and make a decided failure a rig error. head-60 is the call the agent
// then made again with the issue, and it passes on that second read; its row reads the same
// whether or not the refusal is counted, and it is here because a reader that made any refused
// read a rig error of its own — as a refused daemon answer is — would take it out of the counts.
/** The `read_record` the tool refused for want of the issue: a call of the round, with no answer
 * of its own. */
const refusedRead = (run: string, at: string) =>
  stamped(`call-read-refused-${run}`, "legion", { op: "read_record" }, at, {
    text: "legion read_record was not run: 1 problem\n- issue is required",
    failed: true,
  });
reviewRun(
  "reviewer-text-findings-text-only-head-59",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [...header, ...turn, ...refusedRead("59", "2026-09-30T17:40:30.000Z")]
);
reviewRun(
  "reviewer-text-findings-text-only-head-60",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...refusedRead("60", "2026-09-30T17:40:20.000Z"),
    ...readRecord("2026-09-30T17:40:30.000Z"),
  ]
);
// head-61: a native `legion` read whose call went out before the wake and whose answer came back
// after it. Nothing of the session runs between that call and its answer — the tool suspends it
// — so a message sent in the gap is not one this read ordered: the publish is ordered against
// the answer's own time, which is when the tool returned and when the reviewer could act on what
// it said, so this wake was sent before the record was read and the run fails. A reader ordering
// every route by the call's own time — the assistant message's, written when the model asked —
// would pass it; head-117 below is the one route where that time is the right one, a cell, whose
// code is the reviewer's own and runs inside that gap. Every other fixture here answers in the
// call's own millisecond, as a real read does.
reviewRun(
  "reviewer-text-findings-text-only-head-61",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...readRecord("2026-09-30T17:40:30.000Z", "accepting", "2026-09-30T17:43:00.000Z"),
  ]
);
// head-62: the read and the approval at one time, which the stand-in and the transcript both
// record to the millisecond. A read answered in the submission's own millisecond was not taken
// after it, and the step is the read after the verdict (review-threads.md step 1); a reader
// comparing `>=` would score this round as having read the record its own submission had not yet
// moved.
reviewRun(
  "reviewer-text-findings-text-only-head-62",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [...header, ...turn, ...readRecord("2026-09-30T17:29:30.000Z")]
);
// head-63: two calls that name the record and are no read of it — a `write` of a `read_record`
// body to a file of the run's own, which is no `xd://legion` call, and a `bash` call whose
// output is a record the agent read out of a file. The tool decides both whether the call asked
// for the record and where its answer lives, so the round made no read at all and fails on the
// step: a reader that took any `write` carrying the op would find an answer with no record in it
// and make the run a rig error, and one that read a record out of any tool's result would pass
// the run on the `cat`.
reviewRun(
  "reviewer-text-findings-text-only-head-63",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...stamped(
      "call-write-body",
      "write",
      {
        path: "LWEVAL-1-read-record.json",
        content: JSON.stringify({ op: "read_record", issue: "LWEVAL-1" }),
      },
      "2026-09-30T17:40:00.000Z",
      { text: "wrote LWEVAL-1-read-record.json" }
    ),
    ...stamped(
      "call-cat-record",
      "bash",
      { command: "cat LWEVAL-1-record.json" },
      "2026-09-30T17:40:10.000Z",
      { text: JSON.stringify({ record: { key: "LWEVAL-1", phase: "accepting" } }) }
    ),
  ]
);
// head-64 and head-65: the same route with the body on its other two carriers. head-64 pipes the
// body in (`--input -`), which the stand-in records as the call's stdin, and head-65 writes the
// verdict and the head as fields, which is every part of a review `-f` can carry — an inline
// comment is an array of objects, which is why the prompt prescribes a body file. So head-64
// passes as head-1 does and head-65 fails for its two uncommented findings, and a reader of
// `--input <file>` alone would score either as a round that submitted nothing.
reviewRun("reviewer-text-findings-text-only-head-64", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: ["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews", "--input", "-"],
      exit: 0,
      stdin: JSON.stringify(approval()),
    },
  ]),
});
reviewRun("reviewer-text-findings-text-only-head-65", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews",
        "-f",
        "event=APPROVE",
        "-f",
        `commit_id=${head}`,
        "-f",
        "body=The open threads are the work the merge waits for.",
      ],
      exit: 0,
    },
  ]),
});
// head-66: the approval, and after it a comment posted under the review it created. The
// submission's route ends at `…/reviews`: `…/reviews/<id>/comments` is a comment on a review and
// no verdict of its own, and a reader without the route's closing anchor would read that body as
// the round's last submission — a review with no event, no head and no inline comment on it.
reviewRun("reviewer-text-findings-text-only-head-66", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(approval()),
    {
      at: "2026-09-30T17:29:40.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/comments",
        "--input",
        "comment.json",
      ],
      exit: 0,
      files: {
        "comment.json": JSON.stringify({
          path: "docs/cli.md",
          line: 9,
          body: "And the example under it still names the old flag.",
        }),
      },
    },
  ]),
});
// head-67 and head-68: the two-step route GitHub documents beside the one-step POST — a create
// whose body names no event, which leaves a pending review, and a POST to that review's
// `/events` carrying the verdict. The rig answers both: the create from the world's own route,
// and the submit from the review's own state, which is the gh stand-in's answer and no world's
// (gh-standin.ts's `reviewSubmission`), so a round can take that route and be told nothing.
// The two calls are one submission, the create's head and inline comments under the submit
// call's verdict, so head-67 passes as head-1 does; head-68 is the create left unsubmitted,
// which is a pending review and no verdict, and it fails on the empty event it carries. A reader
// of the create alone scored head-67 that same empty event, which is the row of a round that
// submitted nothing — the one thing head-67 did not do.
const pendingReview = {
  at: "2026-09-30T17:29:20.000Z",
  as: "gh",
  argv: [
    "api",
    "--method",
    "POST",
    "repos/example/widgets/pulls/1001/reviews",
    "--input",
    "pending.json",
  ],
  exit: 0,
  files: {
    "pending.json": JSON.stringify({
      commit_id: head,
      body: "The open threads are the work the merge waits for.",
      comments: findings,
    }),
  },
};
reviewRun("reviewer-text-findings-text-only-head-67", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    pendingReview,
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/events",
        "-f",
        "event=APPROVE",
      ],
      exit: 0,
    },
  ]),
});
reviewRun("reviewer-text-findings-text-only-head-68", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([pendingReview]),
});
// head-69: the approval GitHub took, and the retry of it gh refused — the stand-in records each
// call's exit, and GitHub answers 422 to a second review on one already submitted. Only a call
// that exited 0 submitted anything: a reader counting the refused one would report two
// submissions where the pull request got one, and would have read the round's verdict off a call
// that never landed had the retry carried another verdict.
reviewRun("reviewer-text-findings-text-only-head-69", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(approval()),
    { ...submission(approval()), at: "2026-09-30T17:29:50.000Z", exit: 1 },
  ]),
});
// head-70: an approval naming the head in six hexadecimal digits. Seven is the fewest that names
// one commit, which is what a review's `commit_id` and a merge gate reading it need, so this
// submission names no head and the round fails. A reader taking any prefix the head starts with
// would pass it, and would read a submission carrying "" as a review of the head as well.
reviewRun("reviewer-text-findings-text-only-head-70", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval(findings, head.slice(0, 6)))]),
});
// head-71: two accepting reads after the approval, with the wake between them. The step is the
// first read that showed the issue in `accepting` and the message after it, which this round
// sent in that order and passes on. A reader taking the last such read would find the wake
// before it and score a round that did the step in order as one that woke the implementer early.
reviewRun(
  "reviewer-text-findings-text-only-head-71",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [...header, ...turn, ...readRecord(), ...readRecord("2026-09-30T17:43:00.000Z")]
);
// head-72: an out.txt the rig never finished — the agent's output with no `exit=` line after it,
// which is what a run killed mid-turn leaves. What that run would have gone on to do is unknown,
// so it is a rig error; a reader without that line would score every killed run on half a record,
// and this round's half says it approved the head.
reviewRun("reviewer-text-findings-text-only-head-72", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "out.txt": "Submitted the approval at 099b05dc.\n",
});
// Another label, for the counts.
reviewRun("reviewer-text-findings-text-only-base-1", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
});
// defect: the behavioural finding stands, so the round sends the change back, here with
// `gh pr review --request-changes`. A round that sends the change back opens no fix loop, so its
// transcript carries no message to the implementer.
reviewRun(
  "reviewer-text-findings-defect-head-1",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: [
          "pr",
          "review",
          "1001",
          "--request-changes",
          "--body",
          "The blank-name guard is gone.",
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
reviewRun(
  "reviewer-text-findings-defect-head-2",
  { "case.txt": "defect\n", "calls.jsonl": calls([submission(approval())]) },
  [...header, ...turn]
);
// head-4 and head-5: the two verdicts that are neither the change request the case asks for nor
// the approval it fails — a round that submitted no review at all, which is the shape the
// recorded defect-base-2 has, and one that left the behavioural finding as a COMMENT, which ends
// the round with no verdict the merge gate reads. The case turns on the one verdict: a reader
// asking only that the round did not approve would pass both, and would score a reviewer that
// submitted nothing as one that caught the defect.
reviewRun("reviewer-text-findings-defect-head-4", { "case.txt": "defect\n", "calls.jsonl": "" }, [
  ...header,
  ...turn,
]);
reviewRun(
  "reviewer-text-findings-defect-head-5",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([submission({ ...approval(), event: "COMMENT" })]),
  },
  [...header, ...turn]
);
// head-6: the change request the case asks for, a read after it whose answer holds no record
// this can parse, and the message to the implementer such a round may still send. No approval
// means no loop and no read to order it by, so the run is decided by its submission alone and
// passes. A reader that took whatever the round submitted for the approval would find an
// unreadable read after it and a wake after that, and would take the negative control's own pass
// out of both counts as a rig error.
reviewRun(
  "reviewer-text-findings-defect-head-6",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([submission({ ...approval(), event: "REQUEST_CHANGES" })]),
  },
  [...header, ...turn, ...readRecordVia("eval-unreadable")]
);
// head-7: the change request the case asks for, and a `COMMENT` review after it — a second
// reading posted as a comment, which on GitHub leaves the change request standing. The verdict
// is the round's last `APPROVE` or `REQUEST_CHANGES`, so the control passes; a reader taking
// the last submission of any kind would read `event=COMMENT` and fail a round that caught the
// defect and then said one word more.
reviewRun(
  "reviewer-text-findings-defect-head-7",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      submission({ ...approval(), event: "REQUEST_CHANGES" }),
      {
        ...submission({ event: "COMMENT", body: "And the usage line is worth a word." }),
        at: "2026-09-30T17:31:00.000Z",
      },
    ]),
  },
  [...header, ...turn]
);
// head-8: the change request the case asks for, submitted on another repository's pull request
// with `gh pr review -R`. The stand-in matches its routes on the pull request's number alone, so
// it answered the call as GitHub would answer the right one, and a review of other/widgets sends
// nothing back on the world's example/widgets. The `gh api` route has been read with its
// repository since the deep pass of 2026-11-10 (head-50); this route was not, so the negative
// control passed a round whose verdict its own pull request never got.
reviewRun(
  "reviewer-text-findings-defect-head-8",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: [
          "pr",
          "review",
          "1001",
          "-R",
          "other/widgets",
          "--request-changes",
          "--body",
          "The blank-name guard is gone.",
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
// head-9 is head-8 with the repository written against the flag, `--repo=other/widgets`, which
// gh takes as readily as the two arguments. Catches: a reader of the flag that takes the
// argument after it and nothing else, which finds no repository here, reads the call as a
// review of the world's own pull request, and passes the negative control on a verdict
// example/widgets never got.
reviewRun(
  "reviewer-text-findings-defect-head-9",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: [
          "pr",
          "review",
          "1001",
          "--repo=other/widgets",
          "--request-changes",
          "--body",
          "The blank-name guard is gone.",
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
// head-10: the change request the case asks for, sent with the short flag `-r`. Catches: a
// reader of `--request-changes` alone, which would score a round that caught the defect as one
// that submitted nothing — the negative control passing for the wrong reason, which is the
// control measuring nothing.
reviewRun(
  "reviewer-text-findings-defect-head-10",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: ["pr", "review", "1001", "-r", "--body", "The blank-name guard is gone."],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
// head-73: the approval, and after it the read of the reviews already on the pull request,
// written with the query's parameters as fields — `--method GET -f per_page=100`, which gh
// sends as a GET. A read submits nothing, and a reader that took those fields for a review's
// body made this call the round's last submission, carrying no event: the row of a round that
// submitted nothing, over a round that approved the head and said so.
reviewRun("reviewer-text-findings-text-only-head-73", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(approval()),
    {
      at: "2026-09-30T17:29:40.000Z",
      as: "gh",
      argv: [
        "api",
        "repos/example/widgets/pulls/1001/reviews",
        "--method",
        "GET",
        "-f",
        "per_page=100",
      ],
      exit: 0,
    },
  ]),
});
// head-100, head-101 and head-111: head-73's read of the reviews route in the other spellings gh
// takes its verb in — `-X get`, whose case is the caller's, `--method=GET`, whose value is
// written against the flag, and `-XGET`, whose value is written against the short flag.
// Catches: a verb read in one spelling and one case, where the read of the
// route becomes the round's last submission, carrying no event: the row of a round that
// submitted nothing over one that approved the head and said so. The gh stand-in reads the same
// call with the same reader (`isPost`), so a spelling lost here is a spelling the world answers
// as something else too.
for (const [name, verb] of [
  ["head-100", ["-X", "get"]],
  ["head-101", ["--method=GET"]],
  ["head-111", ["-XGET"]],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([
      submission(approval()),
      {
        at: "2026-09-30T17:29:40.000Z",
        as: "gh",
        argv: ["api", "repos/example/widgets/pulls/1001/reviews", ...verb, "-f", "per_page=100"],
        exit: 0,
      },
    ]),
  });
// head-102, head-112 and head-113: the approval of head-1 submitted with the verb in lower case
// — `--method post` — with it written against the short flag — `-XPOST` — and with no method at
// all, which gh sends as a POST because the call carries an `--input`. Catches: a verb compared
// as it was written, and a body flag left out of the list gh reads a POST from, each of which
// would read this call as no submission, leave the round's one submission out of the row and
// fail a reviewer that approved the head and said so.
for (const [name, verb] of [
  ["head-102", ["--method", "post"]],
  ["head-112", ["-XPOST"]],
  ["head-113", []],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([
      {
        ...submission(approval()),
        argv: ["api", ...verb, "repos/example/widgets/pulls/1001/reviews", "--input", "body.json"],
      },
    ]),
  });
// head-114 and head-115: the approval with field flags beside its `--input`. gh sends the input
// as the body and every field as a query parameter (`gh api --help`), so the body is the file's
// and no field of the call reaches it: head-114's input approves and its field asks for a change
// request, and the round approved; head-115's input names no event at all, and `-f event=APPROVE`
// beside it leaves the round a submission with no verdict, which is the pending review GitHub
// left. Catches: a reader that merges the fields into the input's object, which read head-115 as
// an approval — a pass for a round whose review GitHub never submitted — and one that lets the
// fields override it, which read head-114 as a change request.
reviewRun("reviewer-text-findings-text-only-head-114", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...submission(approval()),
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews",
        "--input",
        "body.json",
        "-f",
        "event=REQUEST_CHANGES",
      ],
    },
  ]),
});
reviewRun("reviewer-text-findings-text-only-head-115", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...submission({ commit_id: head, body: "the open threads", comments: findings }),
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews",
        "--input",
        "body.json",
        "-f",
        "event=APPROVE",
      ],
    },
  ]),
});
// head-116: the approval, and after it a POST on the reviews route whose last argument is a
// bare `-f` with no `key=value` after it. The gh stand-in answers that call from the route and
// records it as it records every other, so the row reads it: it carries no field this can read
// and no input, so it submits nothing and the round's one submission is its approval. Catches:
// a reader of
// the fields that splits whatever follows the flag, which throws on the argument that is not
// there and makes a rig error of a round whose verdict is known; and one that reads such a call
// as a submission of its own, which would put a second submission carrying no event in the row.
reviewRun("reviewer-text-findings-text-only-head-116", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(approval()),
    {
      at: "2026-09-30T17:29:50.000Z",
      as: "gh",
      argv: ["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews", "-f"],
      exit: 0,
    },
  ]),
});
// head-74: the approval the rule asks for, and a `COMMENT` review after it, which leaves that
// approval standing. The verdict is the round's last `APPROVE` or `REQUEST_CHANGES`, so the head
// and the two inline comments are read off the approval; a reader taking the last submission of
// any kind would score this round `event=COMMENT head=false doc=false workflow=false` and fail a
// round that did everything the rule asks.
reviewRun("reviewer-text-findings-text-only-head-74", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(approval()),
    {
      ...submission({ event: "COMMENT", body: "And the trimmed name reads well." }),
      at: "2026-09-30T17:31:00.000Z",
    },
  ]),
});
// head-75: an unreadable read after the approval, the wake after it, and an accepting read after
// the wake. The late read could decide nothing — the message it would have ordered was sent
// before its answer came back — so what the reviewer was shown when it sent that message is
// still the unreadable read's answer, and the run is a rig error. A reader asking whether any
// read after the approval answered `accepting` finds this one, turns the unknown into
// `pass=false` and breaks the rule that a run whose verdict is unknown leaves both counts;
// head-37, the same run without the unreadable read, is the decided failure.
reviewRun(
  "reviewer-text-findings-text-only-head-75",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:41:00.000Z")),
  },
  [
    ...header,
    ...turn,
    ...readRecordVia("eval-unreadable", "2026-09-30T17:40:30.000Z"),
    ...readRecord("2026-09-30T17:43:00.000Z"),
  ]
);
// head-76: a read whose answer carries no time of its own. Times are compared as instants, so a
// missing one is the rig's fault and the run is a rig error naming it; compared as text it would
// sort before the approval and the read would drop out of the ordering without a word, which is
// how a run that never took the step could have read as one that did.
reviewRun(
  "reviewer-text-findings-text-only-head-76",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    {
      type: "message",
      timestamp: "2026-09-30T17:40:30.000Z",
      message: {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "call-read-untimed",
            name: "legion",
            arguments: { op: "read_record", issue: "LWEVAL-1" },
          },
        ],
      },
    },
    {
      type: "message",
      message: {
        role: "toolResult",
        toolCallId: "call-read-untimed",
        content: [{ type: "text", text: "the record of LWEVAL-1, in accepting" }],
        details: { record: { key: "LWEVAL-1", phase: "accepting", status: "needs_review" } },
      },
    },
  ]
);
// head-77, head-78 and head-79: the wake with one of its three names left out and the other two
// in it — the head (head-77), the document (head-78), the workflow (head-79). Catches: a reader
// of the wake that asks for two of the three names. Step 1 has the reviewer name the approved
// head and each open thread, and the implementer acts on the one message it is handed, so a
// message short of any one name leaves it without that fact; head-49, which names the head
// alone, is short of two at once and pins neither.
reviewRun("reviewer-text-findings-text-only-head-77", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo(
      "implementer",
      "Approved the head I read: docs/cli.md and .github/workflows/greet.yml are yours to close above it."
    )
  ),
});
reviewRun("reviewer-text-findings-text-only-head-78", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo(
      "implementer",
      `Approved ${head}: .github/workflows/greet.yml and the other thread are yours to close above it.`
    )
  ),
});
reviewRun("reviewer-text-findings-text-only-head-79", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo(
      "implementer",
      `Approved ${head}: docs/cli.md and the other thread are yours to close above it.`
    )
  ),
});
// head-80: a native `read_record` after the approval whose answer carries no record this can
// read — the tool answered, and what it answered is not a record. Catches: a reader that takes
// the native route's answer for a record whenever the call named the op, which would make this
// run a round that read no record and fail it on the step. What the reviewer was shown is
// unknown, so the run is a rig error, as head-40's cell is on the route that prints its answer.
reviewRun(
  "reviewer-text-findings-text-only-head-80",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...stamped(
      "call-read-recordless",
      "legion",
      { op: "read_record", issue: "LWEVAL-1" },
      "2026-09-30T17:40:30.000Z",
      {
        text: "the record of LWEVAL-1, in accepting",
        details: { key: "LWEVAL-1", phase: "accepting", status: "needs_review" },
      }
    ),
  ]
);
// head-81: a cell that built its arguments elsewhere, so its code names no op, and printed the
// record it got. Catches: a reader of the cell route that asks for the op in the code. A cell is
// read by its answer where its code says nothing, since the arguments can be built anywhere, and
// the record is parsed out of what it printed: head-45's cell names the word and prints no
// record, and this one prints the record and names no word.
reviewRun(
  "reviewer-text-findings-text-only-head-81",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...stamped(
      "call-read-built",
      "eval",
      { language: "py", code: "print(tool.legion(args))" },
      "2026-09-30T17:40:30.000Z",
      {
        text: `the cell printed ${JSON.stringify({
          record: { key: "LWEVAL-1", phase: "accepting", status: "needs_review" },
        })}`,
        details: { cells: [{ language: "py" }], language: "py" },
      }
    ),
  ]
);
// head-82: the same read, printed after the cell's own words and a brace run that is no JSON —
// the Python mapping the cell echoed. Catches: a reader that parses the first `{` of the output
// and stops. The record is parsed out of the text and the words around it are read not at all,
// so a cell that prints anything before its answer is read as head-39's is.
reviewRun(
  "reviewer-text-findings-text-only-head-82",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...stamped(
      "call-read-echoed",
      "eval",
      {
        language: "py",
        code: 'args = {"op": "read_record", "issue": "LWEVAL-1"}\nprint(args, tool.legion(args))',
      },
      "2026-09-30T17:40:30.000Z",
      {
        text: `the cell printed {'op': 'read_record', 'issue': 'LWEVAL-1'} ${JSON.stringify({
          record: { key: "LWEVAL-1", phase: "accepting", status: "needs_review" },
        })}`,
        details: { cells: [{ language: "py" }], language: "py" },
      }
    ),
  ]
);
// head-83: the wake, and after it the read that holds no record. Catches: a rig error made of
// any unreadable read after the approval, wherever the wake fell. The unknown is the answer the
// wake followed, so a read that came after the message it would have ordered could have decided
// nothing: this round sent its wake before it read anything, which is head-37's decided failure
// with one read it could not read added. head-75 is the other order — the unreadable read, then
// the wake — and is the rig error.
reviewRun(
  "reviewer-text-findings-text-only-head-83",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:40:00.000Z")),
  },
  [...header, ...turn, ...readRecordVia("eval-unreadable", "2026-09-30T17:40:30.000Z")]
);
// head-84: the two-step route with the verdict's own body carrying the head and both inline
// comments, over a create that named another head and one comment. Catches: a fold that takes
// the create's head and comments whatever the submit call carries. The create is the pending
// review and the submit call is the round's act, so what that call says stands and the create
// fills in only what it leaves out, which is what head-67 reads the other way round.
reviewRun("reviewer-text-findings-text-only-head-84", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...pendingReview,
      files: {
        "pending.json": JSON.stringify({
          commit_id: "7".repeat(40),
          body: "A first reading, at the head I had.",
          comments: findings.slice(0, 1),
        }),
      },
    },
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/events",
        "--input",
        "submit.json",
      ],
      exit: 0,
      files: { "submit.json": JSON.stringify(approval()) },
    },
  ]),
});
// head-85, head-86 and head-87: `gh pr review` with the short flags the command takes and with
// no flag at all. Catches: a reader of the long flags alone, which would score `-a` and `-c` as
// rounds that submitted nothing. head-85 approves with `-a`, which is head-6's row by another
// spelling; head-86 comments with `-c`, which is no verdict the merge gate reads; head-87 names
// no event, which is the command GitHub answers with a pending review and which submits no
// verdict — a submission carrying none, not a call that submitted nothing.
for (const [name, flag, body] of [
  ["head-85", "-a", "Looks good."],
  ["head-86", "-c", "One more reading, for the record."],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: ["pr", "review", "1001", flag, "--body", body],
        exit: 0,
      },
    ]),
  });
reviewRun("reviewer-text-findings-text-only-head-87", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: ["pr", "review", "1001", "--body", "The open threads are yours to close."],
      exit: 0,
    },
  ]),
});
// head-88: the submission with the leading slash `gh api` takes either way, which is how the
// route is written wherever it is copied from GitHub's own documentation. Catches: a route read
// without it, which would score this round as one that submitted nothing.
reviewRun("reviewer-text-findings-text-only-head-88", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...submission(approval()),
      argv: [
        "api",
        "--method",
        "POST",
        "/repos/example/widgets/pulls/1001/reviews",
        "--input",
        "body.json",
      ],
    },
  ]),
});
// head-89 and head-90: the pending review of head-68, and a submit call the round's own pull
// request never got — head-89 submits it on another repository's, which the stand-in answers as
// GitHub would answer the right one, and head-90 names a route that is not the submit route,
// here with the trailing slash a copied URL carries. Catches: a submit route read without its
// repository or without its end, either of which would fold the create into a call this pull
// request has no verdict from. Each round is left with its pending review, which is no verdict.
for (const [name, route] of [
  ["head-89", "repos/sjawhar/legion-smoke/pulls/1001/reviews/5002/events"],
  ["head-90", "repos/example/widgets/pulls/1001/reviews/5002/events/"],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([
      pendingReview,
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: ["api", "--method", "POST", route, "-f", "event=APPROVE"],
        exit: 0,
      },
    ]),
  });
// head-91 and head-92: the review written as fields through the other three flags gh takes one
// by — `--raw-field` and `-F` in head-91, `--field` beside `-f` in head-92. Catches: a reader of
// one spelling, which would lose the verdict or the head of a round that wrote it with another.
// Each round fails as head-65 does, for the two findings a field cannot carry.
for (const [name, eventFlag, headFlag] of [
  ["head-91", "--raw-field", "-F"],
  ["head-92", "--field", "-f"],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: [
          "api",
          "--method",
          "POST",
          "repos/example/widgets/pulls/1001/reviews",
          eventFlag,
          "event=APPROVE",
          headFlag,
          `commit_id=${head}`,
        ],
        exit: 0,
      },
    ]),
  });
// head-93: a submission whose body file holds no JSON, which is what a round that wrote its
// review as prose and piped the file in leaves. Catches: a reader that parses the body and lets
// the throw out, which would make a rig error of a round whose verdict is known: the call went
// out and the stand-in took it, so the round submitted a review carrying nothing this can read,
// and the row says what the submission holds.
reviewRun("reviewer-text-findings-text-only-head-93", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...submission(approval()),
      files: { "body.json": "Approved. The two threads are the work the merge waits for.\n" },
    },
  ]),
});
// head-94 and head-95: the head named by its first seven characters, which is the fewest that
// names one commit, and by its first twelve, which is how a message names one everywhere else in
// this loop. Catches: a reader that asks for more than seven, and one that asks for the whole
// forty — head-70's six characters fail under every reading, so the boundary and the prefix are
// each a run of their own.
for (const [name, length] of [
  ["head-94", 7],
  ["head-95", 12],
] as const)
  reviewRun(`reviewer-text-findings-text-only-${name}`, {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval(findings, head.slice(0, length)))]),
  });
// head-96: the wake sent in the millisecond the read was answered in. Catches: an ordering read
// as `>=`, where a message written in the answer's own millisecond follows it. The step is the
// record read and then the message, as head-62 is the submission and then the read, and both
// times are the rig's own to the millisecond.
reviewRun(
  "reviewer-text-findings-text-only-head-96",
  {
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "published.jsonl": bus(toldImplementer(head, "2026-09-30T17:40:30.000Z")),
  },
  [...header, ...turn, ...readRecord("2026-09-30T17:40:30.000Z")]
);
// head-97: a wake that names both findings and writes the head's first twelve characters into a
// word of its own. Catches: a commit read out of any run of hexadecimal characters, wherever it
// falls. `namesCommit` asks for a word of seven digits or more, so a run glued to the word before
// it is no name of the head: the implementer reading this message has the two paths and no head
// to land them above.
reviewRun("reviewer-text-findings-text-only-head-97", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo(
      "implementer",
      `Approved commit${head.slice(0, 12)}: docs/cli.md and .github/workflows/greet.yml are yours to close above it.`
    )
  ),
});
// head-98 and head-99: the round that passes as head-1 does, with the reference read (head-98)
// and with two other skills read and the reference not (head-99). Catches: a `ref` that answers
// the same for every run — every other run of this scenario reads nothing, so `ref=false` is
// pinned by all of them and `ref=true` by none, and a reader that counted any skill read would
// credit head-99 with a reference it never opened. `ref` is counted beside the pass and not in
// it, so both rounds pass.
reviewRun(
  "reviewer-text-findings-text-only-head-98",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...readRecord(),
    ...readOf("call-reference", "skill://legion-worker/references/merge-gate.md"),
  ]
);
reviewRun(
  "reviewer-text-findings-text-only-head-99",
  { "case.txt": "text-only\n", "calls.jsonl": calls([submission(approval())]) },
  [
    ...header,
    ...turn,
    ...readRecord(),
    ...readOf("call-skill", "skill://legion-worker"),
    ...readOf("call-threads", "skill://legion-worker/references/review-threads.md"),
  ]
);
// head-103: an approval of the head whose two inline comments are on docs/cli.md and on
// docs/workflows.md, a document of the same word that is not the workflow the world's second
// finding is on (.github/workflows/greet.yml). Catches: the workflow read as any path holding
// the word `workflows`, which would pass a round that wrote one of the two findings as a thread
// and left the other with none. The finding's file is the path the reviewer's comment must be
// on, and a document that merely names it is another file.
reviewRun("reviewer-text-findings-text-only-head-103", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    submission(
      approval([
        ...findings.slice(0, 1),
        {
          path: "docs/workflows.md",
          line: 3,
          side: "RIGHT",
          body: "The page about the workflows reads well.",
        },
      ])
    ),
  ]),
});
// head-104: the two-step route with the submit call written with the leading slash `gh api`
// takes either way, which is how the route reads wherever it is copied from GitHub's own
// documentation. Catches: a submit route read without it, which would leave the create standing
// as the round's one submission — a pending review, carrying no event — over a round that
// created it and then submitted the approval. head-88 is the same spelling of the create.
reviewRun("reviewer-text-findings-text-only-head-104", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    pendingReview,
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "/repos/example/widgets/pulls/1001/reviews/5002/events",
        "-f",
        "event=APPROVE",
      ],
      exit: 0,
    },
  ]),
});
// head-105: two pending creates, the first at a head the round had before the implementer's
// last push and carrying one of the two findings, the second at the world's head carrying both,
// and then the submit of the verdict. Catches: a submit folded into the first pending create
// rather than the last, which would read this round's approval at a head the pull request left
// behind and with one finding written. The create the submit folds in is the last one before it
// that named no event, which is the review the round was still writing.
reviewRun("reviewer-text-findings-text-only-head-105", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...pendingReview,
      at: "2026-09-30T17:29:00.000Z",
      files: {
        "pending.json": JSON.stringify({
          commit_id: "7".repeat(40),
          body: "A first reading, at the head I had.",
          comments: findings.slice(0, 1),
        }),
      },
    },
    pendingReview,
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/events",
        "-f",
        "event=APPROVE",
      ],
      exit: 0,
    },
  ]),
});
// head-106: a pending create carrying the head and both findings, a `COMMENT` review of its own
// after it — a reading the round posted whole, naming no head and no inline comment — and then
// the submit of the verdict. Catches: a submit folded into the last review of any kind rather
// than into the pending one, which would take that comment's empty head and no comments for the
// approval's and fail a round that wrote the review the rule asks for. A submitted review is no
// pending review, and neither is one the round posted entire.
reviewRun("reviewer-text-findings-text-only-head-106", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    pendingReview,
    {
      ...submission({ event: "COMMENT", body: "One more reading, for the record." }),
      at: "2026-09-30T17:29:25.000Z",
    },
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/events",
        "-f",
        "event=APPROVE",
      ],
      exit: 0,
    },
  ]),
});
// head-107: a submit call carrying the head and both findings and naming no event, and a second
// submit naming the event alone. Catches: a submit folded into an earlier submit, which would
// read the head and the comments of the first call as the second's and pass a round whose
// verdict names no head at all. The call that submits a review is no pending review for the
// next one to fold in — the create is, and this round made none — so the round's verdict is the
// second call as it stands.
reviewRun("reviewer-text-findings-text-only-head-107", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:20.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/events",
        "--input",
        "first.json",
      ],
      exit: 0,
      files: {
        "first.json": JSON.stringify({
          commit_id: head,
          body: "The open threads are the work the merge waits for.",
          comments: findings,
        }),
      },
    },
    {
      at: "2026-09-30T17:29:30.000Z",
      as: "gh",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews/5002/events",
        "-f",
        "event=APPROVE",
      ],
      exit: 0,
    },
  ]),
});
// head-108: an approval whose `commit_id` is twelve characters taken from the middle of the
// head. Catches: a head named by any run of characters the SHA holds, where a `commit_id` that
// is no prefix of it would name it. A review names the head it was submitted on by a prefix
// (`namesHead`), so this submission names a commit this pull request has not got, as head-94's
// seven characters and head-95's twelve do name one.
reviewRun("reviewer-text-findings-text-only-head-108", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval(findings, head.slice(5, 17)))]),
});
// head-109: the whole wake, naming the head and both findings, published to a topic that holds
// the implementer's role word and ends in another: `…-implementer2`. Catches: a topic read for
// the role anywhere in it, where a message that reached no implementer's topic would count as
// the implementer's wake. A role topic ends with `-<role>` (contracts' roleToken), and Envoy
// hands each topic's messages to the session that holds it, so no implementer reads this one.
reviewRun("reviewer-text-findings-text-only-head-109", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "published.jsonl": bus(
    sentTo(
      "implementer2",
      `Approved ${head}: docs/cli.md and .github/workflows/greet.yml are yours to close above it.`
    )
  ),
});
// head-110: the round head-1 passes on, with one push to a ref of the agent's own,
// `refs/heads/scratch`. Catches: the pushes counted on the issue branch alone. A review round
// pushes nothing at all, whatever the ref — the branch is the one the reviewer reads and the
// architect sees, and a ref beside it is still this round spending a push it had no work for.
reviewRun("reviewer-text-findings-text-only-head-110", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "pushes.log": `2026-09-30T17:30:00.000Z refs/heads/scratch ${"0".repeat(40)} ${head}\n`,
});
// defect head-11: the change request the case asks for, sent with `-r`, and a `gh pr review
// --comment` after it — a second reading posted as a comment, which on GitHub leaves the change
// request standing. Catches: `--comment` read as an approval, which would make this round's
// verdict `APPROVE` and fail a control that caught the defect and then said one word more.
// head-7 is the same pair on the `gh api` route.
reviewRun(
  "reviewer-text-findings-defect-head-11",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: ["pr", "review", "1001", "-r", "--body", "The blank-name guard is gone."],
        exit: 0,
      },
      {
        at: "2026-09-30T17:31:00.000Z",
        as: "gh",
        argv: [
          "pr",
          "review",
          "1001",
          "--comment",
          "--body",
          "And the usage line is worth a word.",
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
// defect head-12: the change request the case asks for, written as fields with no `--method` at
// all, which gh sends as a POST because the call carries them. Catches: a call with a field and
// no method read as a GET, which would leave this round's one submission out of the row and
// fail a control that caught the defect. The body is fields and no `--input`, which is why this
// is the defect control's row and not the text-only arm's: a review's inline comments are a
// list, which no field can carry.
reviewRun(
  "reviewer-text-findings-defect-head-12",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: [
          "api",
          "repos/example/widgets/pulls/1001/reviews",
          "-f",
          "event=REQUEST_CHANGES",
          "-f",
          `commit_id=${head}`,
          "-f",
          "body=The blank-name guard is gone.",
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
// defect head-13: the change request the case asks for, on a `gh pr review` that names the
// repository twice — another repository first and the world's own after it. gh's flag parser
// takes the value of the last one it reads, so this call reviewed example/widgets. Catches: a
// reader of the first occurrence, which reads the call as a review of other/widgets, scores the
// round `submissions=0` and fails a control that caught the defect. head-8 names the other
// repository once and is no submission of this round.
reviewRun(
  "reviewer-text-findings-defect-head-13",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: [
          "pr",
          "review",
          "1001",
          "-R",
          "other/widgets",
          "-R",
          "example/widgets",
          "-r",
          "--body",
          "The blank-name guard is gone.",
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);
// defect head-14: the change request the case asks for, and after it an approval written with
// the value against each flag — `--raw-field=event=APPROVE`, which gh takes as `--raw-field
// event=APPROVE`. The round's verdict is its last `APPROVE` or `REQUEST_CHANGES`, so the
// control fails: it approved a change that carries the defect. Catches: a field value read only
// where it is the argument after the flag, which would read this call as a submission carrying
// no event, leave the change request standing and pass a control that approved.
reviewRun(
  "reviewer-text-findings-defect-head-14",
  {
    "case.txt": "defect\n",
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:29:30.000Z",
        as: "gh",
        argv: ["pr", "review", "1001", "-r", "--body", "The blank-name guard is gone."],
        exit: 0,
      },
      {
        at: "2026-09-30T17:31:00.000Z",
        as: "gh",
        argv: [
          "api",
          "--method",
          "POST",
          "repos/example/widgets/pulls/1001/reviews",
          "--raw-field=event=APPROVE",
          `--raw-field=commit_id=${head}`,
        ],
        exit: 0,
      },
    ]),
  },
  [...header, ...turn]
);

// The rig's two records of the case disagree, and a directory that names no case at all.
reviewRun("reviewer-text-findings-defect-head-3", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
});
reviewRun("reviewer-text-findings-head-9", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
});

// threads-closed: what the two agents of one run wrote — the gh calls the stand-in recorded,
// each carrying the role that made it, the pushes the run's own remote logged, the body the
// stand-in kept, the daemon stand-in's log and the two transcripts. The world's threads are the
// fixture's own record of them (threads.json).
const fixIds = ["PRRT_kwDOfix001", "PRRT_kwDOfix002"];
const fixThreadsFile = JSON.stringify([
  { id: fixIds[0], path: "docs/cli.md", line: 5 },
  { id: fixIds[1], path: ".github/workflows/greet.yml", line: 1 },
]);
const defectThreadsFile = JSON.stringify([{ id: "PRRT_kwDOfix101", path: "docs/cli.md", line: 5 }]);
/** The same two threads, with the first carrying the underscore and the hyphen GitHub's
 * base64url node ids carry (`PRRT_kwDO_ab-1`): the shape head-86 reads the thread-id pattern
 * with, whose files are the world's two documents as every other threads-closed run's are. */
const oddIds = ["PRRT_kwDO_ab-1", "PRRT_kwDOfix002"];
const oddThreadsFile = JSON.stringify([
  { id: oddIds[0], path: "docs/cli.md", line: 5 },
  { id: oddIds[1], path: ".github/workflows/greet.yml", line: 1 },
]);
/** The reviewer's listing of the threads: the `reviewThreads` query of
 * skills/legion-worker/references/review-threads.md. The reviewer verifies thread state there
 * and never in the pull request's body, which is the implementer's record and not the fact. */
const threadsList = (at: string, by = "reviewer") => ({
  at,
  as: "gh",
  by,
  argv: [
    "api",
    "graphql",
    "--paginate",
    "-f",
    "query=query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved path}}}}}",
  ],
  exit: 0,
});
/** The implementer's reply on one thread, the mutation review-threads.md defines, naming the
 * commit that fixed it. */
const threadReply = (id: string, at: string, commit = "", by = "implementer") => ({
  at,
  as: "gh",
  by,
  argv: [
    "api",
    "graphql",
    "-f",
    `query=mutation{addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:"${id}",body:"Fixed in ${commit.slice(0, 12)}: the sentence now says what the CLI does."}){comment{id}}}`,
  ],
  exit: 0,
});
/** The resolution, one call per thread — or, naming two, the bulk call the reference forbids. */
const threadResolve = (ids: string[], at: string, by = "reviewer") => ({
  at,
  as: "gh",
  by,
  argv: [
    "api",
    "graphql",
    "-f",
    `query=mutation{${ids
      .map((id, index) => `t${index}:resolveReviewThread(input:{threadId:"${id}"}){thread{id}}`)
      .join(" ")}}`,
  ],
  exit: 0,
});
/** One thread resolved by the call the reference prints, with a `--jq` selector that reads the
 * mutation's own answer back: the shape a run of 2026-11-07 used, which names the field a second
 * time while resolving one thread. */
const threadResolveVerified = (id: string, at: string) => ({
  at,
  as: "gh",
  by: "reviewer",
  argv: [
    "api",
    "graphql",
    "-f",
    "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
    "-F",
    `id=${id}`,
    "--jq",
    ".data.resolveReviewThread.thread.isResolved",
  ],
  exit: 0,
});
/** One thread resolved by a call that carries the mutation twice on that one thread: the shape
 * of a request that resolves and then resolves again, which names one thread and invokes the
 * mutation two times. */
const threadResolveTwice = (id: string, at: string) => ({
  at,
  as: "gh",
  by: "reviewer",
  argv: [
    "api",
    "graphql",
    "-f",
    `query=mutation{t0:resolveReviewThread(input:{threadId:"${id}"}){thread{id}} t1:resolveReviewThread(input:{threadId:"${id}"}){thread{isResolved}}}`,
  ],
  exit: 0,
});
/** One thread resolved by one invocation of the mutation, with a second thread named in the
 * query's own comment: two ids in the call's text, one resolution in it. */
const threadResolveNaming = (id: string, other: string, at: string) => ({
  at,
  as: "gh",
  by: "reviewer",
  argv: [
    "api",
    "graphql",
    "-f",
    `query=mutation{resolveReviewThread(input:{threadId:"${id}"}){thread{id}}} # ${other} is the same finding`,
  ],
  exit: 0,
});
/** The implementer's reply on one thread through the REST route, under the review comment the
 * thread hangs from (`POST repos/{owner}/{repo}/pulls/comments/{id}/replies`), naming the thread
 * it answers and the commit that fixed it: the other route review-threads.md's reply is made by.
 */
const restReply = (id: string, comment: number, at: string, commit: string) => ({
  at,
  as: "gh",
  by: "implementer",
  argv: [
    "api",
    "--method",
    "POST",
    `repos/example/widgets/pulls/comments/${comment}/replies`,
    "-f",
    `body=Fixed in ${commit.slice(0, 12)}: thread ${id} now says what the CLI does.`,
  ],
  exit: 0,
});
/** The body the implementer leaves, with its `Threads` entries written out as given: each entry
 * is what `threadsLine` reads, so a run below can break one thing about one entry. */
const bodyOf = (at: string, entries: string[]) => ({
  at,
  as: "gh",
  by: "implementer",
  argv: ["pr", "edit", "1001", "--body-file", "body.md"],
  exit: 0,
  body: [`**Threads:** 0 resolved, ${entries.length} unresolved.`, ...entries].join("\n"),
});
/** The `Threads` section of the body the implementer leaves, each entry rewritten at the head
 * its push made (references/pr-body.md). `open` leaves them as the approval wrote them. */
const fixBody = (ids: string[], at: string, commit: string, open = false) =>
  bodyOf(
    at,
    ids.map((id) =>
      open
        ? `- Thread ${id}: still open — the sentence still stands; unresolved at ${head.slice(0, 12)}.`
        : `- Thread ${id}: fixed in ${commit.slice(0, 12)} — says what the CLI does; unresolved at ${commit.slice(0, 12)}.`
    )
  );
/** The reviewer's second approval, at the head the push made, named by SHA. */
const secondApproval = (commit: string, at = "2026-09-30T17:38:00.000Z") => ({
  ...submission({
    commit_id: commit,
    event: "APPROVE",
    body: "The text fixes close both threads, which I resolved at this head.",
  }),
  at,
  by: "reviewer",
});
/** The reviewer's `REQUEST_CHANGES` naming the behavioural finding, which the code-defect case's
 * thread turns out to be: the review GitHub carries to the daemon, which sends the issue back. */
const changesRequested = (commit: string, at = "2026-09-30T17:38:00.000Z") => ({
  ...submission({
    commit_id: commit,
    event: "REQUEST_CHANGES",
    body: "Thread PRRT_kwDOfix101 is false because the code is wrong: a blank name must exit 2.",
  }),
  at,
  by: "reviewer",
});
/** The daemon stand-in's log line of a completion it refused, which is the answer each half of
 * this loop gets: both are booted in `accepting`, the architect's phase, which neither of them
 * runs, so the stand-in answers 409 HANDOFF_NOT_CURRENT_PHASE as the workflow does
 * (daemon-standin.ts). */
const refusedCompletion = {
  at: "2026-09-30T17:40:00.000Z",
  path: "/legion/v1/handoff/complete",
  status: 409,
  grantId: "g-1",
};
/** The line of a completion the stand-in accepted, which is what a reviewing phase's own
 * completion gets and what no half of this loop can have (head-63). */
const completed = { ...refusedCompletion, status: 200 };
/** The backward move the architect makes and neither role does. */
const movedBack = (reason: string) => ({
  at: "2026-09-30T17:40:00.000Z",
  path: "/legion/v1/phase/backward",
  status: 200,
  grantId: "g-1",
  to: "implementing",
  reason,
});
/** The implementer's wake of the reviewer: the loop runs in `accepting`, where the daemon starts
 * no phase, so the reviewer learns the fix is pushed from this message, which names the new head
 * and each thread the push answered (review-threads.md step 2). */
const toldReviewerOfPush = (commit: string, at?: string, sender?: string) =>
  sentTo(
    "reviewer",
    `The text fixes are pushed: the head to read is ${commit}, closing ${fixIds[0]} and ${fixIds[1]}.`,
    at,
    sender
  );
/** The reviewer's wake of the architect: the second approval is the architect's gate, and no
 * phase ends to carry it, so this message naming the head it approved is how the architect
 * learns the loop closed. */
const toldArchitectOfHead = (commit: string, at = "2026-09-30T17:42:00.000Z") =>
  sentTo("architect", `I approved the head the fix landed on, ${commit}.`, at, SECOND_SESSION);
/** The run's own remote, built once as this module is read, and its four commits: the head the
 * reviewer approved, a commit that changes the two documents, one that changes the code too, and
 * one above the approved head that changes one document alone.
 *
 * Two things make the ids what they are, and the second is why literals were refused.
 *
 * First, the git this builds with is given an environment of its own rather than the process's.
 * A Legion pane carries `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`,
 * `GIT_COMMITTER_EMAIL` and a `GIT_CONFIG_COUNT` set of `GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*`
 * pairs, and git reads those over the `-c` settings a caller passes, while `GIT_CONFIG_GLOBAL`
 * blocks the global file and none of them. So a seed that inherited them committed under
 * whichever identity its host carried: measured on 2026-11-11, one pod made
 * `a4d68ec975261ac0…`, a GitHub runner `b50d021dc49f9cd8…` and a second pod
 * `3d06d7372cdfc7c5…`, from one tree and one date.
 *
 * Second, the ids are read from the repository rather than written here. A literal id asserts
 * nothing a fixture needs — the fixtures need the commits of their own remote, whatever they are
 * — and it is a fact about one host, which is what reddened CI at `adc231d52ea7`. The
 * environment isolation is what the suite pins instead, below: the same seed built under a
 * hostile identity gives the same ids.
 *
 * It is built in the module body, because the fixtures below name its commits as they are read
 * and no hook has run by then, and it is built at **one path per content** rather than in a
 * fresh directory: `skill-scenarios-seed-<sha of the recipe>` under the temporary directory,
 * made by renaming a finished build into place. A process that finds it already there uses it
 * and builds nothing. That is what keeps the module body from leaving a directory behind,
 * measured on 2026-11-11: a removal registered on `process.on("exit")` never ran under the test
 * runner, and sixteen `skill-scenarios-threads-*` directories had collected by the time it was
 * read — one per run of the file, including the `bun test -t <pattern>` runs that select no row.
 * One cache shared by every run grows no further, and a change to the recipe below makes a new
 * path rather than reusing the old one. The clones go to the runs that asked for them in the
 * staging, where the runs directory exists. */
const seedRecipe = (steps: string[]) =>
  createHash("sha256").update(steps.join("\u0000")).digest("hex").slice(0, 16);
/** The date every commit of that repository is authored and committed at, in git's own raw
 * format, so two runs of this file on one host make one repository rather than two. */
const FIX_DATE = "@1759251600 +0000";
/** The environment a seed's git runs in: the caller's, less every variable git reads a commit's
 * identity or its configuration from, plus the ones this seed names. `GIT_CONFIG_COUNT` and its
 * numbered pairs are deleted whatever the count says, since one left behind is a setting this
 * seed never asked for. */
const seedGitEnv = (env: Record<string, string | undefined>) => {
  const own: Record<string, string | undefined> = { ...env };
  for (const name of [
    "GIT_AUTHOR_NAME",
    "GIT_AUTHOR_EMAIL",
    "GIT_COMMITTER_NAME",
    "GIT_COMMITTER_EMAIL",
    "GIT_CONFIG_COUNT",
    "GIT_CONFIG_SYSTEM",
    "GIT_CONFIG_NOSYSTEM",
  ])
    delete own[name];
  for (const key of Object.keys(own)) if (/^GIT_CONFIG_(KEY|VALUE)_\d+$/.test(key)) delete own[key];
  return {
    ...own,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_AUTHOR_DATE: FIX_DATE,
    GIT_COMMITTER_DATE: FIX_DATE,
  };
};
/** The `-c` settings every git of the seed runs under: the identity its commits carry, and the
 * two settings a host could otherwise decide. They are inputs of the build as much as the steps
 * are, so the recipe hashes them (SEED_INPUTS). */
const SEED_SETTINGS = [
  "user.name=t",
  "user.email=t@example.invalid",
  "commit.gpgsign=false",
  "core.autocrlf=false",
];
const seedGit = (dir: string, env: Record<string, string | undefined>, args: string[]) => {
  const result = Bun.spawnSync(
    ["git", ...SEED_SETTINGS.flatMap((setting) => ["-c", setting]), "-C", dir, ...args],
    { env: seedGitEnv(env) }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
  return result.stdout.toString().trim();
};
/** The steps of the seed, as the recipe its path is named for: one line per write and per git
 * command, so a change to any of them is a different path and never a reused repository. */
const SEED_STEPS: [kind: "write" | "git", ...parts: string[]][] = [
  ["git", "init", "-q", "-b", "legion/LWEVAL-1", "--object-format=sha1"],
  ["write", "greet.ts", 'export const greet = (name: string) => "Hello, " + name.trim() + "!";\n'],
  ["write", "docs/cli.md", "It prints the name exactly as it was given, spaces and all.\n"],
  [
    "write",
    ".github/workflows/greet.yml",
    "# Runs on every pull request and on every push to main.\n",
  ],
  ["git", "add", "-A"],
  ["git", "commit", "-qm", "the approved head"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  ["git", "commit", "-qam", "docs: the two text fixes"],
  ["write", "greet.ts", 'export const greet = (name: string) => "Hi, " + name.trim() + "!";\n'],
  ["git", "commit", "-qam", "feat: and the code too"],
  // A commit of its own above the approved head that fixes one of the two documents: the push
  // whose diff is a subset of the threads' files, which is a run that left a thread's sentence
  // false. It is made on a branch of its own, so its diff against the approved head is that one
  // file and no other.
  ["git", "checkout", "-q", "-b", "one-file", "HEAD~2"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["git", "commit", "-qam", "docs: one of the two text fixes"],
  ["git", "checkout", "-q", "legion/LWEVAL-1"],
];
/** Every input of the build the seed's path is named for: its steps, the date every commit
 * carries and the settings its git runs under. The recipe hashed the steps alone, so a change
 * to the date or to one of those settings named the same path for another repository — the
 * first process to build it would win, and every later one would read commits its own recipe
 * would never have made. */
const SEED_INPUTS = [...SEED_STEPS.flat(), FIX_DATE, ...SEED_SETTINGS];
/** The four commits a seed holds, read through `read`: the head the reviewer approved, the two
 * text fixes above it, the commit that changes the code too, and the one-file branch. Every
 * reader of a seed answers with these four — a build of its own and the shared one below — so
 * the two cannot drift into naming different revisions. */
const commitsOf = (read: (revision: string) => string) => ({
  approved: read("legion/LWEVAL-1~2"),
  fixed: read("legion/LWEVAL-1~1"),
  withCode: read("legion/LWEVAL-1"),
  docOnly: read("one-file"),
});
/** Builds the seed in `dir`, under `env`, and answers its four commits. The `one-file` branch
 * starts at `HEAD~2`, the approved head two commits below the tip, so the recipe names no id of
 * its own and a change to it is a change of the recipe's own text. */
const buildSeed = (dir: string, env: Record<string, string | undefined>) => {
  for (const [kind, ...parts] of SEED_STEPS) {
    if (kind === "write") {
      const [file, text] = parts as [string, string];
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(path.join(dir, file), text);
    } else seedGit(dir, env, parts);
  }
  return commitsOf((revision) => seedGit(dir, env, ["rev-parse", revision]));
};
/** Where the seed every run of this file shares lives: one path per recipe, under the
 * temporary directory. */
const fixDir = path.join(tmpdir(), `skill-scenarios-seed-${seedRecipe(SEED_INPUTS)}`);
const fixGit = (...args: string[]) => seedGit(fixDir, process.env, args);
/** That seed's four commits. A process that finds it there reads them and builds nothing; one
 * that does not builds into a fresh directory and renames it into place, so a reader never
 * meets a half-built repository and two processes racing leave one of the two builds behind
 * under `rename`'s own rules. Asking again is what the second run of a recipe does, and the row
 * below asks twice to read that the second ask built nothing. */
const seedOf = () => {
  if (!existsSync(path.join(fixDir, ".git"))) {
    const building = mkdtempSync(path.join(tmpdir(), "skill-scenarios-seed-building-"));
    buildSeed(building, process.env);
    try {
      renameSync(building, fixDir);
    } catch {
      // Another process of the same recipe got there first, which is the one answer its own
      // build would have given too.
      rmSync(building, { recursive: true, force: true });
    }
  }
  return commitsOf((revision) => fixGit("rev-parse", revision));
};
const fixCommits = seedOf();
/** Each run that asked for a clone of that repository, which the staging below gives a bare one
 * of in its own directory, as rig.sh's worker_fixture leaves one. Each run names itself through
 * `remoteFor` as the module body runs, and the body is over before the hook drains this staging,
 * so no list of the runs is kept by hand — the one written here had gone out of order and had to
 * be edited for every fixture added. */
const fixClones: string[] = [];
const fix = { ...fixCommits, remoteFor: (name: string) => fixClones.push(name) };
stagings.push(() => {
  for (const name of fixClones) {
    const remote = path.join(runs, name, "remote.git");
    mkdirSync(path.dirname(remote), { recursive: true });
    fixGit("clone", "-q", "--bare", fixDir, remote);
  }
});
// Catches: a seed that commits under whatever identity its own environment carries. Git reads
// `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` and a
// `GIT_CONFIG_COUNT` set of `GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*` pairs over the `-c` settings
// a caller passes, and a Legion pane exports all of them, so a seed that inherits its process's
// environment makes a different commit on every host: measured on 2026-11-11, `a4d68ec975261ac0…`
// in one pod, `b50d021dc49f9cd8…` on a GitHub runner and `3d06d7372cdfc7c5…` in a second pod,
// from one tree and one date. This builds the seed twice, under a hostile identity and under an
// environment with no `GIT_*` in it at all, and requires one answer from both: the fixtures read
// the repository's own commits, so what must hold is that the repository is the same wherever it
// is built. It also catches a cache that answers for another recipe: both builds go to
// directories of their own and neither reads the shared one.
test("the seed's commits are the same under any environment its host carries", () => {
  const built = (env: Record<string, string | undefined>) => {
    const dir = mkdtempSync(path.join(tmpdir(), "skill-scenarios-seed-built-"));
    try {
      return buildSeed(dir, env);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };
  const hostile = {
    ...process.env,
    GIT_AUTHOR_NAME: "A Pane",
    GIT_AUTHOR_EMAIL: "pane@example.invalid",
    GIT_COMMITTER_NAME: "A Pane",
    GIT_COMMITTER_EMAIL: "pane@example.invalid",
    GIT_CONFIG_COUNT: "1",
    GIT_CONFIG_KEY_0: "user.name",
    GIT_CONFIG_VALUE_0: "Another Pane",
  };
  const bare = Object.fromEntries(
    Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))
  );
  expect(built(hostile)).toEqual(built(bare));
  // And the shared seed this file's fixtures name is that same repository, whichever process
  // built it: a cache read as a repository of its own would pass the two builds above and still
  // hand the fixtures another commit.
  expect(fixCommits).toEqual(built(bare));
}, 120_000);
// Catches: a recipe that hashes the steps alone, leaving out the date every commit carries and
// the `-c` settings the seed's git runs under. Two builds that differ in either are two
// repositories, and one path for both hands whichever process came second the commits of the
// first — which the row above cannot see, since both of its builds go to directories of their
// own and neither reads the shared one.
test("the seed's recipe names every input of the build, not its steps alone", () => {
  const inputs = (date: string, settings: string[]) => [...SEED_STEPS.flat(), date, ...settings];
  expect(seedRecipe(SEED_INPUTS)).toBe(seedRecipe(inputs(FIX_DATE, SEED_SETTINGS)));
  expect(seedRecipe(SEED_STEPS.flat())).not.toBe(seedRecipe(SEED_INPUTS));
  expect(seedRecipe(inputs("@1759251601 +0000", SEED_SETTINGS))).not.toBe(seedRecipe(SEED_INPUTS));
  expect(seedRecipe(inputs(FIX_DATE, ["user.name=another", ...SEED_SETTINGS.slice(1)]))).not.toBe(
    seedRecipe(SEED_INPUTS)
  );
});
// Catches: a module body that leaves a directory behind. The seed is built as this file is read,
// before any hook runs, so no `afterAll` removes it and `process.on("exit")` never ran under the
// test runner — sixteen `skill-scenarios-threads-*` directories had collected by 2026-11-11, one
// per run of the file, the `bun test -t <pattern>` runs that select no row among them. One path
// per recipe is what bounds it, so this asks for the seed a second time between the two readings
// of the directory, which is what the next run of this recipe does: that ask must build nothing,
// and a build renamed into place would be a directory of its own, which its inode says. Each
// reading names the recipe's own path and no other: `skill-scenarios-seed-` also begins the
// `-building-` and `-built-` directories a concurrent run of this file makes and removes, so a
// count of everything under that prefix counts another process's work and can move while
// nothing here does.
test("the seed is one directory per recipe, and a second ask for it builds nothing", () => {
  const name = `skill-scenarios-seed-${seedRecipe(SEED_INPUTS)}`;
  expect(fixDir).toBe(path.join(tmpdir(), name));
  const own = () => ({
    entries: readdirSync(tmpdir()).filter((entry) => entry === name),
    directory: statSync(fixDir).ino,
  });
  const before = own();
  expect(before.entries).toEqual([name]);
  expect(seedOf()).toEqual(fixCommits);
  expect(own()).toEqual(before);
});
const pushed = (sha: string) =>
  `2026-09-30T17:35:00.000Z refs/heads/legion/LWEVAL-1 ${fix.approved} ${sha}\n`;
/** A threads-closed run: the text-fixes world and its two threads unless `files` says otherwise,
 * with the reviewer's own transcript in <run>/sessions/reviewer, where rig.sh's worker_boot puts
 * the second agent's. Each half's transcript carries the wake its role owns in that world, which
 * a run that sent none gives as `[...header, ...turn]`.
 *
 * Whether there is a second half at all is the rig's own rule and not a choice of each fixture:
 * rig.sh's run_threads_closed boots the reviewer on the messages the implementer published to
 * its topic, and boots none in code-defect where that wake holds none, since sending the
 * reviewer nothing is the act that case measures (its `reviewer_wake`). So a run whose record
 * holds no message on the reviewer's topic gets a reviewer half only where the rig would have
 * booted one, which in text-fixes is always — the implementer owed a wake there, and the empty
 * prompt it leaves is the `toldReviewer` failure it already was. In all 20 code-defect runs
 * recorded on 2026-11-10 `sessions/reviewer` existed exactly when `reviewer-wake.json` held a
 * message, and before this each code-defect fixture that published nothing to that topic still
 * got a reviewer half, so the terms over the reviewer's acts were read against an agent the rig
 * would never have run. `reviewer: null` says so for a run whose record this cannot read, and a
 * transcript with no assistant message in it (head-18's `[...header]`, head-16's
 * `[...secondHeader]`) is the other shape: a half that started and got no model turn, which is
 * a rig error in every case. */
function fixRun(
  name: string,
  files: Record<string, string | null>,
  transcripts: { implementer?: object[]; reviewer?: object[] | null } = {}
) {
  fix.remoteFor(name);
  const world: Record<string, string | null> = {
    "out.txt": "exit=0\n",
    "world.json": worldOf(fix.approved),
    "threads.json": fixThreadsFile,
    "case.txt": "text-fixes\n",
    "pushes.log": "",
    "daemon.log": calls([refusedCompletion]),
    "handoffs.json": handoffs(reviewRecord),
    // What Envoy accepted on this run's topics, with the boot line of each half: the
    // implementer's wake of the reviewer, then the reviewer's wake of the architect. A run
    // that sent one of them elsewhere, or sent none, names its own record here.
    "published.jsonl": bus(toldReviewerOfPush(fix.fixed), toldArchitectOfHead(fix.fixed)),
    ...files,
  };
  run(name, world, transcripts.implementer ?? [...header, ...turn]);
  // The wake the rig would have handed the reviewer: the messages of this run's record on the
  // reviewer's topic, which is what `reviewer_wake` prints for it, read by the topic alone as
  // the score reads them.
  const woken = (world["published.jsonl"] ?? "")
    .split("\n")
    .filter((line) => line !== "")
    .some((line) => {
      const sent: unknown = JSON.parse(line);
      if (typeof sent !== "object" || sent === null || !("topic" in sent)) return false;
      return typeof sent.topic === "string" && sent.topic.endsWith("-reviewer");
    });
  if (transcripts.reviewer === null) return;
  if (transcripts.reviewer === undefined && world["case.txt"] === "code-defect\n" && !woken) return;
  stagings.push(() => {
    const dir = path.join(runs, name, "sessions", "reviewer");
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, "2026-09-30T15-51-17-651Z_01b1f3dd.jsonl"),
      (transcripts.reviewer ?? [...secondHeader, ...turn])
        .map((entry) => `${JSON.stringify(entry)}\n`)
        .join("")
    );
  });
}
/** The pass, done above `commit`: the implementer's one reply per thread naming it, its body
 * entries at the head that commit made and no resolution of its own; then the reviewer's own
 * listing, one resolution per thread, and the second approval at that head. */
const closedAbove = (commit: string) => [
  threadReply(fixIds[0] ?? "", "2026-09-30T17:31:00.000Z", commit),
  threadReply(fixIds[1] ?? "", "2026-09-30T17:31:10.000Z", commit),
  fixBody(fixIds, "2026-09-30T17:33:00.000Z", commit),
  threadsList("2026-09-30T17:36:00.000Z"),
  threadResolve([fixIds[0] ?? ""], "2026-09-30T17:37:00.000Z"),
  threadResolve([fixIds[1] ?? ""], "2026-09-30T17:37:10.000Z"),
  secondApproval(commit),
];
/** The pass at the commit the two text fixes are in, which is the push every run below makes
 * unless it names another. */
const closedBoth = closedAbove(fix.fixed);
fixRun("threads-closed-text-fixes-head-1", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
// The implementer replied on one thread and the reviewer resolved both: the thread it never
// answered is still unanswered.
fixRun("threads-closed-text-fixes-head-2", {
  "calls.jsonl": calls(closedBoth.filter((call) => call !== closedBoth[1])),
  "pushes.log": pushed(fix.fixed),
});
// One call resolving both threads: the bulk resolution the reference forbids.
fixRun("threads-closed-text-fixes-head-3", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 4),
    threadResolve(fixIds as string[], "2026-09-30T17:37:00.000Z"),
    closedBoth[6] ?? {},
  ]),
  "pushes.log": pushed(fix.fixed),
});
// The commit carries the code with the text fixes, which the one commit of text fixes does not.
// Both wakes name the head that push made, so the run fails on the diff alone.
fixRun("threads-closed-text-fixes-head-4", {
  "calls.jsonl": calls([...closedBoth.slice(0, 6), secondApproval(fix.withCode)]),
  "pushes.log": pushed(fix.withCode),
  "published.jsonl": bus(toldReviewerOfPush(fix.withCode), toldArchitectOfHead(fix.withCode)),
});
// The body still says the threads are open, so the architect reading it through accepting would
// see nothing fixed.
fixRun("threads-closed-text-fixes-head-5", {
  "calls.jsonl": calls([
    closedBoth[0] ?? {},
    closedBoth[1] ?? {},
    fixBody(fixIds, "2026-09-30T17:33:00.000Z", fix.fixed, true),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Everything, and no completion: this loop runs in no phase and writes no handoff, so a run that
// completed nothing passes. Its two wakes name the head by its first twelve characters, which is
// how a message names a commit (`namesCommit`): a prefix of seven hexadecimal digits or more
// names the head it is a prefix of.
fixRun("threads-closed-text-fixes-head-6", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([]),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed.slice(0, 12)),
    toldArchitectOfHead(fix.fixed.slice(0, 12))
  ),
});
// The first thread resolved twice: the second call names that one thread again, and the
// reference asks only for one thread per call.
fixRun("threads-closed-text-fixes-head-7", {
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolve([fixIds[0] ?? ""], "2026-09-30T17:37:20.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// A second reply on the first thread: the reference asks for one disposition reply per thread.
fixRun("threads-closed-text-fixes-head-8", {
  "calls.jsonl": calls([
    ...closedBoth,
    threadReply(fixIds[0] ?? "", "2026-09-30T17:34:10.000Z", fix.fixed),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Two pushes of the one change: the rule gives the implementer one push of one commit, and a
// second push is a second head for the reviewer to read and the architect to see.
fixRun("threads-closed-text-fixes-head-10", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed) + pushed(fix.fixed).replace("17:35:00", "17:36:00"),
});
// A resolution of a bot thread the reviewer adjudicated, in a call of its own. That thread is
// none of the scenario's two, so it is in no threads.json. One call, one thread: no bulk
// resolution.
fixRun("threads-closed-text-fixes-head-11", {
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolve(["PRRT_kwDOrig0001"], "2026-09-30T17:37:30.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// A body that quotes the resolution command and names every thread it closed resolves nothing:
// the call that resolves a thread is the `gh api graphql` mutation, and a `gh pr edit` is not it.
fixRun("threads-closed-text-fixes-head-12", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      at: "2026-09-30T17:37:40.000Z",
      as: "gh",
      by: "reviewer",
      argv: ["pr", "edit", "1001", "--body-file", "body.md"],
      files: {
        "body.md": `Resolved with gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' for ${fixIds[0]}, ${fixIds[1]} and PRRT_kwDOrig0001.`,
      },
      exit: 0,
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Every reply and every resolution right, and no `reviewThreads` query of the reviewer's: it
// resolved threads whose state it read somewhere other than GitHub. The reference has the
// reviewer list the pull request's threads itself and verify their state there, never from the
// body, which is the implementer's record while GitHub is the fact.
fixRun("threads-closed-text-fixes-head-13", {
  "calls.jsonl": calls(closedBoth.filter((call) => call !== closedBoth[3])),
  "pushes.log": pushed(fix.fixed),
});
// The implementer resolved a thread it replied on: the resolution is the reviewer's judgment,
// and the implementer resolves none.
fixRun("threads-closed-text-fixes-head-14", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 3),
    threadResolve([fixIds[0] ?? ""], "2026-09-30T17:34:30.000Z", "implementer"),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// The second approval names the head the reviewer approved the first time, not the head the
// push made: the merge gate would read an approval of a head the fix is not on.
fixRun("threads-closed-text-fixes-head-15", {
  "calls.jsonl": calls([...closedBoth.slice(0, 6), secondApproval(fix.approved)]),
  "pushes.log": pushed(fix.fixed),
});
// Replies that name no commit: the reference has each reply name the commit that fixed it, so a
// reader of the thread reaches the fix from it.
fixRun("threads-closed-text-fixes-head-16", {
  "calls.jsonl": calls([
    threadReply(fixIds[0] ?? "", "2026-09-30T17:31:00.000Z"),
    threadReply(fixIds[1] ?? "", "2026-09-30T17:31:10.000Z"),
    ...closedBoth.slice(2),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// The reviewer half never started: the run holds the implementer's work and nothing of the
// reviewer's, either because no transcript was written (head-17) or because the one written
// carries no assistant message (head-18). Each would otherwise be a failure of the text under
// test, and neither measured the loop at all.
fixRun(
  "threads-closed-text-fixes-head-17",
  { "calls.jsonl": calls(closedBoth.slice(0, 3)), "pushes.log": pushed(fix.fixed) },
  { reviewer: null }
);
fixRun(
  "threads-closed-text-fixes-head-18",
  { "calls.jsonl": calls(closedBoth.slice(0, 3)), "pushes.log": pushed(fix.fixed) },
  { reviewer: [...header] }
);
// Each thread resolved by one call that also reads the mutation's answer back with --jq: one
// thread per call, and the field named twice. The run did the loop and passes.
fixRun("threads-closed-text-fixes-head-19", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 4),
    threadResolveVerified(fixIds[0] ?? "", "2026-09-30T17:37:00.000Z"),
    threadResolveVerified(fixIds[1] ?? "", "2026-09-30T17:37:10.000Z"),
    closedBoth[6] ?? {},
  ]),
  "pushes.log": pushed(fix.fixed),
});
// The whole loop right, and the issue asked back to implementing anyway. The loop exists to
// spend no round, which is what k2's claim means by "opens no new round", and the design says it
// of both cases: "Neither the implementer nor the reviewer moves the issue itself." The
// code-defect arm scores that (`moved === undefined`) and the text-fixes arm does not, so a run
// that did every act of the loop and then spent the round it was meant to save passes.
fixRun("threads-closed-text-fixes-head-20", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([
    refusedCompletion,
    movedBack("the text fixes are in, moving it back anyway"),
  ]),
});
// Every act of the loop right, and the implementer told the reviewer nothing: the loop runs in
// `accepting`, where the daemon starts no phase, so the reviewer learns the head its push left
// from that message and from nothing else.
fixRun("threads-closed-text-fixes-head-21", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(toldArchitectOfHead(fix.fixed)),
});
// Every act of the loop right, and the reviewer told the architect nothing: the architect's gate
// is that approval, and no phase ends in `accepting` to carry it.
fixRun("threads-closed-text-fixes-head-22", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(toldReviewerOfPush(fix.fixed)),
});
// Every act of the loop right, and each wake sent to the wrong role's topic: the implementer
// told the architect that the fixes are pushed (head-23), and the reviewer told the implementer
// which head it approved (head-24). Each message carries the words the rule asks for, and the
// role that waits for it reads its own topic alone, so it waits on.
fixRun("threads-closed-text-fixes-head-23", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo("architect", `The text fixes are pushed: the head the reviewer reads is ${fix.fixed}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
fixRun("threads-closed-text-fixes-head-24", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    sentTo(
      "implementer",
      `I approved the head the fix landed on, ${fix.fixed}; the architect's gate is open.`
    )
  ),
});
// What the agent's own code says it would send carries no weight: the score reads the run's
// record of what Envoy accepted. head-25's implementer wrote the reviewer's wake as an
// `envoy_publish` call and nothing reached the topic — the shape of a call the tool refused, or
// of a cell that built a message it never sent — so the reviewer never learned its turn began.
// head-26 is the other direction: the message reached the topic while the agent's transcript
// records no publish call this repository knows, which is the run the old reader failed for its
// own blindness. Measured on the batch of 2026-11-08T13:08Z, where two reviewers published from
// `eval` and read as having sent nothing, and again on 2026-11-09, where a publish read out of a
// cell scored a wake the reviewer half never had.
fixRun(
  "threads-closed-text-fixes-head-25",
  {
    "calls.jsonl": calls(closedBoth),
    "pushes.log": pushed(fix.fixed),
    "published.jsonl": bus(toldArchitectOfHead(fix.fixed)),
  },
  {
    implementer: [
      ...header,
      ...turn,
      publishCall("reviewer", `The text fixes are pushed: the head to read is ${fix.fixed}.`),
    ],
  }
);
fixRun("threads-closed-text-fixes-head-26", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads a record the recorder did not finish, which would report every
// message it missed as a message never sent — the parser's old fault in a new place. head-27's
// recorder subscribed and died before the run ended (no closing line); head-28's never
// subscribed at all, and the file holds the messages of some other reader. Each is a rig error,
// out of both counts.
fixRun("threads-closed-text-fixes-head-27", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": busLines(recordingLine(), toldReviewerOfPush(fix.fixed)),
});
fixRun("threads-closed-text-fixes-head-28", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": busLines(
    toldReviewerOfPush(fix.fixed),
    toldArchitectOfHead(fix.fixed),
    closedLine()
  ),
});
// head-32: the recorder could not read a message of one of this run's role topics and wrote it
// as `unreadable`. What reached that topic is then unknown, so the run is a rig error rather than
// a row whose wake "was not sent" — the reader's limit is never an act an agent did not do.
fixRun("threads-closed-text-fixes-head-32", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    JSON.stringify({
      at: "2026-09-30T17:41:00.000Z",
      unreadable: "notifications.role.legion-skills1-lweval-1-architect",
      subject: "notifications.agent.01a0f2cc",
    })
  ),
});
// Two messages to the reviewer's topic, which Envoy delivers as two: the harness hands the half
// both, in order, and the score reads the one the rule names among them.
fixRun("threads-closed-text-fixes-head-30", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    sentTo("reviewer", "And the body is updated.", "2026-09-30T17:43:00.000Z"),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a reader of "to that topic alone" that knows the three roles of this loop and no
// others. head-29's implementer does the whole loop and copies the tester's topic, which is a
// role topic of the run like any other; the deep pass of 2026-11-09 built exactly this run and
// the reader passed it.
fixRun("threads-closed-text-fixes-head-29", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    sentTo("tester", `For your notes: the text fixes are at ${fix.fixed}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a scorer blind to "and to that topic alone" (review-threads.md step 2). head-31's
// implementer does the whole loop and copies the architect on its push, telling it of a head no
// second approval has reached yet; the architect hears of that head from the reviewer or from
// nobody.
fixRun("threads-closed-text-fixes-head-31", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    sentTo("architect", `For your acceptance: the text fixes are at ${fix.fixed}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a scorer that reads a `git diff` the run's own remote refused as a push that carried
// no file. head-33's push log names a commit the remote does not hold, so the diff of that push
// cannot be read at all, and the empty answer a failed `git diff` prints is what a push changing
// nothing looks like: the run is a rig error naming the command, never a row scored on a diff
// nobody read.
fixRun("threads-closed-text-fixes-head-33", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed("f".repeat(40)),
});
// Catches: a scorer that reads "every thread resolved" as "one of them was". head-34's reviewer
// listed the threads and resolved the first, leaving the second open while the body says the
// fix closed it, which is the state the loop exists to end.
fixRun("threads-closed-text-fixes-head-34", {
  "calls.jsonl": calls(closedBoth.filter((call) => call !== closedBoth[5])),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads the reviewer's last submission as a verdict of any kind, so a
// `COMMENT` at the head the fix landed on — which the merge gate reads as no approval at all —
// closes the loop. head-35 did every other act and left the second approval unmade.
fixRun("threads-closed-text-fixes-head-35", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 6),
    {
      ...submission({
        commit_id: fix.fixed,
        event: "COMMENT",
        body: "The text fixes read well at this head.",
      }),
      at: "2026-09-30T17:38:00.000Z",
      by: "reviewer",
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads the reviewer's first submission, or any of them, rather than the
// one that stands. head-36 approved the head the fix landed on and then requested changes on
// that same head: the pull request's verdict is the later one, and the loop did not close.
fixRun("threads-closed-text-fixes-head-36", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      ...submission({
        commit_id: fix.fixed,
        event: "REQUEST_CHANGES",
        body: "On a second reading the trimmed name is a behaviour change after all.",
      }),
      at: "2026-09-30T17:39:00.000Z",
      by: "reviewer",
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads the role off nothing, so the implementer's own calls count as the
// reviewer's. head-37's second approval was submitted by the implementer, which is the half the
// merge gate does not take an approval from; head-38's `reviewThreads` listing is the
// implementer's, so the reviewer resolved threads whose state it never read itself.
fixRun("threads-closed-text-fixes-head-37", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 6),
    { ...secondApproval(fix.fixed), by: "implementer" },
  ]),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-text-fixes-head-38", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 3),
    threadsList("2026-09-30T17:36:00.000Z", "implementer"),
    ...closedBoth.slice(4),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads one of the three things a `Threads` entry must say and not the
// others, and one that asks them of any entry rather than of every one. An entry says the thread
// is no longer open and names the commit that closed it, at the head the push made
// (references/pr-body.md): head-39's entries name that commit and still say "still open", which
// is what the architect reads through accepting; head-40's say neither, so the reader has no way
// from the thread to the fix; head-41's first entry is right and its second names the head the
// approval was made on, which the fix is not in.
fixRun("threads-closed-text-fixes-head-39", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 2),
    bodyOf(
      "2026-09-30T17:33:00.000Z",
      fixIds.map(
        (id) => `- Thread ${id}: still open — pushed in ${fix.fixed.slice(0, 12)}, not yet read.`
      )
    ),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-text-fixes-head-40", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 2),
    bodyOf(
      "2026-09-30T17:33:00.000Z",
      fixIds.map((id) => `- Thread ${id}: fixed above — the sentence says what the CLI does.`)
    ),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-text-fixes-head-41", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 2),
    bodyOf("2026-09-30T17:33:00.000Z", [
      `- Thread ${fixIds[0]}: fixed in ${fix.fixed.slice(0, 12)} — says what the CLI does.`,
      `- Thread ${fixIds[1]}: fixed in ${fix.approved.slice(0, 12)} — says what the workflow runs on.`,
    ]),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that asks the push's diff to touch the threads' files and not to touch all
// of them. head-42's one commit fixes docs/cli.md and leaves the workflow's comment as it was,
// so a thread the reviewer is about to resolve has nothing closing it, and every other act of
// the loop was done above that commit.
fixRun("threads-closed-text-fixes-head-42", {
  "calls.jsonl": calls(closedAbove(fix.docOnly)),
  "pushes.log": pushed(fix.docOnly),
  "published.jsonl": bus(toldReviewerOfPush(fix.docOnly), toldArchitectOfHead(fix.docOnly)),
});
// Catches: a scorer that reads a reply naming several threads as each of their disposition
// replies. head-43's implementer answered both threads in one reply on the first, which leaves
// the second thread's reader with nothing on their own thread, and replied on the second as the
// rule asks.
fixRun("threads-closed-text-fixes-head-43", {
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:31:00.000Z",
      as: "gh",
      by: "implementer",
      argv: [
        "api",
        "graphql",
        "-f",
        `query=mutation{addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:"${fixIds[0]}",body:"Fixed in ${fix.fixed.slice(0, 12)}: ${fixIds[0]} and ${fixIds[1]} both say what the CLI does now."}){comment{id}}}`,
      ],
      exit: 0,
    },
    ...closedBoth.slice(1),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads "one thread per call" as the count of ids alone, or as the count
// of mutations alone. Each half of that rule has a run of its own, because the bulk call the
// reference forbids breaks both at once (head-3) and would pin neither. head-44 resolves the
// first thread with a call that invokes the mutation twice on it — one id, two resolutions;
// head-45 adds a call that resolves one bot thread and names a second in the query's own
// comment — two ids, one resolution. Both threads of the scenario are resolved in each, so the
// bulk call is the only thing either run did wrong.
fixRun("threads-closed-text-fixes-head-44", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 4),
    threadResolveTwice(fixIds[0] ?? "", "2026-09-30T17:37:00.000Z"),
    ...closedBoth.slice(5),
  ]),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-text-fixes-head-45", {
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolveNaming("PRRT_kwDOrig0001", "PRRT_kwDOrig0002", "2026-09-30T17:37:30.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that asks every thread to be resolved and stops there, so a run that
// resolved each of its own threads one call each and then closed two bot threads in one call
// passes although the reference forbids that call.
fixRun("threads-closed-text-fixes-head-46", {
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolve(["PRRT_kwDOrig0001", "PRRT_kwDOrig0002"], "2026-09-30T17:37:30.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads a call gh refused as a call that did what it asked. head-47's
// second approval exited 1 — the stand-in records the call and its exit, and the recorded runs
// of 2026-11-10 hold 249 such calls — so the pull request carries no approval of the head the
// fix landed on, whatever the agent meant to submit.
fixRun("threads-closed-text-fixes-head-47", {
  "calls.jsonl": calls([...closedBoth.slice(0, 6), { ...secondApproval(fix.fixed), exit: 1 }]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads a half's messages against a blank sender. head-48's reviewer
// transcript has no session header, so who sent each message of the record cannot be told from
// it, and a blank would match a sender Envoy never recorded: the run is a rig error naming that,
// as a reviewer-text-findings run with no header already is. The rig has left no such
// transcript — each of the 126 under the recorded runs of 2026-11-10 carries its one `session`
// header — so this is a shape the reader must answer for and not one a run of today produced.
fixRun(
  "threads-closed-text-fixes-head-48",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  { reviewer: [...turn] }
);
// Catches: a scorer that takes any hexadecimal word in a reply for the commit that fixed the
// thread. head-49's replies name four characters of the head, which names too many commits to
// be one: a reader of the thread cannot reach the fix from them, and `namesCommit` asks for
// seven digits or more, as a head is named everywhere else in this loop.
fixRun("threads-closed-text-fixes-head-49", {
  "calls.jsonl": calls([
    threadReply(fixIds[0] ?? "", "2026-09-30T17:31:00.000Z", fix.fixed.slice(0, 4)),
    threadReply(fixIds[1] ?? "", "2026-09-30T17:31:10.000Z", fix.fixed.slice(0, 4)),
    ...closedBoth.slice(2),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a check that reads the run's own agent's transcript and not the half booted after it.
// head-50's reviewer had its daemon answer refused by its own checkout's contract, so that half
// could not read its record and spent its turns on the daemon; the run says nothing about the
// loop, in whichever half the refusal fell.
fixRun(
  "threads-closed-text-fixes-head-50",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  {
    reviewer: [
      ...secondHeader,
      ...turn,
      {
        type: "message",
        timestamp: "2026-09-30T17:36:30.000Z",
        message: {
          role: "toolResult",
          toolCallId: "call-reviewer-state",
          isError: true,
          content: [
            {
              type: "text",
              text: "GET /legion/v1/state answered 200 with a body the daemon API contract does not describe: settings.max_fix_attempts: Invalid input: expected number, received undefined",
            },
          ],
        },
      },
    ],
  }
);
// Catches: a scorer that counts every line of the push log as a push onto the issue branch. The
// rule is one push of one commit onto that branch (review-threads.md step 2), and head-51's
// implementer also pushed a scratch ref of its own, which is no head the reviewer or the
// architect reads: the loop it ran is the clean one.
fixRun("threads-closed-text-fixes-head-51", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": `${pushed(fix.fixed)}2026-09-30T17:36:00.000Z refs/heads/scratch/LWEVAL-1-notes ${fix.approved} ${fix.withCode}\n`,
});
// Catches: a scorer that reads the record for the message and not for who sent it. head-52's
// message to the reviewer's topic names the new head and both threads, and the reviewer sent
// it, so the implementer told it nothing and the half that had to read the push learned of it
// from itself.
fixRun("threads-closed-text-fixes-head-52", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed, "2026-09-30T17:42:00.000Z", SECOND_SESSION),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a scorer that reads a backward move the daemon refused as a move. head-53's agent
// asked for one and the stand-in answered 409: the issue stayed where it was, the loop spent no
// round, and what the agent asked for is no act of the daemon's.
fixRun("threads-closed-text-fixes-head-53", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([
    refusedCompletion,
    { ...movedBack("the text fixes are in, moving it back anyway"), status: 409 },
  ]),
});
// Catches: a scorer that reads a message Envoy accepted from nobody as some half's. head-54's
// record holds a line with no `sender`, which the recorder writes for no message — it writes
// such a message as `unreadable` (record-publishes.ts) — so a record holding one is a record
// this cannot read, and the run is a rig error naming the line.
fixRun("threads-closed-text-fixes-head-54", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    JSON.stringify({
      at: "2026-09-30T17:42:00.000Z",
      topic: "notifications.role.legion-skills1-lweval-1-reviewer",
      message: `The text fixes are pushed: the head to read is ${fix.fixed}.`,
    }),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a scorer that knows one of the two routes a reply is made by. head-55's implementer
// replied under each thread's review comment through the REST route
// (`pulls/comments/<id>/replies`), which review-threads.md gives beside the GraphQL mutation,
// naming the thread and the commit in each: the loop it ran is the clean one.
fixRun("threads-closed-text-fixes-head-55", {
  "calls.jsonl": calls([
    restReply(fixIds[0] ?? "", 5001, "2026-09-30T17:31:00.000Z", fix.fixed),
    restReply(fixIds[1] ?? "", 5002, "2026-09-30T17:31:10.000Z", fix.fixed),
    ...closedBoth.slice(2),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads the mutation's name in any gh call as a resolution, where head-12
// reads it in a `gh pr edit`. head-56's reviewer posted a conversation comment with
// `gh api --method POST repos/example/widgets/issues/1001/comments` quoting the command it ran
// and naming both threads: a call that is no `gh api graphql` resolves nothing, and reading it
// as one would score the run a bulk resolution for saying what it did.
fixRun("threads-closed-text-fixes-head-56", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      at: "2026-09-30T17:37:40.000Z",
      as: "gh",
      by: "reviewer",
      argv: [
        "api",
        "--method",
        "POST",
        "repos/example/widgets/issues/1001/comments",
        "-f",
        `body=Resolved ${fixIds[0]} and ${fixIds[1]} with resolveReviewThread(input:{threadId:…}), one call each.`,
      ],
      exit: 0,
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads the first body the stand-in kept rather than the last. The body a
// reader of the pull request sees is the one standing at the end, and head-57's implementer
// rewrote the entries at the head its push made and then edited the body again, leaving both
// threads marked open at the head the approval was on.
fixRun("threads-closed-text-fixes-head-57", {
  "calls.jsonl": calls([
    ...closedBoth,
    fixBody(fixIds, "2026-09-30T17:39:00.000Z", fix.fixed, true),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that credits the run with the reference when one of its halves read it. Each
// half acts on its own half of the loop, so each must have read where that half is defined:
// head-58's implementer read it and its reviewer, which resolved the threads and approved the
// head, did not. The run passes, since `ref` is counted beside the pass and not in it.
fixRun(
  "threads-closed-text-fixes-head-58",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  {
    implementer: [
      ...header,
      ...turn,
      {
        type: "message",
        timestamp: "2026-09-30T17:30:00.000Z",
        message: {
          role: "assistant",
          content: [
            {
              type: "toolCall",
              id: "call-reference",
              name: "read",
              arguments: { path: "skill://legion-worker/references/review-threads.md" },
            },
          ],
        },
      },
    ],
  }
);
// Catches: a reader of the implementer's wake that asks for the new head and for nothing else.
// Step 2 has the implementer name "the new head by SHA, the threads it answered, and that the
// push's own diff changes no code that runs", and head-59's wake names the head alone, leaving
// the reviewer to work out for itself which threads that push closed. Every head wake of the
// runs recorded on 2026-11-10 named both thread ids and the compare, so this is a shape the
// reader must answer for and not one the rig has seen.
fixRun("threads-closed-text-fixes-head-59", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo("reviewer", `New head: ${fix.fixed}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a reader that asks whether each wake was sent and never when. head-60's implementer
// told the reviewer the head its push would leave five minutes before the remote took that push
// (17:30 against 17:35), so the reviewer was sent to read a head that was not there; head-61's
// reviewer told the architect of the head it approved nine months before the approval, which is
// a passing run's record with one date moved.
fixRun("threads-closed-text-fixes-head-60", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed, "2026-09-30T17:30:00.000Z"),
    toldArchitectOfHead(fix.fixed)
  ),
});
fixRun("threads-closed-text-fixes-head-61", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    toldArchitectOfHead(fix.fixed, "2026-01-01T00:00:00.000Z")
  ),
});
// Catches: times compared as text, where a message whose record carries no time at all sorts
// before everything that happened. head-62's wake of the reviewer names the head and both
// threads and carries an empty `at`: what it answers for is unknown, so the run is a rig error
// naming the time rather than a row scored on an order nobody could read.
fixRun("threads-closed-text-fixes-head-62", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(toldReviewerOfPush(fix.fixed, ""), toldArchitectOfHead(fix.fixed)),
});
// Catches: a count of the completions the stand-in accepted, which is 0 for every run of this
// loop whatever it did — both halves are booted in `accepting`, the architect's phase, where
// each of their completions is refused 409 — so the term reported nothing and the refusals it
// was named for went unread. head-63's daemon.log holds the accepted completion a reviewing
// phase's own completion gets, which no half of this loop can have, and no refusal: its row
// reads 0 where head-1's, holding the refusal the rig writes, reads 1.
fixRun("threads-closed-text-fixes-head-63", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([completed]),
});
// Catches: a reader of "one thread per call" that counts the ids a call names and lets a call
// naming none through. head-64's reviewer resolved the first thread with the id in a file
// (`-F id=@thread-id.txt`, which gh reads the value of from that file), so the call shows no
// one thread of its own: what it closed cannot be read from it, which is the state the rule's
// one thread per call exists to leave behind. head-3's bulk call names two and breaks the other
// half of the rule at the same time.
fixRun("threads-closed-text-fixes-head-64", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      at: "2026-09-30T17:37:30.000Z",
      as: "gh",
      by: "reviewer",
      argv: [
        "api",
        "graphql",
        "-f",
        "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
        "-F",
        "id=@thread-id.txt",
      ],
      exit: 0,
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-65, head-66 and head-67: the run that passes as head-1 does, each missing one of the
// three records a worker run leaves. Catches: the missing-record rule read as the other
// scenario's alone — every run of this one holds all three, so a rule that skipped
// threads-closed would be pinned by nothing here. Each record is written by the thing that
// keeps it, so an absent one is a record nothing wrote, and it reads as no call, no push and no
// move, which is what the code-defect pass looks like.
for (const [name, file] of [
  ["head-65", "calls.jsonl"],
  ["head-66", "pushes.log"],
  ["head-67", "daemon.log"],
] as const)
  fixRun(`threads-closed-text-fixes-${name}`, {
    "calls.jsonl": calls(closedBoth),
    "pushes.log": pushed(fix.fixed),
    [file]: null,
  });
// head-68: the three records there and holding no line of the run's own — each stand-in's
// opening line, and the push log rig.sh empties — which is the other reading: the run made no
// call, pushed nothing and moved nothing. So it scores, its row names each of them, and it
// fails on every act of the loop.
fixRun("threads-closed-text-fixes-head-68", {
  "calls.jsonl": openedRecord("calls.jsonl"),
  "pushes.log": "",
  "daemon.log": openedRecord("daemon.log"),
});
// head-69 and head-70: the record of what the run's agents sent, there and holding no line
// (head-69) and absent altogether (head-70). Catches: a reader that takes either for a run that
// published nothing. The recorder writes its opening line as soon as it has subscribed, so a
// file without one is a recorder that never did: what reached the topics is unknown, and both
// wakes of this loop would otherwise read as messages never sent. head-28 is the third shape, a
// record with messages in it and no opening line.
fixRun("threads-closed-text-fixes-head-69", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": "",
});
fixRun("threads-closed-text-fixes-head-70", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": null,
});
// head-71: the whole loop, and a `COMMENT` review of the reviewer's after its second approval —
// the compare it read, posted as a review of its own, which on GitHub leaves the approval
// standing. The verdict is the round's last `APPROVE` or `REQUEST_CHANGES`, so the run passes;
// a reader of the reviewer's last submission of any kind scored this `approved=false` and
// failed a run that did every act of the loop.
fixRun("threads-closed-text-fixes-head-71", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      ...submission({
        commit_id: fix.fixed,
        event: "COMMENT",
        body: "The compare of the approved head against this one changes no code that runs.",
      }),
      at: "2026-09-30T17:39:00.000Z",
      by: "reviewer",
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-72: the implementer half died in its claim boot. It registered with the daemon, could not
// finish the boot, and the pane exited 1 (the line is the claim boot's own,
// `packages/pi-shared/src/legion-claim-core.ts`), so the one reply it had made and the push it
// never made answer nothing the loop asks of it. Six of the twelve runs of 2026-11-11 ended that
// way and were scored as failures of the text under test. Its own output is `out-implementer.txt`,
// where rig.sh's worker_boot moves it when the reviewer half boots.
fixRun("threads-closed-text-fixes-head-72", {
  "calls.jsonl": calls(closedBoth.slice(0, 1)),
  "out-implementer.txt":
    "[legion] claim boot failed at claimRole after claims/register registered legion-skills1-lweval-1-implementer with the daemon at http://127.0.0.1:38411; exiting so the daemon launches it again: ECONNREFUSED\nexit=1\n",
  // A half that died sent nothing: the record is the recorder's own two lines and no message.
  "published.jsonl": bus(),
  // Two requests, so the row must name the later of them: the sentence says when the run last
  // reached its daemon, which is where a reader of the rig looks next, and the first request of
  // a run that died in its boot is the register every half makes.
  "daemon.log": calls([
    { at: "2026-09-30T17:30:01.000Z", path: "/legion/v1/claims/register", status: 200 },
    { at: "2026-09-30T17:30:05.000Z", path: "/legion/v1/claims/ready", status: 204 },
  ]),
});
// head-73 and head-74: the loop done above the head the push made, with every name of that head
// written as its first seven characters (head-73) and as its first six (head-74) — the two
// replies, the two body entries, the implementer's wake of the reviewer and the reviewer's wake
// of the architect. Seven hexadecimal digits are the fewest that name one commit, which is what
// `namesCommit` asks of a reply, a body entry and a wake, so head-73 passes and head-74 names
// no commit anywhere and fails on each of the four. Catches: that floor moved either way — at
// eight, a run naming the head by git's own default short SHA would read as a run that named no
// head and fail every act of the loop; at six, a run leaving a reader too many commits to find
// the fix would pass. The second approval names the whole head in both, since a verdict's head
// is read by `namesHead` and a six-character `commit_id` would fail that under every reading,
// which would leave head-74 failing whatever this floor is. head-49's four characters are short
// under both floors and pin neither.
for (const [name, length] of [
  ["head-73", 7],
  ["head-74", 6],
] as const) {
  const named = fix.fixed.slice(0, length);
  fixRun(`threads-closed-text-fixes-${name}`, {
    "calls.jsonl": calls([
      threadReply(fixIds[0] ?? "", "2026-09-30T17:31:00.000Z", named),
      threadReply(fixIds[1] ?? "", "2026-09-30T17:31:10.000Z", named),
      fixBody(fixIds, "2026-09-30T17:33:00.000Z", named),
      threadsList("2026-09-30T17:36:00.000Z"),
      threadResolve([fixIds[0] ?? ""], "2026-09-30T17:37:00.000Z"),
      threadResolve([fixIds[1] ?? ""], "2026-09-30T17:37:10.000Z"),
      secondApproval(fix.fixed),
    ]),
    "pushes.log": pushed(fix.fixed),
    "published.jsonl": bus(toldReviewerOfPush(named), toldArchitectOfHead(named)),
  });
}
// head-75: the loop done above the head the push made, with each reply naming ten characters
// taken from the middle of that head and everything else naming the head itself. Catches: a
// commit named by any run of characters the head holds, where a reply pointing at a commit this
// branch has not got would read as the reply the rule asks for. A reply names the fix by a
// prefix of the head (`namesCommit`), which is what sends its thread's reader to the commit.
fixRun("threads-closed-text-fixes-head-75", {
  "calls.jsonl": calls([
    threadReply(fixIds[0] ?? "", "2026-09-30T17:31:00.000Z", fix.fixed.slice(3, 13)),
    threadReply(fixIds[1] ?? "", "2026-09-30T17:31:10.000Z", fix.fixed.slice(3, 13)),
    ...closedBoth.slice(2),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-76 and head-77: the loop head-1 passes on, with the reviewer reading the reviews already
// on the pull request after its approval — the query's parameters written as fields, which gh
// sends as a GET, in the two spellings of the verb this loop had no row for: `-X GET`
// (head-76) and `--method get` (head-77). Catches: a verb read in one spelling or one case
// here, where that read becomes a submission of the round carrying no event: the row would
// name a submission the pull request never got beside the approval that stands, and a reader
// that took the round's last submission for its verdict would score a reviewer that approved
// the fix head and then read the reviews as one that approved nothing. The reviewer scenario's
// head-73, head-100 and head-101 are the same reads on the other side of this file.
for (const [name, verb] of [
  ["head-76", ["-X", "GET"]],
  ["head-77", ["--method", "get"]],
] as const)
  fixRun(`threads-closed-text-fixes-${name}`, {
    "calls.jsonl": calls([
      ...closedBoth,
      {
        at: "2026-09-30T17:39:00.000Z",
        as: "gh",
        by: "reviewer",
        argv: ["api", "repos/example/widgets/pulls/1001/reviews", ...verb, "-f", "per_page=100"],
        exit: 0,
      },
    ]),
    "pushes.log": pushed(fix.fixed),
  });
// head-78: the loop head-1 passes on, with a tag of the issue's own name pushed beside the one
// commit — `refs/tags/legion/LWEVAL-1`, which carries the branch's whole name inside its ref.
// Catches: a push log read for the branch's name anywhere in a ref rather than for the ref
// `refs/heads/<branch>` itself, which counts that tag as a second push onto the issue branch
// and fails a run whose loop was the clean one — and, where the tag is the last line, reads
// the head the reviewer and the architect must see off a ref neither of them reads. head-51's
// scratch ref is the other half of this: a ref that carries the key and not the branch.
fixRun("threads-closed-text-fixes-head-78", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": `${pushed(fix.fixed)}2026-09-30T17:36:00.000Z refs/tags/legion/LWEVAL-1 ${"0".repeat(40)} ${fix.withCode}\n`,
});
// head-79: the loop head-1 passes on, with each thread named on two lines of the kept body —
// the line the approval left above the entry the implementer wrote, which is the body a reader
// of the pull request ends on. Catches: a reader that takes the first line naming a thread for
// its entry, which scores a body whose last word on the thread is "fixed in <tip>" as one that
// still marks it open.
fixRun("threads-closed-text-fixes-head-79", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 2),
    bodyOf(
      "2026-09-30T17:33:00.000Z",
      fixIds.flatMap((id) => [
        `- Thread ${id}: was still open at ${fix.approved.slice(0, 12)} — the sentence the approval found.`,
        `- Thread ${id}: fixed in ${fix.fixed.slice(0, 12)} — says what the CLI does; unresolved at ${fix.fixed.slice(0, 12)}.`,
      ])
    ),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-80: head-39's body with "Still Open" written in capitals, which is how an entry reads
// when the agent title-cases its own table. Catches: a reader that tests the entry for the
// lower-case words alone, which passes a body telling the architect through accepting that
// both threads are open at the head the fix landed on.
fixRun("threads-closed-text-fixes-head-80", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 2),
    bodyOf(
      "2026-09-30T17:33:00.000Z",
      fixIds.map(
        (id) => `- Thread ${id}: Still Open — pushed in ${fix.fixed.slice(0, 12)}, not yet read.`
      )
    ),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-81: the implementer's wake names both threads the push answered and no head at all.
// Catches: a reader that asks for any one of the facts step 2 names rather than all of them in
// one message, which passes a wake the reviewer cannot act on: Envoy hands each message on its
// own, so a reviewer told which threads were answered and not which head to read has the whole
// branch to search. head-59 is the same wake with the head alone and the threads left out.
fixRun("threads-closed-text-fixes-head-81", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo("reviewer", `The text fixes are pushed, closing ${fixIds[0]} and ${fixIds[1]}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
// head-82: the reviewer approved the head the approval before it was on, told the architect of
// the head the fix landed on, and only then approved that head. Catches: a reader that asks
// whether the architect was told and never when, which passes a wake reporting an approval
// nobody had made yet — the architect's gate is the approval READY reads, and a message sent
// before it is a message about a verdict the pull request did not carry.
fixRun("threads-closed-text-fixes-head-82", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 6),
    secondApproval(fix.approved, "2026-09-30T17:37:30.000Z"),
    secondApproval(fix.fixed),
  ]),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    toldArchitectOfHead(fix.fixed, "2026-09-30T17:37:45.000Z")
  ),
});
// head-83: the whole loop, with the implementer posting a `COMMENT` review of its own after the
// reviewer's wake of the architect — the compare it was asked to show, left on the pull request
// as a review. Catches: a reader that orders that wake against the round's last submission of
// any kind rather than against the reviewer's own approval, which fails a run whose architect
// was told the moment the approval stood because the other half spoke after it. head-71 is the
// reviewer's own `COMMENT` after its approval, which leaves the verdict standing.
fixRun("threads-closed-text-fixes-head-83", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      ...submission({
        commit_id: fix.fixed,
        event: "COMMENT",
        body: "The compare of the approved head against this one changes no code that runs.",
      }),
      at: "2026-09-30T17:43:00.000Z",
      by: "implementer",
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-84: the loop head-1 passes on, with each half reading `skill://legion-worker/SKILL.md`
// and nothing else. Catches: a `ref` column that counts a read of the skill as a read of the
// reference, where the loop, the one reply per thread and the one thread per resolving call are
// defined: the skill names that file and states none of it, so a run that read only the skill
// read nowhere the rule is. The run passes, since `ref` is counted beside the pass.
fixRun(
  "threads-closed-text-fixes-head-84",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  {
    implementer: [...header, ...turn, ...readOf("call-skill", "skill://legion-worker/SKILL.md")],
    reviewer: [...secondHeader, ...turn, ...readOf("call-skill", "skill://legion-worker/SKILL.md")],
  }
);
// head-85: the whole loop, and a pull-request comment of the reviewer's that says what it ran,
// quoting `gh api graphql` and the mutation and naming both threads. Catches: a reader that
// takes any call whose text names graphql and the mutation for a resolution, where the call
// that resolves a thread is `gh api graphql` and a `gh pr comment` is not it: the quoted
// command names two threads, so the run would score a bulk resolution for saying what it did.
// head-12's body quotes the mutation without the word graphql in it.
fixRun("threads-closed-text-fixes-head-85", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      at: "2026-09-30T17:39:00.000Z",
      as: "gh",
      by: "reviewer",
      argv: [
        "pr",
        "comment",
        "1001",
        "--body",
        `Both threads are closed: for each I ran gh api graphql -f query='mutation{resolveReviewThread(input:{threadId:"${fixIds[0]}"}){thread{id}}}', and the same for ${fixIds[1]}.`,
      ],
      exit: 0,
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-87: the whole loop, and a pull-request comment of the reviewer's that carries the word
// `graphql` as an argument of its own — a heading of the quoted command — with the mutation in
// the body beside it. head-85 puts that word inside the `--body` string, where whole-argument
// matching never finds it, so this is the row that reaches the check the resolution is read by:
// a resolution is `gh api graphql` and nothing else. Catches: that check dropped, which would
// score this comment a third resolution of a loop that made two.
fixRun("threads-closed-text-fixes-head-87", {
  "calls.jsonl": calls([
    ...closedBoth,
    {
      at: "2026-09-30T17:39:00.000Z",
      as: "gh",
      by: "reviewer",
      argv: [
        "pr",
        "comment",
        "1001",
        "graphql",
        "--body",
        `Both threads are closed: resolveReviewThread(input:{threadId:"${fixIds[0]}"}).`,
      ],
      exit: 0,
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-86: the loop head-1 passes on, with a thread whose node id carries an underscore and a
// hyphen, as GitHub's base64url ids do. Catches: a thread-id pattern that stops at the first
// such character, which reads `PRRT_kwDO_ab-1` as `PRRT_kwDO`: the resolving call then names a
// thread of no scenario, the run scores none of that thread's resolutions, and a reviewer that
// closed every thread fails.
fixRun("threads-closed-text-fixes-head-86", {
  "threads.json": oddThreadsFile,
  "calls.jsonl": calls([
    threadReply(oddIds[0] ?? "", "2026-09-30T17:31:00.000Z", fix.fixed),
    threadReply(oddIds[1] ?? "", "2026-09-30T17:31:10.000Z", fix.fixed),
    fixBody(oddIds, "2026-09-30T17:33:00.000Z", fix.fixed),
    threadsList("2026-09-30T17:36:00.000Z"),
    threadResolve([oddIds[0] ?? ""], "2026-09-30T17:37:00.000Z"),
    threadResolve([oddIds[1] ?? ""], "2026-09-30T17:37:10.000Z"),
    secondApproval(fix.fixed),
  ]),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo(
      "reviewer",
      `The text fixes are pushed: the head to read is ${fix.fixed}, closing ${oddIds[0]} and ${oddIds[1]}.`
    ),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Another label, for the counts.
fixRun("threads-closed-text-fixes-base-1", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
// code-defect: the thread no text fix closes. The pass is no commit, nothing resolved, no second
// approval, and the message to the architect, which the implementer's transcript records. This
// world pushes nothing, so neither wake of the text-fixes loop belongs in it: each run below
// states every message its two halves sent.
fixRun("threads-closed-code-defect-head-1", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z"),
  ]),
  "published.jsonl": bus(
    sentTo(
      "architect",
      "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong, so no text fix closes it."
    )
  ),
});
// Wrote the text over the defect and pushed it: the negative control's own failure. Catches: a
// scorer that reads the message to the architect and not what the run did beside it, since a
// thread whose sentence is false only because the code is wrong is closed by no text, however
// well the architect was told. head-2 did every other act of the case — it told the architect,
// resolved nothing, approved nothing and moved nothing — so the push is the one thing it did
// wrong, and the runs below are the same for each of the other terms.
fixRun("threads-closed-code-defect-head-2", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z", fix.fixed)]),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo(
      "architect",
      "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: I wrote the sentence to match the code anyway."
    )
  ),
});
// The message to the architect, and then the role moved the issue itself: the architect moves it
// back to implementing, and neither the implementer nor the reviewer does.
fixRun("threads-closed-code-defect-head-3", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z")]),
  "daemon.log": calls([
    movedBack(
      "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong."
    ),
  ]),
  "published.jsonl": bus(
    sentTo("architect", "Thread PRRT_kwDOfix101 is false because the code is wrong.")
  ),
});
/** The implementer's own message to the reviewer in code-defect, which is the only thing that
 * gives that case a second half: rig.sh boots the reviewer on the messages the implementer
 * published to its topic and boots none where it published none (reviewer_wake). The act the
 * case measures is sending the architect the finding, so a run whose reviewer did anything at
 * all holds this message too, and a run that holds it has not thereby told the architect
 * anything — the reviewer can do nothing with a thread no text fix closes. */
const askedReviewer = sentTo(
  "reviewer",
  "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: nothing I can write closes it. Will you read it?",
  "2026-09-30T17:32:00.000Z"
);
// The reviewer told nobody on Envoy and submitted the `REQUEST_CHANGES` instead. It is submitted
// in `accepting`, where the daemon records no review, so it moves nothing and the finding sits
// where no role reads it. The implementer's message to the reviewer is what the rig booted that
// half on, and it reaches no architect.
fixRun("threads-closed-code-defect-head-5", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z"), changesRequested(fix.approved)]),
  "daemon.log": calls([]),
  "published.jsonl": bus(askedReviewer),
});
// The same `REQUEST_CHANGES`, and the message to the architect beside it: the submission is the
// finding's record on GitHub and the message is what reaches the role that can act on it.
fixRun("threads-closed-code-defect-head-7", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z"), changesRequested(fix.approved)]),
  "daemon.log": calls([]),
  "published.jsonl": bus(
    askedReviewer,
    sentTo(
      "architect",
      "Thread PRRT_kwDOfix101 is false because the code is wrong: a blank name must exit 2.",
      "2026-09-30T17:42:00.000Z",
      SECOND_SESSION
    )
  ),
});
// The reviewer approved the head instead: the one verdict the design forbids while a finding is
// not text, whether or not it also told the architect.
fixRun("threads-closed-code-defect-head-6", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z"), secondApproval(fix.approved)]),
  "daemon.log": calls([]),
  "published.jsonl": bus(
    askedReviewer,
    sentTo("architect", "Thread PRRT_kwDOfix101 is false because the code is wrong.")
  ),
});
// The finding named, and sent to the reviewer's topic rather than the architect's: the
// architect is the one role that can move the issue for it, and the reviewer, which may not
// approve this head either, can do nothing with the message.
fixRun("threads-closed-code-defect-head-8", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z"),
  ]),
  "published.jsonl": bus(
    sentTo(
      "reviewer",
      "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong, so no text fix closes it."
    )
  ),
});
// The half the run never booted. In code-defect the implementer's own act is to tell the
// architect and send the reviewer nothing, and rig.sh boots no second agent on a wake of none
// (run_threads_closed), so the run has one half and no sessions/reviewer at all. The text-fixes
// runs above (head-17, head-18) are the same shape in the case where the wake was owed, and stay
// rig errors.
fixRun(
  "threads-closed-code-defect-head-10",
  {
    "case.txt": "code-defect\n",
    "threads.json": defectThreadsFile,
    "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z", "implementer")]),
    "published.jsonl": bus(
      sentTo(
        "architect",
        "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong, so no text fix closes it."
      )
    ),
  },
  { reviewer: null }
);
// A thread no text fix closes is resolved by nobody: it stays open until the code is right, and
// the arm's two resolution terms — the implementer resolved none (`fixResolves === 0`) and the
// reviewer made no resolving call at all (`resolves.length === 0`) — are each broken by a run
// below, which is what tells them apart. The reviewer resolved the thread (head-11), it made a
// call that resolves nothing of the scenario's and names two threads of the pull request
// (head-13), and the implementer, which resolves none in either case, resolved it (head-12).
// Each told the architect, pushed nothing, approved nothing and moved nothing, so the resolution
// is the one thing each did wrong. `resolved.size === 0` is no term of this arm, and head-13 is
// why: every resolution it counts is one of `resolves`, so a term written that way would pass a
// call that resolves a thread of the pull request that is none of the scenario's.
const defectTold = bus(
  sentTo(
    "architect",
    "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong, so no text fix closes it."
  )
);
/** The same record with the implementer's message to the reviewer in it, which is what the rig
 * booted a reviewer on: the two runs below whose reviewer acted hold it, and head-12, whose
 * acts are all the implementer's, has one half and no such message. */
const defectWokeReviewer = bus(
  askedReviewer,
  sentTo(
    "architect",
    "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong, so no text fix closes it."
  )
);
fixRun("threads-closed-code-defect-head-11", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z"),
    threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z"),
    threadResolve(["PRRT_kwDOfix101"], "2026-09-30T17:37:00.000Z"),
  ]),
  "published.jsonl": defectWokeReviewer,
});
fixRun("threads-closed-code-defect-head-12", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z"),
    threadResolve(["PRRT_kwDOfix101"], "2026-09-30T17:37:00.000Z", "implementer"),
  ]),
  "published.jsonl": defectTold,
});
fixRun("threads-closed-code-defect-head-13", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z"),
    threadResolve(["PRRT_kwDOrig0001", "PRRT_kwDOrig0002"], "2026-09-30T17:37:00.000Z"),
  ]),
  "published.jsonl": defectWokeReviewer,
});
// Catches: a scorer that reads the verdict off the reviewer's calls alone, so the approval the
// design forbids while a finding is not text passes when the implementer submitted it. head-14
// told the architect and did every other act of the case; the approval on the pull request is
// an approval of this head whichever half made it.
fixRun("threads-closed-code-defect-head-14", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    { ...secondApproval(fix.approved), by: "implementer" },
  ]),
  "published.jsonl": defectTold,
});
// Catches: a scorer that reads the message to the architect for the thread's node id alone. The
// finding is the thread's, and head-15 named it by the file its sentence is on, which is what
// the architect acts on and what a message about a document naturally says.
fixRun("threads-closed-code-defect-head-15", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z", "implementer")]),
  "published.jsonl": bus(
    sentTo(
      "architect",
      "The sentence in docs/cli.md is false because the code is wrong: a blank name must exit 2, and the CLI greets."
    )
  ),
});
// Catches: a reader that lets a code-defect run off the half rule altogether, rather than off
// the half the rig never booted. head-16's implementer woke the reviewer, so the rig booted one,
// and that half got no model turn: it measured nothing of the loop, as head-18 measures nothing
// of the text-fixes one, and the run is the rig error that says so. The case excuses the absent
// directory and nothing else.
fixRun(
  "threads-closed-code-defect-head-16",
  {
    "case.txt": "code-defect\n",
    "threads.json": defectThreadsFile,
    "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z", "implementer")]),
    "published.jsonl": defectWokeReviewer,
  },
  { reviewer: [...secondHeader] }
);
// Catches: a `ref` that asks the half the run never booted to have read the reference, which
// reads as a run that read it in neither half. head-17 is head-10 with the implementer's read of
// review-threads.md in it: the one half that ran read where its own half of the loop is defined,
// and there is no second half to ask. Reverting this to the two-half reading moved the recorded
// code-defect label from four runs with the reference to none, since every code-defect run that
// did the act the case measures has one half.
fixRun(
  "threads-closed-code-defect-head-17",
  {
    "case.txt": "code-defect\n",
    "threads.json": defectThreadsFile,
    "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z", "implementer")]),
    "published.jsonl": defectTold,
  },
  {
    implementer: [
      ...header,
      ...turn,
      ...readOf("call-reference", "skill://legion-worker/references/review-threads.md"),
    ],
  }
);
// head-18: the case's own pass, with the implementer pushing a tag of the issue's name and no
// commit — `refs/tags/legion/LWEVAL-1`, which holds the branch's whole name inside its ref.
// The case turns on the branch carrying no commit of this round, and a tag is no head the
// reviewer or the architect reads. Catches: a push log read for the branch's name anywhere in
// a ref, which scores this run as the text fix the case exists to fail.
fixRun("threads-closed-code-defect-head-18", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z"),
  ]),
  "pushes.log": `2026-09-30T17:35:00.000Z refs/tags/legion/LWEVAL-1 ${"0".repeat(40)} ${fix.approved}\n`,
  "published.jsonl": defectTold,
});
// The rig's two records of the case disagree, and a directory that names no case at all.
fixRun("threads-closed-code-defect-head-4", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-head-9", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});

/** What `score.ts runs` printed over the staged world, and the code it exited with. Both are
 *  read in a hook, below the one that stages that world. */
let out = "";
let scoredExitCode: number | null = null;
// Bun's default hook bound is 5s, and this one runs the whole scorer over every run of the file:
// three audit runs and two pods saw it killed at that bound, which reads as "0 pass, a
// beforeEach/afterEach hook timed out" and says nothing about the scorer. 120s is this file's
// own staging bound.
beforeAll(() => {
  const scored = Bun.spawnSync(["bun", path.join(import.meta.dir, "score.ts"), "runs", runs]);
  out = scored.stdout.toString();
  scoredExitCode = scored.exitCode;
}, 120_000);

/** The summary line `score.ts runs` prints for one scenario key and label, built from the run
 * rows of that same output: `<scenario>\t<label>\t<passed>/<scored>\t<with ref>/<scored>\t<rig
 * errors>\t<flagged>`. The counts are what the summary is for — the table read against the rows
 * it aggregates — and the alternative is a line written by hand that every fixture added to this
 * file makes wrong. `flagged` is no part of a run row and no scenario summarised this way flags
 * one, so it is 0 here. */
function summaryLine(scenario: string, label: string): string {
  const prefix = `${scenario.replace(":", "-")}-${label}-`;
  const rows = out.split("\n").filter((line) => line.startsWith(prefix));
  const scored = rows.filter((line) => line.includes("\tpass="));
  const passed = scored.filter((line) => line.includes("\tpass=true\t"));
  const withRef = passed.filter((line) => line.includes("\tref=true\t"));
  const errors = rows.length - scored.length;
  return `${scenario}\t${label}\t${passed.length}/${scored.length}\t${withRef.length}/${scored.length}\t${errors}\t0\n`;
}

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

// Catches: a scorer that reads the pull request's state, which the stand-in's static routes
// cannot show, instead of the submission the round made.
test("reviewer-text-findings, text-only: an approval of the head carrying both findings passes", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-1\tpass=true\tref=false\tcase=text-only event=APPROVE head=true doc=true workflow=true toldImplementer=true submissions=1 resolveThread=0 pushes=0 reviewRecords=1/
  );
  expect(out).toContain('commentPaths=["docs/cli.md",".github/workflows/greet.yml"]');
});

// Catches: a scorer that scores the verdict alone, so a round that approves the head and tells
// the implementer nothing passes. The loop runs in `accepting`, where the daemon starts no
// phase and records no review (`packages/daemon/internal/workflow/review.go` returns before it
// records one outside `reviewing`), so the implementer learns its turn began from the
// reviewer's message and from nothing else, and a round that sends none leaves the two text
// threads open with nobody fixing them. And: a scorer that reads the role out of the call's
// whole text rather than the topic it published to, so a message to the architect that names
// the implementer and the head counts as the implementer's wake, which no implementer reads.
test("reviewer-text-findings, text-only: an approval that wakes no implementer fails, and one that wakes the wrong topic", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-10\tpass=false\t.*workflow=true toldImplementer=false /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-11\tpass=false\t.*workflow=true toldImplementer=false /
  );
});

// Catches: a pass given to an approval that dropped a finding rather than writing it as a thread.
test("reviewer-text-findings, text-only: an approval that leaves a finding uncommented fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-2\tpass=false\t.*doc=true workflow=false /
  );
});

// Catches: the rule's own failure, a text finding sent back as a change request.
test("reviewer-text-findings, text-only: REQUEST_CHANGES on a text finding fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-3\tpass=false\t.*event=REQUEST_CHANGES /
  );
});

// Catches: a scorer blind to a thread resolution GitHub refuses the review App, or to a push a
// review round must not make, or to an approval that names another head than the one it read.
test("reviewer-text-findings, text-only: a push and another head each fail, and a resolution is reported", () => {
  // The round's own findings are new inline comments, so it can resolve none of them; the one
  // thread the world holds open is the bot's, which the reviewer adjudicates and may resolve
  // (review-threads.md). So the call is reported and scores nothing, and four live runs of
  // 2026-11-06 that approved the head and resolved exactly that thread are passes.
  expect(out).toMatch(/reviewer-text-findings-text-only-head-4\tpass=true\t.*resolveThread=1 /);
  expect(out).toMatch(/reviewer-text-findings-text-only-head-7\tpass=false\t.*pushes=1 /);
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-5\tpass=false\t.*event=APPROVE head=false /
  );
});

// Catches: the pushes of a review round counted on the issue branch alone. head-110 approved the
// head and said so, and pushed once to `refs/heads/scratch`: a review round pushes nothing at
// all, so the ref the push went to is no part of the reading, and head-7's push to the branch
// is the same failure on the ref the reviewer reads.
test("reviewer-text-findings, text-only: a push to another ref is a push of the round", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-110\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*pushes=1 /
  );
});

// Catches: a scorer that reads the resolution out of the call's arguments alone, so one sent
// through `--input` or a here-document goes uncounted and the row says a round resolved nothing
// when it resolved a thread. The count is reported rather than scored, and it must be right.
test("reviewer-text-findings, text-only: a resolveReviewThread in the call's body is counted", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-8\tpass=true\t.*resolveThread=1 /);
});

// Catches: a scorer that reads only `gh api`, so a `gh pr review` verdict goes unseen.
test("reviewer-text-findings: a gh pr review carries its event in a flag, and no head or comment", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-6\tpass=false\t.*event=APPROVE head=false doc=false workflow=false /
  );
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-1\tpass=true\t.*event=REQUEST_CHANGES head=false /
  );
});

// Catches: a scorer that passes any verdict, so the negative control measures nothing.
test("reviewer-text-findings, defect: an approval fails, and the counts are per case and label", () => {
  expect(out).toMatch(/reviewer-text-findings-defect-head-2\tpass=false\t.*event=APPROVE /);
  for (const [scenario, label] of [
    ["reviewer-text-findings:text-only", "head"],
    ["reviewer-text-findings:text-only", "base"],
    ["reviewer-text-findings:defect", "head"],
  ] as const)
    expect(out).toContain(summaryLine(scenario, label));
});

// Catches: a rig whose two records of the case can disagree without being caught, which would
// score one case's run under another's rule.
test("reviewer-text-findings: a case.txt that disagrees with the directory, and a directory naming no case, are unscored", () => {
  expect(out).toContain(
    "reviewer-text-findings-defect-head-3\trig error: case.txt names 'text-only', but the run's directory names defect"
  );
  expect(out).toContain(
    "reviewer-text-findings-head-9\trig error: the directory names no case: expected reviewer-text-findings-<case>-<label>-<n>"
  );
});

// Catches: a scorer that reads the pull request's thread state, which the stand-in's static
// routes serve unresolved whatever the run did, instead of the calls, the push and the body the
// run left; and one that scores one half of the loop and not the other.
test("threads-closed, text-fixes: the fix and its push, then the resolutions and the second approval, pass", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-1\tpass=true\tref=false\tcase=text-fixes threads=2 replied=2 replyCalls=2 repliedCommit=2 fixResolves=0 pushes=1 fixed=true threadsLine=true toldReviewer=true aloneToReviewer=true listed=true resolved=2 resolveCalls=2 bulk=false approved=true toldArchitectOfHead=true requested=false approvedAny=true told=false moved=none refusedCompletions=1/
  );
  expect(out).toContain('changed=[".github/workflows/greet.yml","docs/cli.md"]');
});

// Catches: a scorer that counts resolutions and never reads the replies, or one blind to the
// bulk call review-threads.md forbids.
test("threads-closed, text-fixes: a thread left unanswered, and a call resolving both, each fail", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-2\tpass=false\t.*replied=1 replyCalls=1 /);
  expect(out).toMatch(
    /threads-closed-text-fixes-head-3\tpass=false\t.*resolved=0 resolveCalls=1 bulk=true /
  );
});

// Catches: a scorer that counts the string `resolveReviewThread` in a call's whole text, so a
// `--jq` selector reading the mutation's own answer back doubles the count and fails a run for
// verifying its own write. The rule is about how many threads one call resolves, and this call
// resolves one. Measured on threads-closed-text-fixes-head-3 of 2026-11-07, which resolved both
// threads one call each and scored bulk=true.
test("threads-closed, text-fixes: a resolve call that reads its own answer back is no bulk call", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-19\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a scorer that scores every act of the loop and not the thing the loop is for. k2's
// claim is that the implementer "opens no new round", and review-threads.md says of this loop
// "Neither the implementer nor the reviewer moves the issue itself". The code-defect arm scores
// it and the text-fixes arm did not, so a run that replied, fixed, pushed, resolved, approved
// and then asked the daemon to move the issue back passed as a clean loop.
test("threads-closed, text-fixes: a run that spends the round it was meant to save fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-1\tpass=true\t.*moved=none /);
  expect(out).toMatch(
    /threads-closed-text-fixes-head-20\tpass=false\t.*moved=implementing refusedCompletions=1/
  );
});

// Catches: a scorer that takes any push for the text fixes, so a commit carrying code passes.
test("threads-closed, text-fixes: a push that carries the code with the fixes fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-4\tpass=false\t.*pushes=1 fixed=false /);
  expect(out).toContain('changed=[".github/workflows/greet.yml","docs/cli.md","greet.ts"]');
});

// Catches: a scorer blind to the body the architect reads through accepting; one that holds
// this loop to a completion, which it asks of neither role, since it runs in no phase and writes
// no handoff; and one that holds a wake to the head's whole forty characters, so the two
// messages of a run whose roles named it as a reply names a commit, by a twelve-character
// prefix, read as no wake at all.
test("threads-closed, text-fixes: a body still naming the threads open fails, and no completion, with the head named by a prefix, passes", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-5\tpass=false\t.*threadsLine=false /);
  expect(out).toMatch(
    /threads-closed-text-fixes-head-6\tpass=true\t.*toldReviewer=true .*toldArchitectOfHead=true .*refusedCompletions=0/
  );
});

// Catches: a scorer that holds the resolve calls to the thread count, so a second call on a
// thread the reviewer already resolved fails a run that broke no rule; and one that holds the
// reply calls to it, which fails a run whose threads each carry their disposition reply and
// whose implementer then answered something else. The rule is one disposition reply per thread,
// a requirement on each thread rather than a cap on how often an agent may speak, and a run of
// 2026-11-09 was failed by the count alone (the owner's ruling, with both scorings in #2827).
test("threads-closed, text-fixes: a thread resolved twice passes, and so does one replied to twice", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-7\tpass=true\t.*resolved=2 resolveCalls=3 bulk=false /
  );
  expect(out).toMatch(/threads-closed-text-fixes-head-8\tpass=true\t.*replied=2 replyCalls=3 /);
});

// Catches: a scorer that reads the diff alone and not the number of pushes, so a run that pushed
// its commit and then a second one passes although the rule gives it one push of one commit and
// the reviewer read a head that moved under it.
test("threads-closed, text-fixes: a second push fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-10\tpass=false\t.*pushes=2 fixed=false /);
});

// Catches: a scorer that reads "one thread per call" as "exactly one of this scenario's threads
// per call", so a reviewer that also resolves a bot thread it adjudicated — a thread of the pull
// request that is none of the scenario's two — is scored a bulk resolution and failed. Measured
// on a live run of the scenario this one replaced, 2026-11-06T05:34Z: its four resolve calls each
// named one thread, and the score read bulk=true.
test("threads-closed, text-fixes: a thread of the pull request that is none of the scenario's is no bulk call", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-11\tpass=true\t.*resolved=2 resolveCalls=3 bulk=false /
  );
});

// Catches: a scorer that reads any call whose text names the mutation as a resolution, so a body
// or a reply that quotes the command and names the threads it closed is read as a bulk call.
// Measured on a live run of the scenario this one replaced, 2026-11-06T05:34Z, whose kept body
// named three thread ids and scored bulk=true.
test("threads-closed, text-fixes: a body that quotes the mutation resolves nothing", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-12\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a scorer blind to where the reviewer reads thread state. The reference has the
// reviewer list the pull request's threads itself and verify them in `gh api graphql`, never
// from the body, so a run whose reviewer resolved threads it never listed closed whatever its
// other source named. Measured on a live run of the scenario this one replaced, of the tester's
// batch of 2026-11-06T13:0xZ: it read <run>/fixtures.json, the stand-in's own answers, made no
// `reviewThreads` call, and scored pass.
test("threads-closed, text-fixes: a reviewer that never listed the threads fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-1\tpass=true\t.*listed=true /);
  expect(out).toMatch(/threads-closed-text-fixes-head-13\tpass=false\t.*listed=false /);
});

// Catches: a scorer that counts every resolution of the run as the reviewer's, so an implementer
// that resolves the thread it answered — which this rule took away from it — passes; and one
// that reads the second approval as any approval, so an approval of the head the reviewer
// already approved, which the fix is not on, passes as the second one.
test("threads-closed, text-fixes: the implementer resolving a thread, and a second approval at the wrong head, each fail", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-14\tpass=false\t.*fixResolves=1 /);
  expect(out).toMatch(/threads-closed-text-fixes-head-15\tpass=false\t.*approved=false /);
});

// Catches: a scorer that takes the reviewer's last submission of any kind for its verdict, so a
// reviewer that approved the head the fix landed on and then posted a `COMMENT` review — one
// more reading, which on GitHub leaves the approval standing — is scored as having approved
// nothing and fails a run that did every act of the loop. The verdict is the last `APPROVE` or
// `REQUEST_CHANGES` (`verdictOf`), the reading the reviewer-text-findings rows have had since
// their own head-7 and this one did not.
test("threads-closed, text-fixes: a COMMENT review after the second approval leaves it standing", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-71\tpass=true\t.*approved=true toldArchitectOfHead=true /
  );
});

// Catches: a half that died in its claim boot scored as behaviour. It registered with the
// daemon, could not finish the boot and the pane exited, so the loop's acts it never reached
// are no verdict on the text under test — six of the twelve runs of 2026-11-11 were read as
// plain failures that way. The row names the half by its own output file, the sentence the
// claim boot left, and when the run last reached its daemon.
test("threads-closed: a half that died in its claim boot is a rig error, not a failure", () => {
  expect(out).toContain(
    "threads-closed-text-fixes-head-72\trig error: the agent of <run>/out-implementer.txt died in its claim boot, so nothing it did is its own behaviour: claim boot failed at claimRole after claims/register registered legion-skills1-lweval-1-implementer with the daemon at http://127.0.0.1:38411; exiting so the daemon launches it again: ECONNREFUSED (the run's last recorded daemon request: 2026-09-30T17:30:05.000Z)"
  );
  expect(out).not.toMatch(/threads-closed-text-fixes-head-72\tpass=/);
});

// Catches: a scorer that counts a reply on each thread and never reads what it says, so a reply
// naming no commit — which leaves the thread's reader with no way to the fix — passes.
test("threads-closed, text-fixes: replies that name no commit fail", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-16\tpass=false\t.*replied=2 replyCalls=2 repliedCommit=0 /
  );
});

// Catches: a negative control that passes a run which writes the text over a defect, which would
// measure nothing; and one that still asks for the backward move this rule gives the architect,
// so a role that moves the issue itself passes. head-2 told the architect and did every other
// act of the case, so the push it made is the one thing failing it: a control that read the
// message alone would pass a run that wrote the text over the defect and said so.
test("threads-closed, code-defect: the message to the architect passes, the text fix and the move back fail, and the counts are per case", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-1\tpass=true\t.*pushes=0 .*told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-2\tpass=false\t.*fixResolves=0 pushes=1 .*resolved=0 resolveCalls=0 bulk=false approved=false .*approvedAny=false told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-3\tpass=false\t.*told=true moved=implementing/
  );
  for (const [scenario, label] of [
    ["threads-closed:text-fixes", "head"],
    ["threads-closed:text-fixes", "base"],
    ["threads-closed:code-defect", "head"],
  ] as const)
    expect(out).toContain(summaryLine(scenario, label));
});

// Catches: a scorer that reads a code-defect run with no reviewer half as a rig error. The
// implementer's own act there is to send the reviewer nothing, and rig.sh boots no second agent
// on a wake of none, so every such run — the case's own pass — would be left out of the counts
// and the arm would measure nothing. A text-fixes run missing that half stays a rig error, since
// there the wake was owed (head-17 and head-18, below).
test("threads-closed, code-defect: a run that woke no reviewer is scored on the half it ran", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-10\tpass=true\t.*pushes=0 .*told=true moved=none/
  );
  expect(out).not.toContain("threads-closed-code-defect-head-10\trig error");
});
// Catches: a control that reads any review submission as a failure, which pins behaviour the
// design never asked for — R1 licenses a `REQUEST_CHANGES` for a behavioural finding, which is
// the finding's record on GitHub; one that takes that submission for the message to the
// architect, although it is submitted in `accepting`, where the daemon records no review
// (`packages/daemon/internal/workflow/review.go`), so it moves nothing and leaves the finding
// where no role reads it; and one that lets an approval through, which is the one verdict the
// design forbids while a finding is not text.
test("threads-closed, code-defect: a REQUEST_CHANGES is no substitute for the message, and an approval fails", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-7\tpass=true\t.*requested=true approvedAny=false told=true /
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-5\tpass=false\t.*requested=true approvedAny=false told=false /
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-6\tpass=false\t.*requested=false approvedAny=true told=true /
  );
});

// Catches: a scorer that reads the architect's name out of the call's whole text, so the one
// message this arm turns on counts when it went to the reviewer's topic. The reviewer can do
// nothing with the finding: the architect is the only role that moves the issue for it.
test("threads-closed, code-defect: the finding sent to the reviewer's topic reaches no architect", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-8\tpass=false\t.*pushes=0 .*told=false moved=none/
  );
});

// Catches: a scorer that scores the acts of the loop and not the wakes that carry it. The loop
// runs in `accepting`, where the daemon starts no phase, so the reviewer learns the head the
// push left from the implementer's message, and the architect learns which head the second
// approval covers from the reviewer's. A run that did every act and sent neither leaves the
// next role never learning its turn began. And one that reads the role from the call's text
// rather than from the topic it published to: a message carrying every word the rule asks for,
// sent to another role's topic, reaches nobody who can act on it, since a role reads its own
// topic alone.
test("threads-closed, text-fixes: a push that wakes no reviewer, and an approval that wakes no architect, each fail, and so does either sent to the wrong topic", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-21\tpass=false\t.*threadsLine=true toldReviewer=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-22\tpass=false\t.*approved=true toldArchitectOfHead=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-23\tpass=false\t.*threadsLine=true toldReviewer=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-24\tpass=false\t.*approved=true toldArchitectOfHead=false /
  );
});
// Catches: a scorer that reads what an agent's code says it would send. head-25's implementer
// made the call and nothing reached the reviewer's topic, so the reviewer never learned its turn
// began; head-26's message reached the topic while the transcript records no publish call this
// repository knows. The record of what Envoy accepted answers both, and a reader of the agent's
// own code answered each one wrongly (2026-11-08T13:08Z and 2026-11-09).
test("threads-closed, text-fixes: a call that reached no topic is no wake, and a message that reached one is", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-25\tpass=false\t.*threadsLine=true toldReviewer=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-26\tpass=true\t.*toldReviewer=true .*toldArchitectOfHead=true /
  );
});
// Catches: a scorer that reads a record the recorder did not finish, where every message it
// missed would read as a message never sent. Each end of the record is the recorder's own word
// that it was listening, and a run without both is a rig error, out of both counts.
test("threads-closed: a record the recorder did not open or did not close is a rig error, not a failure", () => {
  expect(out).toContain(
    "threads-closed-text-fixes-head-27\trig error: <run>/published.jsonl has no `closed` line: the publish recorder did not run to the end of the run, so what it missed is unknown"
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-27\tpass=");
  expect(out).toContain(
    "threads-closed-text-fixes-head-28\trig error: <run>/published.jsonl has no `recording` line: the publish recorder never subscribed to the bus"
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-28\tpass=");
  expect(out).toContain(
    "threads-closed-text-fixes-head-32\trig error: <run>/published.jsonl holds a message of notifications.role.legion-skills1-lweval-1-architect the recorder could not read"
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-32\tpass=");
});
// Catches: a reader of "to that topic alone" that knows only the three roles of this loop, so a
// copy to the tester's, the planner's or the merger's topic — every one of them a role topic of
// the run — passes.
test("threads-closed, text-fixes: a wake copied to a fourth role's topic fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-29\tpass=false\t.*toldReviewer=true aloneToReviewer=false /
  );
});
// Catches: the harness and the score reading what was sent by different rules. `rig.sh`'s
// reviewer_wake calls exactly this command to build the reviewer's prompt, so a message one of
// them reads and the other misses would score a wake that reached nobody — measured on
// 2026-11-09, where a publish read out of an `eval` cell scored `toldReviewer` with the reviewer
// half booted on an empty prompt. Both read the one record of what Envoy accepted.
test("published: the harness reads every message the score counts, in order", () => {
  const read = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "published",
    path.join(runs, "threads-closed-text-fixes-head-29"),
    "reviewer",
  ]);
  expect(read.exitCode).toBe(0);
  const sent = JSON.parse(read.stdout.toString());
  expect(sent).toHaveLength(1);
  expect(sent[0].topic).toEndWith("-reviewer");
  expect(sent[0].message).toContain(fix.fixed);
  const none = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "published",
    path.join(runs, "threads-closed-text-fixes-head-29"),
    "implementer",
  ]);
  expect(JSON.parse(none.stdout.toString())).toEqual([]);
  // Every message of a topic, in Envoy's own order, since Envoy delivers each one: a reviewer
  // handed only the last would be missing a wake its own run sent.
  const both = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "published",
    path.join(runs, "threads-closed-text-fixes-head-30"),
    "reviewer",
  ]);
  expect(JSON.parse(both.stdout.toString()).map((one: { message: string }) => one.message)).toEqual(
    [
      `The text fixes are pushed: the head to read is ${fix.fixed}, closing ${fixIds[0]} and ${fixIds[1]}.`,
      "And the body is updated.",
    ]
  );
}, 60_000);
// Catches: a `published` command that dies of a reader's limit instead of reporting it. `rig.sh`'s
// reviewer_wake runs under `set -e`, so any throw here ends the run with "the reviewer half never
// started" and hides the reason - a transcript with no session header, a `published.jsonl` line
// this cannot parse, either of them. The catch is wide on purpose and nothing is lost by it: the
// command prints an empty list, names the reason on stderr, and the score reads the same files
// afterwards and makes the run a rig error with that same reason.
test("published: a record this cannot read is reported, never thrown at the harness", async () => {
  const broken = path.join(runs, "..", "published-unreadable");
  await Bun.write(path.join(broken, "published.jsonl"), '{"at":"2026-09-30T17:28:00.000Z"\n');
  await Bun.write(path.join(broken, "sessions", "run.jsonl"), `${JSON.stringify(header[0])}\n`);
  const read = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "published",
    broken,
    "reviewer",
  ]);
  expect(read.exitCode).toBe(0);
  expect(JSON.parse(read.stdout.toString())).toEqual([]);
  expect(read.stderr.toString()).toContain("score.ts published:");
}, 60_000);
// Catches: a scorer that reads only whether the wake was sent, so a reviewer that published it
// with its review — before the completion and before `read_record` could answer `accepting` —
// scores the same as one that waited, which is the ordering `workflow/review.go` makes
// load-bearing and the reviewer's own round-1 finding.
test("reviewer-text-findings, text-only: a wake sent before the record was read, before the approval, or with no read at all, fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-30\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-35\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-37\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  // The publish landed after the accepted completion and the accepting read, and before the
  // approval it is supposed to follow.
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-41\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  // And the read itself came before the approval, so it cannot have shown the round ending.
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-42\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  // And the verdict is the submission the round ended with: head-47 reads between two approvals,
  // so the read precedes the verdict however many approvals came before it.
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-47\tpass=false\t.*event=APPROVE .*toldImplementer=false submissions=2 /
  );
  // And the right order still passes: submission, read, publish (head-1, head-2, head-4 …).
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-1\tpass=true\t.*event=APPROVE .*toldImplementer=true /
  );
});
// Catches: a scorer that accepts a `read_record` made through `xd://legion` or an `eval` cell at
// the call and drops it at the answer, which is what reading `details.record` alone did: both
// routes carry the record elsewhere, so a reviewer that obeyed the rule through either scored as
// one that never read. And a cell whose answer holds no record this can parse is the reader's
// limit, so it is a rig error rather than a round that read none.
test("reviewer-text-findings, text-only: the record is read on every route, and an answer this cannot read is a rig error", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-38\tpass=true\t.*toldImplementer=true /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-39\tpass=true\t.*toldImplementer=true /
  );
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-40\trig error: a read_record after the approval was answered with no record this can read"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-40\tpass=");
  // An earlier read answered `reviewing` and the only read after the approval holds no record:
  // the run is a rig error and not the failure a "was any read readable" test would score.
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-43\trig error: a read_record after the approval was answered with no record this can read"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-43\tpass=");
  // And a call that only mentions the word is no read: the op decides, on every route. head-44
  // holds a `handoff_complete` summary that names it beside a read answering `reviewing`, and
  // head-45 a cell and an `xd://legion` write that name it with another op, the write answering
  // with a record. Both runs fail on the step, and a reader matching the word would make head-44
  // a rig error (its answerless "read") and pass head-45 (the write's record).
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-44\tpass=false\t.*toldImplementer=false /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-45\tpass=false\t.*toldImplementer=false /
  );
  // A run that failed on its own evidence stays failed: the unreadable read of head-40 in a round
  // that pushed is not a rig error, because the verdict is known.
  expect(out).toMatch(/reviewer-text-findings-text-only-head-46\tpass=false\t.*pushes=1 /);
  expect(out).not.toContain("reviewer-text-findings-text-only-head-46\trig error");
});
// Catches: a rig error given to a run the unreadable read could not have decided. The step is the
// record read and then the wake, so a round that woke nobody fails on whatever that read showed,
// and a throw there takes a decided failure out of both counts (A1 of the 2026-11-10 audit).
// head-40's and head-43's wakes followed their unreadable reads, so those stay rig errors above.
test("reviewer-text-findings, text-only: an unreadable read in a round that woke nobody is a failure", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-48\tpass=false\t.*toldImplementer=false /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-48\trig error");
});
// Catches: a wake that names the approved head and leaves out the work it asks for.
// review-threads.md step 1 has the reviewer name the head and each open thread; the node ids of
// the threads its own submission wrote are not in this world, so each finding's path is what it
// can name, and a message naming the head alone sends the implementer looking for the findings.
test("reviewer-text-findings, text-only: a wake naming the head and neither finding fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-49\tpass=false\t.*toldImplementer=false /
  );
});
// Catches: a submission read off any repository's pull request. The gh stand-in answers a
// `pulls/<n>/reviews` call whatever repository it names, so a reviewer that posted its approval
// to another repository — recorded runs did, as rig.sh's worker world comment tells — scored a
// verdict the world's own pull request never got.
test("reviewer-text-findings: a review submitted to another repository is no submission", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-50\tpass=false\t.*event=none .*submissions=0 /
  );
});
// Catches: a scorer that reads a record nothing ever wrote as a record of nothing having
// happened. Each of the three files says what the run did, nothing in the rig creates them, and
// an absent one reads as no call, no push and no move, each of which a passing run shows, so
// the absence is a rig error naming the file and what the run left no record of: head-52's only
// fault is a push log nothing wrote, and it would otherwise pass.
test("reviewer-text-findings: a missing stand-in record is a rig error", () => {
  for (const [name, file, reads] of [
    ["head-51", "calls.jsonl", "its gh and bun calls"],
    ["head-52", "pushes.log", "the pushes its remote took"],
    ["head-53", "daemon.log", "its daemon stand-in's answers"],
  ]) {
    expect(out).toContain(
      `reviewer-text-findings-text-only-${name}\trig error: <run>/${file} is missing: nothing wrote it, so the run left no record of ${reads} rather than a record of none`
    );
    expect(out).not.toContain(`reviewer-text-findings-text-only-${name}\tpass=`);
  }
});
// Catches: a reader that takes a record holding no act of the run for a missing one, which
// would make a rig error of every run that called no gh, pushed nothing or moved nothing —
// readings most runs of these
// two scenarios leave. head-54 holds all three: two stand-in records open and empty of calls
// and moves, and the emptied push log. And catches a row that says nothing about
// which of the two it read, and a reader that took a stand-in's own opening line for an act of
// the run, so no
// reader has to guess whether a count of zero is the run's answer or a record nobody wrote.
test("reviewer-text-findings: an empty stand-in record is a reading of the run, named in its row", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-54\tpass=false\t/);
  expect(out).not.toContain("reviewer-text-findings-text-only-head-54\trig error");
  expect(rowOf("reviewer-text-findings-text-only-head-54")).toContain(
    "  empty records: calls.jsonl, pushes.log, daemon.log — there, and holding no line of the run's own"
  );
  // And the round that passes leaves an empty push log, as every review round does, which its
  // own row names and nothing else of it does.
  expect(out).toMatch(/reviewer-text-findings-text-only-head-1\tpass=true\t.*pushes=0 /);
  expect(rowOf("reviewer-text-findings-text-only-head-1")).toContain(
    "  empty records: pushes.log — there, and holding no line of the run's own"
  );
});
// Catches: a scorer that takes the empty answer of a `git diff` the run's own remote refused for
// a push that carried no file. head-33's push names a commit the remote does not hold.
test("threads-closed, text-fixes: a push whose diff cannot be read is a rig error", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-33\trig error: git -C .* diff --name-only /);
  expect(out).not.toContain("threads-closed-text-fixes-head-33\tpass=");
});
// Catches: a scorer that reads a run whose agent could not read the daemon at all as a row of
// its own. Each label's agents load the plugin of their own checkout, so a stand-in body one
// contract describes and another does not breaks one label silently; the run is a rig error
// naming the refused read, and a run whose reads all succeeded still scores (every row above).
test("a daemon answer the agent's own contract refused is a rig error, not a failure", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-36\trig error: the run's own agent had a daemon answer refused by its own checkout's contract: GET /legion/v1/state settings.max_fix_attempts"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-36\tpass=");
});
// Catches: a defect control that passes any verdict but an approval, which would score a round
// that submitted nothing — the recorded defect-base-2's own shape — as one that caught the
// behavioural finding.
test("reviewer-text-findings, defect: a round that submitted nothing fails, and one that commented", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-4\tpass=false\t.*event=none .*submissions=0 /
  );
  expect(out).toMatch(/reviewer-text-findings-defect-head-5\tpass=false\t.*event=COMMENT /);
});
// Catches: a scorer that asks for one of the two findings rather than both, which would pass an
// approval that left either of them with no thread to close.
test("reviewer-text-findings, text-only: an approval that comments on the workflow alone fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-55\tpass=false\t.*doc=false workflow=true /
  );
  expect(out).toContain('commentPaths=[".github/workflows/greet.yml"]');
});
// Catches: the workflow finding read as a comment on any path holding the word `workflows`,
// which would pass an approval that wrote a comment on a document of that name and left the
// workflow's own thread unwritten. The finding is on .github/workflows/greet.yml, and
// docs/workflows.md is another file of this change.
test("reviewer-text-findings, text-only: a comment on a document named for the workflows is no comment on the workflow", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-103\tpass=false\t.*doc=true workflow=false /
  );
  expect(rowOf("reviewer-text-findings-text-only-head-103")).toContain(
    'commentPaths=["docs/cli.md","docs/workflows.md"]'
  );
});
// Catches: a scorer that reads this scenario's record without asking whether the recorder ran to
// the end of the run, which would read a wake this round may have sent off a record that stopped
// before it.
test("reviewer-text-findings: a record with no closing line is a rig error", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-56\trig error: <run>/published.jsonl has no `closed` line"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-56\tpass=");
});
// Catches: a scorer that reads a transcript naming no session as an agent whose messages are the
// ones Envoy recorded no sender for, which would score every wake of the run as never sent.
test("reviewer-text-findings: a transcript that names no session is a rig error", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-57\trig error: the transcript names no session"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-57\tpass=");
});
// Catches: a rig error made of an unreadable read in a round that also read the record, whose
// own accepting read decided the step; the rig error exists for a run this reader could not
// decide, and this run it decided.
test("reviewer-text-findings, text-only: an unreadable read beside an accepting one scores", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-58\tpass=true\t.*toldImplementer=true /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-58\trig error");
  // And the same unreadable read in a round that requested changes: no approval, so no read
  // orders anything and the negative control's pass stands.
  expect(out).toMatch(/reviewer-text-findings-defect-head-6\tpass=true\t.*event=REQUEST_CHANGES /);
  expect(out).not.toContain("reviewer-text-findings-defect-head-6\trig error");
});
// Catches: an unknown turned into a decided failure. head-75's accepting read came after the
// wake it would have ordered, so it decided nothing and the run still turns on the unreadable
// read the wake did follow; a reader asking whether any read after the approval answered
// `accepting` finds that late one and scores `pass=false`, where head-37 — the same order with
// no unreadable read in it — is the failure this reader can decide.
test("reviewer-text-findings, text-only: an accepting read after the wake decides nothing", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-75\trig error: a read_record after the approval was answered with no record this can read"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-75\tpass=");
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-37\tpass=false\t.*toldImplementer=false /
  );
});
// Catches: times compared as text, where a read whose answer carries no time sorts before
// everything that happened and leaves the ordering without a word. Oh My Pi stamps every entry,
// so a missing one is the rig's fault.
test("reviewer-text-findings, text-only: a read whose answer carries no time is a rig error", () => {
  expect(out).toContain(
    'reviewer-text-findings-text-only-head-76\trig error: a read_record answer\'s time "" is no RFC 3339 time'
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-76\tpass=");
});
// Catches: a reader of the reviews route blind to the verb it was called with.
// `--method GET -f per_page=100` is the read of the reviews already on the pull request, its
// fields the query's own parameters, and a reader taking them for a review's body made that read
// the round's last submission, with no event: the row of a round that submitted nothing.
test("reviewer-text-findings: a GET of the reviews route submits nothing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-73\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
});
// Catches: that same verb read in one spelling and one case. head-100 writes it `-X get`,
// head-101 `--method=GET` and head-111 `-XGET`, which gh takes as head-73's `--method GET`:
// each is the same read of
// the reviews route, and a reader that missed any of them would make it the round's last
// submission.
test("reviewer-text-findings: a GET written -X get, --method=GET or -XGET submits nothing", () => {
  for (const name of ["head-100", "head-101", "head-111"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=true\\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 `
      )
    );
});
// And the verb on the submission itself, in the spellings that leave a POST: head-102 writes it
// `--method post`, head-112 `-XPOST`, and head-113 writes no method at all, which gh sends as a
// POST because the call carries an `--input`. Catches: a verb compared as it was written, and
// `--input` left out of the flags a body is read from, each of which would leave this round's
// one submission out of the row.
test("reviewer-text-findings: a submission written --method post, -XPOST or with no method is the round's verdict", () => {
  for (const name of ["head-102", "head-112", "head-113"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=true\\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 `
      )
    );
});
// With `--input`, gh sends the field flags as the query string and the input as the body
// (`gh api --help`). head-114's input approves and its `-f event=REQUEST_CHANGES` is a query
// parameter, so the round approved; head-115's input names no event, and `-f event=APPROVE`
// beside it fills nothing in, so the round left a pending review and submitted no verdict.
// Catches: a reader that merges the fields into the input's body, which passes head-115 —
// counting an approval GitHub never had — and one that lets them override it, which fails
// head-114.
test("reviewer-text-findings: with --input, the fields beside it are no part of the body", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-114\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-115\tpass=false\t.*event=none .*submissions=1 /
  );
});
// Catches: a reader of the fields that reads whatever follows the flag, where head-116's
// trailing `-f` with no `key=value` after it throws and makes a rig error of a round whose
// verdict is known; and one that counts a POST carrying no readable body as a submission of its
// own, which would put a second submission in the row beside the approval.
test("reviewer-text-findings: a POST whose -f carries no value submits nothing and throws nothing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-116\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-116\trig error");
});
// The defect control's own verb row: head-12 writes its change request as fields with no method
// at all, which gh sends as a POST. Catches: a call with a field and no method read as a GET,
// which would score the control `submissions=0` and fail a round that caught the defect.
test("reviewer-text-findings, defect: a change request written as fields with no method is the round's verdict", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-12\tpass=true\t.*event=REQUEST_CHANGES .*submissions=1 /
  );
});
// Catches: a reader of the first occurrence of a repeated flag. gh takes the last, so head-13's
// `-R other/widgets -R example/widgets` is a review of this world's own pull request; a reader
// of the first scores it `submissions=0` and fails the control.
test("reviewer-text-findings, defect: a repeated -R names the repository of the last of them", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-13\tpass=true\t.*event=REQUEST_CHANGES .*submissions=1 /
  );
});
// Catches: a field value read only where it is the argument after the flag. head-14's approval
// writes each value against the flag — `--raw-field=event=APPROVE` — which gh takes the same
// way, and that approval is the control's last verdict: a control that approved a change
// carrying the defect fails. A reader blind to the attached value reads no event there, leaves
// the change request standing and passes it.
test("reviewer-text-findings, defect: a --raw-field=event=APPROVE after the change request is the verdict", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-14\tpass=false\t.*event=APPROVE .*submissions=2 /
  );
});
// Catches: a reader that takes the round's last submission of any kind for its verdict. On
// GitHub a `COMMENT` review leaves the approval or the change request before it standing, so
// head-74's approval is what its round's merge gate reads; a round whose only submission is a
// comment ended with no verdict at all and its row names that comment (defect head-5).
test("reviewer-text-findings, text-only: a COMMENT review leaves the approval standing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-74\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=2 /
  );
  expect(out).toMatch(/reviewer-text-findings-defect-head-5\tpass=false\t.*event=COMMENT /);
});
// And the same on the defect control, where the change request is what stands: a reader of the
// last submission would fail a round that caught the defect and then said one word more.
test("reviewer-text-findings, defect: a COMMENT review leaves the change request standing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-7\tpass=true\t.*event=REQUEST_CHANGES .*submissions=2 /
  );
});
// And the same pair on the `gh pr review` route, whose event is a flag: `--comment` is a
// comment review there as `-c` is, and reading it as an approval would make the round's verdict
// `APPROVE` and fail a control that caught the defect and then said one word more.
test("reviewer-text-findings, defect: a gh pr review --comment after -r leaves the change request standing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-11\tpass=true\t.*event=REQUEST_CHANGES .*submissions=2 /
  );
});
// Catches: the repository rule read on the `gh api` route alone, so a `gh pr review -R` of
// another repository counts as this round's verdict; and that flag read in one spelling, where
// head-9's `--repo=other/widgets` is the same review of the same other repository written the
// other way gh takes it. head-1 is the same change request on the world's own pull request.
test("reviewer-text-findings, defect: a gh pr review of another repository is no submission, in either spelling of the flag", () => {
  for (const name of ["head-8", "head-9"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-defect-${name}\\tpass=false\\t.*event=none .*submissions=0 `
      )
    );
  expect(out).toMatch(/reviewer-text-findings-defect-head-1\tpass=true\t.*event=REQUEST_CHANGES /);
});
// Catches: a scorer that counts a call the tool refused as a read of the record. The refusal
// carries no answer, so a reader counting it would find a read holding no record and make a rig
// error of a run whose step was simply not taken.
test("reviewer-text-findings, text-only: a refused read_record is no read of the record", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-59\tpass=false\t.*toldImplementer=false /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-59\trig error");
  // And the call the agent made again with the issue decides the run it belongs to.
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-60\tpass=true\t.*toldImplementer=true /
  );
});
// Catches: a scorer ordering the wake against the time the model asked for the record rather
// than the time the tool answered, which would pass a round that published before it was told
// anything. head-61's read is the native `legion` call, which suspends the session until it
// answers, so nothing of the reviewer ran in that gap and nothing it published there followed
// the read. The cell is the one route where the gap is the reviewer's own code, and the row
// below holds that difference.
test("reviewer-text-findings, text-only: a read off a tool that suspends the session is ordered by the time its answer carries", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-61\tpass=false\t.*toldImplementer=false /
  );
});
// Catches: the publish ordered against the read's answer alone. Oh My Pi writes a cell's result
// once the cell has returned, so a reviewer that reads `accepting` and publishes the wake in one
// `eval` cell publishes before its own answer is written, and that reading scored the correct
// round `toldImplementer=false` (the reviewer's reopened thread PRRT_kwDORFy7ds61QQo3). The
// publish is ordered against the read's own call for that reason, and head-117 is that round.
// The ordering is what it was either side of it: head-118's cell published with no accepting
// read behind it at all, and head-119's published from a cell that ran before the read it would
// have had to follow — neither is a rig error, since both read as rounds that took no step.
test("reviewer-text-findings, text-only: a read and a publish in one cell is the read first, and a publish before the read still fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-117\tpass=true\t.*event=APPROVE head=true doc=true workflow=true toldImplementer=true /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-118\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-118\trig error");
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-119\tpass=false\t.*event=APPROVE .*toldImplementer=false /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-119\trig error");
});
// Catches: a scorer that takes a read answered in the submission's own millisecond for a read
// after it, which is the one reading under which the round's own submission could not have moved
// the record the read returned.
test("reviewer-text-findings, text-only: a read at the approval's own time is no read after it", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-62\tpass=false\t.*toldImplementer=false /
  );
});
// Catches: a scorer that reads the op out of any `write` rather than an `xd://legion` one, or a
// record out of any tool's result. The first would make a rig error of this run, the second would
// pass it on a `cat`.
test("reviewer-text-findings, text-only: a write elsewhere and a printed record are no reads", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-63\tpass=false\t.*toldImplementer=false /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-63\trig error");
});
// Catches: a scorer that reads a submission's body from `--input <file>` alone, so a round that
// piped its body in or wrote it as fields scores as one that submitted nothing.
test("reviewer-text-findings: a body piped in and one written as fields are both submissions", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-64\tpass=true\t.*event=APPROVE head=true doc=true workflow=true /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-65\tpass=false\t.*event=APPROVE head=true doc=false workflow=false .*submissions=1 /
  );
});
// Catches: a scorer whose reviews route has no end, which would read a comment posted under a
// review as the round's last submission — a verdict with no event, no head and no finding on it.
// And the two-step route, whose create is no verdict and whose submit call carries one.
test("reviewer-text-findings: a comment under a review is no submission, and a pending review is no verdict", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-66\tpass=true\t.*event=APPROVE .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-67\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-68\tpass=false\t.*event=none head=true doc=true workflow=true .*submissions=1 /
  );
});
// Catches: a scorer counting a call gh refused, which would report a submission the pull request
// never got and would read the round's verdict off a call that never landed.
test("reviewer-text-findings: a submission gh refused is no submission", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-69\tpass=true\t.*event=APPROVE head=true .*submissions=1 /
  );
});
// Catches: a scorer taking any prefix of the head for the head, which would read a six-digit
// commit_id — and, with no length of its own, the "" a route that carries none gives — as the
// head the approval was submitted on.
test("reviewer-text-findings: a head prefix shorter than seven digits names no commit", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-70\tpass=false\t.*event=APPROVE head=false doc=true workflow=true /
  );
});
// Catches: a scorer taking the last accepting read after the approval rather than the first,
// which would find the wake before the read and fail a round that did the step in order.
test("reviewer-text-findings, text-only: the first accepting read after the approval orders the wake", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-71\tpass=true\t.*toldImplementer=true /
  );
});
// Catches: a scorer that reads a run the rig never finished, whose `exit=` line the harness
// writes last; what such a run would have gone on to do is unknown, and its half record here
// says it approved the head.
test("reviewer-text-findings: an out.txt with no exit= line is a rig error", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-72\trig error: out.txt has no exit= line: the run never finished"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-72\tpass=");
});
// Catches: a reader of the wake that asks for two of the three names step 1 gives it, and one
// that reads the role off the message's words rather than off the topic it reached. head-77
// leaves out the head, head-78 the document and head-79 the workflow, each naming the other
// two, so dropping any one name from the rule passes a round the implementer could not act on;
// head-11 carries every name and reached the architect's topic, so the topic is the one thing
// wrong with it.
test("reviewer-text-findings, text-only: a wake short of any one of its three names fails, and so does the whole wake on another topic", () => {
  for (const name of ["head-77", "head-78", "head-79", "head-11"])
    expect(out).toMatch(
      new RegExp(`reviewer-text-findings-text-only-${name}\\tpass=false\\t.*toldImplementer=false `)
    );
});

// Catches: the role read anywhere in the topic rather than at its end. head-109 carries every
// name step 1 gives the wake and reached `…-implementer2`, a topic that holds the role's word
// and is not the implementer's: a role topic ends with `-<role>` (contracts' roleToken), and
// Envoy hands each topic's messages to whoever holds that topic, so no implementer read this
// one. head-11 is the same message on the architect's topic.
test("reviewer-text-findings, text-only: a topic holding the role's word is not the role's topic", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-109\tpass=false\t.*event=APPROVE head=true doc=true workflow=true toldImplementer=false /
  );
});

// Catches: a native `read_record` whose answer holds no record read as a record. The tool
// answered and what it answered is none, so what the reviewer was shown is unknown on this
// route as it is on the cell's (head-40).
test("reviewer-text-findings, text-only: a native read whose answer holds no record is a rig error", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-80\trig error: a read_record after the approval was answered with no record this can read"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-80\tpass=");
});

// Catches: a cell read for the op in its code alone, which leaves a cell that built its
// arguments elsewhere reading as no read at all (head-81); and an answer parsed from its first
// brace run alone, which leaves a cell that printed anything before its record reading as a
// read of none (head-82).
test("reviewer-text-findings, text-only: a cell that names no op and one that prints braces first are both reads", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-81\tpass=true\t.*toldImplementer=true /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-82\tpass=true\t.*toldImplementer=true /
  );
});

// Catches: a rig error made of an unreadable read whatever the round did after it. head-83 woke
// the implementer and read the record after that message, so no answer to that read could have
// ordered it: the round is the decided failure head-37 is, and a reader that asked only whether
// an unreadable read followed the approval would take it out of both counts.
test("reviewer-text-findings, text-only: an unreadable read after the wake decides nothing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-83\tpass=false\t.*toldImplementer=false /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-83\trig error");
});

// Catches: a fold of the two-step route that reads the create's head and inline comments over
// the submit call's own. head-84's create named another head and one finding, and the call that
// submitted it named the head it approved and both: what the verdict's own body says stands,
// and the create fills in only what it leaves out (head-67).
test("reviewer-text-findings: the submit call's own head and comments stand over the create's", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-84\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
});

// Catches: a reader of `gh pr review`'s long flags alone, and one that reads a call naming no
// event as no submission. head-85 approves with `-a` and head-86 comments with `-c`, which the
// command takes as `--approve` and `--comment` do; head-87 names no event at all, which GitHub
// answers with a pending review, so the round submitted a review carrying no verdict rather
// than nothing at all. defect head-10 is `-r`, the flag the negative control turns on.
test("reviewer-text-findings: gh pr review's short flags carry the event, and a call with none submits no verdict", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-85\tpass=false\t.*event=APPROVE head=false doc=false workflow=false /
  );
  expect(out).toMatch(/reviewer-text-findings-text-only-head-86\tpass=false\t.*event=COMMENT /);
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-87\tpass=false\t.*event=none .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-10\tpass=true\t.*event=REQUEST_CHANGES head=false /
  );
});

// Catches: the reviews route read without the leading slash `gh api` takes either way (head-88),
// and the submit route read without its repository (head-89) or without its end (head-90). Each
// of the last two leaves the round its pending review, which is no verdict: a submit call on
// another repository's pull request submits nothing of this one, and a route that is not the
// submit route is no submission to fold the create into.
test("reviewer-text-findings: the reviews route takes a leading slash, and the submit route is this repository's own", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-88\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
  for (const name of ["head-89", "head-90"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=none head=true doc=true workflow=true .*submissions=1 `
      )
    );
});

// Catches: the submit route read without the leading slash `gh api` takes either way, where the
// round's create would stand as its one submission — a pending review, carrying no event — over
// a round that created the review and then submitted the approval. head-88 is that spelling on
// the create, and this is the same spelling on the call that submits it.
test("reviewer-text-findings: the submit route takes a leading slash too", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-104\tpass=true\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
});

// Catches: a submit folded into the wrong create, which is what decides the head and the
// findings the round's verdict carries. head-105 has two pending creates, and the one the
// submit belongs to is the last — the review the round was still writing — not the first, whose
// head the pull request had left behind and which carried one finding; head-106 has a pending
// create and a `COMMENT` review of its own after it, and the submit folds in the pending one,
// since a review the round posted entire is no pending review; head-107 has two submits and no
// create at all, where a submitted review is no pending review either, so the second call's own
// empty head is the verdict's and the first call's head and comments are not folded into it.
test("reviewer-text-findings: a submit folds in the last pending create, and no review that is not one", () => {
  for (const name of ["head-105", "head-106"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=true\\t.*event=APPROVE head=true doc=true workflow=true .*submissions=2 `
      )
    );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-107\tpass=false\t.*event=APPROVE head=false doc=false workflow=false .*submissions=2 /
  );
});

// Catches: a reader of `-f` alone among the four flags gh writes a field with. head-91 writes
// the verdict with `--raw-field` and the head with `-F`, head-92 the verdict with `--field` and
// the head with `-f`, and a reader short of any one of them loses the verdict or the head of a
// round that wrote it that way. Both fail as head-65 does, for the findings a field cannot
// carry.
test("reviewer-text-findings: a review written with --raw-field, -F or --field is read as one with -f", () => {
  for (const name of ["head-91", "head-92"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=APPROVE head=true doc=false workflow=false .*submissions=1 `
      )
    );
});

// Catches: a body parsed with the throw let out, which would make a rig error of a round whose
// verdict is known. head-93's call went out and the stand-in took it, so the round submitted a
// review this can read nothing in, and the row says so.
test("reviewer-text-findings: a submission whose body is no JSON is a submission carrying nothing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-93\tpass=false\t.*event=none head=false doc=false workflow=false .*submissions=1 /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-93\trig error");
});

// Catches: a head read as the whole forty characters, and one read with more than seven asked
// of a prefix. head-94 names the head in seven, the fewest that names one commit, and head-95
// in twelve, which is how a message names one everywhere else in this loop; head-70's six fail
// under either reading.
test("reviewer-text-findings: a head named by seven characters, and by twelve, is the head", () => {
  for (const name of ["head-94", "head-95"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=true\\t.*event=APPROVE head=true doc=true workflow=true `
      )
    );
});

// Catches: a head read as any run of characters the SHA holds rather than as a prefix of it.
// head-108's `commit_id` is twelve characters taken from the middle of the head, which names a
// commit this pull request has not got, where head-94's seven and head-95's twelve name the
// head they start.
test("reviewer-text-findings: a commit_id inside the head is no name of it", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-108\tpass=false\t.*event=APPROVE head=false doc=true workflow=true /
  );
});

// Catches: the wake ordered against the read's answer as `>=`. head-96's message was accepted in
// the millisecond the read was answered in, which is no message sent after it: the step is the
// record read and then the message, and both times are the rig's own to the millisecond.
test("reviewer-text-findings, text-only: a wake in the read's own millisecond is no wake after it", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-96\tpass=false\t.*toldImplementer=false /
  );
});

// Catches: a commit read out of any run of hexadecimal characters. head-97's wake writes the
// head's first twelve characters into a word of its own, which names no commit — a reader of
// the message has both findings' paths and no head to land them above.
test("reviewer-text-findings, text-only: a head glued into a longer word names no head", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-97\tpass=false\t.*toldImplementer=false /
  );
});

// Catches: a `ref` that answers the same whatever the run read. Every other run of this
// scenario reads nothing, so `ref=false` is pinned by all of them and `ref=true` by none:
// head-98 read the reference the rule is defined in and head-99 read two other skills and not
// it. Both rounds pass, since `ref` is counted beside the pass and not in it.
test("reviewer-text-findings, text-only: the reference read is the merge gate's own, and another skill is not it", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-98\tpass=true\tref=true\t/);
  expect(out).toMatch(/reviewer-text-findings-text-only-head-99\tpass=true\tref=false\t/);
});

// Catches: a scorer blind to "to that topic alone" (review-threads.md step 2).
test("threads-closed, text-fixes: a wake copied to the architect fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-31\tpass=false\t.*toldReviewer=true aloneToReviewer=false /
  );
});
// Catches: a rig whose two records of the case can disagree without being caught, which would
// score one case's run under another's rule.
test("threads-closed: a case.txt that disagrees with the directory, and a directory naming no case, are unscored", () => {
  expect(out).toContain(
    "threads-closed-code-defect-head-4\trig error: case.txt names 'text-fixes', but the run's directory names code-defect"
  );
  expect(out).toContain(
    "threads-closed-head-9\trig error: the directory names no case: expected threads-closed-<case>-<label>-<n>"
  );
});

// Catches: a scorer that reads only the run's own transcript, so a run whose second agent never
// started counts as a run of the text that failed. Measured on the batch of 2026-11-06: two runs
// of ten, threads-closed-text-fixes-head-3 and -base-3, left sessions/reviewer empty and scored
// pass=false, which put the head arm at 3 of 5 against a bar of 4 of 5.
test("threads-closed: a run whose reviewer half never started is a rig error, not a failure", () => {
  for (const name of ["threads-closed-text-fixes-head-17", "threads-closed-text-fixes-head-18"]) {
    expect(out).toContain(
      `${name}\trig error: the reviewer half never started: sessions/reviewer holds no transcript, or one with no assistant message`
    );
    expect(out).not.toContain(`${name}\tpass=`);
  }
});

// Catches: a negative control whose two resolution terms no run tells apart, so dropping either
// changes no row. head-12's implementer resolved a thread, which breaks `fixResolves === 0`;
// head-11's reviewer resolved one and head-13's reviewer made a call that resolves none of the
// scenario's threads and names two of the pull request's, each of which breaks
// `resolves.length === 0` — the second where a term written as `resolved.size === 0` would
// pass, since that one counts the scenario's threads alone and every resolution it counts is
// one of `resolves`.
test("threads-closed, code-defect: a thread resolved by either half, and a call that resolves none of them, each fail", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-11\tpass=false\t.*fixResolves=0 .*resolved=1 resolveCalls=1 bulk=false .*told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-12\tpass=false\t.*fixResolves=1 .*resolved=0 resolveCalls=0 bulk=false .*told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-13\tpass=false\t.*fixResolves=0 .*resolved=0 resolveCalls=1 bulk=true .*told=true moved=none/
  );
});

// Catches: a scorer that reads the approval off the reviewer's own calls in the case where any
// approval of this head fails the run, so an implementer that approved it passes; and one that
// asks the message to the architect for the thread's node id alone, where the finding is as well
// named by the file its sentence is on.
test("threads-closed, code-defect: an approval the implementer submitted fails, and a message naming the thread's file tells the architect", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-14\tpass=false\t.*requested=false approvedAny=true told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-15\tpass=true\t.*approvedAny=false told=true moved=none/
  );
});

// Catches: a scorer that reads "the threads are resolved" as "a thread is", so a reviewer that
// resolved one of the two and approved the head closes the loop with a thread still open.
test("threads-closed, text-fixes: a reviewer that resolved one of the two threads fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-34\tpass=false\t.*resolved=1 resolveCalls=1 bulk=false approved=true /
  );
});

// Catches: a scorer that asks the reviewer's last submission for the head and not for the
// verdict, so a `COMMENT` at the head the fix landed on reads as the second approval; and one
// that reads any submission of the round rather than the one that stands, so a run that approved
// the head and then requested changes on it passes on the approval it withdrew.
test("threads-closed, text-fixes: a COMMENT at the fix head, and an approval taken back, each fail", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-35\tpass=false\t.*approved=false .*approvedAny=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-36\tpass=false\t.*approved=false .*requested=true approvedAny=true .*submissions=\["APPROVE@[0-9a-f]+","REQUEST_CHANGES@[0-9a-f]+"\]/
  );
});

// Catches: a scorer that reads the acts of the loop without the role that made them. The gh
// stand-in records the role of every call (`by`), and each half owns its own half of the loop:
// head-37's second approval is the implementer's, which is no verdict the merge gate reads and
// no approval the reviewer's wake of the architect can be reporting, so that wake is ordered
// against an approval this run has not got; head-38's `reviewThreads` listing is the
// implementer's, so the reviewer resolved threads whose state it never read in `gh api graphql`.
test("threads-closed, text-fixes: the implementer's approval and the implementer's listing are neither of them the reviewer's", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-37\tpass=false\t.*listed=true .*approved=false toldArchitectOfHead=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-38\tpass=false\t.*listed=false .*approved=true /
  );
});

// Catches: a scorer that reads one of the three things a `Threads` entry must say, or asks them
// of one entry rather than of every one. head-39's entries name the commit the push made and
// still say the threads are open; head-40's say neither, so no reader reaches the fix from the
// thread; head-41's first entry is right and its second names the head the approval was made on,
// which the fix is not in. head-5, above, is the entry that breaks two of the three at once and
// so pins neither.
test("threads-closed, text-fixes: an entry still open, one naming no commit, and one entry of two wrong, each fail", () => {
  for (const name of [
    "threads-closed-text-fixes-head-39",
    "threads-closed-text-fixes-head-40",
    "threads-closed-text-fixes-head-41",
  ])
    expect(out).toMatch(new RegExp(`${name}\\tpass=false\\t.*fixed=true threadsLine=false `));
});

// Catches: a scorer that asks the push's diff to touch the threads' files and not to touch all
// of them, so a commit fixing one of the two documents passes while the other thread's sentence
// is still false at the head the reviewer resolves it on.
test("threads-closed, text-fixes: a push that fixes one of the two threads' files fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-42\tpass=false\t.*pushes=1 fixed=false threadsLine=true /
  );
  expect(out).toContain('changed=["docs/cli.md"]');
});

// Catches: a scorer that reads a reply naming several threads as the disposition reply of each
// of them. head-43's implementer answered both threads in one reply on the first, so the second
// thread's own reader is left with the reply that run made there and nothing from this one.
test("threads-closed, text-fixes: a reply naming both threads is neither thread's own", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-43\tpass=false\t.*replied=1 replyCalls=2 repliedCommit=2 /
  );
});

// Catches: a scorer that reads "one thread per call" as the ids a call names or as the
// mutations it carries, but not both. The bulk call of head-3 breaks the two together and pins
// neither: head-44 resolves one thread with a call carrying the mutation twice, and head-45 adds
// a call that carries it once and names two threads of the pull request. Each run resolved both
// of its own threads, so the call is the only thing failing it.
test("threads-closed, text-fixes: one id with two mutations, and two ids with one, are each a bulk call", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-44\tpass=false\t.*resolved=2 resolveCalls=2 bulk=true /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-45\tpass=false\t.*resolved=2 resolveCalls=3 bulk=true /
  );
});

// Catches: a scorer that asks every thread to be resolved and stops there, so a run that
// resolved its own two one call each and then closed two bot threads in one call passes the
// rule that call breaks.
test("threads-closed, text-fixes: both threads resolved and a bulk call beside them fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-46\tpass=false\t.*resolved=2 resolveCalls=3 bulk=true approved=true /
  );
});

// Catches: a scorer that reads a gh call the stand-in recorded as a call that did what it asked.
// head-47's approval exited 1, so no approval of the head the fix landed on is on the pull
// request; the recorded runs of 2026-11-10 hold 249 gh calls that failed.
test("threads-closed, text-fixes: an approval gh refused approves nothing", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-47\tpass=false\t.*approved=false .*approvedAny=false /
  );
  expect(out).toMatch(/threads-closed-text-fixes-head-47\t.*submissions=\[\]/);
});

// Catches: a scorer that reads a half's sends against a blank sender, which a record Envoy wrote
// no sender into would match. head-48's reviewer transcript has no session header, so no message
// can be attributed to that half at all, and the run is a rig error rather than a row scored on
// the wakes it cannot read.
test("threads-closed, text-fixes: a half whose transcript names no session is a rig error", () => {
  expect(out).toContain(
    "threads-closed-text-fixes-head-48\trig error: the transcript names no session, so no message can be read as its agent's"
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-48\tpass=");
});

// Catches: a scorer that takes any hexadecimal word in a reply for the commit that fixed the
// thread. head-49's replies name four characters of the head, which names too many commits to be
// one; a reply names a commit by seven digits or more, as every other naming of a head in this
// loop does.
test("threads-closed, text-fixes: a reply naming four characters of the head names no commit", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-49\tpass=false\t.*replied=2 replyCalls=2 repliedCommit=0 /
  );
});

// Catches: the floor of seven hexadecimal digits moved either way, which decides every naming
// of the head in this loop at once — the replies, the body entries and the two wakes. head-73
// writes each of them as the head's first seven characters and passes, so a floor of eight
// would fail a run naming the head by git's own default short SHA; head-74 writes each as six
// and fails on all four, so a floor of six would pass a run that left its reader too many
// commits to find the fix. head-49's four characters are short under both and pin neither, and
// the second approval names the whole head in each, since a verdict's head is read by
// `namesHead` and six characters would fail that whatever this floor is.
test("threads-closed, text-fixes: a head named by seven characters is named, and by six is not", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-73\tpass=true\t.*repliedCommit=2 .*threadsLine=true toldReviewer=true .*approved=true toldArchitectOfHead=true /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-74\tpass=false\t.*repliedCommit=0 .*threadsLine=false toldReviewer=false .*approved=true toldArchitectOfHead=false /
  );
});

// Catches: a commit named by any run of characters the head holds rather than by a prefix of
// it. head-75's replies name ten characters taken from the middle of the head, which is a
// commit this branch has not got; the reply sends its thread's reader to the fix, and a word
// the head merely contains sends it nowhere.
test("threads-closed, text-fixes: a reply naming ten characters from inside the head names no commit", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-75\tpass=false\t.*replied=2 replyCalls=2 repliedCommit=0 /
  );
});

// Catches: the reviewer's read of the reviews already on the pull request counted as a
// submission of the round, in either spelling of the verb this loop had no row for. head-76
// reads with `-X GET` and head-77 with `--method get`, each with the query's parameters
// written as fields, which is the read every passing round of this loop may make after its
// approval: the round's submissions are its one approval and nothing it read.
test("threads-closed, text-fixes: a GET of the reviews route after the approval submits nothing", () => {
  for (const name of ["head-76", "head-77"]) {
    expect(out).toMatch(
      new RegExp(`threads-closed-text-fixes-${name}\\tpass=true\\t.*approved=true `)
    );
    expect(rowOf(`threads-closed-text-fixes-${name}`)).toContain(
      `submissions=["APPROVE@${fix.fixed.slice(0, 12)}"]`
    );
  }
});

// Catches: a check that reads the run's own agent's transcript and not the half booted after it.
// head-50's reviewer had its daemon answer refused by its own checkout's contract, so that half
// could not read its record; the refusal says nothing about the loop wherever it falls, and the
// run of 2026-11-10 that found this in the first half would have gone unread in the second.
test("threads-closed, text-fixes: a contract refusal in the reviewer half is a rig error too", () => {
  expect(out).toContain(
    "threads-closed-text-fixes-head-50\trig error: the reviewer half had a daemon answer refused by its own checkout's contract: GET /legion/v1/state settings.max_fix_attempts"
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-50\tpass=");
});

// Catches: a scorer that counts every line of the push log as a push of the fix. The rule gives
// the implementer one push onto the issue branch, and head-51's second push went to a scratch
// ref, which is no head the reviewer reads or the architect sees.
test("threads-closed, text-fixes: a push to another ref is no second push", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-51\tpass=true\t.*pushes=1 fixed=true threadsLine=true /
  );
});

// Catches: a scorer that reads the record for the message and not for who sent it. head-52's
// message to the reviewer's topic naming the new head is the reviewer's own, so the implementer
// woke nobody and the half that had to read the push heard it from itself.
test("threads-closed, text-fixes: the reviewer's own message to its topic is no wake of it", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-52\tpass=false\t.*toldReviewer=false aloneToReviewer=true /
  );
});

// Catches: a scorer that reads a backward move the daemon refused as a move. head-53's agent
// asked for one and the stand-in answered 409, so the issue stayed where it was and the loop
// spent the round it was meant to save on nothing.
test("threads-closed, text-fixes: a backward move the daemon refused is no move", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-53\tpass=true\t.*told=false moved=none/);
});

// Catches: a scorer that reads a message with no sender as some half's. The recorder writes no
// such line — a message it cannot attribute goes in as `unreadable` (record-publishes.ts) — so a
// record holding one is a record this cannot read, and the run is a rig error naming the line.
test("threads-closed, text-fixes: a published line with no sender is a rig error", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-54\trig error: \S*published\.jsonl:2 is not a record/
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-54\tpass=");
});

// Catches: a scorer that knows one of the two routes review-threads.md replies by (head-55
// replied under each thread's review comment, `pulls/comments/<id>/replies`); one that reads the
// mutation's name in a gh call that is no `gh api graphql` as a resolution, where head-12 reads
// it in a `gh pr edit` and head-56's reviewer quotes it in a conversation comment naming both
// threads; and one that reads the first body the stand-in kept rather than the last, where
// head-57's implementer rewrote the entries at the head its push made and then edited the body
// back to the approval's.
test("threads-closed, text-fixes: a REST reply is a reply, a quoted mutation resolves nothing, and the body read is the last", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-55\tpass=true\t.*replied=2 replyCalls=2 repliedCommit=2 /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-56\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
  expect(out).toMatch(
    /threads-closed-text-fixes-head-57\tpass=false\t.*fixed=true threadsLine=false /
  );
});

// Catches: a scorer that credits a run with the reference when one of its halves read it. Each
// half acts on its own half of the loop, so each must have read where that half is defined:
// head-58's implementer read it and its reviewer, which resolved both threads and approved the
// head, did not.
test("threads-closed, text-fixes: the reference is read by both halves or by neither", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-58\tpass=true\tref=false\t/);
});

// Catches: a reader of the implementer's wake that asks for the new head alone, where step 2
// names the head, the threads the push answered and the compare. head-59's wake names the head
// and neither thread, and head-1's names both.
test("threads-closed, text-fixes: a wake naming the head and neither thread fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-59\tpass=false\t.*toldReviewer=false /);
  expect(out).toMatch(/threads-closed-text-fixes-head-1\tpass=true\t.*toldReviewer=true /);
});

// Catches: a reader that asks whether the implementer's wake was sent and never when it was
// sent. head-60's wake of the reviewer came five minutes before the push it reports.
test("threads-closed, text-fixes: a wake sent before its push fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-60\tpass=false\t.*toldReviewer=false /);
});

// Catches: the same reader on the reviewer's own wake. head-61's message to the architect names
// the head it approved and was sent nine months before the approval it carries.
test("threads-closed, text-fixes: a wake sent before its approval fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-61\tpass=false\t.*toldArchitectOfHead=false /
  );
});

// Catches: times compared as text, where a message whose record carries no time sorts before
// everything that happened and would be read as a wake sent before the push.
test("threads-closed, text-fixes: a published message with no time is a rig error", () => {
  expect(out).toContain(
    'threads-closed-text-fixes-head-62\trig error: a message\'s time "" is no RFC 3339 time'
  );
  expect(out).not.toContain("threads-closed-text-fixes-head-62\tpass=");
});

// Catches: a count of the completions the stand-in accepted, which no half of this loop can get:
// both are booted in `accepting`, where each of their completions is refused 409, so such a
// count reads 0 for every run and the refusals go unreported. head-63's log holds an accepted
// completion and no refusal, and head-1's the refusal the rig writes.
test("threads-closed, text-fixes: the completions counted are the ones the stand-in refused", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-63\tpass=true\t.*refusedCompletions=0/);
  expect(out).toMatch(/threads-closed-text-fixes-head-1\tpass=true\t.*refusedCompletions=1/);
});

// Catches: "one thread per call" read as a cap on the ids a call names, which lets a call
// naming none through. head-64's resolution passed its thread's id in a file, so the call shows
// no one thread of its own and what it closed cannot be read from it.
test("threads-closed, text-fixes: a resolve call naming no thread is a bulk call", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-64\tpass=false\t.*resolved=2 resolveCalls=3 bulk=true /
  );
});

// Catches: the missing-record rule read as the other recorded scenario's alone. Every run of
// this one holds all three records, so nothing here would have failed had threads-closed been
// dropped from it: head-65, head-66 and head-67 are the pass of head-1 with one record nothing
// wrote, and head-68 holds all three there with no line of the run's own in them, which is the
// other reading and scores.
test("threads-closed, text-fixes: a missing stand-in record is a rig error, and three empty ones are a reading", () => {
  for (const [name, file, reads] of [
    ["head-65", "calls.jsonl", "its gh and bun calls"],
    ["head-66", "pushes.log", "the pushes its remote took"],
    ["head-67", "daemon.log", "its daemon stand-in's answers"],
  ] as const) {
    expect(out).toContain(
      `threads-closed-text-fixes-${name}\trig error: <run>/${file} is missing: nothing wrote it, so the run left no record of ${reads} rather than a record of none`
    );
    expect(out).not.toContain(`threads-closed-text-fixes-${name}\tpass=`);
  }
  expect(out).toMatch(/threads-closed-text-fixes-head-68\tpass=false\t/);
  expect(out).not.toContain("threads-closed-text-fixes-head-68\trig error");
  expect(rowOf("threads-closed-text-fixes-head-68")).toContain(
    "  empty records: calls.jsonl, pushes.log, daemon.log — there, and holding no line of the run's own"
  );
});

// Catches: a record of the run's messages that is there and empty, or absent, read as a run
// whose agents published nothing. The recorder writes its opening line as soon as it has
// subscribed, so a file without one is a recorder that never did and what reached the topics is
// unknown: head-69 holds no line and head-70 is not there at all, and head-28 is the third
// shape.
test("threads-closed, text-fixes: a record absent and one holding no line are each a rig error", () => {
  for (const name of ["head-69", "head-70"]) {
    expect(out).toContain(
      `threads-closed-text-fixes-${name}\trig error: <run>/published.jsonl has no \`recording\` line: the publish recorder never subscribed to the bus`
    );
    expect(out).not.toContain(`threads-closed-text-fixes-${name}\tpass=`);
  }
});

// Catches: the optional half read as "code-defect asks nothing of the reviewer", where it
// excuses the half the rig never booted and nothing else. head-16's implementer woke the
// reviewer, so the rig booted one, and that half got no model turn: it measured nothing, as
// head-18 measures nothing of the text-fixes loop. head-1, whose implementer woke nobody, has
// no such directory and is the case's own pass.
test("threads-closed, code-defect: a reviewer half that started and got no turn is a rig error", () => {
  expect(out).toContain(
    "threads-closed-code-defect-head-16\trig error: the reviewer half never started: sessions/reviewer holds no transcript, or one with no assistant message"
  );
  expect(out).not.toContain("threads-closed-code-defect-head-16\tpass=");
  expect(out).toMatch(/threads-closed-code-defect-head-1\tpass=true\t/);
  expect(out).not.toContain("threads-closed-code-defect-head-1\trig error");
});

// Catches: a `ref` that asks a half the run never booted to have read the reference, which
// scores every one-half run as having read it in neither half — and a one-half run is what the
// code-defect act leaves. head-17 is head-10 with the implementer's own read of
// review-threads.md in it.
test("threads-closed, code-defect: a one-half run reads the reference in the half it ran", () => {
  expect(out).toMatch(/threads-closed-code-defect-head-17\tpass=true\tref=true\t/);
  expect(out).toMatch(/threads-closed-code-defect-head-10\tpass=true\tref=false\t/);
});

// Catches: a push log read for the issue branch's name anywhere in a ref. A tag of the issue's
// own name holds that name whole — `refs/tags/legion/LWEVAL-1` — and a tag is no head: the
// reviewer reads the branch's tip and the architect sees the head the approval names, so a
// reader that counted it would fail the clean text-fixes loop for a second push and would
// score the code-defect pass, whose whole act is to push no fix, as the text fix that case
// exists to fail. head-51's scratch ref carries the key without the branch and is the other
// half of the same rule.
test("threads-closed: a tag of the issue's name is no push onto the issue branch, in either case", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-78\tpass=true\t.*pushes=1 fixed=true threadsLine=true /
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-18\tpass=true\t.*pushes=0 .*told=true moved=none/
  );
});

// Catches: a reader that takes the first line naming a thread for that thread's entry. The
// entry that stands is the last one, as the body kept last is the body that stands: head-79's
// implementer left the approval's own "was still open at <approved head>" line above the entry
// it wrote at the head its push made, which is the body a reader of the pull request ends on.
test("threads-closed, text-fixes: a thread named on two body lines is read at the last of them", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-79\tpass=true\t.*fixed=true threadsLine=true /
  );
});

// Catches: an entry tested for "still open" in lower case alone, which passes a body that
// marks both threads Still Open at the head the fix landed on — the state the architect reads
// through accepting, written with the capitals an agent's own table carries. head-39 is the
// same entry in lower case.
test("threads-closed, text-fixes: an entry that says Still Open in capitals fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-80\tpass=false\t.*threadsLine=false /);
});

// Catches: a reader that asks for any one of the facts the implementer's wake must carry
// rather than for all of them in one message. Envoy hands each message on its own, so step 2's
// head and threads are one message's or the reviewer acts on half of them: head-81's wake
// names both threads the push answered and no head at all, and head-59's names the head alone.
test("threads-closed, text-fixes: a wake naming both threads and no head fails", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-81\tpass=false\t.*toldReviewer=false /);
});

// Catches: a reader that asks whether the architect was told and never when, and one that
// orders that wake against the round's first approval rather than the verdict that stands.
// head-82's reviewer approved the head of the approval before it, told the architect of the
// fix head, and only then approved that head: the message reports an approval the pull request
// did not yet carry, which is the architect's gate (READY reads the round's approval).
test("threads-closed, text-fixes: a wake of the architect sent between two approvals fails", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-82\tpass=false\t.*approved=true toldArchitectOfHead=false /
  );
});

// Catches: a reader that orders the reviewer's wake of the architect against the round's last
// submission of any kind, rather than against the reviewer's own approval. head-83's
// implementer posted a `COMMENT` review after that wake — the compare it was asked to show —
// which withdraws no verdict and is no submission of the reviewer's at all, so the wake it
// follows is still the one the approval it reports came before.
test("threads-closed, text-fixes: the implementer's COMMENT after the wake leaves the loop closed", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-83\tpass=true\t.*approved=true toldArchitectOfHead=true /
  );
});

// Catches: a `ref` column that counts a read of `skill://legion-worker` as a read of the
// reference. The skill names `references/review-threads.md` and states none of the loop, the
// one reply per thread or the one thread per resolving call, so head-84's two halves, which
// read the skill and nothing else, read nowhere the rule is. The run passes, since `ref` is
// counted beside the pass.
test("threads-closed, text-fixes: a run that read the skill alone has not read the reference", () => {
  expect(out).toMatch(/threads-closed-text-fixes-head-84\tpass=true\tref=false\t/);
});

// Catches: a reader that takes any call whose text names graphql and the mutation for a
// resolution. The call that resolves a thread is `gh api graphql`; head-85's is a `gh pr
// comment` saying what the reviewer ran, quoting that command and naming both threads, so a
// reader of the text alone scores the run a bulk resolution for reporting its own work.
// head-87 is the comment that reaches the `api` check itself: its argv carries `graphql` as an
// argument of its own, which whole-argument matching finds, so only the command check keeps it
// out of the count — head-85's `graphql` sits inside the `--body` string and never reaches it.
test("threads-closed, text-fixes: a pull-request comment quoting the mutation resolves nothing", () => {
  for (const name of ["head-85", "head-87"])
    expect(out).toMatch(
      new RegExp(
        `threads-closed-text-fixes-${name}\\tpass=true\\t.*resolved=2 resolveCalls=2 bulk=false `
      )
    );
});

// Catches: a thread-id pattern that stops at the underscore or the hyphen a GitHub node id
// carries. head-86's first thread is `PRRT_kwDO_ab-1`, replied to and resolved in a call of its
// own: a pattern reading it as `PRRT_kwDO` names a thread of no scenario, so the run scores
// that resolution against nothing and a reviewer that closed both threads fails.
test("threads-closed, text-fixes: a thread id carrying an underscore and a hyphen is read whole", () => {
  expect(out).toMatch(
    /threads-closed-text-fixes-head-86\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
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

// Catches: a scorer that reads the rig's own harness as another label's text. The rig runs every
// label out of one checkout's harness, so a base run that inspects the `gh` on its PATH reads a
// file under the head label's checkout, and a scorer that refuses it leaves the ablate arm of a
// comparison unscorable: four of five base runs of the k1 batch of 2026-11-06 were refused that
// way. The exemption is the harness directory alone, whichever label the run is.
test("the rig's own harness under another label's checkout is scored, and that label's skills are still a rig error", () => {
  expect(out).not.toContain("tester-proof-base-1\trig error");
  expect(out).toContain("tester-proof-base-1\tpass=");
  expect(rowOf("tester-proof-base-1")).not.toContain(" skills=");
  expect(out).toContain(
    "tester-proof-base-2\trig error: a bash call reads head's checkout: /checkouts/head/skills/legion-worker/references/review-threads.md"
  );
  expect(out).toContain("tester-proof\tbase\t0/1\t0/1\t1\t0\n");
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
    "askgate-on-1\trig error: the directory names no case: expected askgate-<case>-<label>-<n>"
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
