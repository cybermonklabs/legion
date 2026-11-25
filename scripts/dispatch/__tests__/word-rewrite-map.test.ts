import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { daemonContract } from "../../../packages/contracts/src/daemon-contract.ts";
import {
  applies,
  builtInMap,
  CHILD_OPERATION_REMOVED,
  type MapEntry,
  MapFileError,
  nameAt,
  parseExtraMap,
  type Reach,
  readContract,
} from "../word-rewrite/name-map.ts";

const ROOT = join(import.meta.dir, "..", "..", "..");
const RECORDED = daemonContract({}, ROOT);

test("readContract refuses a failed script or invalid output", () => {
  const root = mkdtempSync(join(tmpdir(), "word-rewrite-contract-"));
  try {
    const scripts = join(root, "scripts");
    mkdirSync(scripts);
    const script = join(scripts, "daemon-contract.sh");
    writeFileSync(script, "echo 42; echo 'fixture missing' >&2; exit 1\n");
    expect(() => readContract(root)).toThrow(/Cannot read.*[\s\S]*fixture missing/);
    writeFileSync(script, "echo 42-extra\n");
    expect(() => readContract(root)).toThrow("did not print a positive integer contract");
  } finally {
    rmSync(root, { recursive: true });
  }
});

function reach(daemon: number | "none" | null, dispatchHoldsMerge = false): Reach {
  return {
    daemonAtLeast: (contract) => daemon === "none" || (daemon !== null && daemon >= contract),
    dispatchHoldsMerge,
  };
}

function entry(old: string): MapEntry {
  const found = builtInMap(RECORDED).find((candidate) => candidate.old === old);
  if (found === undefined) throw new Error(`the map holds no ${old}`);
  return found;
}

