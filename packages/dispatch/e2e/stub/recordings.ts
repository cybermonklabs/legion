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

/** The directory one run reads its recordings from or writes them to, with a trailing slash. */
export function recordingsDirectory(env: NodeJS.ProcessEnv = process.env): string {
  const named = (env[recordingsDirectoryVariable] ?? "").trim();
  if (named !== "") return named.endsWith("/") ? named : `${named}/`;
  return fileURLToPath(new URL("../__recordings__/", import.meta.url));
}

/** Where one row's recording and scene live: a directory named for the project, then one named
 *  for the spec file, then a file named for the row. The row's name is its `describe` titles
 *  and its own, which is what Playwright reports and what the row map reads, so two rows of one
 *  file never collide.
 *
 *  The project comes first because a row that runs in both stubbed projects - one whose claim
 *  holds at a desktop width and at a phone's - is two rows of one title, each asking the server
 *  from its own viewport. One file for both would be written twice in a recording run, by two
 *  workers at once, and replayed in whichever of the two shapes won. */
export function recordingPath(test: TestInfo, extension: string): string {
  const spec = test.titlePath[0].replace(/\.stub\.e2e\.ts$/, "");
  const row = test.titlePath
    .slice(1)
    .join(" ")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 120);
  return join(recordingsDirectory(), test.project.name, spec, `${row}${extension}`);
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
