/**
 * Patterns of Go's white space, the set Dispatch trims a value by (`strings.TrimSpace`), for the
 * `dispatch` command's flags and URL paths. The set itself is `GO_SPACE` of
 * `@legion/contracts/go-blank`, written out once there beside `goBlankPattern`, whose comment says
 * what it holds, where JavaScript's `trim` differs from it, and which tests hold it to Go.
 */
import { GO_SPACE } from "@legion/contracts/go-blank";

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
