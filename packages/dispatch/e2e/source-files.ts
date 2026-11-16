/** The one recursive walk the suite's two source scans read their files with: `suiteFiles` of
 *  `e2e/fixed-waits.ts` and `specFiles` of `e2e/no-browser-rows.ts`.
 *
 * The two differ in which directories they go down and which names they keep, and in nothing
 * else, so each passes its own pair of predicates and the walk itself is written once here. It
 * is a module of its own rather than a function either scan exports, because neither should
 * have to import the other: `e2e/fixed-waits.ts` reads the file system and is otherwise pure
 * over its arguments, so any process may call it, and `e2e/no-browser-rows.ts` builds a
 * `ts.Program`.
 *
 * This module reads directories and nothing else: no environment, no network and no server.
 */
import { readdirSync } from "node:fs";
import { join } from "node:path";

/** What one walk keeps, each question asked of an entry's own name rather than of its path. */
export type FileFilter = {
  /** Whether the walk goes down a directory of this name. */
  readonly directory: (name: string) => boolean;
  /** Whether a file of this name is one of the caller's. */
  readonly file: (name: string) => boolean;
};

/** Every file under `directory` that `filter` keeps, relative to it, `/`-joined whatever this
 *  platform's separator is, and sorted. `readdirSync` is called on `directory` and on every
 *  directory below it the filter goes down, so one that cannot be read throws here rather than
 *  coming back as a short list. */
export function sourceFiles(directory: string, filter: FileFilter): string[] {
  const files: string[] = [];
  const walk = (relative: string): void => {
    for (const entry of readdirSync(join(directory, relative), { withFileTypes: true })) {
      const path = relative === "" ? entry.name : `${relative}/${entry.name}`;
      if (entry.isDirectory()) {
        if (filter.directory(entry.name)) walk(path);
      } else if (entry.isFile() && filter.file(entry.name)) {
        files.push(path);
      }
    }
  };
  walk("");
  return files.sort();
}
