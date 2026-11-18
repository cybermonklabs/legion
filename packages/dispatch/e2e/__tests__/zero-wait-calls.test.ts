import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { probesOfFiles, refusedProbes } from "../zero-wait-probes";

// A call on a probe's answer whose result decides is an instance, on the answer in place and on a
// name bound to it alike (spec r2): `text.includes("B")` decides an `if` on one instant's read as
// `text === "B"` does. The rows of e2e/ask-concurrent-edit.e2e.ts that branched on
// `secondList.includes("Option B")` were members of the class this issue ends. An answer handed to
// a function that decides inside it is the one form the check may pass, and its header names it.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));
const fixture = "__tests__/fixtures/zero-wait-calls.fixture.ts";
const fixtureProbes = probesOfFiles(e2eDirectory, [fixture]);
const fixtureLines = readFileSync(join(e2eDirectory, fixture), "utf8").split("\n");

// Each line is held to the probe it plants, so a fixture edit that moved one cannot pass for the
// scan reading it.
test("the planted calls stand on the lines the rows below read", () => {
  const planted: [number, string][] = [
    [20, "const text = (await menu.textContent()) ?? "],
    [22, 'if (((await menu.textContent()) ?? "").includes('],
    [24, "const shown = await menu.isVisible();"],
    [26, "if (Boolean(await menu.isVisible()))"],
    [28, "const label = await menu.textContent();"],
    [30, "if ((await menu.textContent())?.trim())"],
    [32, "await clickIfShown(menu, await menu.isVisible());"],
  ];
  for (const [line, text] of planted) expect(fixtureLines[line - 1]).toContain(text);
});

test("a call on the answer whose result decides is refused, in place and through a bound name", () => {
  expect(refusedProbes(fixtureProbes).map(({ probe }) => [probe.line, probe.name])).toEqual([
    [20, "textContent"],
    [22, "textContent"],
    [24, "isVisible"],
    [26, "isVisible"],
    [28, "textContent"],
    [30, "textContent"],
  ]);
});
