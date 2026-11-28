import { expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  answeredModel,
  endOf,
  stepEnd,
  stepId,
  stepReport,
  stepRun,
  stepStart,
} from "./step-records";

// The run directory of the model row, reserved at module scope: `temporaryDirectory` registers
// the hook that makes it, and a hook cannot be registered from inside a test.
const run = temporaryDirectory("step-model", { real: true });

// The ids the label's daemon derives for a run's record (workflow/steps.go's `StepID`), as Go's
// `uuid.NewSHA1` computed them on 2026-11-19 for these runs. Catches: an id derived otherwise,
// under which the stand-in's end of a run and the seeds' start of it are two records, and
// `dispatch steps` lists a run twice, once with no end.
test("a run's record id is the one the daemon derives, its start and its end one id", () => {
  expect(stepId("LWEVAL-1", 1, "testing", 1)).toBe("c0e108ee-c6f2-54a6-a726-4cf6c98c1b84");
  expect(stepId("LWEVAL-1", 1, "implementing", 2)).toBe("e54a19bf-3ee5-5a28-a80c-615f80ab294d");
  expect(stepId("WID-1", 1, "merging", 1)).toBe("24bb1175-0530-55cb-afa2-4794b9754bf1");
});

// Catches: a write that carries a field the daemon's write of that kind does not (a session at
// the start, which only the end names), leaves out the run's identity, ends a move with another
// result than the table's, or sends text Dispatch refuses the whole record over.
test("the start, the end and the late report carry the fields the daemon's writes carry", () => {
  const run = stepRun("LWEVAL-1", 1, "testing", "legion-skills1-lweval-1-tester", 1);
  const identity = {
    task: "default",
    step: 3,
    kind: "step" as const,
    role: "tester",
    type: "testing",
    agent: "legion-skills1-lweval-1-tester",
    generation: 1,
  };
  expect(run).toEqual({ id: "c0e108ee-c6f2-54a6-a726-4cf6c98c1b84", identity });
  expect(stepStart(run, "2026-11-19T10:00:00.000Z")).toEqual({
    ...identity,
    started_at: "2026-11-19T10:00:00.000Z",
  });
  expect(endOf("TriggerBackward")).toEqual({ result: "failed", returns: true });
  expect(endOf("TriggerReviewRejected")).toEqual({ result: "failed", returns: true });
  expect(endOf("TriggerReviewApproved")).toEqual({ result: "passed", returns: false });
  expect(
    stepEnd(run, {
      startedAt: "2026-11-19T10:00:00.000Z",
      endedAt: "2026-11-19T10:05:00.000Z",
      result: "failed",
      returnedTo: 2,
      report: "h-test",
      session: "01a0f2cc",
      model: "anthropic/claude-opus-4-8",
    })
  ).toEqual({
    ...identity,
    started_at: "2026-11-19T10:00:00.000Z",
    ended_at: "2026-11-19T10:05:00.000Z",
    result: "failed",
    returned_to: 2,
    report: "h-test",
    session: "01a0f2cc",
    model: "anthropic/claude-opus-4-8",
  });
  // Text outside the form of a step's agent text is left out, as the daemon leaves it out.
  expect(
    stepEnd(run, {
      endedAt: "2026-11-19T10:05:00.000Z",
      result: "passed",
      session: "a session",
      model: "",
    })
  ).toEqual({ ...identity, ended_at: "2026-11-19T10:05:00.000Z", result: "passed" });
  expect(stepReport(run, "h-test")).toEqual({ ...identity, report: "h-test" });
});

// Catches: a model read off another session's transcript, a message no model wrote read as one,
// and a model named without its provider.
test("the model of a session is the last its own transcript's assistant messages name", () => {
  const write = (file: string, session: string, models: [string, string][]) => {
    mkdirSync(path.dirname(file), { recursive: true });
    writeFileSync(
      file,
      [
        JSON.stringify({ type: "session", id: session }),
        ...models.map(([provider, model]) =>
          JSON.stringify({ type: "message", message: { role: "assistant", provider, model } })
        ),
        JSON.stringify({ type: "message", message: { role: "user" } }),
      ].join("\n")
    );
  };
  write(path.join(run, "sessions", "a.jsonl"), "first", [["anthropic", "claude-opus-4-8"]]);
  write(path.join(run, "sessions", "reviewer", "b.jsonl"), "second", [
    ["anthropic", "claude-opus-4-8"],
    ["gateway", "claude-sonnet-5"],
    ["", "<synthetic>"],
  ]);
  expect(answeredModel(run, "first")).toBe("anthropic/claude-opus-4-8");
  expect(answeredModel(run, "second")).toBe("gateway/claude-sonnet-5");
  expect(answeredModel(run, "third")).toBeUndefined();
});
