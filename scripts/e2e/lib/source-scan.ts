/** The source scan: for every `.` or `source` command a script runs, the file the shell reads
 *  there, or a refusal naming the command and why its file cannot be named.
 *
 *  It reads each script as bash parses it, through the syntax tree `shfmt --to-json` prints
 *  (mvdan.cc/sh, pinned below), so a source is a command in any command position: after `;`,
 *  `&&` or `||`, in a branch, a loop, a block, a function or a command substitution, and behind
 *  `builtin` or `command`. Text inside quotes or a heredoc is not a command. It walks the scripts
 *  in the order the shell runs them, carrying what it knows each variable, each argument of the
 *  running function, `$0`, `${BASH_SOURCE[0]}` and the working directory hold, and names a sourced
 *  file only from what the shell holds at that source:
 *
 *  - An assignment counts once the shell has run it. One below the source, or in a subshell, does
 *    not count. Where the branches of an `if`, a `case`, `&&` or `||` leave a variable different
 *    known values, it holds any of them, and a source on it reads each; where one leaves it unknown
 *    or unbound, it is unknown. In a `case` branch, the variable the `case` tests holds what that
 *    branch's patterns match. A loop is walked until what it binds settles.
 *  - A library runs in its caller's shell: it starts from the caller's bindings at the source, and
 *    its own bindings reach the caller after it. A function runs at each call, with the bindings
 *    and the arguments there, so `"$@"` and `$1` are what the call gave it. A function defined in
 *    two branches has each body walked at its call. A function nothing calls is never walked, as
 *    bash never runs it.
 *  - A command named by an expansion runs what the expansion holds. Known, it is that command.
 *    Unknown, it is refused where it may be `source` of a file: where its name may be `source` and
 *    a word follows that `source` would take for a file. Otherwise it is taken for a program.
 *  - `$0` is the script that runs, which in a library is its caller. `${BASH_SOURCE[0]}` is the file
 *    itself.
 *  - A path relative to the working directory holds only until a `cd`, and a relative literal is
 *    refused outright.
 *  - A nameref binds the variable it names. A command that binds a name the scan does not know,
 *    such as `printf -v "$name"`, leaves unknown every variable that name may be, and an `unset` of
 *    one leaves each such variable its value or empty.
 *  - A trap's text is parsed where the trap is set and walked at the end of its script, with what
 *    holds both where it was set and at that end.
 *  - An `eval`, a source whose file is not known, and a command whose name may be `source` of a file
 *    are refused by name, and nothing is known after them. A trap whose text is not known, a known
 *    path that names no regular file, and a `shopt -s expand_aliases` or `shopt -s lastpipe` are
 *    refused by name.
 *
 *  What the scan does not follow is named in source-scan.test.ts, under "known limits".
 */

import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";

/** The parser, pinned: the module `go install` builds, once per process. */
const shfmtModule = "mvdan.cc/sh/v3/cmd/shfmt@v3.14.1";

/** What the scan found. */
export interface SourceScan {
  /** Every file the scan read: each script it was given and each file a source reached. */
  read: string[];
  /** Every command it refuses, as `<path>:<line>: <command>: <why>`, and every script it cannot
   *  parse, as `<path>: <why>`. */
  refused: string[];
}

/** What a scan takes as given about the environment its scripts run in. */
export interface ScanAssumptions {
  /** Variables of the environment to read as unset, so a script's `${NAME:-default}` reads its
   *  default: the arm it runs where the caller sets nothing. A variable not named here, which the
   *  walk has not bound, is unknown. */
  readonly unset?: readonly string[];
  /** Variables of the environment that name a program to run where the caller sets them: a command
   *  named by one is that program, never `.` or `source`. */
  readonly programs?: readonly string[];
}

type Node = { readonly [key: string]: unknown };

/** A value the scan knows. `text` is the shell's exact string. `path` is a string the scan does not
 *  know exactly but knows to name the file or directory at `path`; with `cwd`, it does so only
 *  while the working directory is the one the run started in. */
type Scalar = { kind: "text"; text: string } | { kind: "path"; path: string; cwd: boolean };
/** A value the scan does not know. `foreign`: it holds a character neither `.` nor `source` holds,
 *  so it is neither. `notSource`: it is neither, though it may be empty. `names`: the plain words
 *  it may be besides a path or nothing, as `$(command -v NAME)` gives. `shape`: the regular
 *  expression its text matches, where the scan knows part of it. `template`: its text with each
 *  part the scan does not know written as one word, where every such part is one shell word that
 *  cannot end a command (the output of `printf %q` or of `mktemp`), so the shell would parse the
 *  text as it parses the template. */
type Unknown = {
  kind: "unknown";
  why: string;
  foreign?: true;
  notSource?: true;
  names?: readonly string[];
  shape?: string;
  template?: string;
};
/** Any one of a few known values, as the branches before here left it. */
type OneOf = { kind: "oneOf"; values: readonly Scalar[] };
type Value = Scalar | Unknown | OneOf;
/** A word of a command once the shell has expanded it, or a run of words whose number the scan
 *  does not know, as `"$@"` gives where the arguments are not known. */
type Word = Value | { kind: "spread"; why: string };
type Text = { kind: "text"; text: string };
type ArrayBinding = { kind: "array"; alternatives: readonly (readonly Word[])[] };
/** What a variable holds: a value, the words of an array (any one of a few lists), or the name of
 *  the variable a nameref stands for. */
type Binding = Value | ArrayBinding | { kind: "ref"; target: string | undefined };

const isValue = (word: Word): word is Value => word.kind !== "spread";
const isTextValue = (value: Value | Word): value is Text => value.kind === "text";
const isArrayBinding = (binding: Binding): binding is ArrayBinding => binding.kind === "array";
const isBinding = (binding: Binding | undefined): binding is Binding => binding !== undefined;
const isPlainValue = (binding: Binding): binding is Value =>
  binding.kind !== "array" && binding.kind !== "ref";

/** The most values a variable or a command's words may take before the scan calls them unknown. */
const mostValues = 8;

const unknown = (why: string): Unknown => ({ kind: "unknown", why });

interface Func {
  name: string;
  /** Where the definition is, `<file>:<offset>`: two definitions of one name are two functions. */
  id: string;
  body: Node;
  /** The file it is defined in, and `${BASH_SOURCE[0]}` there. */
  file: string;
  self: Value;
}

/** What the shell holds at a point of the walk. */
interface Shell {
  /** Each variable the walk has bound; one it has not is unknown, inherited from outside. */
  vars: Map<string, Binding>;
  /** Whether the working directory may differ from the one the run started in. */
  cwdChanged: boolean;
  /** `$0`. */
  zero: Value;
  /** `${BASH_SOURCE[0]}`: the file running now. */
  self: Value;
  /** The arguments of the running function or script, or undefined where they are not known. */
  positional: readonly Word[] | undefined;
  /** The functions each name may run, as the branches before here defined them; `null` where a
   *  branch defined none, so the name runs a program there. */
  functions: Map<string, readonly (Func | null)[]>;
  /** Why every variable the walk has not bound since is unknown, when something forgot them. */
  forgotten?: string;
  /** The environment's variables the scan was told to read as unset (`scanSources`' `unset`). */
  unset: ReadonlySet<string>;
  /** The environment's variables the scan was told name a program (`ScanAssumptions.programs`). */
  programs: ReadonlySet<string>;
}

function copy(shell: Shell): Shell {
  return { ...shell, vars: new Map(shell.vars), functions: new Map(shell.functions) };
}

const json = (value: unknown): string => JSON.stringify(value) ?? "null";

/** The values `value` may be, or `undefined` where it is unknown. */
function scalars(value: Value): readonly Scalar[] | undefined {
  if (value.kind === "unknown") return undefined;
  return value.kind === "oneOf" ? value.values : [value];
}

/** One value from any of `values`: unknown where one is, else the known values they hold. */
function anyOf(values: readonly Value[]): Value {
  const known: Scalar[] = [];
  for (const value of values) {
    const each = scalars(value);
    if (each === undefined) return value;
    for (const scalar of each) if (!known.some((k) => json(k) === json(scalar))) known.push(scalar);
  }
  if (known.length === 1 && known[0] !== undefined) return known[0];
  if (known.length > mostValues) return unknown(`it takes more than ${mostValues} values here`);
  return { kind: "oneOf", values: known };
}

/** Each known value of `value` mapped through `each`. */
function mapValue(value: Value, each: (scalar: Scalar) => Value): Value {
  const all = scalars(value);
  return all === undefined ? value : anyOf(all.map(each));
}

/** The shell after one of `branches` ran, from `shell`: a variable is any of the values the
 *  branches left it, and unknown where one left it unknown or unbound. */
function merge(shell: Shell, branches: readonly Shell[]): void {
  const names = new Set<string>();
  for (const branch of branches) for (const name of branch.vars.keys()) names.add(name);
  for (const name of names) {
    const bindings = branches.map((branch) => branch.vars.get(name));
    shell.vars.set(name, mergeBindings(name, bindings));
  }
  shell.cwdChanged = branches.some((branch) => branch.cwdChanged);
  shell.forgotten = branches.find((branch) => branch.forgotten !== undefined)?.forgotten;
  const positional = branches.map((branch) => json(branch.positional));
  if (!positional.every((each) => each === positional[0])) shell.positional = undefined;
  const functionNames = new Set<string>();
  for (const branch of branches)
    for (const name of branch.functions.keys()) functionNames.add(name);
  for (const name of functionNames) {
    const runs: (Func | null)[] = [];
    for (const branch of branches)
      for (const func of branch.functions.get(name) ?? [null])
        if (!runs.some((run) => (run?.id ?? null) === (func?.id ?? null))) runs.push(func);
    shell.functions.set(name, runs);
  }
}

