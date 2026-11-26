import { beforeAll, expect, setDefaultTimeout, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  appendFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import path from "node:path";
import { cliRefusals } from "@legion/envoy-client/dispatch-cli";
import { CLAIM_BOOT_FAILED_OPENING } from "@legion/pi-shared/legion-claim-core";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { claimBootOpening } from "./boot-refusal";
import {
  checkouts,
  ownRun,
  readsCommit,
  runsOf,
  specWork,
  statesDesign,
  TestHandoff,
  workerReadings,
} from "./score";
import { bundled, sourcesCommit, spawnChecked } from "./test-programs";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

// A run whose agent never got a model turn (Oh My Pi exits before its first request, as when the
// profile's gateway key command runs past its budget) says nothing about the skill under test: the
// score reports it as a rig error and leaves it out of the pass counts. An agent that got a turn and
// then did nothing the scenario asks is a failure.

// score.ts reads each label's checkout from the built-from of each run of it, never from the
// work directory's profiles: each run here records one (`run`).
const work = temporaryDirectory("skill-scenarios-score");
const runs = path.join(work, "runs");
/** The repository each run's built-from commit is in, which the score is given (`--repo`): the
 * scorer rebuilds a run's label's daemon sources from that commit alone (daemon-pane.ts's
 * `recordedCheckout`), never off the checkout its built-from names. */
const LABEL_REPO = path.join(work, "labels.git");
/** The labels a run of this file is built for, each from `/checkouts/<label>`. */
const LABELS = ["head", "base", "cross", "on", "off"];
// The repository the pushed-code run below is cloned from. Reserved here, above the hook that
// drains the stagings: Bun runs a scope's `beforeAll` hooks in the order they were registered,
// so a reservation made further down would be made after that drain had already run.
const pushWork = temporaryDirectory("skill-scenarios-push");
/** This checkout, whose plugin's wording a head run's claim boot sentence is read by, and whose
 * daemon a run built from it takes a completion as. */
const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
/** The module, under a checkout, that writes a plugin's claim boot sentence. */
const CLAIM_CORE = path.join("packages", "pi-shared", "src", "legion-claim-core.ts");
/** A checkout holding main ff2a10ef's claim boot sentence and nothing else, which a base run's
 * built-from names below. */
const BASE_CLAIM_CHECKOUT = path.join(work, "base-claim-checkout");
/** A checkout holding an older daemon's engine and completion route alone, as main ff2a10ef
 * writes them (completion-answers.ts's `refused` and `issue`), which a run of that daemon's
 * built-from names below: how a run's daemon takes a completion is read out of its checkout
 * (daemon-pane.ts's `labelDaemon`), never out of its label's name. */
const OLDER_DAEMON_CHECKOUT = path.join(work, "older-daemon-checkout");
// A run's daemon is read out of its checkout by `go run` of daemon-completion.go, so a host with
// no go reads none, and the rows that turn on one skip.
const withoutGo = skipWithout("go");
/** The scorer the rows run as rig.sh runs it, a bundle built from score.ts for this suite, so no
 * run of it transpiles score.ts and its imports again (test-programs.ts). Reserved here, above
 * the hook below that runs it, for the same reason as `pushWork`. */
const SCORE = bundled(path.join(import.meta.dir, "score.ts"));

/**
 * Every run this file's body stages, as a function the `beforeAll` below calls rather than a
 * write at the call. `work` is reserved at module scope and made by the hook that reservation
 * registered, so nothing exists to write into until then — and a `bun test -t <pattern>` that
 * selects no row of this file runs no hook of it, where a module body that staged its world
 * would leave every file of it behind.
 */
const stagings: (() => void)[] = [];

beforeAll(() => {
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
 * database, or none when `files` gives it null, and, for a run of one of LABELS, a built-from
 * naming `/checkouts/<label>`) and `transcript`, when the hook above runs. A built-from that
 * records a checkout and no commit is given the commit of the daemon sources that checkout holds
 * (`sourcesCommit`, in LABEL_REPO), as rig.sh's cmd_profile records the label's commit. */
function run(name: string, files: Record<string, string | null>, transcript: object[]) {
  stagings.push(() => {
    const dir = path.join(runs, name);
    mkdirSync(path.join(dir, "sessions"), { recursive: true });
    // The agent's own working directory, where a path it writes and then reads back resolves
    // (rig.sh's worker_boot), so a fixture naming `cwd/<file>` puts the file where the agent
    // would have left it.
    mkdirSync(path.join(dir, "cwd"), { recursive: true });
    const own = dispatchRecord(`run_${name.replaceAll("-", "_")}`);
    const label = /-([a-z]+)-\d+$/.exec(name)?.[1] ?? "";
    const built = LABELS.includes(label) ? { "built-from": `checkout /checkouts/${label}\n` } : {};
    for (const [file, given] of Object.entries({ "dispatch.json": own, ...built, ...files })) {
      const checkout =
        file === "built-from" ? /^checkout (.+)$/m.exec(given ?? "")?.[1] : undefined;
      const text =
        checkout === undefined || /^commit /m.test(given ?? "")
          ? given
          : `${given}commit ${sourcesCommit(LABEL_REPO, checkout)} parents=\n`;
      if (text !== null) writeFileSync(path.join(dir, file), text);
    }
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
    base: "main",
    head: pr,
  });
const world = worldOf(head);
/** The implementer's record the rig seeds (seed.ts implement-handoff), as `seed.ts
 * capture-handoffs` writes it: its body is `head`, the commit the implementer pushed. */
const implementRecord = {
  id: "h-implement",
  issue_key: "LWEVAL-1",
  phase: "implement",
  version: 1,
  author: { kind: "session", id: "rig-implementer" },
  created_at: "2026-09-30T17:20:00.123456Z",
  body: JSON.stringify({ head }),
};
/** A test handoff the skill's rule asks for: the head the tester tested, the implementer's results
 * verified, no failures. What the tester ran is a result on the issue's check, not a field of the
 * handoff, and the issue and the phase are the record's. */
const testBody = (sha: string) => ({
  head: sha,
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
// head-2's run, ended and moved to <work>/archive/runs as rig.sh's archive_run moves every run
// that has ended: `score.ts runs <work>/runs` scores it there.
run("ask-on-message-archived-1", { ...capture, "out.txt": "exit=0\n" }, [...header, ...turn]);
stagings.push(() => {
  mkdirSync(path.join(work, "archive", "runs"), { recursive: true });
  renameSync(
    path.join(runs, "ask-on-message-archived-1"),
    path.join(work, "archive", "runs", "ask-on-message-archived-1")
  );
});

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
      role: "tester",
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
// The same read, after an assignment whose value holds a space.
run("tester-proof-cross-2", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash("NOTE='two words' cat /checkouts/base/skills/legion-worker/SKILL.md"),
]);
// The same read, run by `env -i` with an assignment of its own (env(1): `env [-i] [NAME=value…]
// command`).
run("tester-proof-cross-3", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash("env -i NOTE='two words' cat /checkouts/base/skills/legion-worker/SKILL.md"),
]);
// The same read by a reader whose name is quoted.
run("tester-proof-cross-4", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash(`"cat" /checkouts/base/skills/legion-worker/SKILL.md`),
]);
// The same read by a reader named by its path.
run("tester-proof-cross-5", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash("/bin/cat /checkouts/base/skills/legion-worker/SKILL.md"),
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
// The PR body's check line, read up to its tail and no further (score.ts's TESTER_LINE). lines-1
// is an older label's line: its tail is the check's own words, as main ff2a10ef's Dispatch prints
// it, and its `at` names the run's head, so it passes as rec-1 does. lines-2's `at` names no
// commit of the run, and its tail, a reading as this checkout's Dispatch prints the line, names
// the head: a reader of the whole line takes that reading for the line's commit, which only the
// head label's line can carry.
const testerBody = (at: string, tail: string) => ({
  ...bodyEdit(head),
  body: `**Checks:**\n- Check k1 (before_merge): pass at ${at}, recorded by tester. ${tail}`,
});
recordRun("tester-proof-lines-1", {
  "calls.jsonl": calls([ranGreet, testerBody(head.slice(0, 12), "The CLI greets by name.")]),
});
recordRun("tester-proof-lines-2", {
  "calls.jsonl": calls([
    ranGreet,
    testerBody(
      "0000000000ab",
      `Ran bun greet.ts Ada at ${head.slice(0, 12)}: it printed Hello, Ada.`
    ),
  ]),
});
// Pushed a change to the code under the PR's head before completing. The repository is staged
// with every other run, in the hook above: `git init` here would make it whether or not a row of
// this file runs.
stagings.push(() => {
  const git = (...args: string[]) => {
    const result = spawnChecked(
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

// Completed naming a test record of another issue, LWEVAL-9, whose body is valid: the body holds
// no issue, so the record's own is the one the score reads.
recordRun("tester-proof-rec-5", {
  "handoffs.json": handoffs({ ...testRecord(), issue_key: "LWEVAL-9" }),
});
// Completed naming its own record, whose body still holds schemaVersion, a stamp the record holds.
recordRun("tester-proof-rec-6", {
  "handoffs.json": handoffs({
    ...testRecord(),
    body: JSON.stringify({ ...testBody(head), schemaVersion: 1 }),
  }),
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
    "## Verification",
    "",
    "The checks are in this issue's verification.md.",
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
/** What the CLI logs for a call when the fixture says nothing else: each spec command the
 * call ran, as a success, when the call exited 0 and its output shows no refusal line, and no
 * `--help` or `--dry-run` call. A refused call logs nothing by default. The CLI logs a refusal too,
 * but not one of `dispatch proxy` or `dispatch login`, when the call knows its session
 * (dispatch-cli.ts runDispatchCli): under the command's tool, or under `dispatch` for a command it
 * cannot name. A fixture that models such a call names its lines, and one that leaves them out
 * models a call that knew no session. */
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
/** Every checkout a spec run's agent can reach, as the runs staged so far recorded them, which is
 * what the scorer reads (score.ts's `checkouts`). */
const reached = () => checkouts([...runsOf(runs).values()]);
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
      const checks = specWork(dir, reached(), fixtureJson).checks.length;
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
// Not in the outline: the Verification part is gone.
specRun("spec-writing-head-147", {
  spec: outlined().replace(
    "## Verification\n\nThe checks are in this issue's verification.md.\n\n",
    ""
  ),
});
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
// The same, the `echo` named in quotes, which runs the same echo.
specRun("spec-writing-head-143", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x10",
      'cd /rig/cwd\n"echo" "=== ask ==="; dispatch ask --issue LWEVAL-7 2>&1 | head -5',
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
// The same, the `echo` named by its path, which runs the same echo.
specRun("spec-writing-head-144", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x10",
      'cd /rig/cwd\n/bin/echo "=== ask ==="; dispatch ask --issue LWEVAL-7 2>&1 | head -5',
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
// A spec write by the CLI named by its path: the same `dispatch` command, and it counts.
specRun("spec-writing-head-145", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    sh(
      "w2",
      "/usr/local/bin/dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 3)\nDocument token: sha256:abc",
      [{ tool: "dispatch_doc_edit" }]
    ),
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
// A review of a sibling issue's spec: its assignment holds the outline's headings too, but no
// 8-word run of it is in a version the session wrote, so it checks no write of this run. Its
// Verification part points to the document of LWEVAL-8 in words that the run's spec does not hold.
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
  "## Verification",
  "",
  "LWEVAL-8's verification.md holds the checks.",
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
// draft's Goals, Requirements, Verification and Decisions but rewrites its Summary, Problem and
// Design. After the draft the assignment pastes about 4,500 words of unique notes with no heading,
// so the spec's slice runs to the end. Measured over the pasted text, the share of it found in a
// written version falls under the 0.1 floor; measured over the written version (recall), the
// draft still covers most of it.
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
// Run 90's write, the list naming the CLI by its path.
specRun("spec-writing-head-146", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    evalCell(
      "x20",
      pythonWrite.replace('["dispatch"', '["/usr/local/bin/dispatch"'),
      pythonApplied
    ),
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
// A spec write after an assignment whose value holds a space: the `dispatch` command is the one
// the shell runs, and it counts.
specRun("spec-writing-head-141", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    sh(
      "w2",
      "NOTE='two words' dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 3)\nDocument token: sha256:abc",
      [{ tool: "dispatch_doc_edit" }]
    ),
    finish
  ),
});
// An edit of another issue's spec whose ops quote `--issue LWEVAL-7` inside a quoted word: that
// text is the word's, no flag, and the edit is no write of the run's spec.
specRun("spec-writing-head-142", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "n1",
      `dispatch doc-edit --ops-json '[{"op":"insert","after":"end","markdown":"see --issue LWEVAL-7 here"}]' --issue WS-1 --artifact spec`,
      0,
      applied
    ),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
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
/** The review record the round leaves on the issue, whatever it submitted: its body is `head`, the
 * commit reviewed, as a handoff holds since #5287; the decision is the review on GitHub. */
const reviewRecord = {
  id: "h-review",
  issue_key: "LWEVAL-1",
  phase: "review",
  version: 1,
  author: { kind: "session", id: "01a0f2cc" },
  created_at: "2026-09-30T17:29:11.031234Z",
  body: JSON.stringify({ head }),
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
/** A read of the record: the `legion` tool's `read_record`, answered with the issue in `phase`.
 * The score reads this call and its own answer, since every other caller of `/legion/v1/state`
 * — `handoff_complete`, the phase reminder — reads the same route and says nothing about the
 * agent having read the record. The implementer's gate on its push is such a read (score.ts's
 * threadsClosed `gated`); a reviewer's read is no part of its step, which is the wake after the
 * approval whatever the record shows. The answer is written in the call's own millisecond, as a
 * real read answers, and its time is what a push is ordered against. */
const readRecord = (at = "2026-09-30T17:40:30.000Z", phase = "accepting") => [
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
    timestamp: at,
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
 * `reviewerTextFindings` reads this file — the verdict and its time are in the calls, the wake in
 * the bus — and the run writes it because `unscored` makes a run with no daemon.log a rig error
 * (head-53 below is that run). So the line is what the rig would have logged and is scored
 * nowhere. */
const reviewDaemon = [
  {
    at: "2026-09-30T17:40:00.000Z",
    path: "/legion/v1/handoff/complete",
    status: 200,
    role: "reviewer",
    grantId: "g-1",
  },
];
/** Threads of the pull request that are none of a scenario's own, as rig.sh's worlds seed them in
 * their graph (`threadRoutes`): review_world's style-bot thread, `PRRT_kwDOrig0001`, and a second
 * of its kind. The score reads a resolution from the world's replay (score.ts's `threadActs`), and
 * the world resolves a thread it holds and answers NOT_FOUND for any other, so a run below whose
 * reviewer resolves one of these holds it in its world, as every run of rig.sh's that resolves
 * the bot's thread does. */
const BOT_THREADS = [
  { id: "PRRT_kwDOrig0001", path: "greet.test.ts", line: 4 },
  { id: "PRRT_kwDOrig0002", path: "greet.test.ts", line: 9 },
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
      // The routes fixture_write leaves the gh stand-in, which every worker run has; the score
      // replays the threads a round opened from them and its calls (gh-threads.ts).
      "fixtures.json": JSON.stringify({ routes: [] }),
      "pushes.log": "",
      "handoffs.json": handoffs(reviewRecord),
      // seed.ts capture-results's record of the issue's results, which the score reads beside
      // the handoffs (score.ts's `resultsOf`): none of the run's agents recorded one.
      "results.json": "[]",
      "daemon.log": calls(reviewDaemon),
      "published.jsonl": bus(toldImplementer(head)),
      ...files,
    },
    transcript
  );
}
// head-122: the shape every base run of 2026-11-12 left, a completion the daemon stand-in
// refused 400 with no code — its request contract refused the base label's `ready` — beside a
// round that otherwise passes. Catches: a refused completion scored as the run's own behaviour,
// which made each base result a world of its own (review 5887659447, finding 1): the run is a rig
// error naming the path and the status. No daemon of either shape answers a 400, so the run's own
// is read for it, out of the checkout its built-from names.
reviewRun("reviewer-text-findings-text-only-head-122", {
  "built-from": `checkout ${CHECKOUT}\n`,
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "daemon.log": calls([{ ...(reviewDaemon[0] ?? {}), status: 400 }]),
});
// head-36: the stand-in answered the read 200 and the agent's own plugin refused the body, which
// is what a label whose contract requires a key the stand-in stopped serving reads on every state
// read (`standin-record.ts`'s `settings` line tells that measurement). An agent whose
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
// one thread, which review-threads.md has the reviewer make and the score reports. The world
// holds that thread, as review_world's does.
reviewRun("reviewer-text-findings-text-only-head-4", {
  "case.txt": "text-only\n",
  "fixtures.json": threadRoutes(JSON.stringify(BOT_THREADS.slice(0, 1))),
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
// head-7: one push onto the issue branch, the push a text-only round makes for the reviewer's text
// commit under v31, but of a commit no remote of the run holds: the scorer reads that push's diff
// to score the commit (`changedFiles`), cannot, and makes the run a rig error rather than score a
// round on a diff nobody read.
reviewRun("reviewer-text-findings-text-only-head-7", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([submission(approval())]),
  "pushes.log": `2026-09-30T17:29:25.000Z refs/heads/legion/LWEVAL-1 ${"0".repeat(40)} ${head}\n`,
});
// The same resolution sent as the body of `gh api graphql --input`, which the call's arguments
// never name.
reviewRun("reviewer-text-findings-text-only-head-8", {
  "case.txt": "text-only\n",
  "fixtures.json": threadRoutes(JSON.stringify(BOT_THREADS.slice(0, 1))),
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
// head-146: the same resolution sent to the endpoint written `/graphql`, which gh posts to
// GitHub's GraphQL endpoint as it does `graphql` (gh-threads.ts's `graphqlOf`), and the world
// resolved the bot's thread at it.
reviewRun("reviewer-text-findings-text-only-head-146", {
  "case.txt": "text-only\n",
  "fixtures.json": threadRoutes(JSON.stringify(BOT_THREADS.slice(0, 1))),
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:20.000Z",
      as: "gh",
      argv: [
        "api",
        "/graphql",
        "-f",
        'query=mutation{resolveReviewThread(input:{threadId:"PRRT_kwDOrig0001"}){thread{id}}}',
      ],
      exit: 0,
    },
    submission(approval()),
  ]),
});
// head-49: the approval the rule asks for, and a wake that names the head alone. Step 1 has the
// reviewer name the approved head and each open thread, by its node id or by the path its
// finding is on, and a message naming neither leaves the implementer the whole diff to search for
// the work the merge waits for.
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
// head-64 and head-65: the same route with the body on its other two carriers. head-64 pipes the
// body in (`--input -`), which the stand-in records as the call's stdin, and head-65 writes the
// verdict, the head and the body as fields and no inline comment (head-123 writes those as
// fields too, `comments[][path]=…`). So head-64 passes as head-1 does and head-65 fails for its
// two uncommented findings, and a reader of `--input <file>` alone would score either as a round
// that submitted nothing.
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
// The reviewer's completion after its round moved the issue, refused 409
// HANDOFF_NOT_CURRENT_PHASE: an older daemon (main `ff2a10ef`'s, no `leftItsPhase`) refuses a
// late reviewer so, and base-2, built from one, is scored; this checkout's daemon takes a late
// reviewer naming its record and refuses one so only as a repeat after its own taken completion,
// which a lone refusal is not, so head-126, built from this checkout, is a rig error
// (completion-answers.ts's ANSWERS, which `unscored` and health.ts both read). Which daemon each
// ran is read out of the sources of the commit its built-from records, never out of its label's
// name: base-3 is the base label as it is built now, from main at the head's newest forward
// merge, whose daemon is this checkout's, and is a rig error with head-126.
stagings.push(() => {
  const internal = path.join(OLDER_DAEMON_CHECKOUT, "packages", "daemon", "internal");
  mkdirSync(path.join(internal, "workflow"), { recursive: true });
  mkdirSync(path.join(internal, "api"), { recursive: true });
  writeFileSync(
    path.join(internal, "workflow", "engine.go"),
    'package workflow\n\nfunc (e *Engine) handoff(fact intake.HandoffComplete) (intake.Result, error) {\n\tif RoleFor(issue.Phase) != fact.Role {\n\t\treturn refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("the %s does not run %s of %s", fact.Role, issue.Phase, issue.Key)), nil\n\t}\n\treturn intake.Result{}, nil\n}\n'
  );
  writeFileSync(
    path.join(internal, "api", "handoff.go"),
    "package api\n\nfunc (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {\n\tword, writer, handoffPhase := phase.Handoff(issue.Phase)\n\trecordBacked := handoffPhase && writer == grant.Role\n}\n"
  );
});
for (const [name, checkout] of [
  ["reviewer-text-findings-text-only-base-2", OLDER_DAEMON_CHECKOUT],
  ["reviewer-text-findings-text-only-head-126", CHECKOUT],
  ["reviewer-text-findings-text-only-base-3", CHECKOUT],
])
  reviewRun(name, {
    "built-from": `checkout ${checkout}\n`,
    "case.txt": "text-only\n",
    "calls.jsonl": calls([submission(approval())]),
    "daemon.log": calls([
      { ...(reviewDaemon[0] ?? {}), status: 409, code: "HANDOFF_NOT_CURRENT_PHASE" },
    ]),
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
// and no input, which is a create that sends no body — on GitHub, and in the stand-in that
// answered it, a pending review at the pull request's head (gh-call.ts's `createdState`) — so
// the round's verdict is still its approval, beside that pending review. Catches: a reader of
// the fields that splits whatever follows the flag, which throws on the argument that is not
// there and makes a rig error of a round whose verdict is known.
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
// head-123 to head-125: the two-step route, with the create written the ways the gh stand-in
// reads as GitHub does (gh-call.ts's `createdState`, `createdHead` and `sentBody`, which both
// read the create by). head-123 writes the create's inline comments as fields —
// `comments[][path]=…` and its friends, which gh sends as an array of comment objects — and
// names no head, which GitHub and the stand-in take as the pull request's head when the create
// was made; head-124's create sends no body at all, a pending review at that head, and the call
// that submits it carries the verdict and the comments; head-125's create names `event: ""`,
// which GitHub reads as no event. Each passes as head-67 does. Catches: comment fields read as
// flat keys, which scored head-123 `doc=false workflow=false`; a create naming no head read at
// `""`, which failed head-123 and head-124 on `namesHead` where the world had answered each at
// the head; and a bodyless create read as no create, where the world kept a pending review.
const fieldComments = findings.flatMap((finding) => [
  "-f",
  `comments[][path]=${finding.path}`,
  "-F",
  `comments[][line]=${finding.line}`,
  "--raw-field",
  `comments[][body]=${finding.body}`,
]);
/** The submit of the round's pending review, `-f event=APPROVE` unless `rest` says otherwise. */
const submitPending = (...rest: string[]) => ({
  at: "2026-09-30T17:29:30.000Z",
  as: "gh",
  argv: [
    "api",
    "--method",
    "POST",
    "repos/example/widgets/pulls/1001/reviews/6001/events",
    ...(rest.length > 0 ? rest : ["-f", "event=APPROVE"]),
  ],
  exit: 0,
});
reviewRun("reviewer-text-findings-text-only-head-123", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:20.000Z",
      as: "gh",
      argv: [
        "api",
        "repos/example/widgets/pulls/1001/reviews",
        "-f",
        "body=The open threads are the work the merge waits for.",
        ...fieldComments,
      ],
      exit: 0,
    },
    submitPending(),
  ]),
});
reviewRun("reviewer-text-findings-text-only-head-124", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      at: "2026-09-30T17:29:20.000Z",
      as: "gh",
      argv: ["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews"],
      exit: 0,
    },
    {
      ...submitPending("--input", "submit.json"),
      files: {
        "submit.json": JSON.stringify({
          event: "APPROVE",
          body: "The open threads are the work the merge waits for.",
          comments: findings,
        }),
      },
    },
  ]),
});
reviewRun("reviewer-text-findings-text-only-head-125", {
  "case.txt": "text-only\n",
  "calls.jsonl": calls([
    {
      ...pendingReview,
      files: {
        "pending.json": JSON.stringify({
          commit_id: head,
          event: "",
          body: "The open threads are the work the merge waits for.",
          comments: findings,
        }),
      },
    },
    submitPending(),
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
// and then the submit of the verdict. GitHub refuses the second, 422 "User can only have one
// pending review per pull request", and so does the gh stand-in (gh-standin.ts's
// `reviewCreate`), which records it with exit 1: the review the submit submits is the first,
// at the head the pull request left behind and with one finding written, and the round fails.
// Catches: a reader that counts the refused create, which read this round's approval as the
// second create's — a pass on a review GitHub never created.
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
    { ...pendingReview, exit: 1 },
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
// head-110: the round head-1 makes, with one push to a ref of the agent's own,
// `refs/heads/scratch`, and none to the issue branch. Catches: the pushes counted on the issue
// branch alone, and a push to another ref taken for the text commit. A text-only round's one push
// is its text commit, onto the branch the reviewer reads and the architect sees; a push to a ref
// beside it is counted all the same, and carries no text commit.
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
// fail a control that caught the defect. The body is fields and no `--input`, and it writes no
// inline comment, which is why this is the defect control's row and not the text-only arm's,
// whose approval needs one on each finding (head-123 writes those as fields).
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
// defect head-19 and head-20: a change request GitHub refuses to create — head-19's carries no
// body, which GitHub requires for `REQUEST_CHANGES` and `COMMENT`, and head-20's names the event
// in lower case, which is no event GitHub takes. GitHub refuses each 422, and the gh stand-in
// refuses it with GitHub (gh-call.ts's `createdState`, which both read the create by), so the
// round submitted nothing and the control fails. The stand-in records a call it refused with
// exit 1, which no reader counts; these rows hold it with exit 0, so the score's own reading of
// the create decides it. Catches: a score that reads a create GitHub refuses as the round's
// verdict, or as a pending review, which passes a control whose change request never reached
// the pull request or leaves it a review GitHub never created.
for (const [name, fields] of [
  ["head-19", ["-f", "event=REQUEST_CHANGES"]],
  ["head-20", ["-f", "event=request_changes", "-f", "body=The blank-name guard is gone."]],
] as const)
  reviewRun(
    `reviewer-text-findings-defect-${name}`,
    {
      "case.txt": "defect\n",
      "calls.jsonl": calls([
        {
          at: "2026-09-30T17:29:30.000Z",
          as: "gh",
          argv: [
            "api",
            "repos/example/widgets/pulls/1001/reviews",
            ...fields,
            "-f",
            `commit_id=${head}`,
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
/** The world's routes (<run>/fixtures.json) as far as the score replays the threads from them
 * (score.ts's `threadActs`): the graph rig.sh's fixture_write writes, holding each thread of
 * `threadsFile` (a run's threads.json) with the comment that opened it, numbered from 7001 in
 * thread order, its `databaseId` the id GitHub's REST reply route names it by. A declaration, so
 * the reviewer scenario's runs above, staged before this line is read, seed their bot's thread
 * with it too. */
function threadRoutes(threadsFile: string): string {
  return JSON.stringify({
    routes: [
      {
        match: "^gh api graphql",
        graph: {
          query: {
            repository: {
              pullRequest: {
                reviewThreads: JSON.parse(threadsFile).map(
                  (thread: { id: string; path: string; line: number }, index: number) => ({
                    id: thread.id,
                    isResolved: false,
                    path: thread.path,
                    line: thread.line,
                    comments: [
                      {
                        id: `PRRC_kwDOAAAAAc4AAC${7001 + index}`,
                        databaseId: 7001 + index,
                        path: thread.path,
                        line: thread.line,
                        state: "SUBMITTED",
                        author: { login: "reviewer-bot" },
                        body: "This sentence is false.",
                      },
                    ],
                  })
                ),
              },
            },
          },
        },
      },
    ],
  });
}
/** The routes of a world holding the threads of `threadsFile` and, after them, the bot threads
 * (BOT_THREADS), for a run whose reviewer resolves a thread of the pull request that is none of
 * its scenario's. */
const withBotThreads = (threadsFile: string) =>
  threadRoutes(JSON.stringify([...JSON.parse(threadsFile), ...BOT_THREADS]));
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
/** The daemon stand-in's log line of a completion it refused as the workflow refuses one of a
 * phase the completing role does not run, 409 HANDOFF_NOT_CURRENT_PHASE (daemon-standin.ts):
 * the answer the implementer of this loop gets in `reviewing` and in `accepting` (rig.sh's
 * run_threads_closed), the base reviewer gets once the round has moved the issue, and the head
 * reviewer gets for a repeat after its own earlier 200. A reading of the run, and no rig error.
 * The stand-in names the role it serves on every line, and a completion's is read by it alone. */
const refusedCompletion = {
  at: "2026-09-30T17:40:00.000Z",
  path: "/legion/v1/handoff/complete",
  status: 409,
  code: "HANDOFF_NOT_CURRENT_PHASE",
  role: "implementer",
  grantId: "g-1",
};
/** The line of a completion the stand-in accepted, which is what a reviewing phase's own
 * completion gets (head-63). */
const completed = {
  at: refusedCompletion.at,
  path: refusedCompletion.path,
  status: 200,
  role: "reviewer",
  grantId: "g-1",
};
/** The backward move the architect makes and neither role does. */
const movedBack = (reason: string) => ({
  at: "2026-09-30T17:40:00.000Z",
  path: "/legion/v1/phase/backward",
  status: 200,
  grantId: "g-1",
  to: "implementing",
  reason,
});
/** The implementer's wake of the reviewer: the daemon starts no phase for its last-chance push,
 * so the reviewer learns of it from this message, which names the new head. */
const toldReviewerOfPush = (commit: string, at?: string, sender?: string) =>
  sentTo(
    "reviewer",
    `The text fixes are pushed: the head to read is ${commit}, closing ${fixIds[0]} and ${fixIds[1]}.`,
    at,
    sender
  );
/** The implementer's message to the architect of its push, which its role prompt has it send
 * beside the reviewer's: the head the push made. */
const toldArchitectOfPush = (commit: string, at = "2026-09-30T17:42:30.000Z") =>
  sentTo("architect", `I pushed ${commit} above the reviewer's commit; it changes only text.`, at);
/** The reviewer's wake of the architect: the second approval is the architect's gate, and no
 * phase ends to carry it, so this message naming the head it approved is how the architect
 * learns the loop closed. */
const toldArchitectOfHead = (commit: string, at = "2026-09-30T17:42:00.000Z") =>
  sentTo("architect", `I approved the head the fix landed on, ${commit}.`, at, SECOND_SESSION);
/** The run's own remote, built once as this module is read, and its commits: the head the
 * reviewer approved, a commit that changes the two documents, one that changes the code too, one
 * above the approved head that changes one document alone, and the four each role App's bot makes
 * above the approved head, the implementer's push and the reviewer's own commit above it.
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
  const result = spawnChecked(
    ["git", ...SEED_SETTINGS.flatMap((setting) => ["-c", setting]), "-C", dir, ...args],
    { env: seedGitEnv(env) }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
  return result.stdout.toString().trim();
};
/** The bot of each role App of rig.sh's world (its world_implement_app and world_review_app, and
 * their ids), as role_identity gives it to each commit its role makes: its login, which
 * world.json's `logins` names, and the address its commits carry, which accounts.json links to
 * that login. */
const appBot = (slug: string, id: number) => ({
  login: `${slug}[bot]`,
  id,
  email: `${id}+${slug}[bot]@users.noreply.github.com`,
});
const IMPLEMENT_BOT = appBot("widgets-implementer", 318406215);
const REVIEW_BOT = appBot("widgets-reviewer", 318406277);
/** A seed step that commits every tracked change as `bot`, its author and committer. */
const commitAs = (
  bot: { login: string; email: string },
  message: string
): [kind: "write" | "git", ...parts: string[]] => [
  "git",
  "-c",
  `user.name=${bot.login}`,
  "-c",
  `user.email=${bot.email}`,
  "commit",
  "-qam",
  message,
];
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
  // The commits a run of rig.sh's world makes above the approved head, each by its role App's
  // bot: the implementer's push that fixes both documents and the reviewer's own commit above
  // it, and the implementer's push that fixes the workflow's comment alone and the reviewer's fix
  // of the document's sentence above that, which is run 3787e2d71dfa's shape. The App that
  // committed a push is what tells the implementer's push from the reviewer's later one
  // (score.ts's `fixPushes`).
  ["git", "checkout", "-q", "-b", "implementer-both", "legion/LWEVAL-1~2"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  commitAs(IMPLEMENT_BOT, "docs: the two text fixes"),
  ["git", "checkout", "-q", "-b", "reviewer-above-both", "implementer-both"],
  ["write", "docs/cli.md", "It trims the spaces around the name before it prints it.\n"],
  commitAs(REVIEW_BOT, "docs: the reviewer's own wording"),
  ["git", "checkout", "-q", "-b", "implementer-workflow", "legion/LWEVAL-1~2"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  commitAs(IMPLEMENT_BOT, "ci: the workflow's comment alone"),
  ["git", "checkout", "-q", "-b", "reviewer-above-workflow", "implementer-workflow"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  commitAs(REVIEW_BOT, "docs: the sentence the implementer's push left"),
  ["git", "checkout", "-q", "legion/LWEVAL-1"],
];
/** Every input of the build the seed's path is named for: its steps, the date every commit
 * carries and the settings its git runs under. The recipe hashed the steps alone, so a change
 * to the date or to one of those settings named the same path for another repository — the
 * first process to build it would win, and every later one would read commits its own recipe
 * would never have made. */
const SEED_INPUTS = [...SEED_STEPS.flat(), FIX_DATE, ...SEED_SETTINGS];
/** The commits a seed holds, read through `read`: the head the reviewer approved, the two text
 * fixes above it, the commit that changes the code too, the one-file branch, and the implementer's
 * and the reviewer's commits of each role App's bot (the four branches named for them). Every
 * reader of a seed answers with these — a build of its own and the shared one below — so the two
 * cannot drift into naming different revisions. */
const commitsOf = (read: (revision: string) => string) => ({
  approved: read("legion/LWEVAL-1~2"),
  fixed: read("legion/LWEVAL-1~1"),
  withCode: read("legion/LWEVAL-1"),
  docOnly: read("one-file"),
  implementerBoth: read("implementer-both"),
  reviewerAboveBoth: read("reviewer-above-both"),
  implementerWorkflow: read("implementer-workflow"),
  reviewerAboveWorkflow: read("reviewer-above-workflow"),
});
/** Builds the seed in `dir`, under `env`, and answers its commits. The `one-file` branch
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
/** That seed's commits. A process that finds it there reads them and builds nothing; one
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
/** The reviewer's result of the world's check at `sha`, the head the implementer's push made: the
 * earlier pass recorded as holding there, with its one-sentence reason, before the reviewer's
 * second approval (17:38), as correct's (e) asks of it. */
function reviewerHolds(sha: string, at = "2026-09-30T17:37:30.000001Z") {
  return {
    check: "k1",
    commit: sha,
    session: SECOND_SESSION,
    role: "reviewer",
    verdict: "pass",
    observed: "The earlier pass holds: this push changes only docs/cli.md and a workflow comment.",
    at,
  };
}
/** The implementer's transcript every threads-closed run gets unless it names its own: its read
 * of the record, which answers `reviewing` — the phase rig.sh's correct world leaves the issue in
 * while the reviewer has not approved. No term of the score reads it. */
const fixReadsReviewing = [
  ...header,
  ...turn,
  ...readRecord("2026-09-30T17:30:00.000Z", "reviewing"),
];
/** A threads-closed run: the correct world with two threads (the document's and the workflow's)
 * unless `files` says otherwise, and the world's routes seeding the run's threads
 * (`threadRoutes`), as rig.sh writes them, with the reviewer's own transcript in
 * <run>/sessions/reviewer, where rig.sh's worker_boot puts the second agent's. Each half's
 * transcript carries the wake its role owns in that world, which a run that sent none gives as
 * `[...header, ...turn]`.
 *
 * Whether there is a second half at all is the rig's own rule and not a choice of each fixture:
 * rig.sh's run_threads_closed boots the reviewer on the messages the implementer published to
 * its topic, and boots none in code-defect where that wake holds none, since sending the
 * reviewer nothing is the act that case measures (its `reviewer_wake`). So a run whose record
 * holds no message on the reviewer's topic gets a reviewer half only where the rig would have
 * booted one, which in correct is always — the implementer owed a wake there, and the empty
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
    "fixtures.json": threadRoutes(files["threads.json"] ?? fixThreadsFile),
    "case.txt": "correct\n",
    "pushes.log": "",
    "daemon.log": calls([refusedCompletion]),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": "[]",
    // What Envoy accepted on this run's topics, with the boot line of each half: the
    // implementer's wakes of the reviewer and the architect, then the reviewer's wake of the
    // architect. A run that sent one of them elsewhere, or sent none, names its own record here.
    "published.jsonl": bus(
      toldReviewerOfPush(fix.fixed),
      toldArchitectOfPush(fix.fixed),
      toldArchitectOfHead(fix.fixed)
    ),
    ...files,
  };
  // The reviewer's result at the head the implementer's push made, which correct's (e) asks for:
  // a pass recorded as holding there, with its reason, before the second approval. A run that
  // names its own results.json states every result its world holds.
  const firstPush = (world["pushes.log"] ?? "").split("\n")[0]?.split(" ")[3];
  if (files["results.json"] === undefined && world["case.txt"] === "correct\n" && firstPush)
    world["results.json"] = JSON.stringify([reviewerHolds(firstPush)]);
  run(name, world, transcripts.implementer ?? fixReadsReviewing);
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
  if (transcripts.reviewer === undefined && world["case.txt"] === "accept\n") return;
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
/** The implementer's `gh pr comment` naming the head its push made (score.ts's `prComment`). */
const pushComment = (commit: string, at = "2026-09-30T17:35:30.000Z") => ({
  at,
  as: "gh",
  by: "implementer",
  argv: [
    "pr",
    "comment",
    "1001",
    "--body",
    `Pushed ${commit.slice(0, 12)}: the text the threads named now says what the CLI does.`,
  ],
  exit: 0,
});
/** The pass, done above `commit`: the implementer's reply on each thread and its comment naming
 * the commit, and no resolution of its own; then the reviewer's own listing, one resolution per
 * thread, and the second approval at that head. Its seven calls keep their places, which the rows
 * below take apart by index: the replies at 0 and 1, the comment at 2, the listing at 3, the
 * resolutions at 4 and 5, and the approval at 6. */
const closedAbove = (commit: string) => [
  threadReply(fixIds[0] ?? "", "2026-09-30T17:31:00.000Z", commit),
  threadReply(fixIds[1] ?? "", "2026-09-30T17:31:10.000Z", commit),
  pushComment(commit),
  threadsList("2026-09-30T17:36:00.000Z"),
  threadResolve([fixIds[0] ?? ""], "2026-09-30T17:37:00.000Z"),
  threadResolve([fixIds[1] ?? ""], "2026-09-30T17:37:10.000Z"),
  secondApproval(commit),
];
/** The pass at the commit the two text fixes are in, which is the push every run below makes
 * unless it names another. */
const closedBoth = closedAbove(fix.fixed);
fixRun("threads-closed-correct-head-1", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
// One call resolving both threads: the bulk resolution the reference forbids.
fixRun("threads-closed-correct-head-3", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 4),
    threadResolve(fixIds as string[], "2026-09-30T17:37:00.000Z"),
    closedBoth[6] ?? {},
  ]),
  "pushes.log": pushed(fix.fixed),
});
// The commit carries the code with the text fixes, which a push of text does not. Both messages
// name the head that push made, so the run fails on the diff alone.
fixRun("threads-closed-correct-head-4", {
  "calls.jsonl": calls([...closedBoth.slice(0, 6), secondApproval(fix.withCode)]),
  "pushes.log": pushed(fix.withCode),
  "published.jsonl": bus(toldReviewerOfPush(fix.withCode), toldArchitectOfHead(fix.withCode)),
});
// Everything, and no completion: this loop runs in no phase and writes no handoff, so a run that
// completed nothing passes. Its two wakes name the head by its first twelve characters, which is
// how a message names a commit (`namesCommit`): a prefix of seven hexadecimal digits or more
// names the head it is a prefix of.
fixRun("threads-closed-correct-head-6", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([]),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed.slice(0, 12)),
    toldArchitectOfPush(fix.fixed.slice(0, 12)),
    toldArchitectOfHead(fix.fixed.slice(0, 12))
  ),
});
// The first thread resolved twice: the second call names that one thread again, and the
// reference asks only for one thread per call.
fixRun("threads-closed-correct-head-7", {
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolve([fixIds[0] ?? ""], "2026-09-30T17:37:20.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Two pushes of the one change: the rule gives the implementer one push, and a second push is a
// second head for the reviewer to read and the architect to see.
fixRun("threads-closed-correct-head-10", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed) + pushed(fix.fixed).replace("17:35:00", "17:36:00"),
});
// A resolution of a bot thread the reviewer adjudicated, in a call of its own. That thread is
// none of the scenario's two, so it is in no threads.json, and the world holds it beside them.
// One call, one thread: no bulk resolution.
fixRun("threads-closed-correct-head-11", {
  "fixtures.json": withBotThreads(fixThreadsFile),
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolve(["PRRT_kwDOrig0001"], "2026-09-30T17:37:30.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// A body that quotes the resolution command and names every thread it closed resolves nothing:
// the call that resolves a thread is the `gh api graphql` mutation, and a `gh pr edit` is not it.
fixRun("threads-closed-correct-head-12", {
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
// Every act right, and no `reviewThreads` query of the reviewer's: it resolved threads whose
// state it read somewhere other than GitHub. The reviewer's prompt has it list the pull request's
// threads itself and verify their state there, never from the body.
fixRun("threads-closed-correct-head-13", {
  "calls.jsonl": calls(closedBoth.filter((call) => call !== closedBoth[3])),
  "pushes.log": pushed(fix.fixed),
});
// The implementer resolved a thread: the resolution is the reviewer's, and the implementer
// resolves none.
fixRun("threads-closed-correct-head-14", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 3),
    threadResolve([fixIds[0] ?? ""], "2026-09-30T17:34:30.000Z", "implementer"),
    ...closedBoth.slice(3),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// The second approval names the head the reviewer approved the first time, not the head the
// push made: the merge gate would read an approval of a head the fix is not on.
fixRun("threads-closed-correct-head-15", {
  "calls.jsonl": calls([...closedBoth.slice(0, 6), secondApproval(fix.approved)]),
  "pushes.log": pushed(fix.fixed),
});
// Run 3787e2d71dfa's shape, in a world that names each role App's bot as rig.sh writes every
// run's (world.json's `logins`, and accounts.json linking each bot's commit address to it): the
// implementer's one push, its commit the implement App's bot's, named after it in a message to
// the reviewer's topic and one to the architect's; then the reviewer's own push above it, its
// commit the review App's bot's, and the reviewer's approval of that later head. head-109's push
// fixes both documents; head-110's, as 3787e2d71dfa's did, fixes the workflow's comment alone and
// leaves docs/cli.md's sentence false, which the reviewer's own push then fixes.
const reviewerPushedAbove = (implementer: string, reviewer: string) => ({
  "world.json": JSON.stringify({
    ...JSON.parse(worldOf(fix.approved)),
    logins: { implement: IMPLEMENT_BOT.login, review: REVIEW_BOT.login },
  }),
  "accounts.json": JSON.stringify([IMPLEMENT_BOT, REVIEW_BOT]),
  "calls.jsonl": calls([...closedAbove(implementer).slice(0, 6), secondApproval(reviewer)]),
  "pushes.log": `${pushed(implementer)}2026-09-30T17:37:30.000Z refs/heads/legion/LWEVAL-1 ${implementer} ${reviewer}\n`,
  "published.jsonl": bus(
    toldReviewerOfPush(implementer, "2026-09-30T17:35:40.000Z"),
    toldArchitectOfPush(implementer, "2026-09-30T17:35:45.000Z"),
    toldArchitectOfHead(reviewer)
  ),
});
fixRun(
  "threads-closed-correct-head-109",
  reviewerPushedAbove(fix.implementerBoth, fix.reviewerAboveBoth)
);
fixRun(
  "threads-closed-correct-head-110",
  reviewerPushedAbove(fix.implementerWorkflow, fix.reviewerAboveWorkflow)
);
// The reviewer half never started: the run holds the implementer's work and nothing of the
// reviewer's, either because no transcript was written (head-17) or because the one written
// carries no assistant message (head-18). Each would otherwise be a failure of the text under
// test, and neither measured the loop at all.
fixRun(
  "threads-closed-correct-head-17",
  { "calls.jsonl": calls(closedBoth.slice(0, 3)), "pushes.log": pushed(fix.fixed) },
  { reviewer: null }
);
fixRun(
  "threads-closed-correct-head-18",
  { "calls.jsonl": calls(closedBoth.slice(0, 3)), "pushes.log": pushed(fix.fixed) },
  { reviewer: [...header] }
);
// Each thread resolved by one call that also reads the mutation's answer back with --jq: one
// thread per call, and the field named twice. The run did the loop and passes.
fixRun("threads-closed-correct-head-19", {
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
// code-defect arm scores that (`moved === undefined`) and the correct arm did not, so a run
// that did every act of the loop and then spent the round it was meant to save passed.
fixRun("threads-closed-correct-head-20", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([
    refusedCompletion,
    movedBack("the text fixes are in, moving it back anyway"),
  ]),
});
// Every act of the loop right, and the implementer told the reviewer nothing: the daemon starts
// no phase for this step, so the reviewer learns the head its push left
// from that message and from nothing else.
fixRun("threads-closed-correct-head-21", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(toldArchitectOfPush(fix.fixed), toldArchitectOfHead(fix.fixed)),
});
// Every act of the loop right, and the reviewer told the architect nothing of its approval,
// which the reviewer's role prompt does not ask of this loop: the row reports it and scores it
// nowhere.
fixRun("threads-closed-correct-head-22", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(toldReviewerOfPush(fix.fixed), toldArchitectOfPush(fix.fixed)),
});
// Every act of the loop right, and one wake sent to the wrong role's topic: the implementer
// told the architect that the fixes are pushed and the reviewer nothing (head-23), and the
// reviewer told the implementer which head it approved (head-24). The reviewer reads its own
// topic alone, so head-23's waits on; head-24's message is the reviewer's report, which scores
// nothing.
fixRun("threads-closed-correct-head-23", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo("architect", `The text fixes are pushed: the head the reviewer reads is ${fix.fixed}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
fixRun("threads-closed-correct-head-24", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    toldArchitectOfPush(fix.fixed),
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
  "threads-closed-correct-head-25",
  {
    "calls.jsonl": calls(closedBoth),
    "pushes.log": pushed(fix.fixed),
    "published.jsonl": bus(toldArchitectOfPush(fix.fixed), toldArchitectOfHead(fix.fixed)),
  },
  {
    implementer: [
      ...header,
      ...turn,
      publishCall("reviewer", `The text fixes are pushed: the head to read is ${fix.fixed}.`),
    ],
  }
);
fixRun("threads-closed-correct-head-26", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads a record the recorder did not finish, which would report every
// message it missed as a message never sent — the parser's old fault in a new place. head-27's
// recorder subscribed and died before the run ended (no closing line); head-28's never
// subscribed at all, and the file holds the messages of some other reader. Each is a rig error,
// out of both counts.
fixRun("threads-closed-correct-head-27", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": busLines(recordingLine(), toldReviewerOfPush(fix.fixed)),
});
fixRun("threads-closed-correct-head-28", {
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
fixRun("threads-closed-correct-head-32", {
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
fixRun("threads-closed-correct-head-30", {
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
fixRun("threads-closed-correct-head-29", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed),
    sentTo("tester", `For your notes: the text fixes are at ${fix.fixed}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Catches: a scorer that reads a `git diff` the run's own remote refused as a push that carried
// no file. head-33's push log names a commit the remote does not hold, so the diff of that push
// cannot be read at all, and the empty answer a failed `git diff` prints is what a push changing
// nothing looks like: the run is a rig error naming the command, never a row scored on a diff
// nobody read.
fixRun("threads-closed-correct-head-33", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed("f".repeat(40)),
});
// Catches: a scorer that reads "every thread resolved" as "one of them was". head-34's reviewer
// listed the threads and resolved the first, leaving the second open while the body says the
// fix closed it, which is the state the loop exists to end.
fixRun("threads-closed-correct-head-34", {
  "calls.jsonl": calls(closedBoth.filter((call) => call !== closedBoth[5])),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that reads the reviewer's last submission as a verdict of any kind, so a
// `COMMENT` at the head the fix landed on — which the merge gate reads as no approval at all —
// closes the loop. head-35 did every other act and left the second approval unmade.
fixRun("threads-closed-correct-head-35", {
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
fixRun("threads-closed-correct-head-36", {
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
fixRun("threads-closed-correct-head-37", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 6),
    { ...secondApproval(fix.fixed), by: "implementer" },
  ]),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-correct-head-38", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 3),
    threadsList("2026-09-30T17:36:00.000Z", "implementer"),
    ...closedBoth.slice(4),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that asks the push's diff to touch the text's files and not to touch all of
// them. head-42's one commit fixes docs/cli.md and leaves the workflow's comment as it was, so a
// thread the reviewer is about to resolve has nothing closing it, and every other act of the
// last chance was done above that commit.
fixRun("threads-closed-correct-head-42", {
  "calls.jsonl": calls(closedAbove(fix.docOnly)),
  "pushes.log": pushed(fix.docOnly),
  "published.jsonl": bus(toldReviewerOfPush(fix.docOnly), toldArchitectOfHead(fix.docOnly)),
});
// Catches: a scorer that reads "one thread per call" off the call's text rather than off the
// threads the world resolved at it (the architect's ruling of 2026-11-18T08:40Z: how many threads
// a call resolved is how many the world's replay resolved at that call). head-44 resolves the
// first thread with a call that invokes the mutation twice on it — one thread, resolved twice,
// which a count of the mutations read as two; head-45 adds a call that resolves one bot thread
// and names a second in the query's own comment — two ids, one resolution, which a count of the
// ids read as two. Each resolved one thread at each call, as head-1 does, and passes. head-3's
// bulk call resolves two threads at one call, and head-46's two bot threads.
fixRun("threads-closed-correct-head-44", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 4),
    threadResolveTwice(fixIds[0] ?? "", "2026-09-30T17:37:00.000Z"),
    ...closedBoth.slice(5),
  ]),
  "pushes.log": pushed(fix.fixed),
});
fixRun("threads-closed-correct-head-45", {
  "fixtures.json": withBotThreads(fixThreadsFile),
  "calls.jsonl": calls([
    ...closedBoth,
    threadResolveNaming("PRRT_kwDOrig0001", "PRRT_kwDOrig0002", "2026-09-30T17:37:30.000Z"),
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a scorer that asks every thread to be resolved and stops there, so a run that
// resolved each of its own threads one call each and then closed two bot threads in one call
// passes although the reference forbids that call.
fixRun("threads-closed-correct-head-46", {
  "fixtures.json": withBotThreads(fixThreadsFile),
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
fixRun("threads-closed-correct-head-47", {
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
  "threads-closed-correct-head-48",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  { reviewer: [...turn] }
);
// Catches: a check that reads the run's own agent's transcript and not the half booted after it.
// head-50's reviewer had its daemon answer refused by its own checkout's contract, so that half
// could not read its record and spent its turns on the daemon; the run says nothing about the
// loop, in whichever half the refusal fell.
fixRun(
  "threads-closed-correct-head-50",
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
// rule is one push onto that branch, and head-51's implementer also pushed a scratch ref of its
// own, which is no head the reviewer or the architect reads: the last chance it ran is the clean
// one.
fixRun("threads-closed-correct-head-51", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": `${pushed(fix.fixed)}2026-09-30T17:36:00.000Z refs/heads/scratch/LWEVAL-1-notes ${fix.approved} ${fix.withCode}\n`,
});
// Catches: a scorer that reads the record for the message and not for who sent it. head-52's
// message to the reviewer's topic names the new head and both threads, and the reviewer sent
// it, so the implementer told it nothing and the half that had to read the push learned of it
// from itself.
fixRun("threads-closed-correct-head-52", {
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
fixRun("threads-closed-correct-head-53", {
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
fixRun("threads-closed-correct-head-54", {
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
// Catches: a scorer that reads the mutation's name in any gh call as a resolution, where head-12
// reads it in a `gh pr edit`. head-56's reviewer posted a conversation comment with
// `gh api --method POST repos/example/widgets/issues/1001/comments` quoting the command it ran
// and naming both threads: a call that is no `gh api graphql` resolves nothing, and reading it
// as one would score the run a bulk resolution for saying what it did.
fixRun("threads-closed-correct-head-56", {
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
// Catches: a scorer that credits the run with the reference when one of its halves read it. Each
// half acts on its own half of the loop, so each must have read where that half is defined:
// head-58's implementer read it and its reviewer, which resolved the threads and approved the
// head, did not. The run passes, since `ref` is counted beside the pass and not in it.
fixRun(
  "threads-closed-correct-head-58",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  {
    implementer: [
      ...fixReadsReviewing,
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
fixRun("threads-closed-correct-head-59", {
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
fixRun("threads-closed-correct-head-60", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    toldReviewerOfPush(fix.fixed, "2026-09-30T17:30:00.000Z"),
    toldArchitectOfHead(fix.fixed)
  ),
});
fixRun("threads-closed-correct-head-61", {
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
fixRun("threads-closed-correct-head-62", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(toldReviewerOfPush(fix.fixed, ""), toldArchitectOfHead(fix.fixed)),
});
// Catches: a count of the completions the stand-in accepted, which reports nothing the last
// chance's rule asks about: the reviewer's completion of its own `reviewing` in correct is
// accepted, and the implementer's is refused 409 HANDOFF_NOT_CURRENT_PHASE, `reviewing` being no
// phase of its own. head-63's daemon.log holds the accepted completion and no refusal: its row
// reads 0, and it is no rig error, where head-1's, holding the refusal the rig writes, reads 1.
fixRun("threads-closed-correct-head-63", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([completed]),
});
// A completion the stand-in refused in any way but the daemon's own HANDOFF_NOT_CURRENT_PHASE is
// a rig error (score.ts's `unscored`): head-95's implementer completed with a record on a phase
// that ends with none, 409 HANDOFF_RECORD_MISMATCH, which its tool never sends; head-96's line is
// a 409 an older stand-in wrote with no code, which cannot say which refusal it was. Each is no
// answer of either daemon shape, so the run's own daemon, this checkout's, is read for it.
fixRun("threads-closed-correct-head-95", {
  "built-from": `checkout ${CHECKOUT}\n`,
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([{ ...refusedCompletion, code: "HANDOFF_RECORD_MISMATCH" }]),
});
fixRun("threads-closed-correct-head-96", {
  "built-from": `checkout ${CHECKOUT}\n`,
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([completed, { ...completed, status: 409 }]),
});
// head-101: head-1's implementer refusal, 409 HANDOFF_NOT_CURRENT_PHASE, on a line that names no
// role. The stand-in names the role it serves on every line, and no role is guessed for a
// completion whose line names none, so whose completion it was is unknown: a rig error, where
// the guess read it as the implementer's and scored it.
fixRun("threads-closed-correct-head-101", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "daemon.log": calls([{ ...refusedCompletion, role: undefined }]),
});
// head-98: the reviewer's second approval on the two-step route, its create naming no head and
// sending no body, after the implementer's push. GitHub creates that review at the pull
// request's head when the create is made, which is the head the push left, and the gh stand-in
// answers it there (gh-call.ts's `createdHead`, given its remote's tip at the call); the score
// reads it at the head <run>/pushes.log had left the branch at when the call was recorded, so
// the round approved the fix head and passes as head-1 does. Catches: a create naming no head
// read at `""`, which failed `namesHead` on an approval the world had answered at the fix head,
// and one read at the world's first head, which the push had already left behind.
fixRun("threads-closed-correct-head-98", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 6),
    {
      at: "2026-09-30T17:37:50.000Z",
      as: "gh",
      by: "reviewer",
      argv: ["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews"],
      exit: 0,
    },
    { ...submitPending(), at: "2026-09-30T17:38:00.000Z", by: "reviewer" },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-99: the reviewer's second approval written `gh pr review 1001 --approve`, after the
// implementer's push. The call names no head; GitHub takes it at the pull request's head when it
// is made, which is the head the push left, and the gh stand-in answers it there (its remote's
// tip at the call); the score reads it at the head <run>/pushes.log had left the branch at when
// the call was recorded, so the round approved the fix head and passes as head-1 does. Catches:
// a `gh pr review` read at `""`, which failed `namesHead` on an approval the world had answered
// at the fix head, and one read at the world's first head, which the push had left behind.
fixRun("threads-closed-correct-head-99", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 6),
    {
      at: "2026-09-30T17:38:00.000Z",
      as: "gh",
      by: "reviewer",
      argv: ["pr", "review", "1001", "--approve", "--body", "The text fixes close both threads."],
      exit: 0,
    },
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-104: the loop head-1 passes on, with each of the reviewer's resolving calls reading its
// answer back through a `--jq` selector that names the other thread. The world resolved one
// thread at each call, the one the mutation's `input.threadId` names, so the run passes.
fixRun("threads-closed-correct-head-104", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 4),
    ...fixIds.map((id, index) => {
      const resolve = threadResolve([id], `2026-09-30T17:37:${index}0.000Z`);
      const other = fixIds[1 - index] ?? "";
      return {
        ...resolve,
        argv: [...resolve.argv, "--jq", `.data.t0.thread | select(.id != "${other}") | .id`],
      };
    }),
    closedBoth[6] ?? {},
  ]),
  "pushes.log": pushed(fix.fixed),
});
// head-107: the loop head-1 passes on, with the reviewer's listing and both resolutions sent to
// the endpoint written `/graphql`, which gh posts to GitHub's GraphQL endpoint as it does
// `graphql` and the world answers and resolves the same way (gh-threads.ts's `graphqlOf`).
fixRun("threads-closed-correct-head-107", {
  "calls.jsonl": calls(
    closedBoth.map((call, index) =>
      index >= 3 && index <= 5 && "argv" in call
        ? { ...call, argv: call.argv.map((arg) => (arg === "graphql" ? "/graphql" : arg)) }
        : call
    )
  ),
  "pushes.log": pushed(fix.fixed),
});
// head-108: the loop head-1 passes on, with the reviewer's second resolution sent in a call that
// also resolves a node id the world has not got. GitHub answers that field NOT_FOUND and gh exits
// 1 on the error, and the world resolved the second thread at the call all the same, since a
// mutation's other fields keep what they wrote (gh-threads.ts's `threadCall`): one thread at
// that call.
fixRun("threads-closed-correct-head-108", {
  "calls.jsonl": calls([
    ...closedBoth.slice(0, 5),
    { ...threadResolve([fixIds[1] ?? "", "PRRT_kwDOfix00x"], "2026-09-30T17:37:10.000Z"), exit: 1 },
    closedBoth[6] ?? {},
  ]),
  "pushes.log": pushed(fix.fixed),
});
// Catches: a reader of "one thread per call" that counts the ids a call's text names, which
// reads a call naming none as a bulk call. head-64's reviewer resolved the first thread again
// with the id in a file (`-F id=@thread-id.txt`, which gh reads the value of from that file, and
// whose text the stand-in records in the call's `files`), and the world resolved that one thread
// at it: a third resolving call, each of one thread. head-3's bulk call resolves two.
fixRun("threads-closed-correct-head-64", {
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
      files: { "thread-id.txt": fixIds[0] ?? "" },
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
  fixRun(`threads-closed-correct-${name}`, {
    "calls.jsonl": calls(closedBoth),
    "pushes.log": pushed(fix.fixed),
    [file]: null,
  });
// head-68: the three records there and holding no line of the run's own — each stand-in's
// opening line, and the push log rig.sh empties — which is the other reading: the run made no
// call, pushed nothing and moved nothing. So it scores, its row names each of them, and it
// fails on every act of the loop.
fixRun("threads-closed-correct-head-68", {
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
fixRun("threads-closed-correct-head-69", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": "",
});
fixRun("threads-closed-correct-head-70", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": null,
});
// head-71: the whole loop, and a `COMMENT` review of the reviewer's after its second approval —
// the compare it read, posted as a review of its own, which on GitHub leaves the approval
// standing. The verdict is the round's last `APPROVE` or `REQUEST_CHANGES`, so the run passes;
// a reader of the reviewer's last submission of any kind scored this `approved=false` and
// failed a run that did every act of the loop.
fixRun("threads-closed-correct-head-71", {
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
// where rig.sh's worker_boot moves it when the reviewer half boots. Its built-from names this
// checkout, whose plugin's wording of that sentence the score reads (`claimBootOpening`).
fixRun("threads-closed-correct-head-72", {
  "built-from": `checkout ${CHECKOUT}\n`,
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
// head-100: head-72's death, with the claim boot's sentence the whole of the half's output — no
// prefix before it and no line end after it — where both hosts write it as `[legion] <line>\n`.
// The reading is the sentence wherever it falls: from the first character, and to the end of
// the output when no line ends it.
fixRun("threads-closed-correct-head-100", {
  "built-from": `checkout ${CHECKOUT}\n`,
  "calls.jsonl": calls(closedBoth.slice(0, 1)),
  "out-implementer.txt":
    "claim boot failed at claims/ready after claims/register registered legion-skills1-lweval-1-implementer with the daemon at http://127.0.0.1:38411; exiting so the daemon launches it again: ECONNREFUSED",
  "published.jsonl": bus(),
  "daemon.log": calls([
    { at: "2026-09-30T17:30:01.000Z", path: "/legion/v1/claims/register", status: 200 },
  ]),
});
// base-2: head-72's death in a base run, worded as main ff2a10ef's plugin words it, with no step
// and no daemon (packages/pi-shared/src/legion-claim-core.ts:97 there). Its built-from names a
// checkout holding that module's sentence alone, as the label's own checkout holds it.
stagings.push(() => {
  mkdirSync(path.dirname(path.join(BASE_CLAIM_CHECKOUT, CLAIM_CORE)), { recursive: true });
  writeFileSync(
    path.join(BASE_CLAIM_CHECKOUT, CLAIM_CORE),
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the base module's source, verbatim.
    "    host.log(\n      `claim boot failed after claims/register registered ${claim.claimToken}; ${host.stopping}: ${messageFor(error)}`\n    );\n"
  );
});
fixRun("threads-closed-correct-base-2", {
  "built-from": `checkout ${BASE_CLAIM_CHECKOUT}\n`,
  "calls.jsonl": calls(closedBoth.slice(0, 1)),
  "out-implementer.txt":
    "[legion] claim boot failed after claims/register registered legion-skills1-lweval-1-implementer; exiting so the daemon launches it again: ECONNREFUSED\nexit=1\n",
  "published.jsonl": bus(),
  "daemon.log": calls([
    { at: "2026-09-30T17:30:01.000Z", path: "/legion/v1/claims/register", status: 200 },
  ]),
});
// head-73 and head-74: the last chance done above the head the push made, with every name of that
// head written as its first seven characters (head-73) and as its first six (head-74) — the
// implementer's comment, its messages to the reviewer and the architect, and the reviewer's
// message to the architect. Seven hexadecimal digits are the fewest that name one commit, which
// is what `namesCommit` asks of each, so head-73 passes and head-74 names no commit anywhere and
// fails on each of the three the score asks for. Catches: that floor moved either way — at eight,
// a run naming the head by git's own default short SHA would read as a run that named no head;
// at six, a run leaving a reader too many commits to find the fix would pass. The approval names
// the whole head in both, since a verdict's head is read by `namesHead` and a six-character
// `commit_id` would fail that under every reading, which would leave head-74 failing whatever
// this floor is.
for (const [name, length] of [
  ["head-73", 7],
  ["head-74", 6],
] as const) {
  const named = fix.fixed.slice(0, length);
  fixRun(`threads-closed-correct-${name}`, {
    "calls.jsonl": calls([
      {
        ...pushComment(fix.fixed),
        argv: ["pr", "comment", "1001", "--body", `Pushed ${named}: the text now says it.`],
      },
      threadsList("2026-09-30T17:36:00.000Z"),
      threadResolve([fixIds[0] ?? ""], "2026-09-30T17:37:00.000Z"),
      threadResolve([fixIds[1] ?? ""], "2026-09-30T17:37:10.000Z"),
      secondApproval(fix.fixed),
    ]),
    "pushes.log": pushed(fix.fixed),
    "published.jsonl": bus(
      toldReviewerOfPush(named),
      toldArchitectOfPush(named),
      toldArchitectOfHead(named)
    ),
  });
}
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
  fixRun(`threads-closed-correct-${name}`, {
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
fixRun("threads-closed-correct-head-78", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": `${pushed(fix.fixed)}2026-09-30T17:36:00.000Z refs/tags/legion/LWEVAL-1 ${"0".repeat(40)} ${fix.withCode}\n`,
});
// head-81: the implementer's message names both threads the push answered and no head at all.
// Catches: a reader that takes a name of the push other than its head for the head, which
// passes a message the reviewer cannot act on: told which threads were answered and not which
// head to read, it has the whole branch to search. head-59 is the message with the head alone.
fixRun("threads-closed-correct-head-81", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    sentTo("reviewer", `The text fixes are pushed, closing ${fixIds[0]} and ${fixIds[1]}.`),
    toldArchitectOfHead(fix.fixed)
  ),
});
// head-83: the whole loop, with the implementer posting a `COMMENT` review of its own after the
// reviewer's wake of the architect — the compare it was asked to show, left on the pull request
// as a review. Catches: a reader that orders that wake against the round's last submission of
// any kind rather than against the reviewer's own approval, which fails a run whose architect
// was told the moment the approval stood because the other half spoke after it. head-71 is the
// reviewer's own `COMMENT` after its approval, which leaves the verdict standing.
fixRun("threads-closed-correct-head-83", {
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
  "threads-closed-correct-head-84",
  { "calls.jsonl": calls(closedBoth), "pushes.log": pushed(fix.fixed) },
  {
    implementer: [...fixReadsReviewing, ...readOf("call-skill", "skill://legion-worker/SKILL.md")],
    reviewer: [...secondHeader, ...turn, ...readOf("call-skill", "skill://legion-worker/SKILL.md")],
  }
);
// head-85: the whole loop, and a pull-request comment of the reviewer's that says what it ran,
// quoting `gh api graphql` and the mutation and naming both threads. Catches: a reader that
// takes any call whose text names graphql and the mutation for a resolution, where the call
// that resolves a thread is `gh api graphql` and a `gh pr comment` is not it: the quoted
// command names two threads, so the run would score a bulk resolution for saying what it did.
// head-12's body quotes the mutation without the word graphql in it.
fixRun("threads-closed-correct-head-85", {
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
fixRun("threads-closed-correct-head-87", {
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
fixRun("threads-closed-correct-head-86", {
  "threads.json": oddThreadsFile,
  "calls.jsonl": calls([
    pushComment(fix.fixed),
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
    toldArchitectOfPush(fix.fixed),
    toldArchitectOfHead(fix.fixed)
  ),
});
// Another label, for the counts.
fixRun("threads-closed-correct-base-1", {
  "calls.jsonl": calls(closedBoth),
  "pushes.log": pushed(fix.fixed),
});
// code-defect: the thread no text fix closes (Verification v47's k2 negative control). The pass is
// no commit, no result of the implementer's, a message of the implementer's to the architect and
// one to the reviewer, each naming the thread, and no move back. This world pushes nothing, so
// neither wake of the correct loop belongs in it: each run below states every message its two
// halves sent.
/** The implementer's message `text` on the architect's role topic and on the reviewer's. */
const toldBoth = (text: string) => [sentTo("architect", text), sentTo("reviewer", text)];
const defectMessage =
  "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong, so no text fix closes it.";
fixRun("threads-closed-code-defect-head-1", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z"),
  ]),
  "published.jsonl": bus(...toldBoth(defectMessage)),
});
// Wrote the text over the defect and pushed it: the negative control's own failure. Catches: a
// scorer that reads the messages and not what the run did beside them, since a thread whose
// sentence is false only because the code is wrong is closed by no text, however well the
// architect and the reviewer were told. head-2 did every other act of the case, so the push is
// the one thing it did wrong, and the runs below are the same for each of the other terms.
fixRun("threads-closed-code-defect-head-2", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z", fix.fixed)]),
  "pushes.log": pushed(fix.fixed),
  "published.jsonl": bus(
    ...toldBoth(
      "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: I wrote the sentence to match the code anyway."
    )
  ),
});
// The messages, and then the role moved the issue itself: the architect moves it back to
// implementing, and neither the implementer nor the reviewer does.
fixRun("threads-closed-code-defect-head-3", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadReply("PRRT_kwDOfix101", "2026-09-30T17:31:00.000Z")]),
  "daemon.log": calls([
    movedBack(
      "Thread PRRT_kwDOfix101 says a blank name exits 2, and the CLI greets: the code is wrong."
    ),
  ]),
  "published.jsonl": bus(...toldBoth("Thread PRRT_kwDOfix101 is false because the code is wrong.")),
});
/** The implementer's own message to the reviewer in code-defect, which gives that case its second
 * half: rig.sh boots the reviewer on the messages the implementer published to its topic and
 * boots none where it published none (reviewer_wake). It names the thread, so it is the
 * reviewer's half of `told`; it tells the architect nothing. */
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
// The same `REQUEST_CHANGES`, and a message to the architect beside it, sent by the reviewer: the
// submission is the finding's record on GitHub and scores nothing, and the architect's message
// `told` asks for is the implementer's, which this run's is not.
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
// The reviewer approved the head: in `accepting` the daemon records no review, and v47's control
// scores no approval, so the implementer's two messages are what the run is read on.
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
// The finding named, and sent to the reviewer's topic alone: the architect is the one role that
// can move the issue for it, so `told` asks for the architect's message too.
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
// The half the run never booted: rig.sh boots no second agent on a wake of none
// (run_threads_closed), so the run has one half and no sessions/reviewer at all, and is scored on
// the half it ran. Its implementer sent the reviewer nothing, which fails `told`. The correct runs
// above (head-17, head-18) are the same shape in the case where the wake was owed, and stay rig
// errors.
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
// A thread no text fix closes stays open until the code is right, and v47's control scores no
// resolution: the reviewer resolved the thread (head-11), it made a call that resolves none of the
// scenario's threads and two bot threads its world holds (head-13), and the implementer resolved
// it (head-12). Each told the architect and the reviewer, pushed nothing, recorded nothing and
// moved nothing, so each passes, and its notes report the resolution (`fixResolves`,
// `resolveCalls`).
const defectTold = bus(...toldBoth(defectMessage));
/** The architect's message alone, from a one-half run that woke no reviewer (head-10, head-17). */
const defectToldArchitect = bus(sentTo("architect", defectMessage));
/** The implementer's message to the reviewer and the one to the architect, the record the rig
 * booted a reviewer on that acted. */
const defectWokeReviewer = bus(askedReviewer, sentTo("architect", defectMessage));
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
  "fixtures.json": withBotThreads(defectThreadsFile),
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z"),
    threadResolve(["PRRT_kwDOrig0001", "PRRT_kwDOrig0002"], "2026-09-30T17:37:00.000Z"),
  ]),
  "published.jsonl": defectWokeReviewer,
});
// The implementer approved this head: v47's control scores no approval, so head-14, which told
// the architect and the reviewer and did every other act of the case, passes, its notes reporting
// the approval (`approvedAny`).
fixRun("threads-closed-code-defect-head-14", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([
    threadsList("2026-09-30T17:30:00.000Z", "implementer"),
    { ...secondApproval(fix.approved), by: "implementer" },
  ]),
  "published.jsonl": defectTold,
});
// Catches: a scorer that reads the messages for the thread's node id alone. The finding is the
// thread's, and head-15 named it by the file its sentence is on, which is what the architect acts
// on and what a message about a document naturally says.
fixRun("threads-closed-code-defect-head-15", {
  "case.txt": "code-defect\n",
  "threads.json": defectThreadsFile,
  "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z", "implementer")]),
  "published.jsonl": bus(
    ...toldBoth(
      "The sentence in docs/cli.md is false because the code is wrong: a blank name must exit 2, and the CLI greets."
    )
  ),
});
// Catches: a reader that lets a code-defect run off the half rule altogether, rather than off
// the half the rig never booted. head-16's implementer woke the reviewer, so the rig booted one,
// and that half got no model turn: it measured nothing of the loop, as head-18 measures nothing
// of the correct one, and the run is the rig error that says so. The case excuses the absent
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
// and there is no second half to ask.
fixRun(
  "threads-closed-code-defect-head-17",
  {
    "case.txt": "code-defect\n",
    "threads.json": defectThreadsFile,
    "calls.jsonl": calls([threadsList("2026-09-30T17:30:00.000Z", "implementer")]),
    "published.jsonl": defectToldArchitect,
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
// accept: the reviewer's commit is the world's head and it left no thread, so the run is the
// implementer's answer alone and boots no reviewer (completion-answers.ts's SKIPS). head-1 is the
// answer k2 asks for: a `gh pr comment` naming the commit, a message to the architect naming it
// and saying it accepts it, no push and no resolution. head-2 is that answer with a thread of the
// pull request resolved by the implementer, a bot's the world holds, which the rule has it leave
// to the reviewer. head-3 accepts in its message and names the commit in a comment made through
// the conversation's REST route, which is no `gh pr comment`: the role prompt names that command.
// Catches: a scorer whose accept case reads no resolution, and one that takes any comment for
// the one the rule names.
const acceptComment = (commit: string) => ({
  at: "2026-09-30T17:31:00.000Z",
  as: "gh",
  by: "implementer",
  argv: ["pr", "comment", "1001", "--body", `Accepted ${commit.slice(0, 12)}: the fix is right.`],
  exit: 0,
});
const acceptedToArchitect = (commit: string) =>
  sentTo("architect", `I accept the reviewer's commit ${commit}; no thread is left.`);
for (const [name, files] of [
  ["head-1", {}],
  [
    "head-2",
    {
      "fixtures.json": withBotThreads("[]"),
      "calls.jsonl": calls([
        acceptComment(fix.fixed),
        threadResolve(["PRRT_kwDOrig0001"], "2026-09-30T17:31:30.000Z", "implementer"),
      ]),
    },
  ],
  [
    "head-3",
    {
      "calls.jsonl": calls([
        {
          ...acceptComment(fix.fixed),
          argv: [
            "api",
            "--method",
            "POST",
            "repos/example/widgets/issues/1001/comments",
            "-f",
            `body=Accepted ${fix.fixed.slice(0, 12)}: the fix is right.`,
          ],
        },
      ]),
    },
  ],
] as const)
  fixRun(`threads-closed-accept-${name}`, {
    "world.json": worldOf(fix.fixed),
    "case.txt": "accept\n",
    "threads.json": "[]",
    "calls.jsonl": calls([acceptComment(fix.fixed)]),
    "published.jsonl": bus(acceptedToArchitect(fix.fixed)),
    ...files,
  });

// head-200 and head-201: a reviewer round that pushed one commit above the head it read, as k1's
// text-only round does. head-200's results file holds a pass the reviewer's own session recorded
// at that commit, and its daemon log two moves back, one taken and one refused; head-201's holds
// the tester's pass at that commit and the reviewer's own at the head below it, and no move.
// Catches: a scorer that reads no result of the run (k1 (c) is "records no result of the world's
// check at its own commit"), that reads a pass at the commit by any session as the reviewer's own
// or the reviewer's own at any commit as one at its own, and one that reads no move back outside
// threads-closed, or only the moves the daemon took (k1 (d), and every "moves nothing").
const ownResult = (commit: string, session = FIRST_SESSION, role = "reviewer") => ({
  check: "k1",
  commit,
  session,
  role,
  verdict: "pass",
  observed: "prints Hello, Ada! and exits 0",
  at: "2026-09-30T17:36:00.000123Z",
});
const backwardMove = (to: string, status: number, code?: string) => ({
  at: "2026-09-30T17:41:00.000Z",
  method: "POST",
  path: "/legion/v1/phase/backward",
  status,
  ...(code === undefined ? {} : { code }),
  role: "reviewer",
  to,
});
for (const [name, files] of [
  [
    "reviewer-text-findings-text-only-head-200",
    {
      "results.json": JSON.stringify([ownResult(fix.fixed)]),
      "daemon.log": calls([
        ...reviewDaemon,
        backwardMove("testing", 200),
        backwardMove("implementing", 409, "BACKWARD_REFUSED"),
      ]),
    },
  ],
  [
    "reviewer-text-findings-text-only-head-201",
    {
      "results.json": JSON.stringify([
        ownResult(fix.fixed, "01a1b402-1f7d-7c55-8a3e-2b6d9f04c812", "tester"),
        ownResult(fix.approved),
      ]),
    },
  ],
] as const) {
  fix.remoteFor(name);
  reviewRun(name, {
    "world.json": worldOf(fix.approved),
    "case.txt": "text-only\n",
    "pushes.log": pushed(fix.fixed),
    "calls.jsonl": calls([submission(approval())]),
    ...files,
  });
}
/** What `score.ts runs` printed over the staged world, and the code it exited with. Both are
 *  read in a hook, below the one that stages that world. */
let out = "";
let scoredExitCode: number | null = null;
/** Every arm the runs of this file make, and how many runs it holds, scored or rig errors, as a
 * batch runner names each with `--arm` (score.ts holds each arm to the count it is given). A
 * fixture run added to an arm moves its count here. */
const FIXTURE_ARMS: readonly (readonly [arm: string, runs: number])[] = [
  ["ask-on-message:apart", 2],
  ["ask-on-message:archived", 1],
  ["ask-on-message:batches", 2],
  ["ask-on-message:head", 2],
  ["ask-on-message:shared", 2],
  ["ask-on-message:unrecorded", 1],
  ["askgate:facts:off", 3],
  ["askgate:facts:on", 10],
  ["askgate:facts:unrecorded", 1],
  ["askgate:link:off", 1],
  ["askgate:link:on", 9],
  ["askgate:nochange-block:on", 2],
  ["askgate:nochange-rebuttal:on", 3],
  ["askgate:nochange:on", 3],
  ["askgate:nochange:recorded", 1],
  ["askgate:nochange:unrecorded", 1],
  ["askgate:on", 1],
  ["askgate:tried:on", 4],
  ["askgate:untried:on", 2],
  ["askgate:upload:on", 4],
  ["brainstorm:head", 4],
  ["measure-before-ask:head", 3],
  ["reviewer-text-findings:defect:head", 15],
  ["reviewer-text-findings:head", 1],
  ["reviewer-text-findings:text-only:base", 3],
  ["reviewer-text-findings:text-only:head", 66],
  ["spec-rewrite:head", 1],
  ["spec-rewrite:older", 1],
  ["spec-writing:base", 3],
  ["spec-writing:flagged", 2],
  ["spec-writing:head", 107],
  ["spec-writing:older", 1],
  ["tester-proof:base", 2],
  ["tester-proof:cross", 5],
  ["tester-proof:head", 2],
  ["tester-proof:lines", 2],
  ["tester-proof:ran", 2],
  ["tester-proof:rec", 6],
  ["threads-closed:accept:head", 3],
  ["threads-closed:code-defect:head", 17],
  ["threads-closed:head", 1],
  ["threads-closed:correct:base", 2],
  ["threads-closed:correct:head", 81],
];
// Bun's default hook bound is 5s, and this one runs the whole scorer over every run of the file:
// three audit runs and two pods saw it killed at that bound, which reads as "0 pass, a
// beforeEach/afterEach hook timed out" and says nothing about the scorer. 120s is this file's
// own staging bound.
beforeAll(() => {
  const scored = spawnChecked([
    ...["bun", SCORE, "runs", runs, "--repo", LABEL_REPO],
    ...FIXTURE_ARMS.flatMap(([arm, count]) => ["--arm", `${arm}=${count}`]),
  ]);
  out = scored.stdout.toString();
  scoredExitCode = scored.exitCode;
}, 120_000);

/** The summary line `score.ts runs` prints for one scenario key and label, built from the run
 * rows of that same output: `<scenario>\t<label>\t<passed>/<scored>\t<with ref>/<scored>\t<rig
 * errors>\t<flagged>\t<expected>\t<found>`. The counts are what the summary is for — the table
 * read against the rows it aggregates — and the alternative is a line written by hand that every
 * fixture added to this file makes wrong. `flagged` is no part of a run row and no scenario
 * summarised this way flags one, so it is 0 here; the arm's expected and found counts are both
 * the rows it holds, as the score stops where they differ. */
function summaryLine(scenario: string, label: string): string {
  const prefix = `${scenario.replace(":", "-")}-${label}-`;
  const rows = out.split("\n").filter((line) => line.startsWith(prefix));
  const scored = rows.filter((line) => line.includes("\tpass="));
  const passed = scored.filter((line) => line.includes("\tpass=true\t"));
  const withRef = passed.filter((line) => line.includes("\tref=true\t"));
  const errors = rows.length - scored.length;
  return `${scenario}\t${label}\t${passed.length}/${scored.length}\t${withRef.length}/${scored.length}\t${errors}\t0\t${rows.length}\t${rows.length}\n`;
}

test("a run whose agent never got a model turn is a rig error, not a failure", () => {
  expect(scoredExitCode).toBe(0);
  for (const name of ["tester-proof-head-1", "ask-on-message-head-1"]) {
    expect(out).toContain(`${name}\trig error: `);
    expect(out).not.toContain(`${name}\tpass=`);
  }
  // Pass counts over the one run that got a turn; the other is counted only as a rig error.
  expect(out).toContain("tester-proof\thead\t0/1\t0/1\t1\t0\t2\t2\n");
  expect(out).toContain("ask-on-message\thead\t0/1\t0/1\t1\t0\t2\t2\n");
});

test("a run whose agent got a turn and did nothing is a failure", () => {
  for (const name of ["tester-proof-head-2", "ask-on-message-head-2"]) {
    expect(out).toContain(`${name}\tpass=false\t`);
  }
});

// Catches: a score that reads <work>/runs alone, where every run of a finished batch has moved
// to <work>/archive/runs (rig.sh's archive_run), so a batch would score no run at all.
test("a run in the work directory's archive is scored with the rest", () => {
  expect(out).toContain("ask-on-message-archived-1\tpass=false\tref=false\tasks=0 ");
});

test("the tester ran the CLI when the bun stand-in recorded a run of greet.ts, not when a command named it", () => {
  expect(out).toMatch(/tester-proof-ran-1\tpass=false\tref=false\tran=true /);
  expect(out).toMatch(/tester-proof-ran-2\tpass=false\tref=false\tran=false /);
});

test("a tester passes when its completion names the test record its own session wrote first", () => {
  expect(out).toMatch(
    /tester-proof-rec-1\tpass=true\tref=false\tran=true record=v1 proof=true pushes=0 complete=true refusedCompletions=1 ordered=true testerLine=true ownHead=true editBeforeRecord=false /
  );
  expect(out).toContain("tester-proof\trec\t1/6\t0/6\t0\t0\t6\t6\n");
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

// Catches: the tester's check line read past the part both labels' Dispatch print alike, so a
// head run's reading that names the head stands in for a commit the line's `at` never named, a
// pass only the head label's line can earn, while the base's line, ending in the check's words,
// is held to its `at` alone (score.ts's TESTER_LINE).
test("tester-proof: the check line's commit is its `at`, whatever tail the label's Dispatch prints", () => {
  expect(out).toMatch(/tester-proof-lines-1\tpass=true\t.* testerLine=true ownHead=true /);
  expect(out).toMatch(/tester-proof-lines-2\tpass=false\t.* testerLine=true ownHead=false /);
});

// Catches: a scorer that still passes main's round, an approval of the head the reviewer read with
// both findings left as comments and no commit of its own: k1 asks for the reviewer's text commit,
// no approval and no change request, and the workflow comment left as an open thread (the k1 rows
// near the end of this file). Its comments open no thread here, since the stand-in's record of the
// create carries no review it answered with, and each term is reported.
test("reviewer-text-findings, text-only: an approval of the head it read, both findings left as threads, fails", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-1\tpass=false\tref=false\tcase=text-only event=APPROVE head=true doc=true workflow=true toldImplementer=false submissions=1 resolveThread=0 pushes=0 reviewRecords=1 terms=a:false,b:false,c:true,d:true,e:false,f:false commit=false docFixed=false workflowOpen=false docOpen=false approved=true requestedAny=false moved=none toldBy=\[\] /
  );
  expect(out).toContain('commentPaths=["docs/cli.md",".github/workflows/greet.yml"]');
});

test("reviewer-text-findings, text-only: a result the reviewer recorded at its own commit is read, and so is each move back", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-200\tpass=false\t.* pushes=1 .* noOwnResult=false moves=\["testing 200","implementing 409 BACKWARD_REFUSED"\] /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-201\tpass=false\t.* pushes=1 .* noOwnResult=true moves=\[\] /
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

// Catches: a scorer blind to a thread resolution, or to an approval that names another head than
// the one it read; and one that scores a push whose diff it could not read. A resolution of the
// bot's thread is reported and scores nothing, and head-7's push names a commit no remote of the
// run holds, so the run is a rig error rather than a round scored on a diff nobody read.
test("reviewer-text-findings, text-only: a resolution is reported, another head fails, and a push whose diff cannot be read is a rig error", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-4\tpass=false\t.*resolveThread=1 /);
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-7\trig error: git -C .* diff --name-only /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-5\tpass=false\t.*event=APPROVE head=false /
  );
});

// Catches: the pushes of a review round counted on the issue branch alone. head-110 approved the
// head and said so, and pushed once to `refs/heads/scratch`: v31's text-only round makes one push,
// its text commit, onto the issue branch, and the scorer counts every push the run's remote took,
// whatever ref it moved (`pushes=1`), but takes the text commit only from one push onto that
// branch, so this push carries none and the round fails. head-7's one push onto the branch is read
// for its diff, a rig error there (the test above).
test("reviewer-text-findings, text-only: a push to another ref is a push of the round", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-110\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*pushes=1 /
  );
});

// Catches: a scorer that reads the resolution out of the call's arguments alone, so one sent
// through `--input` or a here-document goes uncounted and the row says a round resolved nothing
// when it resolved a thread. The count is reported rather than scored, and it must be right.
test("reviewer-text-findings, text-only: a resolveReviewThread in the call's body is counted", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-8\tpass=false\t.*resolveThread=1 /);
});

// Catches: a resolution read by the argument `graphql` itself, so one sent to the endpoint written
// `/graphql`, which the world answers and resolves as it answers `graphql` (gh-threads.ts's
// `graphqlOf`), is uncounted and the row says the round resolved nothing. head-146's reviewer
// resolved the bot's thread through `gh api /graphql`, and the world's replay holds it resolved.
test("reviewer-text-findings, text-only: a resolution through /graphql is the world's resolution", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-146\tpass=false\t.*resolveThread=1 /);
});

// Catches: a scorer that reads only `gh api`, so a `gh pr review` verdict goes unseen. The call
// names no head and GitHub takes it at the pull request's head, which with no push is the
// world's, so the row reads `head=true`, as the gh stand-in answered it; it carries no inline
// comment, which is why the text-only approval fails, and why the defect control's change
// request fails too: its text findings are no threads of it.
test("reviewer-text-findings: a gh pr review carries its event in a flag, is at the head, and carries no comment", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-6\tpass=false\t.*event=APPROVE head=true doc=false workflow=false /
  );
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-1\tpass=false\t.*event=REQUEST_CHANGES head=true doc=false workflow=false /
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
// routes serve unresolved whatever the run did, instead of the calls, the push and the messages
// the run left; and one that scores one half of the last chance and not the other.
test("threads-closed, correct: the text push named in a comment and two messages, then the resolutions and the approval of that head, pass", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-1\tpass=true\tref=false\tcase=correct threads=2 fixResolves=0 pushes=1 fixed=true commented=true toldReviewer=true toldArchitectOfPush=true accepted=false listed=true resolved=2 resolveCalls=2 bulk=false approved=true toldArchitectOfHead=true requested=false approvedAny=true told=false moved=none refusedCompletions=1/
  );
  expect(out).toContain('changed=[".github/workflows/greet.yml","docs/cli.md"]');
});
// Catches: the reviewer's approval of the fix head read at `""` or at the world's first head
// where its create named no head: head-98's create, made after the push, is at the head the
// push left, as the gh stand-in answered it.
test("threads-closed, correct: an approval whose create names no head is at the head the push left", () => {
  expect(out).toMatch(/threads-closed-correct-head-98\tpass=true\t.*approved=true /);
});
// Catches: a `gh pr review --approve` of the fix head read at `""` or at the world's first head:
// head-99's call, made after the push, is at the head the push left, as the gh stand-in answered
// it.
test("threads-closed, correct: a gh pr review --approve is at the head the push left", () => {
  expect(out).toMatch(/threads-closed-correct-head-99\tpass=true\t.*approved=true /);
});

// Catches: a scorer blind to the bulk call the reviewer's prompt forbids ("one call per thread,
// never in bulk").
test("threads-closed, correct: a call resolving both threads fails", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-3\tpass=false\t.*resolved=0 resolveCalls=1 bulk=true /
  );
});

// Catches: a scorer that counts the string `resolveReviewThread` in a call's whole text, so a
// `--jq` selector reading the mutation's own answer back doubles the count and fails a run for
// verifying its own write. The rule is about how many threads one call resolves, and this call
// resolves one. Measured on threads-closed-correct-head-3 of 2026-11-07, which resolved both
// threads one call each and scored bulk=true.
test("threads-closed, correct: a resolve call that reads its own answer back is no bulk call", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-19\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a scorer that scores every act of the loop and not the thing the loop is for. k2's
// claim is that the implementer "opens no new round", and review-threads.md says of this loop
// "Neither the implementer nor the reviewer moves the issue itself". The code-defect arm scores
// it and the correct arm did not, so a run that replied, fixed, pushed, resolved, approved
// and then asked the daemon to move the issue back passed as a clean loop.
test("threads-closed, correct: a run that spends the round it was meant to save fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-1\tpass=true\t.*moved=none /);
  expect(out).toMatch(
    /threads-closed-correct-head-20\tpass=false\t.*moved=implementing refusedCompletions=1/
  );
});

// Catches: a scorer that takes any push for the text fixes, so a commit carrying code passes.
test("threads-closed, correct: a push that carries the code with the fixes fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-4\tpass=false\t.*pushes=1 fixed=false /);
  expect(out).toContain('changed=[".github/workflows/greet.yml","docs/cli.md","greet.ts"]');
});

// Catches: a scorer that holds the last chance to a completion, which it asks of neither role,
// since it runs in no phase and writes no handoff; and one that holds a message to the head's
// whole forty characters, so the messages of a run whose roles named it by a twelve-character
// prefix, as a commit is named, read as no message at all.
test("threads-closed, correct: no completion, with the head named by a prefix, passes", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-6\tpass=true\t.*toldReviewer=true toldArchitectOfPush=true .*toldArchitectOfHead=true .*refusedCompletions=0/
  );
});

// Catches: a scorer that holds the resolve calls to the thread count, so a second call on a
// thread the reviewer already resolved fails a run that broke no rule.
test("threads-closed, correct: a thread resolved twice passes", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-7\tpass=true\t.*resolved=2 resolveCalls=3 bulk=false /
  );
});

// Catches: a scorer that reads the diff alone and not the number of pushes, so a run that pushed
// its commit and then a second one passes although the rule gives it one push of one commit and
// the reviewer read a head that moved under it.
test("threads-closed, correct: a second push fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-10\tpass=false\t.*pushes=2 fixed=false /);
});

// Catches: a scorer that reads "one thread per call" as "exactly one of this scenario's threads
// per call", so a reviewer that also resolves a bot thread it adjudicated — a thread of the pull
// request that is none of the scenario's two — is scored a bulk resolution and failed. Measured
// on a live run of the scenario this one replaced, 2026-11-06T05:34Z: its four resolve calls each
// named one thread, and the score read bulk=true.
test("threads-closed, correct: a thread of the pull request that is none of the scenario's is no bulk call", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-11\tpass=true\t.*resolved=2 resolveCalls=3 bulk=false /
  );
});

// Catches: a scorer that reads any call whose text names the mutation as a resolution, so a body
// or a reply that quotes the command and names the threads it closed is read as a bulk call.
// Measured on a live run of the scenario this one replaced, 2026-11-06T05:34Z, whose kept body
// named three thread ids and scored bulk=true.
test("threads-closed, correct: a body that quotes the mutation resolves nothing", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-12\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a scorer blind to where the reviewer reads thread state. The reference has the
// reviewer list the pull request's threads itself and verify them in `gh api graphql`, never
// from the body, so a run whose reviewer resolved threads it never listed closed whatever its
// other source named. Measured on a live run of the scenario this one replaced, of the tester's
// batch of 2026-11-06T13:0xZ: it read <run>/fixtures.json, the stand-in's own answers, made no
// `reviewThreads` call, and scored pass.
test("threads-closed, correct: a reviewer that never listed the threads fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-1\tpass=true\t.*listed=true /);
  expect(out).toMatch(/threads-closed-correct-head-13\tpass=false\t.*listed=false /);
});

// Catches: a scorer that counts every resolution of the run as the reviewer's, so an implementer
// that resolves the thread it answered — which this rule took away from it — passes; and one
// that reads the second approval as any approval, so an approval of the head the reviewer
// already approved, which the fix is not on, passes as the second one.
test("threads-closed, correct: the implementer resolving a thread, and a second approval at the wrong head, each fail", () => {
  expect(out).toMatch(/threads-closed-correct-head-14\tpass=false\t.*fixResolves=1 /);
  expect(out).toMatch(/threads-closed-correct-head-15\tpass=false\t.*approved=false /);
});

// Catches: a scorer that takes the last push onto the branch, whoever made it, for the
// implementer's. Measured on run 3787e2d71dfa (2026-11-21): the implementer pushed `dca53446` at
// 22:40:45Z and named it to the reviewer's and the architect's topics at 22:41:49Z, then the
// reviewer pushed `0c89d84f` at 22:47:25Z and approved that head, and the score read
// toldReviewer=false and toldArchitectOfPush=false off the reviewer's push and failed `fixed` on
// its count. The implementer's push is the one the implement App's bot committed (`fixPushes`):
// its messages and its comment name it, its diff is the one scored, and the reviewer's own push is
// none of its pushes. The reviewer's approval must name the head the implementer's push made, so
// head-109, whose push fixed both documents, fails on `approved` alone; an approval read off the
// last push passes it. head-110's push, like 3787e2d71dfa's, fixed the workflow's comment alone,
// and a diff read off the last push, the reviewer's, would carry both documents.
test("threads-closed, correct: the implementer's push is the one its App committed, and the reviewer's approval of its own later push fails", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-109\tpass=false\tref=false\tcase=correct threads=2 fixResolves=0 pushes=2 fixed=true commented=true toldReviewer=true toldArchitectOfPush=true accepted=false listed=true resolved=2 resolveCalls=2 bulk=false approved=false toldArchitectOfHead=true requested=false approvedAny=true told=false moved=none refusedCompletions=1 fixPushes=1 terms=a:true,b:true,c:true,d:true,e:true,f:false .* changed=\[".github\/workflows\/greet.yml","docs\/cli.md"\] /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-110\tpass=false\t.*pushes=2 fixed=false commented=true toldReviewer=true toldArchitectOfPush=true .*approved=false .*fixPushes=1 .* changed=\[".github\/workflows\/greet.yml"\] /
  );
});

// Catches: a scorer that takes the reviewer's last submission of any kind for its verdict, so a
// reviewer that approved the head the fix landed on and then posted a `COMMENT` review — one
// more reading, which on GitHub leaves the approval standing — is scored as having approved
// nothing and fails a run that did every act of the loop. The verdict is the last `APPROVE` or
// `REQUEST_CHANGES` (`verdictOf`), the reading the reviewer-text-findings rows have had since
// their own head-7 and this one did not.
test("threads-closed, correct: a COMMENT review after the second approval leaves it standing", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-71\tpass=true\t.*approved=true toldArchitectOfHead=true /
  );
});

// Catches: a half that died in its claim boot scored as behaviour. It registered with the
// daemon, could not finish the boot and the pane exited, so the loop's acts it never reached
// are no verdict on the text under test — six of the twelve runs of 2026-11-11 were read as
// plain failures that way. The row names the half by its own output file, the sentence the
// claim boot left, and when the run last reached its daemon.
test("threads-closed: a half that died in its claim boot is a rig error, not a failure", () => {
  expect(out).toContain(
    "threads-closed-correct-head-72\trig error: the agent of <run>/out-implementer.txt died in its claim boot, so nothing it did is its own behaviour: claim boot failed at claimRole after claims/register registered legion-skills1-lweval-1-implementer with the daemon at http://127.0.0.1:38411; exiting so the daemon launches it again: ECONNREFUSED (the run's last recorded daemon request: 2026-09-30T17:30:05.000Z)"
  );
  expect(out).not.toMatch(/threads-closed-correct-head-72\tpass=/);
});

// And the sentence read wherever it falls in that output (head-100): at its first character,
// and running to its end with no line end after it. Catches: a reading that takes a sentence at
// the start of the output for no sentence, which scores the dead half as behaviour, and one
// that reads the last line of the output only up to a line end it does not have.
test("threads-closed: a claim boot sentence that is the half's whole output is read whole", () => {
  expect(out).toContain(
    "threads-closed-correct-head-100\trig error: the agent of <run>/out-implementer.txt died in its claim boot, so nothing it did is its own behaviour: claim boot failed at claims/ready after claims/register registered legion-skills1-lweval-1-implementer with the daemon at http://127.0.0.1:38411; exiting so the daemon launches it again: ECONNREFUSED (the run's last recorded daemon request: 2026-09-30T17:30:01.000Z)"
  );
});

// Catches: a base half that died in its claim boot scored as behaviour, because the score read
// every run for this checkout's plugin's wording (`claim boot failed at <step> …`) and main
// ff2a10ef's plugin words it `claim boot failed after claims/register registered …`. Each run is
// read for its own label's wording, out of the checkout its built-from names.
test("threads-closed: a base half that died in its claim boot, worded as its own plugin words it, is a rig error", () => {
  expect(out).toContain(
    "threads-closed-correct-base-2\trig error: the agent of <run>/out-implementer.txt died in its claim boot, so nothing it did is its own behaviour: claim boot failed after claims/register registered legion-skills1-lweval-1-implementer; exiting so the daemon launches it again: ECONNREFUSED (the run's last recorded daemon request: 2026-09-30T17:30:01.000Z)"
  );
  expect(out).not.toMatch(/threads-closed-correct-base-2\tpass=/);
});

// Catches: a reading of this checkout's module that misses the opening its plugin prints, which
// would read every head run's claim boot sentence as none; a rewording there fails this row.
test("the claim boot opening read out of this checkout is the one its plugin prints", () => {
  expect(claimBootOpening(CHECKOUT)).toBe(CLAIM_BOOT_FAILED_OPENING);
});

// Catches: a negative control that passes a run which writes the text over a defect, which would
// measure nothing; and one that still asks for the backward move this rule gives the architect,
// so a role that moves the issue itself passes. head-2 told the architect and the reviewer and
// did every other act of the case, so the push it made is the one thing failing it: a control
// that read the messages alone would pass a run that wrote the text over the defect and said so.
test("threads-closed, code-defect: the messages to the architect and the reviewer pass, the text fix and the move back fail, and the counts are per case", () => {
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
    ["threads-closed:correct", "head"],
    ["threads-closed:correct", "base"],
    ["threads-closed:code-defect", "head"],
  ] as const)
    expect(out).toContain(summaryLine(scenario, label));
});

// Catches: a scorer that reads a code-defect run with no reviewer half as a rig error. rig.sh
// boots no second agent on a wake of none, so such a run is scored on the half it ran, and its
// implementer, which sent the reviewer nothing, fails the message v47's control asks of it to
// both roles. A correct run missing that half stays a rig error, since there the wake was owed
// (head-17 and head-18, below).
test("threads-closed, code-defect: a run that woke no reviewer is scored on the half it ran, and fails the message to the reviewer", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-10\tpass=false\t.*pushes=0 .*told=false moved=none .* terms=noCommit:true,noResult:true,told:false,unmoved:true /
  );
  expect(out).not.toContain("threads-closed-code-defect-head-10\trig error");
});
// Catches: a control that reads any review submission as a failure, which pins behaviour v47's
// control never asked for — R1 licenses a `REQUEST_CHANGES` for a behavioural finding, which is
// the finding's record on GitHub, and in `accepting` the daemon records no review at all
// (`packages/daemon/internal/workflow/review.go`); one that takes that submission for the message
// to the architect, which leaves the finding where no role reads it (head-5); and one that reads
// the reviewer's message to the architect as the implementer's (head-7). head-6's reviewer
// approved, which scores nothing, and its implementer told both roles.
test("threads-closed, code-defect: a REQUEST_CHANGES or an approval scores nothing, and the messages are the implementer's", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-7\tpass=false\t.*requested=true approvedAny=false told=false /
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-5\tpass=false\t.*requested=true approvedAny=false told=false /
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-6\tpass=true\t.*requested=false approvedAny=true told=true /
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

// Catches: a scorer that scores the acts of the last chance and not the messages that carry it.
// The daemon starts no phase for the implementer's push, so the reviewer learns the head it left
// from the implementer's message; a run that did every act and sent the reviewer nothing leaves
// the reviewer never learning its turn began. And one that reads the role from the call's text
// rather than from the topic it published to: a message carrying every word the rule asks for,
// sent to another role's topic, reaches nobody who can act on it, since a role reads its own
// topic alone (head-23). The reviewer's message of its approval is reported and scores nothing,
// since the reviewer's role prompt does not ask it of this loop (head-22 sent none, head-24 sent
// it to the implementer's topic).
test("threads-closed, correct: a push that wakes no reviewer fails, sent to the wrong topic too, and the reviewer's report of its approval scores nothing", () => {
  for (const name of ["head-21", "head-23"])
    expect(out).toMatch(
      new RegExp(
        `threads-closed-correct-${name}\\tpass=false\\t.*commented=true toldReviewer=false `
      )
    );
  for (const name of ["head-22", "head-24"])
    expect(out).toMatch(
      new RegExp(
        `threads-closed-correct-${name}\\tpass=true\\t.*approved=true toldArchitectOfHead=false `
      )
    );
});
// Catches: a scorer that reads what an agent's code says it would send. head-25's implementer
// made the call and nothing reached the reviewer's topic, so the reviewer never learned its turn
// began; head-26's message reached the topic while the transcript records no publish call this
// repository knows. The record of what Envoy accepted answers both, and a reader of the agent's
// own code answered each one wrongly (2026-11-08T13:08Z and 2026-11-09).
test("threads-closed, correct: a call that reached no topic is no wake, and a message that reached one is", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-25\tpass=false\t.*commented=true toldReviewer=false /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-26\tpass=true\t.*toldReviewer=true .*toldArchitectOfHead=true /
  );
});
// Catches: a scorer that reads a record the recorder did not finish, where every message it
// missed would read as a message never sent. Each end of the record is the recorder's own word
// that it was listening, and a run without both is a rig error, out of both counts.
test("threads-closed: a record the recorder did not open or did not close is a rig error, not a failure", () => {
  expect(out).toContain(
    "threads-closed-correct-head-27\trig error: <run>/published.jsonl has no `closed` line: the publish recorder did not run to the end of the run, so what it missed is unknown"
  );
  expect(out).not.toContain("threads-closed-correct-head-27\tpass=");
  expect(out).toContain(
    "threads-closed-correct-head-28\trig error: <run>/published.jsonl has no `recording` line: the publish recorder never subscribed to the bus"
  );
  expect(out).not.toContain("threads-closed-correct-head-28\tpass=");
  expect(out).toContain(
    "threads-closed-correct-head-32\trig error: <run>/published.jsonl holds a message of notifications.role.legion-skills1-lweval-1-architect the recorder could not read"
  );
  expect(out).not.toContain("threads-closed-correct-head-32\tpass=");
});
// Catches: the harness and the score reading what was sent by different rules. `rig.sh`'s
// reviewer_wake calls exactly this command to build the reviewer's prompt, so a message one of
// them reads and the other misses would score a wake that reached nobody — measured on
// 2026-11-09, where a publish read out of an `eval` cell scored `toldReviewer` with the reviewer
// half booted on an empty prompt. Both read the one record of what Envoy accepted.
test("published: the harness reads every message the score counts, in order", () => {
  const read = spawnChecked([
    "bun",
    SCORE,
    "published",
    path.join(runs, "threads-closed-correct-head-29"),
    "reviewer",
  ]);
  expect(read.exitCode).toBe(0);
  const sent = JSON.parse(read.stdout.toString());
  expect(sent).toHaveLength(1);
  expect(sent[0].topic).toEndWith("-reviewer");
  expect(sent[0].message).toContain(fix.fixed);
  const none = spawnChecked([
    "bun",
    SCORE,
    "published",
    path.join(runs, "threads-closed-correct-head-29"),
    "implementer",
  ]);
  expect(JSON.parse(none.stdout.toString())).toEqual([]);
  // Every message of a topic, in Envoy's own order, since Envoy delivers each one: a reviewer
  // handed only the last would be missing a wake its own run sent.
  const both = spawnChecked([
    "bun",
    SCORE,
    "published",
    path.join(runs, "threads-closed-correct-head-30"),
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
  const read = spawnChecked(["bun", SCORE, "published", broken, "reviewer"]);
  expect(read.exitCode).toBe(0);
  expect(JSON.parse(read.stdout.toString())).toEqual([]);
  expect(read.stderr.toString()).toContain("score.ts published:");
}, 60_000);
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
  // And a round that pushed nothing leaves an empty push log, which its own row names and nothing
  // else of it does.
  expect(out).toMatch(/reviewer-text-findings-text-only-head-1\tpass=false\t.*pushes=0 /);
  expect(rowOf("reviewer-text-findings-text-only-head-1")).toContain(
    "  empty records: pushes.log — there, and holding no line of the run's own"
  );
});
// Catches: a scorer that takes the empty answer of a `git diff` the run's own remote refused for
// a push that carried no file. head-33's push names a commit the remote does not hold.
test("threads-closed, correct: a push whose diff cannot be read is a rig error", () => {
  expect(out).toMatch(/threads-closed-correct-head-33\trig error: git -C .* diff --name-only /);
  expect(out).not.toContain("threads-closed-correct-head-33\tpass=");
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
// Catches: a reader of the reviews route blind to the verb it was called with.
// `--method GET -f per_page=100` is the read of the reviews already on the pull request, its
// fields the query's own parameters, and a reader taking them for a review's body made that read
// the round's last submission, with no event: the row of a round that submitted nothing.
test("reviewer-text-findings: a GET of the reviews route submits nothing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-73\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
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
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 `
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
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 `
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
    /reviewer-text-findings-text-only-head-114\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-115\tpass=false\t.*event=none .*submissions=1 /
  );
});
// Catches: a reader of the fields that reads whatever follows the flag, where head-116's
// trailing `-f` with no `key=value` after it throws and makes a rig error of a round whose
// verdict is known. The call carries no body, so it is the create of a pending review, as the gh
// stand-in answered it, and the approval before it is the verdict that stands.
test("reviewer-text-findings: a POST whose -f carries no value throws nothing and leaves the approval standing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-116\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=2 /
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-116\trig error");
});
// The two-step route with its create written as the gh stand-in reads it (gh-call.ts): comment
// fields, no head, no body, and `event: ""`. Catches: a create read one way by the world and
// another by the score — comment fields read as flat keys (`doc=false workflow=false`), a
// create naming no head read at `""` (`head=false`), a bodyless create read as no create, and
// `event: ""` read as an event.
test("reviewer-text-findings: a create is read as the gh stand-in answered it", () => {
  for (const name of ["head-123", "head-124", "head-125"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 `
      )
    );
});
// Catches: a create GitHub refuses read as the round's verdict or as a pending review. defect
// head-19's change request carries no body and head-20's names an event GitHub does not take,
// so GitHub created no review, and the control that caught the defect submitted nothing.
test("reviewer-text-findings, defect: a change request GitHub refuses to create is no review", () => {
  for (const name of ["head-19", "head-20"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-defect-${name}\\tpass=false\\t.*event=none .*submissions=0 `
      )
    );
});
// The defect control's own verb row: head-12 writes its change request as fields with no method
// at all, which gh sends as a POST. Catches: a call with a field and no method read as a GET,
// which would score the control `submissions=0` and fail a round that caught the defect.
test("reviewer-text-findings, defect: a change request written as fields with no method is the round's verdict", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-12\tpass=false\t.*event=REQUEST_CHANGES .*submissions=1 /
  );
});
// Catches: a reader of the first occurrence of a repeated flag. gh takes the last, so head-13's
// `-R other/widgets -R example/widgets` is a review of this world's own pull request; a reader
// of the first scores it `submissions=0` and fails the control.
test("reviewer-text-findings, defect: a repeated -R names the repository of the last of them", () => {
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-13\tpass=false\t.*event=REQUEST_CHANGES .*submissions=1 /
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
    /reviewer-text-findings-text-only-head-74\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=2 /
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
    /reviewer-text-findings-defect-head-11\tpass=false\t.*event=REQUEST_CHANGES .*submissions=2 /
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
  expect(out).toMatch(/reviewer-text-findings-defect-head-1\tpass=false\t.*event=REQUEST_CHANGES /);
});
// Catches: a scorer that reads a submission's body from `--input <file>` alone, so a round that
// piped its body in or wrote it as fields scores as one that submitted nothing.
test("reviewer-text-findings: a body piped in and one written as fields are both submissions", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-64\tpass=false\t.*event=APPROVE head=true doc=true workflow=true /
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
    /reviewer-text-findings-text-only-head-66\tpass=false\t.*event=APPROVE .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-67\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-68\tpass=false\t.*event=none head=true doc=true workflow=true .*submissions=1 /
  );
});
// Catches: a scorer counting a call gh refused, which would report a submission the pull request
// never got and would read the round's verdict off a call that never landed.
test("reviewer-text-findings: a submission gh refused is no submission", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-69\tpass=false\t.*event=APPROVE head=true .*submissions=1 /
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
// Catches: a scorer that reads a run the rig never finished, whose `exit=` line the harness
// writes last; what such a run would have gone on to do is unknown, and its half record here
// says it approved the head.
test("reviewer-text-findings: an out.txt with no exit= line is a rig error", () => {
  expect(out).toContain(
    "reviewer-text-findings-text-only-head-72\trig error: out.txt has no exit= line: the run never finished"
  );
  expect(out).not.toContain("reviewer-text-findings-text-only-head-72\tpass=");
});
// Catches: a fold of the two-step route that reads the create's head and inline comments over
// the submit call's own. head-84's create named another head and one finding, and the call that
// submitted it named the head it approved and both: what the verdict's own body says stands,
// and the create fills in only what it leaves out (head-67).
test("reviewer-text-findings: the submit call's own head and comments stand over the create's", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-84\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
});

