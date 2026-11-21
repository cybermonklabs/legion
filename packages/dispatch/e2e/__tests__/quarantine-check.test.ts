/** The nightly's half of the quarantine, `e2e/tools/quarantine-check.ts`, driven through its pure
 *  part with the planted history of `fixtures/quarantine-history.fixture.ts`: what `decide`
 *  names, the lines `report` gives for each, and how a row's own last change and its fix move
 *  what counts, and the refusal `blame` gives an answer that holds no blame, read from a
 *  stand-in for GitHub. Nothing here reads GitHub or the clock; the moment of the reading is the
 *  fixture's. */
import { expect, test } from "bun:test";

import { entryLines } from "../quarantine";
import {
  blame,
  decide,
  type FixReading,
  lastChange,
  type RowChange,
  report,
  rowLines,
} from "../tools/quarantine-check";
import {
  blames,
  eighteenDays,
  fourteenDays,
  history,
  picturesEntry,
  picturesFix,
  picturesRow,
  readAt,
  stillRows,
} from "./fixtures/quarantine-history.fixture";

const pictures = "pictures.e2e.ts [chromium] a picture uploads @pr";

/** The rows of `stillRows` with the pictures row's last change read from `ranges`, as the
 *  adapter reads it: its own lines from the listing, their newest commit from the blame. */
function withPicturesBlame(ranges: (typeof blames)[string]): Map<string, RowChange> {
  const { end, start } = rowLines(picturesRow.line, picturesRow.fileRows, picturesRow.fileLength);
  const commit = lastChange(ranges, start, end);
  if (commit === undefined) throw new Error("the planted blame holds none of the row's lines");
  return new Map([
    ...stillRows,
    [pictures, { ...commit, lines: `pictures.e2e.ts:${start}-${end}` }],
  ]);
}

const named = (changes: ReadonlyMap<string, RowChange>, fixes: readonly FixReading[] = []) =>
  decide(history, [], fixes, changes, readAt).toQuarantine.map(({ row, runs }) => [
    `${row.file} ${row.title}`,
    runs.map((run) => run.id),
  ]);

test("a row flaky in two runs of the fourteen days is named, with its runs and the lines to add", () => {
  const decision = decide(history, [], [], stillRows, readAt);
  expect(decision.toQuarantine.map(({ row, runs }) => [row, runs.map((run) => run.id)])).toEqual([
    [{ file: "pictures.e2e.ts", project: "chromium", title: "a picture uploads @pr" }, [101, 102]],
  ]);
  expect(decision.overdue).toEqual([]);
  const { annotations, failed, summary } = report(decision, readAt);
  expect(failed).toBe(true);
  expect(summary).toContain(
    "- `pictures.e2e.ts [chromium] a picture uploads @pr`, flaky in 2 runs after its own lines " +
      "(pictures.e2e.ts:1-40) last changed, in 1111111111111111111111111111111111111111 at " +
      "2026-10-01T00:00:00Z: [run 101](https://github.com/cybermonklabs/legion/actions/runs/101) " +
      "of 2026-11-08 (schedule), [run 102](https://github.com/cybermonklabs/legion/actions/runs/102) " +
      "of 2026-11-12 (merge_group)."
  );
  const lines = summary.slice(summary.indexOf("  ```ts") + 1, summary.indexOf("  ```ts") + 8);
  expect(lines).toEqual([
    "    {",
    '      file: "pictures.e2e.ts",',
    '      project: "chromium",',
    '      title: "a picture uploads @pr",',
    '      issue: "dispatch://<KEY>",',
    '      added: "2026-11-19",',
    "    },",
  ]);
  expect(annotations).toEqual([
    "::error title=dispatch-browser quarantine::pictures.e2e.ts [chromium] a picture uploads " +
      "@pr was flaky in 2 runs of 14 days and is not quarantined: " +
      "https://github.com/cybermonklabs/legion/actions/runs/101 " +
      "https://github.com/cybermonklabs/legion/actions/runs/102",
  ]);
});

