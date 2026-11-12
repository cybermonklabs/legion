import { readdirSync } from "node:fs";
import { join } from "node:path";

/**
 * Reading the app's own source as JavaScript reads it, for the rows that hold a rule no type can
 * hold: which modules the app ships, which stretches of one are code rather than comment or
 * string, and where an object literal a call is written over ends.
 */

/** `web/src`, the root every path named here is read from. */
const webSrc = join(import.meta.dir, "..");

let shipped: readonly string[] | undefined;

/** The app's own source: every module under `web/src` but the tests and their fixtures, read
 *  from the directory once and kept, since one file holds several rules over the same list. The
 *  separators are those of a path a reader writes - `/` on every host - before the fixtures are
 *  filtered out by it. */
export function shippedSources(): readonly string[] {
  shipped ??= readdirSync(webSrc, { recursive: true })
    .map((entry) => String(entry).replaceAll("\\", "/"))
    .filter(
      (path) =>
        (path.endsWith(".ts") || path.endsWith(".tsx")) &&
        !path.includes(".test.") &&
        !path.startsWith("__tests__/")
    )
    .sort();
  return shipped;
}

/** Whether each index of `source` is code: a name a doc comment or a string holds is no call. */
export function codeMask(source: string): readonly boolean[] {
  const mask: boolean[] = [];
  let mode: "code" | "line" | "block" | "'" | '"' | "`" = "code";
  for (let index = 0; index < source.length; index += 1) {
    const ch = source[index] ?? "";
    const next = source[index + 1] ?? "";
    mask.push(mode === "code");
    if (mode === "line") {
      if (ch === "\n") mode = "code";
      continue;
    }
    if (mode === "block") {
      if (ch === "*" && next === "/") {
        mode = "code";
        mask.push(false);
        index += 1;
      }
      continue;
    }
    if (mode === "'" || mode === '"' || mode === "`") {
      if (ch === "\\") {
        mask.push(false);
        index += 1;
      } else if (ch === mode) mode = "code";
      continue;
    }
    if (ch === "/" && next === "/") mode = "line";
    else if (ch === "/" && next === "*") mode = "block";
    else if (ch === "'" || ch === '"' || ch === "`") mode = ch;
  }
  return mask;
}

/** An object literal a call is written over: its text and the fields it writes at its own level.
 *  A field of an object nested in it - the one a `rowQuery(…)` spread into it names - is not
 *  this object's. */
export interface ObjectLiteral {
  readonly text: string;
  readonly fields: ReadonlyMap<string, string>;
}

/** The object literal that starts at `open` in `source`, read as JavaScript reads it: a brace in
 *  a string, a template, a comment or a template's `${…}` is not a brace of the object. */
export function objectAt(source: string, open: number, where: string): ObjectLiteral {
  const fields = new Map<string, string>();
  let depth = 0;
  let nesting = 0;
  let word = "";
  let field: { readonly name: string; readonly from: number } | undefined;
  const templates: number[] = [];
  let mode: "code" | "line" | "block" | "'" | '"' | "`" = "code";
  const close = (end: number) => {
    if (field !== undefined) fields.set(field.name, source.slice(field.from, end).trim());
    field = undefined;
  };
  for (let index = open; index < source.length; index += 1) {
    const ch = source[index] ?? "";
    const next = source[index + 1] ?? "";
    if (mode === "line") {
      if (ch === "\n") mode = "code";
      continue;
    }
    if (mode === "block") {
      if (ch === "*" && next === "/") {
        mode = "code";
        index += 1;
      }
      continue;
    }
    if (mode === "'" || mode === '"') {
      if (ch === "\\") index += 1;
      else if (ch === mode) mode = "code";
      continue;
    }
    if (mode === "`") {
      if (ch === "\\") index += 1;
      else if (ch === "`") mode = "code";
      else if (ch === "$" && next === "{") {
        templates.push(depth);
        depth += 1;
        index += 1;
        mode = "code";
      }
      continue;
    }
    if (ch === "/" && next === "/") {
      mode = "line";
      index += 1;
      continue;
    }
    if (ch === "/" && next === "*") {
      mode = "block";
      index += 1;
      continue;
    }
    if (ch === "'" || ch === '"' || ch === "`") {
      mode = ch;
      continue;
    }
    if (ch === "(" || ch === "[") nesting += 1;
    else if (ch === ")" || ch === "]") nesting -= 1;
    else if (ch === "{") depth += 1;
    else if (ch === "}") {
      depth -= 1;
      if (templates.at(-1) === depth) {
        templates.pop();
        mode = "`";
        continue;
      }
      if (depth === 0) {
        close(index);
        return { fields, text: source.slice(open, index + 1) };
      }
    } else if (ch === ":" && depth === 1 && nesting === 0 && word !== "") {
      field = { from: index + 1, name: word };
    } else if (ch === "," && depth === 1 && nesting === 0) {
      close(index);
    }
    word = /[A-Za-z0-9_$"]/u.test(ch) ? `${word}${ch}` : "";
  }
  throw new Error(`${where}: the object literal never closes`);
}
