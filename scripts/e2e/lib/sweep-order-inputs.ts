/**
 * The input-flow analysis of the order search (lib/sweep-order.ts): which variables of a driver
 * and its libraries can hold a command's output, and what a check reads that is not one of the
 * start's inputs, over the scripts lib/sweep-order-shell.ts parsed.
 */

import {
  alone,
  type Command,
  type Definition,
  declarers,
  eachCommand,
  heredocOp,
  type List,
  nameOf,
  type Script,
  type Word,
} from "./sweep-order-shell";

// ---- What a check reads -------------------------------------------------------------------------

/** The commands a check runs, or the words it reads, in the function whose positional parameters
 *  they read (null at the top level). */
export type Guard =
  | { readonly list: List; readonly ctx: string | null }
  | { readonly words: readonly Word[]; readonly ctx: string | null };

// The commands that read nothing of the machine: they test or print what they are given, or set a
// variable or a parameter, or work on the text of a path (`dirname`, `basename`), or print the
// start's own working directory (`pwd`) or the names of its own shell (`compgen`). `[`, `[[` and
// `test` only with no test of a file. `cat` with no operand reads only its standard input, as a
// usage text written as a here-document does; `cd`, `pushd`, `.` and `source` only of a place of
// the start's own checkout (`placeReaders`).
const inputCommands: Record<string, true> = {
  "[": true,
  "[[": true,
  test: true,
  echo: true,
  printf: true,
  ":": true,
  true: true,
  false: true,
  local: true,
  declare: true,
  typeset: true,
  readonly: true,
  export: true,
  unset: true,
  shift: true,
  return: true,
  exit: true,
  break: true,
  continue: true,
  set: true,
  shopt: true,
  let: true,
  dirname: true,
  basename: true,
  pwd: true,
  compgen: true,
};
// The commands that read the place each operand names, after the options their pattern matches:
// `cd` and `pushd` fail where it is gone or is no directory, and `.` and `source` where it is no
// file they can read. Under `set -e` such a failure ends the start, so each reads the machine,
// unless every operand names a place of the start's own checkout (`Flow.own`), which bash runs
// the script from. A path an input names (`cd "$LOCK.free"`, `cd -- "$TMPDIR/x"`) is not one: a
// dead run can leave it, remove it, or leave a file where a directory was.
const placeReaders: Record<string, RegExp> = {
  cd: /^-[LPe@]+$/,
  pushd: /^-n$/,
  ".": /(?!)/,
  source: /(?!)/,
};
// The commands of a command substitution whose output is a place of the start's own checkout,
// where each operand is one (`Flow.own`): `dirname` prints its operand's directory, and `pwd`,
// after a `cd` to such a place, prints that place.
const placePrinters: Record<string, RegExp> = {
  dirname: /^-z$/,
  cd: placeReaders.cd,
  pwd: /^-[LP]+$/,
};
// The script's own path, the place bash read it from: `$0` of a driver and `BASH_SOURCE` of the
// file it is in.
const ownPlace: Record<string, true> = { "0": true, BASH_SOURCE: true };
const fileTests = /^-([abcdefgGhkLNOprsStuwx]|nt|ot|ef)$/;
// The variables the shell sets from what a command did, whatever the scripts assign.
const shellSet = ["BASH_REMATCH", "REPLY", "MAPFILE", "OPTARG", "PIPESTATUS", "COPROC"];
const identifier = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** The variables of a driver and its libraries that can hold a command's output, and the
 *  functions whose positional parameters can: a data flow over every assignment, to a fixed
 *  point. And the variables that hold only a place of the start's own checkout (`own`). */
export class Flow {
  private readonly tainted = new Set<string>(shellSet);
  // The functions whose positional parameters can hold a command's output; "" is the script's.
  private readonly impureArgs = new Set<string>();
  private readonly assigns: { name: string; value: Word; ctx: string | null }[] = [];
  private readonly calls: { fn: string; args: Word[]; ctx: string | null }[] = [];
  private readonly positional: { value: Word; ctx: string | null }[] = [];
  // A function that sets the variable its Nth argument names (`printf -v "$1"`, `read "$2"`).
  private readonly dynamic: { fn: string; index: number }[] = [];
  // The variables that hold only a place of the start's own checkout (`own`).
  private readonly owned = new Set<string>();

