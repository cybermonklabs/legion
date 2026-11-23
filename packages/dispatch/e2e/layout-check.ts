/** What the layout layer's rows share: how one route is read at every width and in every state,
 *  how a reading is judged against `e2e/layout-breaches.ts` and the route's required groups, and
 *  the stale-entry check each row file runs after its worker's rows. The rows
 *  (`e2e/layout-rule.stub.e2e.ts`) and the instrument's own controls
 *  (`e2e/layout-measure.stub.e2e.ts`) both judge through `judgeReading`, so the controls prove
 *  the judgement the rows make and not a copy of it.
 *
 *  It reaches no harness: its imports are Playwright's types, the breach list, the measure, the
 *  route list and the settle wait, so a stubbed row imports it in a run with no stack at all. */
import { expect, type Page } from "@playwright/test";

import { type KnownLayoutBreach, type LayoutBreachKind, layoutBreaches } from "./layout-breaches";
import { LAYOUT_WIDTHS, type LayoutReading, measureLayout } from "./layout-measure";
import type { LayoutRoute, LayoutSeed } from "./layout-routes";
import { waitForRouteSettled } from "./review-routes";

/** The fewest elements a page's own region (`[data-testid="main-content"]`) holds once it has
 *  drawn, unless the route names a floor of its own (`minElements`) with why. */
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

/** What a worker measured, and which entries its readings matched in full. `staleEntries` reads
 *  both, as `e2e/accessibility.e2e.ts`'s afterAll does and for its reasons: an entry whose place
 *  this worker measured, and which its readings did not match, would hide the regression it once
 *  accepted; judging only what this worker measured keeps a shard that ran none of an entry's
 *  route from failing for it. */
const measuredHere = new Set<string>();
const matchedHere = new Set<string>();

/** What a reading must hold beyond its breaches: the groups the page must mark, and the fewest
 *  elements its own region holds (`MIN_ELEMENTS` unless the route says why it draws fewer). */
export interface ReadingNeeds {
  readonly groups?: readonly string[];
  readonly minElements?: number;
}

/** One thing a reading broke, as the breach list names it. */
interface Found {
  readonly kind: LayoutBreachKind;
  readonly locator: string;
  /** For a page-scroll breach, the listed selectors any element past the viewport inside it
   *  sits in; empty for every other kind, which matches its own locator alone. */
  readonly listed: readonly string[];
  readonly said: string;
}

/** Judges one reading of `place`. Failures: a page whose own region drew too little, a breach no
 *  entry of `entries` accepts or one past its entry's count - each written as the entry that would
 *  list it - and an entry at `place` that matched fewer breaches than its count. Records the
 *  place as measured and the entries it matched in full. */
export function judgeReading(
  place: ReadingPlace,
  reading: LayoutReading,
  { groups = [], minElements = MIN_ELEMENTS }: ReadingNeeds = {},
  entries: readonly KnownLayoutBreach[] = layoutBreaches
): string[] {
  const where = `${place.route}${place.state === undefined ? "" : ` in ${place.state}`} at ${place.width}`;
  const failures: string[] = [];
  measuredHere.add(placeKey(place));
  if (reading.viewport !== place.width) {
    failures.push(`${where}: the page read a ${reading.viewport}px viewport`);
  }
  if (reading.contentElements < minElements) {
    failures.push(
      `${where}: the page's own region drew ${reading.contentElements} elements, fewer than ` +
        `${minElements}, so it is blank, a sign-in page or a skeleton, and reads as no pass`
    );
  }
  const found: Found[] = [
    ...reading.breaches.map((breach) => ({
      kind: breach.kind,
      listed: breach.kind === "page-scroll" ? breach.listed : [],
      locator: breach.locator,
      said: `at ${breach.where}: ${breach.detail}; text "${breach.text}"`,
    })),
    ...groups
      .filter((group) => !reading.groups.includes(group))
      .map((group) => ({
        kind: "group-missing" as const,
        listed: [],
        locator: group,
        said: `no element carries data-layout-group="${group}"`,
      })),
  ];
  const listed = entriesAt(place, entries);
  const taken = new Map<KnownLayoutBreach, number>();
  const room = (entry: KnownLayoutBreach) => (taken.get(entry) ?? 0) < (entry.count ?? 1);
  const unlisted = new Map<string, { count: number; said: string }>();
  for (const breach of found) {
    const ofKind = listed.filter((candidate) => candidate.kind === breach.kind);
    // Its own locator first: a page-scroll breach inside a region another entry names would
    // otherwise take that wider entry's place, and leave its own unmatched on every run alike.
    const entry =
      ofKind.find((candidate) => candidate.element === breach.locator && room(candidate)) ??
      ofKind.find((candidate) => breach.listed.includes(candidate.element) && room(candidate));
    if (entry !== undefined) {
      taken.set(entry, (taken.get(entry) ?? 0) + 1);
      continue;
    }
    const key =
      `{ route: "${place.route}", ${place.state === undefined ? "" : `state: "${place.state}", `}` +
      `width: ${place.width}, kind: "${breach.kind}", element: ${JSON.stringify(breach.locator)}`;
    const held = unlisted.get(key);
    unlisted.set(key, { count: (held?.count ?? 0) + 1, said: held?.said ?? breach.said });
  }
  for (const [key, { count, said }] of unlisted) {
    failures.push(`${key}${count === 1 ? "" : `, count: ${count}`} } ${said}`);
  }
  for (const entry of listed) {
    const matched = taken.get(entry) ?? 0;
    const count = entry.count ?? 1;
    if (matched === count) {
      matchedHere.add(entryKey(entry));
    } else {
      failures.push(
        `${entryKey(entry)}: the entry accepts ${count}, and this reading had ${matched}`
      );
    }
  }
  console.log(
    `LAYOUT route=${place.route} state=${place.state ?? "-"} width=${place.width} ` +
      `elements=${reading.elements} content=${reading.contentElements} ` +
      `breaches=${found.length} listed=${[...taken.values()].reduce((sum, n) => sum + n, 0)}`
  );
  return failures;
}

