/**
 * The shell syntax of the order search (lib/sweep-order.ts): a script read as bash parses it,
 * into its words, commands and lists, with every function it defines and each library of
 * scripts/e2e/lib it sources. Nothing here decides what a check reads (lib/sweep-order-inputs.ts)
 * or which commands must come after the removal (lib/sweep-order-policy.ts).
 */

// ---- The shell's words, commands and lists, as bash parses them ----------------------------------

/** A word: its text, where it starts, the commands of each substitution in it, and each parameter
 *  it expands (a name, a digit or a special parameter, and `!name` for an indirect one). */
export interface Word {
  readonly text: string;
  readonly at: string;
  readonly subs: List[];
  readonly vars: string[];
}
interface Redirect {
  readonly op: string;
  readonly target: Word;
}
/** NAME=VALUE before a command or as an argument of `local`, `declare` and the like; each word
 *  of an array is a value. */
interface Assignment {
  readonly name: string;
  readonly values: Word[];
}
export type Command =
  | {
      readonly k: "simple";
      readonly at: string;
      readonly assigns: Assignment[];
      readonly words: Word[];
      readonly redirects: Redirect[];
    }
  | {
      readonly k: "group";
      readonly at: string;
      readonly subshell: boolean;
      readonly body: List;
      readonly redirects: Redirect[];
    }
  | {
      readonly k: "if";
      readonly at: string;
      readonly arms: { readonly cond: List | null; readonly body: List }[];
      readonly redirects: Redirect[];
    }
  | {
      readonly k: "loop";
      readonly at: string;
      readonly cond: List;
      readonly body: List;
      readonly redirects: Redirect[];
    }
  | {
      readonly k: "for";
      readonly at: string;
      readonly name: string | null;
      readonly words: Word[];
      readonly body: List;
      readonly redirects: Redirect[];
    }
  | {
      readonly k: "case";
      readonly at: string;
      readonly subject: Word;
      readonly arms: { readonly patterns: Word[]; readonly body: List }[];
      readonly redirects: Redirect[];
    }
  | { readonly k: "function"; readonly at: string; readonly name: string; readonly body: Command }
  | {
      readonly k: "arith";
      readonly at: string;
      readonly text: string;
      readonly vars: string[];
      readonly subs: List[];
    };
interface Pipeline {
  readonly negated: boolean;
  readonly commands: Command[];
}
/** Pipelines joined by `&&` and `||`, and whether the list runs in the background (`&`), in a
 *  subshell of its own. */
export interface AndOr {
  readonly pipelines: Pipeline[];
  readonly background?: true;
}
export type List = AndOr[];

type Token =
  | { readonly t: "word"; readonly word: Word }
  | { readonly t: "op"; readonly op: string; readonly line: number; readonly glued: boolean }
  | { readonly t: "redirect"; readonly op: string; readonly line: number }
  | { readonly t: "newline"; readonly line: number }
  | { readonly t: "end"; readonly line: number };

/** A script the parser cannot read, with the line where it stopped. */
export class Unparsed extends Error {}

const controlOps = [";;&", "&&", "||", ";;", ";&", "|&", ";", "&", "|", "(", ")"];
const redirectOps = ["&>>", "<<<", "<<-", "&>", ">>", "<<", "<&", ">&", "<>", ">|", "<", ">"];
const special = "?!@*#$-0123456789";
// A here-document's operator, `<<` or `<<-`, with its descriptor; `<<<` is a here-string.
export const heredocOp = /(^|[^<])<<-?$/;

/** The characters of a script, read into tokens on demand, with a parser of its own for each
 *  command substitution, so a `)` of a `case` in one does not close it. */
class Lexer {
  pos = 0;
  line: number;
  private readonly heredocs: { word: string; tabs: boolean }[] = [];

  constructor(
    readonly src: string,
    readonly file: string,
    line = 1
  ) {
    this.line = line;
  }

  fail(why: string): never {
    throw new Unparsed(`${this.file}:${this.line}: ${why}`);
  }

  /** Whether the text at the cursor matches `re`. */
  ahead(re: RegExp): boolean {
    return re.test(this.src.slice(this.pos, this.pos + 80));
  }

