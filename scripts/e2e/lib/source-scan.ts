/** The source scan: for every `.` or `source` command a script runs, the file the shell reads
 *  there, or a refusal naming the command and why it cannot be named.
 *
 *  It reads each script as bash parses it, through the syntax tree `shfmt --to-json` prints
 *  (mvdan.cc/sh, pinned below), so a source anywhere in a command list is a command and text
 *  inside quotes or a heredoc is not. It then walks the scripts in the order the shell runs them,
 *  carrying what it knows each variable, `$0`, `${BASH_SOURCE[0]}` and the working directory
 *  hold, and names a sourced file only from what the shell holds at that source:
 *
 *  - An assignment counts once the shell has run it. One below the source, in a branch the shell
 *    may not take, in a loop that may not run, in a subshell, or in a function nothing has called
 *    leaves the variable as it was, which for a script that runs is unknown.
 *  - A library runs in its caller's shell, so it starts from the caller's bindings at the source
 *    and its own bindings reach the caller after it. A function runs at its call, with the
 *    bindings there; one nothing calls by name runs with nothing known.
 *  - `$0` is the script that runs, which in a library is its caller. `${BASH_SOURCE[0]}` is the
 *    file itself.
 *  - A path relative to the working directory holds only while the directory is the one the run
 *    started in, so after a `cd` it is unknown, and a relative literal is refused outright.
 *  - A source the scan refuses may bind anything, so after it nothing is known.
 *
 *  The syntax tree gives the order of the text; the order the shell runs it is the walk's, and
 *  where the walk cannot know it (a command named by a variable, `eval`, a trap) it takes nothing
 *  as known rather than guess. A refusal is never a skip.
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
  /** Every source it cannot name the file of, as `<path>:<line>: <command>: <why>`, and every
   *  script it cannot parse, as `<path>: <why>`. */
  refused: string[];
}

type Node = { readonly [key: string]: unknown };

/** What the scan knows a word holds. `text` is the shell's exact string. `path` is a string the
 *  scan does not know exactly but knows to name the file or directory at `path`; with `cwd`, it
 *  does so only while the working directory is the one the run started in. */
type Known = { kind: "text"; text: string } | { kind: "path"; path: string; cwd: boolean };
type Value = Known | { kind: "unknown"; why: string };

const unknown = (why: string): Value => ({ kind: "unknown", why });

/** What the shell holds at a point of the walk. */
interface Shell {
  /** Each variable the walk has bound; one it has not is unknown, inherited from outside. */
  vars: Map<string, Value>;
  /** Whether the working directory may differ from the one the run started in. */
  cwdChanged: boolean;
  /** `$0`. */
  zero: Value;
  /** `${BASH_SOURCE[0]}`: the file running now. */
  self: Value;
  /** Each function defined so far, by name. */
  functions: Map<string, Func>;
  /** Why every variable the walk has not bound since is unknown, when something forgot them. */
  forgotten?: string;
}

interface Func {
  body: Node;
  /** The file it is defined in, and `${BASH_SOURCE[0]}` there. */
  file: string;
  self: Value;
}

function copy(shell: Shell): Shell {
  return { ...shell, vars: new Map(shell.vars), functions: new Map(shell.functions) };
}

/** The shell after one of `branches` ran, from `shell`: a variable is known only where every
 *  branch left it the same. */
function merge(shell: Shell, branches: readonly Shell[]): void {
  const names = new Set<string>();
  for (const branch of branches) for (const name of branch.vars.keys()) names.add(name);
  for (const name of names) {
    const values = branches.map((branch) => branch.vars.get(name));
    const first = values[0];
    const same =
      first !== undefined &&
      first.kind !== "unknown" &&
      values.every((value) => JSON.stringify(value) === JSON.stringify(first));
    // Where some branch did not know it, that branch's reason is the cause worth naming.
    const cause = values.find((value) => value === undefined || value.kind === "unknown");
    shell.vars.set(
      name,
      same
        ? first
        : cause?.kind === "unknown"
          ? cause
          : unknown(`$${name} differs between the branches before here`)
    );
  }
  shell.cwdChanged = branches.some((branch) => branch.cwdChanged);
  for (const branch of branches)
    for (const [name, func] of branch.functions) shell.functions.set(name, func);
}

