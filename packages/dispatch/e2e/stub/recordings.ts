/** Where the stubbed layer's recordings live, and how one row's files are named. One module,
 *  because four read this layout - the fixture that replays and records
 *  (`e2e/stub/stub-test.ts`), the contract test (`e2e/__tests__/stub-recordings.test.ts`), the
 *  drift comparator (`e2e/tools/compare-recordings.ts`) and the row map
 *  (`e2e/tools/row-map.ts`) - and four copies of a path are four chances for one of them to go
 *  on reading a directory the others have left.
 */
import { existsSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import type { TestInfo } from "@playwright/test";

/** Moves the recordings a run reads or writes off the committed set. The nightly's re-record
 *  job sets it, so the fresh recordings land in an empty directory of their own and the
 *  comparison can see a recording the committed set holds and no row writes any more; left
 *  unset, which is every other run, the directory is the committed one. */
export const recordingsDirectoryVariable = "DISPATCH_E2E_STUB_RECORDINGS";

/** The committed set, which is this checkout's own `e2e/__recordings__`, with no trailing
 *  slash. A tool that compares the directory it was given with the one the tree holds names
 *  this rather than calling `recordingsDirectory` with an empty environment, which reads as a
 *  trick until a reader finds out why the argument is there. */
export const committedRecordings = fileURLToPath(new URL("../__recordings__", import.meta.url));

/** The directory one run reads its recordings from or writes them to, with no trailing slash:
 *  every caller joins a path onto it, and a slash each of them then strips again is a layout
 *  decided in four places. */
export function recordingsDirectory(env: NodeJS.ProcessEnv = process.env): string {
  const named = (env[recordingsDirectoryVariable] ?? "").trim();
  return named === "" ? committedRecordings : named.replace(/\/+$/, "");
}

/** A row's file name, from its `describe` titles and its own: lower case, every run of other
 *  characters a single hyphen, cut to 120. The listing's own title separator collapses to that
 *  one hyphen too, so a row read from `playwright test --list` and the same row running under
 *  the fixture name one file. */
export function recordingName(title: string): string {
  return title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 120);
}

/** Where one row's recording and scene live, relative to a set's root: a directory named for
 *  the project, then one named for the spec file, then a file named for the row. Two rows of
 *  one file never collide, because the name carries the `describe` titles above the row.
 *
 *  The project comes first because a row that runs in both stubbed projects - one whose claim
 *  holds at a desktop width and at a phone's - is two rows of one title, each asking the server
 *  from its own viewport. One file for both would be written twice in a recording run, by two
 *  workers at once, and replayed in whichever of the two shapes won. */
export function recordingFor(
  row: { project: string; file: string; title: string },
  extension: string
): string {
  const family = row.file.replace(/\.stub\.e2e\.ts$/, "");
  return join(row.project, family, `${recordingName(row.title)}${extension}`);
}

/** The absolute path of one running row's recording or scene, under the directory this run
 *  reads and writes. */
export function recordingPath(test: TestInfo, extension: string): string {
  return join(
    recordingsDirectory(),
    recordingFor(
      {
        file: test.titlePath[0],
        project: test.project.name,
        title: test.titlePath.slice(1).join(" "),
      },
      extension
    )
  );
}

/** The spec family a `<project>/<family>/<row>.har` belongs to, read by this module rather
 *  than by a caller splitting the path on its own. */
export function recordingFamily(path: string): string {
  return path.split("/")[1] ?? "";
}

/** Every `<project>/<family>/<row>.har` under `root`, by its path relative to `root`, sorted.
 *  A root that does not exist answers an empty list - before the first family moved there was
 *  none, and each caller says what it does with that - and every other failure is thrown, so a
 *  directory that cannot be read is not read as a tree holding nothing. */
export function recordingsUnder(root: string): string[] {
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
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT" && !existsSync(root)) return [];
    throw error;
  }
  return found.sort();
}
