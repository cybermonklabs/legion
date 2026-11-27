import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { flexRender as libraryFlexRender } from "@tanstack/react-table";

import { stripComments } from "../__tests__/strip-comments";
import { CONTROL } from "./control-names";
import { flexRender } from "./DataTable";

/**
 * One definition per control, and one owner for the page address (INFRA-3135).
 *
 * Every search box, panel, menu, column filter, selection bar, table and pager a Dispatch page
 * draws is a component of this directory, named once in `control-names.ts`; and the address
 * those pages read and write is `lib/page-address.ts`'s alone. Both rules hold only for as long
 * as the NEXT page follows them, and a page that writes a box, a `<table>` or a
 * `useSearchParams` of its own compiles, passes its own tests and looks right on the screen.
 * This is the test that refuses it. In every file under `features/` it refuses a search box of
 * the page's own (`type="search"`), a `<table>`, a panel opened through `AnchoredPanel` rather
 * than through `PanelTrigger`, `DisplayMenu` or `MultiSelect`, a `data-control` written by hand,
 * and a value import of the table library (`SHARED_MARKS`, `tableImportRefusals`). In the seven
 * features whose address that module owns (`ADDRESS_FEATURES`) it refuses every read and write
 * of the address that goes round it - the router's hooks, a hand-built query, a `navigate` or a
 * `<Link>` to a query of the page's own, the browser's own location and history - and the
 * router's own `<Navigate>` redirect (`ADDRESS_MARKS`).
 *
 * It carries no allow-list and names no exception, deliberately: a mark it finds is a mark to
 * take out, and a control a page genuinely lacks is a control to add here, where every page
 * gets it. Comments are stripped first (`__tests__/strip-comments.ts`, which leaves a `/*` or a
 * `//` inside a string, a template or a regular expression where it is) so this file and the
 * pages it scans can both describe the marks in prose without being refused for it. Each mark
 * is then matched against the whole file rather than a line at a time, because the formatter
 * breaks a long construct across lines: a long `<table` sits alone at the end of its line, and
 * a `navigate({` has its `search:` on a line below.
 */

const FEATURES_ROOT = join(import.meta.dir, "..", "features");

/** One mark: what it matches, and what the failure says. */
interface Mark {
  /** What was found, and what to draw instead. */
  readonly what: string;
  /** Matched against the whole stripped file, so a `\s` in it spans the formatter's line breaks;
   *  each match is reported at the line it starts on. */
  readonly pattern: RegExp;
}

