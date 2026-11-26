import { expect, test } from "bun:test";
import path from "node:path";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import { dispatchFirstInjected, session, skillReads } from "./transcript";

// F2 will replace this synthetic JSONL once the first builds Job captures real transcripts.
const run = path.join(import.meta.dir, "testdata", "lane-r-synthetic");

test("counts skill bytes in call order, excluding errors and the skill banner", () => {
  expect(skillReads(session(run))).toEqual([
    { skill: "legion-worker", path: "SKILL.md", resolved: true, bytes: 7, beforeFirstAction: true },
    { skill: "withheld", path: "SKILL.md", resolved: false, bytes: 0, beforeFirstAction: false },
    {
      skill: "legion-worker",
      path: "references/merge-gate.md",
      resolved: true,
      bytes: 7,
      beforeFirstAction: false,
    },
  ]);
});

test("detects context injection without mistaking a read of the marker for injection", () => {
  expect(dispatchFirstInjected(session(run))).toBe(true);
  expect(
    dispatchFirstInjected([
      { message: { role: "toolResult", content: [{ type: "text", text: DISPATCH_FIRST_MARKER }] } },
    ])
  ).toBe(false);
  expect(dispatchFirstInjected([], true)).toBe(true);
});
