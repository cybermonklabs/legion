import { expect, test } from "bun:test";
import path from "node:path";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import { dispatchFirstInjected, session, skillReads } from "./transcript";

// Fixture F2: a real lane R transcript, P10's production-check look on the base label, as the
// T4 proof's lane R Job recorded it (testdata/f2/f2.command). The synthetic JSONL beside it holds
// what F2 does not: a read that does not resolve and the injected message itself.
const f2 = path.join(import.meta.dir, "testdata", "f2");
const synthetic = path.join(import.meta.dir, "testdata", "lane-r-synthetic");

test("F2's skill reads, in call order, with the bytes each returned and whether a non-read came first", () => {
  expect(skillReads(session(f2))).toEqual([
    {
      skill: "legion-worker",
      path: "SKILL.md",
      resolved: true,
      bytes: 7967,
      beforeFirstAction: true,
    },
    {
      skill: "legion-worker",
      path: "references/merge-gate.md",
      resolved: true,
      bytes: 7747,
      beforeFirstAction: true,
    },
    {
      skill: "legion-worker",
      path: "references/findings.md",
      resolved: true,
      bytes: 6934,
      beforeFirstAction: false,
    },
    { skill: "dispatch", path: "SKILL.md", resolved: true, bytes: 5951, beforeFirstAction: false },
  ]);
});

test("counts skill bytes in call order, excluding errors and the skill banner", () => {
  expect(skillReads(session(synthetic))).toEqual([
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

// Oh My Pi stores no per-request context message: F2's transcript holds no injected
// Dispatch-first message, so its injection is read on the wire (the case gateway's record).
test("detects context injection without mistaking a read of the marker for injection", () => {
  expect(dispatchFirstInjected(session(synthetic))).toBe(true);
  expect(dispatchFirstInjected(session(f2))).toBe(false);
  expect(dispatchFirstInjected(session(f2), true)).toBe(true);
  expect(
    dispatchFirstInjected([
      { message: { role: "toolResult", content: [{ type: "text", text: DISPATCH_FIRST_MARKER }] } },
    ])
  ).toBe(false);
  expect(
    dispatchFirstInjected([
      {
        message: {
          role: "user",
          content: [
            { type: "text", text: "<system-reminder>Today.</system-reminder>" },
            { type: "text", text: `${DISPATCH_FIRST_MARKER}\nFollow it.` },
          ],
        },
      },
    ])
  ).toBe(true);
});