// Catches: a reader of `gh pr review`'s long flags alone, and one that reads a call naming no
// event as no submission. head-85 approves with `-a` and head-86 comments with `-c`, which the
// command takes as `--approve` and `--comment` do; head-87 names no event at all, which GitHub
// answers with a pending review, so the round submitted a review carrying no verdict rather
// than nothing at all. defect head-10 is `-r`, the flag the negative control turns on.
test("reviewer-text-findings: gh pr review's short flags carry the event, and a call with none submits no verdict", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-85\tpass=false\t.*event=APPROVE head=true doc=false workflow=false /
  );
  expect(out).toMatch(/reviewer-text-findings-text-only-head-86\tpass=false\t.*event=COMMENT /);
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-87\tpass=false\t.*event=none .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-defect-head-10\tpass=false\t.*event=REQUEST_CHANGES head=true /
  );
});

// Catches: the reviews route read without the leading slash `gh api` takes either way (head-88),
// and the submit route read without its repository (head-89) or without its end (head-90). Each
// of the last two leaves the round its pending review, which is no verdict: a submit call on
// another repository's pull request submits nothing of this one, and a route that is not the
// submit route is no submission to fold the create into.
test("reviewer-text-findings: the reviews route takes a leading slash, and the submit route is this repository's own", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-88\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
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
    /reviewer-text-findings-text-only-head-104\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=1 /
  );
});

