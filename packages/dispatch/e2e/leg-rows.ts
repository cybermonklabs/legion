/** Whether the legs of a sharded run ran each row of their selection exactly once: the check the
 *  merge job of .github/workflows/dispatch-browser.yaml makes over the row lists each shard leg
 *  writes (`e2e/shard-reporter.ts`), and the one `e2e/__tests__/shard-split.test.ts` makes over
 *  its listings.
 *
 * Why it is needed beside the merge job's count. That job compares the rows the legs printed with
 * the rows the merged report holds, and both count only what the legs ran: a file that no leg
 * kept is missing from both, and a file two legs kept is in both twice, so the counts agree and
 * the run is green. On 2026-11-22 a planted reporter that put the selection's last file on no leg
 * ran 1,616 of the full lane's 1,617 rows, and that count read 1,616 against 1,616. What can see
 * it is the selection itself, which every leg reads whole before it keeps its own part
 * (`preprocess`), against the rows each leg was given to run (`onBegin`).
 *
 * A row is any string that names it the same way in both lists: the reporter writes
 * `JSON.stringify([project, file, ...titles])`, one a line. Nothing here imports Playwright.
 */

/** One leg's two lists: every row of the selection as the leg read it, and the rows it ran. */
export type LegRows = {
  readonly leg: string;
  readonly selection: readonly string[];
  readonly ran: readonly string[];
};

/** How many rows a fault names before it says how many more there are. */
const shownAtMost = 10;

/** A row's name in a leg's lists: its project, its file and its titles, from a Playwright
 *  `TestCase.titlePath()` (whose first entry is the root's empty title), as one JSON line. One
 *  spelling for both lists, which the reporter writes and the merge job reads. */
export function rowId(titlePath: readonly string[]): string {
  return JSON.stringify(titlePath.slice(1));
}

/** A row as Playwright's list reporter prints it, `[project] › file › titles`, for a fault. */
export function shownRow(row: string): string {
  try {
    const [project, ...rest] = JSON.parse(row) as string[];
    return [`[${project}]`, ...rest].join(" › ");
  } catch {
    return row;
  }
}

/** `entries`, at most `shownAtMost` of them, then how many are left. */
function named(entries: readonly string[]): string {
  const shown = entries.slice(0, shownAtMost).join("; ");
  return entries.length > shownAtMost
    ? `${shown}; and ${entries.length - shownAtMost} more`
    : shown;
}

/** Every fault of the legs' lists, each a sentence naming the rows and legs it is about; none
 *  when every leg read the same selection and each of its rows ran on exactly one leg. `show`
 *  turns a row into what a reader reads. No legs at all is a fault: a check that read nothing has
 *  not passed. */
export function legRowFaults(
  legs: readonly LegRows[],
  show: (row: string) => string = (row) => row
): string[] {
  if (legs.length === 0)
    return ["no shard leg's row lists were read, so no row can be said to have run"];
  const faults: string[] = [];
  const [first] = legs as [LegRows, ...LegRows[]];
  const selection = new Set(first.selection);
  for (const leg of legs.slice(1)) {
    const theirs = new Set(leg.selection);
    const onlyTheirs = [...theirs].filter((row) => !selection.has(row)).sort();
    const onlyFirst = [...selection].filter((row) => !theirs.has(row)).sort();
    if (onlyTheirs.length > 0 || onlyFirst.length > 0) {
      faults.push(
        `${leg.leg} read a selection of ${theirs.size} rows and ${first.leg} one of ${selection.size}, ` +
          `so the legs placed different selections: only ${leg.leg}'s holds ` +
          `${onlyTheirs.length > 0 ? named(onlyTheirs.map(show)) : "nothing"}, only ${first.leg}'s holds ` +
          `${onlyFirst.length > 0 ? named(onlyFirst.map(show)) : "nothing"}`
      );
    }
  }
  const ranOn = new Map<string, string[]>();
  for (const leg of legs) {
    for (const row of new Set(leg.ran)) ranOn.set(row, [...(ranOn.get(row) ?? []), leg.leg]);
  }
  const onNoLeg = [...selection].filter((row) => !ranOn.has(row)).sort();
  if (onNoLeg.length > 0) {
    faults.push(
      `${onNoLeg.length} rows of the selection ran on no leg: ${named(onNoLeg.map(show))}`
    );
  }
  const onTwo = [...ranOn].filter(([, on]) => on.length > 1).sort(([a], [b]) => (a < b ? -1 : 1));
  if (onTwo.length > 0) {
    faults.push(
      `${onTwo.length} rows ran on more than one leg: ` +
        named(onTwo.map(([row, on]) => `${show(row)} (on ${on.join(", ")})`))
    );
  }
  const outside = [...ranOn.keys()].filter((row) => !selection.has(row)).sort();
  if (outside.length > 0) {
    faults.push(
      `${outside.length} rows ran that the selection does not hold: ${named(outside.map(show))}`
    );
  }
  return faults;
}
