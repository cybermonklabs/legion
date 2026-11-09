/** The one thing `e2e/tools/compare-recordings.ts` must refuse: being pointed at the committed
 *  set as its own fresh one.
 *
 *  A recording compared with itself can never differ, so such a run prints that every committed
 *  recording still answers as it holds and exits 0 while proving nothing - and it is the run a
 *  person is most likely to make, because the tool's own failure message tells a reader to copy
 *  the re-recorded artifact "over packages/dispatch/e2e/__recordings__" and read the diff, and
 *  the obvious next command names that directory. The review round that added the guard added
 *  it for that reason: the vanished-recording finding was unreachable while the two sides could
 *  be one directory.
 *
 *  The guard compares the caller's word with the absolute path of the committed set, so it
 *  holds for an absolute spelling and not for the relative one a person types from
 *  `packages/dispatch`, where every command of this package is written from. Measured at
 *  legion/INFRA-1407 8fe394e1: `bun e2e/tools/compare-recordings.ts e2e/__recordings__` printed
 *  "every one of the 132 committed recordings still answers as it holds" and exited 0.
 */

import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const committed = fileURLToPath(new URL("../__recordings__", import.meta.url));

/** Two spawns of about a second each. */
const refusalTimeout = 60_000;

/** What the tool did with `fresh` as its one argument, run from the package root as every
 *  command of this package is. */
async function comparing(fresh: string): Promise<{ code: number; output: string }> {
  const child = Bun.spawn(["bun", "e2e/tools/compare-recordings.ts", fresh], {
    cwd: dispatchPackage,
    env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "" },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  return { code, output: `${stdout}${stderr}` };
}

test(
  "the committed set named by the path a person types is refused, not compared with itself",
  async () => {
    const { code, output } = await comparing("e2e/__recordings__");
    expect(
      { code, refused: output.includes("is the committed set itself") },
      "A relative path to the committed set reaches the comparison, where every recording " +
        "matches itself and the run exits 0 having proved nothing. Compare the resolved path " +
        "with the committed root, so the guard holds whichever way a person spells it."
    ).toEqual({ code: 2, refused: true });
  },
  refusalTimeout
);

test(
  "the committed set named by its absolute path is refused",
  async () => {
    // The spelling the guard already holds, so the rule above cannot be met by dropping it.
    const { code, output } = await comparing(committed);
    expect(code).toBe(2);
    expect(output).toContain("is the committed set itself");
  },
  refusalTimeout
);