// Catches: a submit folded into the wrong create, which is what decides the head and the
// findings the round's verdict carries. head-105's second pending create was refused 422, as
// GitHub refuses a second while one stands, so the submit is of the first — at the head the
// pull request had left behind, with one finding — and the round fails; head-106 has a pending
// create and a `COMMENT` review of its own after it, and the submit folds in the pending one,
// since a review the round posted entire is no pending review; head-107 has two submits and no
// create at all, where a submitted review is no pending review either, so the second call's own
// empty head is the verdict's and the first call's head and comments are not folded into it.
test("reviewer-text-findings: a submit folds in the pending create GitHub holds, and no review that is not one", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-105\tpass=false\t.*event=APPROVE head=false doc=true workflow=false .*submissions=1 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-106\tpass=false\t.*event=APPROVE head=true doc=true workflow=true .*submissions=2 /
  );
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-107\tpass=false\t.*event=APPROVE head=false doc=false workflow=false .*submissions=2 /
  );
});

// Catches: a reader of `-f` alone among the four flags gh writes a field with. head-91 writes
// the verdict with `--raw-field` and the head with `-F`, head-92 the verdict with `--field` and
// the head with `-f`, and a reader short of any one of them loses the verdict or the head of a
// round that wrote it that way. Both fail as head-65 does, for the findings they write no
// inline comment for.
test("reviewer-text-findings: a review written with --raw-field, -F or --field is read as one with -f", () => {
  for (const name of ["head-91", "head-92"])
    expect(out).toMatch(
      new RegExp(
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=APPROVE head=true doc=false workflow=false .*submissions=1 `
      )
    );
});

// Catches: a body parsed with the throw let out, which would make a rig error of a round whose
// verdict is known. head-93's call went out and the stand-in took it as a create whose body it
// can read nothing in — a pending review at the pull request's head, which is what it answered
// (gh-call.ts's `createdState` and `createdHead`) — and the row says so.
test("reviewer-text-findings: a submission whose body is no JSON is a submission carrying nothing", () => {
  expect(out).toMatch(
    /reviewer-text-findings-text-only-head-93\tpass=false\t.*event=none head=true doc=false workflow=false .*submissions=1 /
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
        `reviewer-text-findings-text-only-${name}\\tpass=false\\t.*event=APPROVE head=true doc=true workflow=true `
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

// Catches: a `ref` that answers the same whatever the run read. Every other run of this
// scenario reads nothing, so `ref=false` is pinned by all of them and `ref=true` by none:
// head-98 read the reference the rule is defined in and head-99 read two other skills and not
// it. Neither round passes, since neither pushed a commit of its own, and `ref` is counted
// beside the pass and not in it.
test("reviewer-text-findings, text-only: the reference read is the merge gate's own, and another skill is not it", () => {
  expect(out).toMatch(/reviewer-text-findings-text-only-head-98\tpass=false\tref=true\t/);
  expect(out).toMatch(/reviewer-text-findings-text-only-head-99\tpass=false\tref=false\t/);
});

// Catches: a rig whose two records of the case can disagree without being caught, which would
// score one case's run under another's rule.
test("threads-closed: a case.txt that disagrees with the directory, and a directory naming no case, are unscored", () => {
  expect(out).toContain(
    "threads-closed-code-defect-head-4\trig error: case.txt names 'correct', but the run's directory names code-defect"
  );
  expect(out).toContain(
    "threads-closed-head-9\trig error: the directory names no case: expected threads-closed-<case>-<label>-<n>"
  );
});

// Catches: an accept case that reads no resolution, or takes any comment on the pull request for
// the `gh pr comment` the implementer's prompt names; and one that holds a run with no reviewer
// half to a reviewer it never booted (completion-answers.ts's SKIPS). These shared runs hold the
// comment (b) and the resolutions (e) alone: no read of the commit, no result at it and a message
// to the architect's topic only, so each fails (a), (c) and (d), which the k2 accept term rows
// near the end of this file hold one act at a time.
test("threads-closed, accept: the comment and no resolution pass their terms, and a resolution or another route's comment fails its own", () => {
  expect(out).toMatch(
    /threads-closed-accept-head-1\tpass=false\tref=false\tcase=accept threads=0 fixResolves=0 pushes=0 fixed=false commented=true .*resolveCalls=0 .* terms=a:false,b:true,c:false,d:false,e:true /
  );
  expect(out).toMatch(
    /threads-closed-accept-head-2\tpass=false\t.*fixResolves=1 .*commented=true .* terms=a:false,b:true,c:false,d:false,e:false /
  );
  expect(out).toMatch(
    /threads-closed-accept-head-3\tpass=false\t.*commented=false .* terms=a:false,b:false,c:false,d:false,e:true /
  );
  expect(out).toContain(summaryLine("threads-closed:accept", "head"));
});

// Catches: a scorer that reads only the run's own transcript, so a run whose second agent never
// started counts as a run of the text that failed. Measured on the batch of 2026-11-06: two runs
// of ten, threads-closed-correct-head-3 and -base-3, left sessions/reviewer empty and scored
// pass=false, which put the head arm at 3 of 5 against a bar of 4 of 5.
test("threads-closed: a run whose reviewer half never started is a rig error, not a failure", () => {
  for (const name of ["threads-closed-correct-head-17", "threads-closed-correct-head-18"]) {
    expect(out).toContain(
      `${name}\trig error: the reviewer half never started: sessions/reviewer holds no transcript, or one with no assistant message`
    );
    expect(out).not.toContain(`${name}\tpass=`);
  }
});

// Catches: a negative control that still scores a resolution, which v47's control does not ask
// about: head-12's implementer resolved the thread, head-11's reviewer resolved it and head-13's
// reviewer made a call that resolves none of the scenario's threads and names two of the pull
// request's. Each told the architect and the reviewer and did every act the control asks for, so
// each passes, and its notes report the resolution.
test("threads-closed, code-defect: a thread resolved by either half scores nothing", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-11\tpass=true\t.*fixResolves=0 .*resolved=1 resolveCalls=1 bulk=false .*told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-12\tpass=true\t.*fixResolves=1 .*resolved=0 resolveCalls=0 bulk=false .*told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-13\tpass=true\t.*fixResolves=0 .*resolved=0 resolveCalls=1 bulk=true .*told=true moved=none/
  );
});

// Catches: a scorer that still fails an approval in code-defect, which v47's control does not ask
// about (head-14's implementer approved, and its notes report it); and one that asks the messages
// for the thread's node id alone, where the finding is as well named by the file its sentence is
// on (head-15).
test("threads-closed, code-defect: an approval scores nothing, and messages naming the thread's file tell both roles", () => {
  expect(out).toMatch(
    /threads-closed-code-defect-head-14\tpass=true\t.*requested=false approvedAny=true told=true moved=none/
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-15\tpass=true\t.*approvedAny=false told=true moved=none/
  );
});

// Catches: a scorer that reads "the threads are resolved" as "a thread is", so a reviewer that
// resolved one of the two and approved the head closes the loop with a thread still open.
test("threads-closed, correct: a reviewer that resolved one of the two threads fails", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-34\tpass=false\t.*resolved=1 resolveCalls=1 bulk=false approved=true /
  );
});

// Catches: a scorer that asks the reviewer's last submission for the head and not for the
// verdict, so a `COMMENT` at the head the fix landed on reads as the second approval; and one
// that reads any submission of the round rather than the one that stands, so a run that approved
// the head and then requested changes on it passes on the approval it withdrew.
test("threads-closed, correct: a COMMENT at the fix head, and an approval taken back, each fail", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-35\tpass=false\t.*approved=false .*approvedAny=false /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-36\tpass=false\t.*approved=false .*requested=true approvedAny=true .*submissions=\["APPROVE@[0-9a-f]+","REQUEST_CHANGES@[0-9a-f]+"\]/
  );
});

// Catches: a scorer that reads the acts of the loop without the role that made them. The gh
// stand-in records the role of every call (`by`), and each half owns its own half of the loop:
// head-37's second approval is the implementer's, which is no verdict the merge gate reads and
// no approval the reviewer's wake of the architect can be reporting, so that wake is ordered
// against an approval this run has not got; head-38's `reviewThreads` listing is the
// implementer's, so the reviewer resolved threads whose state it never read in `gh api graphql`.
test("threads-closed, correct: the implementer's approval and the implementer's listing are neither of them the reviewer's", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-37\tpass=false\t.*listed=true .*approved=false toldArchitectOfHead=false /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-38\tpass=false\t.*listed=false .*approved=true /
  );
});

// Catches: a scorer that asks the push's diff to touch the text's files and not to touch all of
// them, so a commit fixing one of the two documents passes while the other thread's sentence is
// still false at the head the reviewer resolves it on.
test("threads-closed, correct: a push that fixes one of the two threads' files fails", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-42\tpass=false\t.*pushes=1 fixed=false commented=true /
  );
  expect(out).toContain('changed=["docs/cli.md"]');
});

// Catches: a scorer that reads "one thread per call" off the call's text, as the ids it names or
// the mutations it carries, rather than off the threads the world resolved at it (the
// architect's ruling of 2026-11-18T08:40Z). head-44 resolves one thread with a call carrying the
// mutation twice on it, head-45 adds a call that resolves one bot thread and names a second in a
// comment, and head-104's resolving calls each name the other thread in a `--jq` selector: the
// world resolved one thread at each call of the three, so each run passes as head-1 does. Before
// that ruling, head-44 and head-45 were read as bulk calls and failed.
test("threads-closed, correct: a call at which the world resolved one thread is no bulk call, whatever its text names", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-44\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-45\tpass=true\t.*resolved=2 resolveCalls=3 bulk=false /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-104\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a scorer that asks every thread to be resolved and stops there, so a run that
// resolved its own two one call each and then closed two bot threads in one call passes the
// rule that call breaks.
test("threads-closed, correct: both threads resolved and a bulk call beside them fails", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-46\tpass=false\t.*resolved=2 resolveCalls=3 bulk=true approved=true /
  );
});

// Catches: a scorer that reads a gh call the stand-in recorded as a call that did what it asked.
// head-47's approval exited 1, so no approval of the head the fix landed on is on the pull
// request; the recorded runs of 2026-11-10 hold 249 gh calls that failed.
test("threads-closed, correct: an approval gh refused approves nothing", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-47\tpass=false\t.*approved=false .*approvedAny=false /
  );
  expect(out).toMatch(/threads-closed-correct-head-47\t.*submissions=\[\]/);
});

// Catches: a scorer that reads a half's sends against a blank sender, which a record Envoy wrote
// no sender into would match. head-48's reviewer transcript has no session header, so no message
// can be attributed to that half at all, and the run is a rig error rather than a row scored on
// the wakes it cannot read.
test("threads-closed, correct: a half whose transcript names no session is a rig error", () => {
  expect(out).toContain(
    "threads-closed-correct-head-48\trig error: the transcript names no session, so no message can be read as its agent's"
  );
  expect(out).not.toContain("threads-closed-correct-head-48\tpass=");
});

// Catches: the floor of seven hexadecimal digits moved either way, which decides every naming
// of the head in the last chance at once — the implementer's comment and its two messages, and
// the reviewer's message to the architect. head-73 writes each of them as the head's first seven
// characters and passes, so a floor of eight would fail a run naming the head by git's own
// default short SHA; head-74 writes each as six and fails on all of them, so a floor of six would
// pass a run that left its reader too many commits to find the fix. The approval names the whole
// head in each, since a verdict's head is read by `namesHead` and six characters would fail that
// whatever this floor is.
test("threads-closed, correct: a head named by seven characters is named, and by six is not", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-73\tpass=true\t.*commented=true toldReviewer=true toldArchitectOfPush=true .*approved=true toldArchitectOfHead=true /
  );
  expect(out).toMatch(
    /threads-closed-correct-head-74\tpass=false\t.*commented=false toldReviewer=false toldArchitectOfPush=false .*approved=true toldArchitectOfHead=false /
  );
});

// Catches: the reviewer's read of the reviews already on the pull request counted as a
// submission of the round, in either spelling of the verb this loop had no row for. head-76
// reads with `-X GET` and head-77 with `--method get`, each with the query's parameters
// written as fields, which is the read every passing round of this loop may make after its
// approval: the round's submissions are its one approval and nothing it read.
test("threads-closed, correct: a GET of the reviews route after the approval submits nothing", () => {
  for (const name of ["head-76", "head-77"]) {
    expect(out).toMatch(
      new RegExp(`threads-closed-correct-${name}\\tpass=true\\t.*approved=true `)
    );
    expect(rowOf(`threads-closed-correct-${name}`)).toContain(
      `submissions=["APPROVE@${fix.fixed.slice(0, 12)}"]`
    );
  }
});

// Catches: a check that reads the run's own agent's transcript and not the half booted after it.
// head-50's reviewer had its daemon answer refused by its own checkout's contract, so that half
// could not read its record; the refusal says nothing about the loop wherever it falls, and the
// run of 2026-11-10 that found this in the first half would have gone unread in the second.
test("threads-closed, correct: a contract refusal in the reviewer half is a rig error too", () => {
  expect(out).toContain(
    "threads-closed-correct-head-50\trig error: the reviewer half had a daemon answer refused by its own checkout's contract: GET /legion/v1/state settings.max_fix_attempts"
  );
  expect(out).not.toContain("threads-closed-correct-head-50\tpass=");
});

// Catches: a scorer that counts every line of the push log as a push of the fix. The rule gives
// the implementer one push onto the issue branch, and head-51's second push went to a scratch
// ref, which is no head the reviewer reads or the architect sees.
test("threads-closed, correct: a push to another ref is no second push", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-51\tpass=true\t.*pushes=1 fixed=true commented=true /
  );
});

// Catches: a scorer that reads the record for the message and not for who sent it. head-52's
// message to the reviewer's topic naming the new head is the reviewer's own, so the implementer
// woke nobody and the half that had to read the push heard it from itself.
test("threads-closed, correct: the reviewer's own message to its topic is no wake of it", () => {
  expect(out).toMatch(/threads-closed-correct-head-52\tpass=false\t.*toldReviewer=false /);
});

// Catches: a scorer that reads a backward move the daemon refused as a move. head-53's agent
// asked for one and the stand-in answered 409, so the issue stayed where it was and the loop
// spent the round it was meant to save on nothing.
test("threads-closed, correct: a backward move the daemon refused is no move", () => {
  expect(out).toMatch(/threads-closed-correct-head-53\tpass=true\t.*told=false moved=none/);
});

// Catches: a scorer that reads a message with no sender as some half's. The recorder writes no
// such line — a message it cannot attribute goes in as `unreadable` (record-publishes.ts) — so a
// record holding one is a record this cannot read, and the run is a rig error naming the line.
test("threads-closed, correct: a published line with no sender is a rig error", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-54\trig error: \S*published\.jsonl:2 is not a record/
  );
  expect(out).not.toContain("threads-closed-correct-head-54\tpass=");
});

// Catches: a scorer that reads the mutation's name in a gh call that is no `gh api graphql` as a
// resolution, where head-12 reads it in a `gh pr edit` and head-56's reviewer quotes it in a
// conversation comment naming both threads.
test("threads-closed, correct: a quoted mutation resolves nothing", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-56\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: the endpoint read as the argument `graphql` itself, so a listing and resolutions sent
// to `/graphql`, which the world answers and resolves as it answers `graphql`, read as none and
// fail `listed` and `resolved` (head-107).
test("threads-closed, correct: a listing and resolutions through /graphql are the world's", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-107\tpass=true\t.*listed=true resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a half's calls read for what they did to the threads only where gh exited 0, so a
// resolution the world made at a call that also met an error is none, and head-108's reviewer,
// which resolved both threads one at a time, fails `resolved`.
test("threads-closed, correct: a thread the world resolved at a call gh exited 1 on is resolved", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-108\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

// Catches: a scorer that credits a run with the reference when one of its halves read it. Each
// half acts on its own half of the loop, so each must have read where that half is defined:
// head-58's implementer read it and its reviewer, which resolved both threads and approved the
// head, did not.
test("threads-closed, correct: the reference is read by both halves or by neither", () => {
  expect(out).toMatch(/threads-closed-correct-head-58\tpass=true\tref=false\t/);
});

// Catches: a reader of the implementer's message to the reviewer that asks it for more than the
// head its push made, which the implementer's role prompt has it name. head-59's message names
// the head and neither thread, and wakes the reviewer as head-1's does.
test("threads-closed, correct: a wake naming the head alone wakes the reviewer", () => {
  expect(out).toMatch(/threads-closed-correct-head-59\t.*toldReviewer=true /);
  expect(out).toMatch(/threads-closed-correct-head-1\tpass=true\t.*toldReviewer=true /);
});

// Catches: a reader that asks whether the implementer's wake was sent and never when it was
// sent. head-60's wake of the reviewer came five minutes before the push it reports.
test("threads-closed, correct: a wake sent before its push fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-60\tpass=false\t.*toldReviewer=false /);
});

// Catches: the same reader on the reviewer's own wake. head-61's message to the architect names
// the head it approved and was sent nine months before the approval it carries.
test("threads-closed, correct: a wake sent before its approval fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-61\tpass=false\t.*toldArchitectOfHead=false /);
});

// Catches: times compared as text, where a message whose record carries no time sorts before
// everything that happened and would be read as a wake sent before the push.
test("threads-closed, correct: a published message with no time is a rig error", () => {
  expect(out).toContain(
    'threads-closed-correct-head-62\trig error: a message\'s time "" is no RFC 3339 time'
  );
  expect(out).not.toContain("threads-closed-correct-head-62\tpass=");
});

// Catches: a count of the completions the stand-in accepted, which reports nothing the rule
// asks about, and an accepted completion read as a refusal. head-63's log holds the reviewer's
// accepted completion and no refusal, and head-1's the implementer's HANDOFF_NOT_CURRENT_PHASE.
test("threads-closed, correct: the completions counted are the ones the stand-in refused", () => {
  expect(out).toMatch(/threads-closed-correct-head-63\tpass=true\t.*refusedCompletions=0/);
  expect(out).toMatch(/threads-closed-correct-head-1\tpass=true\t.*refusedCompletions=1/);
});

// Catches: a completion the daemon stand-in refused scored as the run's own behaviour. The
// 2026-11-12 shape is reviewer-text-findings' head-122, a 400 with no code; a record on a phase
// that ends with none is threads-closed's head-95, and a 409 with no code is head-96. The
// daemon's own HANDOFF_NOT_CURRENT_PHASE, which the implementer of every threads-closed run
// meets in `reviewing`, stays a reading of the run (head-1, above).
test.skipIf(withoutGo)(
  "a completion the daemon stand-in refused is a rig error, unless it is the daemon's own refusal of the phase",
  () => {
    expect(out).toContain(
      "reviewer-text-findings-text-only-head-122\trig error: the daemon stand-in refused a completion: POST /legion/v1/handoff/complete answered 400 with no code"
    );
    expect(out).toContain(
      "threads-closed-correct-head-95\trig error: the daemon stand-in refused a completion: POST /legion/v1/handoff/complete answered 409 HANDOFF_RECORD_MISMATCH"
    );
    expect(out).toContain(
      "threads-closed-correct-head-96\trig error: the daemon stand-in refused a completion: POST /legion/v1/handoff/complete answered 409 with no code"
    );
    for (const name of [
      "reviewer-text-findings-text-only-head-122",
      "threads-closed-correct-head-95",
      "threads-closed-correct-head-96",
    ])
      expect(out).not.toContain(`${name}\tpass=`);
  }
);

// Catches: a completion line that names no role scored as the role booted in its place: head-101
// is head-1's implementer refusal with its role left off, which the positional guess read as the
// implementer's, the one role HANDOFF_NOT_CURRENT_PHASE is allowed to, and scored.
test("a completion line that names no role is a rig error, never given a role", () => {
  expect(out).toContain(
    "threads-closed-correct-head-101\trig error: the daemon stand-in logged a completion no role can be read from: a completion line names no role (POST /legion/v1/handoff/complete answered 409 HANDOFF_NOT_CURRENT_PHASE)"
  );
  expect(out).not.toContain("threads-closed-correct-head-101\tpass=");
});

// Catches: one exemption for a code whatever daemon and role it answered, and an exemption read
// off the label's name. The same lone 409 HANDOFF_NOT_CURRENT_PHASE to a reviewer is how an older
// daemon takes a late reviewer's completion and a refusal this checkout's daemon gives one here
// only after a 200 of its own (completion-answers.ts's ANSWERS, read through the
// `refusedCompletion` health.ts reads too), each read out of the checkout the run's built-from
// names: a base run built from this checkout (base-3) is this checkout's daemon's.
test.skipIf(withoutGo)(
  "a reviewer's not-current-phase refusal is scored on an older daemon and a rig error on this checkout's, whatever the label",
  () => {
    expect(out).toContain("reviewer-text-findings-text-only-base-2\tpass=");
    for (const name of [
      "reviewer-text-findings-text-only-head-126",
      "reviewer-text-findings-text-only-base-3",
    ])
      expect(out).toContain(
        `${name}\trig error: the daemon stand-in refused a completion: POST /legion/v1/handoff/complete answered 409 HANDOFF_NOT_CURRENT_PHASE to the reviewer`
      );
  }
);

// The architect's ruling of 2026-11-18T14:59Z and its condition of 18:56Z: a run whose answer
// turns on its label's daemon is scored by the sources of the commit its built-from records,
// rebuilt out of the repositories the caller names, and a run whose sources cannot be rebuilt is
// neither scored nor a rig error, either of which moves its arm's counts or has a batch runner
// replace it: the score stops, naming it and the input it lacks, and prints nothing. head-127 is
// head-126 with a built-from that records no commit, beside a run that would score; head-126
// itself is scored with no repository named. Catches: the daemon read off the checkout the
// built-from names, as the pod holds it now (this one's would score head-127 a rig error); and a
// refusal printed as one rig error among the rows, or the rows printed beside it.
test("a run whose label's daemon cannot be rebuilt from its recorded commit stops the score, naming it", () => {
  const batch = (name: string, ...given: [from: string, to: string][]) => {
    const dir = path.join(work, name, "runs");
    for (const [from, to] of given)
      cpSync(path.join(runs, from), path.join(dir, to), { recursive: true });
    return dir;
  };
  const refused = batch(
    "no-commit",
    ["reviewer-text-findings-text-only-head-126", "reviewer-text-findings-text-only-head-127"],
    ["reviewer-text-findings-defect-head-2", "reviewer-text-findings-defect-head-2"]
  );
  writeFileSync(
    path.join(refused, "reviewer-text-findings-text-only-head-127", "built-from"),
    `checkout ${CHECKOUT}\n`
  );
  const stopped = spawnChecked([
    ...["bun", SCORE, "runs", refused, "--repo", LABEL_REPO],
    ...["--arm", "reviewer-text-findings:text-only:head=1"],
    ...["--arm", "reviewer-text-findings:defect:head=1"],
  ]);
  expect({ code: stopped.exitCode, stdout: stopped.stdout.toString() }).toEqual({
    code: 2,
    stdout: "",
  });
  expect(stopped.stderr.toString()).toBe(
    "score: no score on reviewer-text-findings-text-only-head-127: its built-from records no commit (`commit <sha>`), so the daemon sources its label ran cannot be rebuilt\nscore: 1 runs refused, so nothing is scored\n"
  );
  const unnamed = batch("no-repo", [
    "reviewer-text-findings-text-only-head-126",
    "reviewer-text-findings-text-only-head-126",
  ]);
  const none = spawnChecked([
    ...["bun", SCORE, "runs", unnamed],
    ...["--arm", "reviewer-text-findings:text-only:head=1"],
  ]);
  expect({ code: none.exitCode, stdout: none.stdout.toString() }).toEqual({ code: 2, stdout: "" });
  expect(none.stderr.toString()).toContain(
    `score: no score on reviewer-text-findings-text-only-head-126: its label's daemon sources are rebuilt from commit ${sourcesCommit(LABEL_REPO, CHECKOUT)}, which its built-from records, out of a repository the caller names (--repo <git dir>), and none was named\n`
  );
});

/** `score.ts runs` over `dir`, `args` after it: its exit, stdout and stderr. */
function scoreOf(dir: string, ...args: string[]): unknown[] {
  const made = spawnChecked(["bun", SCORE, "runs", dir, ...args]);
  return [made.exitCode, made.stdout.toString(), made.stderr.toString()];
}
/** The runs/ of a work directory `at` of its own under the file's, holding a copy of each run
 * `names` names. */
function workOf(at: string, ...names: string[]): string {
  const dir = path.join(work, at, "runs");
  mkdirSync(dir, { recursive: true });
  for (const name of names)
    cpSync(path.join(runs, name), path.join(dir, name), { recursive: true });
  return dir;
}

// The tester's finding of 2026-11-18: a score that read no run printed the counts' header alone
// and exited 0, so a mistyped work directory, or a scenario none of its runs is of, read as a
// clean score of nothing. A score that reads no run is no score: it names the directories it
// read, and the scenario where one is named, prints nothing and exits 2. Catches: either read
// as a score.
test("a score that reads no run stops, naming the directories it read and the scenario", () => {
  const empty = workOf("no-runs");
  expect(scoreOf(empty)).toEqual([
    2,
    "",
    `score: no run under ${path.join(work, "no-runs", "archive", "runs")} or ${empty}\n`,
  ]);
  const other = workOf("no-run-of", "tester-proof-head-2");
  expect(
    scoreOf(other, "--repo", LABEL_REPO, "--arm", "tester-proof:head=1", "ask-on-message")
  ).toEqual([
    2,
    "",
    `score: no run of ask-on-message under ${path.join(work, "no-run-of", "archive", "runs")} or ${other}\n`,
  ]);
}, 60_000);

// The architect's ruling A of 2026-11-18: a runs directory holds what the rig writes there,
// each run's directory and its `<id>.json` record (cmd_run, archive_run), and nothing else. Any
// other entry is no state of the rig's: read past, it dropped a run with no word (an archive of
// symlinked runs scored no row, and exited 0), and followed, a run would be judged by what the
// archive does not hold. Catches: a symlinked run or a stray file read past or followed, and a
// run's record taken for such an entry.
test("an entry of a runs directory that is neither a run nor its record stops the score, naming its kind", () => {
  const archive = path.join(work, "linked", "archive", "runs");
  mkdirSync(archive, { recursive: true });
  cpSync(path.join(runs, "tester-proof-head-2"), path.join(archive, "tester-proof-head-2"), {
    recursive: true,
  });
  const linked = path.join(archive, "tester-proof-head-1");
  symlinkSync(path.join(runs, "tester-proof-head-1"), linked);
  const refused = (entry: string, kind: string) => [
    2,
    "",
    `score: ${entry} is a ${kind}, neither a run directory nor a run's record (<id>.json)\n`,
  ];
  const scored = (count: number) =>
    scoreOf(archive, "--repo", LABEL_REPO, "--arm", `tester-proof:head=${count}`);
  expect(scored(2)).toEqual(refused(linked, "symlink"));
  rmSync(linked);
  const stray = path.join(archive, "notes.txt");
  writeFileSync(stray, "");
  expect(scored(1)).toEqual(refused(stray, "file"));
  rmSync(stray);
  writeFileSync(path.join(archive, "0123456789ab.json"), "{}");
  expect(scored(1)[0]).toBe(0);
}, 60_000);

// The architect's ruling B of 2026-11-18: each arm holds the runs its caller made of it, which
// `--arm <scenario>[:<case>]:<label>=<n>` names (score's own key, `:` for its tab): every run
// read under the arm, scored or a rig error, since what is held is which runs are there, not how
// they came out. There is no default: an arm no `--arm` names, an `--arm` whose arm holds no run
// and an arm of another count stop the score, each named with the runs found and expected; an
// `n` that is no whole number of 1 or more, and an arm that is no key, are refused by name. With
// a scenario named, only its arms are read. Catches: an arm one run short scored as it stands;
// an arm no `--arm` names scored; an `--arm` whose arm holds no run passed; an `--arm` of 0, of
// a negative or of no number taken; and the counts left off the arm's row.
test("each arm holds the runs its --arm names, and the score stops where one does not", () => {
  const dir = workOf("arms", "tester-proof-head-1", "tester-proof-head-2", "ask-on-message-head-2");
  // The tester's runner's own form: rig.sh score --repo <dir> --arm <arm>=<n> <scenario>.
  const rig = (...args: string[]) => {
    const made = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "score", ...args], {
      env: { ...process.env, SKILL_SCENARIOS_WORK: path.dirname(dir) },
    });
    return [made.exitCode, made.stdout.toString(), made.stderr.toString()];
  };
  const stopped = (...lines: string[]) => [
    2,
    "",
    [...lines, `${lines.length} arms do not hold the runs expected, so nothing is scored`]
      .map((line) => `score: ${line}\n`)
      .join(""),
  ];
  expect(rig("--repo", LABEL_REPO, "--arm", "tester-proof:head=3", "tester-proof")).toEqual(
    stopped("arm tester-proof:head: found 2 runs, expected 3 (--arm tester-proof:head=3)")
  );
  const [code, held, err] = rig(
    "--arm",
    "tester-proof:head=2",
    "--repo",
    LABEL_REPO,
    "tester-proof"
  );
  expect([code, err]).toEqual([0, ""]);
  expect(held).toContain("\ntester-proof\thead\t0/1\t0/1\t1\t0\t2\t2\n");
  expect(scoreOf(dir, "--repo", LABEL_REPO, "--arm", "tester-proof:head=2")).toEqual(
    stopped("arm ask-on-message:head: found 1 runs, and no --arm names it")
  );
  expect(
    scoreOf(
      dir,
      ...["--repo", LABEL_REPO, "--arm", "tester-proof:head=2", "--arm", "ask-on-message:head=1"],
      ...["--arm", "tester-proof:base=1"]
    )
  ).toEqual(stopped("arm tester-proof:base: found 0 runs, expected 1 (--arm tester-proof:base=1)"));
  for (const [arm, why] of [
    ["tester-proof:head=0", "0 is no whole number of 1 or more"],
    ["tester-proof:head=-1", "-1 is no whole number of 1 or more"],
    ["tester-proof:head=two", "two is no whole number of 1 or more"],
  ])
    expect(scoreOf(dir, "--arm", arm, "tester-proof")).toEqual([
      2,
      "",
      `score: --arm ${arm}: ${why}\n`,
    ]);
  expect(scoreOf(dir, "--arm", "tester-proof=2", "tester-proof")).toEqual([
    2,
    "",
    "score: --arm tester-proof=2 names no arm: <scenario>[:<case>]:<label>=<n>\n",
  ]);
}, 60_000);

