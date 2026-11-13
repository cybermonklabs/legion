import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { codeMask, shippedSources } from "../../__tests__/source-scan";

/**
 * No module but the write store writes a cache (R7 and R10 of dispatch://INFRA-299). The store
 * keeps Dispatch's copy of each cache a live write shows a change on apart from the screen, and
 * draws the screen as that copy with every live change over it (`Writes.#draw`); a write into
 * such a cache from outside the store would carry those changes into what the store takes as
 * Dispatch's, and a change that stopped being drawn would stay on the page. Which caches a change
 * is drawn on is decided as the page runs, so no module writes a cache by hand at all: every
 * answer goes through `Writes.answer`, which merges it into the copy where one is held and into
 * the cache where none is.
 *
 * So this file bans, in every module the app ships but the store, the names TanStack writes a
 * cache's data by, however they are spelled: `setQueryData`, `setQueriesData` and `setData` (a
 * query's own) anywhere in code - a call, a `.bind`, a `.call`, an `.apply`, a name taken out of
 * an object - or as a string a bracket reads (`client["setQueryData"]`), with whatever key, read
 * or not, a call names; and `setState` reached as a member of anything but `this`, read by a
 * bracket, or taken out of an object by name. A query's `setState` is the one any module can
 * reach as a member; `this.setState` is a React class component's own state (`ErrorBoundary`),
 * and a `setState` written as a bare name is a local function - a `useState` setter, a module's
 * own - which no query is. Comments and strings are read as JavaScript reads them
 * (`codeMask`), so a name a comment holds is no write, and one a template's `${…}` holds is.
 *
 * Test files and their `__tests__/` helpers are outside the ban (`shippedSources`): their
 * fixtures seed caches before any write is out.
 *
 * And every `structuralSharing` rule the app ships is set beneath the store's copies
 * (`answerSharing`): a rule judges an answer against the cache it lands on, which is the screen
 * where a live change is drawn, and only `answerSharing` judges it against Dispatch's copy. Every
 * rule is held to it, whatever cache it is on, since which caches a change is drawn on is decided
 * as the page runs; one on a cache no change is drawn on behaves as the bare rule does. A row
 * rather than a type: TanStack's option is typed `boolean | (old, new) => unknown`, and no
 * declaration this app can add narrows a property TanStack declares. `select` is not held to it:
 * it picks what a component reads out of the screen, which is what the page shows, and decides
 * nothing of what the cache or the copy holds.
 */

/** `web/src`, which every path below is read from. */
const webSrc = join(import.meta.dir, "..", "..");

/** The store itself, the one module that writes a cache. */
const STORE = "features/writes/write-store.ts";