  constructor(
    scripts: readonly Script[],
    private readonly functions: ReadonlyMap<string, readonly Definition[]>
  ) {
    for (const script of scripts)
      eachCommand(script.top, null, (c, ctx) => this.collect(c, ctx, functions));
    // What a function sets by the name its caller passes, which no assignment shows.
    for (const d of this.dynamic) {
      for (const c of this.calls) {
        const w = c.fn === d.fn ? c.args[d.index - 1] : undefined;
        if (w !== undefined && identifier.test(w.text)) this.tainted.add(w.text);
      }
    }
    // A variable holds only a place of the checkout where nothing but an assignment gives it a
    // value and each of its assignments is such a place: the largest such set, from every
    // assigned variable down. It comes before the flow below, whose `cd` reads it. A value's word
    // is `NAME=VALUE`, and an append (`NAME+=VALUE`) is no place of its own.
    const value = (a: { name: string; value: Word }): Word =>
      a.value.text.startsWith(`${a.name}=`)
        ? { ...a.value, text: a.value.text.slice(a.name.length + 1) }
        : a.value;
    for (const a of this.assigns) if (!this.tainted.has(a.name)) this.owned.add(a.name);
    for (let changed = true; changed; ) {
      changed = false;
      for (const a of this.assigns) {
        if (this.owned.has(a.name) && !this.own(value(a))) {
          this.owned.delete(a.name);
          changed = true;
        }
      }
    }
    const called = new Set(this.calls.map((c) => c.fn));
    // A function that no command of these files calls gets what nobody can read here.
    for (const name of functions.keys()) if (!called.has(name)) this.impureArgs.add(name);
    for (let changed = true; changed; ) {
      changed = false;
      const add = (set: Set<string>, name: string) => {
        if (set.has(name)) return;
        set.add(name);
        changed = true;
      };
      for (const a of this.assigns) if (this.impure(a.value, a.ctx)) add(this.tainted, a.name);
      for (const c of this.calls)
        if (c.args.some((w) => this.impure(w, c.ctx))) add(this.impureArgs, c.fn);
      for (const p of this.positional)
        if (this.impure(p.value, p.ctx)) add(this.impureArgs, p.ctx ?? "");
    }
  }

  /** A variable that a command sets: tainted by its name, or by the name its caller passes. */
  private target(w: Word | undefined, ctx: string | null): void {
    if (w === undefined) return;
    if (identifier.test(w.text)) this.tainted.add(w.text);
    const positional = /^"?\$\{?([1-9])\}?"?$/.exec(w.text);
    if (positional !== null && ctx !== null)
      this.dynamic.push({ fn: ctx, index: Number(positional[1]) });
  }

