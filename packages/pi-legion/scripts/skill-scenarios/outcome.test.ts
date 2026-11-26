import { expect, test } from "bun:test";
import { cpSync, existsSync, readFileSync, writeFileSync } from "node:fs";
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
  jsonl(
    "calls.jsonl",
    { as: "bun", argv: ["test"], at: time("000000001") },
    { as: "gh", argv: ["pr", "list", "--json", "body"], at: time("000000001") },
    {
      as: "gh",
      argv: ["api", "/repos/example/widgets/pulls", "--method=POST", "--input", "-"],
      files: { "-": '{"head":"legion/ACME-12","base":"main"}' },
      at: time("000000004"),
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
          payload: { body: "seeded" },
        },
        {
          seq: 9,
          created_at: time("000000001"),
          type: "result.recorded",
          issue_key: "ACME-12",
          payload: { result_id: "res", check_block_id: "k1", commit_sha: head, verdict: "pass" },
        },
        {
          seq: 10,
          created_at: time("000000002"),
          type: "handoff.recorded",
          issue_key: "ACME-12",
          payload: { handoff_id: "h", phase: "implement", version: 1 },
        },
        // Dispatch's own failed hand-off of a comment to its target: never a scored action.
        {
          seq: 11,
          created_at: time("000000002"),
          type: "comment.delivery",
          issue_key: "ACME-12",
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
      { type: "session", id: "ses-reads" },
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
