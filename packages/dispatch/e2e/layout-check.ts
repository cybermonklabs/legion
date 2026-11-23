/** What the layout layer's rows share: how one route is read at every width and in every state,
 *  how a reading is judged against `e2e/layout-breaches.ts` and the route's required groups, and
 *  the stale-entry check each row file runs after its worker's rows. The rows
 *  (`e2e/layout-rule.stub.e2e.ts`) and the instrument's own controls
 *  (`e2e/layout-measure.stub.e2e.ts`) both judge through `judgeReading`, so the controls prove
 *  the judgement the rows make and not a copy of it.
 *
 *  It reaches no harness: its imports are Playwright's types, the breach list, the measure, the
 *  route list and the settle wait, so a stubbed row imports it in a run with no stack at all. */
import { readFileSync } from "node:fs";

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

/** The faces the rows draw the app's font stacks in, from the files committed under `e2e/fonts/`
 *  (each directory holds its licence), in the faces CI's Chromium resolves today (read with
 *  `CSS.getPlatformFontsForNode`). The pages load no font file for these stacks, so each machine
 *  draws them in what it holds, and the list's exact counts follow the text's width. Each face is
 *  declared under the first family its stack names, and an installed font of that name loses to
 *  it on any browser. A stack whose first family is a keyword some browser resolves itself
 *  (`ui-monospace`, `-apple-system`) cannot be won that way, since `@font-face` does not override
 *  a keyword, so the two stacks that open with one, which are Tailwind's variables, are set below
 *  to open with their pinned face:
 *  - `Inter`, the root's first family, written on `:root` in `web/src/styles.css` (every page but
 *    the new issue page, which ships its own face): DejaVu Sans 2.37.
 *  - `--font-mono`, Tailwind's `font-mono`, which `code`, `kbd`, `samp` and `pre` take through
 *    `--default-mono-font-family`: set to open with `Liberation Mono`, Liberation Mono 2.1.5, the
 *    face its `"Liberation Mono"` gives on Linux.
 *  - `--font-sans`, Tailwind's `font-sans`: set to open with `Liberation Sans`, Liberation Sans
 *    2.1.5, the face its `Arial` gives on Linux. The new issue page sets its own `--font-sans` on
 *    `html:has([data-issue-page])`, which outranks this sheet's `:root`.
 *  - `SF Mono`, the first family of the document editor's code stack (`.milkdown code`, in the
 *    editor's own sheet), and `JetBrains Mono`, the diagrams' (`.mono` in a Mermaid picture):
 *    DejaVu Sans Mono 2.37.
 *  Each is a Book and a Bold face. A weight between is matched to them as the browser matches it
 *  to the installed fonts, and italic is drawn slanted from Book; a monospace face's advances do
 *  not change with its slant, and that machine holds no DejaVu Sans Oblique. */
const PINNED_FACES = [
  ...["Inter"].flatMap((family) => [
    { dir: "dejavu", family, file: "DejaVuSans.ttf", weight: 400 },
    { dir: "dejavu", family, file: "DejaVuSans-Bold.ttf", weight: 700 },
  ]),
  ...["Liberation Mono"].flatMap((family) => [
    { dir: "liberation", family, file: "LiberationMono-Regular.ttf", weight: 400 },
    { dir: "liberation", family, file: "LiberationMono-Bold.ttf", weight: 700 },
  ]),
  ...["Liberation Sans"].flatMap((family) => [
    { dir: "liberation", family, file: "LiberationSans-Regular.ttf", weight: 400 },
    { dir: "liberation", family, file: "LiberationSans-Bold.ttf", weight: 700 },
  ]),
  ...["SF Mono", "JetBrains Mono"].flatMap((family) => [
    { dir: "dejavu", family, file: "DejaVuSansMono.ttf", weight: 400 },
    { dir: "dejavu", family, file: "DejaVuSansMono-Bold.ttf", weight: 700 },
  ]),
];
/** The family names those faces are declared under, which every stack a row reads opens with
 *  (`e2e/layout-measure.stub.e2e.ts` holds the app's stacks to it). */
export const PINNED_FAMILIES: readonly string[] = [
  ...new Set(PINNED_FACES.map((face) => face.family)),
];
/** Where the page asks for those faces; the row answers it from the committed files. */
const PINNED_FACE_PATH = "/__layout-fonts/";
/** The faces, and Tailwind's two font variables opened with their pinned face. */
export const PINNED_FACE_SHEET = [
  ...PINNED_FACES.map(
    ({ dir, family, file, weight }) =>
      `@font-face { font-family: ${JSON.stringify(family)}; font-style: normal; font-weight: ${weight}; src: url("${PINNED_FACE_PATH}${dir}/${file}") format("truetype"); }`
  ),
  ':root { --font-mono: "Liberation Mono", monospace; --font-sans: "Liberation Sans", sans-serif; }',
].join("\n");

/** Answers the page's requests for the pinned faces from `e2e/fonts/`. */
async function servePinnedFaces(page: Page): Promise<void> {
  await page.route(`**${PINNED_FACE_PATH}**`, async (route) => {
    const asked = new URL(route.request().url()).pathname.slice(PINNED_FACE_PATH.length);
    const face = PINNED_FACES.find((candidate) => `${candidate.dir}/${candidate.file}` === asked);
    if (face === undefined) {
      await route.fulfill({ body: `no pinned face ${asked}`, status: 404 });
      return;
    }
    await route.fulfill({
      body: readFileSync(new URL(`./fonts/${face.dir}/${face.file}`, import.meta.url)),
      contentType: "font/ttf",
    });
  });
}

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
  // the sheet makes the first frame after the load the settled one. The pinned faces go in with
  // it, and `document.fonts.ready` waits for them before the page is read.
  await page.addStyleTag({
    content: `*, *::before, *::after { transition: none !important; }\n${PINNED_FACE_SHEET}`,
  });
  await settle(page, route, address);
}

/** Reads the settled page: the fonts landed, then `measureLayout` with the entries `place` may
 *  match. */
async function measureSettledPage(page: Page, place: ReadingPlace): Promise<LayoutReading> {
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
  await servePinnedFaces(page);
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
      ...judgeReading(plain, await measureSettledPage(page, plain), {
        groups: route.requiredGroups,
        minElements,
      })
    );
    for (const state of route.states ?? []) {
      await loadSettled(page, route, address);
      await state.act(page);
      // What the action opened reads its own data, a body that parses and the Inbox number among
      // them, as a load does.
      await settle(page, route, address);
      const acted = { route: route.name, state: state.name, width };
      failures.push(
        ...judgeReading(acted, await measureSettledPage(page, acted), {
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
