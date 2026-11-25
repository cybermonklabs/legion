/** The one recursive walk that the suite's source scans read their files with. A new scan of
 *  the suite's own files calls `sourceFiles` here rather than writing a walk of its own.
 *
 * The scans differ in which directories they go down and which names they keep, and in nothing
 * else, so each passes its own pair of predicates and the walk itself is written once here. It
 * is a module of its own rather than a function one scan exports, because no scan should have
 * to import another: a scan that reads only the file system stays pure over its arguments, so
 * any process may call it, and a scan that builds a `ts.Program` loads TypeScript, which the
 * others need not load.
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