  heredoc(word: string, tabs: boolean): void {
    this.heredocs.push({ word: word.replace(/["'\\]/g, ""), tabs });
  }

  next(): Token {
    let glued = true;
    for (;;) {
      const c = this.src[this.pos];
      if (c === " " || c === "\t") {
        this.pos++;
        glued = false;
      } else if (c === "\\" && this.src[this.pos + 1] === "\n") {
        this.pos += 2;
        this.line++;
        glued = false;
      } else if (c === "#") {
        while (this.pos < this.src.length && this.src[this.pos] !== "\n") this.pos++;
      } else break;
    }
    const line = this.line;
    if (this.pos >= this.src.length) return { t: "end", line };
    const c = this.src[this.pos];
    if (c === "\n") {
      this.pos++;
      this.line++;
      this.readHeredocs();
      return { t: "newline", line };
    }
    if ((c === "<" || c === ">") && this.src[this.pos + 1] === "(") return this.word();
    if (this.src.startsWith("((", this.pos)) {
      this.pos += 2;
      return { t: "op", op: "((", line, glued };
    }
    for (const op of redirectOps) {
      if (this.src.startsWith(op, this.pos)) {
        this.pos += op.length;
        return { t: "redirect", op, line };
      }
    }
    for (const op of controlOps) {
      if (this.src.startsWith(op, this.pos)) {
        this.pos += op.length;
        return { t: "op", op, line, glued };
      }
    }
    return this.word();
  }

  private readHeredocs(): void {
    for (const { word, tabs } of this.heredocs.splice(0)) {
      while (this.pos < this.src.length) {
        const end = this.src.indexOf("\n", this.pos);
        const text = this.src.slice(this.pos, end < 0 ? this.src.length : end);
        this.pos = end < 0 ? this.src.length : end + 1;
        this.line++;
        if ((tabs ? text.replace(/^\t+/, "") : text) === word) break;
      }
    }
  }

  /** One word: up to a blank, a newline or an operator outside quotes; in `[[`, up to a blank or
   *  a newline only. */
  private word(cond = false): Token {
    const line = this.line;
    const subs: List[] = [];
    const vars: string[] = [];
    let text = "";
    const stops = cond ? " \t\n" : " \t\n;&|()<>";
    while (this.pos < this.src.length) {
      const c = this.src[this.pos];
      if ((c === "<" || c === ">") && this.src[this.pos + 1] === "(" && !cond) {
        this.pos += 2;
        subs.push(new Parser(this).untilParen());
        text += `${c}(…)`;
      } else if (c === "(" && /[@!?*+]$/.test(text)) {
        // An extended glob, @(…) and the like, is one word.
        text += this.balanced();
      } else if (stops.includes(c)) {
        break;
      } else if (c === "\\") {
        if (this.src[this.pos + 1] === "\n") this.line++;
        text += this.src.slice(this.pos, this.pos + 2);
        this.pos += 2;
      } else if (c === "'") {
        const end = this.src.indexOf("'", this.pos + 1);
        if (end < 0) this.fail("a single quote that does not close");
        text += this.src.slice(this.pos, end + 1);
        this.count(this.pos, end);
        this.pos = end + 1;
      } else if (c === '"') {
        text += this.double(subs, vars);
      } else if (c === "$") {
        text += this.dollar(subs, vars);
      } else if (c === "`") {
        text += this.backquote(subs);
      } else {
        text += c;
        this.pos++;
      }
    }
    const next = this.src[this.pos];
    if (
      !cond &&
      (next === "<" || next === ">") &&
      /^(\d+|\{[A-Za-z_][A-Za-z0-9_]*\})$/.test(text)
    ) {
      for (const op of redirectOps) {
        if (this.src.startsWith(op, this.pos)) {
          this.pos += op.length;
          return { t: "redirect", op: `${text}${op}`, line };
        }
      }
    }
    return { t: "word", word: { text, at: `${this.file}:${line}`, subs, vars } };
  }

  private count(from: number, to: number): void {
    for (let i = from; i < to; i++) if (this.src[i] === "\n") this.line++;
  }

  /** A parenthesised run of text, returned whole. */
  private balanced(): string {
    const start = this.pos;
    let depth = 0;
    while (this.pos < this.src.length) {
      const c = this.src[this.pos];
      this.pos++;
      if (c === "\\") this.pos++;
      else if (c === "(") depth++;
      else if (c === ")") {
        depth--;
        if (depth === 0) break;
      } else if (c === "\n") this.line++;
    }
    return this.src.slice(start, this.pos);
  }

  private double(subs: List[], vars: string[]): string {
    const start = this.pos;
    this.pos++;
    while (this.pos < this.src.length && this.src[this.pos] !== '"') {
      const c = this.src[this.pos];
      if (c === "\\") {
        if (this.src[this.pos + 1] === "\n") this.line++;
        this.pos += 2;
      } else if (c === "$") this.dollar(subs, vars, true);
      else if (c === "`") this.backquote(subs);
      else {
        if (c === "\n") this.line++;
        this.pos++;
      }
    }
    if (this.pos >= this.src.length) this.fail("a double quote that does not close");
    this.pos++;
    return this.src.slice(start, this.pos);
  }

  /** A `$` expansion: a command substitution parsed whole, an arithmetic one, a `${…}` with what
   *  it holds, or a parameter. Inside double quotes, `$'` and `$"` are a `$` alone. Returns its
   *  text. */
  private dollar(subs: List[], vars: string[], quoted = false): string {
    const start = this.pos;
    const next = this.src[this.pos + 1];
    if (this.src.startsWith("$((", this.pos)) {
      this.pos += 3;
      this.arithmetic(vars, subs);
    } else if (next === "(") {
      this.pos += 2;
      subs.push(new Parser(this).untilParen());
    } else if (next === "{") {
      this.pos += 2;
      this.brace(subs, vars);
    } else if (next === "'" && !quoted) {
      this.pos += 2;
      while (this.pos < this.src.length && this.src[this.pos] !== "'") {
        if (this.src[this.pos] === "\\") this.pos++;
        if (this.src[this.pos] === "\n") this.line++;
        this.pos++;
      }
      this.pos++;
    } else if (next === '"' && !quoted) {
      this.pos++;
      this.double(subs, vars);
    } else {
      const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(this.src.slice(this.pos + 1, this.pos + 65));
      if (name !== null) {
        vars.push(name[0]);
        this.pos += 1 + name[0].length;
      } else if (next !== undefined && special.includes(next)) {
        vars.push(next);
        this.pos += 2;
      } else this.pos++;
    }
    return this.src.slice(start, this.pos);
  }

  /** The inside of `${…}`: its parameter, then what follows it to the closing brace. */
  private brace(subs: List[], vars: string[]): void {
    const head = /^([#!]?)([A-Za-z_][A-Za-z0-9_]*|[?!@*#$\-0-9])/.exec(
      this.src.slice(this.pos, this.pos + 66)
    );
    if (head !== null) {
      // `${#name}` is a length, and `${!name}` reads the variable that name names.
      vars.push(head[1] === "!" ? `!${head[2]}` : head[2]);
      this.pos += head[0].length;
    }
    let depth = 0;
    while (this.pos < this.src.length) {
      const c = this.src[this.pos];
      if (c === "}" && depth === 0) {
        this.pos++;
        return;
      }
      if (c === "\\") this.pos += 2;
      else if (c === '"') this.double(subs, vars);
      else if (c === "'") {
        const end = this.src.indexOf("'", this.pos + 1);
        if (end < 0) this.fail("a single quote in a parameter expansion that does not close");
        this.count(this.pos, end);
        this.pos = end + 1;
      } else if (c === "$") this.dollar(subs, vars);
      else if (c === "`") this.backquote(subs);
      else {
        if (c === "{") depth++;
        else if (c === "}") depth--;
        else if (c === "\n") this.line++;
        this.pos++;
      }
    }
    this.fail("a parameter expansion that does not close");
  }

  /** The inside of `$((…))` or `((…))`, through its closing `))`: each bare name in it is a
   *  parameter it reads. Returns its text. */
  arithmetic(vars: string[], subs: List[]): string {
    const start = this.pos;
    let depth = 0;
    while (this.pos < this.src.length) {
      const c = this.src[this.pos];
      if (c === ")" && depth === 0 && this.src[this.pos + 1] === ")") {
        const text = this.src.slice(start, this.pos);
        this.pos += 2;
        for (const name of text.matchAll(/[A-Za-z_][A-Za-z0-9_]*/g)) vars.push(name[0]);
        return text;
      }
      if (c === "$") {
        this.dollar(subs, vars);
        continue;
      }
      if (c === "(") depth++;
      else if (c === ")") depth--;
      else if (c === "\n") this.line++;
      this.pos++;
    }
    return this.fail("an arithmetic (( that does not close");
  }

  private backquote(subs: List[]): string {
    const start = this.pos;
    const line = this.line;
    let inner = "";
    this.pos++;
    while (this.pos < this.src.length && this.src[this.pos] !== "`") {
      const c = this.src[this.pos];
      if (c === "\\" && "`\\$".includes(this.src[this.pos + 1] ?? "")) {
        inner += this.src[this.pos + 1];
        this.pos += 2;
        continue;
      }
      if (c === "\n") this.line++;
      inner += c;
      this.pos++;
    }
    if (this.pos >= this.src.length) this.fail("a backquote that does not close");
    this.pos++;
    subs.push(new Parser(new Lexer(inner, this.file, line)).program());
    return this.src.slice(start, this.pos);
  }

  /** The words of `[[ … ]]` after its `[[`, through its `]]`. */
  condition(): Word[] {
    const words: Word[] = [];
    for (;;) {
      const c = this.src[this.pos];
      if (c === " " || c === "\t") this.pos++;
      else if (c === "\n") {
        this.pos++;
        this.line++;
      } else if (c === "\\" && this.src[this.pos + 1] === "\n") {
        this.pos += 2;
        this.line++;
      } else if (this.pos >= this.src.length) this.fail("a [[ that does not close");
      else if (this.ahead(/^\]\](?=$|[\s;&|)])/)) {
        this.pos += 2;
        return words;
      } else {
        const token = this.word(true);
        if (token.t === "word") words.push(token.word);
      }
    }
  }
}

const reserved = (t: Token, ...words: string[]) => t.t === "word" && words.includes(t.word.text);
const op = (t: Token, ...ops: string[]) => t.t === "op" && ops.includes(t.op);
export const declarers: Record<string, true> = {
  local: true,
  declare: true,
  typeset: true,
  export: true,
  readonly: true,
};
const assignment = /^([A-Za-z_][A-Za-z0-9_]*)(\[[^\]]*\])?\+?=/;

/** A recursive-descent parser of bash's grammar over one lexer, one token ahead. */
class Parser {
  private held: Token | undefined;