/** The four marks no page under `features/` may carry, whatever it is. */
const SHARED_MARKS: readonly Mark[] = [
  {
    what: 'writes a search box of its own (`type="search"`) — draw `components/SearchBox.tsx`',
    pattern: /type\s*[:=]\s*(?:\{\s*)?["'`]search["'`]|role=(?:\{\s*)?["'`]searchbox["'`]/,
  },
  {
    what: "writes a `<table>` of its own — draw `components/TableShell.tsx` or `components/DataTable.tsx`",
    pattern:
      /<table(?![\w-])|\bcreateElement\(\s*["'`]table["'`]|role=(?:\{\s*)?["'`](?:table|grid)["'`]/,
  },
  {
    what: "writes a `data-control` attribute by hand — a page draws the control, which carries its own name from `components/control-names.ts`",
    pattern: /data-control|\bdataset(?:\.control\b|\[["']control["']\])/,
  },
  {
    what: "imports `components/AnchoredPanel` — open a panel through `components/PanelTrigger.tsx`, `components/DisplayMenu.tsx` or `components/MultiSelect.tsx`",
    pattern: /components\/AnchoredPanel(?:\.js)?["']/,
  },
];

/** The feature directories whose whole address is `lib/page-address.ts`'s: the pages this change
 *  moved onto it. A feature outside this list still owns its own route reading. */
const ADDRESS_FEATURES = [
  "agents",
  "artifacts",
  "delivery",
  "inbox",
  "priorities",
  "project",
  "search",
];

/** The marks only those seven carry: a direct read or write of the page address, by the names
 *  the pages used before one module owned it, and the hooks and the redirect the router offers. */
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
    pattern: /new (?:window\.|globalThis\.)?URLSearchParams\b/,
  },
  {
    what: "navigates with a query of its own — `lib/page-address.ts`'s `useAddressNavigate`",
    // A path holding `?`, or an object naming `search` among its own keys: the lookbehind takes
    // `search` only where a key can stand, so `state: search` and `hash: x.search` are values
    // and not a query. A key of an object nested in it is not the query either, and a
    // template's `${…}` inside it is skipped whole.
    pattern: /navigate\(\s*(?:["'`][^"'`]*\?|\{(?:[^{}]|\{[^{}]*\})*?(?<=[{,]\s*)search\s*[:,}])/,
  },
  {
    what: "builds a link's query by hand (a `<Link>` to a query) — `lib/page-address.ts`'s `copyAddress` and `addressPath`",
    // A `<Link>` or `<NavLink>` whose `to` is a query string, a template query, or an object
    // naming `search`: the router's own link carrying an address the page joined for itself.
    pattern: /<(?:Link|NavLink)\b[^>]*\bto=(?:["'`]\?|\{\s*`\?|\{\{\s*search\b)/,
  },
  {
    what: "redirects with the router's own `<Navigate>` — `lib/page-address.ts`'s `AddressRedirect`",
    pattern:
      /<Navigate\b|\bNavigate\s+as\s+\w|<[A-Za-z_$][\w$]*\.Navigate\b|\bcreateElement\(\s*Navigate\b/,
  },
  {
    what: "reads or writes the address through the browser's own location (`window.location`) — `lib/page-address.ts`'s `usePageAddress` to read it, `useAddressUpdate` or `useAddressNavigate` to write it",
    // `self`, `top` and `parent` name this window as `window` does; each is matched only as a
    // name of its own, so an object's own `parent.location` is not one.
    pattern:
      /(?:\b(?:window|document|globalThis)|(?<![\w$.])(?:self|top|parent))(?:\??\.(?:self|top|parent))?(?:\??\.location\b|\[["']location["']\])|\bdocument\.URL\b/,
  },
  {
    what: "reads the browser's own location through the bare global (`location.search`) — `lib/page-address.ts`'s `usePageAddress`, under a name of its own, since a bare `location` is the browser's",
    pattern: /(?<![\w$.])location(?:\??\.[A-Za-z_$]|!\.[A-Za-z_$]|\[["'])/,
  },
  {
    what: "takes the browser's own location whole (`const { search } = location`) — `lib/page-address.ts`'s `usePageAddress`, under a name of its own, since a bare `location` is the browser's",
    // The global bound to a name or destructured, which reads the address just as `location.x`
    // does: the `=` is an assignment or a default, never a comparison (`==`, `===`, `!==`), and
    // `location` is taken whole, not through a member (`location!.x`, `location["x"]`), which the
    // bare-global mark above answers.
    pattern: /(?<![=!<>])=\s*location\s*(?![\w$.?(![])/,
  },
  {
    what: "writes the address through the browser's own history (`history.pushState` / `history.replaceState`), behind the router's back — `lib/page-address.ts`'s `useAddressUpdate` or `useAddressNavigate`",
    pattern:
      /\bhistory(?:\??\.(?:pushState|replaceState)\b|\[["'](?:pushState|replaceState)["']\])/,
  },
];

/** The table library's module specifiers: its main package, any subpath of it, and the core
 *  package, each of which a page reaches as a value only through `components/DataTable.tsx`. */
const TABLE_MODULE = /["'`]@tanstack\/(?:react-table(?:\/[^"'`]*)?|table-core)["'`]/g;

/**
 * `components/DataTable.tsx` is the web app's one TanStack table, and it re-exports `flexRender`
 * so a page that draws its own rows never reaches the library as a value. A page may still name
 * the library's types - a `ColumnDef`, a `Row`, a `SortingState` - and the project List augments
 * its column `meta` through `declare module`, which has to sit beside the columns it describes:
 * that `declare module` is allowed when it adds the `ColumnMeta` interface and nothing else.
 *
 * A statement is read whole, from where it starts to the specifier it ends at. It starts at its
 * own `import`, `export` or `declare`, or after the `;` that ends the statement before it,
 * whichever comes later. So a multi-line `import type { … } from "@tanstack/react-table"` is
 * read as the one `import type` it is rather than as the bare `}` on the line the specifier sits
 * on, and an `export { … } from "@tanstack/react-table"` after it is read as the re-export it is
 * rather than as the tail of that `import type`. A re-export of the library's types,
 * `export type { … }`, names types as an `import type` does.
 */
function tableImportRefusals(relativePath: string, source: string): string[] {
  const refused: string[] = [];
  TABLE_MODULE.lastIndex = 0;
  for (let match = TABLE_MODULE.exec(source); match !== null; match = TABLE_MODULE.exec(source)) {
    const specifier = match[0];
    const at = match.index;
    const before = source.slice(0, at);
    const keyword = /\b(?:import|export|declare)\b/g;
    let start = before.lastIndexOf(";") + 1;
    for (let found = keyword.exec(before); found !== null; found = keyword.exec(before)) {
      start = Math.max(start, found.index);
    }
    const statement = source.slice(start, at + specifier.length).trimStart();
    const module = specifier.slice(1, -1);
    const declared = new RegExp(
      `^declare\\s+module\\s+["'\`]${module.replace(/[.*+?^${}()|[\]\\/-]/g, "\\$&")}["'\`]$`
    );
    const statementStart = at + specifier.length - statement.length;
    const line = source.slice(0, statementStart).split("\n").length;
    if (declared.test(statement)) {
      if (!addsColumnMetaAlone(source.slice(at + specifier.length))) {
        refused.push(
          `${relativePath}:${line} declares \`${module}\` with more than the one \`ColumnMeta\` it may add — the one table is \`components/DataTable.tsx\`, which re-exports \`flexRender\``
        );
      }
    } else if (!/^(?:import|export)\s+type\b/.test(statement)) {
      refused.push(
        `${relativePath}:${line} imports \`${module}\` as a value — the one table is \`components/DataTable.tsx\`, which re-exports \`flexRender\``
      );
    }
  }
  TABLE_MODULE.lastIndex = 0;
  return refused;
}

/** The text from `open` (an index of `{`) to the `}` that closes it, both included; the rest of
 *  `text` where it never closes. The text is stripped of comments already. */
function braced(text: string, open: number): string {
  let depth = 0;
  for (let index = open; index < text.length; index++) {
    if (text[index] === "{") depth++;
    else if (text[index] === "}" && --depth === 0) return text.slice(open, index + 1);
  }
  return text.slice(open);
}

/** Whether the body of a `declare module` block, read from just after its specifier, holds the
 *  one `interface ColumnMeta<…> { … }` and nothing else. */
function addsColumnMetaAlone(afterSpecifier: string): boolean {
  const open = afterSpecifier.indexOf("{");
  if (open === -1 || afterSpecifier.slice(0, open).trim() !== "") return false;
  const body = braced(afterSpecifier, open).slice(1, -1);
  const meta = /^\s*interface\s+ColumnMeta\s*<[^{}]*>\s*/.exec(body);
  if (meta === null) return false;
  const rest = body.slice(meta[0].length);
  if (!rest.startsWith("{")) return false;
  return rest.slice(braced(rest, 0).length).trim() === "";
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

/** Each feature file's text with its comments stripped, by its path under `web/src`: read from
 *  disk and stripped once, for every scan below that reads it. */
const featureCode = new Map<string, string>();

function codeOf(relativePath: string, file: string): string {
  let code = featureCode.get(relativePath);
  if (code === undefined) {
    code = stripComments(readFileSync(file, "utf8"), relativePath);
    featureCode.set(relativePath, code);
  }
  return code;
}

/** Every refusal one file earns, each naming the file, the line and what was found there.
 *  `source` is the file as it stands on disk; its comments are stripped here. */
function refusals(relativePath: string, source: string): string[] {
  return codeRefusals(relativePath, stripComments(source, relativePath));
}

/** `refusals` of a file's text whose comments are stripped already. */
function codeRefusals(relativePath: string, code: string): string[] {
  const marks = ADDRESS_FEATURES.some((feature) => relativePath.startsWith(`features/${feature}/`))
    ? [...SHARED_MARKS, ...ADDRESS_MARKS]
    : SHARED_MARKS;
  const found = marks
    .flatMap(({ pattern, what }) =>
      Array.from(code.matchAll(new RegExp(pattern.source, `${pattern.flags}g`)), (match) => ({
        line: code.slice(0, match.index).split("\n").length,
        what,
      }))
    )
    .sort((left, right) => left.line - right.line)
    .map(({ line, what }) => `${relativePath}:${line} ${what}`);
  return [...found, ...tableImportRefusals(relativePath, code)];
}

describe("no page builds a control or an address of its own", () => {
  test("scans every source file under features/", () => {
    // A guard on the walk itself: a collection that collapsed to nothing would let every check
    // below pass while covering no page at all.
    expect(FEATURE_FILES.length).toBeGreaterThan(200);
  });

  test("a page of the seven that redirects with `<Navigate>` is sent to `AddressRedirect`", () => {
    const source = [
      'import { Navigate } from "react-router-dom";',
      "",
      'export const Gone = () => <Navigate replace to="/x" />;',
    ].join("\n");
    expect(refusals("features/project/Gone.tsx", source)).toEqual([
      "features/project/Gone.tsx:3 redirects with the router's own `<Navigate>` — `lib/page-address.ts`'s `AddressRedirect`",
    ]);
    // The redirect the seven draw instead, and a feature outside them, are both left alone.
    expect(refusals("features/project/Gone.tsx", '<AddressRedirect to="/x" />')).toEqual([]);
    expect(refusals("features/architecture/Gone.tsx", source)).toEqual([]);
  });

  // Each form below is one the formatter or an ordinary page writes, and the guard has to read
  // through it: a construct broken across lines, a second spelling of a mark, the browser's
  // bare `location`, a re-export after an `import type`, and a `/*` inside a string.

  test("a `<table` the formatter leaves alone at the end of its line is refused", () => {
    const source = [
      "export const Probe = () => (",
      "  <table",
      '    aria-label="A table whose attributes are long enough to go one to a line"',
      '    className="w-full"',
      "  >",
      "    <tbody />",
      "  </table>",
      ");",
    ].join("\n");
    expect(refusals("features/inbox/Probe.tsx", source)).toEqual([
      "features/inbox/Probe.tsx:2 writes a `<table>` of its own — draw `components/TableShell.tsx` or `components/DataTable.tsx`",
    ]);
  });

  test("a `navigate({` whose `search` is on a later line is refused", () => {
    const source = [
      "export function useOpenBugs(navigate: (to: { pathname: string; search: string }) => void) {",
      "  return () =>",
      "    navigate({",
      '      pathname: "/projects/CORE/issues",',
      '      search: "?label=bug",',
      "    });",
      "}",
    ].join("\n");
    expect(refusals("features/project/open-bugs.ts", source)).toEqual([
      "features/project/open-bugs.ts:3 navigates with a query of its own — `lib/page-address.ts`'s `useAddressNavigate`",
    ]);
    // The key written short is the same query, and a nested object's `search` or a `/search`
    // path is not one.
    expect(refusals("features/project/open-bugs.ts", "navigate({ pathname, search });")).toEqual([
      "features/project/open-bugs.ts:1 navigates with a query of its own — `lib/page-address.ts`'s `useAddressNavigate`",
    ]);
    expect(
      refusals(
        "features/project/open-bugs.ts",
        `navigate({ pathname: \`/search/\${key}\`, state: { search: "x" } });`
      )
    ).toEqual([]);
  });

  test("a `navigate({` whose `search` is a value rather than a key is left alone", () => {
    // `state: search` carries the page's own text into the next page's router state, and
    // `hash: x.search` is a fragment read off an object: neither names a query of its own.
    expect(
      refusals("features/project/open-bugs.ts", "navigate({ pathname, state: search });")
    ).toEqual([]);
    expect(
      refusals("features/project/open-bugs.ts", "navigate({ pathname: p, hash: x.search });")
    ).toEqual([]);
    // The key itself is still refused, however the formatter spaced it.
    expect(
      refusals("features/project/open-bugs.ts", "navigate({\n  pathname,\n  search: query,\n});")
    ).toEqual([
      "features/project/open-bugs.ts:1 navigates with a query of its own — `lib/page-address.ts`'s `useAddressNavigate`",
    ]);
  });

  test("a page of the seven that reads `self`, `top` or `parent` `.location` is refused", () => {
    const what =
      "reads or writes the address through the browser's own location (`window.location`) — `lib/page-address.ts`'s `usePageAddress` to read it, `useAddressUpdate` or `useAddressNavigate` to write it";
    for (const name of ["self", "top", "parent"]) {
      expect(
        refusals("features/inbox/probe-query.ts", `export const q = ${name}.location.search;`)
      ).toEqual([`features/inbox/probe-query.ts:1 ${what}`]);
    }
    // A `parent` of something else is that object's own field, not this window.
    expect(
      refusals("features/inbox/probe-query.ts", "export const q = node.parent.location;")
    ).toEqual([]);
  });

  test("a page of the seven that takes the bare global `location` whole is refused", () => {
    const what =
      "takes the browser's own location whole (`const { search } = location`) — `lib/page-address.ts`'s `usePageAddress`, under a name of its own, since a bare `location` is the browser's";
    expect(
      refusals("features/inbox/probe-query.ts", "const { search } = location;\nexport { search };")
    ).toEqual([`features/inbox/probe-query.ts:1 ${what}`]);
    expect(refusals("features/inbox/probe-query.ts", "export const here = location;")).toEqual([
      `features/inbox/probe-query.ts:1 ${what}`,
    ]);
    // A read through the global is the other mark's, reported once; the page address under a
    // name of its own, a comparison against the global and a feature outside the seven are
    // left alone.
    expect(refusals("features/inbox/probe-query.ts", "export const q = location.search;")).toEqual([
      "features/inbox/probe-query.ts:1 reads the browser's own location through the bare global (`location.search`) — `lib/page-address.ts`'s `usePageAddress`, under a name of its own, since a bare `location` is the browser's",
    ]);
    expect(
      refusals("features/inbox/probe-query.ts", "const { search } = usePageAddress();")
    ).toEqual([]);
    expect(
      refusals("features/architecture/probe-query.ts", "export const here = location;")
    ).toEqual([]);
  });

  test('a search box spelled `type={"search"}` is refused', () => {
    const source = 'export const Probe = () => <input aria-label="Search" type={"search"} />;';
    expect(refusals("features/inbox/Probe.tsx", source)).toEqual([
      'features/inbox/Probe.tsx:1 writes a search box of its own (`type="search"`) — draw `components/SearchBox.tsx`',
    ]);
  });

  test("a page of the seven that reads the bare global `location.search` is refused", () => {
    const source = [
      "export function probeQuery(): string {",
      "  return location.search;",
      "}",
    ].join("\n");
    expect(refusals("features/inbox/probe-query.ts", source)).toEqual([
      "features/inbox/probe-query.ts:2 reads the browser's own location through the bare global (`location.search`) — `lib/page-address.ts`'s `usePageAddress`, under a name of its own, since a bare `location` is the browser's",
    ]);
    // The page address under a name of its own, and a feature outside the seven, are left alone.
    const named = ["const address = usePageAddress();", "export const search = address.search;"];
    expect(refusals("features/inbox/probe-query.ts", named.join("\n"))).toEqual([]);
    expect(refusals("features/architecture/probe-query.ts", source)).toEqual([]);
  });

  test("a value re-export of the table library after an `import type` is refused", () => {
    const source = [
      'import type { ColumnDef } from "@tanstack/react-table";',
      'export { useReactTable } from "@tanstack/react-table";',
    ].join("\n");
    expect(refusals("features/inbox/probe-table.ts", source)).toEqual([
      "features/inbox/probe-table.ts:2 imports `@tanstack/react-table` as a value — the one table is `components/DataTable.tsx`, which re-exports `flexRender`",
    ]);
    // A re-export of the library's types names types, as the `import type` does.
    const types = [
      'import type { ColumnDef } from "@tanstack/react-table";',
      'export type { Row } from "@tanstack/react-table";',
    ].join("\n");
    expect(refusals("features/inbox/probe-table.ts", types)).toEqual([]);
  });

  test('a box and a `<table` after `accept="image/*"` and before a doc comment are refused', () => {
    const source = [
      "export const Upload = () => (",
      "  <form>",
      '    <input accept="image/*" type="file" />',
      '    <input aria-label="Search" type="search" />',
      "    <table>",
      "      <tbody />",
      "    </table>",
      "  </form>",
      ");",
      "",
      "/** A later doc comment, whose close is where the old strip's comment ended. */",
      "export const after = 1;",
    ].join("\n");
    expect(refusals("features/inbox/Upload.tsx", source)).toEqual([
      'features/inbox/Upload.tsx:4 writes a search box of its own (`type="search"`) — draw `components/SearchBox.tsx`',
      "features/inbox/Upload.tsx:5 writes a `<table>` of its own — draw `components/TableShell.tsx` or `components/DataTable.tsx`",
    ]);
  });

  // INFRA-3135's review round 1, follow-up F4 (class 11): each spelling the guard missed, beside
  // a spelling it already caught, refused now. No live file under `features/` carries any of these.
  test("the `<Navigate>` mark catches an aliased import, a member use and `createElement`", () => {
    const what =
      "redirects with the router's own `<Navigate>` — `lib/page-address.ts`'s `AddressRedirect`";
    for (const source of [
      'import { Navigate as Go } from "react-router-dom";',
      'export const Gone = () => <Router.Navigate replace to="/x" />;',
      'export const Gone = () => createElement(Navigate, { to: "/x" });',
    ]) {
      expect(refusals("features/inbox/Gone.tsx", source)).toEqual([
        `features/inbox/Gone.tsx:1 ${what}`,
      ]);
    }
  });

  test("the link-query mark catches a query string, a template query and a `search` object, on Link and NavLink", () => {
    const what =
      "builds a link's query by hand (a `<Link>` to a query) — `lib/page-address.ts`'s `copyAddress` and `addressPath`";
    for (const source of [
      'export const L = () => <Link to="?label=bug">Bugs</Link>;',
      // biome-ignore lint/suspicious/noTemplateCurlyInString: this string is source the guard must refuse, not a template
      "export const L = () => <Link to={`?${query}`}>Bugs</Link>;",
      "export const L = () => <Link to={{ search: query }}>Bugs</Link>;",
      'export const L = () => <NavLink to="?label=bug">Bugs</NavLink>;',
    ]) {
      expect(refusals("features/inbox/Links.tsx", source)).toEqual([
        `features/inbox/Links.tsx:1 ${what}`,
      ]);
    }
    // A plain path, and a link outside the seven, are left alone.
    expect(refusals("features/inbox/Links.tsx", '<Link to="/issues/CORE-1">Open</Link>')).toEqual(
      []
    );
    expect(refusals("features/architecture/Links.tsx", "<Link to={{ search: query }} />")).toEqual(
      []
    );
  });

  test("the `new URLSearchParams` mark catches the window and globalThis globals", () => {
    const what =
      "builds the address directly (`new URLSearchParams`) — `lib/page-address.ts`'s `copyAddress`";
    for (const global of ["window", "globalThis"]) {
      expect(
        refusals("features/inbox/probe.ts", `export const p = new ${global}.URLSearchParams(x);`)
      ).toEqual([`features/inbox/probe.ts:1 ${what}`]);
    }
  });

  test("the `window.location` mark catches optional chaining, nested window names, the bracket form and `document.URL`", () => {
    const what =
      "reads or writes the address through the browser's own location (`window.location`) — `lib/page-address.ts`'s `usePageAddress` to read it, `useAddressUpdate` or `useAddressNavigate` to write it";
    for (const source of [
      "export const q = window?.location.search;",
      "export const q = window.top.location.search;",
      "export const q = window.parent.location.search;",
      "export const q = window.self.location.search;",
      'export const q = window["location"].search;',
      "export const q = document.URL;",
    ]) {
      expect(refusals("features/inbox/probe.ts", source)).toEqual([
        `features/inbox/probe.ts:1 ${what}`,
      ]);
    }
  });

  test("the bare `location` mark catches the non-null assertion and the bracket form", () => {
    const what =
      "reads the browser's own location through the bare global (`location.search`) — `lib/page-address.ts`'s `usePageAddress`, under a name of its own, since a bare `location` is the browser's";
    for (const source of [
      "export const q = location!.search;",
      'export const q = location["search"];',
    ]) {
      expect(refusals("features/inbox/probe.ts", source)).toEqual([
        `features/inbox/probe.ts:1 ${what}`,
      ]);
    }
  });

  test("the history mark catches optional chaining and the bracket form", () => {
    const what =
      "writes the address through the browser's own history (`history.pushState` / `history.replaceState`), behind the router's back — `lib/page-address.ts`'s `useAddressUpdate` or `useAddressNavigate`";
    for (const source of [
      'history?.pushState(null, "", url);',
      'history["replaceState"](null, "", url);',
    ]) {
      expect(refusals("features/inbox/probe.ts", source)).toEqual([
        `features/inbox/probe.ts:1 ${what}`,
      ]);
    }
  });

  test('the search-box mark catches a spaced attribute, an object `type`, and `role="searchbox"`', () => {
    const what =
      'writes a search box of its own (`type="search"`) — draw `components/SearchBox.tsx`';
    for (const source of [
      'export const B = () => <input type = "search" />;',
      'export const o = { type: "search" };',
      'export const B = () => <div role="searchbox" />;',
    ]) {
      expect(refusals("features/inbox/Box.tsx", source)).toEqual([
        `features/inbox/Box.tsx:1 ${what}`,
      ]);
    }
  });

  test('the table mark catches `createElement("table")` and a div table\'s role', () => {
    const what =
      "writes a `<table>` of its own — draw `components/TableShell.tsx` or `components/DataTable.tsx`";
    for (const source of [
      'export const T = () => createElement("table");',
      'export const T = () => <div role="table" />;',
      'export const T = () => <div role="grid" />;',
    ]) {
      expect(refusals("features/inbox/T.tsx", source)).toEqual([`features/inbox/T.tsx:1 ${what}`]);
    }
  });

  test("the `data-control` mark catches `dataset.control` and the bracket form", () => {
    const what =
      "writes a `data-control` attribute by hand — a page draws the control, which carries its own name from `components/control-names.ts`";
    for (const source of [
      "export const c = node.dataset.control;",
      'export const c = node.dataset["control"];',
    ]) {
      expect(refusals("features/inbox/probe.ts", source)).toEqual([
        `features/inbox/probe.ts:1 ${what}`,
      ]);
    }
    // A longer dataset key is not this one.
    expect(
      refusals("features/inbox/probe.ts", "export const r = node.dataset.controlRow;")
    ).toEqual([]);
  });

  test("the AnchoredPanel mark catches a `.js` specifier", () => {
    const what =
      "imports `components/AnchoredPanel` — open a panel through `components/PanelTrigger.tsx`, `components/DisplayMenu.tsx` or `components/MultiSelect.tsx`";
    expect(
      refusals(
        "features/inbox/P.tsx",
        'import { AnchoredPanel } from "../../components/AnchoredPanel.js";'
      )
    ).toEqual([`features/inbox/P.tsx:1 ${what}`]);
  });

  test("a value import of the table core, or a subpath of the table library, is refused", () => {
    expect(
      refusals(
        "features/inbox/probe-table.ts",
        'import { createTable } from "@tanstack/table-core";'
      )
    ).toEqual([
      "features/inbox/probe-table.ts:1 imports `@tanstack/table-core` as a value — the one table is `components/DataTable.tsx`, which re-exports `flexRender`",
    ]);
    expect(
      refusals(
        "features/inbox/probe-table.ts",
        'import { useReactTable } from "@tanstack/react-table/build";'
      )
    ).toEqual([
      "features/inbox/probe-table.ts:1 imports `@tanstack/react-table/build` as a value — the one table is `components/DataTable.tsx`, which re-exports `flexRender`",
    ]);
    // A type-only import of the core is a page naming its types, left alone.
    expect(
      refusals(
        "features/inbox/probe-table.ts",
        'import type { Table } from "@tanstack/table-core";'
      )
    ).toEqual([]);
  });

  test.each(FEATURE_FILES)("%s draws the shared controls", (relativePath, file) => {
    expect(codeRefusals(relativePath, codeOf(relativePath, file))).toEqual([]);
  });

  test("a `declare module` of the table library that adds anything but `ColumnMeta` is refused", () => {
    const declaring = (body: string) =>
      [
        'import type { RowData } from "@tanstack/react-table";',
        "",
        `declare module "@tanstack/react-table" {${body}}`,
      ].join("\n");
    const refusal =
      "features/inbox/probe-table.ts:3 declares `@tanstack/react-table` with more than the one `ColumnMeta` it may add — the one table is `components/DataTable.tsx`, which re-exports `flexRender`";
    expect(
      refusals(
        "features/inbox/probe-table.ts",
        declaring("\n  interface TableMeta<TData extends RowData> {\n    rows: TData[];\n  }\n")
      )
    ).toEqual([refusal]);
    expect(
      refusals(
        "features/inbox/probe-table.ts",
        declaring(
          "\n  interface ColumnMeta<TData extends RowData, TValue> {\n    show?: (value: TValue) => string;\n  }\n  export function useReactTable(): TData;\n"
        )
      )
    ).toEqual([refusal]);
    // The one augmentation a page may write: `ColumnMeta` alone, its body holding braces of its
    // own, as the project List's columns write it.
    expect(
      refusals(
        "features/inbox/probe-table.ts",
        declaring(
          "\n  interface ColumnMeta<TData extends RowData, TValue> {\n    show?: (value: string) => string;\n    order?: { first: TData; then: TValue };\n  }\n"
        )
      )
    ).toEqual([]);
  });

  test("the one `declare module` of the table library is the project List's, which adds `ColumnMeta`", () => {
    const declaring = FEATURE_FILES.filter(([relativePath, file]) =>
      /\bdeclare\s+module\s+["'`]@tanstack\//.test(codeOf(relativePath, file))
    ).map(([relativePath]) => relativePath);
    expect(declaring).toEqual(["features/project/issues-table-columns.tsx"]);
  });
});

describe("the shared controls are defined once, under components/", () => {
  test("the one table re-exports the library's own `flexRender`, which a page draws its cells with", () => {
    expect(flexRender).toBe(libraryFlexRender);
  });

  /** Each shared control, by its component's name and the key of `CONTROL` that holds the
   *  `data-control` value it sets: the key in PascalCase is the component's name and its file's. */
  const SHARED_CONTROLS = Object.keys(CONTROL).map(
    (key) => [`${key.charAt(0).toUpperCase()}${key.slice(1)}`, key] as const
  );

  test.each(
    SHARED_CONTROLS
  )("%s is exported from components/ and sets its own `data-control`, and no page defines one", (name, key) => {
    const source = stripComments(
      readFileSync(join(import.meta.dir, `${name}.tsx`), "utf8"),
      `components/${name}.tsx`
    );
    expect(source).toMatch(new RegExp(`\\bexport\\s+function\\s+${name}\\b`));
    expect(source).toMatch(new RegExp(`\\bCONTROL\\.${key}\\b`));
    const defines = new RegExp(`\\b(?:function|const|let|class)\\s+${name}\\b`);
    const definedIn = FEATURE_FILES.filter(([relativePath, file]) =>
      defines.test(codeOf(relativePath, file))
    ).map(([relativePath]) => relativePath);
    expect(definedIn).toEqual([]);
  });
});
