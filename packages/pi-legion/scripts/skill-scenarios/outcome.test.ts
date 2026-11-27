import { expect, test } from "bun:test";
import { cpSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { type OutcomeRecord, parseHeldoutFile, parseOutcome } from "./heldout-case";
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

// A run's Dispatch gives each seeded record an id of its own, and the seed writes each seeded
// handoff's and comment's Dispatch id by its case id to dispatch-ids.json (seed.ts
// seedCaseDispatch). The role names the Dispatch id and the case's rubric names the case's, so every
// string of the run's actions, the other writes, the report and the last text names a seeded id by
// the case's: the whole id, or its first 8 hexadecimal characters where no other seeded id begins
// so. An id is read only as a whole token: one that a lower-case hexadecimal character precedes or
// follows is part of a longer run, and a whole uuid, seeded or not, is one token.

/** A comment the event log holds after the seed, by `actor`, with `payload`. */
function commentEvent(seq: number, fraction: string, actor: object, payload: object) {
  return {
    seq,
    created_at: `2026-01-07T15:00:02.${fraction}Z`,
    type: "comment.created",
    issue_key: "ACME-12",
    actor,
    payload,
  };
}

/** The fields of each comment among `outcome`'s actions. */
function commentFields(outcome: OutcomeRecord) {
  return outcome.actions.flatMap((action) =>
    action.kind === "dispatch" && action.command === "comment" ? [action.fields] : []
  );
}

const ROLE = { kind: "session", id: "ses-synthetic" };

test("a comment body that names a seeded handoff's Dispatch id, whole or by its first 8 characters, is written with the case's id", () => {
  const { dir, write, caseHead } = syntheticRun();
  const test1 = "5f2c8a1e-3b4d-4e6f-8a9b-0c1d2e3f4a5b";
  const test2 = "9d4e7b20-6c1a-4f3e-9b8d-2e5f6a7c8d90";
  // The comment's id begins as the run's head commit does, so it is read as a Dispatch id before
  // any commit is.
  const departure = "aaaaaaaa-1b2c-4d3e-8f40-5a6b7c8d9e0f";
  write(
    "dispatch-ids.json",
    JSON.stringify({
      "hnd-acme-12-test-1": test1,
      "hnd-acme-12-test-2": test2,
      "cmt-acme-12-departure": departure,
    })
  );
  const tester = { kind: "session", id: "ses-acme-12-tester" };
  addEvents(
    dir,
    commentEvent(12, "000000007", ROLE, {
      body: `The test handoff was written twice, ${test1} and ${test2.slice(0, 8)} (dispatch://ACME-12/comment/${departure}), at ${"a".repeat(40)}.`,
      reply_to: departure,
    }),
    commentEvent(13, "000000008", tester, { body: `Only ${test1.slice(0, 8)} is mine.` })
  );
  write(
    "sessions/example.jsonl",
    [
      { type: "session", id: ROLE.id },
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
          content: [{ type: "text", text: `Read dispatch://ACME-12/handoff/${test1}.` }],
        },
      },
      {
        message: {
          role: "assistant",
          content: [{ type: "text", text: `${test2} repeats ${test1.slice(0, 8)}.` }],
        },
      },
    ]
      .map((entry) => JSON.stringify(entry))
      .join("\n")
  );
  const got = writeOutcome(dir, "plan-reviewer");
  expect(commentFields(got)).toEqual([
    {
      body: `The test handoff was written twice, hnd-acme-12-test-1 and hnd-acme-12-test-2 (dispatch://ACME-12/comment/cmt-acme-12-departure), at ${caseHead}.`,
      reply_to: "cmt-acme-12-departure",
    },
  ]);
  expect(got.otherWrites).toMatchObject([
    { command: "comment", fields: { body: "Only hnd-acme-12-test-1 is mine." }, actor: tester },
  ]);
  expect(got.agentReport).toBe("Read dispatch://ACME-12/handoff/hnd-acme-12-test-1.");
  expect(got.lastText).toBe("hnd-acme-12-test-2 repeats hnd-acme-12-test-1.");
  expect(parseOutcome(readFileSync(path.join(dir, "outcome.json"), "utf8"))).toEqual(got);
});

test("an id that no seed wrote, and a prefix two seeded ids share, are left as they are", () => {
  const { dir, write } = syntheticRun();
  const first = "00000000-0000-4000-8000-000000000001";
  const second = "00000000-0000-4000-8000-000000000002";
  const departure = "5f2c8a1e-3b4d-4e6f-8a9b-0c1d2e3f4a5b";
  // No seed wrote this id, though it begins as the seeded comment's does.
  const unseeded = "5f2c8a1e-0000-4000-8000-0000000000ff";
  write(
    "dispatch-ids.json",
    JSON.stringify({
      "hnd-acme-12-test-1": first,
      "hnd-acme-12-test-2": second,
      "cmt-acme-12-departure": departure,
    })
  );
  addEvents(
    dir,
    commentEvent(12, "000000007", ROLE, {
      body: `${unseeded} and 7c3e9a10 are no seed's, 00000000 begins both handoffs, and ${second} is the second.`,
    })
  );
  expect(commentFields(writeOutcome(dir))).toEqual([
    {
      body: `${unseeded} and 7c3e9a10 are no seed's, 00000000 begins both handoffs, and hnd-acme-12-test-2 is the second.`,
    },
  ]);
});

test("a seeded id, or its first 8 characters, inside a longer hexadecimal run is left as it is", () => {
  const { dir, write } = syntheticRun();
  const test1 = "5f2c8a1e-3b4d-4e6f-8a9b-0c1d2e3f4a5b";
  const prefix = test1.slice(0, 8);
  write("dispatch-ids.json", JSON.stringify({ "hnd-acme-12-test-1": test1 }));
  addEvents(
    dir,
    commentEvent(12, "000000007", ROLE, {
      body: `Inside a run: f${test1}, ${test1}0, 0${prefix} and ${prefix}c. Beside a dash or a stop: ${test1}-v2, handoff/${prefix}.`,
    })
  );
  expect(commentFields(writeOutcome(dir))).toEqual([
    {
      body: `Inside a run: f${test1}, ${test1}0, 0${prefix} and ${prefix}c. Beside a dash or a stop: hnd-acme-12-test-1-v2, handoff/hnd-acme-12-test-1.`,
    },
  ]);
});

test("a run with no dispatch-ids.json is written as before", () => {
  const { dir, caseHead } = syntheticRun();
  const test1 = "5f2c8a1e-3b4d-4e6f-8a9b-0c1d2e3f4a5b";
  addEvents(
    dir,
    commentEvent(12, "000000007", ROLE, {
      body: `${test1} (${test1.slice(0, 8)}) at ${"a".repeat(40)}.`,
    })
  );
  expect(existsSync(path.join(dir, "dispatch-ids.json"))).toBe(false);
  expect(commentFields(writeOutcome(dir))).toEqual([
    { body: `${test1} (${test1.slice(0, 8)}) at ${caseHead}.` },
  ]);
});
