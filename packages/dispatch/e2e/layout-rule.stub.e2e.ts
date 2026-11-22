/** The layout rule read on every page a recording can draw: each route of
 *  `e2e/layout-routes.ts` with no `needsStack`, over the review corpus and the stress layer of
 *  `e2e/layout-seed.ts`, at every width and in every state, by `checkLayout`
 *  (`e2e/layout-check.ts`). A layout reading is what the browser draws from the page's GET answers
 *  and the live socket's subscription frames, with no write of the row's own read back, so each
 *  row runs against its recording rather than against a harness (packages/dispatch/README.md,
 *  "The stubbed layer"). The routes that need a frame or a socket a recording does not hold are
 *  read on the real stack by `e2e/layout.e2e.ts`.
 *
 *  A row fails on any breach `e2e/layout-breaches.ts` does not list, on a required group the
 *  page does not mark, and on a page that drew too little; after the worker's rows, on an entry
 *  whose route, state and width it measured and which matched nothing. Each reading prints one
 *  `LAYOUT …` line to stdout. Re-record after a change to the stress layer or to what a page
 *  reads: `bun run e2e:stub:record -- layout-rule.stub`. */
import { expect } from "@playwright/test";

import { expectTimeout } from "./expect-timeout";
import { checkLayout, staleEntries } from "./layout-check";
import { layoutRoutes } from "./layout-routes";
import { seedLayoutStress } from "./layout-seed";
import { stubbed } from "./stub/stub-test";

// The seed runs in a recording run alone, against the harness; a replay hands each row the
// scene that run wrote.
const test = stubbed(() => seedLayoutStress({ boundMs: expectTimeout }));

for (const route of layoutRoutes.filter((candidate) => candidate.needsStack === undefined)) {
  // SLOW(>=2s): a load of a drawn page at each of five widths, and again for each of its states,
  // each settled and measured.
  test(`${route.name} breaks no layout rule from 360 to 1920 px @pr`, async ({ page, scene }) => {
    await checkLayout(page, route, scene);
  });
}

test.afterAll(() => {
  expect(
    staleEntries(),
    "entries of e2e/layout-breaches.ts whose route, state and width were measured and which matched nothing"
  ).toEqual([]);
});
