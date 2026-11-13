import { expect, test } from "bun:test";
import ts from "typescript";

import { nodesOf, parsed, shippedTexts, siteOf, writtenName } from "../../__tests__/source-scan";

/**
 * No shipped module reads a cache past the live writes' changes (R7 and R10 of
 * dispatch://INFRA-299). The cache holds only Dispatch's answers; what the person sees is that
 * data with every live change composed over it as it is read (`Writes.overlay`). A reader that
 * skips the composition shows Dispatch's answer without the person's unsaved change - a
 * stale-looking value, never a corrupt one - so the guard holds the doors a reader could skip it
 * by, as the compiler parses each module (`source-scan.ts`): a name in a comment, a string, a
 * regex literal or JSX text is no read, and nothing before it hides what follows.
 *
 * - TanStack's query hooks and observers (`QUERY_READERS`) are imported, re-exported or reached as
 *   a member nowhere but the wrapper (`shown-query.ts`), whatever key they read. Which caches a
 *   change names is decided as the page runs - `Shown.under` names every cache under a prefix,
 *   and keys are built by factories - so no reading of the source can tell a hook read of a key
 *   no write names from one a later write will; a read of such a key through the wrapper costs
 *   one look at the live changes, and hands back the data itself. The one observer outside it,
 *   `reference-target.ts`'s, is named below with its reason.
 * - A bare `getQueryData` or `getQueriesData` - a call, a member, a string a bracket reads, a
 *   name taken out of an object - reads the cache without the composition, so it is made nowhere
 *   but the store (`Writes.shown` is the read that composes) and the reads that want Dispatch's
 *   answer, each named below with its reason and its count, so one more read in such a module is
 *   named too.
 * - `hydrate` and `HydrationBoundary` write a query's state wholesale, under no write's rule.
 *
 * A write into the cache is no longer held: the cache is Dispatch's, and a merge of Dispatch's
 * answer into it is a plain `setQueryData`. A local function or a React state setter named
 * `setData`, say, is nobody's concern here. The guard's reach is the module's own source: a read
 * built to evade it by a name made at run time is the reviewer's to catch.
 */

/** The wrapper, the one module that imports TanStack's query hooks. */
const WRAPPER = "features/writes/shown-query.ts";
/** The store, whose `shown` composes what it reads. */
const STORE = "features/writes/write-store.ts";
/** Where TanStack's query reads come from. */
const LIBRARY = new Set(["@tanstack/react-query", "@tanstack/query-core"]);

/** Every hook and observer that reads a cache into a component or a listener. */
const QUERY_READERS = new Set([
  "InfiniteQueryObserver",
  "QueriesObserver",
  "QueryObserver",
  "useInfiniteQuery",
  "usePrefetchInfiniteQuery",
  "usePrefetchQuery",
  "useQueries",
  "useQuery",
  "useSuspenseInfiniteQuery",
  "useSuspenseQueries",
  "useSuspenseQuery",
]);
/** What writes a query's state wholesale. */
const HYDRATION = new Set(["HydrationBoundary", "hydrate"]);
/** What reads a cache's data bare. */
const BARE_READS = new Set(["getQueriesData", "getQueryData"]);

/** The modules a bare read stands in, with how many and why each wants what the cache holds
 *  rather than what the person sees. */
const BARE_READ_SITES: ReadonlyMap<string, { readonly reads: number; readonly why: string }> =
  new Map([
    [
      "api/event-feed.ts",
      {
        reads: 2,
        why: "who Dispatch says is signed in (`whoami`), which no write's change names",
      },
    ],
    [
      "features/credentials/pending.ts",
      {
        reads: 1,
        why: "whether Dispatch answered the credential list with `null`, which no write's change names",
      },
    ],
    [
      "features/inbox/useAskHandOn.ts",
      { reads: 1, why: "who is signed in (`whoami`), which no write's change names" },
    ],
    [
      "features/issue/useIssueAssignee.ts",
      {
        reads: 1,
        why: "each project's default assignee, read inside the change itself, which no write's change names",
      },
    ],
    [
      "features/refs/reference-target.ts",
      {
        reads: 4,
        why: "the document a reference warms its text for, by id and version, which no write's change names",
      },
    ],
  ]);

/** The modules a query observer stands in outside the wrapper, with how many and why. */
const OBSERVER_SITES: ReadonlyMap<string, { readonly reads: number; readonly why: string }> =
  new Map([
    [
      "features/refs/reference-target.ts",
      {
        reads: 1,
        why: "a document picture's summary for an editor's node view outside React, which no write's change names",
      },
    ],
  ]);

