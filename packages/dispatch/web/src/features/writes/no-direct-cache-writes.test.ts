import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { dirname, join, normalize } from "node:path";

import { shippedSources } from "../../__tests__/source-scan";

/**
 * No surface writes into a cache a write shows a change on but through the store (R7 and R10 of
 * dispatch://INFRA-299). The store keeps Dispatch's copy of each such cache apart from the screen
 * and draws the screen as that copy with every live change over it (`Writes.#draw`); a
 * `setQueryData` over the cache as it stands would carry those changes into what the store takes
 * as Dispatch's, and a refused change would stay on the page. So every merge of an answer goes
 * through `Writes.answer`, and this file reads every call of `setQueryData` and `setQueriesData`
 * the app ships, reads the key it writes - through the constants, the key factories and the
 * query options it names - and fails on each one whose key some `shown.on` or `shown.under`
 * names, naming the file and line of both. A write whose key it cannot read fails too, unless it
 * is one of the few let through below by name, each with the reason it writes no such cache.
 */

/** `web/src`, which every path below is read from. */
const webSrc = join(import.meta.dir, "..", "..");

/** One element of a key this file cannot read: any value. */
const ANY = Symbol("any element");
/** The rest of a key this file cannot read: any elements, however many. */
const REST = Symbol("any elements");
type Part = string | typeof ANY | typeof REST;
/** A key as this file reads it: each element a string it names, or one it cannot. */
type Key = readonly Part[];

/** A call that names a cache, where it is, and the keys it may name (`undefined`: unread). */
interface Site {
  readonly where: string;
  readonly text: string;
  /** Whether the call names every cache under its key (`setQueriesData`, `shown.under`) rather
   *  than the one cache of it. */
  readonly prefix: boolean;
  readonly keys: readonly Key[] | undefined;
}

/** The writes let through by their file and their key's text, each with why it writes no cache
 *  a write shows a change on. */
const letThrough: readonly { readonly path: string; readonly key: string; readonly why: string }[] =
  [
    {
      key: "sub.queryKey",
      path: "api/live-socket.ts",
      why: "a delivery view's own key (`LiveSocket.subscribe`), written whole from Dispatch's snapshot and its deltas; no write shows a change on a delivery view",
    },
    {
      key: "myRepliesOwnerQuery(owner).queryKey",
      path: "features/threads/replies-fixture.ts",
      why: "a fixture the tests seed their caches with before any write is out",
    },
  ];

/** The store itself, which draws every cache a write shows a change on. */
const STORE = "features/writes/write-store.ts";

/** `source` with every comment blanked, its line breaks kept, and every string left whole. */
function withoutComments(source: string): string {
  let out = "";
  let mode: "code" | "line" | "block" | "'" | '"' | "`" = "code";
  for (let index = 0; index < source.length; index += 1) {
    const ch = source[index] ?? "";
    const next = source[index + 1] ?? "";
    if (mode === "line" || mode === "block") {
      if (mode === "line" && ch === "\n") mode = "code";
      if (mode === "block" && ch === "*" && next === "/") {
        mode = "code";
        out += "  ";
        index += 1;
        continue;
      }
      out += ch === "\n" ? "\n" : " ";
      continue;
    }
    if (mode !== "code") {
      out += ch;
      if (ch === "\\") {
        out += next;
        index += 1;
      } else if (ch === mode) mode = "code";
      continue;
    }
    if (ch === "/" && next === "/") {
      mode = "line";
      out += " ";
      continue;
    }
    if (ch === "/" && next === "*") {
      mode = "block";
      out += " ";
      continue;
    }
    if (ch === "'" || ch === '"' || ch === "`") mode = ch;
    out += ch;
  }
  return out;
}

/** Walks `text` from `from`, a string or a template read whole, and answers the index of the
 *  first of `stops` at the depth it started at, or the index of the bracket that closes that
 *  depth, or the end. */
function scanTo(text: string, from: number, stops: string): number {
  let depth = 0;
  for (let index = from; index < text.length; index += 1) {
    const ch = text[index] ?? "";
    if (ch === "'" || ch === '"' || ch === "`") {
      index = stringEnd(text, index);
      continue;
    }
    if (depth === 0 && stops.includes(ch)) {
      // `=>` and `?.` are no stop of a `>` or a `?`.
      if (ch === "?" && (text[index + 1] === "." || text[index + 1] === "?")) {
        index += 1;
        continue;
      }
      return index;
    }
    if (ch === "(" || ch === "[" || ch === "{") depth += 1;
    else if (ch === ")" || ch === "]" || ch === "}") {
      if (depth === 0) return index;
      depth -= 1;
    }
  }
  return text.length;
}

