/**
 * The model of the search's analysis: the labels of where a value came from and of the line that
 * wrote it into text, and a line the search finds.
 */
import type ts from "typescript";
import type { FunctionWithBody } from "./rules";

export type Labels = ReadonlySet<number>;
export const EMPTY: Labels = new Set();

/** Where a value came from. `keyed` is the setting as a whole (the environment, the command
 *  line, a file's text) that a member read names more closely; `depth` counts those reads. A
 *  variable of the environment is `global`, the same wherever it is read; any other setting read
 *  inside a function is named again for each call that answers with it (`called`), so a helper's
 *  file read is one setting for each of its callers. */
export interface Source {
  readonly kind: "source";
  readonly what: string;
  readonly where: string;
  readonly keyed?: "environment" | "command line" | "file";
  readonly depth: number;
  readonly global?: boolean;
  readonly called?: boolean;
  /** A setting named for a call: the setting it names inside the function. */
  readonly origin?: number;
  /** A member read of a setting as a whole: that setting, and the key read. */
  readonly parent?: number;
  readonly key?: string;
}
export interface Written {
  readonly kind: "written";
  readonly how: string;
  readonly file: string;
  readonly line: number;
  readonly expression: string;
  readonly sources: Set<number>;
  /** The settings written at the head of the text, where a URL's scheme and host stand. */
  readonly leading: Set<number>;
  readonly ends: Set<string>;
  /** For the line as one call fills it in: the line of the function that wrote the parameter. */
  readonly base?: number;
}
export interface Fn {
  readonly kind: "function";
  readonly node: FunctionWithBody;
}
/** What a function's parameter holds, as its answer is read: the call's argument stands in. */
export interface Param {
  readonly kind: "parameter";
  readonly fn: ts.Node;
  readonly index: number;
  /** The parameter as a line of the function wrote it into text: that line's label, which a call
   *  fills in with what its own argument holds, and whether it wrote it at the head of the text. */
  readonly rendered: boolean;
  readonly writer: number;
  readonly leading: boolean;
}
export type Label = Source | Written | Fn | Param;

/** One line the search finds, with what writes the value there, which settings it comes from,
 *  and where it is printed. */
export interface Finding {
  readonly file: string;
  readonly line: number;
  readonly text: string;
  readonly hows: readonly string[];
  readonly expressions: readonly string[];
  readonly sources: readonly string[];
  readonly ends: readonly string[];
}

export function union(...sets: Labels[]): Labels {
  let first: Labels | undefined;
  let out: Set<number> | undefined;
  for (const set of sets) {
    if (set.size === 0) continue;
    if (first === undefined) {
      first = set;
      continue;
    }
    if (out === undefined) out = new Set(first);
    for (const label of set) out.add(label);
  }
  return out ?? first ?? EMPTY;
}