/** One door a module reads a cache through: where, which name, and which rule it breaks. */
interface Door {
  readonly at: string;
  readonly name: string;
  readonly rule: "hook" | "observer" | "hydration" | "bare read" | "namespace";
}

/** The rule `name`, taken from the library or reached as a member, falls under, if any. */
function ruleOf(name: string): Door["rule"] | undefined {
  if (HYDRATION.has(name)) return "hydration";
  if (name.endsWith("Observer") && QUERY_READERS.has(name)) return "observer";
  if (QUERY_READERS.has(name)) return "hook";
  return undefined;
}

/** Whether `node` is the module name of a declaration that imports or re-exports the library. */
function fromLibrary(specifier: ts.Expression | undefined): boolean {
  return specifier !== undefined && ts.isStringLiteral(specifier) && LIBRARY.has(specifier.text);
}

/** Every door `source`, the module at `path`, reads a cache through, in source order. */
function doorsIn(path: string, source: string): Door[] {
  const file = parsed(path, source);
  const doors: Door[] = [];
  const add = (node: ts.Node, name: string, rule: Door["rule"] | undefined) => {
    if (rule !== undefined) doors.push({ at: siteOf(file, node), name, rule });
  };
  for (const node of nodesOf(file)) {
    if (ts.isImportDeclaration(node) && fromLibrary(node.moduleSpecifier)) {
      const bindings = node.importClause?.namedBindings;
      if (bindings !== undefined && ts.isNamespaceImport(bindings)) {
        add(bindings, bindings.name.text, "namespace");
      }
    } else if (
      (ts.isImportSpecifier(node) || ts.isExportSpecifier(node)) &&
      fromLibrary(
        ts.isImportSpecifier(node)
          ? node.parent.parent.parent.moduleSpecifier
          : node.parent.parent.moduleSpecifier
      )
    ) {
      const name = (node.propertyName ?? node.name).text;
      add(node, name, ruleOf(name));
    } else if (
      ts.isExportDeclaration(node) &&
      node.exportClause === undefined &&
      fromLibrary(node.moduleSpecifier)
    ) {
      add(node, "*", "namespace");
    } else if (
      ts.isCallExpression(node) &&
      node.expression.kind === ts.SyntaxKind.ImportKeyword &&
      fromLibrary(node.arguments[0])
    ) {
      add(node, "import()", "namespace");
    } else {
      const name = writtenName(node);
      if (name === undefined) continue;
      add(node, name, BARE_READS.has(name) ? "bare read" : ruleOf(name));
    }
  }
  return doors;
}

/** Every door of `sources` but those the wrapper, the store and the named sites stand in, one
 *  line each, naming its file and line. */
function bareReads(sources: ReadonlyMap<string, string>): string[] {
  const found: string[] = [];
  for (const [path, source] of sources) {
    const doors = doorsIn(path, source);
    const allowed = (rule: Door["rule"], sites: typeof BARE_READ_SITES) => {
      const site = sites.get(path);
      const of = doors.filter((door) => door.rule === rule);
      return site !== undefined && of.length === site.reads;
    };
    const readsAllowed = path === STORE || allowed("bare read", BARE_READ_SITES);
    const observersAllowed = path === WRAPPER || allowed("observer", OBSERVER_SITES);
    for (const door of doors) {
      if (door.rule === "hook" && path === WRAPPER) continue;
      if (door.rule === "bare read" && readsAllowed) continue;
      if (door.rule === "observer" && observersAllowed) continue;
      found.push(`${door.at} ${door.name} (${door.rule})`);
    }
  }
  return found;
}

test("no shipped module reads a cache past the live writes' changes", () => {
  expect(bareReads(shippedTexts())).toEqual([]);
});

// The guard has to see what it guards: read as any module is, the wrapper's source is full of the
// hooks it alone may import, and each named site holds the reads it is named for.
test("the guard reads the wrapper's hooks and every named site's reads", () => {
  const sources = shippedTexts();
  const hooks = doorsIn(WRAPPER, sources.get(WRAPPER) ?? "").filter((door) => door.rule === "hook");
  expect(hooks.map((door) => door.name)).toEqual(
    expect.arrayContaining(["useInfiniteQuery", "useQueries", "useQuery"])
  );
  for (const [path, { reads }] of BARE_READ_SITES) {
    const doors = doorsIn(path, sources.get(path) ?? "");
    expect({ path, reads: doors.filter((door) => door.rule === "bare read").length }).toEqual({
      path,
      reads,
    });
  }
});

