/** `bun packages/dispatch/e2e/tools/leg-rows.ts <rows-dir> <leg>...`: whether the shard legs ran
 *  each row of their selection exactly once, read from the two lists each leg writes into its
 *  report's `rows/` directory (`<leg>.selection.jsonl` and `<leg>.ran.jsonl`, e2e/shard-reporter.ts).
 *
 * Who runs it: .github/scripts/dispatch-browser-reading-rows.sh, in the merge job's step "Reading
 * one, the rows" of .github/workflows/dispatch-browser.yaml, with that job's selected rows
 * directory and every shard leg the run owed (`shard-1` and on, read from the run's jobs). The
 * check itself is `legRowFaults` (e2e/leg-rows.ts), which says why the merge job's row count
 * cannot see a row no leg ran.
 *
 * It prints one `::error::` line per fault, each naming the rows and the legs, and exits 1; a leg
 * named here whose two lists are not both in the directory is a fault too, since a leg that wrote
 * nothing has shown nothing. With no fault it prints one `::notice::` line with the count and
 * exits 0. Each line also goes to $GITHUB_STEP_SUMMARY where the runner sets it.
 */
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { type LegRows, legRowFaults, shownRow } from "../leg-rows";

const usage =
  "usage: bun packages/dispatch/e2e/tools/leg-rows.ts <rows-dir> <leg>...\n" +
  "  <rows-dir>  the directory holding each leg's <leg>.selection.jsonl and <leg>.ran.jsonl\n" +
  "  <leg>       a shard leg the run owed, such as shard-1";

if (import.meta.main) {
  const [dir, ...names] = process.argv.slice(2);
  if (dir === undefined || names.length === 0) {
    console.error(usage);
    process.exit(2);
  }
  const lines = (path: string) =>
    readFileSync(path, "utf8")
      .split("\n")
      .filter((line) => line !== "");
  const faults: string[] = [];
  const legs: LegRows[] = [];
  for (const leg of names) {
    const selection = join(dir, `${leg}.selection.jsonl`);
    const ran = join(dir, `${leg}.ran.jsonl`);
    const absent = [selection, ran].filter((path) => !existsSync(path));
    if (absent.length > 0) {
      faults.push(
        `${leg} wrote no row list (${absent.join(", ")} absent), so what it ran cannot be read`
      );
      continue;
    }
    legs.push({ leg, ran: lines(ran), selection: lines(selection) });
  }
  // A leg whose lists are absent leaves the others unjudgeable as a whole: its rows would read as
  // run on no leg. So the cross-check runs only over a complete set.
  if (faults.length === 0) faults.push(...legRowFaults(legs, shownRow));
  const summary = process.env.GITHUB_STEP_SUMMARY;
  const say = (line: string) => {
    console.log(line);
    if (summary) appendFileSync(summary, `${line.replace(/^::[^:]*::/, "")}\n\n`);
  };
  if (faults.length > 0) {
    for (const fault of faults) say(`::error title=dispatch-browser leg rows::${fault}`);
    process.exit(1);
  }
  const rows = new Set(legs[0]?.selection ?? []).size;
  say(
    `::notice title=dispatch-browser leg rows::the ${legs.length} shard legs read one selection of ` +
      `${rows} rows and ran each of them on exactly one leg`
  );
}
