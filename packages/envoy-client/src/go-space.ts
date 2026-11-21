import { goBlankPattern } from "@legion/contracts/go-blank";

/**
 * Go's white space, the set Dispatch trims a value by (`strings.TrimSpace`), as the contents of a
 * character class: the class `goBlankPattern` is written with (`@legion/contracts/go-blank`). Its
 * comment names the characters, the two on which JavaScript's `trim` differs from Go, and the
 * tests that hold the set to Go's own `unicode.IsSpace`. The set is read from that pattern, so it
 * is written out in one place.
 */
const GO_SPACE = goSpaceClass();

/** The character class of `goBlankPattern`, which is written `^[…]*$`. */
function goSpaceClass(): string {
  const set = /^\^\[(.+)\]\*\$$/u.exec(goBlankPattern.source)?.[1];
  if (set === undefined) {
    throw new Error(
      `goBlankPattern (@legion/contracts/go-blank) is no longer one character class: ${goBlankPattern.source}`
    );
  }
  return set;
}

/** Go's white space at either end of a text, which `strings.TrimSpace` takes away. */
export const goSpaceAtEnds = new RegExp(`^[${GO_SPACE}]+|[${GO_SPACE}]+$`, "gu");

/**
 * A value that names nothing in a URL path: Go's white space alone, or, inside such white space,
 * only dots and slashes. Empty, it leaves an empty step (`POST /api/v1/comments//resolve`); `.`
 * and `..` are steps the URL parser drops (`/api/v1/agents/../answers` is `/api/v1/answers`);
 * and `./`, `../`, `.//` or `/` is what a script writes for `"./$X"` with an empty `X`, which a
 * server that reads the path decoded takes as no step or a step up.
 */
export const pathNamesNothingPattern = new RegExp(`^[${GO_SPACE}]*[./]*[${GO_SPACE}]*$`, "u");
