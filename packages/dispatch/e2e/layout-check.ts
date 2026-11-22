/** What the layout layer's rows share: how one route is read at every width and in every state,
 *  how a reading is judged against `e2e/layout-breaches.ts` and the route's required groups, and
 *  the stale-entry check each row file runs after its worker's rows. The stubbed rows
 *  (`e2e/layout-rule.stub.e2e.ts`), the real-stack rows (`e2e/layout.e2e.ts`) and the
 *  instrument's own controls (`e2e/layout-measure.stub.e2e.ts`) all judge through `judgeReading`,
 *  so the controls prove the judgement the rows make and not a copy of it.
 *
 *  It reaches no harness: its imports are Playwright's types, the breach list, the measure, the
 *  route list and the settle wait, so a stubbed row imports it in a run with no stack at all. */
import { expect, type Page } from "@playwright/test";

import { type KnownLayoutBreach, layoutBreaches } from "./layout-breaches";
import { LAYOUT_WIDTHS, type LayoutReading, measureLayout } from "./layout-measure";
import type { LayoutRoute, LayoutSeed } from "./layout-routes";
import { waitForRouteSettled } from "./review-routes";

/** The fewest elements a page that drew itself holds. The smallest page the layer reads, the
 *  not-found page, draws several hundred with the shell around it. */
export const MIN_ELEMENTS = 100;

/** The height of every viewport: tall enough for a header and the first rows, which is where the
 *  rows the rule governs are, at every width. */
const HEIGHT = 900;

/** One reading: a route, in one of its states or as it loads, at one width. */
export interface ReadingPlace {
  readonly route: string;
  readonly state?: string;
  readonly width: number;
}

function placeKey(place: ReadingPlace): string {
  return `${place.route}${place.state === undefined ? "" : `#${place.state}`} ${place.width}`;
}

function entryKey(entry: KnownLayoutBreach): string {
  return `${placeKey(entry)} ${entry.kind} ${entry.element}`;
}

/** The entries of the breach list that a reading of `place` may match. */
function entriesAt(
  place: ReadingPlace,
  entries: readonly KnownLayoutBreach[]
): KnownLayoutBreach[] {
  return entries.filter((entry) => placeKey(entry) === placeKey(place));
}

/** What a worker measured, and which entries its readings matched. `staleEntries` reads both, as
 *  `e2e/accessibility.e2e.ts`'s afterAll does and for its reasons: an entry whose place this
 *  worker measured, and which nothing matched, is stale, and a stale entry would hide the
 *  regression it once accepted; judging only what this worker measured keeps a shard that ran
 *  none of an entry's route from failing for it. */
const measuredHere = new Set<string>();
const matchedHere = new Set<string>();

/** Judges one reading of `place`: a page that drew too little, a required group the page does not
 *  mark, and each breach no entry of `entries` covers are failures, written as the entry that
 *  would list the breach. Records the place as measured and the entries it matched. */
export function judgeReading(
  place: ReadingPlace,
  reading: LayoutReading,
  requiredGroups: readonly string[] = [],
  entries: readonly KnownLayoutBreach[] = layoutBreaches
): string[] {
  const where = `${place.route}${place.state === undefined ? "" : ` in ${place.state}`} at ${place.width}`;
  const failures: string[] = [];
  measuredHere.add(placeKey(place));
  if (reading.viewport !== place.width) {
    failures.push(`${where}: the page read a ${reading.viewport}px viewport`);
  }
  if (reading.elements < MIN_ELEMENTS) {
    failures.push(
      `${where}: the page drew ${reading.elements} elements, fewer than ${MIN_ELEMENTS}, so it ` +
        "is blank, a sign-in page or a skeleton, and reads as no pass"
    );
  }
  for (const group of requiredGroups) {
    if (!reading.groups.includes(group)) {
      failures.push(`${where}: no element carries data-layout-group="${group}"`);
    }
  }
  const listed = entriesAt(place, entries);
  let accepted = 0;
  for (const breach of reading.breaches) {
    const entry = listed.find(
      (candidate) => candidate.kind === breach.kind && breach.listed.includes(candidate.element)
    );
    if (entry === undefined) {
      failures.push(
        `{ route: "${place.route}", ${place.state === undefined ? "" : `state: "${place.state}", `}` +
          `width: ${place.width}, kind: "${breach.kind}", element: ${JSON.stringify(breach.locator)} } ` +
          `at ${breach.where}: ${breach.detail}; text "${breach.text}"`
      );
    } else {
      accepted += 1;
      matchedHere.add(entryKey(entry));
    }
  }
  console.log(
    `LAYOUT route=${place.route} state=${place.state ?? "-"} width=${place.width} ` +
      `elements=${reading.elements} breaches=${reading.breaches.length} listed=${accepted}`
  );
  return failures;
}

/** The entries whose place this worker measured and which matched nothing. */
export function staleEntries(entries: readonly KnownLayoutBreach[] = layoutBreaches): string[] {
  return entries
    .filter((entry) => measuredHere.has(placeKey(entry)) && !matchedHere.has(entryKey(entry)))
    .map(entryKey);
}

/** Loads `address` fresh at the page's current width and waits until `route` has settled. */
async function loadSettled(page: Page, route: LayoutRoute, address: string): Promise<void> {
  await page.goto(address);
  // A width transition would be read part-way, as `e2e/accessibility.e2e.ts` says of colours:
  // the sheet makes the first frame after the load the settled one.
  await page.addStyleTag({ content: "*, *::before, *::after { transition: none !important; }" });
  await waitForRouteSettled(page, {
    kind: "reviewed",
    layers: [],
    name: route.name,
    path: () => address,
    pattern: route.pattern,
    ready: route.ready,
  });
}

/** Reads the settled page: the fonts landed, then `measureLayout` with the entries `place` may
 *  match. */
async function read(page: Page, place: ReadingPlace): Promise<LayoutReading> {
  // A web font that lands after the wait widens every line it sets.
  await page.evaluate(() => document.fonts.ready.then(() => undefined));
  return await page.evaluate(
    measureLayout,
    entriesAt(place, layoutBreaches).map((entry) => entry.element)
  );
}

/** Reads `route` at every width of `LAYOUT_WIDTHS`: the page loaded fresh and settled, and then,
 *  for each of its states, loaded fresh again, settled and acted on. Fails once, after every
 *  reading, with every failure `judgeReading` found. */
export async function checkLayout(page: Page, route: LayoutRoute, seed: LayoutSeed): Promise<void> {
  await route.prepare?.(page.context());
  const address = route.path(seed);
  const failures: string[] = [];
  for (const width of LAYOUT_WIDTHS) {
    await page.setViewportSize({ height: HEIGHT, width });
    await loadSettled(page, route, address);
    const plain = { route: route.name, width };
    failures.push(...judgeReading(plain, await read(page, plain), route.requiredGroups));
    for (const state of route.states ?? []) {
      await loadSettled(page, route, address);
      await state.act(page);
      const acted = { route: route.name, state: state.name, width };
      failures.push(...judgeReading(acted, await read(page, acted), state.requiredGroups));
    }
  }
  expect(
    failures,
    "layout breaches no entry of e2e/layout-breaches.ts lists, required groups the page does not mark, and pages that drew too little"
  ).toEqual([]);
}
