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
