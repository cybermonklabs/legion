import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { stripComments } from "../__tests__/strip-comments";

/**
 * The ten pages R8 of INFRA-4948 draws its control bar on, in the spec's own words, and the file
 * under `web/src/features/` that draws `ControlRow` for each (the plan's I7). A file that draws
 * the bar and is not here is a page whose bar no check reads; a page no file here draws is a page
 * that lost its bar. Each is a failure of this test, by name.
 */
const PAGES = [
  "the Inbox at /",
  "the Inbox drawer",
  "the project List",
  "the project Board",
  "the Agents page",
  "the Delivery page",
  "the search bar",
  "an issue's Artifacts tab",
  "the priorities page",
  "a priority's own page",
] as const;

type Page = (typeof PAGES)[number];

/** Each file that draws `ControlRow`, by its path under `web/src/features/`, and the pages it
 *  draws the bar on. The project List's toolbar (`IssueTableToolbar`) is also the bar above each
 *  issues table of the priorities page and of a priority's own page. */
const BAR_FILES: Readonly<Record<string, readonly Page[]>> = {
  "agents/AgentsPage.tsx": ["the Agents page"],
  "artifacts/ArtifactsTab.tsx": ["an issue's Artifacts tab"],
  "delivery/DeliveryPage.tsx": ["the Delivery page"],
  "inbox/Inbox.tsx": ["the Inbox at /", "the Inbox drawer"],
  "priorities/priority-table-controls.tsx": ["the priorities page"],
  "project/IssueBoard.tsx": ["the project Board"],
  "project/issues-table-page.tsx": [
    "the project List",
    "the priorities page",
    "a priority's own page",
  ],
  "search/SearchPalette.tsx": ["the search bar"],
};

const FEATURES_ROOT = join(import.meta.dir, "..", "features");

/** Every non-test source file under `web/src/features/`, by its path there. */
function featureFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return featureFiles(path);
    const source = entry.name.endsWith(".ts") || entry.name.endsWith(".tsx");
    const test = entry.name.endsWith(".test.ts") || entry.name.endsWith(".test.tsx");
    return source && !test ? [path.slice(FEATURES_ROOT.length + 1)] : [];
  });
}

/** An import of `components/ControlRow`, read from code with its comments stripped. Stripping
 *  only blanks comments, so a file whose text names no `components/ControlRow` is never parsed. */
const IMPORTS_CONTROL_ROW = /\bfrom\s+["'](?:\.\.\/)+components\/ControlRow["']/;

test("every file that draws ControlRow draws it on a page of R8, and every page of R8 has one", () => {
  const drawing = featureFiles(FEATURES_ROOT)
    .filter((path) => {
      const source = readFileSync(join(FEATURES_ROOT, path), "utf8");
      return (
        source.includes("components/ControlRow") &&
        IMPORTS_CONTROL_ROW.test(stripComments(source, `features/${path}`))
      );
    })
    .sort();
  const unnamed = drawing.filter((path) => BAR_FILES[path] === undefined);
  expect(
    unnamed,
    `${unnamed.join(", ")} draws ControlRow on no page of R8 this test names`
  ).toEqual([]);
  const drawn = new Set(drawing.flatMap((path) => BAR_FILES[path] ?? []));
  const bare = PAGES.filter((page) => !drawn.has(page));
  expect(bare, `no file draws the control bar on ${bare.join(", ")}`).toEqual([]);
  // A file the map names that no longer draws the bar leaves its pages to another, or to none.
  const gone = Object.keys(BAR_FILES).filter((path) => !drawing.includes(path));
  expect(gone, `${gone.join(", ")} no longer draws ControlRow`).toEqual([]);
});