/** The index of the quote that closes the string or template opening at `open`. */
function stringEnd(text: string, open: number): number {
  const quote = text[open];
  for (let index = open + 1; index < text.length; index += 1) {
    if (text[index] === "\\") index += 1;
    else if (text[index] === quote) return index;
  }
  return text.length;
}

/** The arguments of the call whose `(` is at `open`, each as its text. */
function argumentsAt(text: string, open: number): string[] {
  const args: string[] = [];
  let at = open + 1;
  for (;;) {
    const end = scanTo(text, at, ",");
    args.push(text.slice(at, end).trim());
    if (text[end] !== ",") return args.filter((arg) => arg !== "");
    at = end + 1;
  }
}

/** `expr` without what only types it: `as const`, `as QueryKey`, `satisfies …`, parentheses. */
function bare(expr: string): string {
  let out = expr.trim().replace(/\s+(?:as|satisfies)\s+[\w.<>[\]\s|]+$/, "");
  while (out.startsWith("(") && scanTo(out, 1, "") === out.length - 1) out = out.slice(1, -1);
  return out.trim();
}

/** Every element of the keys `a` and `b` name, joined end to end. */
function joined(a: readonly Key[], b: readonly Key[]): Key[] {
  return a.flatMap((head) => b.map((tail) => [...head, ...tail]));
}

/** The source of `path`, comments blanked. */
function sourceAt(sources: ReadonlyMap<string, string>, path: string): string {
  return withoutComments(sources.get(path) ?? "");
}

/** Reads the keys of every query key `expr` names in `path`. */
class KeyReader {
  readonly #sources: ReadonlyMap<string, string>;
  /** The names being read now, so a name defined by itself is read as unreadable. */
  readonly #reading = new Set<string>();

  constructor(sources: ReadonlyMap<string, string>) {
    this.#sources = sources;
  }

  /** The keys `expr`, written in `path`, may name, or `undefined` where it cannot be read. */
  keys(expr: string, path: string): Key[] | undefined {
    const text = bare(expr);
    const question = scanTo(text, 0, "?");
    if (question < text.length && text[question] === "?") {
      const colon = scanTo(text, question + 1, ":");
      const yes = this.keys(text.slice(question + 1, colon), path);
      const no = this.keys(text.slice(colon + 1), path);
      return yes === undefined || no === undefined ? undefined : [...yes, ...no];
    }
    if (text.startsWith("[") && scanTo(text, 1, "") === text.length - 1) {
      return this.#array(text.slice(1, -1), path);
    }
    const options = /^(.*)\.queryKey$/s.exec(text);
    if (options !== null) return this.#optionsKeys(options[1] ?? "", path);
    const call = /^([A-Za-z_$][\w$]*)\s*(?:<[^()]*>)?\(/.exec(text);
    if (call !== null && scanTo(text, (call[0] ?? "").length, "") === text.length - 1) {
      return this.#returned(call[1] ?? "", path);
    }
    if (/^[A-Za-z_$][\w$]*$/.test(text)) return this.#named(text, path);
    return undefined;
  }

  /** The keys an array literal's elements name: each string a string, each spread the keys it
   *  spreads, and anything else any element. */
  #array(body: string, path: string): Key[] {
    let keys: Key[] = [[]];
    for (const element of argumentsAt(`(${body})`, 0)) {
      if (element.startsWith("...")) {
        keys = joined(keys, this.keys(element.slice(3), path) ?? [[REST]]);
        continue;
      }
      const literal = /^(["'`])(.*)\1$/s.exec(element);
      const value = literal === null || literal[2]?.includes("${") ? ANY : (literal[2] ?? "");
      keys = joined(keys, [[value]]);
    }
    return keys;
  }

  /** The keys of the query options `expr` names - a call of a factory of them, or a name one is
   *  kept under - read off their `queryKey`. */
  #optionsKeys(expr: string, path: string): Key[] | undefined {
    const text = bare(expr);
    const call = /^([A-Za-z_$][\w$]*)\s*\(/.exec(text);
    const name = call?.[1] ?? (/^[A-Za-z_$][\w$]*$/.test(text) ? text : undefined);
    if (name === undefined) return undefined;
    return this.#withDefinition(name, path, (definition) => {
      if (call === null && definition.kind === "const") {
        return this.#optionsKeys(definition.text, definition.path);
      }
      const at = /\bqueryKey\s*:/.exec(definition.text);
      if (at === null) return undefined;
      const from = at.index + at[0].length;
      const value = definition.text.slice(from, scanTo(definition.text, from, ",}"));
      return this.keys(value, definition.path);
    });
  }

