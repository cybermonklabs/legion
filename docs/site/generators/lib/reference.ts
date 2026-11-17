// Shared by the generators that read a reference table from a binary this repository builds.
// scripts/generate.ts puts the binaries scripts/build-binaries.sh built from this commit first on
// PATH. To run such a generator alone, build the binaries into a directory and put that first on
// PATH:
// `docs/site/scripts/build-binaries.sh <dir> && PATH=<dir>:$PATH bun <generator> <out>`.
import { inline } from "./markdown.ts";

/** The vocabulary a table row's `required` is written in, the Dispatch settings table's
 *  (packages/envoy/cmd/dispatch/settings.go), which Legion's keytable.go holds itself to as well. */
export const REQUIRED = /^(yes|no|when .+)$/;

/** A `required` or a `default` as a sentence of the page: its first character capitalised, and
 *  escaped for where it goes — a table cell when `cell`, running prose otherwise. */
export function capitalized(text: string, cell = false): string {
  return inline(
    text.replace(/^./, (first) => first.toUpperCase()),
    cell
  );
}

/** Runs `<binary> <args…>` from PATH and returns the rows of the JSON it prints: the document
 *  itself when that is an array, else the array it holds under `rows`, which also names the rows
 *  in a message. It refuses a missing binary, a non-zero exit, no rows, and a row without a
 *  non-empty string in each of `fields`; each generator checks the rest of its rows' shape
 *  itself, and one that checks every field of every row passes no `fields` at all. */
export function readTable(
  binary: string,
  args: readonly string[],
  rows: string,
  fields: readonly string[]
): Record<string, unknown>[] {
  const path = Bun.which(binary);
  if (path === null) {
    throw new Error(
      `${binary} is not on PATH: run this generator through docs/site/scripts/generate.ts, which builds it (scripts/build-binaries.sh) and puts it there`
    );
  }
  const command = `${binary} ${args.join(" ")}`;
  const run = Bun.spawnSync([path, ...args], { stdout: "pipe", stderr: "pipe" });
  if (run.exitCode !== 0) {
    throw new Error(`${command} exited ${run.exitCode}:\n${run.stderr.toString()}`);
  }
  const printed = JSON.parse(run.stdout.toString()) as unknown;
  const table = Array.isArray(printed) ? printed : (printed as Record<string, unknown>)[rows];
  if (!Array.isArray(table) || table.length === 0) {
    throw new Error(`${command} printed no ${rows}`);
  }
  for (const row of table) {
    for (const field of fields) {
      if (typeof row?.[field] !== "string" || row[field] === "") {
        throw new Error(`${command}: ${JSON.stringify(row)} has no ${field}`);
      }
    }
  }
  return table;
}
