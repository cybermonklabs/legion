/** `bun e2e/tools/compare-recordings.ts <committed copy> [fresh]`: whether a fresh recording of
 *  every stubbed row still says what the committed one says.
 *
 *  The re-record job of `.github/workflows/dispatch-browser-nightly.yaml` runs it: it copies
 *  `e2e/__recordings__` aside, re-records every stub row against the real harness, which writes
 *  over that directory, and then calls this with the copy. A recording is never byte-identical
 *  twice, so what is compared is the comparison form of each
 *  (`canonical` in e2e/stub/recording.ts): the volatile transport erased as it is written, and
 *  the ids the server mints and the times it stamps masked here.
 *
 *  It prints one line per recording that moved, and the first few lines of the difference, and
 *  exits 1. A recording the fresh run no longer wrote at all, and one it wrote that the tree
 *  does not hold, are both named: the first is a row that no longer runs, the second a row whose
 *  recording was never committed.
 */
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { type Har, shapes } from "../stub/recording";

const usage =
  "usage: bun e2e/tools/compare-recordings.ts <committed copy> [fresh]\n" +
  "  <committed copy>  a copy of e2e/__recordings__ taken before the re-recording\n" +
  "  [fresh]           the re-recorded set; e2e/__recordings__ by default";

const [committedRoot, freshRoot] = process.argv.slice(2);
if (committedRoot === undefined) {
  console.error(usage);
  process.exit(2);
}
const fresh = freshRoot ?? fileURLToPath(new URL("../__recordings__", import.meta.url));

/** Every `.har` under `root`, by its path relative to `root`. */
function recordings(root: string): string[] {
  const found: string[] = [];
  const walk = (directory: string): void => {
    for (const name of readdirSync(directory)) {
      const path = join(directory, name);
      if (statSync(path).isDirectory()) walk(path);
      else if (name.endsWith(".har")) found.push(relative(root, path));
    }
  };
  try {
    walk(root);
  } catch {
    return [];
  }
  return found.sort();
}

/** The first lines on which two comparison forms part, which is what names the drift. */
function firstDifference(before: string, after: string): string {
  const left = before.split("\n");
  const right = after.split("\n");
  for (let line = 0; line < Math.max(left.length, right.length); line += 1) {
    if (left[line] !== right[line]) {
      return `    line ${line + 1}\n      committed: ${left[line] ?? "<end of file>"}\n      recorded:  ${right[line] ?? "<end of file>"}`;
    }
  }
  return "    the two differ in length alone";
}

const committedFiles = recordings(committedRoot);
const freshFiles = recordings(fresh);
if (committedFiles.length === 0) {
  console.error(`${committedRoot} holds no recording, so there is nothing to compare`);
  process.exit(2);
}

const moved: string[] = [];
const gone: string[] = [];
for (const name of committedFiles) {
  if (!freshFiles.includes(name)) {
    gone.push(name);
    continue;
  }
  const before = shapes(JSON.parse(readFileSync(join(committedRoot, name), "utf8")) as Har);
  const after = shapes(JSON.parse(readFileSync(join(fresh, name), "utf8")) as Har);
  // The routes both recordings hold, and nothing else. A route only one of them holds is the
  // page's own racing reads - a poll that fired once more, a prefetch that won - and reporting
  // it would make this job red every night on nothing. A route that really goes away goes away
  // from the row too, and the row then fails its own replay against the committed recording,
  // which is louder than a line here.
  for (const [asked, answered] of before) {
    const now = after.get(asked);
    if (now !== undefined && now !== answered) {
      moved.push(`  ${name}\n    ${asked}\n${firstDifference(answered, now)}`);
    }
  }
}
const uncommitted = freshFiles.filter((name) => !committedFiles.includes(name));

if (moved.length === 0 && gone.length === 0 && uncommitted.length === 0) {
  console.log(`every one of the ${committedFiles.length} committed recordings still matches`);
  process.exit(0);
}
if (moved.length > 0) {
  console.error(`${moved.length} recording(s) no longer match the server:`);
  for (const line of moved) console.error(line);
}
for (const name of gone) {
  console.error(`  ${name}: the re-recording wrote no such file, so no row writes it any more`);
}
for (const name of uncommitted) {
  console.error(`  ${name}: the re-recording wrote this, and the tree holds no committed copy`);
}
console.error(
  "\nThe re-recorded set is this run's `dispatch-browser-recordings` artifact: download it and " +
    "copy it over packages/dispatch/e2e/__recordings__ to apply it, then read the diff."
);
process.exit(1);
