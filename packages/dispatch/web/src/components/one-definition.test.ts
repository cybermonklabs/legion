import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { stripComments } from "../__tests__/strip-comments";

/**
 * One definition per control, and one owner for the page address (INFRA-3135).
 *
 * Every search box, panel, menu, column filter, selection bar, table and pager a Dispatch page
 * draws is a component of this directory, named once in `control-names.ts`; and the address
 * those pages read and write is `lib/page-address.ts`'s alone. Both rules hold only for as long
 * as the NEXT page follows them, and a page that writes a box, a `<table>` or a
 * `useSearchParams` of its own compiles, passes its own tests and looks right on the screen.
 * This is the test that refuses it.
 *
 * It carries no allow-list and names no exception, deliberately: a mark it finds is a mark to
 * take out, and a control a page genuinely lacks is a control to add here, where every page
 * gets it. Comments are stripped first so this file and the pages it scans can both describe
 * the marks in prose without being refused for it.
 */

const FEATURES_ROOT = join(import.meta.dir, "..", "features");

/** What the six marks are, in the words the failure uses. */
interface Mark {
  /** What was found, and what to draw instead. */
  readonly what: string;
  readonly pattern: RegExp;
}

/** The four marks no page under `features/` may carry, whatever it is. */
const SHARED_MARKS: readonly Mark[] = [
  {
    what: 'writes a search box of its own (`type="search"`) — draw `components/SearchBox.tsx`',
    pattern: /type="search"/,
  },
  {
    what: "writes a `<table>` of its own — draw `components/TableShell.tsx` or `components/DataTable.tsx`",
    pattern: /<table[\s/>]/,
  },
  {
    what: "writes a `data-control` attribute by hand — a page draws the control, which carries its own name from `components/control-names.ts`",
    pattern: /data-control/,
  },
  {
    what: "imports `components/AnchoredPanel` — open a panel through `components/PanelTrigger.tsx`, `components/DisplayMenu.tsx` or `components/MultiSelect.tsx`",
    pattern: /components\/AnchoredPanel"/,
  },
];

/** The feature directories whose whole address is `lib/page-address.ts`'s: the pages this change
 *  moved onto it. A feature outside this list still owns its own route reading. */
const ADDRESS_FEATURES = ["agents", "artifacts", "delivery", "inbox", "project", "search"];

/** The marks only those six carry: a direct read or write of the page address, by the names the
 *  pages used before one module owned it, and the hooks and the redirect the router offers. */