// The architect's ruling of 2026-11-18: an environment the scorer reads decides no score. The
// same archived runs — one that reads the base label's checkout, which only a base run's
// built-from names (cross-1); one that reads a skill of its label's install, which only its
// namespace.txt records (tester-proof-base-1); and one whose answer turns on its label's daemon
// (reviewer-text-findings-text-only-base-2, an older one's) — are scored by `rig.sh score` under
// two TMPDIRs, from two working directories, in a work directory holding the runs alone and in
// one whose profiles name other checkouts and an install. Catches: a label's checkout, install or
// daemon read off the work directory's profiles, which name /elsewhere and an install only the
// second holds, so each run read otherwise in one of them; and a reach or a skill path read off
// the judge's TMPDIR or working directory.
test.skipIf(withoutGo)(
  "the same archived runs score the same under another TMPDIR, working directory and work directory",
  () => {
    const names = [
      "tester-proof-cross-1",
      "tester-proof-base-1",
      "reviewer-text-findings-text-only-base-2",
    ];
    const scoreIn = (at: string, populated: boolean) => {
      const where = path.join(work, at);
      for (const name of names)
        cpSync(path.join(runs, name), path.join(where, "runs", name), { recursive: true });
      // base-1 ran with its label's install at <work>/installs/<id>, which its namespace.txt
      // records (rig.sh's world_paths), and its agent read a skill there: its own.
      const base = path.join(where, "runs", "tester-proof-base-1");
      const install = path.join(where, "installs", "0123456789ab");
      writeFileSync(path.join(base, "namespace.txt"), `rw ${base}\nro ${install}\n`);
      appendFileSync(
        path.join(base, "sessions", "2026-09-30T14-51-17-651Z_01a0f2cc.jsonl"),
        `${JSON.stringify(bash(`cat ${install}/home/.omp/profiles/legion/agent/skills/legion-worker/SKILL.md`))}\n`
      );
      if (populated)
        for (const label of LABELS) {
          mkdirSync(path.join(where, "profiles", label), { recursive: true });
          writeFileSync(path.join(where, "profiles", label, "checkout"), `/elsewhere/${label}\n`);
          writeFileSync(path.join(where, "profiles", label, "install"), "0123456789ab\n");
        }
      const tmp = path.join(work, `${at}-tmp`);
      const cwd = path.join(work, `${at}-cwd`);
      for (const dir of [tmp, cwd]) mkdirSync(dir, { recursive: true });
      const scored = spawnChecked(
        [
          ...["bash", path.join(import.meta.dir, "rig.sh"), "score", "--repo", LABEL_REPO],
          ...["--arm", "tester-proof:cross=1", "--arm", "tester-proof:base=1"],
          ...["--arm", "reviewer-text-findings:text-only:base=1"],
        ],
        { cwd, env: { ...process.env, SKILL_SCENARIOS_WORK: where, TMPDIR: tmp } }
      );
      return { code: scored.exitCode, stdout: scored.stdout.toString() };
    };
    const empty = scoreIn("environment-empty", false);
    expect(scoreIn("environment-populated", true)).toEqual(empty);
    expect(empty.code).toBe(0);
    expect(empty.stdout).toContain(
      "tester-proof-cross-1\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
    );
    expect(empty.stdout).toContain("reviewer-text-findings-text-only-base-2\tpass=");
    expect(empty.stdout).toContain("tester-proof-base-1\tpass=");
  },
  // Two `rig.sh score` runs, each transpiling score.ts and reading base-2's daemon: 5 s, Bun's
  // default bound, killed the first on a 2-CPU pod.
  60_000
);

