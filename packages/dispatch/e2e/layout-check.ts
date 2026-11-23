import { readFileSync } from "node:fs";

import { expect, type Page } from "@playwright/test";

import { type KnownLayoutBreach, type LayoutBreachKind, layoutBreaches } from "./layout-breaches";
import { LAYOUT_WIDTHS, type LayoutReading, measureLayout } from "./layout-measure";
import type { LayoutRoute, LayoutSeed } from "./layout-routes";
import { waitForRouteSettled } from "./review-routes";

/** The fewest elements a page's own region (`[data-testid="main-content"]`) holds once it has
 *  drawn, unless the route names a floor of its own (`minElements`) with why. */
export const MIN_ELEMENTS = 100;

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
const PINNED_FACE_PATH = "/__layout-fonts/";
/** The faces, and Tailwind's two font variables opened with their pinned face. */
export const PINNED_FACE_SHEET = [
  ...PINNED_FACES.map(
    ({ dir, family, file, weight }) =>
      `@font-face { font-family: ${JSON.stringify(family)}; font-style: normal; font-weight: ${weight}; src: url("${PINNED_FACE_PATH}${dir}/${file}") format("truetype"); }`
  ),
  ':root { --font-mono: "Liberation Mono", monospace; --font-sans: "Liberation Sans", sans-serif; }',
].join("\n");

const servedPages = new WeakSet<Page>();

