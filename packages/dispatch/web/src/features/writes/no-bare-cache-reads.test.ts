import { expect, test } from "bun:test";
import ts from "typescript";

import { nodesOf, parsed, shippedTexts, siteOf, writtenName } from "../../__tests__/source-scan";

/**
 * No shipped module reads a cache past the live writes' changes (R7 and R10 of
 * dispatch://INFRA-299). The cache holds only Dispatch's answers; what the person sees is that
 * data with every live change composed over it as it is read (`Writes.overlay`). Every read of a
 * cache's data is one of two named reads: the screen, Dispatch's answer with the live changes
 * over it (the wrapper's hooks and `watchShownQuery`, `shown-query.ts`, and `Writes.shown`), or
 * Dispatch's answer alone (`Writes.dispatched`, `fetchDispatched`, `Writes.dispatchedOf`). A
 * reader that skips both shows Dispatch's answer without the person's unsaved change - a
 * stale-looking value, never a corrupt one - and leaves which of the two it meant unsaid, so the
 * guard holds every door a reader could take past them, as the compiler parses each module
 * (`source-scan.ts`): a name in a comment, a string, a regex literal or JSX text is no read, and
 * nothing before it hides what follows. No module is let through by name but the wrapper and the
 * store, which hold the named reads:
 *
 * - TanStack's query hooks and observers (`QUERY_READERS`) are imported, re-exported or reached as
 *   a member nowhere but the wrapper, whatever key they read. Which caches a change names is
 *   decided as the page runs - `Shown.under` names every cache under a prefix, and keys are built
 *   by factories - so no reading of the source can tell a read of a key no write names from one a
 *   later write will.
 * - A bare read of a cache's data is made nowhere but the store: `getQueryData` and
 *   `getQueriesData`, and the fetches that hand the data back (`fetchQuery`,
 *   `fetchInfiniteQuery`, `ensureQueryData`, `ensureInfiniteQueryData`, and a method call of
 *   `query` or `infiniteQuery`, TanStack's newer names for them) - a call, a member, a string a
 *   bracket reads, a name taken out of an object; a `getQueryState(...)` read for anything but a
 *   member other than `data` or an equality test, since what a variable holding it is read for
 *   later is past what one node can tell; and a query's `state.data` reached any way - a found
 *   query's, one a predicate, an option's callback or a cache iteration is handed, `{ data }`
 *   taken out of a `state`, and `.data` of a local holding a `state`: one set to a `state`
 *   member, or taken out of a parameter or a loop's item (`({ state }) =>`, `for (const { state }
 *   of`). A `state` taken out of what a call answered - a hook read's result - is no query's.
 * - `hydrate` and `HydrationBoundary` write a query's state wholesale, under no write's rule.
 *
 * A query's state that is no data is read freely: `getQueryState(key)?.fetchStatus`, whether a
 * query exists (`getQueryState(key) === undefined`), `query.state.status`, its `dataUpdatedAt`.
 * A write into the cache is no longer held: the cache is Dispatch's, and a merge of Dispatch's
 * answer into it is a plain `setQueryData`. A local function or a React state setter named
 * `setData`, say, is nobody's concern here. The guard's reach is the module's own source: a read
 * built to evade it by a name made at run time, a query's `state` passed on whole, or a query's
 * own `fetch` and `promise`, is the reviewer's to catch.
 */