// Every spelling a reader could reach for is named by file and line, each one after an apostrophe
// in JSX text and a quote in a regex literal, which a reading of the source as text took for the
// start of a string running to the end of the file; a React state setter named `setData`, a
// comment, a string and JSX text that only mention a name are not.
test("every spelling of a bare cache read is named by file and line", () => {
  const sources = new Map(shippedTexts());
  const where = "features/issue/read-past.tsx";
  sources.set(
    where,
    [
      'import type { QueryClient } from "@tanstack/react-query";',
      'import { HydrationBoundary, hydrate, useQuery as read } from "@tanstack/react-query";',
      'import * as Query from "@tanstack/query-core";',
      'import { type ReactNode, useState } from "react";',
      "",
      "export function ReadPast({ client }: { client: QueryClient }): ReactNode {",
      "  const [data, setData] = useState(0);",
      "  setData(data + 1);",
      '  const quote = /"/;',
      '  const state = client.getQueryData(["user-state"]);',
      "  // client.getQueryData(['user-state']) in a comment is no read,",
      '  const said = "and a string that says getQueryData is none either";',
      "  void [read, Query, quote, state, said];",
      "  return (",
      "    <HydrationBoundary state={undefined}>",
      "      <p>Don't read getQueryData here: JSX text is no read.</p>",
      '      {String(client.getQueriesData({ queryKey: ["inbox"] }))}',
      "    </HydrationBoundary>",
      "  );",
      "}",
      "",
      'export const bracket = (client: QueryClient) => client["getQueryData"](["inbox"]);',
      "export function taken(client: QueryClient): unknown {",
      "  const { getQueryData } = client;",
      "  hydrate(client, { mutations: [], queries: [] });",
      '  return getQueryData(["inbox"]);',
      "}",
      'export { useInfiniteQuery } from "@tanstack/react-query";',
      'export const later = () => import("@tanstack/react-query");',
      "",
    ].join("\n")
  );
  expect(bareReads(sources)).toEqual([
    `${where}:2 HydrationBoundary (hydration)`,
    `${where}:2 hydrate (hydration)`,
    `${where}:2 useQuery (hook)`,
    `${where}:3 Query (namespace)`,
    `${where}:10 getQueryData (bare read)`,
    `${where}:17 getQueriesData (bare read)`,
    `${where}:22 getQueryData (bare read)`,
    `${where}:24 getQueryData (bare read)`,
    `${where}:28 useInfiniteQuery (hook)`,
    `${where}:29 import() (namespace)`,
  ]);
});

// R7 of dispatch://INFRA-299: no reader is asked to remember anything, so every ordinary way a
// module can read a cache's data past the live changes is a door this guard holds. TanStack
// hands a cache's data back by three calls besides `getQueryData` and `getQueriesData`, none of
// them a name made at run time, and each is already called in shipped code: `getQueryState`
// (`api/event-feed.ts`), `fetchQuery` with a staleTime that answers from the cache
// (`features/project/issues-table-data.ts`), and a found query's `state.data`
// (`api/query-refresh.ts`). A reader written through any of them shows Dispatch's answer
// without the person's unsaved change, which is the fault this guard exists to stop.
test("a cache read bare through getQueryState, fetchQuery or a found query's state is named too", () => {
  const sources = new Map(shippedTexts());
  const where = "features/issue/read-past-state.ts";
  sources.set(
    where,
    [
      'import type { QueryClient } from "@tanstack/react-query";',
      'import { userStateQuery } from "../../api/queries";',
      "",
      'export const byState = (client: QueryClient) => client.getQueryState(["user-state"])?.data;',
      "export const byFetch = (client: QueryClient) =>",
      "  client.fetchQuery({ ...userStateQuery(), staleTime: Number.POSITIVE_INFINITY });",
      "export const byCache = (client: QueryClient) =>",
      '  client.getQueryCache().find({ queryKey: ["user-state"] })?.state.data;',
      "",
    ].join("\n")
  );
  const lines = bareReads(sources)
    .filter((door) => door.startsWith(`${where}:`))
    .map((door) => door.split(" ")[0]);
  expect(lines).toEqual([`${where}:4`, `${where}:6`, `${where}:8`]);
});
