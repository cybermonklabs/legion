import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { probesOfFiles, refusedProbes } from "../zero-wait-probes";

// A loop whose number of turns is one zero-wait read decides a path as a branch does: read before
// the page has drawn the elements, it runs no turn, and the assertions inside it never run. The
// row "phone status labels are readable and closed issues load from the Status filter" of
// e2e/issues-table.e2e.ts loops over `strip.getByRole("button").all()` right after `page.goto`,
// with only a `toHaveCount(0)` before it, which holds on a page that has drawn nothing. Run with a
// line that prints the loop's length, it read 0 buttons on 6 of 6 runs in the `iphone` project,
// and the row passed each time without reading one label. This file holds the check of
// e2e/zero-wait-probes.ts to such loops.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));
const fixture = "__tests__/fixtures/zero-wait-loops.fixture.ts";
const fixtureProbes = probesOfFiles(e2eDirectory, [fixture]);
const fixtureLines = readFileSync(join(e2eDirectory, fixture), "utf8").split("\n");

// Each line is held to the loop it plants, so a fixture edit that moved one cannot pass for the
// scan reading it.
test("the planted loops stand on the lines the rows below read", () => {
  const planted: [number, string][] = [
    [19, "for (const row of await rows.all())"],
    [21, "const items = await rows.all();"],
    [25, "for (const text of await rows.allTextContents())"],
    [32, "for (const row of await rows.all())"],
  ];
  for (const [line, text] of planted) expect(fixtureLines[line - 1]).toContain(text);
});

test("a loop over a zero-wait read of the page's elements is refused at its line", () => {
  expect(refusedProbes(fixtureProbes).map(({ probe }) => [probe.line, probe.name])).toEqual([
    [19, "all"],
    [21, "all"],
    [25, "allTextContents"],
  ]);
});

test("the same loop under a reason of eight words is read, and kept", () => {
  const kept = fixtureProbes.filter((probe) => probe.line === 32);
  expect(kept.map((probe) => probe.name)).toEqual(["all"]);
  expect(refusedProbes(kept)).toEqual([]);
});