/** Forgets every binding: after a source the scan refused, or an `eval`, anything may be bound. */
function forget(shell: Shell, why: string): void {
  for (const name of shell.vars.keys())
    shell.vars.set(name, unknown(`$${name} is not known here: ${why}`));
  shell.cwdChanged = true;
  shell.forgotten = why;
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

/** The words of a call, evaluated as exact text where they are literals. */
function literal(word: Node | undefined): string | undefined {
  if (word === undefined) return undefined;
  const parts = list(word, "Parts");
  let text = "";
  for (const part of parts) {
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

const parsed = new Map<string, { tree: Node; text: string } | { error: string }>();

/** The syntax tree of the script at `path`, or why there is none. */
function parse(path: string): { tree: Node; text: string } | { error: string } {
  const cached = parsed.get(path);
  if (cached !== undefined) return cached;
  const text = readFileSync(path, "utf8");
  const run = spawnSync(shfmt(), ["--to-json", "-ln", "bash"], {
    input: text,
    encoding: "utf8",
    maxBuffer: 1 << 30,
  });
  const result =
    run.status === 0
      ? { tree: JSON.parse(run.stdout) as Node, text }
      : {
          error: `shfmt cannot parse it as bash: ${(run.stderr || String(run.error)).trim().split("\n")[0]}`,
        };
  parsed.set(path, result);
  return result;
}

/** `path` where it names a regular file, or a link to one. */
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
function dirnameOf(value: Known): Known {
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

/** `a` then `b`, as the shell joins two parts of one word. */
function join2(a: Value, b: Value): Value {
  if (a.kind === "unknown") return a;
  if (b.kind === "unknown") return b;
  if (a.kind === "text" && b.kind === "text") return { kind: "text", text: a.text + b.text };
  if (a.kind === "path" && b.kind === "text")
    return { kind: "path", path: a.path + b.text, cwd: a.cwd };
  if (a.kind === "text" && a.text === "") return b;
  return unknown("a word joins a path to other text");
}

/** The state of one walk: the tree, what it read and refused, and what it has called. */
interface Walk {
  tree: string;
  read: Set<string>;
  refused: Set<string>;
  /** `<file>#<name>` of every function defined, and of every one called by name. */
  defined: Map<string, Func & { name: string }>;
  called: Set<string>;
  /** The functions running now, so a call into one of them does not recurse. */
  running: string[];
  /** What a call into a closed function left, by what it could see (`callFunction`). */
  memo: Map<string, { vars: [string, Value | undefined][]; cwdChanged: boolean }>;
}

/** Everything `shell` would hold after running `stmts` of `file`. */
function run(walk: Walk, file: string, text: string, stmts: readonly Node[], shell: Shell): void {
  for (const stmt of stmts) runStmt(walk, file, text, stmt, shell);
}

function runStmt(walk: Walk, file: string, text: string, stmt: Node, shell: Shell): void {
  const cmd = get(stmt, "Cmd");
  if (cmd === undefined) return;
  if (stmt.Background === true || stmt.Coprocess === true) {
    runCmd(walk, file, text, stmt, cmd, copy(shell));
    return;
  }
  runCmd(walk, file, text, stmt, cmd, shell);
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
    case "ForClause": {
      // A loop runs its body any number of times, none among them: what it binds is unknown
      // before the body and after the loop.
      const body = [...list(cmd, "Cond"), ...list(cmd, "Do")];
      forgetBound(shell, cmd, body, "is bound in a loop before here");
      const loop = get(cmd, "Loop");
      const name = str(get(loop, "Name"), "Value");
      if (name !== undefined) shell.vars.set(name, unknown(`$${name} is a loop's variable here`));
      const inside = copy(shell);
      run(walk, file, text, body, inside);
      merge(shell, [copy(shell), inside]);
      return;
    }
    case "CaseClause":
      runCase(walk, file, text, cmd, shell);
      return;
    case "FuncDecl": {
      const name = str(get(cmd, "Name"), "Value");
      const body = get(cmd, "Body");
      if (name === undefined || body === undefined) return;
      const func = { body, file, self: shell.self };
      shell.functions.set(name, func);
      walk.defined.set(`${file}#${name}`, { ...func, name });
      return;
    }
    case "TimeClause": {
      const inner = get(cmd, "Stmt");
      if (inner !== undefined) runStmt(walk, file, text, inner, shell);
      return;
    }
    case "ArithmCmd":
    case "LetClause":
      // Arithmetic can assign any variable it names.
      forgetNamed(shell, JSON.stringify(cmd), "is assigned by arithmetic before here");
      return;
    default:
      return;
  }
}

/** What each tree node binds and whether it changes directory, read once per node: a function
 *  body is asked at every call into a command named by a variable. */
const effects = new WeakMap<Node, { names: Set<string>; cd: boolean }>();

/** Marks unknown every variable `stmts`, the statements of `owner`, may bind, at any depth. */
function forgetBound(shell: Shell, owner: Node, stmts: readonly Node[], why: string): void {
  let found = effects.get(owner);
  if (found === undefined) {
    found = { names: boundNames(stmts), cd: changesDirectory(stmts) };
    effects.set(owner, found);
  }
  for (const name of found.names) shell.vars.set(name, unknown(`$${name} ${why}`));
  if (found.cd) shell.cwdChanged = true;
}

/** Every variable name `stmts` may bind, read from the tree: assignments, declarations, and
 *  the names `read`, `mapfile`, `printf -v`, `getopts` and `for` give a value. */
function boundNames(stmts: readonly Node[]): Set<string> {
  const names = new Set<string>();
  const visit = (node: unknown): void => {
    if (Array.isArray(node)) {
      for (const item of node) visit(item);
      return;
    }
    if (typeof node !== "object" || node === null) return;
    const n = node as Node;
    for (const assign of list(n, "Assigns")) {
      const name = str(get(assign, "Name"), "Value");
      if (name !== undefined) names.add(name);
    }
    if (n.Type === "DeclClause")
      for (const arg of list(n, "Args")) {
        const name = str(get(arg, "Name"), "Value");
        if (name !== undefined) names.add(name);
      }
    const loopName = str(get(get(n, "Loop"), "Name"), "Value");
    if (n.Type === "ForClause" && loopName !== undefined) names.add(loopName);
    if (n.Type === "CallExpr") for (const name of commandBinds(list(n, "Args"))) names.add(name);
    for (const value of Object.values(n)) if (typeof value === "object") visit(value);
  };
  visit(stmts);
  return names;
}

/** Whether `stmts` may change the working directory, outside any subshell. */
function changesDirectory(stmts: readonly Node[]): boolean {
  let changes = false;
  const visit = (node: unknown): void => {
    if (changes) return;
    if (Array.isArray(node)) {
      for (const item of node) visit(item);
      return;
    }
    if (typeof node !== "object" || node === null) return;
    const n = node as Node;
    if (n.Type === "Subshell" || n.Type === "CmdSubst" || n.Type === "ProcSubst") return;
    if (n.Type === "CallExpr") {
      const name = literal(list(n, "Args")[0]);
      if (name === "cd" || name === "pushd" || name === "popd" || name === "eval") changes = true;
    }
    for (const value of Object.values(n)) if (typeof value === "object") visit(value);
  };
  visit(stmts);
  return changes;
}

/** The variables a command such as `read` or `printf -v` gives a value. */
function commandBinds(args: readonly Node[]): string[] {
  const name = literal(args[0]);
  const words = args.slice(1).map((arg) => literal(arg));
  const identifier = (word: string | undefined): word is string =>
    word !== undefined && /^[A-Za-z_][A-Za-z0-9_]*$/.test(word);
  if (name === "read" || name === "mapfile" || name === "readarray")
    return words.filter(identifier);
  if (name === "printf") {
    const at = words.indexOf("-v");
    const target = at >= 0 ? words[at + 1] : undefined;
    return identifier(target) ? [target] : [];
  }
  if (name === "getopts") return identifier(words[1]) ? [words[1]] : [];
  if (name === "unset") return words.filter(identifier);
  return [];
}

function forgetNamed(shell: Shell, text: string, why: string): void {
  for (const found of text.matchAll(/"Value":"([A-Za-z_][A-Za-z0-9_]*)"/g)) {
    const name = found[1] ?? "";
    shell.vars.set(name, unknown(`$${name} ${why}`));
  }
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
    shell.vars.set(
      own,
      self.kind === "unknown"
        ? unknown(`$${own} is the directory of a file whose path is not known here`)
        : dirnameOf(self)
    );
    return;
  }
  const branches = list(cmd, "Items").map((item) => {
    const branch = copy(shell);
    run(walk, file, text, list(item, "Stmts"), branch);
    return branch;
  });
  merge(shell, [copy(shell), ...branches]);
}

function runDecl(walk: Walk, file: string, text: string, cmd: Node, shell: Shell): void {
  const variant = str(get(cmd, "Variant"), "Value");
  for (const arg of list(cmd, "Args")) {
    const name = str(get(arg, "Name"), "Value");
    if (name === undefined) continue;
    const value = get(arg, "Value");
    if (arg.Naked === true || value === undefined) {
      if (variant === "local" || variant === "declare" || variant === "typeset")
        shell.vars.set(name, unknown(`$${name} is declared with no value here`));
      continue;
    }
    shell.vars.set(name, evaluate(walk, file, text, value, shell));
  }
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
  // A command substitution in any word runs in a subshell of its own; a source inside it is
  // still a source the shell runs.
  for (const word of [...args, ...list(cmd, "Assigns").map((a) => get(a, "Value"))])
    for (const inner of substitutions(word)) run(walk, file, text, inner, copy(shell));
  if (args.length === 0) {
    for (const assign of list(cmd, "Assigns")) {
      const name = str(get(assign, "Name"), "Value");
      if (name === undefined) continue;
      const value = get(assign, "Value");
      const plain =
        assign.Append !== true &&
        get(assign, "Index") === undefined &&
        get(assign, "Array") === undefined;
      shell.vars.set(
        name,
        plain && value !== undefined
          ? evaluate(walk, file, text, value, shell)
          : plain
            ? { kind: "text", text: "" }
            : unknown(`$${name} is assigned an array or appended to before here`)
      );
    }
    return;
  }
  const name = literal(args[0]);
  if (name === undefined) {
    // A command named by a variable may be any function: what any function binds is unknown.
    for (const func of shell.functions.values())
      forgetBound(
        shell,
        func.body,
        [func.body],
        "may be bound by a function called through a variable"
      );
    return;
  }
  let words = args;
  if (
    (name === "builtin" || name === "command") &&
    [".", "source"].includes(literal(args[1]) ?? "")
  )
    words = args.slice(1);
  const verb = literal(words[0]);
  if (verb === "." || verb === "source") {
    runSource(walk, file, text, stmt, words, shell);
    return;
  }
  if (verb === "cd" || verb === "pushd" || verb === "popd") {
    shell.cwdChanged = true;
    return;
  }
  if (verb === "eval") {
    refuse(
      walk,
      file,
      text,
      stmt,
      "eval runs text this scan does not parse, which may source a library"
    );
    forget(shell, `eval ran before here (${relative(walk.tree, file)}:${line(stmt)})`);
    return;
  }
  // Bash expands aliases in a script only once it turns expand_aliases on; from there a word may
  // stand for `.` or `source`, and the scan does not follow aliases.
  if (
    verb === "shopt" &&
    words.some((word) => literal(word) === "-s") &&
    words.some((word) => literal(word) === "expand_aliases")
  ) {
    refuse(
      walk,
      file,
      text,
      stmt,
      "an alias may name a source, and this scan does not follow aliases"
    );
    return;
  }
  for (const bound of commandBinds(words))
    shell.vars.set(bound, unknown(`$${bound} is read by ${verb} before here`));
  const func = verb === undefined ? undefined : shell.functions.get(verb);
  if (func !== undefined && verb !== undefined) callFunction(walk, verb, func, shell);
}

/** What a function body reads and binds, the functions it calls by name, and whether it is open:
 *  a source, an `eval`, a command named by a variable or a function defined inside it can bind
 *  names its text does not hold, or refuse a source. Read from the tree once per body. */
interface BodyFacts {
  reads: Set<string>;
  binds: Set<string>;
  calls: Set<string>;
  open: boolean;
}

const bodyFacts = new WeakMap<Node, BodyFacts>();

function factsOf(body: Node): BodyFacts {
  const cached = bodyFacts.get(body);
  if (cached !== undefined) return cached;
  const facts: BodyFacts = {
    reads: new Set(),
    binds: boundNames([body]),
    calls: new Set(),
    open: false,
  };
  const visit = (node: unknown): void => {
    if (Array.isArray(node)) {
      for (const item of node) visit(item);
      return;
    }
    if (typeof node !== "object" || node === null) return;
    const n = node as Node;
    if (n.Type === "ParamExp") facts.reads.add(str(get(n, "Param"), "Value") ?? "");
    if (n.Type === "FuncDecl" && n !== body) facts.open = true;
    if (n.Type === "ArithmCmd" || n.Type === "LetClause" || n.Type === "ArithmExp")
      for (const found of JSON.stringify(n).matchAll(/"Value":"([A-Za-z_][A-Za-z0-9_]*)"/g)) {
        facts.reads.add(found[1] ?? "");
        facts.binds.add(found[1] ?? "");
      }
    if (n.Type === "CallExpr") {
      const args = list(n, "Args");
      const first = literal(args[0]);
      const second = literal(args[1]);
      if (first === undefined && args.length > 0) facts.open = true;
      else if (first === "." || first === "source" || first === "eval") facts.open = true;
      else if (
        (first === "builtin" || first === "command") &&
        (second === "." || second === "source")
      )
        facts.open = true;
      else if (first !== undefined) facts.calls.add(first);
    }
    for (const value of Object.values(n)) if (typeof value === "object") visit(value);
  };
  visit(body);
  bodyFacts.set(body, facts);
  return facts;
}

/** A function's facts with those of every function it calls by name, as `functions` resolves the
 *  names now, and the identity of each, or `undefined` where any of them is open. */
function closedFacts(
  func: Func,
  functions: ReadonlyMap<string, Func>
): { names: string[]; reads0: boolean; identities: string[] } | undefined {
  const names = new Set<string>();
  const identities: string[] = [];
  const seen = new Set<Node>();
  let reads0 = false;
  const pending = [func];
  for (let next = pending.pop(); next !== undefined; next = pending.pop()) {
    if (seen.has(next.body)) continue;
    seen.add(next.body);
    const facts = factsOf(next.body);
    if (facts.open) return undefined;
    identities.push(`${next.file}:${String(get(next.body, "Pos")?.Offset)}`);
    for (const name of facts.reads) names.add(name);
    for (const name of facts.binds) names.add(name);
    if (facts.reads.has("0")) reads0 = true;
    for (const called of facts.calls) {
      const callee = functions.get(called);
      if (callee !== undefined) pending.push(callee);
    }
  }
  return { names: [...names].sort(), reads0, identities: identities.sort() };
}

function callFunction(walk: Walk, name: string, func: Func, shell: Shell): void {
  const key = `${func.file}#${name}`;
  walk.called.add(key);
  if (walk.running.includes(key)) {
    forgetBound(shell, func.body, [func.body], "may be bound by a function that calls itself");
    return;
  }
  const parsedFile = parse(func.file);
  if ("error" in parsedFile) return;
  // A closed function binds only the names its text and its callees' hold, and refuses nothing,
  // so what it leaves follows from those names' values, the directory and $0 alone: a call with
  // the same ones as an earlier call leaves what that call left.
  const closed = closedFacts(func, shell.functions);
  const memo =
    closed === undefined
      ? undefined
      : JSON.stringify([
          closed.identities,
          shell.cwdChanged,
          shell.forgotten ?? null,
          closed.reads0 ? shell.zero : null,
          closed.names.map((bound) => shell.vars.get(bound) ?? null),
        ]);
  const recorded = memo === undefined ? undefined : walk.memo.get(memo);
  if (recorded !== undefined) {
    for (const [bound, value] of recorded.vars) {
      if (value === undefined) shell.vars.delete(bound);
      else shell.vars.set(bound, value);
    }
    shell.cwdChanged = recorded.cwdChanged;
    return;
  }
  const locals = localNames(func.body);
  const saved = new Map([...locals].map((local) => [local, shell.vars.get(local)]));
  const outerSelf = shell.self;
  shell.self = func.self;
  walk.running.push(key);
  runStmt(walk, func.file, parsedFile.text, func.body, shell);
  walk.running.pop();
  shell.self = outerSelf;
  for (const [local, value] of saved) {
    if (value === undefined) shell.vars.delete(local);
    else shell.vars.set(local, value);
  }
  if (memo !== undefined && closed !== undefined)
    walk.memo.set(memo, {
      vars: closed.names.map((bound) => [bound, shell.vars.get(bound)]),
      cwdChanged: shell.cwdChanged,
    });
}

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
      ["local", "declare", "typeset"].includes(str(get(n, "Variant"), "Value") ?? "")
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

/** Every list of statements a command substitution or a process substitution of `word` holds. */
function substitutions(word: Node | undefined): Node[][] {
  const found: Node[][] = [];
  const visit = (node: unknown): void => {
    if (Array.isArray(node)) {
      for (const item of node) visit(item);
      return;
    }
    if (typeof node !== "object" || node === null) return;
    const n = node as Node;
    if (n.Type === "CmdSubst" || n.Type === "ProcSubst") {
      found.push(list(n, "Stmts"));
      return;
    }
    for (const value of Object.values(n)) if (typeof value === "object") visit(value);
  };
  visit(word);
  return found;
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
  walk.refused.add(`${relative(walk.tree, file)}:${line(stmt)}: ${command}: ${why}`);
}

function runSource(
  walk: Walk,
  file: string,
  text: string,
  stmt: Node,
  words: readonly Node[],
  shell: Shell
): void {
  const word = words[1];
  const value: Value =
    word === undefined ? unknown("names no file") : evaluate(walk, file, text, word, shell);
  let path: string | undefined;
  let why: string | undefined;
  if (value.kind === "unknown") why = value.why;
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
      shell,
      `a source before here could not be read (${relative(walk.tree, file)}:${line(stmt)})`
    );
    return;
  }
  const library = parse(path);
  walk.read.add(path);
  if ("error" in library) {
    walk.refused.add(`${relative(walk.tree, path)}: ${library.error}`);
    forget(shell, `a file sourced before here could not be parsed (${relative(walk.tree, path)})`);
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
      if (/[*?[]/.test(raw.replace(/\\./g, "")))
        return unknown("a glob names files this scan does not list");
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
    default:
      return unknown(`a ${String(part.Type)} part is not evaluated by this scan`);
  }
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
  let base: Value;
  if (name === "BASH_SOURCE" && (index === undefined || literal(index) === "0")) base = shell.self;
  else if (index !== undefined)
    return unknown(`\${${name}[…]} indexes an array this scan does not follow`);
  else if (name === "0") base = shell.zero;
  else
    base =
      shell.vars.get(name) ??
      unknown(
        shell.forgotten !== undefined
          ? `$${name} is not known here: ${shell.forgotten}`
          : `$${name} holds nothing known here`
      );
  if (exp === undefined) return base;
  // `${NAME:-WORD}` and `${NAME-WORD}`: the variable's value where it has one, else the word.
  // A variable the walk has not bound takes its value from the environment, which may set it.
  const op = str(exp, "Op");
  if (op === ":-" || op === "-") {
    if (shell.vars.get(name) === undefined && name !== "0" && name !== "BASH_SOURCE")
      return unknown(
        `$${name} comes from the environment where it is set, so \${${name}${op}…} is not known here`
      );
    if (base.kind === "unknown") return base;
    const empty = base.kind === "text" && base.text === "";
    if (!empty || op === "-") return base;
    const word = get(exp, "Word");
    return word === undefined
      ? { kind: "text", text: "" }
      : evaluate(walk, file, text, word, shell);
  }
  // `${NAME%/*}`, the part before the last `/`: exact on known text, and the directory of a path
  // whose text is absolute. A path whose text may be relative may hold no `/` at all, which
  // leaves it whole, so it is unknown.
  if (str(exp, "Op") !== "%" || literal(get(exp, "Word")) !== "/*")
    return unknown(`\${${name}…} transforms its value in a way this scan does not follow`);
  if (base.kind === "unknown") return base;
  if (base.kind === "text") {
    const at = base.text.lastIndexOf("/");
    return { kind: "text", text: at < 0 ? base.text : base.text.slice(0, at) };
  }
  return base.cwd
    ? unknown(`\${${name}%/*} cuts a path whose text may hold no /`)
    : { kind: "path", path: dirname(base.path), cwd: false };
}

/** `$(dirname WORD)`, and `$(cd WORD && pwd)` with or without `--` and `-P`: the forms that name
 *  a script's own directory. Any other command substitution is unknown. */
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
    if (literal(args[0]) === "dirname" && args.length === 2 && args[1] !== undefined) {
      const of = evaluate(walk, file, text, args[1], shell);
      return of.kind === "unknown" ? of : dirnameOf(of);
    }
  }
  if (cmd?.Type === "BinaryCmd" && str(cmd, "Op") === "&&") {
    const cd = words(get(get(cmd, "X"), "Cmd"));
    const pwd = list(get(get(cmd, "Y"), "Cmd"), "Args").map((arg) => literal(arg));
    const pwdForm =
      pwd[0] === "pwd" && pwd.slice(1).every((flag) => flag === "-P" || flag === "-L");
    if (literal(cd[0]) === "cd" && cd.length === 2 && cd[1] !== undefined && pwdForm) {
      const to = evaluate(walk, file, text, cd[1], shell);
      if (to.kind === "unknown") return to;
      let target: string;
      if (to.kind === "path") {
        if (to.cwd && shell.cwdChanged)
          return unknown(
            "a path relative to the working directory is used after the script changed it"
          );
        target = to.path;
      } else if (isAbsolute(to.text)) target = resolve(to.text);
      else return unknown("a relative path reads the working directory");
      // Where the directory does not exist, cd fails and the substitution is empty.
      return isDirectory(target)
        ? { kind: "text", text: resolve(target) }
        : { kind: "text", text: "" };
    }
  }
  return unknown("a command substitution this scan does not run");
}