  constructor(private readonly lx: Lexer) {}

  private peek(): Token {
    this.held ??= this.lx.next();
    return this.held;
  }

  private take(): Token {
    const t = this.peek();
    this.held = undefined;
    return t;
  }

  private expect(...words: string[]): void {
    const t = this.take();
    if (!reserved(t, ...words) && !op(t, ...words)) this.lx.fail(`expected ${words.join(" or ")}`);
  }

  private lines(): void {
    while (this.peek().t === "newline") this.take();
  }

  program(): List {
    const list = this.list(() => false);
    if (this.take().t !== "end") this.lx.fail("text after the end of the script");
    return list;
  }

  /** A substitution's list, through its closing parenthesis. */
  untilParen(): List {
    const list = this.list((t) => op(t, ")"));
    if (!op(this.take(), ")")) this.lx.fail("a $( that does not close");
    return list;
  }

  private list(stop: (t: Token) => boolean): List {
    const items: List = [];
    for (;;) {
      this.lines();
      const t = this.peek();
      if (t.t === "end" || stop(t)) return items;
      const ao = this.andOr();
      const after = this.peek();
      items.push(op(after, "&") ? { ...ao, background: true } : ao);
      if (op(after, ";", "&") || after.t === "newline") this.take();
    }
  }

  private andOr(): AndOr {
    const pipelines = [this.pipeline()];
    while (op(this.peek(), "&&", "||")) {
      this.take();
      this.lines();
      pipelines.push(this.pipeline());
    }
    return { pipelines };
  }