  private collect(
    c: Command,
    ctx: string | null,
    functions: ReadonlyMap<string, readonly Definition[]>
  ): void {
    if (c.k === "for" && c.name !== null) {
      for (const value of c.words) this.assigns.push({ name: c.name, value, ctx });
      return;
    }
    if (c.k === "arith" || c.k === "for") {
      const text = c.k === "arith" ? c.text : c.words.map((w) => w.text).join(" ");
      const set =
        /([A-Za-z_][A-Za-z0-9_]*)\s*(?:[-+*/%&|^]?=(?!=)|\+\+|--)|(?:\+\+|--)\s*([A-Za-z_][A-Za-z0-9_]*)/g;
      for (const m of text.matchAll(set)) this.tainted.add(m[1] ?? m[2]);
      return;
    }
    if (c.k !== "simple") return;
    const name = nameOf(c.words);
    const args = c.words.slice(1);
    const nameRef = args.some((w) => /^-[a-zA-Z]*n/.test(w.text));
    for (const a of c.assigns) {
      if (name !== undefined && declarers[name] === true && nameRef) this.tainted.add(a.name);
      for (const value of a.values) this.assigns.push({ name: a.name, value, ctx });
    }
    if (name === undefined) return;
    if (functions.has(name)) this.calls.push({ fn: name, args, ctx });
    // The words after the options, where `valued` names the options that take a value.
    const operands = (valued: string) => {
      const rest: Word[] = [];
      for (let i = 0; i < args.length; i++) {
        const flag = /^-([A-Za-z]+)$/.exec(args[i].text);
        if (flag === null) rest.push(args[i]);
        else if (valued.includes(flag[1].slice(-1))) {
          if (name === "read" && flag[1].endsWith("a")) this.target(args[i + 1], ctx);
          i++;
        }
      }
      return rest;
    };
    switch (name) {
      case "read":
        for (const w of operands("adinNptu")) this.target(w, ctx);
        return;
      case "mapfile":
      case "readarray": {
        const rest = operands("dnOsuCc");
        if (rest.length === 0) this.tainted.add("MAPFILE");
        else this.target(rest[rest.length - 1], ctx);
        return;
      }
      case "printf":
        if (args[0]?.text === "-v") this.target(args[1], ctx);
        return;
      case "getopts":
        this.target(args[1], ctx);
        return;
      case "let":
        for (const w of args) {
          const m = /^["']?([A-Za-z_][A-Za-z0-9_]*)/.exec(w.text);
          if (m !== null) this.tainted.add(m[1]);
        }
        return;
      case "set": {
        const dashes = args.findIndex((w) => w.text === "--");
        if (dashes >= 0)
          for (const value of args.slice(dashes + 1)) this.positional.push({ value, ctx });
        return;
      }
    }
  }

  /** Whether a word can hold a command's output: a command substitution in it that reads more
   *  than the start's inputs (`commandReads`, a call of a function counted as such a read), or a
   *  parameter that can. `$(cd "$(dirname "$0")/../.." && pwd)` reads only the place of the
   *  checkout the start runs from, and `$(compgen -e)` only the names of its own environment. */
  impure(w: Word, ctx: string | null): boolean {
    return (
      w.subs.some((sub) => {
        const out = new Set<string>();
        eachCommand(sub, ctx, (c, inner) =>
          commandReads(c, inner, out, this, (name) =>
            this.functions.has(name) ? [name] : undefined
          )
        );
        return out.size > 0;
      }) || w.vars.some((v) => this.variable(v, ctx) !== undefined)
    );
  }

  /** Whether the word `w` names a place of the start's own checkout, which bash runs the script
   *  from, so that no run of a stack leaves it, removes it or makes it no directory: it starts
   *  with an expansion; each parameter it expands is the script's own path (`ownPlace`) or a
   *  variable that holds only such a place (`owned`); each command substitution in it prints only
   *  such a place (`printsOwn`); and it holds no pattern that a glob expands against the machine.
   *  `"$(dirname "$0")/../.."`, `"$root/scripts"` and `"${BASH_SOURCE%/*}/lib.sh"` are such
   *  words, where `root` is `$(cd "$(dirname "$0")/../.." && pwd)`. A path an input names
   *  (`"$LOCK.free"`, `"$TMPDIR/x"`) is not, nor is one written in the script, since a dead run
   *  can leave it, remove it or leave a file where it was. */
  own(w: Word): boolean {
    return (
      /^"?\$[^'"]/.test(w.text) &&
      !globs(w.text) &&
      w.vars.every((v) => ownPlace[v] === true || this.owned.has(v)) &&
      w.subs.every((sub) => this.printsOwn(sub))
    );
  }

  /** Whether a command substitution prints only a place of the start's own checkout: each of its
   *  commands is `dirname`, `cd` or `pwd` (`placePrinters`) alone in its pipeline, with no
   *  assignment and no redirection but to a descriptor or /dev/null (`silent`); each operand of
   *  each is such a place (`own`); and a `pwd` comes only after a `cd`, so that it prints the
   *  place the `cd` went to. */
  private printsOwn(sub: List): boolean {
    let moved = false;
    for (const ao of sub) {
      if (ao.background === true) return false;
      for (const p of ao.pipelines) {
        const c = p.commands[0];
        if (p.negated || p.commands.length !== 1 || c?.k !== "simple" || c.assigns.length > 0)
          return false;
        if (!c.redirects.every(silent)) return false;
        const name = nameOf(c.words);
        const options = name === undefined ? undefined : placePrinters[name];
        if (options === undefined) return false;
        const operands = operandsOf(c.words, options);
        if (name === "pwd") {
          if (!moved || operands.length > 0) return false;
        } else if (operands.length === 0 || !operands.every((w) => this.own(w))) return false;
        if (name === "cd") moved = true;
      }
    }
    return sub.length > 0;
  }

