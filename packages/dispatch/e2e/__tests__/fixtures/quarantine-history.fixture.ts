/** The planted history of `e2e/__tests__/quarantine-check.test.ts`: what the merge jobs of six
 *  runs said, read at `readAt`, the nightly's 02:17 on 2026-11-19. The rows and runs are planted;
 *  each message has the shape the merge job's "Reading two" step prints,
 *  `<file>:<line> [<project>] <title>`.
 *
 * - `pictures.e2e.ts [chromium] a picture uploads @pr` is flaky in two runs that count, a
 *   nightly and a merge group, at two lines, since the row moved between them.
 * - `keyed-writes.e2e.ts [webkit] … @webkit` is flaky in one run, named by two attempts of it.
 * - `deep-links.e2e.ts [firefox] … @firefox` is flaky in a merge group and in a pull request.
 * - `mermaid.e2e.ts [webkit] … @webkit` is flaky in a merge group and in a nightly of 18 days
 *   before, outside the window.
 */
import type { FixEntry, QuarantineEntry } from "../../quarantine";
import type { BlameRange, RowChange, RunReading } from "../../tools/quarantine-check";

export const readAt = new Date("2026-11-19T02:17:00Z");

const run = (id: number, event: string, createdAt: string, flakyRows: string[]): RunReading => ({
  createdAt,
  event,
  flakyRows,
  id,
  unread: 0,
  url: `https://github.com/cybermonklabs/legion/actions/runs/${id}`,
});

const pictures = "pictures.e2e.ts:412 [chromium] a picture uploads @pr";
const keyedWrites = "keyed-writes.e2e.ts:233 [webkit] an ask answer whose answer is lost @webkit";
const deepLinks = "deep-links.e2e.ts:88 [firefox] a held chunk lands @firefox";
const mermaid = "mermaid.e2e.ts:40 [webkit] a diagram draws @webkit";

export const history: readonly RunReading[] = [
  run(101, "schedule", "2026-11-08T03:02:11Z", [pictures]),
  run(102, "merge_group", "2026-11-12T15:40:07Z", [
    "pictures.e2e.ts:415 [chromium] a picture uploads @pr",
    keyedWrites,
    keyedWrites,
  ]),
  run(103, "merge_group", "2026-11-14T09:12:55Z", [deepLinks, mermaid]),
  run(104, "pull_request", "2026-11-16T11:30:00Z", [deepLinks]),
  run(105, "schedule", "2026-11-01T02:31:40Z", [mermaid]),
  run(106, "merge_group", "2026-11-18T20:05:19Z", []),
];

/** The entry that holds the pictures row, added a week before `readAt`. */
export const picturesEntry: QuarantineEntry = {
  file: "pictures.e2e.ts",
  project: "chromium",
  title: "a picture uploads @pr",
  issue: "dispatch://INFRA-2001",
  added: "2026-11-12",
};

/** An entry of 14 days at `readAt`, which is the most the quarantine allows. */
export const fourteenDays: QuarantineEntry = {
  file: "heavy-streamed-turn.e2e.ts",
  project: "webkit",
  title: "a streamed body grows under its own paint @webkit",
  issue: "dispatch://INFRA-2002",
  added: "2026-11-05",
};

/** An entry of 18 days at `readAt`. */
export const eighteenDays: QuarantineEntry = {
  file: "offline-edits.e2e.ts",
  project: "firefox",
  title: "an edit is restored across a reload @firefox",
  issue: "dispatch://INFRA-2003",
  added: "2026-11-01",
};

/** The last change of each row's lines long before the window, so every run of `history`
 *  counts: the reading of a tree whose rows have stood still. */
export const stillRows: ReadonlyMap<string, RowChange> = new Map(
  [pictures, keyedWrites, deepLinks, mermaid].map((message) => [
    message.replace(/:\d+ \[/, " ["),
    {
      committedDate: "2026-10-01T00:00:00Z",
      lines: `${message.split(":")[0]}:1-40`,
      oid: "1111111111111111111111111111111111111111",
    },
  ])
);

/** `pictures.e2e.ts` at the reading: the pictures row at line 10, the file's next row at 31, 60
 *  lines in all, so the row's own lines are 10 to 30. */
export const picturesRow = { fileLength: 60, fileRows: [10, 31], line: 10 };

const old = {
  committedDate: "2026-10-01T00:00:00Z",
  oid: "1111111111111111111111111111111111111111",
};

/** Blames of `pictures.e2e.ts`, each with one commit of 2026-11-13 - after both runs that named
 *  the pictures row, 101 of 11-08 and 102 of 11-12 - or of 11-10, between them. */
export const blames: Readonly<Record<string, readonly BlameRange[]>> = {
  ownLinesAfter: [
    { commit: old, endingLine: 9, startingLine: 1 },
    {
      commit: { committedDate: "2026-11-13T09:00:00Z", oid: "2".repeat(40) },
      endingLine: 30,
      startingLine: 10,
    },
    { commit: old, endingLine: 60, startingLine: 31 },
  ],
  elsewhereAfter: [
    { commit: old, endingLine: 30, startingLine: 1 },
    {
      commit: { committedDate: "2026-11-13T09:00:00Z", oid: "3".repeat(40) },
      endingLine: 40,
      startingLine: 31,
    },
    { commit: old, endingLine: 60, startingLine: 41 },
  ],
  ownLinesBetween: [
    { commit: old, endingLine: 19, startingLine: 1 },
    {
      commit: { committedDate: "2026-11-10T09:00:00Z", oid: "4".repeat(40) },
      endingLine: 22,
      startingLine: 20,
    },
    { commit: old, endingLine: 60, startingLine: 23 },
  ],
};

/** A fix of the pictures row. */
export const picturesFix: FixEntry = {
  file: "pictures.e2e.ts",
  project: "chromium",
  title: "a picture uploads @pr",
  issue: "dispatch://INFRA-2001",
  fixed: "5".repeat(40),
};