  private pipeline(): Pipeline {
    let negated = false;
    while (reserved(this.peek(), "!", "time")) {
      if (reserved(this.take(), "!")) negated = true;
    }
    const commands = [this.command()];
    while (op(this.peek(), "|", "|&")) {
      this.take();
      this.lines();
      commands.push(this.command());
    }
    return { negated, commands };
  }

  private command(): Command {
    const t = this.peek();
    const at =
      t.t === "word" ? t.word.at : `${this.lx.file}:${t.t === "end" ? this.lx.line : t.line}`;
    if (op(t, "(")) {
      this.take();
      const body = this.list((s) => op(s, ")"));
      this.expect(")");
      return { k: "group", at, subshell: true, body, redirects: this.redirects() };
    }
    if (op(t, "((")) {
      this.take();
      const vars: string[] = [];
      const subs: List[] = [];
      const text = this.lx.arithmetic(vars, subs);
      return { k: "arith", at, text, vars, subs };
    }
    if (t.t === "word") {
      switch (t.word.text) {
        case "if":
          return this.ifCommand(at);
        case "while":
        case "until":
          return this.loop(at);
        case "for":
          return this.forCommand(at);
        case "case":
          return this.caseCommand(at);
        case "{": {
          this.take();
          const body = this.list((s) => reserved(s, "}"));
          this.expect("}");
          return { k: "group", at, subshell: false, body, redirects: this.redirects() };
        }
        case "function": {
          this.take();
          const name = this.take();
          if (name.t !== "word") return this.lx.fail("a function with no name");
          if (this.lx.ahead(/^[ \t]*\([ \t]*\)/)) {
            this.expect("(");
            this.expect(")");
          }
          this.lines();
          return { k: "function", at, name: name.word.text, body: this.command() };
        }
      }
      if (/^[A-Za-z_][A-Za-z0-9_.:-]*$/.test(t.word.text) && this.lx.ahead(/^[ \t]*\([ \t]*\)/)) {
        this.take();
        this.expect("(");
        this.expect(")");
        this.lines();
        return { k: "function", at, name: t.word.text, body: this.command() };
      }
    }
    return this.simple(at);
  }

