import { expect, test } from "bun:test";
import {
  appendFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { parseHeldoutFile, parseOutcome } from "./heldout-case";
import { writeOutcome } from "./outcome";

const data = path.join(import.meta.dir, "testdata");
const work = temporaryDirectories("lane-r-outcomes");

function syntheticRun() {
  const dir = work();
  cpSync(path.join(data, "lane-r-synthetic"), dir, { recursive: true });
  const write = (name: string, value: string) => writeFileSync(path.join(dir, name), value);
  const jsonl = (name: string, ...rows: unknown[]) =>
    write(name, rows.map((row) => JSON.stringify(row)).join("\n"));
  const time = (fraction: string) => `2026-01-07T15:00:02.${fraction}Z`;
  const example = parseHeldoutFile(
    readFileSync(path.join(data, "heldout-case-example.json"), "utf8")
  );
  const p05 = example.cases.find(({ id }) => id === "P05");
  if (!p05?.world.git) throw new Error("no P05 git world");
  const caseHead = p05.world.git.workspace.standingOn;
  const head = "a".repeat(40);
  write("clock.txt", `${p05.world.clock}\n`);
  write("clock-real.txt", "2026-01-07T15:00:00Z\n");
  write("commit-map.json", JSON.stringify({ [caseHead]: head }));
  write("pushes.log", `${time("000000003")} refs/heads/legion/ACME-12 ${"0".repeat(40)} ${head}\n`);
  jsonl(
    "daemon.log",
    {
      at: time("000000001"),
      method: "POST",
      path: "/legion/v1/grants",
      status: 200,
      body: {},
      answer: {},
    },
    {
      at: time("000000005"),
      method: "POST",
      path: "/legion/v1/handoff/complete",
      status: 409,
      body: { grantId: "grant", wakeAt: "2030-01-07T16:00:00Z", commit: head },
      answer: { code: "HANDOFF_NO_PULL_REQUEST" },
    }
  );
  // calls.jsonl as gh-standin.ts writes it: each gh call's request, as gh reads its arguments, and
  // the change it kept; and a line of rig.sh's `bun` stand-in.
  jsonl(
    "calls.jsonl",
    { as: "bun", argv: ["test"], at: time("000000001"), exit: 0 },
    {
      as: "gh",
      argv: ["pr", "list", "--json", "body"],
      at: time("000000001"),
      request: { command: "gh pr list", fields: {} },
      exit: 0,
    },
    {
      as: "gh",
      argv: ["api", "/repos/example/widgets/pulls", "--method=POST", "--input", "-"],
      files: { "-": '{"head":"legion/ACME-12","base":"main"}' },
      at: time("000000004"),
      request: {
        method: "POST",
        path: "repos/example/widgets/pulls",
        fields: { head: "legion/ACME-12", base: "main" },
      },
      exit: 0,
      pull: { action: "open", number: 1, head: "legion/ACME-12", base: "main", sha: head },
    }
  );
  jsonl("staging.jsonl", {
    at: time("000000006"),
    tree: "b".repeat(40),
    files: ["app.ts"],
    job: "test",
  });
  write(
    "dispatch-readback.json",
    JSON.stringify({
      seedThrough: 8,
      handoffs: [{ id: "h", body: JSON.stringify({ head }) }],
      results: { checks: [{ results: [{ id: "res", run: "bun test" }] }] },
      events: [
        {
          seq: 8,
          created_at: time("000000001"),
          type: "comment.created",
          issue_key: "ACME-12",
          actor: { kind: "session", id: "rig-seed" },
          payload: { body: "seeded" },
        },
        {
          seq: 9,
          created_at: time("000000001"),
          type: "result.recorded",
          issue_key: "ACME-12",
          actor: { kind: "session", id: "ses-synthetic" },
          payload: { result_id: "res", check_block_id: "k1", commit_sha: head, verdict: "pass" },
        },
        {
          seq: 10,
          created_at: time("000000002"),
          type: "handoff.recorded",
          issue_key: "ACME-12",
          actor: { kind: "session", id: "ses-synthetic" },
          payload: { handoff_id: "h", phase: "implement", version: 1 },
        },
        // Dispatch's own failed hand-off of a comment to its target: never a scored action.
        {
          seq: 11,
          created_at: time("000000002"),
          type: "comment.delivery",
          issue_key: "ACME-12",
          actor: { kind: "system", id: "dispatch" },
          payload: { state: "failed", target: "role:legion-acme-acme-12-tester" },
        },
      ],
    })
  );
  return { dir, write, caseHead };
}

test("writes one nanosecond-ordered list, retaining P05's premature result and handoff", () => {
  const { dir, caseHead } = syntheticRun();
  const got = writeOutcome(dir);
  expect(got.actions).toMatchObject([
    {
      seq: 1,
      at: "2030-01-07T15:00:02.000000001Z",
      kind: "dispatch",
      command: "result",
      target: "ACME-12",
      fields: { check: "k1", commit: caseHead, run: "bun test" },
      actor: { kind: "session", id: "ses-synthetic" },
    },
    { seq: 2, kind: "github", command: "gh pr list" },
    { seq: 3, kind: "dispatch", command: "handoff-write", fields: { body: { head: caseHead } } },
    { seq: 4, kind: "push", ref: "legion/ACME-12", commit: caseHead },
    {
      seq: 5,
      kind: "github",
      method: "POST",
      path: "repos/example/widgets/pulls",
      fields: { head: "legion/ACME-12", base: "main" },
      change: { action: "open", number: 1, head: "legion/ACME-12", base: "main", sha: caseHead },
    },
    {
      seq: 6,
      kind: "daemon",
      op: "handoff_complete",
      args: { wake_at: "2030-01-07T16:00:00Z", commit: caseHead },
      answer: { code: "HANDOFF_NO_PULL_REQUEST" },
    },
    { seq: 7, kind: "staging", files: ["app.ts"] },
  ]);
  expect(got.lastText).toBe("Done.");
  expect(got.dispatchFirstInjected).toBe(true);
  expect(got.skillReads[0]?.skill).toBe("legion-worker");
  const text = readFileSync(path.join(dir, "outcome.json"), "utf8");
  expect(parseOutcome(text)).toEqual(got);
  expect(text).not.toContain("grantId");
  expect(writeOutcome(dir).id).not.toBe(got.id);
});

test("invalid records are refused before outcome.json is written", () => {
  const { dir, write } = syntheticRun();
  write("staging.jsonl", '{"at":"2026-01-07T15:00:02Z","tree":"not-a-tree","files":[]}\n');
  expect(() => writeOutcome(dir)).toThrow("tree");
  expect(existsSync(path.join(dir, "outcome.json"))).toBe(false);
});

// A skill read's `beforeFirstAction` is whether it came before the run's first scored action, the
// first action of outcome.json, by their places on the host clock the transcript and the logs
// share (heldout-case.ts's SkillRead). A tool call that is no scored action, a `bash` that runs
// `git status`, comes first here and decides nothing.
test("a skill read is before the first action where it precedes outcome.json's first scored action", () => {
  const { dir, write } = syntheticRun();
  const call = (at: string, id: string, name: string, args: object, result: string) => [
    {
      type: "message",
      timestamp: at,
      message: { role: "assistant", content: [{ type: "toolCall", id, name, arguments: args }] },
    },
    {
      type: "message",
      message: {
        role: "toolResult",
        toolName: name,
        toolCallId: id,
        content: [{ type: "text", text: result }],
      },
    },
  ];
  write(
    "sessions/example.jsonl",
    [
      { type: "session", id: "ses-synthetic", timestamp: "2026-01-07T15:00:00Z" },
      ...call("2026-01-07T15:00:01.000Z", "status", "bash", { command: "git status" }, "clean"),
      ...call(
        "2026-01-07T15:00:01.500Z",
        "entry",
        "read",
        { path: "skill://legion-worker" },
        "A café"
      ),
      // The first scored action, P05's result, is at 15:00:02.000000001 (syntheticRun).
      ...call(
        "2026-01-07T15:00:03.000Z",
        "reference",
        "read",
        { path: "skill://legion-worker/references/merge-gate.md" },
        "Details"
      ),
    ]
      .map((entry) => JSON.stringify(entry))
      .join("\n")
  );
  const got = writeOutcome(dir);
  expect(got.actions[0]?.at).toBe("2030-01-07T15:00:02.000000001Z");
  expect(got.skillReads.map(({ path, beforeFirstAction }) => [path, beforeFirstAction])).toEqual([
    ["SKILL.md", true],
    ["references/merge-gate.md", false],
  ]);
});

/** `dir`'s dispatch-readback.json with `events` added to its event log. */
function addEvents(dir: string, ...events: object[]) {
  const file = path.join(dir, "dispatch-readback.json");
  const readback = JSON.parse(readFileSync(file, "utf8"));
  readback.events.push(...events);
  writeFileSync(file, JSON.stringify(readback));
}

// The record keeps the actor of each Dispatch event. Only the events the role's own session wrote
// after the seed are its actions; another actor's after the seed (Dispatch itself, a person,
// another session) is none of the role's and stays in the record, apart, with its writer. A new
// version of a document the role wrote (`artifact.version`, as Dispatch names it) is the role's
// `doc-edit`; a person's edit of it in the browser (`artifact.human_edited`) is the person's.
test("the role's actions are its own session's Dispatch writes, each with its actor; another actor's stay apart", () => {
  const { dir } = syntheticRun();
  const at = (fraction: string) => `2026-01-07T15:00:02.${fraction}Z`;
  const event = (seq: number, fraction: string, type: string, actor: object, payload = {}) => ({
    seq,
    created_at: at(fraction),
    type,
    issue_key: "ACME-12",
    artifact_id: "art-plan",
    actor,
    payload,
  });
  const role = { kind: "session", id: "ses-synthetic" };
  const person = { kind: "user", id: "usr-acme" };
  const tester = { kind: "session", id: "ses-acme-12-tester" };
  const dispatch = { kind: "system", id: "dispatch" };
  addEvents(
    dir,
    event(12, "000000007", "artifact.version", role, { artifact_id: "art-plan", name: "plan.md" }),
    event(13, "000000008", "comment.created", person, { body: "A person's note." }),
    event(14, "000000009", "artifact.human_edited", person, { artifact_id: "art-plan" }),
    event(15, "000000010", "comment.created", tester, { body: "The tester's note." }),
    event(16, "000000011", "issue.updated", dispatch, { status: "in_progress" })
  );
  const got = writeOutcome(dir);
  expect(
    got.actions
      .filter((action) => action.kind === "dispatch")
      .map(({ command, actor }) => [command, actor])
  ).toEqual([
    ["result", role],
    ["handoff-write", role],
    ["doc-edit", role],
  ]);
  expect(got.otherWrites?.map(({ at, command, actor }) => [at, command, actor])).toEqual([
    ["2030-01-07T15:00:02.000000008Z", "comment", person],
    ["2030-01-07T15:00:02.000000009Z", "artifact.human_edited", person],
    ["2030-01-07T15:00:02.000000010Z", "comment", tester],
    ["2030-01-07T15:00:02.000000011Z", "issue-update", dispatch],
  ]);
  expect(parseOutcome(readFileSync(path.join(dir, "outcome.json"), "utf8"))).toEqual(got);
});

// A GitHub action is gh-standin.ts's record of the call alone: the request gh makes for it, as
// gh-standin read the arguments, and the change it kept, which is what the call did. outcome.ts
// reads no argument of the call again: a short flag is what gh-standin says it is, and a call that
// kept no change records none, whatever its arguments say.
test("a GitHub action is the stand-in's request and the change it kept, never a second reading of the arguments", () => {
  const { dir, write } = syntheticRun();
  const at = (fraction: string) => `2026-01-07T15:00:02.${fraction}Z`;
  const review = { action: "review", number: 7, state: "APPROVED", body: "" };
  write(
    "calls.jsonl",
    [
      {
        as: "gh",
        argv: ["pr", "review", "-a"],
        at: at("000000001"),
        request: { command: "gh pr review", fields: { approve: true } },
        exit: 0,
        pull: review,
      },
      {
        as: "gh",
        argv: ["pr", "merge", "7", "-s"],
        at: at("000000002"),
        request: { command: "gh pr merge", fields: { number: 7, squash: true } },
        exit: 1,
      },
      {
        as: "gh",
        argv: ["api", "-H", "Accept: x", "repos/acme/widgets/pulls/7", "-f", "body=b"],
        at: at("000000003"),
        request: { method: "POST", path: "repos/acme/widgets/pulls/7", fields: { body: "b" } },
        exit: 0,
      },
    ]
      .map((row) => JSON.stringify(row))
      .join("\n")
  );
  // Each GitHub call lands among the run's other actions by its time: the result before the
  // review, the handoff before the merge, the push after the POST.
  expect(writeOutcome(dir).actions.filter((action) => action.kind === "github")).toEqual([
    {
      kind: "github",
      seq: 2,
      at: "2030-01-07T15:00:02.000000001Z",
      command: "gh pr review",
      fields: { approve: true },
      change: review,
    },
    {
      kind: "github",
      seq: 4,
      at: "2030-01-07T15:00:02.000000002Z",
      command: "gh pr merge",
      fields: { number: 7, squash: true },
    },
    {
      kind: "github",
      seq: 5,
      at: "2030-01-07T15:00:02.000000003Z",
      method: "POST",
      path: "repos/acme/widgets/pulls/7",
      fields: { body: "b" },
    },
  ]);
  write("calls.jsonl", JSON.stringify({ as: "gh", argv: ["pr", "view"], at: at("000000001") }));
  expect(() => writeOutcome(dir)).toThrow("records no request");
});

test("records the role's own reads: its legion read calls and its GitHub reads, not the extension's record check", () => {
  const { dir, write } = syntheticRun();
  const time = (fraction: string) => `2026-01-07T15:00:03.${fraction}Z`;
  const get = (at: string, issue: string, status: number, answer: unknown) => ({
    at,
    method: "GET",
    path: `/legion/v1/issues/${issue}/record`,
    status,
    body: null,
    answer,
  });
  const record = { record: { key: "ACME-12", phase: "implementing" } };
  write(
    "daemon.log",
    [
      get(time("100"), "ACME-12", 200, record),
      get(time("100"), "ACME-99", 404, { code: "RECORD_NOT_FOUND" }),
      // The extension's own check, after every legion call has its result.
      get(time("900"), "ACME-12", 200, record),
    ]
      .map((line) => JSON.stringify(line))
      .join("\n")
  );
  const legion = (id: string, args: object) => ({
    type: "toolCall",
    id,
    name: "legion",
    arguments: args,
  });
  const result = (id: string, at: string) => ({
    type: "message",
    timestamp: at,
    message: {
      role: "toolResult",
      toolName: "legion",
      toolCallId: id,
      content: [{ type: "text", text: "{}" }],
    },
  });
  write(
    "sessions/example.jsonl",
    [
      { type: "session", id: "ses-synthetic" },
      {
        type: "message",
        timestamp: time("050"),
        message: {
          role: "assistant",
          content: [
            legion("own", { op: "read_record", i: "Reading the record" }),
            legion("child", { op: "read_record", issue: "ACME-99" }),
          ],
        },
      },
      result("own", time("200")),
      result("child", time("200")),
      {
        type: "message",
        message: { role: "assistant", content: [{ type: "text", text: "Done." }] },
      },
    ]
      .map((entry) => JSON.stringify(entry))
      .join("\n")
  );
  const got = writeOutcome(dir);
  expect(got.actions.filter((action) => action.kind === "daemon")).toEqual([
    {
      seq: 7,
      at: "2030-01-07T15:00:03.100000000Z",
      kind: "daemon",
      op: "read_record",
      args: {},
      answer: { code: null, body: record },
    },
    {
      seq: 8,
      at: "2030-01-07T15:00:03.100000000Z",
      kind: "daemon",
      op: "read_record",
      args: { issue: "ACME-99" },
      answer: { code: "RECORD_NOT_FOUND", body: { code: "RECORD_NOT_FOUND" } },
    },
  ]);
  expect(got.actions.filter((action) => action.kind === "github")).toMatchObject([
    { kind: "github", command: "gh pr list" },
    { kind: "github", method: "POST", path: "repos/example/widgets/pulls" },
  ]);
});

test("A case takes the selected task's returned report, not the host paraphrase", () => {
  const { dir, write } = syntheticRun();
  const entries = [
    { type: "session", id: "host" },
    {
      message: {
        role: "assistant",
        content: [
          { type: "toolCall", name: "task", id: "task-1", arguments: { agent: "plan-reviewer" } },
        ],
      },
    },
    {
      message: {
        role: "toolResult",
        toolCallId: "task-1",
        content: [{ type: "text", text: "The report." }],
      },
    },
    { message: { role: "assistant", content: [{ type: "text", text: "My paraphrase." }] } },
  ];
  write("sessions/example.jsonl", entries.map((entry) => JSON.stringify(entry)).join("\n"));
  const outcome = writeOutcome(dir, "plan-reviewer");
  expect(outcome.agentReport).toBe("The report.");
  expect(outcome.lastText).toBe("My paraphrase.");
  expect(() => writeOutcome(dir, "plan-gap-analyst")).toThrow("exactly one task call");
});

// The records of a run's other tools: what its transcript shows of an Envoy tool, a `task` call and
// the workspace, and what dispatch-recorder.ts logged of each `dispatch` command.

/** A transcript entry in which the assistant makes the tool call `id` of `name` at `at`. */
const calls = (at: string, ...made: { id: string; name: string; args: object }[]) => ({
  type: "message",
  timestamp: at,
  message: {
    role: "assistant",
    content: made.map(({ id, name, args }) => ({
      type: "toolCall",
      id,
      name,
      arguments: args,
    })),
  },
});
/** The tool's answer to call `id`, an error where `isError`. */
const answer = (id: string, text: string, isError = false) => ({
  type: "message",
  message: {
    role: "toolResult",
    toolName: "tool",
    toolCallId: id,
    isError,
    content: [{ type: "text", text }],
  },
});
/** Writes the run's session transcript as `entries` under the run's own session id. */
function transcriptOf(write: (name: string, value: string) => void, ...entries: object[]) {
  write(
    "sessions/example.jsonl",
    [{ type: "session", id: "ses-synthetic" }, ...entries].map((e) => JSON.stringify(e)).join("\n")
  );
}
/** Appends `rows` to the run's calls.jsonl. */
function addCalls(dir: string, ...rows: object[]) {
  appendFileSync(
    path.join(dir, "calls.jsonl"),
    `\n${rows.map((row) => JSON.stringify(row)).join("\n")}`
  );
}
const host = (fraction: string) => `2026-01-07T15:00:01.${fraction}Z`;
const caseTime = (fraction: string) => `2030-01-07T15:00:01.${fraction}Z`;

// An Envoy message is what its tool answered without error, called by the tool's name or as a write
// to its xd:// device. A send the tool refused (a session no listener knows) sent nothing, and a call
// it never answered, nor any other device, is no message.
test("an Envoy message is a call its tool answered, by name or by device, and a refused or unanswered one is none", () => {
  const { dir, write } = syntheticRun();
  const device = (name: string, args: object) => ({
    name: "write",
    args: { path: `xd://${name}`, content: JSON.stringify(args), i: "Post" },
  });
  transcriptOf(
    write,
    calls(
      host("100"),
      {
        id: "published",
        name: "envoy_publish",
        args: {
          topic: "notifications.role.acme.acme-12-tester",
          message: "ACME-12 is pushed.",
          expects_reply: false,
          i: "Telling the tester",
        },
      },
      { id: "sent", ...device("envoy_send", { session_id: "ses-other", message: "hello" }) },
      { id: "refused", name: "envoy_send", args: { session_id: "ses-gone", message: "lost" } },
      { id: "device", ...device("envoy_publish", { topic: "team.news", message: "m2" }) },
      { id: "unanswered", name: "envoy_publish", args: { topic: "team.none", message: "m3" } },
      { id: "elsewhere", ...device("bash", { command: "echo envoy_publish" }) },
      { id: "listing", name: "envoy_list", args: {} }
    ),
    answer("published", "Published."),
    answer("sent", "Sent."),
    answer("refused", "no live session ses-gone", true),
    answer("device", "Published."),
    answer("elsewhere", "ok"),
    answer("listing", "[]")
  );
  const sent = writeOutcome(dir).actions.filter((action) => action.kind === "envoy");
  expect(sent.map(({ tool, target, fields }) => [tool, target, fields])).toEqual([
    [
      "publish",
      "notifications.role.acme.acme-12-tester",
      { message: "ACME-12 is pushed.", expects_reply: false },
    ],
    ["send", "ses-other", { message: "hello" }],
    ["publish", "team.news", { message: "m2" }],
  ]);
  expect(sent[0]?.at).toBe(`${caseTime("100").slice(0, -1)}000000Z`);
});

// One agent for each item of a `task` call's tasks, with the end of its own transcript. A call the
// tool refused, whose agents wrote nothing, started none.
test("a task call with two items is two agent actions, each with its own end where its transcript wrote one", () => {
  const { dir, write } = syntheticRun();
  transcriptOf(
    write,
    calls(host("100"), {
      id: "two",
      name: "task",
      args: {
        context: "Shared brief.",
        tasks: [
          { agent: "sonic", name: "Alpha", task: "Read the diff.", solutionSpace: "one reading" },
          { name: "Beta", task: "Read the spec." },
        ],
        i: "Starting two agents",
      },
    }),
    calls(host("200"), {
      id: "refused",
      name: "task",
      args: { tasks: [{ agent: "sonic", name: "Gamma", task: "Never started." }] },
    }),
    answer("two", "Both done."),
    answer("refused", "Unknown agent sonic", true)
  );
  const message = (at: string) => ({ type: "message", timestamp: at, message: { role: "user" } });
  mkdirSync(path.join(dir, "sessions", "example"));
  write(
    "sessions/example/Alpha.jsonl",
    [{ type: "session", id: "ses-alpha" }, message(host("300")), message(host("900"))]
      .map((entry) => JSON.stringify(entry))
      .join("\n")
  );
  const agents = writeOutcome(dir).actions.filter((action) => action.kind === "agent");
  expect(agents).toHaveLength(2);
  expect(agents[0]).toMatchObject({
    agent: "sonic",
    name: "Alpha",
    fields: { task: "Read the diff.", solutionSpace: "one reading" },
    at: `${caseTime("100").slice(0, -1)}000000Z`,
    endedAt: `${caseTime("900").slice(0, -1)}000000Z`,
  });
  expect(agents[1]).toMatchObject({
    agent: "task",
    name: "Beta",
    fields: { task: "Read the spec." },
  });
  expect(agents[1]).not.toHaveProperty("endedAt");
});

// A `dispatch` command is a read where it exited 0 and sent Dispatch only GET requests, and no dry
// run. The CLI's own write stays the `dispatch` action Dispatch's events make, once.
test("a dispatch command that wrote nothing is a dispatch-read, and one that wrote, failed, only checked or sent nothing known is none", () => {
  const { dir } = syntheticRun();
  const get = (path: string) => ({ method: "GET", path });
  const row = (fraction: string, command: string, flags: object, extra: object = {}) => ({
    at: host(fraction),
    done: host(fraction),
    as: "dispatch",
    argv: [command],
    exit: 0,
    requests: [get(`/api/v1/${command}`)],
    call: { command, flags, dryRun: false },
    ...extra,
  });
  addCalls(
    dir,
    row("100", "read", { issue: "ACME-12" }),
    row("200", "handoff-read", { issue: "ACME-12", phase: "implement", "handoff-id": "h" }),
    row("300", "doc-read", { ref: "dispatch://ACME-12/spec", project: "ACME" }),
    row("400", "open-asks", {}),
    row(
      "500",
      "comment",
      { issue: "ACME-12", body: "A note." },
      {
        requests: [
          get("/api/v1/issues/ACME-12"),
          { method: "POST", path: "/api/v1/issues/ACME-12/comments" },
        ],
      }
    ),
    row(
      "600",
      "read",
      { issue: "ACME-12" },
      { call: { command: "read", flags: { issue: "ACME-12" }, dryRun: true } }
    ),
    row("700", "read", { issue: "ACME-12" }, { exit: 1 }),
    row("800", "read", { issue: "ACME-12" }, { requests: null }),
    row("900", "read", { issue: "ACME-12" }, { call: null }),
    row("950", "read", { issue: "ACME-12" }, { requests: [] })
  );
  addEvents(dir, {
    seq: 12,
    created_at: host("500"),
    type: "comment.created",
    issue_key: "ACME-12",
    actor: { kind: "session", id: "ses-synthetic" },
    payload: { body: "A note." },
  });
  const got = writeOutcome(dir);
  const reads = got.actions.filter((action) => action.kind === "dispatch-read");
  expect(reads.map(({ command, target, fields }) => [command, target, fields])).toEqual([
    ["read", "ACME-12", {}],
    ["handoff-read", "ACME-12", { phase: "implement", "handoff-id": "h" }],
    ["doc-read", "dispatch://ACME-12/spec", { project: "ACME" }],
    ["open-asks", "/api/v1/open-asks", {}],
  ]);
  // The command that wrote is one `dispatch` action, from Dispatch's own event, and no read.
  const comments = got.actions.filter(
    (action) => action.kind === "dispatch" && action.command === "comment"
  );
  expect(comments).toHaveLength(1);
  expect(got.actions.map((action) => action.seq)).toEqual(got.actions.map((_, at) => at + 1));
});

// The skill reads before the first action of a T0 kind: a Dispatch read, an Envoy message or a task
// agent's start before it does not move that point.
test("a skill read between a dispatch-read and the first action of a T0 kind is before the first action", () => {
  const { dir, write } = syntheticRun();
  addCalls(dir, {
    at: "2026-01-07T15:00:00.900Z",
    done: "2026-01-07T15:00:00.950Z",
    as: "dispatch",
    argv: ["read"],
    exit: 0,
    requests: [{ method: "GET", path: "/api/v1/issues/ACME-12" }],
    call: { command: "read", flags: { issue: "ACME-12" }, dryRun: false },
  });
  const read = (at: string, id: string, uri: string) => [
    calls(at, { id, name: "read", args: { path: uri } }),
    answer(id, "A café"),
  ];
  transcriptOf(
    write,
    ...read("2026-01-07T15:00:01.500Z", "first", "skill://legion-worker"),
    ...read("2026-01-07T15:00:03.000Z", "later", "skill://legion-worker/references/merge-gate.md")
  );
  const got = writeOutcome(dir);
  expect(got.actions[0]).toMatchObject({ kind: "dispatch-read", seq: 1 });
  // The first action of a T0 kind is P05's result, at 15:00:02.000000001.
  expect(got.actions[1]).toMatchObject({ kind: "dispatch", seq: 2 });
  expect(got.skillReads.map(({ path, beforeFirstAction }) => [path, beforeFirstAction])).toEqual([
    ["SKILL.md", true],
    ["references/merge-gate.md", false],
  ]);
});

// The workspace is jj's operation record, one entry for each operation in the order it ended, with
// the commits it left `@-` on written as the world names them.
test("the workspace is each jj operation by its end, in that order, with the world's commit ids; a staged Job keeps its name and source", () => {
  const { dir, write, caseHead } = syntheticRun();
  const head = "a".repeat(40);
  const root = "0".repeat(40);
  const operation = (end: string, description: string, standingOn: string[]) => ({
    id: description,
    start: end,
    end,
    description,
    standingOn,
  });
  // Written newest first, as a record of concurrent operations may be: the entries are in time order.
  write(
    "workspace-ops.jsonl",
    [
      operation("2026-01-07T15:00:02.500+00:00", `commit ${head.slice(0, 12)}`, [head]),
      operation("2026-01-07T15:00:01.250+00:00", "add workspace 'default'", [root]),
    ]
      .map((line) => `${JSON.stringify(line)}\n`)
      .join("")
  );
  write(
    "staging.jsonl",
    `${JSON.stringify({
      at: host("600"),
      tree: "b".repeat(40),
      files: ["app.ts"],
      job: "build",
      source: "$TMPDIR/stage-a",
    })}\n${JSON.stringify({ at: host("700"), tree: "c".repeat(40), files: ["app.ts"] })}\n`
  );
  const got = writeOutcome(dir);
  expect(got.workspace).toEqual([
    {
      at: "2030-01-07T15:00:01.250000000Z",
      description: "add workspace 'default'",
      standingOn: [root],
    },
    {
      at: "2030-01-07T15:00:02.500000000Z",
      description: `commit ${caseHead.slice(0, 12)}`,
      standingOn: [caseHead],
    },
  ]);
  const staged = got.actions.filter((action) => action.kind === "staging");
  expect(staged).toMatchObject([{ job: "build", source: "$TMPDIR/stage-a" }, {}]);
  expect(staged[1]).not.toHaveProperty("job");
  expect(parseOutcome(readFileSync(path.join(dir, "outcome.json"), "utf8"))).toEqual(got);
  // A run with no record of its workspace has no workspace in its outcome.
  const { dir: bare } = syntheticRun();
  expect(writeOutcome(bare)).not.toHaveProperty("workspace");
});