function mergeBindings(name: string, bindings: readonly (Binding | undefined)[]): Binding {
  const first = bindings[0];
  if (first !== undefined && bindings.every((binding) => json(binding) === json(first)))
    return first;
  if (bindings.some((binding) => binding === undefined))
    return unknown(`$${name} is bound on only some of the paths before here`);
  const defined = bindings.filter(isBinding);
  const cause = defined.find((binding) => binding.kind === "unknown");
  if (cause !== undefined) return cause;
  const arrays = defined.filter(isArrayBinding);
  if (arrays.length === defined.length) {
    const alternatives: (readonly Word[])[] = [];
    for (const binding of arrays)
      for (const words of binding.alternatives)
        if (!alternatives.some((each) => json(each) === json(words))) alternatives.push(words);
    return alternatives.length > mostValues
      ? unknown(`$${name} holds more than ${mostValues} lists of words here`)
      : { kind: "array", alternatives };
  }
  const values = defined.filter(isPlainValue);
  if (values.length !== defined.length)
    return unknown(`$${name} is an array or a nameref on only some of the paths before here`);
  return anyOf(values);
}

const get = (node: unknown, key: string): Node | undefined => {
  const value = (node as Node | undefined)?.[key];
  return typeof value === "object" && value !== null ? (value as Node) : undefined;
};
const list = (node: unknown, key: string): Node[] => {
  const value = (node as Node | undefined)?.[key];
  return Array.isArray(value) ? (value as Node[]) : [];
};
const str = (node: unknown, key: string): string | undefined => {
  const value = (node as Node | undefined)?.[key];
  return typeof value === "string" ? value : undefined;
};
const line = (node: unknown): number => {
  const value = get(node, "Pos")?.Line;
  return typeof value === "number" ? value : 0;
};
const identifier = (text: string): boolean => /^[A-Za-z_][A-Za-z0-9_]*$/.test(text);

/** A statement of the syntax tree: the one kind of node that holds a command and its
 *  redirections and carries no `Type`. */
const isStatement = (node: Node): boolean =>
  node.Type === undefined && ("Cmd" in node || ("Redirs" in node && !("Op" in node)));

/** The exact text of a word that holds no expansion, or `undefined` where it holds one. */
function literal(word: Node | undefined): string | undefined {
  if (word === undefined) return undefined;
  let text = "";
  for (const part of list(word, "Parts")) {
    if (part.Type === "Lit") text += unescapeBare(str(part, "Value") ?? "");
    else if (part.Type === "SglQuoted") text += str(part, "Value") ?? "";
    else if (part.Type === "DblQuoted" && list(part, "Parts").every((p) => p.Type === "Lit"))
      text += list(part, "Parts")
        .map((p) => unescapeDouble(str(p, "Value") ?? ""))
        .join("");
    else return undefined;
  }
  return text;
}