/** Walks each script of `scripts`, read in the checkout `tree`. A script that is executable
 *  runs: its `$0` and `${BASH_SOURCE[0]}` are itself, and nothing outside it is known. A script
 *  that is not executable and that no source of the walk reaches is a library with a caller the
 *  scan cannot see, so nothing is known in it, its `$0` and working directory included. Then each
 *  function no walk called by name runs once with nothing known. */
export function scanSources(tree: string, scripts: readonly string[]): SourceScan {
  // The parser first, so a scan that cannot read shell fails here, whatever it was given.
  shfmt();
  const walk: Walk = {
    tree,
    read: new Set(),
    refused: new Set(),
    defined: new Map(),
    called: new Set(),
    running: [],
    memo: new Map(),
  };
  const start = (path: string, runs: boolean): void => {
    walk.read.add(path);
    const script = parse(path);
    if ("error" in script) {
      walk.refused.add(`${relative(tree, path)}: ${script.error}`);
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
      functions: new Map(),
    };
    run(walk, path, script.text, list(script.tree, "Stmts"), shell);
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
  for (const [key, func] of walk.defined) {
    if (walk.called.has(key)) continue;
    const file = parse(func.file);
    if ("error" in file) continue;
    walk.called.add(key);
    const shell: Shell = {
      vars: new Map(),
      cwdChanged: true,
      zero: unknown("$0 is the script that calls this function, which no script here does by name"),
      self: func.self,
      functions: new Map(),
      forgotten: `nothing calls ${func.name} by name, so nothing it reads is known`,
    };
    walk.running.push(key);
    runStmt(walk, func.file, file.text, func.body, shell);
    walk.running.pop();
  }
  return { read: [...walk.read], refused: [...walk.refused] };
}
