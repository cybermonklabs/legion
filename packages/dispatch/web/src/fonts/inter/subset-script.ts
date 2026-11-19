import { readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * subset.sh's values as the checks read them: the font file's SHA-256 and the characters it keeps
 * (UNICODES), which `subset.test.ts` holds the shipped file and the face's `unicode-range` to and
 * `features/issue-page/page-typeface.test.ts` holds the page's own text to. One other check reads
 * the script itself: `main`'s `features/issue-page/shared-controls.tooltips.test.tsx`, in its case
 * "inside NoTooltips no shared control draws a character the page's typeface lacks", with its own
 * pattern for an entry, which takes four to six hex digits where `subsetRanges` takes one to six.
 */

const subsetSh = readFileSync(join(import.meta.dir, "subset.sh"), "utf8");

/** The value of subset.sh's `name=` line. */
export function subsetScriptValue(name: string): string {
  const match = new RegExp(`^${name}=(\\S+)$`, "m").exec(subsetSh);
  if (match === null) {
    throw new Error(`subset.sh has no ${name}= line`);
  }
  return match[1];
}

/** The code points UNICODES keeps, as each entry's first and last: an entry is `U+XXXX` or
 *  `U+XXXX-YYYY`, and one of any other form, such as a wildcard, is refused rather than read as
 *  something it does not say. */
export function subsetRanges(): (readonly [first: number, last: number])[] {
  return subsetScriptValue("UNICODES")
    .split(",")
    .map((entry) => {
      const match = /^U\+([0-9A-F]{1,6})(?:-([0-9A-F]{1,6}))?$/i.exec(entry.trim());
      if (match === null) {
        throw new Error(`subset.sh's UNICODES has an entry this reads no range from: ${entry}`);
      }
      const first = Number.parseInt(match[1], 16);
      return [first, match[2] === undefined ? first : Number.parseInt(match[2], 16)] as const;
    });
}
