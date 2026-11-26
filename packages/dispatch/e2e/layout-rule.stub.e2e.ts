/** The layout rule read on every page: each route of `e2e/layout-routes.ts`, over the review
 *  corpus and the stress layer of `e2e/layout-seed.ts`, at every width and in every state, by
 *  `checkLayout` (`e2e/layout-check.ts`). A layout reading is what the browser draws from the
 *  page's GET answers and the frames its document and live sockets carry, with no write of the
 *  row's own read back, so each row runs against its recording rather than against a harness
 *  (packages/dispatch/README.md, "The stubbed layer"): the HAR answers the HTTP reads, and
 *  `e2e/layout-sockets.ts` answers the sockets from the frames the recording run kept.
 *
 *  A row fails on a breach `e2e/layout-breaches.ts` does not accept, on an entry its readings did
 *  not match exactly, and on a page that drew too little. Each reading prints one `LAYOUT …` line
 *  to stdout. Re-record after a change to the stress layer or to what a page reads:
 *  `bun run e2e:stub:record -- layout-rule.stub`. */
import { expectTimeout } from "./expect-timeout";
import { checkLayout, layoutRowTimeoutMs } from "./layout-check";
import { layoutRoutes } from "./layout-routes";
import { seedLayoutStress } from "./layout-seed";
import { answerIssueReadMarks, routeLayoutSockets } from "./layout-sockets";
import { stubbed } from "./stub/stub-test";

// The seed runs in a recording run alone, against the harness; a replay hands each row the
// scene that run wrote.
const test = stubbed(() => seedLayoutStress({ boundMs: expectTimeout }));

// The pages format dates and numbers in the browser's language and time zone, so the text widths
// the list is exact about would move with the host's; these pin them, as `going-operators.e2e.ts`
// pins its time zone.
test.use({ locale: "en-US", timezoneId: "UTC" });

for (const route of layoutRoutes) {
  // SLOW(>=2s): a load of a drawn page at each of five widths, and again for each of its states
  // after the first, each settled and measured.
  test(`${route.name} breaks no layout rule from 360 to 1920 px @pr`, async ({
    page,
    scene,
  }, testInfo) => {
    // The budget follows the row's readings (`layoutRowTimeoutMs`): twice a reading's cost on CI.
    test.setTimeout(layoutRowTimeoutMs(route));
    const keep = await routeLayoutSockets(page, testInfo);
    await answerIssueReadMarks(page);
    try {
      await checkLayout(page, route, scene);
    } finally {
      // A recording run keeps what the sockets carried even when the row's own judgement fails,
      // as its HAR is kept: a recording is made before the breach list that judges it.
      keep();
    }
  });
}