const ADDRESS_MARKS: readonly Mark[] = [
  {
    what: "reads the address directly (`useSearchParams`) — read it through `lib/page-address.ts`",
    pattern: /\buseSearchParams\b/,
  },
  {
    what: "reads the address directly (`useCappedSearchParam`) — `lib/page-address.ts`'s `useCappedAddressParam`",
    pattern: /\buseCappedSearchParam\b/,
  },
  {
    what: "reads the address directly (`useRepeatableSearchParams`) — `lib/page-address.ts`'s `useRepeatableAddressParams`",
    pattern: /\buseRepeatableSearchParams\b/,
  },
  {
    what: "writes the address directly (`useSearchParamsUpdate`) — `lib/page-address.ts`'s `useAddressUpdate`",
    pattern: /\buseSearchParamsUpdate\b/,
  },
  {
    what: "reads the router's location (`useLocation`) — `lib/page-address.ts`'s `usePageAddress` hands back the `pathname` and `search` a route parser takes, and `useNavigationState` what a navigation carried",
    pattern: /\buseLocation\b/,
  },
  {
    what: "builds the address directly (`new URLSearchParams`) — `lib/page-address.ts`'s `copyAddress`",
    pattern: /new URLSearchParams\b/,
  },
  {
    what: "navigates with a query of its own — `lib/page-address.ts`'s `useAddressNavigate`",
    pattern: /navigate\(\s*(?:["'`][^"'`]*\?|\{[^}]*\bsearch\s*:)/,
  },
  {
    what: "redirects with the router's own `<Navigate>` — `lib/page-address.ts`'s `AddressRedirect`",
    pattern: /<Navigate\b/,
  },
  {
    what: "reads or writes the address through the browser's own location (`window.location`) — `lib/page-address.ts`'s `usePageAddress` to read it, `useAddressUpdate` or `useAddressNavigate` to write it",
    pattern: /\b(?:window|document|globalThis)\.location\b/,
  },
  {
    what: "writes the address through the browser's own history (`history.pushState` / `history.replaceState`), behind the router's back — `lib/page-address.ts`'s `useAddressUpdate` or `useAddressNavigate`",
    pattern: /\bhistory\.(?:pushState|replaceState)\b/,
  },
];

/** The table library's module specifier, as every one of its statements ends. */
const TABLE_MODULE = '"@tanstack/react-table"';

/**
 * `components/DataTable.tsx` is the web app's one TanStack table, and it re-exports `flexRender`
 * so a page that draws its own rows never reaches the library as a value. A page may still name
 * the library's types - a `ColumnDef`, a `Row`, a `SortingState` - and the project List augments
 * its column `meta` through `declare module`, which has to sit beside the columns it describes.
 *
 * A statement is read whole, from its own `import` or `declare` to the specifier it ends at, so
 * a multi-line `import type { … } from "@tanstack/react-table"` is read as the one `import type`
 * it is rather than as the bare `}` on the line the specifier sits on.
 */
function tableImportRefusals(relativePath: string, source: string): string[] {
  const refused: string[] = [];
  for (
    let at = source.indexOf(TABLE_MODULE);
    at !== -1;
    at = source.indexOf(TABLE_MODULE, at + 1)
  ) {
    const before = source.slice(0, at);
    const keyword = /\b(?:import|declare)\b/g;
    let start = -1;
    for (let match = keyword.exec(before); match !== null; match = keyword.exec(before)) {
      start = match.index;
    }
    const statement = source.slice(start === -1 ? at : start, at + TABLE_MODULE.length);
    const allowed =
      /^import\s+type\b/.test(statement) ||
      /^declare\s+module\s+"@tanstack\/react-table"$/.test(statement);
    if (!allowed) {
      const line = source.slice(0, start === -1 ? at : start).split("\n").length;
      refused.push(
        `${relativePath}:${line} imports \`@tanstack/react-table\` as a value — the one table is \`components/DataTable.tsx\`, which re-exports \`flexRender\``
      );
    }
  }
  return refused;
}

function collectFeatureFiles(dir: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const entryPath = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...collectFeatureFiles(entryPath));
      continue;
    }
    const isSource = entry.name.endsWith(".ts") || entry.name.endsWith(".tsx");
    const isUnitTest = entry.name.endsWith(".test.ts") || entry.name.endsWith(".test.tsx");
    if (isSource && !isUnitTest) {
      files.push(entryPath);
    }
  }
  return files;
}

const FEATURE_FILES = collectFeatureFiles(FEATURES_ROOT).map(
  (file) => [join("features", file.slice(FEATURES_ROOT.length + 1)), file] as const
);

/** Every refusal one file earns, each naming the file, the line and what was found there. */
function refusals(relativePath: string, source: string): string[] {
  const marks = ADDRESS_FEATURES.some((feature) => relativePath.startsWith(`features/${feature}/`))
    ? [...SHARED_MARKS, ...ADDRESS_MARKS]
    : SHARED_MARKS;
  const lines = source.split("\n");
  const found = lines.flatMap((line, index) =>
    marks
      .filter(({ pattern }) => pattern.test(line))
      .map(({ what }) => `${relativePath}:${index + 1} ${what}`)
  );
  return [...found, ...tableImportRefusals(relativePath, source)];
}

describe("no page builds a control or an address of its own", () => {
  test("scans every source file under features/", () => {
    // A guard on the walk itself: a collection that collapsed to nothing would let every check
    // below pass while covering no page at all.
    expect(FEATURE_FILES.length).toBeGreaterThan(200);
  });

  test("a page of the six that redirects with `<Navigate>` is sent to `AddressRedirect`", () => {
    const source = [
      'import { Navigate } from "react-router-dom";',
      "",
      'export const Gone = () => <Navigate replace to="/x" />;',
    ].join("\n");
    expect(refusals("features/project/Gone.tsx", source)).toEqual([
      "features/project/Gone.tsx:3 redirects with the router's own `<Navigate>` — `lib/page-address.ts`'s `AddressRedirect`",
    ]);
    // The redirect the six draw instead, and a feature outside them, are both left alone.
    expect(refusals("features/project/Gone.tsx", '<AddressRedirect to="/x" />')).toEqual([]);
    expect(refusals("features/architecture/Gone.tsx", source)).toEqual([]);
  });

  test.each(FEATURE_FILES)("%s draws the shared controls", (relativePath, file) => {
    expect(refusals(relativePath, stripComments(readFileSync(file, "utf8")))).toEqual([]);
  });
});
