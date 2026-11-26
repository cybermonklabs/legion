import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// A review round ends on the reviewer's decision, whether or not the reviewer has completed (the
// daemon's roundRejected and roundApproved, packages/daemon/internal/workflow/review.go), so the
// issue can stand in its next phase before the reviewer's handoff record is in the daemon's state.
// The move into reviewing empties the record the reviewer's role named before (the daemon's
// clearHandoff, workflow/engine.go), so until the round's completion the state shows no reviewer
// record. Stage 3's and Stage 4b's drivers run only live,
// so this file holds their text to a bounded wait for a record other than the one the state showed
// before the round (role_handoff_recorded under until_true, given that earlier record) on the line
// before each check of it, and drives lib/workflow.sh's role_handoff_recorded against a stubbed
// daemon state.
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));
const drivers = ["stage3-devbox-workflow.sh", "stage4b-sandbox-tree.sh"];
const check = /^\s*assert_handoff_record (\S+) reviewer reviewing /;

/** Each check of the reviewer's handoff record in a driver, with the line before it. */
function checksIn(driver: string) {
  const lines = readFileSync(join(import.meta.dir, "..", driver), "utf8").split("\n");
  return lines.flatMap((line, index) => {
    const found = check.exec(line);
    return found === null ? [] : [{ issue: found[1], line, before: lines[index - 1] ?? "" }];
  });
}

for (const driver of drivers) {
  test(`${driver} waits, bounded, for the reviewer's handoff record before each check of it`, () => {
    const checks = checksIn(driver);
    expect(checks.length).toBeGreaterThan(0);
    for (const { issue, line, before } of checks) {
      const wait = new RegExp(
        `until_true "\\$phase_seconds" ".*" role_handoff_recorded ${issue.replace(/\$/g, "\\$")} reviewer "\\$[a-z_]+"$`
      );
      expect({ line, before, waits: wait.test(before) }).toEqual({ line, before, waits: true });
    }
  });
}

function recorded(state: unknown, before?: string): number {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
daemon_state() { printf '%s' "$STATE"; }
${fn("role_handoff_record")}
${fn("role_handoff_recorded")}
role_handoff_recorded LEGION-208 reviewer ${before === undefined ? "" : '"$BEFORE"'}`,
    ],
    { env: { ...process.env, STATE: JSON.stringify(state), BEFORE: before ?? "" } }
  );
  return result.exitCode;
}

test("role_handoff_recorded fails until the role's handoff record is in the daemon's state", () => {
  expect(recorded({ issues: { "LEGION-208": { workers: { reviewer: {} } } } })).not.toBe(0);
  expect(recorded({ issues: {} })).not.toBe(0);
  const record = { handoffRecord: { id: "rec-1", version: 2 } };
  expect(
    recorded({ issues: { "LEGION-208": { workers: { tester: record, reviewer: {} } } } })
  ).not.toBe(0);
  expect(recorded({ issues: { "LEGION-208": { workers: { reviewer: record } } } })).toBe(0);
  expect(recorded({ issues: { "LEGION-208": { workers: { reviewer: record } } } }, "")).toBe(0);
});

test("role_handoff_recorded fails while the role's record is the one the state showed before the round", () => {
  const before = "rec-1 2";
  const state = (version: number) => ({
    issues: {
      "LEGION-208": { workers: { reviewer: { handoffRecord: { id: "rec-1", version } } } },
    },
  });
  expect(recorded(state(2), before)).not.toBe(0);
  expect(recorded({ issues: { "LEGION-208": { workers: { reviewer: {} } } } }, before)).not.toBe(0);
  expect(recorded(state(3), before)).toBe(0);
  expect(
    recorded(
      {
        issues: {
          "LEGION-208": { workers: { reviewer: { handoffRecord: { id: "rec-2", version: 1 } } } },
        },
      },
      before
    )
  ).toBe(0);
});