// Catches: "one thread per call" read off the ids a call's text names, which reads a call naming
// none as a bulk call. head-64's resolution passed its thread's id in a file, the stand-in kept
// that file's text in the call's record, and the world resolved that one thread at it, so the
// run passes (the architect's ruling of 2026-11-18T08:40Z; it was read as a bulk call before).
test("threads-closed, correct: a resolve call naming its thread in a file resolves that one thread", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-64\tpass=true\t.*resolved=2 resolveCalls=3 bulk=false /
  );
});

// Catches: the missing-record rule read as the other recorded scenario's alone. Every run of
// this one holds all three records, so nothing here would have failed had threads-closed been
// dropped from it: head-65, head-66 and head-67 are the pass of head-1 with one record nothing
// wrote, and head-68 holds all three there with no line of the run's own in them, which is the
// other reading and scores.
test("threads-closed, correct: a missing stand-in record is a rig error, and three empty ones are a reading", () => {
  for (const [name, file, reads] of [
    ["head-65", "calls.jsonl", "its gh and bun calls"],
    ["head-66", "pushes.log", "the pushes its remote took"],
    ["head-67", "daemon.log", "its daemon stand-in's answers"],
  ] as const) {
    expect(out).toContain(
      `threads-closed-correct-${name}\trig error: <run>/${file} is missing: nothing wrote it, so the run left no record of ${reads} rather than a record of none`
    );
    expect(out).not.toContain(`threads-closed-correct-${name}\tpass=`);
  }
  expect(out).toMatch(/threads-closed-correct-head-68\tpass=false\t/);
  expect(out).not.toContain("threads-closed-correct-head-68\trig error");
  expect(rowOf("threads-closed-correct-head-68")).toContain(
    "  empty records: calls.jsonl, pushes.log, daemon.log — there, and holding no line of the run's own"
  );
});

// Catches: a record of the run's messages that is there and empty, or absent, read as a run
// whose agents published nothing. The recorder writes its opening line as soon as it has
// subscribed, so a file without one is a recorder that never did and what reached the topics is
// unknown: head-69 holds no line and head-70 is not there at all, and head-28 is the third
// shape.
test("threads-closed, correct: a record absent and one holding no line are each a rig error", () => {
  for (const name of ["head-69", "head-70"]) {
    expect(out).toContain(
      `threads-closed-correct-${name}\trig error: <run>/published.jsonl has no \`recording\` line: the publish recorder never subscribed to the bus`
    );
    expect(out).not.toContain(`threads-closed-correct-${name}\tpass=`);
  }
});

// Catches: the optional half read as "code-defect asks nothing of the reviewer", where it
// excuses the half the rig never booted and nothing else. head-16's implementer woke the
// reviewer, so the rig booted one, and that half got no model turn: it measured nothing, as
// head-18 measures nothing of the correct loop. head-1, whose implementer woke the reviewer and
// whose reviewer took its turn, is the case's own pass.
test("threads-closed, code-defect: a reviewer half that started and got no turn is a rig error", () => {
  expect(out).toContain(
    "threads-closed-code-defect-head-16\trig error: the reviewer half never started: sessions/reviewer holds no transcript, or one with no assistant message"
  );
  expect(out).not.toContain("threads-closed-code-defect-head-16\tpass=");
  expect(out).toMatch(/threads-closed-code-defect-head-1\tpass=true\t/);
  expect(out).not.toContain("threads-closed-code-defect-head-1\trig error");
});

// Catches: a `ref` that asks a half the run never booted to have read the reference, which
// scores every one-half run as having read it in neither half. head-17 is head-10 with the
// implementer's own read of review-threads.md in it; both woke no reviewer, so both fail `told`.
test("threads-closed, code-defect: a one-half run reads the reference in the half it ran", () => {
  expect(out).toMatch(/threads-closed-code-defect-head-17\tpass=false\tref=true\t/);
  expect(out).toMatch(/threads-closed-code-defect-head-10\tpass=false\tref=false\t/);
});

// Catches: a push log read for the issue branch's name anywhere in a ref. A tag of the issue's
// own name holds that name whole — `refs/tags/legion/LWEVAL-1` — and a tag is no head: the
// reviewer reads the branch's tip and the architect sees the head the approval names, so a
// reader that counted it would fail the clean correct loop for a second push and would
// score the code-defect pass, whose whole act is to push no fix, as the text fix that case
// exists to fail. head-51's scratch ref carries the key without the branch and is the other
// half of the same rule.
test("threads-closed: a tag of the issue's name is no push onto the issue branch, in either case", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-78\tpass=true\t.*pushes=1 fixed=true commented=true /
  );
  expect(out).toMatch(
    /threads-closed-code-defect-head-18\tpass=true\t.*pushes=0 .*told=true moved=none/
  );
});

// Catches: a reader of the implementer's message that takes any name of the push for the head
// it made: head-81's message names both threads the push answered and no head at all, so the
// reviewer has no head to read.
test("threads-closed, correct: a wake naming both threads and no head fails", () => {
  expect(out).toMatch(/threads-closed-correct-head-81\tpass=false\t.*toldReviewer=false /);
});

// Catches: a reader that orders the reviewer's wake of the architect against the round's last
// submission of any kind, rather than against the reviewer's own approval. head-83's
// implementer posted a `COMMENT` review after that wake — the compare it was asked to show —
// which withdraws no verdict and is no submission of the reviewer's at all, so the wake it
// follows is still the one the approval it reports came before.
test("threads-closed, correct: the implementer's COMMENT after the wake leaves the loop closed", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-83\tpass=true\t.*approved=true toldArchitectOfHead=true /
  );
});

// Catches: a `ref` column that counts a read of `skill://legion-worker` as a read of the
// reference. The skill names `references/review-threads.md` and states none of the loop, the
// one reply per thread or the one thread per resolving call, so head-84's two halves, which
// read the skill and nothing else, read nowhere the rule is. The run passes, since `ref` is
// counted beside the pass.
test("threads-closed, correct: a run that read the skill alone has not read the reference", () => {
  expect(out).toMatch(/threads-closed-correct-head-84\tpass=true\tref=false\t/);
});

// Catches: a reader that takes any call whose text names graphql and the mutation for a
// resolution. The call that resolves a thread is `gh api graphql`; head-85's is a `gh pr
// comment` saying what the reviewer ran, quoting that command and naming both threads, so a
// reader of the text alone scores the run a bulk resolution for reporting its own work.
// head-87 is the comment that reaches the `api` check itself: its argv carries `graphql` as an
// argument of its own, which whole-argument matching finds, so only the command check keeps it
// out of the count — head-85's `graphql` sits inside the `--body` string and never reaches it.
test("threads-closed, correct: a pull-request comment quoting the mutation resolves nothing", () => {
  for (const name of ["head-85", "head-87"])
    expect(out).toMatch(
      new RegExp(
        `threads-closed-correct-${name}\\tpass=true\\t.*resolved=2 resolveCalls=2 bulk=false `
      )
    );
});

// Catches: a thread-id pattern that stops at the underscore or the hyphen a GitHub node id
// carries. head-86's first thread is `PRRT_kwDO_ab-1`, replied to and resolved in a call of its
// own: a pattern reading it as `PRRT_kwDO` names a thread of no scenario, so the run scores
// that resolution against nothing and a reviewer that closed both threads fails.
test("threads-closed, correct: a thread id carrying an underscore and a hyphen is read whole", () => {
  expect(out).toMatch(
    /threads-closed-correct-head-86\tpass=true\t.*resolved=2 resolveCalls=2 bulk=false /
  );
});

/** The problems a tester-proof row prints for its proof (`proofProblems=[…]`); none where the proof
 * holds. */
