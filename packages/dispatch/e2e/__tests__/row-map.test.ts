/** The committed row map is the one a fresh generation writes.
 *
 *  `e2e/row-map.md` is what dispatch://INFRA-1407's k2 is read from and what the pull request
 *  links, and it is generated rather than written (`e2e/tools/row-map.ts`). A map that drifts
 *  from the suite is worse than none: it reads as a current account of what moved and what did
 *  not, so a row added, moved or tagged since would be covered by a sentence nobody wrote about
 *  it. This fails on exactly that, and the fix is one command.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { movedRowNotes, unchangedByTheMove } from "../stub/moved-row-notes";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const mapPath = fileURLToPath(new URL("../row-map.md", import.meta.url));

/** The generation reads a `playwright test --list` of the whole config, which costs about five
 *  seconds on a two-core runner; a minute and a half is the ceiling at which something is wrong
 *  rather than slow, as the other listing tests of this directory take it. */
const generationTimeout = 90_000;

test(
  "the committed row map is what a fresh generation writes",
  async () => {
    const child = Bun.spawn(["bun", "e2e/tools/row-map.ts"], {
      cwd: dispatchPackage,
      // The generation lists the config in a subprocess of its own, which the config exempts
      // from its unset-port refusal, so no harness port of this shell reaches it.
      env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "" },
      stderr: "pipe",
      stdout: "pipe",
    });
    const [code, stderr, fresh] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
      new Response(child.stdout).text(),
    ]);
    expect(code, `generating the row map exited ${code}:\n${stderr}`).toBe(0);
    expect(
      fresh,
      "e2e/row-map.md is not what the suite says any more. Run `bun e2e/tools/row-map.ts --write` " +
        "in packages/dispatch and commit what it writes."
    ).toBe(readFileSync(mapPath, "utf8"));
  },
  generationTimeout
);

/** A row the map says is unchanged is a row no reviewer looks at, so a row whose assertions
 *  did move must carry its own note and not that sentence. The generation writes one or the
 *  other; this holds the committed map to it, which is the half a reader of k2 depends on. */
test("a row with a note carries it in the map, and is not called unchanged", () => {
  const map = readFileSync(mapPath, "utf8");
  const lines = map.split("\n").filter((line) => line.startsWith("| `e2e/"));
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
});