test("the same history names nothing once that row is quarantined", () => {
  const decision = decide(history, [picturesEntry], [], stillRows, readAt);
  expect(decision.toQuarantine).toEqual([]);
  const { annotations, failed, summary } = report(decision, readAt);
  expect(failed).toBe(false);
  expect(annotations).toEqual([]);
  expect(summary).toContain("No entry has been held longer than 14 days.");
});

// `keyed-writes.e2e.ts`'s row is named by two attempts of run 102 and by no other run, and
// `mermaid.e2e.ts`'s by run 103 and by a nightly 18 days before the reading: each is one run.
test("a row flaky in one run is not named, however many attempts of it named the row", () => {
  const decision = decide(history, [picturesEntry], [], stillRows, readAt);
  expect(decision.toQuarantine).toEqual([]);
  expect(decision.counted.map((run) => run.id)).toEqual([101, 102, 103, 106]);
});

// `deep-links.e2e.ts`'s row is flaky in merge group 103 and in pull request 104. The control is
// the same history with 104 a merge group, where the row is named: the event is what kept it out.
test("a pull request's flaky row does not count", () => {
  const asMergeGroup = history.map((run) =>
    run.id === 104 ? { ...run, event: "merge_group" } : run
  );
  expect(
    decide(asMergeGroup, [picturesEntry], [], stillRows, readAt).toQuarantine.map(
      ({ row, runs }) => [`${row.file} ${row.title}`, runs.map((run) => run.id)]
    )
  ).toEqual([["deep-links.e2e.ts a held chunk lands @firefox", [103, 104]]]);
});

// The row's own lines are 10 to 30: from its line to the line before the file's next row.
test("a row flaky twice before its own lines changed is not named, and the runs left out say why", () => {
  const changes = withPicturesBlame(blames.ownLinesAfter);
  expect(named(changes)).toEqual([]);
  const decision = decide(history, [], [], changes, readAt);
  expect(decision.leftOut.map(({ run, why }) => [run.id, why])).toEqual([
    [
      101,
      `created before its own lines last changed, in ${"2".repeat(40)} at 2026-11-13T09:00:00Z`,
    ],
    [
      102,
      `created before its own lines last changed, in ${"2".repeat(40)} at 2026-11-13T09:00:00Z`,
    ],
  ]);
  expect(report(decision, readAt).summary).toContain(
    "- `pictures.e2e.ts [chromium] a picture uploads @pr` in " +
      "[run 101](https://github.com/cybermonklabs/legion/actions/runs/101) of 2026-11-08 " +
      `(schedule): created before its own lines last changed, in ${"2".repeat(40)} at ` +
      "2026-11-13T09:00:00Z."
  );
});

test("a row flaky twice after a change elsewhere in its file is named", () => {
  expect(named(withPicturesBlame(blames.elsewhereAfter))).toEqual([
    ["pictures.e2e.ts a picture uploads @pr", [101, 102]],
  ]);
});

test("a row flaky once before its own lines changed and once after is not named", () => {
  const changes = withPicturesBlame(blames.ownLinesBetween);
  expect(named(changes)).toEqual([]);
  expect(decide(history, [], [], changes, readAt).leftOut.map(({ run }) => run.id)).toEqual([101]);
});

test("a row flaky twice before its fix is not named, and flaky twice after it is", () => {
  expect(named(stillRows, [{ committedDate: "2026-11-13T09:00:00Z", entry: picturesFix }])).toEqual(
    []
  );
  expect(named(stillRows, [{ committedDate: "2026-11-06T09:00:00Z", entry: picturesFix }])).toEqual(
    [["pictures.e2e.ts a picture uploads @pr", [101, 102]]]
  );
});

