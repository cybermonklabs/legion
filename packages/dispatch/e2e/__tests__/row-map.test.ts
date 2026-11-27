/** This test generates the row map itself on each run. It reads no map from a file.
 *
 *  `e2e/tools/row-map.ts` generates the map, and the test reads the map from the tool's output.
 *  Git does not keep `e2e/row-map.md`. A map that drifts from the suite is worse than none: it
 *  reads as a current account of what moved and what did not, so a row added, moved or tagged
 *  since would be covered by a sentence nobody wrote about it. A map generated on each run does
 *  not drift.
 *
 *  The test fails on each refusal of the tool. Among the refusals are:
 *
 *  - A note for a row that does not exist.
 *  - A recording that no row owns.
 *  - A real-stack file with no reason.
 *  - A reason for a file with no real-stack row.
 *
 *  One row holds the header rule: a real-stack file that the list does not name gives its reason
 *  in its own header, as a stub file does.
 */

import { expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { movedRowNotes, unchangedByTheMove } from "../stub/moved-row-notes";
import { headerReasons } from "../stub/real-stack-reasons";
import { rowLimit, runBounded } from "./bounded-subprocess";
import { retained } from "./playwright-listing";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** The generation of the map. It starts once, when this file loads, and each row that reads the
 *  map awaits it. `bunfig.toml` runs the rows in random order, so no row depends on another. */
const generation = retained(
  runBounded({
    argv: ["bun", "e2e/tools/row-map.ts"],
    bound: "generation",
    cwd: dispatchPackage,
    // The generation lists the config in a subprocess of its own, and a listing runs no
    // `globalSetup`, so it starts no stack and nothing of this shell's environment reaches it.
    env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "" },
    from: import.meta.url,
    // The two listings are bounded children of the generation, so their cost lines are in its
    // stderr. This brings them up to the reader rather than losing them with the rest of it.
    relaysLines: true,
  })
);

/** Each refusal of the tool stops it with a non-zero code and its reason on stderr, so this row
 *  fails on each refusal and shows the reason. */
test(
  "a fresh generation of the row map refuses nothing",
  async () => {
    const { code, named, stderr } = await generation;
    expect(code, `the generation ${named} exited ${code}:\n${stderr}`).toBe(0);
  },
  rowLimit("generation")
);

/** A row the map says is unchanged is a row no reviewer looks at, so a row whose assertions
 *  did move must carry its own note and not that sentence. The generation writes one or the
 *  other. This row holds the map that the generation prints to that rule, which is the half a
 *  reader of k2 depends on. */
test(
  "a row with a note carries it in the map, and is not called unchanged",
  async () => {
    const { code, named, stderr, stdout } = await generation;
    expect(code, `the generation ${named} exited ${code}:\n${stderr}`).toBe(0);
    const lines = stdout.split("\n").filter((line) => line.startsWith("| `e2e/"));
    expect(lines.length, "the map's moved table is empty").toBeGreaterThan(0);
    for (const [key, note] of Object.entries(movedRowNotes)) {
      // The first separator only: a row inside a `describe` carries more of them in its title.
      const separator = key.indexOf(" › ");
      const file = key.slice(0, separator);
      const title = key.slice(separator + " › ".length);
      const row = lines.find(
        (line) => line.startsWith(`| \`e2e/${file}\` | ${title} |`) && !line.includes(" | --- |")
      );
      expect(row, `the map holds no moved row for ${key}`).toBeDefined();
      expect(row, `${key}: the map does not carry this row's note`).toContain(note);
      expect(
        row?.includes(unchangedByTheMove),
        `${key}: the map calls this row unchanged while e2e/stub/moved-row-notes.ts says its ` +
          "assertions moved"
      ).toBe(false);
    }
  },
  rowLimit("generation")
);

/** One file of the header case: its path under the directory, its text, and the reason that
 *  `headerReasons` gives for it. A file with no `reason` gives none. */
type HeaderRow = { readonly file: string; readonly text: string; readonly reason?: string };

const headerRows: readonly HeaderRow[] = [
  {
    file: "new.e2e.ts",
    text: [
      "/** Its rows assert what the server stores, and the list does not name it.",
      " *  The second line of the header joins the first. */",
      'import { test } from "@playwright/test";',
    ].join("\n"),
    reason:
      "Its rows assert what the server stores, and the list does not name it. The second line " +
      "of the header joins the first.",
  },
  {
    file: "sub/nested.e2e.ts",
    text: "/** A file one directory down gives its reason too. */\n",
    reason: "A file one directory down gives its reason too.",
  },
  // A stub file's header is its stub reason. The listing names no multiprocess file.
  { file: "stubbed.stub.e2e.ts", text: "/** Its rows read recordings. */\n" },
  { file: "two.multiprocess.e2e.ts", text: "/** Its rows start two stacks. */\n" },
  // The list gives this file its reason, and the list wins over the header.
  { file: "listed.e2e.ts", text: "/** A header that the list overrules. */\n" },
  { file: "bare.e2e.ts", text: 'import { test } from "@playwright/test";\n' },
  // A comment that does not start at the first byte is no header.
  {
    file: "late.e2e.ts",
    text: 'import { test } from "@playwright/test";\n/** A comment after an import. */\n',
  },
  // A package's own files are not the suite's files.
  { file: "node_modules/package/dep.e2e.ts", text: "/** A file of a package. */\n" },
];

test("a real-stack file the list does not name gives its reason in its own header, as a stub file does", () => {
  const directory = mkdtempSync(join(tmpdir(), "row-map-"));
  try {
    for (const { file, text } of headerRows) {
      const path = join(directory, file);
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, text);
    }
    const expected = Object.fromEntries(
      headerRows.flatMap(({ file, reason }) => (reason === undefined ? [] : [[file, reason]]))
    );
    expect(headerReasons(directory, { "listed.e2e.ts": "the reason the list gives" })).toEqual(
      expected
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
