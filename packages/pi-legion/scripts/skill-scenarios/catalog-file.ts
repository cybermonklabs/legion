import { readFileSync } from "node:fs";
import path from "node:path";

/** A scenario's or a case's name: lower-case words joined by single hyphens. */
const NAME = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

/** The lines of the catalog `file`, a data file of this directory that rig.sh reads too, each as
 * its names, blank lines passed over. A line that is not `form`, `most` names or fewer, or a name
 * that is not lower-case words and hyphens, is refused here, since rig.sh's `read` would take it
 * otherwise and score.ts puts each name into a pattern. */
export function catalogLines(
  file: URL,
  most: number,
  form: string
): readonly (readonly string[])[] {
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => {
      const names = line.trim().split(/\s+/);
      if (names.length > most || !names.every((name) => NAME.test(name)))
        throw new Error(`${path.basename(file.pathname)}: '${line}' is not ${form}`);
      return names;
    });
}