/** The wrapper, the one module that reads through TanStack's query hooks and an observer. */
const WRAPPER = "features/writes/shown-query.ts";
/** The store, whose `shown` composes what it reads and whose `dispatched` reads it alone. */
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
/** What reads a cache's data bare, however its name is reached. */
const BARE_READS = new Set([
  "ensureInfiniteQueryData",
  "ensureQueryData",
  "fetchInfiniteQuery",
  "fetchQuery",
  "getQueriesData",
  "getQueryData",
]);
/** What hands a cache's data back when called as a method: names too plain to hold elsewhere. */
const BARE_READ_CALLS = new Set(["infiniteQuery", "query"]);
/** The comparisons a query's state is tested for existence by. */
const EQUALITY = new Set([
  ts.SyntaxKind.EqualsEqualsEqualsToken,
  ts.SyntaxKind.EqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsToken,
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

/** Where a door through `node` is named: a member's own name, so a chain over several lines
 *  names the line the read is written on. */
function nameNode(node: ts.Node): ts.Node {
  if (ts.isPropertyAccessExpression(node)) return node.name;
  if (ts.isElementAccessExpression(node)) return node.argumentExpression;
  return node;
}

/** Whether `node`, a member access, is what a call calls. */
function called(node: ts.Node): boolean {
  return ts.isCallExpression(node.parent) && node.parent.expression === node;
}

/** Whether the `getQueryState` member `node` reaches is called and its result read for no data:
 *  a member other than `data` (`fetchStatus`, `status`), or an equality test (whether it
 *  exists). */
function readsNoData(node: ts.Node): boolean {
  if (!called(node)) return false;
  const call = node.parent;
  const use = call.parent;
  if (
    (ts.isPropertyAccessExpression(use) || ts.isElementAccessExpression(use)) &&
    use.expression === call
  ) {
    const name = writtenName(use);
    return name !== undefined && name !== "data";
  }
  return ts.isBinaryExpression(use) && EQUALITY.has(use.operatorToken.kind);
}

/** The node a local declared at `declaration` is seen in: its function, block, loop or module. */
function scopeOf(declaration: ts.Node): ts.Node {
  let node = declaration.parent;
  while (
    !ts.isSourceFile(node) &&
    !ts.isFunctionLike(node) &&
    !ts.isBlock(node) &&
    !ts.isIterationStatement(node, false)
  ) {
    node = node.parent;
  }
  return node;
}

/** Whether the binding `node` is a parameter's or a loop's over a collection: where a query is
 *  handed to a reader - a predicate, an option's callback, a cache iteration - rather than a
 *  value a call answered, such as a hook read's result. */
function handedIn(node: ts.BindingElement): boolean {
  let root: ts.Node = node;
  while (ts.isBindingElement(root) || ts.isObjectBindingPattern(root)) root = root.parent;
  return (
    ts.isParameter(root) ||
    (ts.isVariableDeclaration(root) &&
      (ts.isForOfStatement(root.parent.parent) || ts.isForInStatement(root.parent.parent)))
  );
}

/** Every local of `file` that holds a query's `state`, by its name, with the nodes it is seen in:
 *  one a query handed in is taken apart for (`({ state }) =>`, `for (const { state: held } of`),
 *  or one set to a `state` member. */
function stateLocals(file: ts.SourceFile): Map<string, ts.Node[]> {
  const locals = new Map<string, ts.Node[]>();
  for (const node of nodesOf(file)) {
    const holds =
      (ts.isBindingElement(node) && writtenName(node) === "state" && handedIn(node)) ||
      (ts.isVariableDeclaration(node) &&
        node.initializer !== undefined &&
        writtenName(node.initializer) === "state");
    if (holds && ts.isIdentifier(node.name)) {
      locals.set(node.name.text, [...(locals.get(node.name.text) ?? []), scopeOf(node)]);
    }
  }
  return locals;
}

/** Whether `node` holds a query's `state` where `at` reads it: a `state` member, or a local that
 *  holds one (`locals`) seen there. */
function holdsState(
  node: ts.Node,
  at: ts.Node,
  locals: ReadonlyMap<string, readonly ts.Node[]>
): boolean {
  if (writtenName(node) === "state") return true;
  const scopes = ts.isIdentifier(node) ? locals.get(node.text) : undefined;
  return scopes?.some((scope) => scope.pos <= at.pos && at.end <= scope.end) ?? false;
}

/** Whether `node` reads `data` out of a query's `state`: `query.state.data`, `query.state?.data`,
 *  `query["state"]["data"]`, `{ data }` taken out of a `state` or out of `{ state: … }`, and
 *  `.data` of a local that holds a `state`. */
function readsStateData(node: ts.Node, locals: ReadonlyMap<string, readonly ts.Node[]>): boolean {
  if (writtenName(node) !== "data") return false;
  if (ts.isPropertyAccessExpression(node) || ts.isElementAccessExpression(node)) {
    return holdsState(node.expression, node, locals);
  }
  if (!ts.isBindingElement(node)) return false;
  const holder = node.parent.parent;
  if (ts.isBindingElement(holder)) return writtenName(holder) === "state";
  return (
    ts.isVariableDeclaration(holder) &&
    holder.initializer !== undefined &&
    holdsState(holder.initializer, node, locals)
  );
}

/** The bare-read rule `node`, a written name, falls under, if any. */
function bareReadOf(node: ts.Node, name: string, locals: ReadonlyMap<string, readonly ts.Node[]>) {
  if (BARE_READS.has(name)) return name;
  if (BARE_READ_CALLS.has(name) && ts.isPropertyAccessExpression(node) && called(node)) return name;
  if (name === "getQueryState" && !readsNoData(node)) return name;
  if (readsStateData(node, locals)) return "state.data";
  return undefined;
}

/** Every door `source`, the module at `path`, reads a cache through, in source order. */
function doorsIn(path: string, source: string): Door[] {
  const file = parsed(path, source);
  const locals = stateLocals(file);
  const doors: Door[] = [];
  const add = (node: ts.Node, name: string, rule: Door["rule"] | undefined) => {
    if (rule !== undefined) doors.push({ at: siteOf(file, nameNode(node)), name, rule });
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
      const bare = bareReadOf(node, name, locals);
      if (bare === undefined) add(node, name, ruleOf(name));
      else add(node, bare, "bare read");
    }
  }
  return doors;
}

/** Every door of `sources` but the wrapper's hooks and observer and the store's bare reads, one
 *  line each, naming its file and line. */
function bareReads(sources: ReadonlyMap<string, string>): string[] {
  const found: string[] = [];
  for (const [path, source] of sources) {
    for (const door of doorsIn(path, source)) {
      if ((door.rule === "hook" || door.rule === "observer") && path === WRAPPER) continue;
      if (door.rule === "bare read" && path === STORE) continue;
      found.push(`${door.at} ${door.name} (${door.rule})`);
    }
  }
  return found;
}

test("no shipped module reads a cache past the live writes' changes", () => {
  expect(bareReads(shippedTexts())).toEqual([]);
});

// The guard has to see what it lets through: read as any module is, the wrapper's source is full
// of the hooks and the observer it alone may import, and the store's of the bare reads its named
// reads are made of.
test("the guard reads the wrapper's hooks and observer and the store's bare reads", () => {
  const sources = shippedTexts();
  const wrapper = doorsIn(WRAPPER, sources.get(WRAPPER) ?? "");
  expect(wrapper.filter((door) => door.rule !== "namespace").map((door) => door.name)).toEqual(
    expect.arrayContaining(["QueryObserver", "useInfiniteQuery", "useQueries", "useQuery"])
  );
  const store = doorsIn(STORE, sources.get(STORE) ?? "").filter(
    (door) => door.rule === "bare read"
  );
  expect(store.map((door) => door.name)).toEqual(
    expect.arrayContaining(["fetchQuery", "getQueryData", "getQueryState", "state.data"])
  );
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

// No module is let through by name: one that holds the reads the guard leaves - a query's state
// that is no data (`api/event-feed.ts`'s `fetchStatus` and existence checks), the named reads
// (`features/refs/reference-target.ts`'s `dispatched`, `fetchDispatched` and its observer through
// the wrapper) - has one more bare read named all the same, and that one alone.
test("a bare read in a module that holds the reads the guard leaves is named", () => {
  for (const where of ["api/event-feed.ts", "features/refs/reference-target.ts"]) {
    const sources = new Map(shippedTexts());
    const source = sources.get(where) ?? "";
    const line = source.split("\n").length;
    sources.set(
      where,
      `${source}export const leaked = (client: QueryClient) => client.getQueryData(["whoami"]);\n`
    );
    expect({ where, reads: bareReads(sources) }).toEqual({
      where,
      reads: [`${where}:${line} getQueryData (bare read)`],
    });
  }
});

// A query's state that is no data, and the two named reads, are no door; a query's `state`
// taken out of an object, under its own name or another, and read for its data is one, as is a
// `getQueryState` result kept to be read later, TanStack's newer `query` fetch, and an observer
// outside the wrapper.
test("a state read for no data and the named reads pass, and every other way to the data is named", () => {
  const sources = new Map(shippedTexts());
  const where = "features/issue/read-state.ts";
  sources.set(
    where,
    [
      'import { type Query, type QueryClient, QueryObserver } from "@tanstack/react-query";',
      'import { Writes, writesOf } from "../writes/write-store";',
      "",
      "export const passing = (client: QueryClient, query: Query) => [",
      '  client.getQueryState(["inbox"])?.fetchStatus === "fetching",',
      '  client.getQueryState(["inbox"]) !== undefined,',
      '  query.state.status === "error" && query.state.dataUpdatedAt > 0,',
      '  writesOf(client).dispatched(["inbox"]),',
      '  writesOf(client).shown(["inbox"]),',
      "  Writes.dispatchedOf(query),",
      "];",
      "export const interval = ({ state }: Query) => (state.data === null ? false : 1000);",
      "export function walk(client: QueryClient, query: Query): unknown[] {",
      "  const found: unknown[] = [];",
      "  for (const { state: held } of client.getQueryCache().getAll()) found.push(held.data);",
      "  const { data } = query.state;",
      "  const {",
      "    state: { data: kept },",
      "  } = query;",
      '  const state = client.getQueryState(["inbox"]);',
      '  found.push(data, kept, state, client.query({ queryKey: ["inbox"] }));',
      "  return [found, QueryObserver];",
      "}",
      "",
    ].join("\n")
  );
  expect(bareReads(sources).filter((door) => door.startsWith(`${where}:`))).toEqual([
    `${where}:1 QueryObserver (observer)`,
    `${where}:12 state.data (bare read)`,
    `${where}:15 state.data (bare read)`,
    `${where}:16 state.data (bare read)`,
    `${where}:18 state.data (bare read)`,
    `${where}:20 getQueryState (bare read)`,
    `${where}:21 query (bare read)`,
  ]);
});