  /** The keys a function named `name` answers with: every `return` of its body, or the body of
   *  an arrow that has none. */
  #returned(name: string, path: string): Key[] | undefined {
    return this.#withDefinition(name, path, (definition) => {
      const arrow = /=>\s*/.exec(definition.text);
      const body =
        definition.kind === "const" && arrow !== null
          ? definition.text.slice(arrow.index + arrow[0].length)
          : definition.text;
      if (!body.trimStart().startsWith("{") && definition.kind === "const") {
        return this.keys(body, definition.path);
      }
      const values = [...body.matchAll(/\breturn\s+/g)].map((match) => {
        const from = match.index + match[0].length;
        return body.slice(from, scanTo(body, from, ";"));
      });
      if (values.length === 0) return undefined;
      const keys = values.map((value) => this.keys(value, definition.path));
      return keys.some((key) => key === undefined) ? undefined : keys.flatMap((key) => key ?? []);
    });
  }

  /** The keys the constant `name` holds. */
  #named(name: string, path: string): Key[] | undefined {
    return this.#withDefinition(name, path, (definition) =>
      definition.kind === "const" ? this.keys(definition.text, definition.path) : undefined
    );
  }

  /** `read` of every definition of `name` that `path` sees - its own, else the one it imports -
   *  joined, or `undefined` where there is none or one cannot be read. */
  #withDefinition(
    name: string,
    path: string,
    read: (definition: Definition) => Key[] | undefined
  ): Key[] | undefined {
    const id = `${path}#${name}`;
    if (this.#reading.has(id)) return undefined;
    this.#reading.add(id);
    try {
      const definitions = this.#definitions(name, path);
      if (definitions.length === 0) return undefined;
      const keys = definitions.map(read);
      return keys.some((key) => key === undefined) ? undefined : keys.flatMap((key) => key ?? []);
    } finally {
      this.#reading.delete(id);
    }
  }

  /** Every definition of `name` in `path`, or, where it has none, in the module it imports
   *  `name` from. */
  #definitions(name: string, path: string): Definition[] {
    const source = sourceAt(this.#sources, path);
    const own = definitionsIn(source, name, path);
    if (own.length > 0) return own;
    for (const match of source.matchAll(/\bimport\s+(?:type\s+)?\{([^}]*)\}\s*from\s*"([^"]+)"/g)) {
      const names = (match[1] ?? "").split(",").map((entry) => entry.replace(/^\s*type\s+/, ""));
      if (!names.some((entry) => entry.trim() === name)) continue;
      const from = moduleAt(path, match[2] ?? "", this.#sources);
      return from === undefined ? [] : definitionsIn(sourceAt(this.#sources, from), name, from);
    }
    return [];
  }
}

/** One definition of a name: a constant's initializer, or a function's whole text. */
interface Definition {
  readonly kind: "const" | "function";
  readonly path: string;
  readonly text: string;
}

/** Every `const`, `let` and `function` named `name` in `source`. */
function definitionsIn(source: string, name: string, path: string): Definition[] {
  const found: Definition[] = [];
  for (const match of source.matchAll(
    new RegExp(`\\b(?:const|let)\\s+${name}\\b\\s*(?::[^=]*)?=(?!=|>)`, "g")
  )) {
    const from = match.index + match[0].length;
    found.push({ kind: "const", path, text: source.slice(from, scanTo(source, from, ";")) });
  }
  for (const match of source.matchAll(new RegExp(`\\bfunction\\s+${name}\\s*[<(]`, "g"))) {
    // A function the app ships at its top level closes on a `}` at the start of a line.
    const end = source.indexOf("\n}", match.index);
    found.push({
      kind: "function",
      path,
      text: source.slice(match.index, end === -1 ? source.length : end + 2),
    });
  }
  return found;
}

/** The shipped module `specifier`, imported by `path`, by its path under `web/src`. */
function moduleAt(
  path: string,
  specifier: string,
  sources: ReadonlyMap<string, string>
): string | undefined {
  if (!specifier.startsWith(".")) return undefined;
  const base = normalize(join(dirname(path), specifier)).replaceAll("\\", "/");
  return [`${base}.ts`, `${base}.tsx`, `${base}/index.ts`].find((candidate) =>
    sources.has(candidate)
  );
}

/** The line `index` is on in `source`, counted from one. */
function lineOf(source: string, index: number): number {
  return source.slice(0, index).split("\n").length;
}

/** Every call in `sources` that writes a cache directly, and every one that shows a change on
 *  one, each with the keys it names. */
function sitesIn(sources: ReadonlyMap<string, string>): {
  readonly writes: readonly Site[];
  readonly shown: readonly Site[];
} {
  const reader = new KeyReader(sources);
  const writes: Site[] = [];
  const shown: Site[] = [];
  for (const path of sources.keys()) {
    const source = sourceAt(sources, path);
    for (const match of source.matchAll(/\.(setQueryData|setQueriesData)\s*(?:<[^()]*>)?\(/g)) {
      const [first] = argumentsAt(source, match.index + match[0].length - 1);
      const prefix = match[1] === "setQueriesData";
      const filter = prefix ? /^\{\s*queryKey\s*:\s*([\s\S]*?)\s*[,}]/.exec(first ?? "") : null;
      const text = (prefix ? filter?.[1] : first) ?? "";
      const where = `${path}:${lineOf(source, match.index)}`;
      writes.push({ keys: reader.keys(text, path), prefix, text, where });
    }
    for (const match of source.matchAll(/\bshown\.(on|under)\s*(?:<[^()]*>)?\(/g)) {
      const [first] = argumentsAt(source, match.index + match[0].length - 1);
      const text = first ?? "";
      const where = `${path}:${lineOf(source, match.index)}`;
      shown.push({ keys: reader.keys(text, path), prefix: match[1] === "under", text, where });
    }
  }
  return { shown, writes };
}

/** Whether some cache is named both by `a` and by `b`, each naming one key, or every key under
 *  it where it is open. */
function meet(a: readonly Part[], aOpen: boolean, b: readonly Part[], bOpen: boolean): boolean {
  const [x, ...restA] = a;
  const [y, ...restB] = b;
  if (x === REST || y === REST) return true;
  if (x === undefined) return y === undefined || aOpen;
  if (y === undefined) return bOpen;
  if (typeof x === "string" && typeof y === "string" && x !== y) return false;
  return meet(restA, aOpen, restB, bOpen);
}

/** What is wrong with the cache writes of `sources`, one line each. */
function directWritesOverShownCaches(sources: ReadonlyMap<string, string>): string[] {
  const { shown, writes } = sitesIn(sources);
  const found: string[] = [];
  for (const site of shown) {
    if (site.keys === undefined) {
      found.push(`${site.where} shows a change on a cache whose key cannot be read: ${site.text}`);
    }
  }
  for (const site of writes) {
    if (site.where.startsWith(`${STORE}:`)) continue;
    const named = letThrough.some(
      (entry) => site.where.startsWith(`${entry.path}:`) && entry.key === site.text
    );
    if (named) continue;
    if (site.keys === undefined) {
      found.push(`${site.where} writes a cache whose key cannot be read: ${site.text}`);
      continue;
    }
    for (const change of shown) {
      const over = site.keys.some((key) =>
        (change.keys ?? []).some((other) => meet(key, site.prefix, other, change.prefix))
      );
      if (over) {
        found.push(
          `${site.where} writes ${site.text} directly, a cache ${change.where} shows a change on: merge it through Writes.answer`
        );
      }
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

test("no shipped module writes a cache a write shows a change on but through the store", () => {
  expect(directWritesOverShownCaches(shipped())).toEqual([]);
});

// The rule has to see what it guards: every change the app shows names a cache it reads, the
// pin's among them, and every write it lets through by name is still one the app makes.
test("the scan reads the caches the app's changes are shown on, and every write it names", () => {
  const sources = shipped();
  const { shown, writes } = sitesIn(sources);
  expect(shown.length).toBeGreaterThan(20);
  expect(shown.find((site) => site.where.startsWith("features/issue/pin-write.ts:"))?.keys).toEqual(
    [["user-state"]]
  );
  expect(
    shown.find((site) => site.where.startsWith("features/project/board-moves.ts:"))?.keys
  ).toEqual([
    ["issues", "project", ANY],
    ["issues", "project", ANY, "labels", ANY],
  ]);
  for (const entry of letThrough) {
    expect(
      writes.some((site) => site.where.startsWith(`${entry.path}:`) && site.text === entry.key)
    ).toBe(true);
  }
  expect(writes.some((site) => site.where.startsWith(`${STORE}:`))).toBe(true);
});

// The scan fails on the very write the store's copy replaced: a surface merging an answer over
// the viewer's state as it stands, where a pin shows its change.
test("a direct merge over a cache a write shows a change on is named by file and line", () => {
  const sources = new Map(shipped());
  sources.set(
    "features/issue/merged-over.ts",
    [
      'import type { QueryClient } from "@tanstack/react-query";',
      "",
      "export function mergeOver(client: QueryClient, next: unknown): void {",
      '  client.setQueryData(["user-state"], (current: object) => ({ ...current, next }));',
      "}",
      "",
    ].join("\n")
  );
  const found = directWritesOverShownCaches(sources);
  expect(found).toContainEqual(
    expect.stringMatching(
      /^features\/issue\/merged-over\.ts:4 writes \["user-state"\] directly, a cache features\/issue\/pin-write\.ts:\d+ shows a change on/
    )
  );
  expect(found.every((line) => line.startsWith("features/issue/merged-over.ts:4 "))).toBe(true);
});
