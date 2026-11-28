/** `legRowFaults` (e2e/leg-rows.ts): each way the shard legs' lists can disagree is a fault that
 *  names its rows and legs, and lists that agree are none. */
import { expect, test } from "bun:test";

import { legRowFaults, rowId, shownRow } from "../leg-rows";

const selection = ["a", "b", "c", "d"];
const leg = (name: string, ran: string[], read: string[] = selection) => ({
  leg: name,
  ran,
  selection: read,
});

test("legs that read one selection and ran each of its rows once have no fault", () => {
  expect(
    legRowFaults([leg("shard-1", ["a", "c"]), leg("shard-2", ["b"]), leg("shard-3", ["d"])])
  ).toEqual([]);
});

test("a row no leg ran is named, as the planted reporter that dropped the last file left it", () => {
  expect(legRowFaults([leg("shard-1", ["a", "c"]), leg("shard-2", ["b"])])).toEqual([
    "1 rows of the selection ran on no leg: d",
  ]);
});

test("a row two legs ran is named with both legs", () => {
  expect(legRowFaults([leg("shard-1", ["a", "b"]), leg("shard-2", ["b", "c", "d"])])).toEqual([
    "1 rows ran on more than one leg: b (on shard-1, shard-2)",
  ]);
});

test("legs that read different selections, and a row outside the selection, are faults", () => {
  expect(
    legRowFaults([leg("shard-1", ["a", "b"]), leg("shard-2", ["c", "d", "e"], [...selection, "e"])])
  ).toEqual([
    "shard-2 read a selection of 5 rows and shard-1 one of 4, so the legs placed different selections: only shard-2's holds e, only shard-1's holds nothing",
    "1 rows ran that the selection does not hold: e",
  ]);
});

test("no legs at all is a fault, never a pass", () => {
  expect(legRowFaults([])).toEqual([
    "no shard leg's row lists were read, so no row can be said to have run",
  ]);
});

test("a fault names at most ten rows and counts the rest", () => {
  const many = Array.from({ length: 12 }, (_, index) => `r${String(index).padStart(2, "0")}`);
  expect(legRowFaults([leg("shard-1", [], many)])).toEqual([
    "12 rows of the selection ran on no leg: r00; r01; r02; r03; r04; r05; r06; r07; r08; r09; and 2 more",
  ]);
});

test("a row is named by its project, file and titles, and shown as the list reporter prints it", () => {
  const row = rowId(["", "stub-phone", "spec-primary.stub.e2e.ts", "a describe", "a row"]);
  expect(row).toBe('["stub-phone","spec-primary.stub.e2e.ts","a describe","a row"]');
  expect(shownRow(row)).toBe("[stub-phone] › spec-primary.stub.e2e.ts › a describe › a row");
  expect(legRowFaults([leg("shard-1", [], [row])], shownRow)).toEqual([
    "1 rows of the selection ran on no leg: [stub-phone] › spec-primary.stub.e2e.ts › a describe › a row",
  ]);
});
