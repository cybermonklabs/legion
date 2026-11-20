/** `bun e2e/tools/compare-recordings.ts <fresh>`: whether a fresh recording of every stubbed
 *  row still says what the committed one says.
 *
 *  The `recordings` job of `.github/workflows/dispatch-browser.yaml` runs it on the full lane,
 *  and a person re-recording the set runs it too (`packages/dispatch/README.md`). Both point the
 *  record mode at an empty directory of their own (`DISPATCH_E2E_STUB_RECORDINGS`,
 *  `e2e/stub/recordings.ts`) rather than letting it write over the committed set, so this can
 *  see a recording the tree holds that no row writes any more - which it could not when the
 *  fresh set was the committed set.
 *
 *  What it compares is the status of each route, and only that: `statusDrift`
 *  (`e2e/stub/recording.ts`) is the whole of the judgement and its header says why, and
 *  `e2e/__tests__/stub-recording.test.ts` is where that judgement is read.
 *
 *  It prints one line for each recording either set holds, named by its path under the set:
 *  `CHECK <recording>: PASS` where the fresh one answers as the committed one holds, and
 *  `CHECK <recording>: FAIL: <reason>` for each of three findings, any of which exits 1:
 *
 *  - a route whose status changed, named with both answers;
 *  - a recording the tree holds that the fresh run wrote nothing for, which is a row that no
 *    longer runs or no longer reads what it read;
 *  - a recording the fresh run wrote that the tree does not hold, which is a row whose
 *    recording was never committed.
 */
import { readFileSync, realpathSync } from "node:fs";
import { join, resolve } from "node:path";

import { type Har, statusDrift } from "../stub/recording";
import { committedRecordings, recordingsUnder } from "../stub/recordings";

const usage =
  "usage: bun e2e/tools/compare-recordings.ts <fresh>\n" +
  "  <fresh>  the directory the re-recording wrote, which must not be the committed set";

const fresh = process.argv[2];
if (fresh === undefined) {
  console.error(usage);
  process.exit(2);
}

/** One directory, however it is spelled. The guard below is defeated by any spelling that is
 *  not the committed set's own absolute path - a relative one, a trailing slash, a `..`, a
 *  symlink - and the relative one is what a person types, because every command of this
 *  package is written from the package root and the tool's own failure message names
 *  `packages/dispatch/e2e/__recordings__`. `realpathSync` answers for a directory that exists,
 *  which both sides of a real self-comparison are; a `fresh` that does not exist resolves
 *  against the working directory instead, and the refusal below it reports that it holds no
 *  recording. */
function directoryIdentity(path: string): string {
  const absolute = resolve(path);
  try {
    return realpathSync(absolute);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return absolute;
    throw error;
  }
}

// The committed set is this checkout's, read through the one definition of that layout. The
// fresh set is the caller's directory, and it must not be the same one: a recording compared
// with itself can never differ, which is how the vanished-recording finding below went
// unreachable until a review read it.
if (directoryIdentity(fresh) === directoryIdentity(committedRecordings)) {
  console.error(
    `${fresh} is the committed set itself, so every comparison would be a recording against ` +
      "itself. Point the re-recording at a directory of its own with " +
      "DISPATCH_E2E_STUB_RECORDINGS and pass that directory here."
  );
  process.exit(2);
}

const committedFiles = recordingsUnder(committedRecordings);
const freshFiles = recordingsUnder(fresh);
if (committedFiles.length === 0) {
  console.error(`${committedRecordings} holds no recording, so there is nothing to compare`);
  process.exit(2);
}
if (freshFiles.length === 0) {
  console.error(`${fresh} holds no recording, so the re-recording wrote nothing`);
  process.exit(2);
}

const read = (root: string, name: string): Har =>
  JSON.parse(readFileSync(join(root, name), "utf8")) as Har;

const committedSet = new Set(committedFiles);
const freshSet = new Set(freshFiles);

/** Why one recording fails the comparison, or nothing where the fresh one answers as the
 *  committed one holds. */
function finding(name: string): string | undefined {
  if (!freshSet.has(name)) {
    return "the re-recording wrote no such file, so no row writes it any more";
  }
  if (!committedSet.has(name)) {
    return "the re-recording wrote this, and the tree holds no committed copy";
  }
  const moved = statusDrift(read(committedRecordings, name), read(fresh, name));
  if (moved.length === 0) return undefined;
  return `${moved.length} route(s) no longer answer as the recording holds: ${moved.join("; ")}`;
}

const uncommitted = freshFiles.filter((name) => !committedSet.has(name));
let failed = 0;
for (const name of [...committedFiles, ...uncommitted]) {
  const reason = finding(name);
  if (reason === undefined) {
    console.log(`CHECK ${name}: PASS`);
  } else {
    failed += 1;
    console.log(`CHECK ${name}: FAIL: ${reason}`);
  }
}

if (failed === 0) {
  console.log(
    `every one of the ${committedFiles.length} committed recordings still answers as it holds`
  );
  process.exit(0);
}
console.error(
  `\n${failed} of the ${committedFiles.length + uncommitted.length} recordings failed. ` +
    "The re-recorded set is this run's `dispatch-browser-recordings` artifact. Replace " +
    "packages/dispatch/e2e/__recordings__ with it - delete the directory first and put the " +
    "artifact in its place - and read the diff. A copy over the directory would leave behind " +
    "every recording named above as written by no row any more, and the next run would report " +
    "them again.\n\nRead the recording step first, because this comparison runs even where it " +
    "failed, and a row that failed leaves one of two things. A row that threw while seeding " +
    "wrote no recording at all, so it is named above as one no row writes any more while its " +
    "committed copy is sound. A row that threw in its body wrote one all the same - record " +
    "mode writes the file after the row for every outcome but a skip - so that recording " +
    "holds whatever the row reached before it failed. In either case fix the row and record " +
    "again rather than replacing the directory, which would delete the first and commit the " +
    "second."
);
process.exit(1);
