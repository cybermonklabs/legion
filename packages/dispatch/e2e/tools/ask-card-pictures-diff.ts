/** `bun e2e/tools/ask-card-pictures-diff.ts <built dir> <demo dir> <out dir>`: lays the ask
 *  card's pictures taken from a build (`ask-card-pictures.e2e.ts`, run somewhere that had no copy
 *  of the demo's pictures) beside the demo's, with each one's share of changed pixels, as the spec
 *  does where it is given the demo's directory. It writes into `<out dir>` each pair, the
 *  difference between them and `report.json` and `report.md`, and prints the report. */
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { PICTURES } from "../ask-card-fixtures";
import { comparePictures } from "../ask-card-pictures-diff";

const [built, demo, out] = process.argv.slice(2);
if (built === undefined || demo === undefined || out === undefined) {
  throw new Error("name the build's pictures, the demo's and where to write: <built> <demo> <out>");
}
comparePictures(PICTURES, built, demo, out);
console.log(readFileSync(join(out, "report.md"), "utf8"));