  /** Why a parameter can hold a command's output, or undefined where it holds only inputs. */
  variable(v: string, ctx: string | null): string | undefined {
    if (v === "?" || v === "!") return `$${v}`;
    if (v.startsWith("!")) return `\${${v}}`;
    if (/^[1-9@*#]$/.test(v))
      return this.impureArgs.has(ctx ?? "") ? `$${v} of ${ctx ?? "the script"}` : undefined;
    if (v === "$" || v === "-" || v === "0") return undefined;
    return this.tainted.has(v) ? `$${v}` : undefined;
  }
}

/** A driver's functions, what its variables hold, and what each check reads. */
export class Reader {
  private readonly bodies = new Map<string, readonly string[]>();
  private readonly reporters = new Map<string, boolean>();

  constructor(
    readonly functions: ReadonlyMap<string, readonly Definition[]>,
    readonly flow: Flow
  ) {}

  /** What the checks read that is not an input. An empty answer: they read only inputs. */
  reads(guards: readonly Guard[]): string[] {
    const out = new Set<string>();
    for (const g of guards) {
      if ("list" in g) this.list(g.list, g.ctx, out);
      else {
        for (const w of g.words) {
          for (const sub of w.subs) this.list(sub, g.ctx, out);
          this.vars(w, g.ctx, out);
        }
      }
    }
    return [...out];
  }

  private list(list: List, ctx: string | null, out: Set<string>): void {
    eachCommand(list, ctx, (c, inner) => this.command(c, inner, out));
  }

  private vars(w: Word, ctx: string | null, out: Set<string>): void {
    for (const v of w.vars) {
      const why = this.flow.variable(v, ctx);
      if (why !== undefined) out.add(why);
    }
  }

  /** One command alone: its words' parameters, its redirections and, for a function, its body;
   *  eachCommand reaches each command inside it. */
  private command(c: Command, ctx: string | null, out: Set<string>): void {
    commandReads(c, ctx, out, this.flow, (name) =>
      this.functions.has(name)
        ? this.body(name).map((r) => (r.includes(" in ") ? r : `${r} in ${name}`))
        : undefined
    );
  }

  /** What a function's body reads, each of its definitions. */
  private body(name: string): readonly string[] {
    const known = this.bodies.get(name);
    if (known !== undefined) return known;
    this.bodies.set(name, []);
    const out = new Set<string>();
    for (const d of this.functions.get(name) ?? []) this.list(alone(d.body), name, out);
    const reads = [...out];
    this.bodies.set(name, reads);
    return reads;
  }

  /** Whether a function always ends the start: each of its definitions has a body in braces that
   *  runs, as a command of its own, an `exit` whose status is not 0 or a call of such a function,
   *  and holds no `return` before it. A body in parentheses ends only its subshell. */
  reporter(name: string): boolean {
    const known = this.reporters.get(name);
    if (known !== undefined) return known;
    this.reporters.set(name, false);
    const definitions = this.functions.get(name) ?? [];
    const ends =
      definitions.length > 0 &&
      definitions.every(
        (d) => d.body.k === "group" && !d.body.subshell && this.endsAlways(d.body.body)
      );
    this.reporters.set(name, ends);
    return ends;
  }

  private endsAlways(body: List): boolean {
    for (const ao of body) {
      let returns = false;
      eachCommand([ao], null, (c) => {
        if (c.k === "simple" && nameOf(c.words) === "return") returns = true;
      });
      if (returns) return false;
      const c = ao.pipelines.length === 1 ? ao.pipelines[0].commands : [];
      const name = c.length === 1 && c[0].k === "simple" ? nameOf(c[0].words) : undefined;
      if (name === undefined || c[0].k !== "simple") continue;
      if (name === "exit" && c[0].words[1]?.text !== "0") return true;
      if (this.functions.has(name) && this.reporter(name)) return true;
    }
    return false;
  }
}

/** What one command alone reads that is not an input, added to `out`: its words' parameters
 *  (`flow.variable`), its redirections, and its program, unless that is one of `inputCommands` (a
 *  file test aside), `cat` with no operand, or one of `placeReaders` whose every operand is a
 *  place of the start's own checkout (`flow.own`); for a function, what `body` answers of its
 *  body, or undefined where the name is no function. */
function commandReads(
  c: Command,
  ctx: string | null,
  out: Set<string>,
  flow: Flow,
  body: (name: string) => readonly string[] | undefined
): void {
  const vars = (w: Word) => {
    for (const v of w.vars) {
      const why = flow.variable(v, ctx);
      if (why !== undefined) out.add(why);
    }
  };
  if (c.k === "function") return;
  if (c.k === "arith") {
    for (const v of c.vars) {
      const why = flow.variable(v, ctx);
      if (why !== undefined) out.add(why);
    }
    return;
  }
  for (const r of c.redirects) {
    if (/<<<$/.test(r.op)) vars(r.target);
    else if (!heredocOp.test(r.op) && !silent(r)) out.add(`${r.op} ${r.target.text}`);
  }
  if (c.k === "for") for (const w of c.words) vars(w);
  if (c.k === "case") for (const w of [c.subject, ...c.arms.flatMap((a) => a.patterns)]) vars(w);
  if (c.k !== "simple") return;
  for (const a of c.assigns) for (const w of a.values) vars(w);
  for (const w of c.words) vars(w);
  let words = c.words;
  if (words[0]?.text === "command") {
    if (words[1]?.text === "-v" || words[1]?.text === "-V") return;
    words = words.slice(words[1]?.text === "-p" ? 2 : 1);
  }
  if (words.length === 0) return;
  const name = nameOf(words);
  const called = name === undefined ? undefined : body(name);
  if (name === undefined) out.add(`a command named by ${words[0].text}`);
  else if (called !== undefined) {
    for (const r of called) out.add(r);
  } else if (name === "exec" && words.length === 1) {
    // `exec` with redirections alone: the redirections above are what it reads.
  } else if (name === "cat" && words.length === 1) {
    // `cat` with no operand reads its standard input, whose redirection is read above.
  } else if (placeReaders[name] !== undefined) {
    const operands = operandsOf(words, placeReaders[name]);
    if (operands.length === 0 || !operands.every((w) => flow.own(w))) out.add(name);
  } else if (inputCommands[name] !== true) out.add(name);
  else if (name === "[" || name === "[[" || name === "test") {
    for (const w of words) if (fileTests.test(w.text)) out.add(`${name} ${w.text}`);
  }
}

/** Whether a redirection only joins a descriptor to another (`2>&1`, `<&-`) or to /dev/null or a
 *  descriptor's own file, and so reads and writes nothing of the machine. */
function silent(r: { readonly op: string; readonly target: Word }): boolean {
  return (
    (/[<>]&$/.test(r.op) && /^(\d+-?|-)$/.test(r.target.text)) ||
    /^["']?\/dev\/(null|stdout|stderr|fd\/\d+)["']?$/.test(r.target.text)
  );
}

/** The operands of the command whose words are `words`: the words after its name, past each
 *  option that `options` matches and a `--` that ends them. */
function operandsOf(words: readonly Word[], options: RegExp): readonly Word[] {
  let at = 1;
  for (; at < words.length; at++) {
    if (words[at].text === "--") return words.slice(at + 1);
    if (!options.test(words[at].text)) break;
  }
  return words.slice(at);
}

/** Whether a word's text holds a pattern that a glob expands against the machine: a `*`, `?` or
 *  `[` outside quotes and outside each `${…}` and `$(…)`, which the word's parameters and
 *  substitutions answer for. */
function globs(text: string): boolean {
  let quote: string | undefined;
  let depth = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === "\\" && quote !== "'") {
      i++;
      continue;
    }
    if (quote === "'") {
      if (c === "'") quote = undefined;
      continue;
    }
    if (c === "$" && (text[i + 1] === "(" || text[i + 1] === "{")) {
      depth++;
      i++;
      continue;
    }
    if (depth > 0) {
      if (c === "(" || c === "{") depth++;
      else if (c === ")" || c === "}") depth--;
      continue;
    }
    if (quote === '"') {
      if (c === '"') quote = undefined;
      continue;
    }
    if (c === "'" || c === '"') quote = c;
    else if (c === "*" || c === "?" || c === "[") return true;
  }
  return false;
}
