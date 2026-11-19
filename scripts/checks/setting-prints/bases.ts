/**
 * What a judgement rests on beyond its line's own text: each basis names a statement of the tree
 * (a guard, a function, a table) by its file and a part of its first line, with the digest of its
 * text when it was judged. A judgement whose basis the tree no longer holds, or whose basis's text
 * changed, no longer stands: the search fails it until it is judged again and its digest written
 * anew.
 */
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";
import ts from "typescript";
import type { Basis, Judged } from "../setting-prints.judged";
import { root } from "./program";

/** The text of the statement a basis names, or undefined when the tree no longer holds it: in a
 *  TypeScript file the outermost statement or class member whose first line holds `at`, and in a Go
 *  file, written as gofmt writes it, the line that holds `at` and, when it opens a block, the lines
 *  to the block's end at the same indentation. */
export function basisText(basis: Basis): string | undefined {
  let text: string;
  try {
    text = readFileSync(path.join(root, basis.file), "utf8");
  } catch {
    return undefined;
  }
  const lines = text.split("\n");
  const index = lines.findIndex((line) => line.includes(basis.at));
  if (index === -1) return undefined;
  if (basis.file.endsWith(".go")) return goSpan(lines, index);
  const source = ts.createSourceFile(basis.file, text, ts.ScriptTarget.ESNext, true);
  let found: ts.Node | undefined;
  const visit = (node: ts.Node) => {
    if (found !== undefined) return;
    const parent = node.parent;
    const placed =
      ts.isClassElement(node) ||
      (parent !== undefined &&
        (ts.isSourceFile(parent) ||
          ts.isBlock(parent) ||
          ts.isModuleBlock(parent) ||
          ts.isCaseClause(parent) ||
          ts.isDefaultClause(parent)));
    if (placed && source.getLineAndCharacterOfPosition(node.getStart()).line === index) {
      found = node;
      return;
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return found?.getText() ?? lines[index];
}

/** The line at `index` of a gofmt'd file, and when it opens a block, the lines to the line at its
 *  indentation that closes the block and opens none. */
function goSpan(lines: readonly string[], index: number): string {
  const opens = (line: string) => /[{(]\s*$/.test(line);
  const first = lines[index] ?? "";
  if (!opens(first)) return first;
  const indent = first.length - first.trimStart().length;
  for (let i = index + 1; i < lines.length; i++) {
    const line = lines[i] ?? "";
    const lead = line.length - line.trimStart().length;
    if (lead === indent && /^[})]/.test(line.trimStart()) && !opens(line)) {
      return lines.slice(index, i + 1).join("\n");
    }
  }
  return lines.slice(index).join("\n");
}

/** The digest a judgement writes of a basis's text: the first 12 hex digits of its SHA-256. */
export function digestOf(text: string): string {
  return createHash("sha256").update(text).digest("hex").slice(0, 12);
}

/** Why each basis of `judged` no longer holds, one line each: one the tree no longer holds, and
 *  one whose text changed since it was judged, with the digest it has now. */
export function staleBases(judged: Judged): string[] {
  return (judged.rests ?? []).flatMap((basis) => {
    const text = basisText(basis);
    const named = `rests on ${JSON.stringify(basis.at)} in ${basis.file}`;
    if (text === undefined) return [`${named}, which the tree no longer holds`];
    const digest = digestOf(text);
    return digest === basis.digest
      ? []
      : [`${named}, whose text changed: its digest is ${digest} now, ${basis.digest} when judged`];
  });
}