async function servePinnedFaces(page: Page): Promise<void> {
  if (servedPages.has(page)) return;
  servedPages.add(page);
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

/** Draws the page's text in the pinned faces, and waits until each has loaded: answers their
 *  requests from `e2e/fonts/`, adds their sheet (with Tailwind's two font variables opened with
 *  them) to the current document, and fails unless every face loaded. A route that stopped
 *  answering, or a sheet that never reached the page, would leave the text in the host's own
 *  faces, which only a host with other fonts than CI's would show; `document.fonts.ready`
 *  resolves either way, so each face is asked for by its family and weight. A navigation drops
 *  the sheet, so call it after each load, before reading anything whose size follows its text:
 *  the layout rows do, and so may any row that measures text, such as a header's height. */
export async function pinFontFaces(page: Page): Promise<void> {
  await servePinnedFaces(page);
  await page.addStyleTag({ content: PINNED_FACE_SHEET });
  const wanted = PINNED_FACES.map(({ family, weight }) => ({ family, weight }));
  const unloaded = await page.evaluate(async (faces) => {
    const missing: string[] = [];
    for (const { family, weight } of faces) {
      try {
        const loaded = await document.fonts.load(`${weight} 16px ${JSON.stringify(family)}`);
        if (loaded.length === 0 || loaded.some((face) => face.status !== "loaded")) {
          missing.push(`${family} ${weight}`);
        }
      } catch {
        missing.push(`${family} ${weight} (load rejected)`);
      }
    }
    return missing;
  }, wanted);
  expect(unloaded, "pinned faces that did not load: the font route or the sheet is broken").toEqual(
    []
  );
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

/** One failure of a reading, with the fields that name it and the message a row prints. */
export type LayoutFailure = { readonly message: string } & (
  | {
      /** The viewport was not the width asked for, or the page's own region drew too little. */
      readonly type: "viewport" | "too-few-elements";
      readonly place: ReadingPlace;
    }
  | {
      /** A breach no entry accepts, or one past its entry's count: the entry that would list it. */
      readonly type: "unaccepted";
      readonly place: ReadingPlace;
      readonly kind: LayoutBreachKind;
      readonly element: string;
      readonly count: number;
    }
  | {
      /** An entry at the place whose reading matched fewer or more breaches than its count. */
      readonly type: "entry-mismatch";
      readonly entry: KnownLayoutBreach;
      readonly matched: number;
    }
);

function entriesAt(
  place: ReadingPlace,
  entries: readonly KnownLayoutBreach[]
): KnownLayoutBreach[] {
  return entries.filter((entry) => placeKey(entry) === placeKey(place));
}

/** What a reading must hold beyond its breaches: the groups the page must mark, and the fewest
 *  elements its own region holds (`MIN_ELEMENTS` unless the route says why it draws fewer). */
export interface ReadingNeeds {
  readonly groups?: readonly string[];
  readonly minElements?: number;
}

interface Found {
  readonly kind: LayoutBreachKind;
  readonly locator: string;
  readonly listed: readonly string[];
  readonly said: string;
}

/** Judges one reading of `place`. Failures, in this order: a viewport of another width, a page
 *  whose own region drew too little, a breach no entry of `entries` accepts or one past its
 *  entry's count - each written as the entry that would list it - and an entry at `place` that
 *  matched fewer breaches than its count. */
export function judgeReading(
  place: ReadingPlace,
  reading: LayoutReading,
  { groups = [], minElements = MIN_ELEMENTS }: ReadingNeeds = {},
  entries: readonly KnownLayoutBreach[] = layoutBreaches
): LayoutFailure[] {
  const where = `${place.route}${place.state === undefined ? "" : ` in ${place.state}`} at ${place.width}`;
  const failures: LayoutFailure[] = [];
  if (reading.viewport !== place.width) {
    failures.push({
      message: `${where}: the page read a ${reading.viewport}px viewport`,
      place,
      type: "viewport",
    });
  }
  if (reading.contentElements < minElements) {
    failures.push({
      message:
        `${where}: the page's own region drew ${reading.contentElements} elements, fewer than ` +
        `${minElements}, so it is blank, a sign-in page or a skeleton, and reads as no pass`,
      place,
      type: "too-few-elements",
    });
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
  const unlisted = new Map<
    string,
    { count: number; kind: LayoutBreachKind; element: string; said: string }
  >();
  for (const breach of found) {
    const ofKind = listed.filter((candidate) => candidate.kind === breach.kind);
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
    unlisted.set(key, {
      count: (held?.count ?? 0) + 1,
      element: breach.locator,
      kind: breach.kind,
      said: held?.said ?? breach.said,
    });
  }
  for (const [key, { count, element, kind, said }] of unlisted) {
    failures.push({
      count,
      element,
      kind,
      message: `${key}${count === 1 ? "" : `, count: ${count}`} } ${said}`,
      place,
      type: "unaccepted",
    });
  }
  for (const entry of listed) {
    const matched = taken.get(entry) ?? 0;
    const count = entry.count ?? 1;
    if (matched !== count) {
      failures.push({
        entry,
        matched,
        message: `${entryKey(entry)}: the entry accepts ${count}, and this reading had ${matched}`,
        type: "entry-mismatch",
      });
    }
  }
  console.log(
    `LAYOUT route=${place.route} state=${place.state ?? "-"} width=${place.width} ` +
      `elements=${reading.elements} content=${reading.contentElements} ` +
      `breaches=${found.length} listed=${[...taken.values()].reduce((sum, n) => sum + n, 0)}`
  );
  return failures;
}

async function loadSettled(page: Page, route: LayoutRoute, address: string): Promise<void> {
  await page.goto(address);
  // A width transition would be read part-way, as `e2e/accessibility.e2e.ts` says of colours:
  // the sheet makes the first frame after the load the settled one.
  await page.addStyleTag({ content: "*, *::before, *::after { transition: none !important; }" });
  await pinFontFaces(page);
  await waitForRouteSettled(page, route);
}

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
  // A minute after the seed, held there: a relative time ("just now", "2 minutes ago", "3 weeks
  // ago") widens its column with the gap between the seed and the read, which moves with the
  // run's pace on the stack and with the recording's age in a replay. `setFixedTime` keeps the
  // page's timers running while `Date` reads the one moment for the whole row.
  await page.clock.setFixedTime(new Date(Date.parse(seed.seededAt) + 60_000));
  const address = route.path(seed);
  const minElements = route.minElements?.count;
  const failures: LayoutFailure[] = [];
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
      await waitForRouteSettled(page, route);
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
    failures.map((failure) => failure.message),
    "layout breaches no entry of e2e/layout-breaches.ts accepts, entries their readings did not match, and pages that drew too little"
  ).toEqual([]);
}