const unescapeBare = (text: string): string => text.replace(/\\(.)/g, "$1");
const unescapeDouble = (text: string): string => text.replace(/\\([$`"\\])/g, "$1");
const escapeShape = (text: string): string => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
/** Whether bash expands `text`, unquoted, as a glob: a `*`, a `?`, or a `[` that a `]` closes. A
 *  lone `[`, the test command, is a word. */
const isGlob = (text: string): boolean => /[*?]|\[[^\]]+\]/.test(text);

let shfmtPath: string | undefined;

/** The shfmt binary, built once per process with `go install`. Throws, naming the build, when
 *  it cannot be built: the scan cannot read shell without it, and must not pass over it. */
function shfmt(): string {
  if (shfmtPath !== undefined) return shfmtPath;
  const go = process.env.SOURCE_SCAN_GO ?? "go";
  const bin = mkdtempSync(join(tmpdir(), "source-scan-shfmt-"));
  process.on("exit", () => rmSync(bin, { recursive: true, force: true }));
  const built = spawnSync(go, ["install", shfmtModule], {
    env: { ...process.env, GOBIN: bin, GOTOOLCHAIN: "local", GOFLAGS: "" },
    encoding: "utf8",
    timeout: 150_000,
  });
  if (built.error !== undefined || built.status !== 0) {
    const how =
      built.error !== undefined
        ? `could not run (${built.error.message})`
        : `exited ${built.status}: ${built.stderr.trim()}`;
    throw new Error(`the source scan cannot parse shell: \`${go} install ${shfmtModule}\` ${how}`);
  }
  shfmtPath = join(bin, "shfmt");
  return shfmtPath;
}

type Parsed = { tree: Node; text: string } | { error: string };

const parsed = new Map<string, Parsed>();

/** The syntax tree of `text`, kept under `key`. */
function parseText(key: string, text: string): Parsed {
  const cached = parsed.get(key);
  if (cached !== undefined) return cached;
  const run = spawnSync(shfmt(), ["--to-json", "-ln", "bash"], {
    input: text,
    encoding: "utf8",
    maxBuffer: 1 << 30,
  });
  const result: Parsed =
    run.status === 0
      ? { tree: JSON.parse(run.stdout) as Node, text }
      : {
          error: `shfmt cannot parse it as bash: ${(run.stderr || String(run.error)).trim().split("\n")[0]}`,
        };
  parsed.set(key, result);
  return result;
}

/** The syntax tree of the script at `path`, or why there is none. */
function parse(path: string): Parsed {
  return parsed.get(path) ?? parseText(path, readFileSync(path, "utf8"));
}

/** Whether `path` names a regular file, or a link to one. */
function regularFile(path: string): boolean {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

function isDirectory(path: string): boolean {
  try {
    return statSync(path).isDirectory();
  } catch {
    return false;
  }
}

/** The directory a value names, as `dirname` gives it. */
function dirnameOf(value: Scalar): Scalar {
  if (value.kind === "path") return { kind: "path", path: dirname(value.path), cwd: value.cwd };
  return { kind: "text", text: posixDirname(value.text) };
}

function posixDirname(text: string): string {
  const trimmed = text.replace(/\/+$/, "");
  if (trimmed === "") return text.startsWith("/") ? "/" : ".";
  const at = trimmed.lastIndexOf("/");
  if (at < 0) return ".";
  return trimmed.slice(0, at).replace(/\/+$/, "") || "/";
}

/** Whether `value` holds a character neither `.` nor `source` holds, whatever it is. */
function isForeign(value: Value): boolean {
  if (value.kind === "text") return /[^.ceorsu]/.test(value.text);
  if (value.kind === "path") return !value.cwd;
  if (value.kind === "oneOf") return value.values.every(isForeign);
  return value.foreign === true;
}

/** The regular expression the text of `value` matches. */
function shapeOf(value: Value): string {
  if (value.kind === "text") return escapeShape(value.text);
  if (value.kind === "path") return value.cwd ? ".*" : "/.*";
  if (value.kind === "oneOf") return `(?:${value.values.map(shapeOf).join("|")})`;
  return value.shape ?? ".*";
}

/** `a` then `b`, as the shell joins two parts of one word. */
function join2(a: Value, b: Value): Value {
  if (a.kind === "text" && a.text === "") return b;
  if (b.kind === "text" && b.text === "") return a;
  if (a.kind === "unknown" || b.kind === "unknown") {
    const cause: Unknown =
      a.kind === "unknown" ? a : b.kind === "unknown" ? b : unknown("a word is not known");
    const foreign = isForeign(a) || isForeign(b);
    const left = templateOf(a);
    const right = templateOf(b);
    return {
      kind: "unknown",
      why: cause.why,
      ...(foreign ? { foreign: true, notSource: true } : {}),
      shape: shapeOf(a) + shapeOf(b),
      ...(left !== undefined && right !== undefined ? { template: left + right } : {}),
    };
  }
  const left = scalars(a) ?? [];
  const right = scalars(b) ?? [];
  if (left.length * right.length > mostValues)
    return unknown(`a word joins more than ${mostValues} values here`);
  return anyOf(left.flatMap((l) => right.map((r) => joinScalars(l, r))));
}

/** The text of `value` with each part the scan does not know written as one word, where the
 *  shell would parse it as it parses that (`Unknown.template`), or `undefined` where it would not. */
function templateOf(value: Value): string | undefined {
  if (value.kind === "text") return value.text;
  if (value.kind === "unknown") return value.template;
  return undefined;
}

/** One shell word the scan does not know and that cannot end a command. */
const opaqueWord = (why: string): Unknown => ({ ...unknown(why), template: "scan_opaque_word" });

function joinScalars(a: Scalar, b: Scalar): Value {
  if (a.kind === "text" && b.kind === "text") return { kind: "text", text: a.text + b.text };
  if (a.kind === "path" && b.kind === "text")
    return { kind: "path", path: a.path + b.text, cwd: a.cwd };
  return { ...unknown("a word joins a path to other text"), shape: shapeOf(a) + shapeOf(b) };
}

/** A call in progress, for the memo: the variables and functions it read, and whether anything
 *  it did depends on more than those. */
interface Frame {
  reads: Set<string>;
  funcs: Set<string>;
  open: boolean;
}

/** What a call left, and what it read to leave it (`callFunction`). */
interface MemoEntry {
  reads: readonly [string, string][];
  funcs: readonly [string, string][];
  vars: readonly [string, Binding | null][];
  functions: readonly [string, readonly (Func | null)[] | null][];
  cwdChanged: boolean;
  forgotten: string | undefined;
}

/** A trap's text, walked at the end of the script that set it, with what holds both where the trap
 *  was set and at that end. */
interface Trap {
  key: string;
  tree: Node;
  text: string;
  /** The top-level script whose walk set it. */
  script: string;
  shell: Shell;
}

/** The state of one walk: the tree, what it read and refused, and what it has called. */
interface Walk {
  tree: string;
  /** The script given to the scan whose walk is running. */
  script: string;
  read: Set<string>;
  /** Every command refused, by where it is and what it says, with the first reason: a command the
   *  walk reaches twice, as a loop's body is, names the cause the first time. */
  refused: Map<string, string>;
  /** The functions running now, by `Func.id`, so a call into one of them does not recurse. */
  running: string[];
  memo: Map<string, MemoEntry[]>;
  frames: Frame[];
  traps: Map<string, Trap>;
}

/** Notes, for every call in progress, that what it did depended on `name`. */
function noteRead(walk: Walk, name: string): void {
  for (const frame of walk.frames) frame.reads.add(name);
}

function noteFunc(walk: Walk, name: string): void {
  for (const frame of walk.frames) frame.funcs.add(name);
}

/** Notes that what every call in progress did depends on more than what it read, so none is kept
 *  in the memo: forgetting every binding touches each name the shell holds. */
function noteOpen(walk: Walk): void {
  for (const frame of walk.frames) frame.open = true;
}

/** Forgets every binding: after a source the scan refused, or an `eval`, anything may be bound. */
function forget(walk: Walk, shell: Shell, why: string): void {
  noteOpen(walk);
  for (const name of shell.vars.keys())
    shell.vars.set(name, unknown(`$${name} is not known here: ${why}`));
  shell.cwdChanged = true;
  shell.positional = undefined;
  shell.forgotten = why;
}

/** What `name` holds, through any nameref. */
function lookup(walk: Walk, shell: Shell, name: string, depth = 0): Binding | undefined {
  noteRead(walk, name);
  const binding = shell.vars.get(name);
  if (binding?.kind !== "ref") return binding;
  if (binding.target === undefined || depth > 8)
    return unknown(`$${name} is a nameref to a variable not known here`);
  return lookup(walk, shell, binding.target, depth + 1);
}

/** Binds `name` to `binding`, through any nameref. */
function assign(walk: Walk, shell: Shell, name: string, binding: Binding, depth = 0): void {
  noteRead(walk, name);
  const current = shell.vars.get(name);
  if (current?.kind === "ref" && binding.kind !== "ref") {
    if (current.target === undefined || depth > 8) {
      forget(walk, shell, `$${name} is a nameref to a variable not known here`);
      return;
    }
    assign(walk, shell, current.target, binding, depth + 1);
    return;
  }
  shell.vars.set(name, binding);
}

/** Gives each variable `target`, a name the shell builds, may be the value a command binds: an
 *  unknown one, or for `unset`, none. A name the scan does not know may be any variable whose name
 *  the target's shape matches: a bound one is then unknown, or for `unset` its value or empty; an
 *  unbound one may now be set, so none is taken as unset any more, but `unset` sets none. */
function bindTarget(walk: Walk, shell: Shell, target: Word, why: string, unsets = false): void {
  const named = target.kind === "spread" ? undefined : scalars(target);
  if (named !== undefined) {
    for (const each of named) {
      if (each.kind !== "text") continue;
      const name = each.text.replace(/\[.*\]$/, "");
      if (!identifier(name)) continue;
      const before = unsets && named.length > 1 ? lookup(walk, shell, name) : undefined;
      assign(
        walk,
        shell,
        name,
        unsets ? maybeEmpty(before, name, named.length > 1) : unknown(`$${name} ${why}`)
      );
    }
    return;
  }
  const shape = target.kind === "spread" ? ".*" : shapeOf(target).replace(/\\\[.*$/, "");
  const pattern = shape === "" ? /^.*$/ : new RegExp(`^(?:${shape})$`);
  const cause = target.kind === "unknown" || target.kind === "spread" ? `: ${target.why}` : "";
  noteOpen(walk);
  for (const name of shell.vars.keys()) {
    if (!pattern.test(name)) continue;
    shell.vars.set(
      name,
      unsets
        ? maybeEmpty(shell.vars.get(name), name, true)
        : unknown(`$${name} may be bound by a name not known here (${why})${cause}`)
    );
  }
  if (unsets) return;
  if (shape === ".*" || shape === "")
    shell.forgotten = `a command bound a name not known here (${why})${cause}`;
  else
    for (const name of shell.unset)
      if (pattern.test(name) && !shell.vars.has(name))
        shell.vars.set(
          name,
          unknown(`$${name} may be bound by a name not known here (${why})${cause}`)
        );
}

/** What `before` is after an `unset` that may or may not name its variable: its value or empty,
 *  and empty where the `unset` names it alone. */
function maybeEmpty(before: Binding | undefined, name: string, maybe: boolean): Binding {
  const empty: Text = { kind: "text", text: "" };
  if (!maybe || before === undefined) return empty;
  if (before.kind === "array" || before.kind === "ref")
    return unknown(`$${name} may be unset before here`);
  return anyOf([before, empty]);
}

/** Everything `shell` would hold after running `stmts` of `file`. */
function run(walk: Walk, file: string, text: string, stmts: readonly Node[], shell: Shell): void {
  for (const stmt of stmts) runStmt(walk, file, text, stmt, shell);
}

function runStmt(walk: Walk, file: string, text: string, stmt: Node, shell: Shell): void {
  // A command substitution or a process substitution in the statement's own words runs before
  // its command, in a subshell of its own; a source inside one is still a source the shell runs.
  for (const inner of ownSubstitutions(stmt)) run(walk, file, text, inner, copy(shell));
  const cmd = get(stmt, "Cmd");
  if (cmd === undefined) return;
  const target = stmt.Background === true || stmt.Coprocess === true ? copy(shell) : shell;
  runCmd(walk, file, text, stmt, cmd, target);
}

/** Every substitution in the words of `stmt`, its redirections and heredocs among them, but not in
 *  the statements it holds, which run where the walk reaches them, and not in the values a plain
 *  assignment gives, which run in order (`runCall`). */
function ownSubstitutions(stmt: Node): Node[][] {
  const cmd = get(stmt, "Cmd");
  const plainAssignments = cmd?.Type === "CallExpr" && list(cmd, "Args").length === 0;
  if (!plainAssignments) return substitutionsIn(stmt, stmt);
  return [...list(stmt, "Redirs").flatMap((redir) => substitutionsIn(redir, stmt))];
}

/** Every substitution under `node`, not descending into a statement other than `own` or into a
 *  function's definition. */
function substitutionsIn(node: unknown, own?: Node): Node[][] {
  const found: Node[][] = [];
  const visit = (each: unknown): void => {
    if (Array.isArray(each)) {
      for (const item of each) visit(item);
      return;
    }
    if (typeof each !== "object" || each === null) return;
    const n = each as Node;
    if (n !== own && isStatement(n)) return;
    if (n.Type === "FuncDecl") return;
    if (n.Type === "CmdSubst" || n.Type === "ProcSubst") {
      found.push(list(n, "Stmts"));
      return;
    }
    for (const value of Object.values(n)) if (typeof value === "object") visit(value);
  };
  visit(node);
  return found;
}

function runCmd(walk: Walk, file: string, text: string, stmt: Node, cmd: Node, shell: Shell): void {
  switch (cmd.Type) {
    case "CallExpr":
      runCall(walk, file, text, stmt, cmd, shell);
      return;
    case "DeclClause":
      runDecl(walk, file, text, cmd, shell);
      return;
    case "Block":
      run(walk, file, text, list(cmd, "Stmts"), shell);
      return;
    case "Subshell":
      run(walk, file, text, list(cmd, "Stmts"), copy(shell));
      return;
    case "BinaryCmd": {
      const op = str(cmd, "Op");
      const x = get(cmd, "X");
      const y = get(cmd, "Y");
      if (op === "&&" || op === "||") {
        if (x !== undefined) runStmt(walk, file, text, x, shell);
        const taken = copy(shell);
        if (y !== undefined) runStmt(walk, file, text, y, taken);
        merge(shell, [copy(shell), taken]);
        return;
      }
      // A pipeline runs each side in a subshell of its own.
      if (x !== undefined) runStmt(walk, file, text, x, copy(shell));
      if (y !== undefined) runStmt(walk, file, text, y, copy(shell));
      return;
    }
    case "IfClause":
      runIf(walk, file, text, cmd, shell);
      return;
    case "WhileClause":
      runLoop(walk, file, text, [...list(cmd, "Cond"), ...list(cmd, "Do")], shell, undefined);
      return;
    case "ForClause": {
      const loop = get(cmd, "Loop");
      const name = str(get(loop, "Name"), "Value");
      let bindItem: ((inside: Shell) => void) | undefined;
      if (loop?.Type === "WordIter" && name !== undefined) {
        const items = list(loop, "Items");
        const value = loopItems(walk, file, text, items, shell, name);
        bindItem = (inside) => assign(walk, inside, name, value);
      }
      runLoop(walk, file, text, list(cmd, "Do"), shell, bindItem);
      return;
    }
    case "CaseClause":
      runCase(walk, file, text, cmd, shell);
      return;
    case "FuncDecl": {
      const name = str(get(cmd, "Name"), "Value");
      const body = get(cmd, "Body");
      const offset = get(cmd, "Pos")?.Offset;
      if (name === undefined || body === undefined) return;
      const func: Func = { name, id: `${file}:${String(offset)}`, body, file, self: shell.self };
      shell.functions.set(name, [func]);
      return;
    }
    case "TimeClause":
    case "CoprocClause": {
      const inner = get(cmd, "Stmt");
      if (inner !== undefined)
        runStmt(walk, file, text, inner, cmd.Type === "CoprocClause" ? copy(shell) : shell);
      return;
    }
    case "ArithmCmd":
    case "LetClause":
      // Arithmetic can assign any variable it names.
      for (const found of JSON.stringify(cmd).matchAll(/"Value":"([A-Za-z_][A-Za-z0-9_]*)"/g))
        assign(
          walk,
          shell,
          found[1] ?? "",
          unknown(`$${found[1] ?? ""} is assigned by arithmetic before here`)
        );
      return;
    default:
      return;
  }
}

/** The value `for NAME in ITEMS` gives NAME in each turn: any of the items, where each is known. */
function loopItems(
  walk: Walk,
  file: string,
  text: string,
  items: readonly Node[],
  shell: Shell,
  name: string
): Value {
  if (items.length === 0) {
    const all = shell.positional;
    if (all === undefined || all.some((word) => word.kind === "spread"))
      return unknown(`$${name} is a loop's variable over arguments not known here`);
    return all.length === 0 ? { kind: "text", text: "" } : anyOf(all.filter(isValue));
  }
  const words = items.flatMap((item) => expandArg(walk, file, text, item, shell));
  if (words.length !== items.length || words.some((each) => each.length !== 1))
    return unknown(`$${name} is a loop's variable over words not known here`);
  const values = words.flat().filter(isValue);
  if (values.length !== words.length)
    return unknown(`$${name} is a loop's variable over words not known here`);
  return anyOf(values);
}

/** A loop's body walked until the state at the loop's head settles: each turn starts from what
 *  the turns before it may have left, and the loop may end at its head after any of them. From the
 *  second turn, a variable a turn still changes is unknown, so the walk settles: a value such as a
 *  string a loop appends to each turn takes a new shape on every turn. */
function runLoop(
  walk: Walk,
  file: string,
  text: string,
  body: readonly Node[],
  shell: Shell,
  bindItem: ((inside: Shell) => void) | undefined
): void {
  let head = copy(shell);
  for (let turn = 0; ; turn++) {
    const inside = copy(head);
    bindItem?.(inside);
    run(walk, file, text, body, inside);
    const next = copy(head);
    merge(next, [head, inside]);
    if (turn > 0)
      for (const [name, binding] of next.vars)
        if (json(binding) !== json(head.vars.get(name) ?? null))
          next.vars.set(name, unknown(`$${name} changes on each turn of a loop before here`));
    if (stateOf(next) === stateOf(head)) break;
    head = next;
    if (turn === 8) {
      forget(walk, head, "a loop's bindings do not settle here");
      break;
    }
  }
  shell.vars = head.vars;
  shell.cwdChanged = head.cwdChanged;
  shell.positional = head.positional;
  shell.functions = head.functions;
  shell.forgotten = head.forgotten;
}

function stateOf(shell: Shell): string {
  return json([
    [...shell.vars].sort(([a], [b]) => (a < b ? -1 : 1)),
    shell.cwdChanged,
    shell.positional ?? null,
    shell.forgotten ?? null,
    [...shell.functions].map(([name, runs]) => [name, runs.map((run) => run?.id ?? null)]).sort(),
  ]);
}

function runIf(walk: Walk, file: string, text: string, cmd: Node, shell: Shell): void {
  run(walk, file, text, list(cmd, "Cond"), shell);
  const then = copy(shell);
  run(walk, file, text, list(cmd, "Then"), then);
  const otherwise = copy(shell);
  const rest = get(cmd, "Else");
  if (rest !== undefined) {
    if (list(rest, "Cond").length > 0) runIf(walk, file, text, rest, otherwise);
    else run(walk, file, text, list(rest, "Then"), otherwise);
  }
  merge(shell, [then, otherwise]);
}

/** The one-line `case` on `${BASH_SOURCE[0]}` that sets NAME to the part before its last `/`,
 *  or to `.` where it holds none: either way the script's own directory. */
function ownDirectoryCase(cmd: Node): string | undefined {
  const word = list(get(cmd, "Word"), "Parts");
  const subject = word.length === 1 ? word[0] : undefined;
  if (subject?.Type !== "ParamExp" || str(get(subject, "Param"), "Value") !== "BASH_SOURCE")
    return undefined;
  const items = list(cmd, "Items");
  if (items.length !== 2) return undefined;
  const [withSlash, without] = items;
  const patterns = (item: Node | undefined) => list(item, "Patterns").map((p) => literal(p));
  if (patterns(withSlash).join() !== "*/*" || patterns(without).join() !== "*") return undefined;
  const assigned = (item: Node | undefined) => {
    const stmts = list(item, "Stmts");
    const assigns = stmts.length === 1 ? list(get(stmts[0], "Cmd"), "Assigns") : [];
    return assigns.length === 1 ? assigns[0] : undefined;
  };
  const first = assigned(withSlash);
  const second = assigned(without);
  const name = str(get(first, "Name"), "Value");
  if (name === undefined || str(get(second, "Name"), "Value") !== name) return undefined;
  const value = list(get(first, "Value"), "Parts")[0];
  const exp = get(value, "Exp");
  if (
    value?.Type !== "ParamExp" ||
    str(get(value, "Param"), "Value") !== "BASH_SOURCE" ||
    str(exp, "Op") !== "%" ||
    literal(get(exp, "Word")) !== "/*" ||
    literal(get(second, "Value")) !== "."
  )
    return undefined;
  return name;
}

function runCase(walk: Walk, file: string, text: string, cmd: Node, shell: Shell): void {
  const own = ownDirectoryCase(cmd);
  if (own !== undefined) {
    const self = shell.self;
    assign(
      walk,
      shell,
      own,
      mapValue(self, (scalar) => dirnameOf(scalar))
    );
    if (self.kind === "unknown")
      assign(
        walk,
        shell,
        own,
        unknown(`$${own} is the directory of a file whose path is not known here`)
      );
    return;
  }
  // In a branch, a plain variable the `case` tests holds a value its patterns match: where it is
  // unknown, its shape is theirs, and where it is known and matches none, the branch does not run.
  const subject = caseSubject(cmd);
  const branches: Shell[] = [];
  for (const item of list(cmd, "Items")) {
    const branch = copy(shell);
    if (subject !== undefined && !narrow(walk, branch, subject, list(item, "Patterns"))) continue;
    run(walk, file, text, list(item, "Stmts"), branch);
    branches.push(branch);
  }
  merge(shell, [copy(shell), ...branches]);
}

/** The variable a `case` tests, where its word is that variable alone. */
function caseSubject(cmd: Node): string | undefined {
  const parts = list(get(cmd, "Word"), "Parts");
  const only = parts.length === 1 ? parts[0] : undefined;
  const inner =
    only?.Type === "DblQuoted" && list(only, "Parts").length === 1 ? list(only, "Parts")[0] : only;
  if (inner?.Type !== "ParamExp" || !plainParam(inner) || get(inner, "Index") !== undefined)
    return undefined;
  const name = str(get(inner, "Param"), "Value") ?? "";
  return identifier(name) ? name : undefined;
}

/** Narrows `name` in `branch` to what `patterns`, a `case` item's globs, match. Answers whether
 *  the branch may run. */
function narrow(walk: Walk, branch: Shell, name: string, patterns: readonly Node[]): boolean {
  const globs = patterns.map((pattern) => literal(pattern));
  const known = globs.filter((glob): glob is string => glob !== undefined);
  if (known.length !== globs.length || known.includes("*")) return true;
  const shapes = known.map(globShape);
  if (shapes.some((each) => each === undefined)) return true;
  const shape = `(?:${shapes.join("|")})`;
  const matches = new RegExp(`^${shape}$`);
  const binding = lookup(walk, branch, name);
  if (binding === undefined || binding.kind === "unknown") {
    const why = binding?.kind === "unknown" ? binding.why : `$${name} holds nothing known here`;
    assign(walk, branch, name, { ...unknown(why), shape });
    return true;
  }
  if (binding.kind === "array" || binding.kind === "ref") return true;
  const values = (scalars(binding) ?? []).filter(
    (scalar) => scalar.kind !== "text" || matches.test(scalar.text)
  );
  if (values.length === 0) return false;
  assign(walk, branch, name, anyOf(values));
  return true;
}

/** The regular expression a glob matches, for `*`, `?` and bracket expressions, or `undefined`
 *  where a bracket expression is one the expression cannot hold, such as a POSIX class. */
function globShape(glob: string): string | undefined {
  let shape = "";
  for (let at = 0; at < glob.length; at++) {
    const char = glob[at] ?? "";
    if (char === "*") shape += ".*";
    else if (char === "?") shape += ".";
    else if (char === "[" && glob.indexOf("]", at + 2) > at) {
      const end = glob.indexOf("]", at + 2);
      const inside = glob
        .slice(at + 1, end)
        .replace(/^[!^]/, "^")
        .replace(/\\/g, "\\\\");
      if (inside.includes("[")) return undefined;
      shape += `[${inside}]`;
      at = end;
    } else shape += escapeShape(char);
  }
  try {
    new RegExp(shape);
    return shape;
  } catch {
    return undefined;
  }
}

/** What one assignment of a statement binds. */
function assignment(walk: Walk, file: string, text: string, assign_: Node, shell: Shell): void {
  const name = str(get(assign_, "Name"), "Value");
  if (name === undefined) return;
  const value = get(assign_, "Value");
  const array = get(assign_, "Array");
  if (get(assign_, "Index") !== undefined) {
    assign(walk, shell, name, unknown(`$${name} is assigned an element before here`));
    return;
  }
  if (array !== undefined) {
    const elems = list(array, "Elems");
    let binding: Binding;
    if (elems.some((elem) => get(elem, "Index") !== undefined)) {
      binding = unknown(`$${name} is assigned an array with keys before here`);
    } else {
      const words = cartesian(
        elems.map((elem) => expandArg(walk, file, text, get(elem, "Value") ?? {}, shell))
      );
      binding =
        words === undefined
          ? unknown(`$${name} holds more than ${mostValues} lists of words here`)
          : { kind: "array", alternatives: words };
    }
    if (assign_.Append === true) {
      const before = lookup(walk, shell, name);
      binding =
        before?.kind === "array" && binding.kind === "array"
          ? appendArrays(name, before.alternatives, binding.alternatives)
          : unknown(`$${name} is appended to before here`);
    }
    assign(walk, shell, name, binding);
    return;
  }
  const given: Value =
    value === undefined ? { kind: "text", text: "" } : evaluate(walk, file, text, value, shell);
  if (assign_.Append === true) {
    const before = lookup(walk, shell, name);
    const joined =
      before === undefined || before.kind === "array" || before.kind === "ref"
        ? unknown(`$${name} is appended to before here`)
        : join2(before, given);
    assign(walk, shell, name, joined);
    return;
  }
  assign(walk, shell, name, given);
}

function appendArrays(
  name: string,
  before: readonly (readonly Word[])[],
  after: readonly (readonly Word[])[]
): Binding {
  if (before.length * after.length > mostValues)
    return unknown(`$${name} holds more than ${mostValues} lists of words here`);
  return { kind: "array", alternatives: before.flatMap((a) => after.map((b) => [...a, ...b])) };
}

function runDecl(walk: Walk, file: string, text: string, cmd: Node, shell: Shell): void {
  const variant = str(get(cmd, "Variant"), "Value") ?? "";
  const args = list(cmd, "Args");
  const flags = args
    .filter((arg) => get(arg, "Name") === undefined)
    .map((arg) => literal(get(arg, "Value")) ?? "")
    .filter((flag) => /^[-+][A-Za-z]+$/.test(flag));
  const has = (letter: string) =>
    flags.some((flag) => flag.startsWith("-") && flag.includes(letter));
  // -p prints, and -f and -F name functions: none binds a variable. `export -n` takes the export
  // away and leaves the value.
  if (has("p") || has("f") || has("F")) return;
  if (variant === "export" && has("n")) return;
  const nameref = has("n");
  // `export NAME` and `readonly NAME` keep the value NAME holds.
  const keepsValue = variant === "export" || variant === "readonly";
  for (const arg of args) {
    const name = str(get(arg, "Name"), "Value");
    const value = get(arg, "Value");
    if (name === undefined) {
      // A word the shell builds into NAME or NAME=VALUE, or a flag.
      if (value === undefined) continue;
      const flag = literal(value);
      if (flag !== undefined && /^[-+][A-Za-z]+$/.test(flag)) continue;
      const built = evaluate(walk, file, text, value, shell);
      const each = scalars(built);
      if (each === undefined) {
        const why = built.kind === "unknown" ? built.why : "a name is not known";
        const shape = shapeOf(built).split("=")[0] ?? ".*";
        bindTarget(
          walk,
          shell,
          { ...unknown(why), shape },
          `is declared by \`${variant}\` before here`
        );
        continue;
      }
      for (const scalar of each) {
        if (scalar.kind !== "text") continue;
        const at = scalar.text.indexOf("=");
        const target = at < 0 ? scalar.text : scalar.text.slice(0, at);
        if (!identifier(target)) continue;
        if (at >= 0)
          assign(
            walk,
            shell,
            target,
            each.length === 1
              ? { kind: "text", text: scalar.text.slice(at + 1) }
              : unknown(`$${target} is declared with any of several values before here`)
          );
        else if (!keepsValue)
          assign(walk, shell, target, unknown(`$${target} is declared with no value here`));
      }
      continue;
    }
    if (nameref) {
      const target = value === undefined ? undefined : evaluate(walk, file, text, value, shell);
      shell.vars.set(name, {
        kind: "ref",
        target: target?.kind === "text" && identifier(target.text) ? target.text : undefined,
      });
      continue;
    }
    if (arg.Naked === true || (value === undefined && get(arg, "Array") === undefined)) {
      if (!keepsValue)
        assign(walk, shell, name, unknown(`$${name} is declared with no value here`));
      continue;
    }
    assignment(walk, file, text, arg, shell);
  }
}

/** The lists of words `lists`, one taken from each, in every way, or `undefined` past
 *  `mostValues`. */
function cartesian(
  lists: readonly (readonly (readonly Word[])[])[]
): (readonly Word[])[] | undefined {
  let out: (readonly Word[])[] = [[]];
  for (const alternatives of lists) {
    if (out.length * alternatives.length > mostValues) return undefined;
    out = out.flatMap((before) => alternatives.map((words) => [...before, ...words]));
  }
  return out;
}

/** The words `word` expands to, as any of a few lists. */
function expandArg(
  walk: Walk,
  file: string,
  text: string,
  word: Node,
  shell: Shell
): (readonly Word[])[] {
  const parts = list(word, "Parts");
  const only = parts.length === 1 ? parts[0] : undefined;
  const quoted = only?.Type === "DblQuoted";
  const inner = quoted && list(only, "Parts").length === 1 ? list(only, "Parts")[0] : only;
  if (inner?.Type === "ParamExp" && plainParam(inner)) {
    const name = str(get(inner, "Param"), "Value") ?? "";
    const index = literal(get(inner, "Index"));
    if ((name === "@" || (name === "*" && !quoted)) && index === undefined) {
      const positional = shell.positional;
      if (positional === undefined)
        return [[{ kind: "spread", why: "the arguments here are not known" }]];
      return [quoted ? positional : positional.flatMap((each) => splitWord(each))];
    }
    if (index === "@" || (index === "*" && !quoted)) {
      const binding = lookup(walk, shell, name);
      if (binding?.kind === "array")
        return binding.alternatives.map((words) =>
          quoted ? words : words.flatMap((each) => splitWord(each))
        );
      if (binding === undefined || binding.kind === "unknown" || binding.kind === "ref")
        return [[{ kind: "spread", why: `the words of \${${name}[@]} are not known here` }]];
      return (scalars(binding) ?? []).map((scalar) => [scalar]);
    }
  }
  const value = evaluate(walk, file, text, word, shell);
  // An unquoted parameter or command substitution is split into words; an arithmetic expansion
  // gives one number, which holds no blank to split on.
  const unquoted = parts.some((part) => part.Type === "ParamExp" || part.Type === "CmdSubst");
  if (value.kind === "unknown") return [[unquoted ? { kind: "spread", why: value.why } : value]];
  const each = scalars(value) ?? [];
  // A word of a few known values is one word holding any of them, unless splitting it would
  // give the values different numbers of words.
  if (
    !unquoted ||
    each.every(
      (scalar) => splitWord(scalar).length === 1 && splitWord(scalar)[0]?.kind === scalar.kind
    )
  )
    return [[value]];
  return each.map((scalar) => splitWord(scalar));
}

/** A word the shell splits on blanks, where it came from an unquoted expansion. */
function splitWord(word: Word): Word[] {
  if (word.kind !== "text") return [word];
  if (isGlob(word.text))
    return [{ kind: "spread", why: "a glob names files this scan does not list" }];
  return word.text
    .split(/[ \t\n]+/)
    .filter((each) => each !== "")
    .map((each) => ({ kind: "text", text: each }));
}

/** Whether `part` is `$NAME`, `${NAME}` or `${NAME[i]}` with nothing done to its value. */
function plainParam(part: Node): boolean {
  return (
    part.Excl !== true &&
    part.Length !== true &&
    part.Width !== true &&
    get(part, "Slice") === undefined &&
    get(part, "Repl") === undefined &&
    get(part, "Names") === undefined &&
    get(part, "Exp") === undefined
  );
}

function runCall(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  cmd: Node,
  shell: Shell
): void {
  const args = list(cmd, "Args");
  if (args.length === 0) {
    // Plain assignments bind in order, and each one's substitutions run before it binds.
    for (const assign_ of list(cmd, "Assigns")) {
      for (const inner of substitutionsIn(assign_)) run(walk, file, text, inner, copy(shell));
      assignment(walk, file, text, assign_, shell);
    }
    return;
  }
  const expanded = args.map((arg) => expandArg(walk, file, text, arg, shell));
  // Past the bound, each word that takes several lists is one word of any of their values, so
  // the command it names stays known.
  const alternatives =
    cartesian(expanded) ??
    cartesian(
      expanded.map((lists) => {
        if (lists.length === 1) return lists;
        const single = lists.every((words) => words.length === 1 && words[0]?.kind !== "spread");
        const values = lists.flat().filter(isValue);
        return single && values.length === lists.length
          ? [[anyOf(values)]]
          : [
              [
                {
                  kind: "spread" as const,
                  why: `a word takes more than ${mostValues} values here`,
                },
              ],
            ];
      })
    );
  if (alternatives === undefined) {
    runWords(
      walk,
      file,
      text,
      stmt,
      [{ kind: "spread", why: `the command's words take more than ${mostValues} values here` }],
      shell
    );
    return;
  }
  if (alternatives.length === 1) {
    runWords(walk, file, text, stmt, alternatives[0] ?? [], shell);
    return;
  }
  const results = alternatives.map((words) => {
    const branch = copy(shell);
    runWords(walk, file, text, stmt, words, branch);
    return branch;
  });
  merge(shell, results);
}

const isText = (word: Word | undefined): word is { kind: "text"; text: string } =>
  word?.kind === "text";

/** Runs the command `words` hold, once the shell has expanded them. */
function runWords(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  given: readonly Word[],
  shell: Shell
): void {
  let words = given;
  // `builtin` and `command` run the command after them; `command -v` and `-V` only name it.
  for (
    let head = words[0];
    isText(head) && (head.text === "builtin" || head.text === "command");
    head = words[0]
  ) {
    const prefix = head.text;
    words = words.slice(1);
    while (isText(words[0]) && words[0].text.startsWith("-") && words[0].text !== "-") {
      const flag = words[0].text;
      words = words.slice(1);
      if (flag === "--") break;
      if (prefix === "command" && /[vV]/.test(flag)) return;
    }
  }
  const head = words[0];
  if (head === undefined) return;
  // A command whose name is any of a few known values runs each of them.
  if (head.kind === "oneOf") {
    const results = head.values.map((value) => {
      const branch = copy(shell);
      runWords(walk, file, text, stmt, [value, ...words.slice(1)], branch);
      return branch;
    });
    merge(shell, results);
    return;
  }
  if (head.kind !== "text") {
    runUnknownHead(walk, file, text, stmt, words, shell);
    return;
  }
  const verb = head.text;
  switch (verb) {
    case ".":
    case "source":
      runSourceWords(walk, file, text, stmt, words.slice(1), shell);
      return;
    case "cd":
    case "pushd":
    case "popd":
      shell.cwdChanged = true;
      return;
    case "eval":
      refuse(
        walk,
        file,
        text,
        stmt,
        "eval runs text this scan does not parse, which may source a library"
      );
      forget(walk, shell, `eval ran before here (${relative(walk.tree, file)}:${line(stmt)})`);
      return;
    case "shopt":
      if (words.some((word) => isText(word) && word.text === "-s")) {
        if (words.some((word) => isText(word) && word.text === "expand_aliases"))
          refuse(
            walk,
            file,
            text,
            stmt,
            "an alias may name a source, and this scan does not follow aliases"
          );
        if (words.some((word) => isText(word) && word.text === "lastpipe"))
          refuse(
            walk,
            file,
            text,
            stmt,
            "lastpipe runs a pipeline's last command in this shell, and this scan reads every pipeline as subshells"
          );
      }
      return;
    case "trap":
      runTrap(walk, file, text, stmt, words.slice(1), shell);
      return;
    case "set": {
      const at = words.findIndex((word) => isText(word) && word.text === "--");
      if (at >= 0) shell.positional = words.slice(at + 1);
      else if (
        words.length > 1 &&
        !words.slice(1).some((word) => !isText(word) || /^[-+]/.test(word.text))
      )
        shell.positional = words.slice(1);
      return;
    }
    case "shift": {
      const count =
        words[1] === undefined
          ? 1
          : isText(words[1]) && /^[0-9]+$/.test(words[1].text)
            ? Number(words[1].text)
            : undefined;
      const positional = shell.positional;
      shell.positional =
        count === undefined ||
        positional === undefined ||
        positional.slice(0, count).some((word) => word.kind === "spread")
          ? undefined
          : positional.slice(count);
      return;
    }
    default:
      break;
  }
  bindByCommand(walk, shell, verb, words);
  callByName(walk, verb, shell, words.slice(1));
}

/** The variables `read`, `mapfile`, `printf -v`, `getopts`, `wait -p` and `unset` give a value. */
function bindByCommand(walk: Walk, shell: Shell, verb: string, words: readonly Word[]): void {
  const rest = words.slice(1);
  const why = `is given a value by ${verb} before here`;
  const takesValue: Record<string, string> = {
    read: "adinNptu",
    mapfile: "dnOsuCc",
    readarray: "dnOsuCc",
  };
  if (verb === "read" || verb === "mapfile" || verb === "readarray") {
    const targets: Word[] = [];
    for (let at = 0; at < rest.length; at++) {
      const word = rest[at];
      if (word === undefined) break;
      if (
        isText(word) &&
        word.text.startsWith("-") &&
        word.text.length > 1 &&
        targets.length === 0
      ) {
        const letters = word.text.slice(1);
        if (letters === "-") continue;
        const valued = [...letters].findIndex((letter) =>
          (takesValue[verb] ?? "").includes(letter)
        );
        if (valued >= 0) {
          const attached = letters.slice(valued + 1);
          const argument = attached !== "" ? undefined : rest[++at];
          if (letters[valued] === "a" && verb === "read")
            targets.push(
              attached !== ""
                ? { kind: "text", text: attached }
                : (argument ?? { kind: "text", text: "" })
            );
        }
        continue;
      }
      targets.push(word);
    }
    if (verb === "read") {
      if (targets.length === 0) targets.push({ kind: "text", text: "REPLY" });
      for (const target of targets) bindTarget(walk, shell, target, why);
    } else bindTarget(walk, shell, targets[0] ?? { kind: "text", text: "MAPFILE" }, why);
    return;
  }
  if (verb === "printf" || verb === "wait") {
    const flag = verb === "printf" ? "-v" : "-p";
    const at = rest.findIndex((word) => isText(word) && word.text === flag);
    const target = at >= 0 ? rest[at + 1] : undefined;
    if (target !== undefined) bindTarget(walk, shell, target, why);
    return;
  }
  if (verb === "getopts") {
    if (rest[1] !== undefined) bindTarget(walk, shell, rest[1], why);
    return;
  }
  if (verb === "unset") {
    if (rest.some((word) => isText(word) && word.text === "-f")) return;
    for (const word of rest)
      if (!isText(word) || !word.text.startsWith("-"))
        bindTarget(walk, shell, word, "is unset before here", true);
  }
}

/** Runs `verb` as the functions it may name, with `args` as their arguments. Where it names none,
 *  it is a program or a builtin, which binds nothing in this shell. */
function callByName(walk: Walk, verb: string, shell: Shell, args: readonly Word[]): void {
  noteFunc(walk, verb);
  const runs = shell.functions.get(verb);
  if (runs === undefined) return;
  if (runs.length === 1 && runs[0] !== null && runs[0] !== undefined) {
    callFunction(walk, runs[0], shell, args);
    return;
  }
  const results = runs.map((func) => {
    const branch = copy(shell);
    if (func !== null) callFunction(walk, func, branch, args);
    return branch;
  });
  merge(shell, results);
}

/** A command whose name the scan does not know. It is refused where it may be `source` of a file:
 *  where its name may be `source` and a word follows that `source` would take for a file, since
 *  `source` refuses any other word that starts with `-` as an invalid option. Where the name may be
 *  only a function `$(command -v NAME)` gave, that function runs; otherwise it is taken for a
 *  program, which binds nothing in this shell. */
function runUnknownHead(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  words: readonly Word[],
  shell: Shell
): void {
  const head = words[0];
  if (head === undefined || head.kind === "text" || head.kind === "path" || head.kind === "oneOf")
    return;
  const notSource = head.kind === "unknown" && (head.notSource === true || head.foreign === true);
  const next = head.kind === "spread" ? head : words[1];
  const takesFile =
    next !== undefined &&
    !(isText(next) && next.text.startsWith("-") && next.text !== "-" && next.text !== "--");
  if (!notSource && takesFile) {
    refuse(walk, file, text, stmt, `its name may be \`source\`: ${head.why}`);
    forget(
      walk,
      shell,
      `a command before here may have been \`source\` (${relative(walk.tree, file)}:${line(stmt)})`
    );
    return;
  }
  if (head.kind === "unknown" && head.names !== undefined) {
    const results = [copy(shell)];
    for (const name of head.names) {
      const branch = copy(shell);
      callByName(walk, name, branch, words.slice(1));
      results.push(branch);
    }
    merge(shell, results);
  }
}

/** `trap TEXT SIGNAL...`: the text runs when the signal comes, so it is parsed now and walked at
 *  the end of the script (`Trap`). A text the scan does not know is refused, but for one whose
 *  unknown parts are each one shell word that cannot end a command (`Unknown.template`). */
function runTrap(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  words: readonly Word[],
  shell: Shell
): void {
  const rest = isText(words[0]) && words[0].text === "--" ? words.slice(1) : words;
  const action = rest[0];
  if (action === undefined || (isText(action) && (action.text === "-p" || action.text === "-l")))
    return;
  if (rest.length === 1 && isText(action) && /^(?:[0-9]+|SIG[A-Z0-9]+|[A-Z]+)$/.test(action.text))
    return;
  const template = action.kind === "unknown" ? action.template : undefined;
  const texts: readonly Scalar[] | undefined =
    template !== undefined
      ? [{ kind: "text", text: template }]
      : action.kind === "spread"
        ? undefined
        : scalars(action);
  if (texts === undefined || texts.some((each) => each.kind !== "text")) {
    const why =
      action.kind === "unknown" || action.kind === "spread" ? action.why : "its text is a path";
    refuse(
      walk,
      file,
      text,
      stmt,
      `a trap's text this scan does not know may source a library; ${why}`
    );
    return;
  }
  for (const each of texts.filter(isTextValue)) {
    if (each.text === "" || each.text === "-") continue;
    const key = `${file}:${line(stmt)} (trap text)${texts.length > 1 ? ` ${json(each.text)}` : ""}`;
    if (walk.traps.has(key)) continue;
    const tree = parseText(key, each.text);
    if ("error" in tree) {
      refuse(walk, file, text, stmt, `a trap's text ${tree.error}`);
      continue;
    }
    walk.traps.set(key, {
      key,
      tree: tree.tree,
      text: each.text,
      script: walk.script,
      shell: copy(shell),
    });
  }
}

/** Calls `func` with `args`, the words after its name. */
function callFunction(walk: Walk, func: Func, shell: Shell, args: readonly Word[]): void {
  if (walk.running.includes(func.id)) {
    forget(walk, shell, `${func.name} calls itself, which this scan does not follow`);
    return;
  }
  const parsedFile = parse(func.file);
  if ("error" in parsedFile) return;
  // A call with the same arguments, directory and $0 as an earlier one, at which every variable
  // and function the earlier one read held what it held then, leaves what that call left.
  const key = json([func.id, args, shell.cwdChanged, shell.forgotten ?? null, shell.zero]);
  const entries = walk.memo.get(key) ?? [];
  for (const entry of entries) {
    const holds =
      entry.reads.every(([name, held]) => json(shell.vars.get(name) ?? null) === held) &&
      entry.funcs.every(([name, held]) => json(idsOf(shell.functions.get(name))) === held);
    if (!holds) continue;
    for (const [name, binding] of entry.vars) {
      if (binding === null) shell.vars.delete(name);
      else shell.vars.set(name, binding);
    }
    for (const [name, runs] of entry.functions) {
      if (runs === null) shell.functions.delete(name);
      else shell.functions.set(name, runs);
    }
    shell.cwdChanged = entry.cwdChanged;
    shell.forgotten = entry.forgotten;
    for (const [name] of entry.reads) noteRead(walk, name);
    for (const [name] of entry.funcs) noteFunc(walk, name);
    return;
  }
  const entryVars = new Map(shell.vars);
  const entryFunctions = new Map(shell.functions);
  const frame: Frame = { reads: new Set(), funcs: new Set(), open: false };
  walk.frames.push(frame);
  const locals = localNames(func.body);
  const saved = new Map([...locals].map((local) => [local, shell.vars.get(local)]));
  const outerSelf = shell.self;
  const outerPositional = shell.positional;
  shell.self = func.self;
  shell.positional = args;
  walk.running.push(func.id);
  runStmt(walk, func.file, parsedFile.text, func.body, shell);
  walk.running.pop();
  shell.self = outerSelf;
  shell.positional = outerPositional;
  for (const [local, value] of saved) {
    if (value === undefined) shell.vars.delete(local);
    else shell.vars.set(local, value);
  }
  walk.frames.pop();
  for (const outer of walk.frames) {
    for (const name of frame.reads) outer.reads.add(name);
    for (const name of frame.funcs) outer.funcs.add(name);
    if (frame.open) outer.open = true;
  }
  if (frame.open) return;
  const changed = new Set([...entryVars.keys(), ...shell.vars.keys()]);
  const changedFunctions = new Set([...entryFunctions.keys(), ...shell.functions.keys()]);
  entries.push({
    reads: [...frame.reads].map((name) => [name, json(entryVars.get(name) ?? null)]),
    funcs: [...frame.funcs].map((name) => [name, json(idsOf(entryFunctions.get(name)))]),
    vars: [...changed]
      .filter((name) => json(entryVars.get(name) ?? null) !== json(shell.vars.get(name) ?? null))
      .map((name) => [name, shell.vars.get(name) ?? null]),
    functions: [...changedFunctions]
      .filter(
        (name) => json(idsOf(entryFunctions.get(name))) !== json(idsOf(shell.functions.get(name)))
      )
      .map((name) => [name, shell.functions.get(name) ?? null]),
    cwdChanged: shell.cwdChanged,
    forgotten: shell.forgotten,
  });
  walk.memo.set(key, entries);
}

const idsOf = (runs: readonly (Func | null)[] | undefined): (string | null)[] | null =>
  runs === undefined ? null : runs.map((run) => run?.id ?? null);

/** The names a function body declares `local`, which its return gives back to the caller. */
function localNames(body: Node): Set<string> {
  const names = new Set<string>();
  const visit = (node: unknown): void => {
    if (Array.isArray(node)) {
      for (const item of node) visit(item);
      return;
    }
    if (typeof node !== "object" || node === null) return;
    const n = node as Node;
    if (n.Type === "FuncDecl") return;
    if (
      n.Type === "DeclClause" &&
      ["local", "declare", "typeset"].includes(str(get(n, "Variant"), "Value") ?? "") &&
      !list(n, "Args").some((arg) => /^-[A-Za-z]*g/.test(literal(get(arg, "Value")) ?? ""))
    )
      for (const arg of list(n, "Args")) {
        const name = str(get(arg, "Name"), "Value");
        if (name !== undefined) names.add(name);
      }
    for (const value of Object.values(n)) if (typeof value === "object") visit(value);
  };
  visit(body);
  return names;
}

function refuse(walk: Walk, file: string, text: string, stmt: Node, why: string): void {
  const start = get(stmt, "Pos")?.Offset;
  const end = get(stmt, "End")?.Offset;
  const command =
    typeof start === "number" && typeof end === "number"
      ? Buffer.from(text)
          .subarray(start, end)
          .toString()
          .replace(/\s*\n\s*/g, " ")
          .replace(/\s*;$/, "")
      : "";
  const at = `${relative(walk.tree, file)}:${line(stmt)}: ${command}`;
  if (!walk.refused.has(at)) walk.refused.set(at, why);
}

/** `.` or `source`, given `words` after it. */
function runSourceWords(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  words: readonly Word[],
  shell: Shell
): void {
  let rest = words;
  if (isText(rest[0]) && rest[0].text === "--") rest = rest.slice(1);
  else if (isText(rest[0]) && rest[0].text === "-p") {
    refuse(walk, file, text, stmt, "`source -p` searches a path this scan does not read");
    forget(
      walk,
      shell,
      `a source before here could not be read (${relative(walk.tree, file)}:${line(stmt)})`
    );
    return;
  }
  // Bash takes any other word that starts with `-` for an invalid option, and sources nothing.
  else if (isText(rest[0]) && rest[0].text.startsWith("-") && rest[0].text !== "-") return;
  const word = rest[0];
  if (word === undefined) return;
  if (word.kind === "spread") {
    refuse(walk, file, text, stmt, word.why);
    forget(
      walk,
      shell,
      `a source before here could not be read (${relative(walk.tree, file)}:${line(stmt)})`
    );
    return;
  }
  const alternatives = scalars(word);
  if (alternatives === undefined || alternatives.length === 1) {
    runSource(walk, file, text, stmt, word, shell);
    return;
  }
  const results = alternatives.map((scalar) => {
    const branch = copy(shell);
    runSource(walk, file, text, stmt, scalar, branch);
    return branch;
  });
  merge(shell, results);
}

function runSource(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  value: Value,
  shell: Shell
): void {
  let path: string | undefined;
  let why: string | undefined;
  if (value.kind === "unknown") why = value.why;
  else if (value.kind === "oneOf") why = "it is one of several values here";
  else if (value.kind === "path") {
    if (value.cwd && shell.cwdChanged)
      why = "its path is relative to a working directory the script changed";
    else path = value.path;
  } else if (isAbsolute(value.text)) path = resolve(value.text);
  else why = "a relative path reads the working directory";
  // A known path that names no regular file is a `.` that fails and binds nothing, so the scan
  // refuses it and keeps what it knows. A path it does not know may be any file, binding anything.
  if (path !== undefined && !regularFile(path)) {
    refuse(walk, file, text, stmt, "names no regular file");
    return;
  }
  if (path === undefined) {
    refuse(walk, file, text, stmt, why ?? "names no file this scan can know");
    forget(
      walk,
      shell,
      `a source before here could not be read (${relative(walk.tree, file)}:${line(stmt)})`
    );
    return;
  }
  const library = parse(path);
  walk.read.add(path);
  if ("error" in library) {
    walk.refused.set(relative(walk.tree, path), library.error);
    forget(
      walk,
      shell,
      `a file sourced before here could not be parsed (${relative(walk.tree, path)})`
    );
    return;
  }
  // The library runs in this shell, as the file it is: its BASH_SOURCE[0] is the path it was
  // sourced by, which holds while the directory does only where that path was relative.
  const source: Value =
    value.kind === "path"
      ? { kind: "path", path, cwd: value.cwd }
      : { kind: "path", path, cwd: false };
  const outerSelf = shell.self;
  shell.self = source;
  run(walk, path, library.text, list(library.tree, "Stmts"), shell);
  shell.self = outerSelf;
}

/** What `word` holds where the shell expands it, from `shell`. */
function evaluate(walk: Walk, file: string, text: string, word: Node, shell: Shell): Value {
  let value: Value = { kind: "text", text: "" };
  const parts = list(word, "Parts");
  parts.forEach((part, index) => {
    value = join2(value, evaluatePart(walk, file, text, part, shell, index === 0, false));
  });
  return value;
}

function evaluatePart(
  walk: Walk,
  file: string,
  text: string,
  part: Node,
  shell: Shell,
  first: boolean,
  quoted: boolean
): Value {
  switch (part.Type) {
    case "Lit": {
      const raw = str(part, "Value") ?? "";
      if (quoted) return { kind: "text", text: unescapeDouble(raw) };
      if (first && raw.startsWith("~"))
        return unknown("a ~ path names a home directory this scan does not know");
      const bare = raw.replace(/\\./g, "");
      if (isGlob(bare))
        return {
          ...unknown("a glob names files this scan does not list"),
          ...(/[^.ceorsu*?[\]!^-]/.test(bare)
            ? { foreign: true as const, notSource: true as const }
            : {}),
        };
      return { kind: "text", text: unescapeBare(raw) };
    }
    case "SglQuoted":
      return { kind: "text", text: str(part, "Value") ?? "" };
    case "DblQuoted": {
      let value: Value = { kind: "text", text: "" };
      for (const inner of list(part, "Parts"))
        value = join2(value, evaluatePart(walk, file, text, inner, shell, false, true));
      return value;
    }
    case "ParamExp":
      return evaluateParam(walk, file, text, part, shell);
    case "CmdSubst":
      return evaluateSubstitution(walk, file, text, list(part, "Stmts"), shell);
    case "ArithmExp":
      return {
        ...unknown("an arithmetic expansion is not evaluated by this scan"),
        foreign: true,
        notSource: true,
        shape: "-?[0-9]+",
      };
    case "ProcSubst":
      return {
        ...unknown("a process substitution names a pipe this scan cannot read"),
        foreign: true,
        notSource: true,
        shape: "/dev/fd/[0-9]+",
      };
    default:
      return unknown(`a ${String(part.Type)} part is not evaluated by this scan`);
  }
}

/** What the variable `name` holds as one word. */
function scalarOf(walk: Walk, shell: Shell, name: string): Value | undefined {
  const binding = lookup(walk, shell, name);
  if (binding === undefined) return undefined;
  if (binding.kind === "ref") return unknown(`$${name} is a nameref to a variable not known here`);
  if (binding.kind === "array") {
    const firsts = binding.alternatives.map(
      (words) => words[0] ?? { kind: "text" as const, text: "" }
    );
    if (firsts.some((word) => word.kind === "spread"))
      return unknown(`the first word of $${name} is not known here`);
    return anyOf(firsts.filter(isValue));
  }
  return binding;
}

function evaluateParam(walk: Walk, file: string, text: string, part: Node, shell: Shell): Value {
  const name = str(get(part, "Param"), "Value") ?? "";
  const index = get(part, "Index");
  const exp = get(part, "Exp");
  const plain =
    part.Excl !== true &&
    part.Length !== true &&
    part.Width !== true &&
    get(part, "Slice") === undefined &&
    get(part, "Repl") === undefined &&
    get(part, "Names") === undefined;
  if (!plain)
    return unknown(`\${${name}…} transforms its value in a way this scan does not follow`);
  const special =
    name === "0" ||
    name === "BASH_SOURCE" ||
    /^[0-9]+$/.test(name) ||
    ["#", "@", "*"].includes(name);
  let base: Value;
  if (name === "BASH_SOURCE" && (index === undefined || literal(index) === "0")) base = shell.self;
  else if (index !== undefined) {
    const at = literal(index);
    const binding = lookup(walk, shell, name);
    if (at === "0" && binding?.kind !== "array")
      base = scalarOf(walk, shell, name) ?? unknown(`\${${name}[0]} holds nothing known here`);
    else if (at === "0")
      base = scalarOf(walk, shell, name) ?? unknown(`\${${name}[0]} holds nothing known here`);
    else return unknown(`\${${name}[…]} indexes an array this scan does not follow`);
  } else if (name === "0") base = shell.zero;
  else if (/^[0-9]+$/.test(name)) base = positionalAt(shell, Number(name));
  else if (name === "#") {
    const positional = shell.positional;
    base =
      positional === undefined || positional.some((word) => word.kind === "spread")
        ? {
            ...unknown("the number of arguments here is not known"),
            foreign: true,
            notSource: true,
          }
        : { kind: "text", text: String(positional.length) };
  } else if (name === "@" || name === "*") {
    const positional = shell.positional;
    base =
      positional === undefined || positional.some((word) => word.kind !== "text")
        ? unknown("the arguments here are not known")
        : {
            kind: "text",
            text: positional
              .filter(isTextValue)
              .map((word) => word.text)
              .join(" "),
          };
  } else {
    const bound = scalarOf(walk, shell, name);
    // A variable the scan was told is unset is empty until the walk binds it, unless something
    // since forgot every binding and may have set it.
    const assumed = bound === undefined && shell.forgotten === undefined && shell.unset.has(name);
    base =
      bound ??
      (assumed
        ? { kind: "text", text: "" }
        : shell.programs.has(name) && shell.forgotten === undefined
          ? {
              ...unknown(`$${name} comes from the environment, which names a program there`),
              notSource: true,
            }
          : unknown(
              shell.forgotten !== undefined
                ? `$${name} is not known here: ${shell.forgotten}`
                : `$${name} holds nothing known here`
            ));
  }
  if (exp === undefined) return base;
  const op = str(exp, "Op");
  if (op === "?" || op === ":?") return base;
  // `${NAME:-WORD}` and `${NAME-WORD}`: the variable's value where it has one, else the word.
  // A variable the walk has not bound takes its value from the environment, which may set it,
  // unless the scan was told it is unset.
  if (op === ":-" || op === "-") {
    const fromEnvironment = !special && lookup(walk, shell, name) === undefined;
    const assumedUnset = fromEnvironment && shell.forgotten === undefined && shell.unset.has(name);
    if (
      fromEnvironment &&
      !assumedUnset &&
      shell.programs.has(name) &&
      shell.forgotten === undefined
    )
      return base;
    if (fromEnvironment && !assumedUnset)
      return unknown(
        `$${name} comes from the environment where it is set, so \${${name}${op}…} is not known here`
      );
    const word = get(exp, "Word");
    const fallback: Value =
      word === undefined ? { kind: "text", text: "" } : evaluate(walk, file, text, word, shell);
    if (assumedUnset) return fallback;
    if (base.kind === "unknown") return base;
    return mapValue(base, (scalar) =>
      scalar.kind === "text" && scalar.text === "" && op === ":-" ? fallback : scalar
    );
  }
  // `${NAME%/*}`, the part before the last `/`: exact on known text, and the directory of a path
  // whose text is absolute. A path whose text may be relative may hold no `/` at all, which
  // leaves it whole, so it is unknown.
  if (op !== "%" || literal(get(exp, "Word")) !== "/*")
    return unknown(`\${${name}…} transforms its value in a way this scan does not follow`);
  return mapValue(base, (scalar) => {
    if (scalar.kind === "text") {
      const at = scalar.text.lastIndexOf("/");
      return { kind: "text", text: at < 0 ? scalar.text : scalar.text.slice(0, at) };
    }
    return scalar.cwd
      ? unknown(`\${${name}%/*} cuts a path whose text may hold no /`)
      : { kind: "path", path: dirname(scalar.path), cwd: false };
  });
}

/** `$N`, the Nth argument of the running function or script. */
function positionalAt(shell: Shell, n: number): Value {
  const positional = shell.positional;
  if (positional === undefined) return unknown(`$${n}, an argument here, is not known`);
  for (let at = 0; at < n; at++) {
    const word = positional[at];
    if (word === undefined) return { kind: "text", text: "" };
    if (word.kind === "spread")
      return unknown(`$${n}, an argument here, is not known: ${word.why}`);
    if (at === n - 1) return word;
  }
  return { kind: "text", text: "" };
}

/** The command substitutions whose output the scan knows or bounds: `$(dirname WORD)`,
 *  `$(cd WORD && pwd)` with or without `--`, `-P` or `-L`, and the outputs of `command -v`,
 *  `type -P`, `which`, `readlink`, `realpath`, `pwd` and `mktemp`, which are a path, a name or
 *  nothing. The outputs of `printf %q` and of `mktemp` are each one shell word, which a trap's
 *  text can hold (`Unknown.template`). Any other command substitution is unknown. */
function evaluateSubstitution(
  walk: Walk,
  file: string,
  text: string,
  stmts: readonly Node[],
  shell: Shell
): Value {
  const cmd = stmts.length === 1 ? get(stmts[0], "Cmd") : undefined;
  const words = (call: Node | undefined) =>
    list(call, "Args").filter((arg, at) => at === 0 || literal(arg) !== "--");
  if (cmd?.Type === "CallExpr") {
    const args = words(cmd);
    const head = literal(args[0]);
    if (head === "dirname" && args.length === 2 && args[1] !== undefined) {
      const of = evaluate(walk, file, text, args[1], shell);
      return mapValue(of, (scalar) => dirnameOf(scalar));
    }
    const named = literal(args[args.length - 1]);
    if (
      (head === "command" && literal(args[1]) === "-v") ||
      (head === "type" && literal(args[1]) === "-P") ||
      head === "which" ||
      (head === "mise" && literal(args[1]) === "which")
    ) {
      const plain = named !== undefined && named !== "." && named !== "source";
      return {
        ...unknown(`the output of \`${head} ${named ?? "…"}\` is not known here`),
        ...(plain
          ? { notSource: true as const, ...(head === "command" ? { names: [named] } : {}) }
          : {}),
      };
    }
    if (head === "mktemp")
      return { ...opaqueWord("the output of `mktemp` is not known here"), notSource: true };
    if (head === "readlink" || head === "realpath" || head === "pwd")
      return { ...unknown(`the output of \`${head}\` is not known here`), notSource: true };
    if (head === "printf" && literal(args[1]) === "%q")
      return opaqueWord("the output of `printf %q` is not known here");
  }
  if (cmd?.Type === "BinaryCmd" && str(cmd, "Op") === "&&") {
    const cd = words(get(get(cmd, "X"), "Cmd"));
    const pwd = list(get(get(cmd, "Y"), "Cmd"), "Args").map((arg) => literal(arg));
    const pwdForm =
      pwd[0] === "pwd" && pwd.slice(1).every((flag) => flag === "-P" || flag === "-L");
    if (literal(cd[0]) === "cd" && cd.length === 2 && cd[1] !== undefined && pwdForm) {
      const to = evaluate(walk, file, text, cd[1], shell);
      return mapValue(to, (scalar) => {
        let target: string;
        if (scalar.kind === "path") {
          if (scalar.cwd && shell.cwdChanged)
            return unknown(
              "a path relative to the working directory is used after the script changed it"
            );
          target = scalar.path;
        } else if (isAbsolute(scalar.text)) target = resolve(scalar.text);
        else return unknown("a relative path reads the working directory");
        // Where the directory does not exist, cd fails and the substitution is empty.
        return isDirectory(target)
          ? { kind: "text", text: resolve(target) }
          : { kind: "text", text: "" };
      });
    }
  }
  return unknown("a command substitution this scan does not run");
}

/** Walks each script of `scripts`, read in the checkout `tree`. A script that is executable
 *  runs: its `$0` and `${BASH_SOURCE[0]}` are itself, and nothing outside it is known but what
 *  `assumptions` says. A script that is not executable and that no source of the walk reaches is
 *  a library with a caller the scan cannot see, so nothing is known in it, its `$0` and working
 *  directory included. At the end of each script, each trap it set runs (`Trap`). A function is
 *  walked only where something calls it: by name, through a value the scan knows, or from a
 *  trap's text. */
export function scanSources(
  tree: string,
  scripts: readonly string[],
  assumptions: ScanAssumptions = {}
): SourceScan {
  // The parser first, so a scan that cannot read shell fails here, whatever it was given.
  shfmt();
  const unset = new Set(assumptions.unset ?? []);
  const programs = new Set(assumptions.programs ?? []);
  const walk: Walk = {
    tree,
    script: "",
    read: new Set(),
    refused: new Map(),
    running: [],
    memo: new Map(),
    frames: [],
    traps: new Map(),
  };
  const walked = new Set<string>();
  const start = (path: string, runs: boolean): void => {
    walk.read.add(path);
    walk.script = path;
    const script = parse(path);
    if ("error" in script) {
      walk.refused.set(relative(tree, path), script.error);
      return;
    }
    const self: Value = { kind: "path", path, cwd: true };
    const shell: Shell = {
      vars: new Map(),
      cwdChanged: !runs,
      zero: runs
        ? self
        : unknown("$0 is the script that sourced this library, which no script here does"),
      self,
      positional: undefined,
      functions: new Map(),
      unset,
      programs,
    };
    run(walk, path, script.text, list(script.tree, "Stmts"), shell);
    // A trap runs when its signal comes: at any point after it is set, and at the latest when the
    // script exits. Its text is walked with what holds both where it was set and at the end.
    for (let more = true; more; ) {
      more = false;
      for (const trap of walk.traps.values()) {
        if (trap.script !== path || walked.has(trap.key)) continue;
        walked.add(trap.key);
        more = true;
        const when = copy(trap.shell);
        merge(when, [trap.shell, shell]);
        when.positional = undefined;
        when.self = trap.shell.self;
        when.zero = trap.shell.zero;
        run(walk, trap.key, trap.text, list(trap.tree, "Stmts"), when);
      }
    }
  };
  const executable = (path: string): boolean => {
    try {
      return (statSync(path).mode & 0o111) !== 0;
    } catch {
      return false;
    }
  };
  for (const path of scripts) if (executable(path)) start(path, true);
  for (const path of scripts) if (!executable(path) && !walk.read.has(path)) start(path, false);
  return {
    read: [...walk.read],
    refused: [...walk.refused].map(([at, why]) => `${at}: ${why}`),
  };
}