  private redirects(): Redirect[] {
    const out: Redirect[] = [];
    while (this.peek().t === "redirect") out.push(this.redirect());
    return out;
  }

  private redirect(): Redirect {
    const t = this.take();
    const target = this.take();
    if (t.t !== "redirect" || target.t !== "word")
      return this.lx.fail("a redirection with no word");
    if (heredocOp.test(t.op)) this.lx.heredoc(target.word.text, t.op.endsWith("-"));
    return { op: t.op, target: target.word };
  }

  private simple(at: string): Command {
    const assigns: Assignment[] = [];
    const words: Word[] = [];
    const redirects: Redirect[] = [];
    for (;;) {
      const t = this.peek();
      if (t.t === "redirect") {
        redirects.push(this.redirect());
        continue;
      }
      if (t.t !== "word") break;
      this.take();
      const name = assignment.exec(t.word.text);
      if (name !== null && (words.length === 0 || declarers[words[0].text] === true)) {
        const next = this.peek();
        if (t.word.text.endsWith("=") && next.t === "op" && next.op === "(" && next.glued) {
          this.take();
          const values: Word[] = [];
          for (;;) {
            this.lines();
            const v = this.take();
            if (op(v, ")")) break;
            if (v.t !== "word") return this.lx.fail("an array that does not close");
            values.push(v.word);
          }
          assigns.push({ name: name[1], values });
        } else assigns.push({ name: name[1], values: [t.word] });
        continue;
      }
      words.push(t.word);
      if (words.length === 1 && t.word.text === "[[") words.push(...this.lx.condition());
    }
    if (assigns.length === 0 && words.length === 0 && redirects.length === 0) {
      const t = this.peek();
      this.lx.fail(`unexpected ${t.t === "op" || t.t === "redirect" ? t.op : t.t}`);
    }
    return { k: "simple", at, assigns, words, redirects };
  }

  private ifCommand(at: string): Command {
    this.take();
    const arms: { cond: List | null; body: List }[] = [];
    for (;;) {
      const cond = this.list((s) => reserved(s, "then"));
      this.expect("then");
      const body = this.list((s) => reserved(s, "elif", "else", "fi"));
      arms.push({ cond, body });
      const t = this.take();
      if (reserved(t, "elif")) continue;
      if (reserved(t, "else")) {
        arms.push({ cond: null, body: this.list((s) => reserved(s, "fi")) });
        this.expect("fi");
      } else if (!reserved(t, "fi")) this.lx.fail("an if with no fi");
      return { k: "if", at, arms, redirects: this.redirects() };
    }
  }

  private loop(at: string): Command {
    this.take();
    const cond = this.list((s) => reserved(s, "do"));
    this.expect("do");
    const body = this.list((s) => reserved(s, "done"));
    this.expect("done");
    return { k: "loop", at, cond, body, redirects: this.redirects() };
  }