/** The names a cache's data is written by. */
const WRITE_NAMES = /\b(setQueryData|setQueriesData|setData|setState)\b/g;
/** The same names, each written whole as a string. */
const WRITE_STRINGS = /(["'`])(setQueryData|setQueriesData|setData|setState)\1/g;

/** The line `index` is on in `source`, counted from one. */
function lineOf(source: string, index: number): number {
  return source.slice(0, index).split("\n").length;
}

/** The code character nearest before `index`, skipping blanks, comments and strings. */
function codeBefore(source: string, mask: readonly boolean[], index: number): string {
  for (let at = index - 1; at >= 0; at -= 1) {
    if (mask[at] === true && !/\s/.test(source[at] ?? "")) return source[at] ?? "";
  }
  return "";
}

/** The bracket that holds `index`, the innermost one still open there. */
function openBracket(source: string, mask: readonly boolean[], index: number): string {
  let depth = 0;
  for (let at = index - 1; at >= 0; at -= 1) {
    if (mask[at] !== true) continue;
    const ch = source[at] ?? "";
    if (")]}".includes(ch)) depth += 1;
    else if ("([{".includes(ch)) {
      if (depth === 0) return ch;
      depth -= 1;
    }
  }
  return "";
}

/** Whether the bare `setState` at `index` is a property named in an object - a literal or a
 *  pattern, `{ setState }`, `{ setState: write }` - rather than a local function. */
function namedInObject(source: string, mask: readonly boolean[], index: number): boolean {
  const before = codeBefore(source, mask, index);
  const after = source.slice(index + "setState".length);
  return (
    openBracket(source, mask, index) === "{" &&
    (before === "{" || before === ",") &&
    /^\s*(?:[:,}]|=(?![=>]))/.test(after)
  );
}

/** Whether the `setState` at `index` is a write into a query's state: one reached as a member of
 *  anything but `this`, or taken out of an object by name. */
function queryState(source: string, mask: readonly boolean[], index: number): boolean {
  const before = source.slice(0, index);
  if (/(?:\?\.|\.)\s*$/.test(before)) return !/\bthis\s*(?:\?\.|\.)\s*$/.test(before);
  return namedInObject(source, mask, index);
}

/** Every cache write in `source`, the module at `path`, as `path:line name`. */
function cacheWritesIn(path: string, source: string): string[] {
  const mask = codeMask(source);
  const found: { readonly index: number; readonly name: string }[] = [];
  for (const match of source.matchAll(WRITE_NAMES)) {
    const name = match[1] ?? "";
    if (mask[match.index] !== true) continue;
    if (name === "setState" && !queryState(source, mask, match.index)) continue;
    found.push({ index: match.index, name });
  }
  for (const match of source.matchAll(WRITE_STRINGS)) {
    if (mask[match.index] === true) found.push({ index: match.index, name: match[0] });
  }
  return found
    .sort((a, b) => a.index - b.index)
    .map(({ index, name }) => `${path}:${lineOf(source, index)} ${name}`);
}

/** What is wrong with the cache writes of `sources`, one line each, naming its file and line. */
function directCacheWrites(sources: ReadonlyMap<string, string>): string[] {
  return [...sources]
    .filter(([path]) => path !== STORE)
    .flatMap(([path, source]) => cacheWritesIn(path, source))
    .map((site) => `${site} writes a cache outside the write store: merge through Writes.answer`);
}

/** Every `structuralSharing` rule in `sources` set outside `answerSharing`, one line each. */
function rulesOutsideAnswerSharing(sources: ReadonlyMap<string, string>): string[] {
  const found: string[] = [];
  for (const [path, source] of sources) {
    if (path === STORE) continue;
    const mask = codeMask(source);
    for (const match of source.matchAll(/\bstructuralSharing\b/g)) {
      if (mask[match.index] !== true) continue;
      const value = source.slice(match.index + match[0].length);
      if (/^\s*:\s*(?:answerSharing\s*\(|true\b|false\b)/.test(value)) continue;
      found.push(
        `${path}:${lineOf(source, match.index)} sets a structuralSharing rule outside answerSharing`
      );
    }
  }
  return found;
}

/** Every module the app ships, by its path under `web/src`. */
function shipped(): ReadonlyMap<string, string> {
  return new Map(
    shippedSources().map((path) => [path, readFileSync(join(webSrc, path), "utf8")] as const)
  );
}

test("no shipped module but the store writes a cache", () => {
  expect(directCacheWrites(shipped())).toEqual([]);
});

// The ban has to see what it guards: read as any module is, the store's own source is full of
// the writes it alone may make.
test("the ban reads the store's own cache writes, which it alone may make", () => {
  const store = shipped().get(STORE) ?? "";
  const names = cacheWritesIn(STORE, store).map((site) => site.split(" ")[1]);
  expect(names).toContain("setQueryData");
  expect(names).toContain("setState");
});

// Every spelling the reviews planted, and the ones a writer could reach for next, is named by
// file and line; a React state, a local setter, a comment and a string that only mention a name
// are not.
test("every spelling of a direct cache write is named by file and line", () => {
  const sources = new Map(shipped());
  sources.set(
    "features/issue/merged-over.ts",
    [
      'import type { Query, QueryClient } from "@tanstack/react-query";',
      'import { Component, useState } from "react";',
      "",
      "export function mergeOver(client: QueryClient, query: Query, next: object): void {",
      '  client.setQueryData(["user-state"], (current: object) => ({ ...current, next }));',
      '  client.getQueryCache().find({ queryKey: ["user-state"] })?.setData(next);',
      "  query.setState({ ...query.state, data: next });",
      "  const write = client.setQueryData.bind(client);",
      '  client.setQueryData.call(client, ["user-state"], next);',
      '  client["setQueryData"](["user-state"], next);',
      '  client.setQueriesData({ queryKey: ["user-state"] }, next);',
      "  query?.setState({ ...query.state, data: next });",
      "  query['setState']({ ...query.state, data: next });",
      "  const { setState } = query;",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the planted module's own template.
      "  const label = `${client.setQueryData(['user-state'], next)}`;",
      "  // client.setQueryData(['user-state'], next) in a comment is no write,",
      '  const said = "and a string that says setQueryData is none either";',
      "  void [write, setState, label, said];",
      "}",
      "",
      "export function useLocal(): void {",
      "  const [state, setState] = useState(0);",
      "  setState(state + 1);",
      "}",
      "",
      "export class Boundary extends Component<object, { failed: boolean }> {",
      "  reset(): void {",
      "    this.setState({ failed: false });",
      "  }",
      "}",
      "",
    ].join("\n")
  );
  const where = "features/issue/merged-over.ts";
  expect(directCacheWrites(sources).map((line) => line.split(" writes ")[0])).toEqual([
    `${where}:5 setQueryData`,
    `${where}:6 setData`,
    `${where}:7 setState`,
    `${where}:8 setQueryData`,
    `${where}:9 setQueryData`,
    `${where}:10 "setQueryData"`,
    `${where}:11 setQueriesData`,
    `${where}:12 setState`,
    `${where}:13 'setState'`,
    `${where}:14 setState`,
    `${where}:15 setQueryData`,
  ]);
});

test("every structuralSharing rule the app ships is set beneath the store's copies", () => {
  expect(rulesOutsideAnswerSharing(shipped())).toEqual([]);
});

// The row reads the rules it holds: the issue's, which keeps out an issue older than the one
// held, is one; a bare rule and one spelled short are named.
test("a structuralSharing rule outside answerSharing is named by file and line", () => {
  const sources = new Map(shipped());
  expect(sources.get("api/queries.ts")).toMatch(/structuralSharing: answerSharing\(/);
  sources.set(
    "features/issue/bare-rule.ts",
    [
      'import { queryOptions } from "@tanstack/react-query";',
      "",
      "const structuralSharing = (_held: unknown, next: unknown) => next;",
      'export const bare = queryOptions({ queryKey: ["s"], structuralSharing: (_a, b) => b });',
      'export const short = queryOptions({ queryKey: ["s"], structuralSharing });',
      'export const plain = queryOptions({ queryKey: ["s"], structuralSharing: false });',
      "",
    ].join("\n")
  );
  expect(rulesOutsideAnswerSharing(sources)).toEqual([
    "features/issue/bare-rule.ts:3 sets a structuralSharing rule outside answerSharing",
    "features/issue/bare-rule.ts:4 sets a structuralSharing rule outside answerSharing",
    "features/issue/bare-rule.ts:5 sets a structuralSharing rule outside answerSharing",
  ]);
});