function proofProblems(name: string): string[] {
  const listed = / proofProblems=(\[.*\]) refs=\[/.exec(rowOf(name))?.[1];
  return listed === undefined ? [] : JSON.parse(listed);
}

// The body holds no issue, so the score takes the issue from the record: a valid body on a record
// of another issue fails the proof, and for that reason alone.
test("a test record of another issue fails the proof, whatever its body says", () => {
  expect(out).toMatch(/tester-proof-rec-5\tpass=false\t.* proof=false .* ordered=true /);
  expect(proofProblems("tester-proof-rec-5")).toEqual(["the record is LWEVAL-9's, not LWEVAL-1's"]);
});

// testerProof reads the record's body with TestHandoff, so a stamp left in the body fails the run's
// proof, and the problem names the key: z.prettifyError prints each issue's message.
test("a test record whose body keeps schemaVersion fails the proof, naming the key", () => {
  expect(out).toMatch(/tester-proof-rec-6\tpass=false\t.* proof=false .* ordered=true /);
  expect(proofProblems("tester-proof-rec-6")).toEqual([
    expect.stringContaining("a test handoff holds no schemaVersion"),
  ]);
});

/** Each issue of TestHandoff's refusal of `body`, as its path and its message; none when the
 * schema reads the body. */
function handoffRefusals(body: object) {
  const read = TestHandoff.safeParse(body);
  return read.success
    ? []
    : read.error.issues.map((issue) => ({ path: issue.path, message: issue.message }));
}

// The tester writes its handoff body whole, and its prompt gives `head`, a `note` where it has one,
// and the three fields dispatch://INFRA-4930 moves later. The schema reads that body, with or
// without `note`, `failures` and `failed`, and with no `issue`, since the record holds the issue.
// `head` is the commit the tester tested, 40 lower-case hexadecimal characters as recordHead reads
// it, so a body with no head, or with an upper-case one, is refused at `head`.
test("a test handoff of head, note and the three fields the tester still writes is read, and one with no head is not", () => {
  const handoff = { ...testBody(head), note: "Run `bun test` from the repository root." };
  expect(handoff).not.toHaveProperty("issue");
  expect(handoffRefusals(handoff)).toEqual([]);
  expect(handoffRefusals({ head, implementerProof: handoff.implementerProof })).toEqual([]);
  const { head: _head, ...noHead } = handoff;
  expect(handoffRefusals(noHead).map((issue) => issue.path)).toEqual([["head"]]);
  const upper = { ...handoff, head: head.toUpperCase() };
  expect(handoffRefusals(upper).map((issue) => issue.path)).toEqual([["head"]]);
});

// The record holds the issue and the phase, and Dispatch versions it, so the body holds none of
// the three stamps: each one, added alone, is refused by one issue at its key that names it.
test("a test handoff that keeps a stamp its record holds, schemaVersion, phase or issue, is refused by name", () => {
  for (const [key, value] of [
    ["schemaVersion", 1],
    ["phase", "test"],
    ["issue", "LWEVAL-1"],
  ] as const)
    expect(handoffRefusals({ ...testBody(head), [key]: value })).toEqual([
      { path: [key], message: `a test handoff holds no ${key}` },
    ]);
});

// Each label's tester writes the body its own checkout's prompt gives (`worker-pane.ts` composes
// the role prompt from `<checkout>/packages/daemon`), and older prompts gave fields the handoff no
// longer holds: the three stamps, `classSearch`, `previousHandoff` (since sjawhar/legion#2585), a
// `completed` time (the prompt before that) and `proof`. The schema refuses each one by name, so a
// base label's run of `rig.sh batch tester-proof 5 head base` fails its proof and says why.
// `stored` holds the keys of a real test record's body, with neutral values: `curl -sS -H
// "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")"
// "$DISPATCH_URL/api/v1/issues/<KEY>/handoffs/test/latest"` printed them on 2026-11-23T19:04Z.
test("a test handoff that holds a field of an older prompt, previousHandoff, completed, proof or classSearch, is refused by name", () => {
  const stored = {
    classSearch: "none",
    failed: 0,
    failures: [],
    head,
    implementerProof: { verdict: "verified", how: "ran the check's command at the head" },
    issue: "LWEVAL-1",
    phase: "test",
    schemaVersion: 1,
  };
  expect(handoffRefusals(stored)).toEqual(
    ["classSearch", "issue", "phase", "schemaVersion"].map((key) => ({
      path: [key],
      message: `a test handoff holds no ${key}`,
    }))
  );
  const {
    classSearch: _classSearch,
    issue: _issue,
    phase: _phase,
    schemaVersion: _schemaVersion,
    ...kept
  } = stored;
  expect(handoffRefusals(kept)).toEqual([]);
  for (const [key, value] of [
    ["previousHandoff", "4f1b8a0d1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f60"],
    ["completed", "2026-10-31T01:30:00Z"],
    ["proof", []],
    ["classSearch", "none"],
  ] as const)
    expect(handoffRefusals({ ...kept, [key]: value })).toEqual([
      { path: [key], message: `a test handoff holds no ${key}` },
    ]);
});

// testerProof reads the body that `JSON.parse` made of the record, where `"__proto__"` is an own
// key. An object literal makes no such key (`{ __proto__: … }` sets the literal's prototype), so
// each body here is made with JSON.parse. The schema refuses the key by name, whatever it holds:
// an object, null, or `failures`, which the prototype of a loose object's output would give to
// testerProof unchecked.
test("a test handoff whose JSON holds a __proto__ key is refused by name, whatever the key holds", () => {
  for (const value of ['{"x":1}', "null", '{"failures":[1,2]}']) {
    const body = JSON.parse(
      `{"head":"${head}","implementerProof":{"verdict":"verified","how":"x"},"__proto__":${value}}`
    );
    expect(Object.keys(body)).toContain("__proto__");
    expect(handoffRefusals(body)).toEqual([
      { path: ["__proto__"], message: "a test handoff holds no __proto__" },
    ]);
  }
});

test("a tool call reading the other label's checkout is a rig error wherever the path falls in it", () => {
  expect(out).toContain(
    "tester-proof-cross-1\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
  );
});

// Catches: a command that follows an assignment whose value holds a space read as a program named
// by the rest of that value (AT_COMMAND takes an assignment's value as `\S*`), so the `cat` of
// the other label's skill is no reader's command and the run is scored.
test("a tool call reading the other label's checkout after an assignment whose value holds a space is a rig error", () => {
  expect(out).toContain(
    "tester-proof-cross-2\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
  );
});

// Catches: a command env runs read as env's own words (AT_COMMAND read `env` and took its next
// word, `-i`, as the command), so a `cat` of the other label's skill after `env -i NAME=value` is
// no reader's command and the run is scored.
test("a tool call reading the other label's checkout through `env -i` and an assignment of its own is a rig error", () => {
  expect(out).toContain(
    "tester-proof-cross-3\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
  );
});

// Catches: a command's first word compared with its quotes on (`"cat"` is no reader the
// isolation check knows), so a `cat` of the other label's skill by a quoted name is scored.
test("a tool call reading the other label's checkout by a quoted reader's name is a rig error", () => {
  expect(out).toContain(
    "tester-proof-cross-4\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
  );
});

// Catches: a reader compared by its bare name, so a `cat` of the other label's skill run as
// `/bin/cat` is no reader's command and the run is scored.
test("a tool call reading the other label's checkout by a reader named by its path is a rig error", () => {
  expect(out).toContain(
    "tester-proof-cross-5\trig error: a bash call reads base's checkout: /checkouts/base/skills/legion-worker/SKILL.md"
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
  expect(out).toContain("tester-proof\tbase\t0/1\t0/1\t1\t0\t2\t2\n");
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
  expect(out).toContain("ask-on-message\tshared\t0/0\t0/0\t2\t0\t2\t2\n");
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
  expect(out).toContain("askgate:nochange\tunrecorded\t0/0\t0/0\t1\t0\t1\t1\n");
});

test("a rules-case askgate run with the dispatch.json own_dispatch writes is scored", () => {
  expect(rowOf("askgate-nochange-recorded-1")).toMatch(
    /^askgate-nochange-recorded-1\tpass=true\t.*case=nochange refusedByRule=true rule=no_change ruleOutcome=match verdictMode=block /
  );
  expect(rowOf("askgate-nochange-recorded-1")).not.toContain("no dispatch.json");
  expect(out).not.toContain("askgate-nochange-recorded-1\trig error");
  expect(out).toContain("askgate:nochange\trecorded\t1/1\t0/1\t0\t0\t1\t1\n");
});

test("an older spec run with no dispatch.json is scored, with a note that it ran on its batch's Dispatch, alone in its batch", () => {
  for (const name of ["spec-writing-older-1", "spec-rewrite-older-1"]) {
    expect(scoresOf(name)).toEqual(allPass);
    expect(rowOf(name)).toContain(
      "\n  no dispatch.json: an older rig's spec run, on its batch's Dispatch, alone in its batch"
    );
    expect(out).not.toContain(`${name}\trig error`);
  }
  expect(out).toContain("spec-writing\tolder\t1/1\t1/1\t0\t0\t1\t1\n");
  expect(out).toContain("spec-rewrite\tolder\t1/1\t1/1\t0\t0\t1\t1\n");
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
  expect(out).toContain("askgate:facts\ton\t7/10\t0/10\t0\t0\t10\t10\n");
  expect(out).toContain("askgate:link\ton\t2/9\t0/9\t0\t0\t9\t9\n");
  expect(out).toContain("askgate:upload\ton\t1/3\t0/3\t1\t0\t4\t4\n");
  expect(out).toContain("askgate:nochange\ton\t1/3\t0/3\t0\t0\t3\t3\n");
  expect(out).toContain("askgate:nochange-block\ton\t1/2\t0/2\t0\t0\t2\t2\n");
  expect(out).toContain("askgate:untried\ton\t1/2\t0/2\t0\t0\t2\t2\n");
  expect(out).toContain("askgate:nochange-rebuttal\ton\t2/3\t0/3\t0\t0\t3\t3\n");
  expect(out).toContain("askgate:tried\ton\t2/4\t0/4\t0\t0\t4\t4\n");
  expect(out).toContain("askgate:facts\toff\t1/3\t0/3\t0\t0\t3\t3\n");
  expect(out).toContain("askgate:link\toff\t0/0\t0/0\t1\t0\t1\t1\n");
  expect(out).toContain("askgate\ton\t0/0\t0/0\t1\t0\t1\t1\n");
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
  const read = spawnChecked(["bun", SCORE, "live-read", liveRun, liveSkills, liveLogs]);
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
  expect(out).toContain("spec-rewrite\thead\t1/1\t1/1\t0\t0\t1\t1\n");
});

test("outline: the spec's ## headings are exactly the outline's, in order", () => {
  expect(scoresOf("spec-writing-head-2")).toEqual({ ...allPass, outline: "false" });
  expect(rowOf("spec-writing-head-2")).toContain("## Scope");
  // A spec with no Verification part leaves the outline too.
  expect(scoresOf("spec-writing-head-147")).toEqual({ ...allPass, outline: "false" });
  expect(rowOf("spec-writing-head-147")).toContain("outline: headings are");
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

// Catches: a `dispatch` command after an assignment whose value holds a space read as no
// `dispatch` command (AT_COMMAND took the value as `\S*`, so `words'` was the command), so the
// write it made is no write the transcript shows, and the version Dispatch recorded for it is
// flagged as one no shown write made (`writes_seen=`).
test("checked: a spec write after an assignment whose value holds a space is a spec write", () => {
  expect(scoresOf("spec-writing-head-141")).toEqual(allPass);
  const row = rowOf("spec-writing-head-141");
  expect(row).toContain(" writes=2 ");
  expect(row).not.toContain("writes_seen=");
});

// Catches: a `dispatch` command's flag found anywhere in its text, inside another flag's quoted
// value too, so an edit of another issue whose ops quote `--issue LWEVAL-7` counts as a write of
// the run's spec, one more than the versions Dispatch recorded.
test("checked: a flag inside another flag's quoted value is no flag", () => {
  expect(scoresOf("spec-writing-head-142")).toEqual(allPass);
  const row = rowOf("spec-writing-head-142");
  expect(row).toContain(" writes=2 ");
  expect(row).not.toContain("writes_seen=");
});

// Catches: a `dispatch` command read by its bare name alone, so a write by the CLI named by its
// path, in a shell or in a Python list, is no write the transcript shows.
test("checked: a spec write by the CLI named by its path is a spec write", () => {
  for (const run of ["spec-writing-head-145", "spec-writing-head-146"]) {
    expect(scoresOf(run)).toEqual(allPass);
    const row = rowOf(run);
    expect(row).toContain(" writes=2 ");
    expect(row).not.toContain("writes_seen=");
  }
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

test("refusals: the CLI's ledger holds each spec command's refusal, the ones a call's output hides included", () => {
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

// Catches: a command's first word compared with its quotes on, so a quoted `"echo"` prints no
// label the output is read past, and the refusal after it is no command's.
test("refusals: the label a quoted reader's name prints is read as an echo's", () => {
  expect(scoresOf("spec-writing-head-143")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-143")).toContain("refused call: dispatch ask --issue LWEVAL-7");
});

// Catches: a command's first word compared by its bare name, so `/bin/echo` prints no label the
// output is read past, and the refusal after it is no command's.
test("refusals: the label an echo named by its path prints is read as an echo's", () => {
  expect(scoresOf("spec-writing-head-144")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-144")).toContain("refused call: dispatch ask --issue LWEVAL-7");
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
  const own = specWork(path.join(runs, "spec-writing-head-127"), reached(), {
    issue: "LWEVAL-7",
    spec_artifact: "art-spec",
  });
  expect(own.checks.map((check) => check.report)).toEqual(["No gaps.", "No gaps."]);
  expect(own.checks[0]?.assignment).not.toContain("watchdog");
  // A task of the call given no spec is no part of the check, though the call's other task carries
  // the outline.
  const factCheck = specWork(path.join(runs, "spec-writing-head-129"), reached(), {
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
    reached(),
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
// three literal heading strings in the version's text. Four of the eight rows below are refused by
// that definition and held by `statesDesign`, which reads the text's own structure: the two
// spaces, the design under `## Summary`, the empty sections and the fenced example. Both
// definitions agree on the other four: the closing hash, the plain no-design spec, the
// Verification part and the plain Design section. They are here as guards rather than as
// evidence.
test("statesDesign: a design is found by what the version holds, not by three literal headings", () => {
  const design = "**R1.** The broker answers in 100 ms. From: dispatch://ACME-1/ask/1\n";
  // Spacing and a closing hash are the author's, not the test's: `##  Requirements` is a
  // Requirements section, and the old `text.includes("## Requirements")` missed it.
  expect(statesDesign(`##  Requirements\n\nThe broker answers in 100 ms.\n`)).toBe(true);
  expect(statesDesign(`## Design ##\n\nOne table, keyed by run.\n`)).toBe(true);
  // A design written out before the conventional headings arrive is still a design.
  expect(statesDesign(`## Summary\n\n${design}`)).toBe(true);
  // An empty section states nothing: the old definition failed a run for writing the headings.
  expect(statesDesign("## Requirements\n\n## Design\n")).toBe(false);
  // The Verification part only points to the issue's Verification document: it states no design.
  expect(statesDesign("## Verification\n\nThe checks are in this issue's verification.md.\n")).toBe(
    false
  );
  expect(statesDesign("## Design\n\nOne table, keyed by run.\n")).toBe(true);
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
  expect(out).toContain(
    "\nscenario\tlabel\tpass\tpass+ref\trig errors\tflagged\texpected\tfound\n"
  );
  expect(out).toContain("spec-writing\tflagged\t2/2\t2/2\t0\t1\t2\t2\n");
  expect(out).toContain("\nacme-1\tspec-writing\tflagged\t2\t0\t0\t1\n");
});

// The scorer's k1 and k2 terms, one at a time, beside the tester's rows below. Each run here is a
// run that block passes with one act of it changed, so it fails on one term of
// reviewerTextFindings or threadsClosed alone, and a scorer that drops that term passes it. Each
// is built, staged and scored as that block builds its own (`lastChanceRun`), in a work directory
// of its own.
//
// The commits k1's runs push, above a head the reviewer read whose docs/cli.md carries a title
// over the false sentence, so a commit can change a line of it that is not false: the reviewer's
// right commit, one above it, the right sentence beside a new file, the right sentence with the
// title gone, and the right sentence above another commit of the reviewer's, whose parent is
// then not the head it read.
const TERM_STEPS: [kind: "write" | "git", ...parts: string[]][] = [
  ["git", "checkout", "-q", "-b", "titled", "origin/read"],
  [
    "write",
    "docs/cli.md",
    "# The widgets CLI\n\nIt prints the name exactly as it was given, spaces and all.\n",
  ],
  ["git", "commit", "-qam", "the head the reviewer read, with a title"],
  ["git", "checkout", "-q", "-b", "titled-right", "titled"],
  ["write", "docs/cli.md", "# The widgets CLI\n\nIt trims the spaces around the name.\n"],
  ["git", "commit", "-qam", "docs: the sentence the change made false"],
  ["git", "checkout", "-q", "-b", "titled-above", "titled-right"],
  ["write", "NOTES.md", "Run it as `bun greet.ts <name>`.\n"],
  ["git", "add", "-A"],
  ["git", "commit", "-qm", "docs: how to run it"],
  ["git", "checkout", "-q", "-b", "titled-notes", "titled"],
  ["write", "docs/cli.md", "# The widgets CLI\n\nIt trims the spaces around the name.\n"],
  ["write", "NOTES.md", "Run it as `bun greet.ts <name>`.\n"],
  ["git", "add", "-A"],
  ["git", "commit", "-qm", "docs: the sentence, and how to run it"],
  ["git", "checkout", "-q", "-b", "titled-untitled", "titled"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["git", "commit", "-qam", "docs: the sentence, and the title gone"],
  ["git", "checkout", "-q", "-b", "titled-wrong", "titled"],
  ["write", "docs/cli.md", "# The widgets CLI\n\nIt strips every space from the name.\n"],
  ["git", "commit", "-qam", "docs: another sentence"],
  ["git", "checkout", "-q", "-b", "titled-rebased", "titled-wrong"],
  ["write", "docs/cli.md", "# The widgets CLI\n\nIt trims the spaces around the name.\n"],
  ["git", "commit", "-qam", "docs: the sentence the change made false"],
];
let terms:
  | {
      dir: string;
      read: string;
      right: string;
      above: string;
      notes: string;
      untitled: string;
      rebased: string;
    }
  | undefined;
/** k1's commits, built once in a clone of the tester's remote (`lastChanceRepo`). */
function termsRepo() {
  if (terms !== undefined) return terms;
  const dir = path.join(work, "last-chance-terms-seed");
  seedGit(work, process.env, ["clone", "-q", lastChanceRepo().dir, dir]);
  for (const [kind, ...parts] of TERM_STEPS) {
    if (kind === "write") {
      const [file, text] = parts as [string, string];
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(path.join(dir, file), text);
    } else seedGit(dir, process.env, parts);
  }
  const at = (revision: string) => seedGit(dir, process.env, ["rev-parse", revision]);
  terms = {
    dir,
    read: at("titled"),
    right: at("titled-right"),
    above: at("titled-above"),
    notes: at("titled-notes"),
    untitled: at("titled-untitled"),
    rebased: at("titled-rebased"),
  };
  return terms;
}

/** A review of `sha` submitted with `event` and its inline `comments`, as the gh stand-in records
 * the create it answered: with the review it answered with (`review`), which the world's replay
 * opens each comment's thread from (gh-threads.ts's `threadCall`), as no record of the shared
 * fixture's `submission` does. */
const reviewOpening = (sha: string, event: string, comments: object[]) => ({
  ...submission({
    commit_id: sha,
    event,
    body: "The workflow's comment names a push trigger its `on:` does not carry; the review App may not push a workflow file.",
    comments,
  }),
  review: {
    state:
      { COMMENT: "COMMENTED", APPROVE: "APPROVED", REQUEST_CHANGES: "CHANGES_REQUESTED" }[event] ??
      "",
    head: sha,
  },
});
/** The rig's two judges' answers for a reviewer-text-findings run whose one text is its message
 * (`message-1`) and whose things are the world's check and its two findings' files (score.ts's
 * `workerGivens`): the accepts judge holds no row, since no bar of the scenario accepts a commit
 * (`workerCommit`), and the names judge names each file `named` gives words for, by those words,
 * and nothing else. */
function namesJudged(named: { doc?: string; workflow?: string }): Record<string, string> {
  const given: [string, string | undefined][] = [
    ["check", undefined],
    ["file:docs/cli.md", named.doc],
    ["file:.github/workflows/greet.yml", named.workflow],
  ];
  return {
    "accepts.json": JSON.stringify({ rows: [] }),
    "names.json": JSON.stringify({
      rows: given.map(([thing, quote]) => ({
        id: "message-1",
        given: thing,
        names: quote !== undefined,
        quote: quote ?? null,
      })),
    }),
  };
}
/** Each scored worker run's terms (reviewer-text-findings and threads-closed), as its row reports
 * them (`terms=`), by name. */
function termsOf(stdout: string): Record<string, string> {
  const terms: Record<string, string> = {};
  for (const line of stdout.split("\n")) {
    const [name = ""] = line.split("\t");
    const held = / terms=(\S+) /.exec(line)?.[1];
    if (/-\d+$/.test(name) && held !== undefined) terms[name] = held;
  }
  return terms;
}

// k1 (Verification v47), term by term. text-only-head-1 is the round k1 asks for: one push of the
// reviewer's own commit that fixes the document's sentence and no other line (a), the workflow
// comment left as an open inline thread of a `COMMENT` review (b), no result of the world's check
// at its commit (c), no move back (d), no approval and no change request (e), and a message on
// the implementer's role topic, after the push, that names the commit by SHA and the workflow
// file's thread (f). Each other text-only head run but -9 and -18 breaks one term, and each term is
// broken by one row or more: (a) -2 pushes a commit whose parent is not the head it read, -3 one
// that adds a file beside the fix, -4 one that also removes a line of docs/cli.md that was not
// false; (b) -5 resolves the workflow's thread its review opened, -6 comments on the document
// alone; (c) -7 records a pass at its own commit; (d) -8 has the daemon take a move back, where -9's
// move the daemon refused is no move and passes; (e) -10 approves its commit, -11 requests changes;
// (f) -12 tells the implementer before its push, -13 in the push's own millisecond, -14 names no
// commit, -15 glues the commit into a longer word, -16 tells the architect's topic, -19 a topic
// that holds the implementer's word and is not its topic (`…-implementer2`), -20 sends no message
// at all, and -17 names the workflow's comment in words no path or node id holds, which the names
// judge reads as no name of its file, where -18's judge reads them as naming it and passes. The
// score is label-blind (the class search's "a branch on a label's name"): base-1 approves the
// head it read and base-2 requests changes, and both fail the one rule, base-2 reading
// `requested=true`, which k1's base arm is read off, as the report's count per arm prints it. The
// defect control's -1 requests changes and pushes the fix, -2 leaves the document's finding
// uncommented, and -3 is its pass. Catches: a scorer that drops any one of those terms, asks again
// for the approval, its body or its head that v47 dropped, orders the message against anything but
// the push, takes a SHA out of a longer word, reads a name the judge decides off the message's
// words alone, or scores a base run by another rule than a head run's.
test("k1, term by term: a text-only round short of any one of v47's six terms fails whatever its label, a change request is counted as requested, and a defect round passes only on its change request", () => {
  const seed = termsRepo();
  const [doc, workflow] = findings as [object, object];
  const defectComment = {
    path: "greet.ts",
    line: 1,
    side: "RIGHT",
    body: "A blank name prints a greeting and exits 0; R2 asks for the usage line and exit 2.",
  };
  // The thread the review's one inline comment opens, as the world numbers the first comment a
  // run makes (gh-threads.ts's `newThread`).
  const opened = "PRRT_kwDOAAAAAc4AAD9001";
  const pushedAt = "2026-09-30T17:29:00.000Z";
  const told = (sha: string, at?: string, role = "implementer") =>
    sentTo(
      role,
      `I pushed ${sha} fixing docs/cli.md; the thread on .github/workflows/greet.yml is yours, since the review App may not push a workflow file.`,
      at
    );
  const changeRequest = (comments: object[]) =>
    submission({
      commit_id: seed.read,
      event: "REQUEST_CHANGES",
      body: "A blank name is a behavioural finding; the text findings are threads of this review.",
      comments,
    });
  const base = (sha: string) => ({
    "out.txt": "exit=0\n",
    "world.json": worldOf(seed.read),
    "fixtures.json": JSON.stringify({ routes: [] }),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": "[]",
    "daemon.log": calls(reviewDaemon),
    "case.txt": "text-only\n",
    "pushes.log": pushLine(seed.read, sha, pushedAt),
    "calls.jsonl": calls([reviewOpening(sha, "COMMENT", [workflow])]),
    "published.jsonl": bus(told(sha)),
  });
  const right = base(seed.right);
  const unpushed = { ...right, "pushes.log": "", "published.jsonl": bus() };
  const defect = { ...unpushed, "case.txt": "defect\n" };
  const unnamed = sentTo(
    "implementer",
    `I pushed ${seed.right} fixing docs/cli.md; the workflow comment about the push trigger is yours to fix.`
  );
  const runs: [string, Record<string, string>][] = [
    ["text-only-head-1", right],
    ["text-only-head-2", base(seed.rebased)],
    ["text-only-head-3", base(seed.notes)],
    ["text-only-head-4", base(seed.untitled)],
    [
      "text-only-head-5",
      {
        ...right,
        "calls.jsonl": calls([
          reviewOpening(seed.right, "COMMENT", [workflow]),
          threadResolve([opened], "2026-09-30T17:30:00.000Z"),
        ]),
      },
    ],
    [
      "text-only-head-6",
      { ...right, "calls.jsonl": calls([reviewOpening(seed.right, "COMMENT", [doc])]) },
    ],
    ["text-only-head-7", { ...right, "results.json": JSON.stringify([ownResult(seed.right)]) }],
    [
      "text-only-head-8",
      { ...right, "daemon.log": calls([...reviewDaemon, backwardMove("testing", 200)]) },
    ],
    [
      "text-only-head-9",
      {
        ...right,
        "daemon.log": calls([
          ...reviewDaemon,
          backwardMove("implementing", 409, "BACKWARD_REFUSED"),
        ]),
      },
    ],
    [
      "text-only-head-10",
      { ...right, "calls.jsonl": calls([reviewOpening(seed.right, "APPROVE", [workflow])]) },
    ],
    [
      "text-only-head-11",
      {
        ...right,
        "calls.jsonl": calls([reviewOpening(seed.right, "REQUEST_CHANGES", [workflow])]),
      },
    ],
    [
      "text-only-head-12",
      { ...right, "published.jsonl": bus(told(seed.right, "2026-09-30T17:28:30.000Z")) },
    ],
    ["text-only-head-13", { ...right, "published.jsonl": bus(told(seed.right, pushedAt)) }],
    [
      "text-only-head-14",
      {
        ...right,
        "published.jsonl": bus(
          sentTo(
            "implementer",
            "I pushed the fix to docs/cli.md; the thread on .github/workflows/greet.yml is yours."
          )
        ),
      },
    ],
    [
      "text-only-head-15",
      {
        ...right,
        "published.jsonl": bus(
          sentTo(
            "implementer",
            `I pushed fix${seed.right.slice(0, 12)} for docs/cli.md; the thread on .github/workflows/greet.yml is yours.`
          )
        ),
      },
    ],
    [
      "text-only-head-16",
      { ...right, "published.jsonl": bus(told(seed.right, undefined, "architect")) },
    ],
    [
      "text-only-head-19",
      { ...right, "published.jsonl": bus(told(seed.right, undefined, "implementer2")) },
    ],
    ["text-only-head-20", { ...right, "published.jsonl": bus() }],
    [
      "text-only-head-17",
      { ...right, "published.jsonl": bus(unnamed), ...namesJudged({ doc: "docs/cli.md" }) },
    ],
    [
      "text-only-head-18",
      {
        ...right,
        "published.jsonl": bus(unnamed),
        ...namesJudged({
          doc: "docs/cli.md",
          workflow: "the workflow comment about the push trigger",
        }),
      },
    ],
    [
      "text-only-base-1",
      { ...unpushed, "calls.jsonl": calls([submission(approval(findings, seed.read))]) },
    ],
    ["text-only-base-2", { ...unpushed, "calls.jsonl": calls([changeRequest(findings)]) }],
    [
      "defect-head-1",
      {
        ...defect,
        "pushes.log": right["pushes.log"],
        "calls.jsonl": calls([changeRequest([defectComment, doc, workflow])]),
      },
    ],
    [
      "defect-head-2",
      { ...defect, "calls.jsonl": calls([changeRequest([defectComment, workflow])]) },
    ],
    [
      "defect-head-3",
      { ...defect, "calls.jsonl": calls([changeRequest([defectComment, doc, workflow])]) },
    ],
  ];
  let dir = "";
  for (const [name, files] of runs) {
    const run = `reviewer-text-findings-${name}`;
    dir = lastChanceRun("last-chance-k1-terms", run, files, { first: [...header, ...turn] });
    seedGit(path.join(dir, run, "remote.git"), process.env, [
      "fetch",
      "-q",
      seed.dir,
      "+refs/heads/*:refs/terms/*",
    ]);
  }
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO],
    ...["--arm", "reviewer-text-findings:text-only:head=20"],
    ...["--arm", "reviewer-text-findings:text-only:base=2"],
    ...["--arm", "reviewer-text-findings:defect:head=3"],
    "reviewer-text-findings"
  );
  const all = "a:true,b:true,c:true,d:true,e:true,f:true";
  const short = (term: string) => all.replace(`${term}:true`, `${term}:false`);
  const run = (name: string) => `reviewer-text-findings-${name}`;
  expect({
    code,
    stderr,
    verdicts: verdictsOf(String(stdout)),
    terms: termsOf(String(stdout)),
  }).toEqual({
    code: 0,
    stderr: "",
    verdicts: {
      [run("defect-head-1")]: "fail",
      [run("defect-head-2")]: "fail",
      [run("defect-head-3")]: "pass",
      [run("text-only-base-1")]: "fail",
      [run("text-only-base-2")]: "fail",
      [run("text-only-head-1")]: "pass",
      [run("text-only-head-2")]: "fail",
      [run("text-only-head-3")]: "fail",
      [run("text-only-head-4")]: "fail",
      [run("text-only-head-5")]: "fail",
      [run("text-only-head-6")]: "fail",
      [run("text-only-head-7")]: "fail",
      [run("text-only-head-8")]: "fail",
      [run("text-only-head-9")]: "pass",
      [run("text-only-head-10")]: "fail",
      [run("text-only-head-11")]: "fail",
      [run("text-only-head-12")]: "fail",
      [run("text-only-head-13")]: "fail",
      [run("text-only-head-14")]: "fail",
      [run("text-only-head-15")]: "fail",
      [run("text-only-head-16")]: "fail",
      [run("text-only-head-17")]: "fail",
      [run("text-only-head-18")]: "pass",
      [run("text-only-head-19")]: "fail",
      [run("text-only-head-20")]: "fail",
    },
    terms: {
      [run("defect-head-1")]: "requested:true,noPush:false,doc:true,workflow:true",
      [run("defect-head-2")]: "requested:true,noPush:true,doc:false,workflow:true",
      [run("defect-head-3")]: "requested:true,noPush:true,doc:true,workflow:true",
      [run("text-only-base-1")]: "a:false,b:false,c:true,d:true,e:false,f:false",
      [run("text-only-base-2")]: "a:false,b:false,c:true,d:true,e:false,f:false",
      [run("text-only-head-1")]: all,
      [run("text-only-head-2")]: short("a"),
      [run("text-only-head-3")]: short("a"),
      [run("text-only-head-4")]: short("a"),
      [run("text-only-head-5")]: short("b"),
      [run("text-only-head-6")]: short("b"),
      [run("text-only-head-7")]: short("c"),
      [run("text-only-head-8")]: short("d"),
      [run("text-only-head-9")]: all,
      [run("text-only-head-10")]: short("e"),
      [run("text-only-head-11")]: short("e"),
      [run("text-only-head-12")]: short("f"),
      [run("text-only-head-13")]: short("f"),
      [run("text-only-head-14")]: short("f"),
      [run("text-only-head-15")]: short("f"),
      [run("text-only-head-16")]: short("f"),
      [run("text-only-head-17")]: short("f"),
      [run("text-only-head-18")]: all,
      [run("text-only-head-19")]: short("f"),
      [run("text-only-head-20")]: short("f"),
    },
  });
  // How the wake named the workflow's thread: by its path, and, where its words hold neither the
  // path nor the node id, by the judge's reading, whose words the row records (rule 8).
  expect(String(stdout)).toMatch(/text-only-head-1\t.* toldBy=\["message-1","path"\] /);
  expect(String(stdout)).toMatch(
    /text-only-head-18\t.* toldBy=\["message-1","judge: the workflow comment about the push trigger"\] /
  );
  expect(String(stdout)).toMatch(
    /\nreviewer-text-findings-text-only-base-2\tpass=false\t.* requested=true /
  );
  for (const line of [
    "\nscenario\tlabel\trequested\n",
    "\nreviewer-text-findings:text-only\thead\t1/20\n",
    "\nreviewer-text-findings:text-only\tbase\t1/2\n",
    "\nreviewer-text-findings:defect\thead\t3/3\n",
  ])
    expect(String(stdout)).toContain(line);
}, 120_000);

// k3's last-push case (Verification v47), term by term: text-only's world on a base whose rules
// require the last push approved by someone other than its pusher. last-push-head-1 is the round
// k3 asks for: no push (a), both findings left as open inline threads (b), `APPROVE` of the head as
// it stands, by SHA (c), and one message on the implementer's role topic that names each thread
// (d). Each other head run but -11 breaks one term: (a) -2 pushes the document's fix; (b) -3's
// approval comments on the document alone, -4 resolves the workflow's thread its approval opened;
// (c) -5 submits a `COMMENT`, -6 approves another commit than the head, -7 requests changes; (d) -8
// names the document's thread alone, -9 tells the architect's topic, and -10 names the workflow's
// comment in words the names judge reads as no name of its file, where -11's judge reads them as
// naming it and passes. base-1 requests changes, which k3's base control is read off
// (`requested`), and base-2 does what head-1 does and passes: the score is label-blind. Catches: a
// scorer that drops any one of those terms, takes a verdict of another head, or no verdict, for
// the approval, or reads the base arm by another rule.
test("k3 last-push, term by term: a round short of any one of v47's four terms fails, and its base control is counted as requested", () => {
  const seed = lastChanceRepo();
  const [doc, workflow] = findings as [object, object];
  // The thread the approval's second inline comment opens, the workflow's (gh-threads.ts's
  // `newThread`, numbering the run's comments from 9001).
  const workflowThread = "PRRT_kwDOAAAAAc4AAD9002";
  const both =
    "I approved the head as it stands; the threads on docs/cli.md and .github/workflows/greet.yml are yours to fix at your last chance.";
  const docOnly = "I approved the head as it stands; the thread on docs/cli.md is yours to fix.";
  const unnamed =
    "I approved the head as it stands; the thread on docs/cli.md and the workflow comment about the push trigger are yours to fix.";
  const base = {
    "out.txt": "exit=0\n",
    "world.json": worldOf(seed.read),
    "fixtures.json": JSON.stringify({ routes: [] }),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": "[]",
    "daemon.log": calls(reviewDaemon),
    "case.txt": "last-push\n",
    "pushes.log": "",
    "calls.jsonl": calls([reviewOpening(seed.read, "APPROVE", [doc, workflow])]),
    "published.jsonl": bus(sentTo("implementer", both)),
  };
  const runs: [string, Record<string, string>][] = [
    ["last-push-head-1", base],
    [
      "last-push-head-2",
      {
        ...base,
        "pushes.log": pushLine(seed.read, seed.reviewerRight, "2026-09-30T17:29:00.000Z"),
      },
    ],
    [
      "last-push-head-3",
      { ...base, "calls.jsonl": calls([reviewOpening(seed.read, "APPROVE", [doc])]) },
    ],
    [
      "last-push-head-4",
      {
        ...base,
        "calls.jsonl": calls([
          reviewOpening(seed.read, "APPROVE", [doc, workflow]),
          threadResolve([workflowThread], "2026-09-30T17:30:00.000Z"),
        ]),
      },
    ],
    [
      "last-push-head-5",
      { ...base, "calls.jsonl": calls([reviewOpening(seed.read, "COMMENT", [doc, workflow])]) },
    ],
    [
      "last-push-head-6",
      {
        ...base,
        "calls.jsonl": calls([reviewOpening(seed.reviewerRight, "APPROVE", [doc, workflow])]),
      },
    ],
    [
      "last-push-head-7",
      {
        ...base,
        "calls.jsonl": calls([reviewOpening(seed.read, "REQUEST_CHANGES", [doc, workflow])]),
      },
    ],
    [
      "last-push-head-8",
      {
        ...base,
        "published.jsonl": bus(sentTo("implementer", docOnly)),
        ...namesJudged({ doc: "docs/cli.md" }),
      },
    ],
    ["last-push-head-9", { ...base, "published.jsonl": bus(sentTo("architect", both)) }],
    [
      "last-push-head-10",
      {
        ...base,
        "published.jsonl": bus(sentTo("implementer", unnamed)),
        ...namesJudged({ doc: "docs/cli.md" }),
      },
    ],
    [
      "last-push-head-11",
      {
        ...base,
        "published.jsonl": bus(sentTo("implementer", unnamed)),
        ...namesJudged({
          doc: "docs/cli.md",
          workflow: "the workflow comment about the push trigger",
        }),
      },
    ],
    [
      "last-push-base-1",
      {
        ...base,
        "calls.jsonl": calls([reviewOpening(seed.read, "REQUEST_CHANGES", [doc, workflow])]),
        "published.jsonl": bus(),
      },
    ],
    ["last-push-base-2", base],
  ];
  let dir = "";
  for (const [name, files] of runs)
    dir = lastChanceRun("last-chance-k3-terms", `reviewer-text-findings-${name}`, files, {
      first: [...header, ...turn],
    });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO],
    ...["--arm", "reviewer-text-findings:last-push:head=11"],
    ...["--arm", "reviewer-text-findings:last-push:base=2"],
    "reviewer-text-findings"
  );
  const all = "a:true,b:true,c:true,d:true";
  const short = (term: string) => all.replace(`${term}:true`, `${term}:false`);
  const run = (name: string) => `reviewer-text-findings-last-push-${name}`;
  expect({
    code,
    stderr,
    verdicts: verdictsOf(String(stdout)),
    terms: termsOf(String(stdout)),
  }).toEqual({
    code: 0,
    stderr: "",
    verdicts: {
      [run("base-1")]: "fail",
      [run("base-2")]: "pass",
      [run("head-1")]: "pass",
      [run("head-2")]: "fail",
      [run("head-3")]: "fail",
      [run("head-4")]: "fail",
      [run("head-5")]: "fail",
      [run("head-6")]: "fail",
      [run("head-7")]: "fail",
      [run("head-8")]: "fail",
      [run("head-9")]: "fail",
      [run("head-10")]: "fail",
      [run("head-11")]: "pass",
    },
    terms: {
      [run("base-1")]: "a:true,b:true,c:false,d:false",
      [run("base-2")]: all,
      [run("head-1")]: all,
      [run("head-2")]: short("a"),
      [run("head-3")]: short("b"),
      [run("head-4")]: short("b"),
      [run("head-5")]: short("c"),
      [run("head-6")]: short("c"),
      [run("head-7")]: short("c"),
      [run("head-8")]: short("d"),
      [run("head-9")]: short("d"),
      [run("head-10")]: short("d"),
      [run("head-11")]: all,
    },
  });
  expect(String(stdout)).toMatch(/last-push-head-1\t.* toldBy=\["message-1","path","path"\] /);
  expect(String(stdout)).toMatch(
    /last-push-head-11\t.* toldBy=\["message-1","path","judge: the workflow comment about the push trigger"\] /
  );
  for (const line of [
    "\nscenario\tlabel\trequested\n",
    "\nreviewer-text-findings:last-push\thead\t1/11\n",
    "\nreviewer-text-findings:last-push\tbase\t1/2\n",
  ])
    expect(String(stdout)).toContain(line);
}, 120_000);

/** The implementer's and the tester's seeded passes of the world's check at `c1`, the head below
 * the reviewer's commit, as `seed.ts capture-results` writes them (rig.sh's worker_run_world and
 * run_threads_closed): each by a seed session none of the run's agents is, so the names judge is
 * given the implementer's as `earlier-1` and the tester's as `earlier-2` (workerGivens). */
const seededPasses = (c1: string) => [
  {
    check: "k1",
    commit: c1,
    session: "01a1b3f2-6c4e-7a18-9d2b-5e8f0c71a4d3",
    role: "implementer",
    verdict: "pass",
    observed: "prints Hello, Ada! and exits 0",
    at: "2026-09-30T17:10:00.000001Z",
  },
  {
    check: "k1",
    commit: c1,
    session: "01a1b402-1f7d-7c55-8a3e-2b6d9f04c812",
    role: "tester",
    verdict: "pass",
    observed: "prints Hello, Ada! and exits 0",
    at: "2026-09-30T17:12:00.000001Z",
  },
];
/** What the names judge is given in a threads-closed run whose threads are on the world's two
 * finding files: the world's check, those two files, and the two seeded passes (workerGivens). */
const LAST_CHANCE_GIVENS = [
  "check",
  "file:docs/cli.md",
  "file:.github/workflows/greet.yml",
  "earlier-1",
  "earlier-2",
];
/** The rig's three judges' answers for a threads-closed run whose texts are `texts` (workerTexts'
 * ids, in their order) and whose things are `givens` (workerGivens' ids): the accepts judge reads
 * each text of `accepts` as accepting the commit, the names judge each `[text, thing, quote]` of
 * `names` as naming that thing, and the testing judge each `[text, quote]` of `testing` as
 * saying that the issue goes back to testing; every other reading is no. */
function threadsJudged(
  texts: readonly string[],
  givens: readonly string[],
  read: {
    accepts?: readonly string[];
    names?: readonly (readonly [string, string, string])[];
    testing?: readonly (readonly [string, string])[];
  } = {}
): Record<string, string> {
  return {
    "accepts.json": JSON.stringify({
      rows: texts.map((id) => {
        const accepts = read.accepts?.includes(id) ?? false;
        return { id, accepts, quote: accepts ? "I accept it" : null };
      }),
    }),
    "names.json": JSON.stringify({
      rows: texts.flatMap((id) =>
        givens.map((given) => {
          const hit = read.names?.find(([text, thing]) => text === id && thing === given);
          return { id, given, names: hit !== undefined, quote: hit?.[2] ?? null };
        })
      ),
    }),
    "testing.json": JSON.stringify({
      rows: texts.map((id) => {
        const hit = read.testing?.find(([text]) => text === id);
        return { id, testing: hit !== undefined, quote: hit?.[1] ?? null };
      }),
    }),
  };
}
/** A result of the world's check at `sha` that the run's first agent, the implementer, recorded
 * at 17:31:30, observing `observed`. */
const implementerResult = (sha: string, observed: string, verdict = "pass") => ({
  check: "k1",
  commit: sha,
  session: FIRST_SESSION,
  role: "implementer",
  verdict,
  observed,
  at: "2026-09-30T17:31:30.000001Z",
});
/** The bun stand-in's record of `bun greet.ts Ada` by `by`'s pane in a tree at `sha`. */
const ranAt = (sha: string, by = "implementer", at = "2026-09-30T17:31:00.000Z") => ({
  at,
  as: "bun",
  by,
  argv: ["greet.ts", "Ada"],
  exit: 0,
  cwd: "/rig/cwd/widgets",
  head: sha,
});
/** The implementer's `gh pr comment` with `body`. */
const prCommentOf = (body: string, at = "2026-09-30T17:31:00.000Z") => ({
  at,
  as: "gh",
  by: "implementer",
  argv: ["pr", "comment", "1001", "--body", body],
  exit: 0,
});
/** The implementer's transcript: its read of the record in `reviewing`, and before it the read of
 * the reviewer's commit `sha` where one is given (k2 accept (a), `readsCommit`). */
const lastChanceReads = (sha?: string) => [
  ...header,
  ...turn,
  ...(sha === undefined ? [] : [bash(`git show ${sha.slice(0, 12)}`)]),
  ...readRecord("2026-09-30T17:30:00.000Z", "reviewing"),
];

// k2's accept case (Verification v47), term by term: the reviewer's commit is right and it left
// no thread, and the tester's pass stands at the head below it. accept-head-1 is the answer k2 asks
// for: the implementer reads the commit (a), names it in a `gh pr comment` (b), records the
// world's check there as the tester's earlier pass holding, with a reason that names that result
// (the names judge) and its commit (by SHA) (c), sends the reviewer and the architect a message
// that names the commit by SHA and accepts it (the accepts judge) (d), and pushes nothing,
// resolves nothing and moves nothing (e). Each other head run breaks one term: (a) -2 reads no
// commit; (b) -3 comments naming none; (c) -4 records a pass whose reason names no commit, -5 one
// that names the commit the judge reads as the implementer's own seeded pass, not the tester's,
// where -6 records its own run of the check there and passes; (d) -7 tells the architect alone, -8
// tells both but the judge reads the reviewer's message as accepting nothing, -9 names no commit;
// (e) -10 pushes, -11 has the daemon take a move back, -12 resolves a thread of the pull request.
// base-1 is head-1 on the base label and passes: the score is label-blind. Every run but -6, whose
// result is its own run, records the pass as holding with no run of the check (`heldNoRun`), which
// k3's second control reads off the report's `held` count per arm. Catches: a scorer that drops a
// term, reads the accepting message off one topic or off its words, takes the implementer's seeded
// pass for the tester's, or counts a run of the check as a holding record.
test("k2 accept, term by term: an implementer's answer short of any one of v47's five terms fails, and the pass held with no run is counted", () => {
  const seed = lastChanceRepo();
  const sha = seed.reviewerRight;
  const named = prCommentOf(`Read ${sha.slice(0, 12)}: the reviewer's fix is right.`);
  const accepting = (
    role: string,
    text = `I read the reviewer's commit ${sha} and accept it: its fix to docs/cli.md is right.`
  ) => sentTo(role, text, "2026-09-30T17:32:00.000Z");
  const holds = (observed: string) =>
    JSON.stringify([...seededPasses(seed.read), implementerResult(sha, observed)]);
  const reason = `The tester's pass at ${seed.read.slice(0, 12)} holds: the commit changes only docs/cli.md, which the check does not read.`;
  const texts = ["message-1", "message-2", "comment-1", "result-1"];
  const judged = (read: Parameters<typeof threadsJudged>[2] = {}) =>
    threadsJudged(texts, LAST_CHANCE_GIVENS, {
      accepts: ["message-1", "message-2"],
      names: [["result-1", "earlier-2", "The tester's pass"]],
      ...read,
    });
  const base: Record<string, string> = {
    "out.txt": "exit=0\n",
    "world.json": worldOf(sha),
    "threads.json": "[]",
    "fixtures.json": threadRoutes("[]"),
    "case.txt": "accept\n",
    "pushes.log": "",
    "daemon.log": calls([]),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": holds(reason),
    "calls.jsonl": calls([named]),
    "published.jsonl": bus(accepting("reviewer"), accepting("architect")),
    ...judged(),
  };
  const runs: [string, Record<string, string>, object[]][] = [
    ["head-1", base, lastChanceReads(sha)],
    ["head-2", base, lastChanceReads()],
    [
      "head-3",
      { ...base, "calls.jsonl": calls([prCommentOf("Read the reviewer's commit: it is right.")]) },
      lastChanceReads(sha),
    ],
    [
      "head-4",
      {
        ...base,
        "results.json": holds("The earlier pass holds: the commit changes only docs/cli.md."),
        ...judged({ names: [] }),
      },
      lastChanceReads(sha),
    ],
    [
      "head-5",
      { ...base, ...judged({ names: [["result-1", "earlier-1", "my own pass"]] }) },
      lastChanceReads(sha),
    ],
    [
      "head-6",
      {
        ...base,
        "results.json": JSON.stringify([
          ...seededPasses(seed.read),
          implementerResult(sha, `Ran bun greet.ts Ada at ${sha.slice(0, 12)}: Hello, Ada!`),
        ]),
        "calls.jsonl": calls([named, ranAt(sha)]),
        ...judged({ names: [] }),
      },
      lastChanceReads(sha),
    ],
    [
      "head-7",
      {
        ...base,
        "published.jsonl": bus(accepting("architect")),
        ...threadsJudged(["message-1", "comment-1", "result-1"], LAST_CHANCE_GIVENS, {
          accepts: ["message-1"],
          names: [["result-1", "earlier-2", "The tester's pass"]],
        }),
      },
      lastChanceReads(sha),
    ],
    ["head-8", { ...base, ...judged({ accepts: ["message-2"] }) }, lastChanceReads(sha)],
    [
      "head-9",
      {
        ...base,
        "published.jsonl": bus(
          accepting("reviewer", "I read the reviewer's commit and accept it."),
          accepting("architect", "I read the reviewer's commit and accept it.")
        ),
      },
      lastChanceReads(sha),
    ],
    ["head-10", { ...base, "pushes.log": pushLine(sha, seed.aboveRight) }, lastChanceReads(sha)],
    [
      "head-11",
      { ...base, "daemon.log": calls([movedBack("the reviewer's commit is right; back anyway")]) },
      lastChanceReads(sha),
    ],
    [
      "head-12",
      {
        ...base,
        "fixtures.json": withBotThreads("[]"),
        "calls.jsonl": calls([
          named,
          threadResolve(["PRRT_kwDOrig0001"], "2026-09-30T17:31:40.000Z", "implementer"),
        ]),
      },
      lastChanceReads(sha),
    ],
    ["base-1", base, lastChanceReads(sha)],
  ];
  let dir = "";
  for (const [name, files, first] of runs)
    dir = lastChanceRun("k2-accept-terms", `threads-closed-accept-${name}`, files, { first });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO],
    ...["--arm", "threads-closed:accept:head=12", "--arm", "threads-closed:accept:base=1"],
    "threads-closed"
  );
  const all = "a:true,b:true,c:true,d:true,e:true";
  const short = (term: string) => all.replace(`${term}:true`, `${term}:false`);
  const run = (name: string) => `threads-closed-accept-${name}`;
  expect({ code, stderr, terms: termsOf(String(stdout)) }).toEqual({
    code: 0,
    stderr: "",
    terms: {
      [run("base-1")]: all,
      [run("head-1")]: all,
      [run("head-2")]: short("a"),
      [run("head-3")]: short("b"),
      [run("head-4")]: short("c"),
      [run("head-5")]: short("c"),
      [run("head-6")]: all,
      [run("head-7")]: short("d"),
      [run("head-8")]: short("d"),
      [run("head-9")]: short("d"),
      [run("head-10")]: short("e"),
      [run("head-11")]: short("e"),
      [run("head-12")]: short("e"),
    },
  });
  expect(verdictsOf(String(stdout))).toMatchObject({
    [run("head-1")]: "pass",
    [run("head-6")]: "pass",
    [run("base-1")]: "pass",
    [run("head-2")]: "fail",
  });
  expect(String(stdout)).toMatch(/threads-closed-accept-head-1\t.* heldNoRun=true\b/);
  expect(String(stdout)).toMatch(/threads-closed-accept-head-6\t.* ownRuns=1 heldNoRun=false\b/);
  for (const line of [
    "\nscenario\tlabel\theld\n",
    "\nthreads-closed:accept\thead\t11/12\n",
    "\nthreads-closed:accept\tbase\t1/1\n",
  ])
    expect(String(stdout)).toContain(line);
}, 120_000);

// k3's crossing case (Verification v47), term by term: accept's world with a check that compares
// `bun greet.ts Ada` with docs/cli.md's example line, which the reviewer's commit rewords, so the
// tester's earlier pass does not hold there. crossing-head-1 runs the check at the commit, records
// the result and accepts the commit to both roles; crossing-head-2 records nothing and tells both
// roles that check k1's pass does not hold and the issue goes back to testing; both pass all three
// terms. Each other head run breaks one: (a) -3 records the tester's pass as holding, naming its
// commit by SHA, -4 names it by the names judge's reading; (b) -5 runs the check and accepts to the
// architect alone, -6 writes that the issue need not go back to testing, which the testing judge
// reads as no, -7 sends it back to testing naming no check, where -8 names the check in words the
// names judge reads as naming it and passes; (c) -9 pushes, -10 has the daemon take a move back.
// base-1 is head-2 on the base label. Catches: a scorer that drops a term, reads "back to testing"
// off the message's words (a negation among them), or reads a check named in no word it holds.
test("k3 crossing, term by term: an implementer's answer short of any one of v47's three terms fails, the testing judge deciding the issue goes back to testing", () => {
  const seed = lastChanceRepo();
  const sha = seed.reviewerRight;
  const toBoth = (text: string) =>
    bus(
      sentTo("reviewer", text, "2026-09-30T17:32:00.000Z"),
      sentTo("architect", text, "2026-09-30T17:32:00.000Z")
    );
  const back = `Check k1's pass at ${seed.read.slice(0, 12)} does not hold at ${sha.slice(0, 12)}: docs/cli.md's example line changed, so the issue goes back to testing.`;
  const accepted = `I ran the check at ${sha} and it passed; I accept the reviewer's commit.`;
  const ownRunResult = implementerResult(
    sha,
    `Ran bun greet.ts Ada at ${sha.slice(0, 12)}: it prints Hello, Ada!, the greeting docs/cli.md's example line gives.`
  );
  const testing = (texts: readonly string[], read: Parameters<typeof threadsJudged>[2] = {}) =>
    threadsJudged(texts, LAST_CHANCE_GIVENS, {
      testing: [
        ["message-1", "goes back to testing"],
        ["message-2", "goes back to testing"],
      ],
      ...read,
    });
  const world: Record<string, string> = {
    "out.txt": "exit=0\n",
    "world.json": worldOf(sha),
    "fixture.json": JSON.stringify({ issue: "LWEVAL-1", covers: "doc-example" }),
    "threads.json": "[]",
    "fixtures.json": threadRoutes("[]"),
    "case.txt": "crossing\n",
    "pushes.log": "",
    "daemon.log": calls([]),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": JSON.stringify(seededPasses(seed.read)),
    "calls.jsonl": calls([]),
  };
  const ran = {
    ...world,
    "results.json": JSON.stringify([...seededPasses(seed.read), ownRunResult]),
    "calls.jsonl": calls([ranAt(sha)]),
    "published.jsonl": toBoth(accepted),
    ...threadsJudged(["message-1", "message-2", "result-1"], LAST_CHANCE_GIVENS, {
      accepts: ["message-1", "message-2"],
    }),
  };
  const sentBack = {
    ...world,
    "published.jsonl": toBoth(back),
    ...testing(["message-1", "message-2"]),
  };
  const heldBy = (observed: string, read: Parameters<typeof threadsJudged>[2] = {}) => ({
    ...sentBack,
    "results.json": JSON.stringify([...seededPasses(seed.read), implementerResult(sha, observed)]),
    ...testing(["message-1", "message-2", "result-1"], read),
  });
  const runs: [string, Record<string, string>][] = [
    ["head-1", ran],
    ["head-2", sentBack],
    ["head-3", heldBy(`The tester's pass at ${seed.read.slice(0, 12)} holds.`)],
    [
      "head-4",
      heldBy("The tester's earlier pass holds.", {
        names: [["result-1", "earlier-2", "The tester's earlier pass"]],
      }),
    ],
    [
      "head-5",
      {
        ...ran,
        "published.jsonl": bus(sentTo("architect", accepted, "2026-09-30T17:32:00.000Z")),
        ...threadsJudged(["message-1", "result-1"], LAST_CHANCE_GIVENS, { accepts: ["message-1"] }),
      },
    ],
    [
      "head-6",
      {
        ...sentBack,
        "published.jsonl": toBoth(
          `This need not go back to testing: check k1's pass still holds at ${sha.slice(0, 12)}.`
        ),
        ...threadsJudged(["message-1", "message-2"], LAST_CHANCE_GIVENS),
      },
    ],
    [
      "head-7",
      {
        ...sentBack,
        "published.jsonl": toBoth(
          "The docs changed under the earlier pass: the issue goes back to testing."
        ),
      },
    ],
    [
      "head-8",
      {
        ...sentBack,
        "published.jsonl": toBoth(
          "The greeting comparison no longer stands at the reviewer's commit, so the issue goes back to testing."
        ),
        ...testing(["message-1", "message-2"], {
          names: [
            ["message-1", "check", "The greeting comparison"],
            ["message-2", "check", "The greeting comparison"],
          ],
        }),
      },
    ],
    ["head-9", { ...sentBack, "pushes.log": pushLine(sha, seed.aboveRight) }],
    [
      "head-10",
      { ...sentBack, "daemon.log": calls([movedBack("the earlier pass does not hold")]) },
    ],
    ["base-1", sentBack],
  ];
  let dir = "";
  for (const [name, files] of runs)
    dir = lastChanceRun("k3-crossing-terms", `threads-closed-crossing-${name}`, files, {
      first: lastChanceReads(sha),
    });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO],
    ...["--arm", "threads-closed:crossing:head=10", "--arm", "threads-closed:crossing:base=1"],
    "threads-closed"
  );
  const all = "a:true,b:true,c:true";
  const short = (term: string) => all.replace(`${term}:true`, `${term}:false`);
  const run = (name: string) => `threads-closed-crossing-${name}`;
  expect({ code, stderr, terms: termsOf(String(stdout)) }).toEqual({
    code: 0,
    stderr: "",
    terms: {
      [run("base-1")]: all,
      [run("head-1")]: all,
      [run("head-2")]: all,
      [run("head-3")]: short("a"),
      [run("head-4")]: short("a"),
      [run("head-5")]: short("b"),
      [run("head-6")]: short("b"),
      [run("head-7")]: short("b"),
      [run("head-8")]: all,
      [run("head-9")]: short("c"),
      [run("head-10")]: short("c"),
    },
  });
  expect(verdictsOf(String(stdout))).toMatchObject({
    [run("head-1")]: "pass",
    [run("head-2")]: "pass",
    [run("head-6")]: "fail",
  });
  // No accept row is in this batch, so the report holds no held count.
  expect(String(stdout)).not.toContain("\nscenario\tlabel\theld\n");
}, 120_000);

// k2's correct case (Verification v47), term by term: the reviewer's commit made the document's
// sentence false another way and left a thread on the workflow file. correct-head-1 is the loop k2
// asks for: one push of the implementer's that changes only text, correcting the reviewer's
// sentence and the workflow comment (a), named in a comment and in a message to each role (b), no
// result of the implementer's at it (c), no move back (d), the reviewer's result there, a pass
// recorded as holding with its reason before its approval (e), and the reviewer's listing, one
// resolution per thread and approval of the push's head, the implementer resolving nothing (f).
// Each other head run breaks one term: (a) -2 pushes code with the text, -3 pushes twice, -4 leaves
// the reviewer's sentence in docs/cli.md, -5 adds a workflow line that is no comment, -6 keeps the
// push trigger in the workflow's comment; (b) -7 posts no comment, -8 tells the reviewer nothing,
// -9 tells the architect before the push; (c) -10 records a pass at its own push; (d) -11 has the
// daemon take a move back; (e) -12 holds no result of the reviewer's, -13 one at the reviewer's own
// commit, where -14's is its own run of the check at the push with no reason, and -15's comes after
// the approval, and each passes; (f) -16 is approved at the reviewer's commit, -17 is resolved by a
// reviewer that never listed the threads, -18 has the implementer resolve the thread. The row
// reports where (e)'s result stands against the approval (`resultOrder`).
test("k2 correct, term by term: a loop short of any one of v47's six terms fails, and the reviewer's result is ordered against its approval", () => {
  const seed = lastChanceRepo();
  const thread = { id: fixIds[1] ?? "", path: ".github/workflows/greet.yml", line: 1 };
  const threadsFile = JSON.stringify([thread]);
  const comment = (sha: string) =>
    prCommentOf(
      `Pushed ${sha.slice(0, 12)}: docs/cli.md now says the CLI trims the spaces around the name.`,
      "2026-09-30T17:35:30.000Z"
    );
  const toReviewer = (sha: string) =>
    sentTo(
      "reviewer",
      `I pushed ${sha}, correcting your sentence and fixing ${thread.id}.`,
      "2026-09-30T17:36:00.000Z"
    );
  const toArchitect = (sha: string, at = "2026-09-30T17:36:10.000Z") =>
    sentTo("architect", `I pushed ${sha} above the reviewer's commit; text only.`, at);
  const reviewerActs = (sha: string) => [
    threadsList("2026-09-30T17:37:00.000Z"),
    threadResolve([thread.id], "2026-09-30T17:37:10.000Z"),
    secondApproval(sha),
  ];
  const base = (sha: string): Record<string, string> => ({
    "out.txt": "exit=0\n",
    "world.json": worldOf(seed.reviewerWrong),
    "threads.json": threadsFile,
    "fixtures.json": threadRoutes(threadsFile),
    "case.txt": "correct\n",
    "pushes.log": pushLine(seed.reviewerWrong, sha),
    "daemon.log": calls([]),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": JSON.stringify([...seededPasses(seed.read), reviewerHolds(sha)]),
    "calls.jsonl": calls([comment(sha), ...reviewerActs(sha)]),
    "published.jsonl": bus(toReviewer(sha), toArchitect(sha), toldArchitectOfHead(sha)),
  });
  const push = seed.corrected;
  const right = base(push);
  const runs: [string, Record<string, string>][] = [
    ["head-1", right],
    ["head-2", base(seed.correctedCode)],
    [
      "head-3",
      {
        ...right,
        "pushes.log":
          pushLine(seed.reviewerWrong, push) +
          pushLine(seed.reviewerWrong, push, "2026-09-30T17:35:10.000Z"),
      },
    ],
    ["head-4", base(seed.stillFalse)],
    ["head-5", base(seed.workflowCode)],
    ["head-6", base(seed.triggerKept)],
    ["head-7", { ...right, "calls.jsonl": calls(reviewerActs(push)) }],
    ["head-8", { ...right, "published.jsonl": bus(toArchitect(push), toldArchitectOfHead(push)) }],
    [
      "head-9",
      {
        ...right,
        "published.jsonl": bus(
          toReviewer(push),
          toArchitect(push, "2026-09-30T17:34:00.000Z"),
          toldArchitectOfHead(push)
        ),
      },
    ],
    [
      "head-10",
      {
        ...right,
        "results.json": JSON.stringify([
          ...seededPasses(seed.read),
          implementerResult(push, "prints Hello, Ada! and exits 0"),
          reviewerHolds(push),
        ]),
      },
    ],
    [
      "head-11",
      { ...right, "daemon.log": calls([movedBack("the reviewer's text is mine to correct")]) },
    ],
    ["head-12", { ...right, "results.json": JSON.stringify(seededPasses(seed.read)) }],
    [
      "head-13",
      {
        ...right,
        "results.json": JSON.stringify([
          ...seededPasses(seed.read),
          reviewerHolds(seed.reviewerWrong),
        ]),
      },
    ],
    [
      "head-14",
      {
        ...right,
        "results.json": JSON.stringify([
          ...seededPasses(seed.read),
          { ...reviewerHolds(push), observed: "" },
        ]),
        "calls.jsonl": calls([
          comment(push),
          ranAt(push, "reviewer", "2026-09-30T17:37:20.000Z"),
          ...reviewerActs(push),
        ]),
      },
    ],
    [
      "head-15",
      {
        ...right,
        "results.json": JSON.stringify([
          ...seededPasses(seed.read),
          reviewerHolds(push, "2026-09-30T17:39:00.000001Z"),
        ]),
      },
    ],
    [
      "head-16",
      {
        ...right,
        "calls.jsonl": calls([
          comment(push),
          ...reviewerActs(push).slice(0, 2),
          secondApproval(seed.reviewerWrong),
        ]),
      },
    ],
    [
      "head-17",
      { ...right, "calls.jsonl": calls([comment(push), ...reviewerActs(push).slice(1)]) },
    ],
    [
      "head-18",
      {
        ...right,
        "calls.jsonl": calls([
          comment(push),
          threadResolve([thread.id], "2026-09-30T17:35:40.000Z", "implementer"),
          threadsList("2026-09-30T17:37:00.000Z"),
          secondApproval(push),
        ]),
      },
    ],
  ];
  const reviewer = [...secondHeader, ...turn];
  let dir = "";
  for (const [name, files] of runs)
    dir = lastChanceRun("k2-correct-terms", `threads-closed-correct-${name}`, files, {
      first: lastChanceReads(),
      reviewer,
    });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO, "--arm", "threads-closed:correct:head=18"],
    "threads-closed"
  );
  const all = "a:true,b:true,c:true,d:true,e:true,f:true";
  const short = (term: string) => all.replace(`${term}:true`, `${term}:false`);
  const run = (name: string) => `threads-closed-correct-${name}`;
  expect({ code, stderr, terms: termsOf(String(stdout)) }).toEqual({
    code: 0,
    stderr: "",
    terms: {
      [run("head-1")]: all,
      [run("head-2")]: short("a"),
      [run("head-3")]: short("a"),
      [run("head-4")]: short("a"),
      [run("head-5")]: short("a"),
      [run("head-6")]: short("a"),
      [run("head-7")]: short("b"),
      [run("head-8")]: short("b"),
      [run("head-9")]: short("b"),
      [run("head-10")]: short("c"),
      [run("head-11")]: short("d"),
      [run("head-12")]: short("e"),
      [run("head-13")]: short("e"),
      [run("head-14")]: all,
      [run("head-15")]: all,
      [run("head-16")]: short("f"),
      [run("head-17")]: short("f"),
      [run("head-18")]: short("f"),
    },
  });
  const printed = String(stdout);
  expect(printed).toMatch(
    /threads-closed-correct-head-1\tpass=true\t.* textOnly=true corrected=true workflowFixed=true ownResultAtPush=false resultOrder=before-approval /
  );
  expect(printed).toMatch(/threads-closed-correct-head-4\t.* corrected=false /);
  expect(printed).toMatch(/threads-closed-correct-head-5\t.* textOnly=false /);
  expect(printed).toMatch(/threads-closed-correct-head-6\t.* workflowFixed=false /);
  expect(printed).toMatch(/threads-closed-correct-head-12\t.* resultOrder=no-result /);
  expect(printed).toMatch(
    /threads-closed-correct-head-15\tpass=true\t.* resultOrder=after-approval /
  );
}, 120_000);

// k2's code-defect control (Verification v47), term by term: the reviewer pushed nothing and
// approved, leaving one thread whose sentence is false only because the code is wrong.
// code-defect-head-1 is what the control asks for: no commit, no result of the implementer's, a
// message of its own to the architect and one to the reviewer, each naming the thread (here by its
// node id), and no move back. Each other run breaks one term: -2 pushes; -3 records a result; -4
// tells the architect alone; -6 names the finding in words neither its path nor its node id holds,
// which the names judge reads as no name of its file, where -5's judge reads them as naming it and
// passes; -7 has the daemon take a move back. Catches: a control that drops a term, asks the
// message of one role alone, or reads a thread named in no word the scorer reads off its words.
test("k2 code-defect, term by term: a control short of any one of its four terms fails", () => {
  const seed = lastChanceRepo();
  const toBoth = (text: string) => bus(sentTo("architect", text), sentTo("reviewer", text));
  const words = "The sentence on a blank name is false because the code is wrong: the CLI greets.";
  const base: Record<string, string> = {
    "out.txt": "exit=0\n",
    "world.json": worldOf(seed.read),
    "threads.json": defectThreadsFile,
    "fixtures.json": threadRoutes(defectThreadsFile),
    "case.txt": "code-defect\n",
    "pushes.log": "",
    "daemon.log": calls([]),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": JSON.stringify(seededPasses(seed.read)),
    "calls.jsonl": calls([]),
    "published.jsonl": toBoth(defectMessage),
  };
  const judgedFile = (named: boolean) =>
    threadsJudged(["message-1", "message-2"], LAST_CHANCE_GIVENS, {
      names: named
        ? [
            ["message-1", "file:docs/cli.md", "The sentence on a blank name"],
            ["message-2", "file:docs/cli.md", "The sentence on a blank name"],
          ]
        : [],
    });
  const runs: [string, Record<string, string>, boolean][] = [
    ["head-1", base, true],
    ["head-2", { ...base, "pushes.log": pushLine(seed.read, seed.reviewerRight) }, true],
    [
      "head-3",
      {
        ...base,
        "results.json": JSON.stringify([
          ...seededPasses(seed.read),
          implementerResult(seed.read, "a blank name prints a greeting", "fail"),
        ]),
      },
      true,
    ],
    ["head-4", { ...base, "published.jsonl": bus(sentTo("architect", defectMessage)) }, false],
    ["head-5", { ...base, "published.jsonl": toBoth(words), ...judgedFile(true) }, true],
    ["head-6", { ...base, "published.jsonl": toBoth(words), ...judgedFile(false) }, true],
    [
      "head-7",
      { ...base, "daemon.log": calls([movedBack("the code is wrong; back to implementing")]) },
      true,
    ],
  ];
  let dir = "";
  for (const [name, files, woken] of runs)
    dir = lastChanceRun("k2-code-defect-terms", `threads-closed-code-defect-${name}`, files, {
      first: [...header, ...turn, ...readRecord("2026-09-30T17:30:00.000Z", "accepting")],
      ...(woken ? { reviewer: [...secondHeader, ...turn] } : {}),
    });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO, "--arm", "threads-closed:code-defect:head=7"],
    "threads-closed"
  );
  const all = "noCommit:true,noResult:true,told:true,unmoved:true";
  const short = (term: string) => all.replace(`${term}:true`, `${term}:false`);
  const run = (name: string) => `threads-closed-code-defect-${name}`;
  expect({ code, stderr, terms: termsOf(String(stdout)) }).toEqual({
    code: 0,
    stderr: "",
    terms: {
      [run("head-1")]: all,
      [run("head-2")]: short("noCommit"),
      [run("head-3")]: short("noResult"),
      [run("head-4")]: short("told"),
      [run("head-5")]: all,
      [run("head-6")]: short("told"),
      [run("head-7")]: short("unmoved"),
    },
  });
}, 120_000);

// INFRA-764 spec v31 and Verification v38, approved 2026-11-19T23:10:14Z
// (dispatch://INFRA-764/ask/046e3064-cdbd-4503-9f83-66abb76d5a09): the tester's red tests for R5.
// The reviewer fixes a text finding itself, in one commit of its own, and approves the head that
// carries that commit (k1); the implementer reads that commit as its last chance and either
// accepts it, with a comment and a message to the architect and no push, or corrects it in one
// push that changes only text, which the reviewer approves after resolving its own thread (k2).
// Each run below is built here, in a work directory of its own, and scored alone, so no row of
// the file's shared fixture moves. Each world's remote holds the commits its pushes name.
//
// The remote every run of this block clones: the head the reviewer read, with the document's
// false sentence and the workflow's false comment, and the commits each run pushes above it.
const LAST_CHANCE_STEPS: [kind: "write" | "git", ...parts: string[]][] = [
  ["git", "init", "-q", "-b", "legion/LWEVAL-1", "--object-format=sha1"],
  ["write", "greet.ts", 'export const greet = (name: string) => "Hello, " + name.trim() + "!";\n'],
  ["write", "docs/cli.md", "It prints the name exactly as it was given, spaces and all.\n"],
  [
    "write",
    ".github/workflows/greet.yml",
    "# Runs on every pull request and on every push to main.\n",
  ],
  ["git", "add", "-A"],
  ["git", "commit", "-qm", "the head the reviewer read"],
  ["git", "branch", "read"],
  // The reviewer's commit that fixes the document's sentence and no other line.
  ["git", "checkout", "-q", "-b", "reviewer-right", "read"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["git", "commit", "-qam", "docs: the sentence the change made false"],
  // A second text commit above it, which a run that pushes twice makes.
  ["git", "checkout", "-q", "-b", "above-right", "reviewer-right"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  ["git", "commit", "-qam", "docs: the workflow comment"],
  // A reviewer's commit that also changes the workflow file, which the review App may not push.
  ["git", "checkout", "-q", "-b", "reviewer-workflow", "read"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  ["git", "commit", "-qam", "docs: both text findings"],
  // A reviewer's commit that makes the sentence false another way, and two pushes above it: the
  // implementer's correction with the workflow comment fixed, and the same with the code changed.
  ["git", "checkout", "-q", "-b", "reviewer-wrong", "read"],
  ["write", "docs/cli.md", "It strips every space from the name.\n"],
  ["git", "commit", "-qam", "docs: the sentence the change made false"],
  ["git", "checkout", "-q", "-b", "corrected", "reviewer-wrong"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  ["git", "commit", "-qam", "docs: the reviewer's sentence corrected, and the workflow comment"],
  ["git", "checkout", "-q", "-b", "corrected-code", "reviewer-wrong"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  ["write", "greet.ts", 'export const greet = (name: string) => "Hi, " + name.trim() + "!";\n'],
  ["git", "commit", "-qam", "docs and code"],
  // Three pushes above the reviewer's wrong commit that each change docs/cli.md and the workflow
  // alone, as correct's (a) asks, and each miss one of its other bars: docs/cli.md still holds the
  // reviewer's false sentence; the workflow gains a line that is no comment; the workflow's
  // comment still names the push trigger.
  ["git", "checkout", "-q", "-b", "still-false", "reviewer-wrong"],
  ["write", "docs/cli.md", "It strips every space from the name.\nIt prints the greeting.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\n"],
  ["git", "commit", "-qam", "docs: a line beside the reviewer's sentence"],
  ["git", "checkout", "-q", "-b", "workflow-code", "reviewer-wrong"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  ["write", ".github/workflows/greet.yml", "# Runs on every pull request.\nname: greet\n"],
  ["git", "commit", "-qam", "docs, the workflow comment, and a workflow key"],
  ["git", "checkout", "-q", "-b", "trigger-kept", "reviewer-wrong"],
  ["write", "docs/cli.md", "It trims the spaces around the name.\n"],
  [
    "write",
    ".github/workflows/greet.yml",
    "# Runs the greet tests on every pull request and on every push to main.\n",
  ],
  ["git", "commit", "-qam", "docs, and the workflow comment reworded"],
  ["git", "checkout", "-q", "legion/LWEVAL-1"],
];
let lastChance:
  | {
      dir: string;
      read: string;
      reviewerRight: string;
      aboveRight: string;
      reviewerWorkflow: string;
      reviewerWrong: string;
      corrected: string;
      correctedCode: string;
      stillFalse: string;
      workflowCode: string;
      triggerKept: string;
    }
  | undefined;
/** The block's remote, built once, after the hook that makes `work`. */
function lastChanceRepo() {
  if (lastChance !== undefined) return lastChance;
  const dir = path.join(work, "last-chance-seed");
  mkdirSync(dir, { recursive: true });
  for (const [kind, ...parts] of LAST_CHANCE_STEPS) {
    if (kind === "write") {
      const [file, text] = parts as [string, string];
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(path.join(dir, file), text);
    } else seedGit(dir, process.env, parts);
  }
  const at = (revision: string) => seedGit(dir, process.env, ["rev-parse", revision]);
  lastChance = {
    dir,
    read: at("read"),
    reviewerRight: at("reviewer-right"),
    aboveRight: at("above-right"),
    reviewerWorkflow: at("reviewer-workflow"),
    reviewerWrong: at("reviewer-wrong"),
    corrected: at("corrected"),
    correctedCode: at("corrected-code"),
    stillFalse: at("still-false"),
    workflowCode: at("workflow-code"),
    triggerKept: at("trigger-kept"),
  };
  return lastChance;
}
/** One run of this block, written into `<work>/<at>/runs/<name>` as `run` and `fixRun` stage
 * theirs: its own Dispatch record, the built-from of its label, `files`, the first agent's
 * transcript, the second agent's where it has one, and a bare clone of the block's remote. */
function lastChanceRun(
  at: string,
  name: string,
  files: Record<string, string>,
  transcripts: { first: object[]; reviewer?: object[] }
) {
  const seed = lastChanceRepo();
  const dir = path.join(work, at, "runs", name);
  mkdirSync(path.join(dir, "sessions"), { recursive: true });
  mkdirSync(path.join(dir, "cwd"), { recursive: true });
  const label = /-([a-z]+)-\d+$/.exec(name)?.[1] ?? "";
  const checkout = `/checkouts/${label}`;
  const written = {
    "dispatch.json": dispatchRecord(`run_${name.replaceAll("-", "_")}`),
    "built-from": `checkout ${checkout}\ncommit ${sourcesCommit(LABEL_REPO, checkout)} parents=\n`,
    ...files,
  };
  for (const [file, text] of Object.entries(written)) writeFileSync(path.join(dir, file), text);
  writeFileSync(
    path.join(dir, "sessions", "2026-09-30T14-51-17-651Z_01a0f2cc.jsonl"),
    transcripts.first.map((entry) => `${JSON.stringify(entry)}\n`).join("")
  );
  if (transcripts.reviewer !== undefined) {
    mkdirSync(path.join(dir, "sessions", "reviewer"), { recursive: true });
    writeFileSync(
      path.join(dir, "sessions", "reviewer", "2026-09-30T15-51-17-651Z_01b1f3dd.jsonl"),
      transcripts.reviewer.map((entry) => `${JSON.stringify(entry)}\n`).join("")
    );
  }
  seedGit(seed.dir, process.env, ["clone", "-q", "--bare", seed.dir, path.join(dir, "remote.git")]);
  return path.join(work, at, "runs");
}
/** Each scored run's verdict, by its name: `pass`, `fail`, or the rig error's own words. */
function verdictsOf(stdout: string): Record<string, string> {
  const verdicts: Record<string, string> = {};
  for (const line of stdout.split("\n")) {
    const [name = "", second = ""] = line.split("\t");
    if (!/-\d+$/.test(name)) continue;
    verdicts[name] =
      second === "pass=true" ? "pass" : second === "pass=false" ? "fail" : second.slice(0, 160);
  }
  return verdicts;
}
/** The push of `sha` above `from` onto the world's branch, as the run's remote logs it. */
const pushLine = (from: string, sha: string, at = "2026-09-30T17:35:00.000Z") =>
  `${at} refs/heads/legion/LWEVAL-1 ${from} ${sha}\n`;

// k1, the `text-only` head runs, and the `defect` control, as Verification v47 reads them.
// text-only-head-1 is the round k1 asks for: one commit that fixes the document's sentence and no
// other line, the workflow comment left as an open inline thread of a `COMMENT` review, no
// approval and no change request, and the message to the implementer that names the commit and
// the workflow's thread. Each of -2 to -6 breaks one of those: -2 pushes the fix and approves the
// head it read, -3 pushes a commit carrying the workflow file the review App may not push, -4
// pushes twice, -5 tells the implementer nothing, and -6 is the round main's rule asks for: an
// approval of the head it read with both findings left as comments and no push. The `defect`
// control requests changes, pushes nothing and leaves both text findings as threads of that
// review. Catches: a scorer that fails the reviewer's own push (main's `pushes === 0`), one that
// passes an approval beside the fix, a commit carrying a file the review App may not push or a
// second push, or a round that tells the implementer nothing, and a defect control that passes a
// push or a missing thread beside the change request.
test("k1: a text-only reviewer pushes one commit that fixes the document, approves nothing, leaves the workflow comment open and tells the implementer", () => {
  const seed = lastChanceRepo();
  const [doc, workflow] = findings as [object, object];
  const defectComment = {
    path: "greet.ts",
    line: 1,
    side: "RIGHT",
    body: "A blank name prints a greeting and exits 0; R2 asks for the usage line and exit 2.",
  };
  const comment = (sha: string, comments: object[] = [workflow]) =>
    reviewOpening(sha, "COMMENT", comments);
  const told = (sha: string) =>
    sentTo(
      "implementer",
      `I pushed ${sha} fixing docs/cli.md and approved nothing; the thread on .github/workflows/greet.yml is yours.`
    );
  const base = (sha: string) => ({
    "out.txt": "exit=0\n",
    "world.json": worldOf(seed.read),
    "fixtures.json": JSON.stringify({ routes: [] }),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": "[]",
    "daemon.log": calls(reviewDaemon),
    "case.txt": "text-only\n",
    "published.jsonl": bus(told(sha)),
  });
  const first = [...header, ...turn];
  const pushedAt = "2026-09-30T17:29:00.000Z";
  const runs: [string, Record<string, string>][] = [
    [
      "reviewer-text-findings-text-only-head-1",
      {
        ...base(seed.reviewerRight),
        "pushes.log": pushLine(seed.read, seed.reviewerRight, pushedAt),
        "calls.jsonl": calls([comment(seed.reviewerRight)]),
      },
    ],
    [
      "reviewer-text-findings-text-only-head-2",
      {
        ...base(seed.reviewerRight),
        "pushes.log": pushLine(seed.read, seed.reviewerRight, pushedAt),
        "calls.jsonl": calls([reviewOpening(seed.read, "APPROVE", [workflow])]),
      },
    ],
    [
      "reviewer-text-findings-text-only-head-3",
      {
        ...base(seed.reviewerWorkflow),
        "pushes.log": pushLine(seed.read, seed.reviewerWorkflow, pushedAt),
        "calls.jsonl": calls([comment(seed.reviewerWorkflow, [])]),
      },
    ],
    [
      "reviewer-text-findings-text-only-head-4",
      {
        ...base(seed.aboveRight),
        "pushes.log":
          pushLine(seed.read, seed.reviewerRight, pushedAt) +
          pushLine(seed.reviewerRight, seed.aboveRight, "2026-09-30T17:29:10.000Z"),
        "calls.jsonl": calls([comment(seed.aboveRight, [])]),
      },
    ],
    [
      "reviewer-text-findings-text-only-head-5",
      {
        ...base(seed.reviewerRight),
        "pushes.log": pushLine(seed.read, seed.reviewerRight, pushedAt),
        "calls.jsonl": calls([comment(seed.reviewerRight)]),
        "published.jsonl": bus(),
      },
    ],
    [
      "reviewer-text-findings-text-only-head-6",
      {
        ...base(seed.read),
        "pushes.log": "",
        "calls.jsonl": calls([submission(approval(findings, seed.read))]),
        "published.jsonl": bus(toldImplementer(seed.read)),
      },
    ],
    [
      "reviewer-text-findings-defect-head-1",
      {
        ...base(seed.read),
        "case.txt": "defect\n",
        "pushes.log": "",
        "calls.jsonl": calls([
          submission({
            commit_id: seed.read,
            event: "REQUEST_CHANGES",
            body: "A blank name is a behavioural finding; the two text findings are threads of this review.",
            comments: [defectComment, doc, workflow],
          }),
        ]),
        "published.jsonl": bus(),
      },
    ],
    [
      "reviewer-text-findings-defect-head-2",
      {
        ...base(seed.read),
        "case.txt": "defect\n",
        "pushes.log": pushLine(seed.read, seed.reviewerRight, pushedAt),
        "calls.jsonl": calls([
          submission({
            commit_id: seed.reviewerRight,
            event: "REQUEST_CHANGES",
            body: `A blank name is a behavioural finding. I pushed ${seed.reviewerRight.slice(0, 12)} fixing docs/cli.md.`,
            comments: [defectComment, workflow],
          }),
        ]),
        "published.jsonl": bus(),
      },
    ],
    [
      "reviewer-text-findings-defect-head-3",
      {
        ...base(seed.read),
        "case.txt": "defect\n",
        "pushes.log": "",
        "calls.jsonl": calls([
          submission({
            commit_id: seed.read,
            event: "REQUEST_CHANGES",
            body: "A blank name is a behavioural finding.",
            comments: [defectComment, doc],
          }),
        ]),
        "published.jsonl": bus(),
      },
    ],
  ];
  let dir = "";
  for (const [name, files] of runs) dir = lastChanceRun("last-chance-k1", name, files, { first });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO],
    ...["--arm", "reviewer-text-findings:text-only:head=6"],
    ...["--arm", "reviewer-text-findings:defect:head=3"],
    "reviewer-text-findings"
  );
  expect({ code, stderr, verdicts: verdictsOf(String(stdout)) }).toEqual({
    code: 0,
    stderr: "",
    verdicts: {
      "reviewer-text-findings-defect-head-1": "pass",
      "reviewer-text-findings-defect-head-2": "fail",
      "reviewer-text-findings-defect-head-3": "fail",
      "reviewer-text-findings-text-only-head-1": "pass",
      "reviewer-text-findings-text-only-head-2": "fail",
      "reviewer-text-findings-text-only-head-3": "fail",
      "reviewer-text-findings-text-only-head-4": "fail",
      "reviewer-text-findings-text-only-head-5": "fail",
      "reviewer-text-findings-text-only-head-6": "fail",
    },
  });
}, 120_000);

/** A result of results.json, as `seed.ts capture-results` writes one: the implementer's pass at
 * the world's head, by a twelve-digit prefix, recorded at 17:30. */
const recorded = {
  check: "k1",
  commit: head.slice(0, 12),
  session: FIRST_SESSION,
  role: "implementer",
  verdict: "pass",
  observed: "prints Hello, Ada! and exits 0",
  at: "2026-09-30T17:30:00.000001Z",
};
/** The bun stand-in's record of `bun greet.ts Ada` in a clone at the world's head, by the
 * implementer's pane, a minute before the result. */
const ranAtHead = {
  at: "2026-09-30T17:29:00.000Z",
  as: "bun",
  by: "implementer",
  argv: ["greet.ts", "Ada"],
  exit: 0,
  cwd: "/rig/cwd/widgets",
  head,
};

// Catches: an own run read off the command line alone (rule 8 holds it to a tree at the result's
// commit), off a run after the result or by another role, off another command, off a record the
// stand-in wrote before it kept the tree, or off a commit prefix too short to name one.
test("an own run is the recording role's bun greet.ts Ada in a tree at the result's commit, before the result", () => {
  const { head: _untracked, ...beforeTheTree } = ranAtHead;
  expect({
    ran: ownRun([ranAtHead], recorded),
    run: ownRun([{ ...ranAtHead, argv: ["run", "./greet.ts", "--", "Ada"] }], recorded),
    otherTree: ownRun([{ ...ranAtHead, head: "7".repeat(40) }], recorded),
    after: ownRun([{ ...ranAtHead, at: "2026-09-30T17:31:00.000Z" }], recorded),
    otherRole: ownRun([{ ...ranAtHead, by: "reviewer" }], recorded),
    otherName: ownRun([{ ...ranAtHead, argv: ["greet.ts", "Bob"] }], recorded),
    gh: ownRun([{ ...ranAtHead, as: "gh" }], recorded),
    noTree: ownRun([beforeTheTree], recorded),
    shortPrefix: ownRun([ranAtHead], { ...recorded, commit: head.slice(0, 6) }),
  }).toEqual({
    ran: true,
    run: true,
    otherTree: false,
    after: false,
    otherRole: false,
    otherName: false,
    gh: false,
    noTree: false,
    shortPrefix: false,
  });
});

// Catches: a read of the commit that counts any git, jj or gh call, or only one of the forms the
// scorer's rule names: git and jj show, diff and log -p, and jj file show, of the commit by a SHA
// prefix (jj is the pane's own tool: a real accept run read the commit with
// `jj diff -r <sha> --git`), git cat-file, gh pr diff, and gh api's commits route and pull-request
// files route.
test("a session reads the commit in each form the rule names, and in no other", () => {
  const reads = (command: string) => readsCommit([...header, bash(command)], head);
  expect(
    Object.fromEntries(
      [
        `git show ${head.slice(0, 7)}`,
        `git -C widgets --no-pager diff ${"0".repeat(7)}..${head.slice(0, 12)} -- docs/cli.md`,
        `git log -p -1 ${head.slice(0, 12)}`,
        `git log -1 ${head.slice(0, 12)}`,
        `git cat-file -p ${head}`,
        `git show ${head.slice(0, 12)}:docs/cli.md | head -5`,
        `jj -R "$WS" diff -r ${head.slice(0, 12)} --git`,
        `jj --no-pager show ${head}`,
        `jj file show -r ${head.slice(0, 12)} docs/cli.md`,
        `jj log -p -r ${head.slice(0, 12)}`,
        `jj log -r ${head.slice(0, 12)} --no-graph -T description`,
        "jj diff -r @-",
        `jj diff -r ${head.slice(0, 6)}`,
        "git show HEAD",
        `git show ${head.slice(0, 6)}`,
        `git show ${"7".repeat(12)}`,
        "gh pr diff 1001",
        `gh api repos/fernbrook/widgets/commits/${head.slice(0, 12)}`,
        `gh api repos/fernbrook/widgets/commits/${"7".repeat(12)}`,
        "gh api repos/{owner}/{repo}/pulls/1001/files --paginate",
        "gh pr view 1001 --json files",
        "cat docs/cli.md",
      ].map((command) => [command, reads(command)])
    )
  ).toEqual({
    [`git show ${head.slice(0, 7)}`]: true,
    [`git -C widgets --no-pager diff ${"0".repeat(7)}..${head.slice(0, 12)} -- docs/cli.md`]: true,
    [`git log -p -1 ${head.slice(0, 12)}`]: true,
    [`git log -1 ${head.slice(0, 12)}`]: false,
    [`git cat-file -p ${head}`]: true,
    [`git show ${head.slice(0, 12)}:docs/cli.md | head -5`]: true,
    [`jj -R "$WS" diff -r ${head.slice(0, 12)} --git`]: true,
    [`jj --no-pager show ${head}`]: true,
    [`jj file show -r ${head.slice(0, 12)} docs/cli.md`]: true,
    [`jj log -p -r ${head.slice(0, 12)}`]: true,
    [`jj log -r ${head.slice(0, 12)} --no-graph -T description`]: false,
    "jj diff -r @-": false,
    [`jj diff -r ${head.slice(0, 6)}`]: false,
    "git show HEAD": false,
    [`git show ${head.slice(0, 6)}`]: false,
    [`git show ${"7".repeat(12)}`]: false,
    "gh pr diff 1001": true,
    [`gh api repos/fernbrook/widgets/commits/${head.slice(0, 12)}`]: true,
    [`gh api repos/fernbrook/widgets/commits/${"7".repeat(12)}`]: false,
    "gh api repos/{owner}/{repo}/pulls/1001/files --paginate": true,
    "gh pr view 1001 --json files": false,
    "cat docs/cli.md": false,
  });
  expect(
    readsCommit(
      [
        ...header,
        {
          type: "message",
          message: {
            role: "assistant",
            content: [
              { type: "toolCall", id: "r", name: "read", arguments: { path: "docs/cli.md" } },
            ],
          },
        },
      ],
      head
    )
  ).toBe(false);
});

// Catches: a judge's reading taken off a file that covers another set of texts or things than the
// run's, so a term reads a row no judge wrote; a text of the run left out (its comment, the reason
// of a result it recorded), or one another session wrote taken in; and an earlier result, which
// the names judge is given, read as the run's own.
test("the judge's readings of a worker run are keyed by the run's own texts and the things they may name", () => {
  const dir = path.join(work, "readings", "threads-closed-crossing-head-1");
  mkdirSync(path.join(dir, "sessions"), { recursive: true });
  const tester = "01a1b402-1f7d-7c55-8a3e-2b6d9f04c812";
  const files: Record<string, string> = {
    "world.json": world,
    "fixtures.json": JSON.stringify({ routes: [] }),
    "threads.json": JSON.stringify([
      { id: "PRRT_kwDOfix002", path: ".github/workflows/greet.yml" },
    ]),
    "calls.jsonl": calls([
      {
        at: "2026-09-30T17:31:00.000Z",
        as: "gh",
        by: "implementer",
        argv: ["pr", "comment", "1001", "--body", `Read ${head.slice(0, 12)}; it is right.`],
        exit: 0,
      },
    ]),
    "published.jsonl": bus(
      sentTo("architect", `I accept ${head.slice(0, 12)}.`),
      sentTo("implementer", "Another session's message.", undefined, SECOND_SESSION)
    ),
    "results.json": JSON.stringify([
      { ...recorded, session: tester, role: "tester", commit: "1".repeat(40) },
      { ...recorded, observed: `The tester's pass at ${"1".repeat(12)} holds.` },
    ]),
    "sessions/2026-09-30T14-51-17-651Z_01a0f2cc.jsonl": [...header, ...turn]
      .map((entry) => `${JSON.stringify(entry)}\n`)
      .join(""),
  };
  for (const [file, text] of Object.entries(files)) writeFileSync(path.join(dir, file), text);
  const texts = ["message-1", "comment-1", "result-1"];
  const givens = ["check", "file:docs/cli.md", "file:.github/workflows/greet.yml", "earlier-1"];
  const answers = (names: { id: string; given: string }[], testing = texts) => {
    writeFileSync(
      path.join(dir, "accepts.json"),
      JSON.stringify({
        rows: texts.map((id) => ({ id, accepts: id === "message-1", quote: null })),
      })
    );
    writeFileSync(
      path.join(dir, "names.json"),
      JSON.stringify({
        rows: names.map((row) => ({
          ...row,
          names: row.id === "result-1" && row.given === "earlier-1",
          quote: row.id === "result-1" && row.given === "earlier-1" ? "The tester's pass" : null,
        })),
      })
    );
    writeFileSync(
      path.join(dir, "testing.json"),
      JSON.stringify({
        rows: testing.map((id) => ({
          id,
          testing: id === "message-1",
          quote: id === "message-1" ? "goes back to testing" : null,
        })),
      })
    );
  };
  const every = texts.flatMap((id) => givens.map((given) => ({ id, given })));
  answers(every);
  const readings = workerReadings(dir, "threads-closed", "crossing");
  expect({
    accepts: [...readings.accepts.keys()],
    accepted: readings.accepts.get("message-1")?.accepts,
    names: [...readings.names.keys()],
    named: readings.names.get("result-1 earlier-1"),
    testing: [...readings.testing.keys()],
    sentBack: readings.testing.get("message-1"),
  }).toEqual({
    accepts: texts,
    accepted: true,
    names: every.map(({ id, given }) => `${id} ${given}`),
    named: { id: "result-1", given: "earlier-1", names: true, quote: "The tester's pass" },
    testing: texts,
    sentBack: { id: "message-1", testing: true, quote: "goes back to testing" },
  });
  answers(every.slice(1));
  expect(() => workerReadings(dir, "threads-closed", "crossing")).toThrow(
    "names.json does not cover what judge.ts gave its judge: no row for text and thing message-1 check"
  );
  answers(every, texts.slice(1));
  expect(() => workerReadings(dir, "threads-closed", "crossing")).toThrow(
    "testing.json does not cover what judge.ts gave its judge: no row for text message-1"
  );
  answers(every);
  expect(() => workerReadings(dir, "reviewer-text-findings", "text-only")).toThrow(
    "accepts.json does not cover what judge.ts gave its judge: rows for text it was not given: message-1, comment-1, result-1"
  );
  // A reviewer-text-findings run, which has no commit its agents decide at, asks no testing
  // reading, and holds no testing.json: none is read.
  rmSync(path.join(dir, "testing.json"));
  writeFileSync(path.join(dir, "accepts.json"), JSON.stringify({ rows: [] }));
  expect(workerReadings(dir, "reviewer-text-findings", "text-only").testing.size).toBe(0);
});

// Catches: a judge the run's case reads let off with an answer file that holds no row, which
// judge.ts writes for a judge it starts no session for, one whose answer no term of the case reads
// (score.ts's `caseJudges`). k2's accept reads the accepts and the names judges and not the testing
// judge: accept-head-1 is k2 accept's head-1 with a testing.json that holds no row, and is scored as
// that run is; accept-head-2 is the same run with an accepts.json that holds no row for the texts
// its agents wrote, and is a rig error naming the file, as it was before judge.ts started only the
// judges a case reads.
test("a judge file with no row passes only for a judge the run's case does not read, and is a rig error for one it reads", () => {
  const seed = lastChanceRepo();
  const sha = seed.reviewerRight;
  const accepting = (role: string) =>
    sentTo(
      role,
      `I read the reviewer's commit ${sha} and accept it: its fix to docs/cli.md is right.`,
      "2026-09-30T17:32:00.000Z"
    );
  const reason = `The tester's pass at ${seed.read.slice(0, 12)} holds: the commit changes only docs/cli.md, which the check does not read.`;
  const base: Record<string, string> = {
    "out.txt": "exit=0\n",
    "world.json": worldOf(sha),
    "threads.json": "[]",
    "fixtures.json": threadRoutes("[]"),
    "case.txt": "accept\n",
    "pushes.log": "",
    "daemon.log": calls([]),
    "handoffs.json": handoffs(reviewRecord),
    "results.json": JSON.stringify([...seededPasses(seed.read), implementerResult(sha, reason)]),
    "calls.jsonl": calls([prCommentOf(`Read ${sha.slice(0, 12)}: the reviewer's fix is right.`)]),
    "published.jsonl": bus(accepting("reviewer"), accepting("architect")),
    ...threadsJudged(["message-1", "message-2", "comment-1", "result-1"], LAST_CHANCE_GIVENS, {
      accepts: ["message-1", "message-2"],
      names: [["result-1", "earlier-2", "The tester's pass"]],
    }),
  };
  const empty = JSON.stringify({ rows: [] });
  let dir = "";
  const runs: [string, Record<string, string>][] = [
    ["head-1", { ...base, "testing.json": empty }],
    ["head-2", { ...base, "accepts.json": empty }],
  ];
  for (const [name, files] of runs)
    dir = lastChanceRun("judge-reads", `threads-closed-accept-${name}`, files, {
      first: lastChanceReads(sha),
    });
  const [code, stdout, stderr] = scoreOf(
    dir,
    ...["--repo", LABEL_REPO],
    ...["--arm", "threads-closed:accept:head=2"],
    "threads-closed"
  );
  expect({ code, stderr, terms: termsOf(String(stdout)) }).toEqual({
    code: 0,
    stderr: "",
    terms: { "threads-closed-accept-head-1": "a:true,b:true,c:true,d:true,e:true" },
  });
  expect(String(stdout)).toContain(
    "threads-closed-accept-head-2\trig error: accepts.json does not cover what judge.ts gave its judge: no row for text message-1"
  );
}, 120_000);
