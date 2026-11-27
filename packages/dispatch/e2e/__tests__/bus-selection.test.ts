/** Whether a run is a bus run, read from the projects it selected (`busRunOf`,
 *  e2e/configurations.ts), which `e2e/global-setup.ts` asks before it starts anything: a run of
 *  bus projects alone gives each stack a JetStream server, a run of none gives none, and a run of
 *  both is refused, since the bus changes the server for every row of the stack it runs in. */
import { expect, test } from "bun:test";

import { busProjectNames, busRunOf, busSpecPattern } from "../configurations";

test("a run of bus projects alone is a bus run, and a run of none is not", () => {
  expect(busRunOf([...busProjectNames])).toBe(true);
  expect(busRunOf(["chromium", "iphone"])).toBe(false);
  // A run that selected nothing, which the stubbed layer's own return comes before, is none.
  expect(busRunOf([])).toBe(false);
});

test("a run that selects a bus project beside another is refused, naming both", () => {
  expect(() => busRunOf(["chromium", "chromium-bus"])).toThrow(
    /bus project chromium-bus beside chromium, which run without a bus.*`bun run e2e:bus`/s
  );
});

test("the bus files are the bus spec's alone", () => {
  expect(busSpecPattern.test("answer-parties.bus.e2e.ts")).toBe(true);
  expect(busSpecPattern.test("ask-answer-area.e2e.ts")).toBe(false);
  expect(busSpecPattern.test("writing-surfaces.e2e.ts")).toBe(false);
});