test("a fix whose commit is not on main fails the check", () => {
  const decision = decide(
    history,
    [picturesEntry],
    [{ committedDate: undefined, entry: picturesFix }],
    stillRows,
    readAt
  );
  expect(decision.offMain).toEqual([picturesFix]);
  const { annotations, failed } = report(decision, readAt);
  expect(failed).toBe(true);
  expect(annotations).toEqual([
    "::error title=dispatch-browser quarantine::the fix of pictures.e2e.ts [chromium] a picture " +
      `uploads @pr, ${"5".repeat(40)}, is not on main`,
  ]);
});

test("a fix older than fourteen days is named for its owner to delete, and fails nothing", () => {
  const decision = decide(
    history,
    [picturesEntry],
    [{ committedDate: "2026-10-20T09:00:00Z", entry: picturesFix }],
    stillRows,
    readAt
  );
  expect(decision.spent).toEqual([{ committedDate: "2026-10-20T09:00:00Z", entry: picturesFix }]);
  const { failed, summary } = report(decision, readAt);
  expect(failed).toBe(false);
  expect(summary).toContain('      fixed: "5555555555555555555555555555555555555555",');
});

test("an entry held longer than fourteen days is named with its issue and the lines to remove", () => {
  const decision = decide(
    history,
    [picturesEntry, fourteenDays, eighteenDays],
    [],
    stillRows,
    readAt
  );
  expect(decision.overdue).toEqual([{ days: 18, entry: eighteenDays }]);
  const { annotations, failed, summary } = report(decision, readAt);
  expect(failed).toBe(true);
  const lines = summary.slice(summary.indexOf("  ```ts") + 1, summary.indexOf("  ```ts") + 8);
  expect(lines).toEqual([
    "    {",
    '      file: "offline-edits.e2e.ts",',
    '      project: "firefox",',
    '      title: "an edit is restored across a reload @firefox",',
    '      issue: "dispatch://INFRA-2003",',
    '      added: "2026-11-01",',
    "    },",
  ]);
  expect(annotations).toEqual([
    "::error title=dispatch-browser quarantine::offline-edits.e2e.ts [firefox] an edit is " +
      "restored across a reload @firefox has been quarantined 18 days, more than 14; its issue " +
      "is dispatch://INFRA-2003",
  ]);
});

test("flaky rows a run counted and no row notice named are reported as unread", () => {
  const short = history.map((run) => (run.id === 106 ? { ...run, unread: 3 } : run));
  const { annotations } = report(decide(short, [picturesEntry], [], stillRows, readAt), readAt);
  expect(annotations).toEqual([
    "::warning title=dispatch-browser quarantine::run 106 counts 3 flaky rows that no row " +
      "notice names, and they are not counted",
  ]);
});

// Biome breaks a property after its key where the line would pass 100 columns, so a long title
// is two lines of e2e/quarantine.ts. The lines `report` names to add or to remove are the lines
// the file holds, which `e2e/__tests__/quarantine.test.ts` checks of every real entry.
test("an entry's lines are the layout Biome keeps, a long title on the line after its key", () => {
  const title =
    "a person's Close withdraws the issue's asks, an earlier version keeps its open block, and " +
    "Reopen brings the asks back @pr";
  expect(entryLines({ ...picturesEntry, title })).toEqual([
    "  {",
    '    file: "pictures.e2e.ts",',
    '    project: "chromium",',
    "    title:",
    `      "${title}",`,
    '    issue: "dispatch://INFRA-2001",',
    '    added: "2026-11-12",',
    "  },",
  ]);
});

// GitHub answers a 404 or a 422 with no body, which the client reads as no answer at all
// (`undefined`), and the refusal must still name the file and what was answered.
test("a blame GitHub gives no answer for is refused, naming the file", async () => {
  await expect(
    blame({ graphql: async () => undefined }, "cybermonklabs/legion", "main", "e2e/pictures.e2e.ts")
  ).rejects.toThrow(
    "GitHub answered no blame of e2e/pictures.e2e.ts at main (null), so the rows of that file " +
      "cannot be read as they stand: the quarantine was not checked."
  );
});