/** The entries whose place this worker measured and which its readings did not match in full. */
export function staleEntries(entries: readonly KnownLayoutBreach[] = layoutBreaches): string[] {
  return entries
    .filter((entry) => measuredHere.has(placeKey(entry)) && !matchedHere.has(entryKey(entry)))
    .map(entryKey);
}

/** Waits until `route`'s page has settled, as both review layers wait (`waitForRouteSettled`):
 *  the route's ready locators, no body still parsing, and the Inbox number holding still. */
async function settle(page: Page, route: LayoutRoute, address: string): Promise<void> {
  await waitForRouteSettled(page, {
    kind: "reviewed",
    layers: [],
    name: route.name,
    path: () => address,
    pattern: route.pattern,
    ready: route.ready,
  });
}

/** Loads `address` fresh at the page's current width and waits until `route` has settled. */
async function loadSettled(page: Page, route: LayoutRoute, address: string): Promise<void> {
  await page.goto(address);
  // A width transition would be read part-way, as `e2e/accessibility.e2e.ts` says of colours:
  // the sheet makes the first frame after the load the settled one.
  await page.addStyleTag({ content: "*, *::before, *::after { transition: none !important; }" });
  await settle(page, route, address);
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
 *  for each of its states, loaded fresh again, settled, acted on and settled again. Fails once,
 *  after every reading, with every failure `judgeReading` found. */
export async function checkLayout(page: Page, route: LayoutRoute, seed: LayoutSeed): Promise<void> {
  await route.prepare?.(page.context());
  // A minute after the seed, held there: a relative time ("just now", "2 minutes ago", "3 weeks
  // ago") widens its column with the gap between the seed and the read, which moves with the
  // run's pace on the stack and with the recording's age in a replay. `setFixedTime` keeps the
  // page's timers running while `Date` reads the one moment for the whole row.
  await page.clock.setFixedTime(new Date(Date.parse(seed.seededAt) + 60_000));
  const address = route.path(seed);
  const minElements = route.minElements?.count;
  const failures: string[] = [];
  for (const width of LAYOUT_WIDTHS) {
    await page.setViewportSize({ height: HEIGHT, width });
    await loadSettled(page, route, address);
    const plain = { route: route.name, width };
    failures.push(
      ...judgeReading(plain, await read(page, plain), { groups: route.requiredGroups, minElements })
    );
    for (const state of route.states ?? []) {
      await loadSettled(page, route, address);
      await state.act(page);
      // What the action opened reads its own data, a body that parses and the Inbox number among
      // them, as a load does.
      await settle(page, route, address);
      const acted = { route: route.name, state: state.name, width };
      failures.push(
        ...judgeReading(acted, await read(page, acted), {
          groups: state.requiredGroups,
          minElements,
        })
      );
    }
  }
  expect(
    failures,
    "layout breaches no entry of e2e/layout-breaches.ts accepts, entries their readings did not match, and pages that drew too little"
  ).toEqual([]);
}