/** The map of old names to new names. This file is one of the places the word may stand. */
describe("the map", () => {
  test("its daemon contract is the one this checkout records, read and not written down", () => {
    expect(readContract(ROOT)).toBe(RECORDED);
  });

  test("an entry gated on a contract applies only at or above it, or where the run names no daemon", () => {
    const notice = entry("parked-decision");
    expect(notice.from).toEqual({ contract: RECORDED });
    expect(applies(notice, reach(RECORDED - 1))).toBe(false);
    expect(applies(notice, reach(RECORDED))).toBe(true);
    expect(applies(notice, reach(RECORDED + 1))).toBe(true);
    expect(applies(notice, reach(null))).toBe(false);
    expect(applies(notice, reach("none"))).toBe(true);
  });

  test("the removed child operation is gated on the contract that removed it", () => {
    expect(entry("park_child").from).toEqual({ contract: CHILD_OPERATION_REMOVED });
    expect(applies(entry("park_child"), reach(CHILD_OPERATION_REMOVED))).toBe(true);
    expect(applies(entry("park_child"), reach(CHILD_OPERATION_REMOVED - 1))).toBe(false);
  });

  test("a Dispatch API name applies only once Dispatch's build holds the merge commit", () => {
    for (const old of ["priority_work_parked", "PriorityWorkParked"]) {
      expect(entry(old).from).toEqual({ dispatchHolds: "merge" });
      expect(applies(entry(old), reach("none", false))).toBe(false);
      expect(applies(entry(old), reach(null, true))).toBe(true);
    }
  });

  test("every name the release carries is gated on the new contract (spec version 19)", () => {
    const apiNames = ["priority_work_parked", "PriorityWorkParked"];
    for (const mapped of builtInMap(RECORDED)) {
      if (apiNames.includes(mapped.old)) continue;
      const expected = mapped.old === "park_child" ? CHILD_OPERATION_REMOVED : RECORDED;
      expect({ old: mapped.old, from: mapped.from }).toEqual({
        old: mapped.old,
        from: { contract: expected },
      });
    }
    for (const file of [
      "parked-decisions.md",
      "park-asks.md",
      "PARKED_DECISION_UNTIL",
      "tree_parked",
    ]) {
      expect(applies(entry(file), reach(RECORDED - 1))).toBe(false);
      expect(applies(entry(file), reach(RECORDED))).toBe(true);
    }
  });

  test("a renamed Go test's name is gated at this checkout's contract", () => {
    for (const old of [
      "TestAParkedDecisionKeepsItsDecisionThroughTheRow",
      "TestAnOperatorsResumeRefusedByTheParkedClaimLeavesItsWakeItsTurn",
    ]) {
      expect(entry(old).from).toEqual({ contract: readContract(ROOT) });
      expect(applies(entry(old), reach(RECORDED - 1))).toBe(false);
      expect(applies(entry(old), reach(RECORDED))).toBe(true);
      const text = `go test -run '${old}' ./internal/daemon/`;
      const start = text.indexOf(old) + old.indexOf("Parked");
      expect(nameAt(text, start, start + 4, builtInMap(RECORDED))?.new).toBe(entry(old).new);
    }
    expect(entry("TestAnOperatorsResumeRefusedByTheParkedClaimLeavesItsWakeItsTurn").new).toBe(
      "TestAnOperatorsResumeRefusedByTheDelayedClaimLeavesItsWakeItsTurn"
    );
  });

  test("an entry from now (an --extra-map row) applies everywhere", () => {
    const [extra] = parseExtraMap(
      '{"old":"OLD-NAME","new":"NEW-NAME","codeName":true,"from":"merged"}\n'
    );
    expect(extra === undefined ? false : applies(extra, reach(null, false))).toBe(true);
  });

  test("--extra-map's file adds entries, one JSON object per line, blank lines skipped", () => {
    const entries = parseExtraMap(
      [
        '{"old":"OLD-ONE","new":"NEW-ONE","codeName":true,"from":"merged"}',
        "",
        '{"old":"Old heading","new":"New heading","codeName":false,"from":{"contract":230}}',
        '{"old":"old_field","new":"new_field","codeName":true,"from":{"dispatchHolds":"merge"}}',
      ].join("\n")
    );
    expect(entries).toEqual([
      { old: "OLD-ONE", new: "NEW-ONE", codeName: true, from: "merged" },
      { old: "Old heading", new: "New heading", codeName: false, from: { contract: 230 } },
      { old: "old_field", new: "new_field", codeName: true, from: { dispatchHolds: "merge" } },
    ]);
  });

  test("a malformed file is refused, naming the line", () => {
    const refusal = (text: string) => {
      try {
        parseExtraMap(text);
      } catch (error) {
        return error instanceof MapFileError ? error.message : `other: ${String(error)}`;
      }
      return "accepted";
    };
    const good = '{"old":"A","new":"B","codeName":true,"from":"merged"}';
    expect(refusal(`${good}\n{not json`)).toBe("--extra-map line 2: the line is not JSON");
    expect(refusal(`${good}\n\n{"old":"A","new":"B","codeName":true,"from":"later"}`)).toBe(
      '--extra-map line 3: from is not "merged" or an object'
    );
    expect(refusal('{"old":"A","new":"B","from":"merged"}')).toBe(
      "--extra-map line 1: codeName is not true or false"
    );
    expect(refusal('{"old":"A","new":"B","codeName":true,"from":"merged","extra":1}')).toBe(
      '--extra-map line 1: unknown key "extra"'
    );
    expect(refusal('{"old":"A","new":"B","codeName":true,"from":{"contract":0}}')).toBe(
      "--extra-map line 1: from.contract is not a positive integer"
    );
  });

  test("an old name covers an occurrence only where it is a whole name, the longest first", () => {
    const entries = builtInMap(RECORDED);
    const text =
      "Read references/parked-decisions.md, then the parked-decision kind and park_children.";
    const at = (marker: string) => {
      const start = text.indexOf(marker);
      return nameAt(text, start, start + 4, entries)?.old ?? null;
    };
    expect(at("parked-decisions.md")).toBe("parked-decisions.md");
    expect(at("parked-decision kind")).toBe("parked-decision");
    expect(at("park_children")).toBeNull();
  });
});
