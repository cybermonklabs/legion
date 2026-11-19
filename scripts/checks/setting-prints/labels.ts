/**
 * The labels of one run of the search: each setting a value was read from, each line that wrote
 * one into text, each function and each parameter, interned once by its key. The analysis reads
 * the tree twice over (`symbolic` says which reading is under way), and a line is labelled apart for
 * each reading.
 */
import path from "node:path";
import type ts from "typescript";
import type { Label, Labels, Source } from "./model";
import { root } from "./program";
import type { FunctionWithBody } from "./rules";

/** The labels of one run, and the functions that make and read them. */
export function labelsOf(symbolic: () => boolean) {
  const labels: Label[] = [];
  const labelIds = new Map<string, number>();
  const intern = (key: string, make: () => Label): number => {
    const known = labelIds.get(key);
    if (known !== undefined) return known;
    labels.push(make());
    labelIds.set(key, labels.length - 1);
    return labels.length - 1;
  };
  const lineOf = (node: ts.Node) =>
    node.getSourceFile().getLineAndCharacterOfPosition(node.getStart()).line + 1;
  const where = (node: ts.Node) =>
    `${path.relative(root, node.getSourceFile().fileName)}:${lineOf(node)}`;
  const one = (id: number): Labels => new Set([id]);
  /** A setting as a whole, read at `node`. */
  const setting = (node: ts.Node, keyed: Source["keyed"], what: string): number =>
    intern(`s ${where(node)} ${what}`, () => ({
      kind: "source",
      what,
      where: where(node),
      keyed,
      depth: 0,
    }));
  /** A variable of the environment, the same label wherever it is read. */
  const variable = (name: string, node: ts.Node): number =>
    intern(`v ${name}`, () => ({
      kind: "source",
      what: `the environment variable ${name}`,
      where: where(node),
      depth: 1,
      global: true,
    }));
  /** The setting `id` as the call at `call` answers with it. */
  const calledAt = (id: number, call: ts.Node): number => {
    const label = labels[id];
    if (label?.kind !== "source" || label.global || label.called) return id;
    return intern(`c ${id} ${call.getSourceFile().fileName} ${call.getStart()}`, () => ({
      ...label,
      where: `${label.where}, through the call at ${where(call)}`,
      called: true,
      origin: id,
    }));
  };
  /** `value` with each setting as a whole in it named more closely by `key`: the variable of the
   *  environment of that name, the argument or field of that key. A member of a value that holds
   *  the environment whose key is not spelled as a variable (`options.path` of options that hold
   *  it) is a field of that value, not a read of the environment. */
  const derive = (value: Labels, key: string | undefined, node: ts.Node): Labels => {
    let out: Set<number> | undefined;
    for (const id of value) {
      const label = labels[id];
      if (label?.kind !== "source" || label.keyed === undefined || key === undefined) continue;
      out ??= new Set(value);
      out.delete(id);
      if (label.keyed === "environment") {
        if (/^[A-Z][A-Z0-9_]*$/.test(key)) out.add(variable(key, node));
      } else {
        const what = `${label.what} ${/^\d+$/.test(key) ? `[${key}]` : `.${key}`}`;
        const depth = label.depth + 1;
        out.add(
          intern(`d ${id} ${key}`, () => ({
            kind: "source",
            what,
            where: label.where,
            keyed: depth < 3 ? label.keyed : undefined,
            depth,
            called: label.called,
            parent: id,
            key,
          }))
        );
      }
    }
    return out ?? value;
  };
  const fnLabel = (node: FunctionWithBody): number =>
    intern(`f ${node.getSourceFile().fileName} ${node.pos}`, () => ({ kind: "function", node }));
  const paramLabel = (fn: ts.Node, index: number, writer = -1, leading = false): number =>
    intern(`p ${fn.getSourceFile().fileName} ${fn.pos} ${index} ${writer} ${leading}`, () => ({
      kind: "parameter",
      fn,
      index,
      rendered: writer !== -1,
      writer,
      leading,
    }));
  /** The settings a value carries: each it holds as read, and each that a line wrote into the text
   *  it holds. */
  const settingsIn = (set: Labels): number[] => {
    const out = new Set<number>();
    for (const id of set) {
      const label = labels[id];
      if (label?.kind === "source") out.add(id);
      else if (label?.kind === "written") for (const each of label.sources) out.add(each);
    }
    return [...out];
  };
  const carries = (set: Labels) => settingsIn(set).length > 0;
  /** The settings a value holds as read, and those a line wrote at the head of its text. */
  const leadingIn = (set: Labels): number[] => {
    const out = new Set<number>();
    for (const id of set) {
      const label = labels[id];
      if (label?.kind === "source") out.add(id);
      else if (label?.kind === "written") for (const each of label.leading) out.add(each);
    }
    return [...out];
  };

  /** A setting the value holds as read, not yet written into text by a line, or in the symbolic
   *  reading such a parameter. */
  const unwritten = (id: number) => {
    const label = labels[id];
    return (
      label?.kind === "source" || (symbolic() && label?.kind === "parameter" && !label.rendered)
    );
  };
  /** The value at `node` written into text by `how`. When the value holds a setting as read, the
   *  line is labelled, and the text it makes carries that label in place of the setting: a line
   *  that writes the text again is not a second line that writes the setting. A parameter of the
   *  symbolic reading becomes that parameter written by this line, which each call fills in with
   *  what its own argument holds (`writtenAt`); the two readings label a line apart, so a call's
   *  answer names only the settings that call hands it. */
  const write = (node: ts.Node, how: string, value: Labels, leading = false): Labels => {
    const read = [...value].filter(unwritten);
    if (read.length === 0) return value;
    const id = intern(
      `w ${node.getSourceFile().fileName} ${node.getStart()} ${how} ${symbolic() ? "s" : "c"}`,
      () => ({
        kind: "written",
        how,
        file: path.relative(root, node.getSourceFile().fileName),
        line: lineOf(node),
        expression: node.getText().replace(/\s+/g, " ").slice(0, 160),
        sources: new Set(),
        leading: new Set(),
        ends: new Set(),
      })
    );
    return written(id, read, value, leading);
  };
  /** `value` with each setting it holds as read, `read`, written by the line `id`. */
  const written = (
    id: number,
    read: readonly number[],
    value: Labels,
    leading: boolean
  ): Labels => {
    const line = labels[id];
    const out = new Set(value);
    for (const each of read) {
      out.delete(each);
      const label = labels[each];
      if (label?.kind === "source" && line?.kind === "written") {
        line.sources.add(each);
        if (leading) line.leading.add(each);
      }
      if (label?.kind === "parameter") out.add(paramLabel(label.fn, label.index, id, leading));
    }
    out.add(id);
    return out;
  };
  /** What the argument `value` of the call `call` holds once the line `writer` of the function
   *  wrote it: that line as this call fills it in. */
  const writtenAt = (writer: number, call: ts.Node, value: Labels, leading: boolean): Labels => {
    const read = [...value].filter(unwritten);
    const line = labels[writer];
    if (read.length === 0 || line?.kind !== "written") return value;
    const base = line.base ?? writer;
    const first = labels[base];
    if (first?.kind !== "written") return value;
    const id = intern(`r ${base} ${call.getSourceFile().fileName} ${call.getStart()}`, () => ({
      kind: "written",
      how: first.how,
      file: first.file,
      line: first.line,
      expression: first.expression,
      sources: new Set(),
      leading: new Set(),
      ends: new Set(),
      base,
    }));
    return written(id, read, value, leading);
  };
  return {
    labels,
    labelIds,
    where,
    one,
    setting,
    variable,
    calledAt,
    derive,
    fnLabel,
    paramLabel,
    settingsIn,
    carries,
    leadingIn,
    write,
    writtenAt,
  };
}