  private forCommand(at: string): Command {
    this.take();
    let name: string | null = null;
    const words: Word[] = [];
    if (op(this.peek(), "((")) {
      this.take();
      const vars: string[] = [];
      const subs: List[] = [];
      const text = this.lx.arithmetic(vars, subs);
      words.push({ text: `((${text}))`, at, subs, vars });
    } else {
      const t = this.take();
      if (t.t !== "word") return this.lx.fail("a for with no name");
      name = t.word.text;
      this.lines();
      if (reserved(this.peek(), "in")) {
        this.take();
        for (let w = this.peek(); w.t === "word"; w = this.peek()) {
          words.push(w.word);
          this.take();
        }
      } else words.push({ text: '"$@"', at, subs: [], vars: ["@"] });
    }
    if (op(this.peek(), ";")) this.take();
    this.lines();
    this.expect("do");
    const body = this.list((s) => reserved(s, "done"));
    this.expect("done");
    return { k: "for", at, name, words, body, redirects: this.redirects() };
  }

  private caseCommand(at: string): Command {
    this.take();
    const subject = this.take();
    if (subject.t !== "word") return this.lx.fail("a case with no word");
    this.lines();
    this.expect("in");
    const arms: { patterns: Word[]; body: List }[] = [];
    for (;;) {
      this.lines();
      if (reserved(this.peek(), "esac")) {
        this.take();
        return { k: "case", at, subject: subject.word, arms, redirects: this.redirects() };
      }
      if (op(this.peek(), "(")) this.take();
      const patterns: Word[] = [];
      for (;;) {
        const p = this.take();
        if (p.t !== "word") return this.lx.fail("a case pattern that is no word");
        patterns.push(p.word);
        if (!op(this.peek(), "|")) break;
        this.take();
      }
      this.expect(")");
      const body = this.list((s) => op(s, ";;", ";&", ";;&") || reserved(s, "esac"));
      if (op(this.peek(), ";;", ";&", ";;&")) this.take();
      arms.push({ patterns, body });
    }
  }
}

// ---- What each script holds ---------------------------------------------------------------------

/** A function a script defines. */
export interface Definition {
  readonly name: string;
  readonly body: Command;
}

/** One script, parsed: its top-level list, every function it defines, each library of
 *  scripts/e2e/lib it sources, by its path under the root, and the options its first line gives
 *  bash where the script runs as a program (`firstLineOptions`). */
export interface Script {
  readonly top: List;
  readonly definitions: readonly Definition[];
  readonly libraries: readonly string[];
  readonly options: readonly string[];
}

/** The options a script's first line gives bash where the script runs as a program, the words
 *  after the `bash` (or `sh`) it names, as `set` reads them: `-e` of `#!/bin/bash -e`, which Linux
 *  hands bash as one word, and of `#!/usr/bin/env -S bash -e`, which `env` splits. Empty where the
 *  line names no such shell. */
function firstLineOptions(text: string): string[] {
  const end = text.indexOf("\n");
  const line = end === -1 ? text : text.slice(0, end);
  if (!line.startsWith("#!")) return [];
  const words = line.slice(2).trim().split(/\s+/);
  const shell = words.findIndex((word) => /(^|\/)(ba)?sh$/.test(word));
  return shell === -1 ? [] : words.slice(shell + 1);
}

// A library of scripts/e2e/lib a script sources: `. "$root/scripts/e2e/lib/<name>.sh"`, or a
// library's sibling through its own location, `. "${BASH_SOURCE[0]%/*}/<name>.sh"`.
const library = /\/lib\/([A-Za-z0-9_.-]+\.sh)["']?$/;
const siblingLibrary = /\$\{BASH_SOURCE\[0\]%\/\*\}\/([A-Za-z0-9_.-]+\.sh)["']?$/;

/** A list of one command. */
export const alone = (c: Command): List => [{ pipelines: [{ negated: false, commands: [c] }] }];

/** Each command of `list` in the order of its text, with the function each is in (null at the
 *  top level): the commands of each compound command, of each substitution in each word, and of
 *  each function's body. */
export function eachCommand(
  list: List,
  ctx: string | null,
  visit: (c: Command, ctx: string | null) => void
): void {
  const words = (ws: readonly Word[], ctx: string | null) => {
    for (const w of ws) for (const sub of w.subs) eachCommand(sub, ctx, visit);
  };
  const one = (c: Command, ctx: string | null): void => {
    visit(c, ctx);
    if (c.k === "function") {
      one(c.body, c.name);
      return;
    }
    if (c.k === "arith") {
      for (const sub of c.subs) eachCommand(sub, ctx, visit);
      return;
    }
    words(
      c.redirects.map((r) => r.target),
      ctx
    );
    switch (c.k) {
      case "simple":
        words([...c.assigns.flatMap((a) => a.values), ...c.words], ctx);
        break;
      case "group":
        eachCommand(c.body, ctx, visit);
        break;
      case "if":
        for (const arm of c.arms) {
          if (arm.cond !== null) eachCommand(arm.cond, ctx, visit);
          eachCommand(arm.body, ctx, visit);
        }
        break;
      case "loop":
        eachCommand(c.cond, ctx, visit);
        eachCommand(c.body, ctx, visit);
        break;
      case "for":
        words(c.words, ctx);
        eachCommand(c.body, ctx, visit);
        break;
      case "case":
        words([c.subject], ctx);
        for (const arm of c.arms) {
          words(arm.patterns, ctx);
          eachCommand(arm.body, ctx, visit);
        }
        break;
    }
  };
  for (const ao of list) for (const p of ao.pipelines) for (const c of p.commands) one(c, ctx);
}

/** The library of scripts/e2e/lib that a `.` or `source` command sources, by its path. */
export function sourcedLibrary(c: Command): string | undefined {
  if (c.k !== "simple" || c.words.length < 2) return undefined;
  if (c.words[0].text !== "." && c.words[0].text !== "source") return undefined;
  const lib = library.exec(c.words[1].text) ?? siblingLibrary.exec(c.words[1].text);
  return lib === null ? undefined : `scripts/e2e/lib/${lib[1]}`;
}

/** A command's name, where its first word is plain text. */
export function nameOf(words: readonly Word[]): string | undefined {
  const w = words[0];
  if (w === undefined || w.subs.length > 0 || w.vars.length > 0) return undefined;
  return /^[A-Za-z0-9_./:+[\]-]+$/.test(w.text) ? w.text : undefined;
}

/** The one word bash makes of `w` where it expands nothing: its text with the quotes and
 *  backslashes bash removes taken out, as `'set'`, `\set`, `"-e"` and `-e` all give a word that bash
 *  reads alike. Undefined where bash may make it anything else: a parameter, a command or
 *  arithmetic substitution, an ANSI-C or locale quote (`$'…'`, `$"…"`), a glob, a brace, a tilde,
 *  or an escape inside double quotes the search does not read. A caller reads such a word as one
 *  it cannot know (`sweep-order-policy.ts`). */
export function literalOf(w: Word): string | undefined {
  if (w.subs.length > 0 || w.vars.length > 0) return undefined;
  let value = "";
  const text = w.text;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i] ?? "";
    if (ch === "'") {
      const end = text.indexOf("'", i + 1);
      if (end === -1) return undefined;
      value += text.slice(i + 1, end);
      i = end;
    } else if (ch === '"') {
      let j = i + 1;
      for (; j < text.length && text[j] !== '"'; j++) {
        const inner = text[j] ?? "";
        if (inner === "$" || inner === "`") return undefined;
        if (inner === "\\") {
          const next = text[j + 1] ?? "";
          // Inside double quotes a backslash quotes only these; before any other it stays.
          if ('$`"\\'.includes(next)) {
            value += next;
            j++;
            continue;
          }
          if (next === "\n") {
            j++;
            continue;
          }
        }
        value += inner;
      }
      if (j >= text.length) return undefined;
      i = j;
    } else if (ch === "\\") {
      if (i + 1 >= text.length) return undefined;
      if (text[i + 1] !== "\n") value += text[i + 1];
      i++;
    } else if ("$`*?[{}".includes(ch) || (ch === "~" && i === 0)) {
      return undefined;
    } else {
      value += ch;
    }
  }
  return value;
}

/** `text`, the script at `shown` under the root, parsed: its top-level list, every function it
 *  defines, each library of scripts/e2e/lib it sources and the options of its first line. Throws
 *  `Unparsed`, naming the line, where it cannot parse it. */
export function readScript(text: string, shown: string): Script {
  const top = new Parser(new Lexer(text, shown)).program();
  const definitions: Definition[] = [];
  const libraries: string[] = [];
  eachCommand(top, null, (c) => {
    if (c.k === "function") definitions.push({ name: c.name, body: c.body });
    const lib = sourcedLibrary(c);
    if (lib !== undefined) libraries.push(lib);
  });
  return { top, definitions, libraries, options: firstLineOptions(text) };
}
