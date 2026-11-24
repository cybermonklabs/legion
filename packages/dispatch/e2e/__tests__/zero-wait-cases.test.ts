import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { probesOfFiles, refusedProbes } from "../zero-wait-probes";

// The check of e2e/zero-wait-probes.ts reads the discriminant of a `switch`, and a probe in a
// `case` decides the same path: `switch (true) { case await menu.isVisible(): ... }` runs the case
// on one instant's answer. Its header lists what it does not see, and a `case` is not in that
// list, so this file holds the check to it.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));
const fixture = "__tests__/fixtures/zero-wait-cases.fixture.ts";
const fixtureProbes = probesOfFiles(e2eDirectory, [fixture]);
const fixtureLines = readFileSync(join(e2eDirectory, fixture), "utf8").split("\n");

// The line is held to the case it plants, so a fixture edit that moved it cannot pass for the scan
// reading it.
test("the planted case stands on the line the row below reads", () => {
  expect(fixtureLines[15]).toContain("case await menu.isVisible():");
});

test("a probe in a switch's case is refused at its line", () => {
  expect(refusedProbes(fixtureProbes).map(({ probe }) => [probe.line, probe.name])).toEqual([
    [16, "isVisible"],
  ]);
});
