/**
 * The removal-order policy of the order search (lib/sweep-order.ts): the walk of one driver's
 * start from its top-level commands, through each function and library it reaches, and which of
 * the refusals and marks on its paths come before its removal, as lib/sweep-order.ts's header
 * states the rule.
 */

import { Flow, type Guard, Reader } from "./sweep-order-inputs";
import {
  type AndOr,
  alone,
  type Command,
  type Definition,
  declarers,
  eachCommand,
  type List,
  literalOf,
  nameOf,
  readScript,
  type Script,
  sourcedLibrary,
  type Word,
} from "./sweep-order-shell";

/** The removal of the Postgres of dead stacks. */
export const sweep = "remove_leftover_postgres";
/** The mark of a start's own Postgres, which the line after it creates the container with. */
export const mark = "postgres_mark";

/** A removal, a mark or a refusal that a driver's start reaches, or a command on its way that the
 *  search cannot read ("unread"): one that may turn errexit on or off, or end the start, in a way
 *  the search does not know, such as `eval "$x"`, which it names instead of reading it as clean. */
export interface Call {
  readonly kind: "removal" | "mark" | "refusal" | "unread";
  /** The removal's or the mark's name, the command that ends the start: `exit`, `return` or
   *  a function that always ends it, such as `fail`, or what the search cannot read of a command. */
  readonly name: string;
  /** The file and line of that command. */
  readonly at: string;
  /** The file and line of the driver's own command through which the start reaches it. */
  readonly from: string;
  /** The functions on the way to it, the outermost first. */
  readonly via: readonly string[];
  /** For a refusal: what its check reads that is not one of the start's inputs. */
  readonly reads: readonly string[];
  /** The file and line of the first command of the branch that ends the start: the branch of
   *  the `if`, the `case`, the loop or the `&&` or `||` list the command is in. */
  readonly begins: string;
}

// ---- The start's path ---------------------------------------------------------------------------

/** A removal, a mark or a refusal on the path, with what decides whether it runs. */
interface Event {
  readonly kind: Call["kind"];
  readonly name: string;
  readonly at: string;
  readonly begins: string;
  readonly from: string | undefined;
  readonly via: readonly string[];
  readonly guards: readonly Guard[];
  /** Whether `guards` is the command's own run alone (`Walk.failure`), which no caller adds to. */
  readonly own?: true;
}
type Step =
  | { readonly k: "event"; readonly event: Event }
  | { readonly k: "choice"; readonly arms: readonly (readonly Step[])[] }
  | { readonly k: "loop"; readonly body: readonly Step[] };

/** Where the walk is: the function it is in, the checks around it in that function, the checks
 *  of each `return` it already passed there, whether a status that is not 0 ends the start,
 *  whether the walk is inside a command or process substitution, whose commands `set -e` does
 *  not end the start on, the driver's own command through which a library's top-level commands
 *  run, and, in a function, the options at each `return` the walk passed in it, the shell the
 *  function runs in (`depth`, `Walk.depth`), and the options a `local -` of that shell saved,
 *  which bash gives back when the function returns (`dash`), with whether it runs on every path
 *  (`sure`). */
interface Frame {
  readonly fn: string | null;
  readonly local: readonly Guard[];
  readonly early: Guard[];
  readonly plain: boolean;
  readonly substitution: boolean;
  readonly through: string | undefined;
  readonly returns?: Options[];
  readonly depth: number;
  readonly dash?: { saved: Options | undefined; sure: boolean };
}

/** Whether a failed command may end the start where nothing tests its status, on some path the
 *  walk came by to where it is: `set -e` (or `set -o errexit`, or `-e` on the script's first line)
 *  may be on, or a `trap` on ERR that ends the start may be set. Bash keeps them per shell: a
 *  subshell, a command or process substitution, a command of a pipeline of more than one and a
 *  list in the background each start from the options of the shell that runs them and change
 *  none of its own. Where paths join after a branch that may or may not run, or a loop that may
 *  run again, each is on where it may be on along one of them (`join`), so the search fails safe. */
interface Options {
  readonly errexit: boolean;
  readonly errTrap: boolean;
}

/** The options after paths join: each on where it is on along one of them. */
function join(first: Options, second: Options): Options {
  return { errexit: first.errexit || second.errexit, errTrap: first.errTrap || second.errTrap };
}

/** The words of the command that `words` runs, past each `builtin` and `command` before it, which
 *  run the command they name and no function of that name (`builtin`): `builtin set -e`, `command
 *  set -e`, `command -p set -e` and `command builtin set -e` each run `set -e`. `command -v` and
 *  `command -V` only describe a command, and run nothing, so they are answered as they are. */
function commandRun(words: readonly Word[]): { run: readonly Word[]; builtin: boolean } {
  let run = words;
  let builtin = false;
  for (;;) {
    const name = run[0] === undefined ? undefined : literalOf(run[0]);
    if (name !== "builtin" && name !== "command") return { run, builtin };
    let at = 1;
    for (; at < run.length; at++) {
      const word = run[at];
      const option = word === undefined ? undefined : literalOf(word);
      if (option === "--") {
        at += 1;
        break;
      }
      if (name === "builtin" || option === undefined || !/^-[pvV]+$/.test(option)) break;
      if (/[vV]/.test(option)) return { run: words, builtin: false };
    }
    run = run.slice(at);
    builtin = true;
  }
}

/** The walk of one driver's start, from its top-level commands. */
class Walk {
  /** Each function's steps, and the options after it, by its name, the frame's flags and the
   *  options it was called with. */
  private readonly memo = new Map<
    string,
    { readonly steps: readonly Step[]; readonly exit: Options }
  >();
  private readonly sourced = new Set<string>();
  /** The options as far as the walk has come (`Options`). */
  private state: Options = { errexit: false, errTrap: false };
  /** How many shells of their own (`scoped`) the walk is in, so a `local -` is read only in the
   *  shell of the function it is in. */
  private depth = 0;

  constructor(
    private readonly reader: Reader,
    private readonly scripts: ReadonlyMap<string, Script>
  ) {}

  /** The steps of a start whose top-level list is `top` and whose first line gives bash
   *  `options` (`Script.options`). */
  start(top: List, options: readonly string[]): Step[] {
    const out: Step[] = [];
    const frame = {
      fn: null,
      local: [],
      early: [],
      plain: true,
      substitution: false,
      through: undefined,
      depth: 0,
    };
    // The first line's options are literal words, which `set` reads in full.
    this.set(options);
    this.list(top, frame, out, undefined);
    return out;
  }

  /** Walks what runs in a shell of its own: the options it changes are not the walk's after it. */
  private scoped(walk: () => void): void {
    const before = this.state;
    this.depth += 1;
    walk();
    this.depth -= 1;
    this.state = before;
  }

  /** Walks what may not run: the options after it are those after it joined with those before. */
  private maybe(walk: () => void): void {
    const before = this.state;
    walk();
    this.state = join(before, this.state);
  }

  /** Walks what may run again, a loop's body, by `once`, which answers the steps of one turn:
   *  where a turn may turn on an option that was off when the loop began, a later turn begins with
   *  it on, and the turn is walked again from there, so its steps are those of such a turn. The
   *  options after it are those of every turn joined with those before. */
  private turns<T>(frame: Frame, once: () => T): T {
    const before = this.state;
    const early = frame.early.length;
    let steps = once();
    const again = join(before, this.state);
    if (again.errexit !== before.errexit || again.errTrap !== before.errTrap) {
      frame.early.length = early;
      this.state = again;
      steps = once();
    }
    this.state = join(again, this.state);
    return steps;
  }

  /** The steps of a list. `begins` is the first command of the branch the list is; undefined,
   *  each command of the list begins its own. */
  private list(list: List, frame: Frame, out: Step[], begins: string | undefined): void {
    for (const ao of list) {
      const walk = () =>
        this.andOr(ao, frame, out, begins ?? ao.pipelines[0]?.commands[0]?.at ?? "");
      if (ao.background === true) this.scoped(walk);
      else walk();
    }
  }

  private andOr(ao: AndOr, frame: Frame, out: Step[], begins: string): void {
    ao.pipelines.forEach((p, i) => {
      const before = ao.pipelines
        .slice(0, i)
        .map((q) => ({ list: [{ pipelines: [q] }], ctx: frame.fn }));
      const last = i === ao.pipelines.length - 1;
      const steps: Step[] = [];
      const run = () =>
        p.commands.forEach((c, j) => {
          const others = p.commands.filter((_, k) => k !== j);
          const local = [
            ...frame.local,
            ...before,
            ...(others.length > 0
              ? [{ list: [{ pipelines: [{ negated: false, commands: others }] }], ctx: frame.fn }]
              : []),
          ];
          const inner = { ...frame, local, plain: frame.plain && last && !p.negated };
          const one = () => this.command(c, inner, steps, i > 0 ? c.at : begins);
          // Each command of a pipeline of more than one runs in a subshell of its own.
          if (p.commands.length > 1) this.scoped(one);
          else one();
        });
      if (i === 0) {
        run();
        out.push(...steps);
      } else {
        this.maybe(run);
        out.push({ k: "choice", arms: [steps, []] });
      }
    });
  }

  private subs(words: readonly Word[], frame: Frame, out: Step[], begins: string): void {
    for (const w of words) this.substitutions(w.subs, frame, out, begins);
  }

  /** The steps of command or process substitutions, each in a shell of its own. */
  private substitutions(lists: readonly List[], frame: Frame, out: Step[], begins: string): void {
    for (const sub of lists)
      this.scoped(() => this.list(sub, { ...frame, substitution: true }, out, begins));
  }

  private command(c: Command, frame: Frame, out: Step[], begins: string): void {
    if (c.k === "function") return;
    if (c.k === "arith") {
      // `(( … ))` fails where its expression is 0, as `set -e` reads a status; the command
      // substitutions in it run first.
      this.substitutions(c.subs, frame, out, begins);
      this.failure(
        c,
        `((${c.text.trim()}))`,
        [{ list: alone(c), ctx: frame.fn }],
        frame,
        out,
        begins
      );
      return;
    }
    // A redirection's process substitution runs before the command.
    this.subs(
      c.redirects.map((r) => r.target),
      frame,
      out,
      begins
    );
    switch (c.k) {
      case "simple":
        this.simple(c, frame, out, begins);
        return;
      case "group":
        if (c.subshell) this.scoped(() => this.list(c.body, frame, out, begins));
        else this.list(c.body, frame, out, begins);
        return;
      case "if": {
        // The options after each branch that runs, and after the last condition where no branch
        // runs.
        const exits: Options[] = [];
        const arm = (index: number, prior: readonly Guard[]): Step[] => {
          const { cond, body } = c.arms[index];
          const steps: Step[] = [];
          const taken: Step[] = [];
          const decided = cond === null ? prior : [...prior, { list: cond, ctx: frame.fn }];
          if (cond !== null)
            this.list(
              cond,
              { ...frame, local: [...frame.local, ...prior], plain: false },
              steps,
              undefined
            );
          const tested = this.state;
          this.list(
            body,
            { ...frame, local: [...frame.local, ...decided] },
            taken,
            body[0]?.pipelines[0]?.commands[0]?.at
          );
          exits.push(this.state);
          this.state = tested;
          if (cond === null) return [...steps, ...taken];
          let rest: Step[] = [];
          if (index + 1 < c.arms.length) rest = arm(index + 1, decided);
          else exits.push(tested);
          steps.push({ k: "choice", arms: [taken, rest] });
          return steps;
        };
        out.push(...arm(0, []));
        this.state = exits.reduce(join);
        return;
      }
      case "loop": {
        const local = [...frame.local, { list: c.cond, ctx: frame.fn }];
        const { cond, body } = this.turns(frame, () => {
          const cond: Step[] = [];
          const body: Step[] = [];
          this.list(c.cond, { ...frame, plain: false }, cond, undefined);
          this.list(c.body, { ...frame, local }, body, c.body[0]?.pipelines[0]?.commands[0]?.at);
          return { cond, body };
        });
        out.push(...cond, { k: "loop", body });
        return;
      }
      case "for": {
        this.subs(c.words, frame, out, begins);
        const local = [...frame.local, { words: c.words, ctx: frame.fn }];
        const body = this.turns(frame, () => {
          const body: Step[] = [];
          this.list(c.body, { ...frame, local }, body, c.body[0]?.pipelines[0]?.commands[0]?.at);
          return body;
        });
        out.push({ k: "loop", body });
        return;
      }
      case "case": {
        this.subs([c.subject], frame, out, begins);
        const entry = this.state;
        const exits: Options[] = [];
        const arms = c.arms.map((arm) => {
          const steps: Step[] = [];
          const local = [...frame.local, { words: [c.subject, ...arm.patterns], ctx: frame.fn }];
          this.state = entry;
          this.list(
            arm.body,
            { ...frame, local },
            steps,
            arm.body[0]?.pipelines[0]?.commands[0]?.at
          );
          exits.push(this.state);
          return steps;
        });
        if (!c.arms.some((arm) => arm.patterns.some((p) => p.text === "*"))) {
          arms.push([]);
          exits.push(entry);
        }
        this.state = exits.reduce(join);
        // A case on an input one of whose arms reaches the removal is the script's choice of
        // command: the arms that reach neither it nor a mark are other commands of the script.
        const starting = arms.filter((arm) => reaches(arm, "removal") || reaches(arm, "mark"));
        const input = this.reader.reads([{ words: [c.subject], ctx: frame.fn }]).length === 0;
        const removing = arms.some((arm) => reaches(arm, "removal"));
        out.push({ k: "choice", arms: input && removing ? starting : arms });
        return;
      }
    }
  }

  private event(kind: Call["kind"], name: string, at: string, frame: Frame, begins: string): Step {
    const from = frame.fn === null ? (frame.through ?? at) : undefined;
    const guards = [...frame.local, ...frame.early];
    return { k: "event", event: { kind, name, at, begins, from, via: [], guards } };
  }

  /** A command whose failure ends the start where `set -e` is on or a `trap` on ERR ends it, and
   *  nothing tests its status. Its own run is its whole check (`checks`): whatever decides that it
   *  runs, whether it fails is decided by what it reads, so it counts unless that is only the
   *  start's inputs, as `[ -n "$x" ]` and `true` read. */
  private failure(
    c: Command,
    shown: string,
    checks: readonly Guard[],
    frame: Frame,
    out: Step[],
    begins: string
  ): void {
    if (!(this.state.errexit || this.state.errTrap) || !frame.plain || frame.substitution) return;
    const from = frame.fn === null ? (frame.through ?? c.at) : undefined;
    const name = `${shown} under set -e`;
    out.push({
      k: "event",
      event: { kind: "refusal", name, at: c.at, begins, from, via: [], guards: checks, own: true },
    });
  }

  /** A command on the start's path that the search cannot read (`Call`'s "unread"): one that may
   *  turn errexit on or off, set a trap on ERR, or end the start, in a way the search does not
   *  know. It counts wherever it runs before the removal, whatever decides that it runs. In a
   *  command substitution it changes nothing of the start's, whose options there are its own. */
  private unread(why: string, c: Command, frame: Frame, out: Step[], begins: string): void {
    if (frame.substitution) return;
    out.push(this.event("unread", why, c.at, frame, begins));
  }

  /** What `set` with the words `args` does to errexit, as bash reads them (`set -e`, `set +e`,
   *  `set -o errexit`, `set -eo pipefail`): each word up to `--`, `-` or the first that is no
   *  option, which begins the positional parameters, and the word after each `o` of an option, which
   *  names an option. A word is its value (`literalOf`), or undefined where the search cannot read
   *  it. Answers false, and changes nothing, where a word it must read is undefined. */
  private set(args: readonly (string | undefined)[]): boolean {
    let errexit = this.state.errexit;
    for (let i = 0; i < args.length; i++) {
      const arg = args[i];
      if (arg === undefined) return false;
      if (arg === "--" || arg === "-" || !/^[-+][A-Za-z]+$/.test(arg)) break;
      const on = arg.startsWith("-");
      if (arg.includes("e")) errexit = on;
      if (arg.includes("o") && i + 1 < args.length) {
        i += 1;
        const option = args[i];
        if (option === undefined) return false;
        if (option === "errexit") errexit = on;
      }
    }
    this.state = { ...this.state, errexit };
    return true;
  }

  /** What `shopt` with the words `args` does, as bash reads them: `-s` or `-u` with `-o` sets or
   *  unsets the options of `set -o` it names, `errexit` among them (`shopt -s -o errexit`, `shopt
   *  -so errexit`, `shopt -os errexit`); without `-s` or `-u` it only prints or tests. Answers false
   *  where a word it must read is undefined (`literalOf`), and where it turns on `expand_aliases`,
   *  after which an alias may run any command the search reads by its name. */
  private shopt(args: readonly (string | undefined)[]): boolean {
    let on: boolean | undefined;
    let setOptions = false;
    let i = 0;
    for (; i < args.length; i++) {
      const arg = args[i];
      if (arg === undefined) return false;
      if (arg === "--") {
        i += 1;
        break;
      }
      if (!/^-[opqsu]+$/.test(arg)) break;
      if (arg.includes("s")) on = true;
      if (arg.includes("u")) on = false;
      if (arg.includes("o")) setOptions = true;
    }
    const names = args.slice(i);
    if (names.includes(undefined)) return false;
    if (on === undefined) return true;
    if (!setOptions) return !(on && names.includes("expand_aliases"));
    if (names.includes("errexit")) this.state = { ...this.state, errexit: on };
    return true;
  }

  /** What `trap` with the words `args` does to whether a failure ends the start: `trap ACTION ...
   *  ERR` (bash reads `err` in any case) with an action that exits, or calls a function that always
   *  does, ends the start on each failure as `set -e` does; `trap - ERR`, `trap '' ERR` and `trap
   *  ERR` do not. Answers false where the action, or a signal it sets, is undefined (`literalOf`):
   *  a signal the search cannot read may be ERR. */
  private trap(args: readonly (string | undefined)[]): boolean {
    const rest = args[0] === "--" ? args.slice(1) : args;
    if (rest.length === 0 || /^-[lpP]$/.test(rest[0] ?? "")) return true;
    const [action, signals] = rest.length === 1 ? ["-", rest] : [rest[0], rest.slice(1)];
    if (signals.includes(undefined)) return false;
    if (!signals.some((signal) => /^err$/i.test(signal ?? ""))) return true;
    if (action === undefined) return false;
    const errTrap =
      /(^|[\s;&|])exit([\s;&|]|$)/.test(action) ||
      action
        .split(/[\s;&|]+/)
        .some((word) => this.reader.functions.has(word) && this.reader.reporter(word));
    this.state = { ...this.state, errTrap };
    return true;
  }

  /** `eval WORDS`: bash joins the words with spaces and runs them as commands of this shell, so
   *  where each word is literal (`literalOf`) the search reads those commands in place, as bash
   *  parses them. It reads in place only commands that give no variable a value and define no
   *  function, since what a check reads is judged from the scripts' own text
   *  (`lib/sweep-order-inputs.ts`); every other eval is unread (`unread`). */
  private evaluate(
    c: Command,
    words: readonly Word[],
    frame: Frame,
    out: Step[],
    begins: string
  ): void {
    const shown = `eval ${words.map((w) => w.text).join(" ")}`;
    const args = words.map(literalOf);
    if (args.includes(undefined)) {
      this.unread(`${shown}, whose words the search cannot read`, c, frame, out, begins);
      return;
    }
    let script: Script;
    try {
      script = readScript(args.join(" "), c.at);
    } catch (error) {
      const why = error instanceof Error ? error.message : String(error);
      this.unread(`${shown}, which the search cannot parse (${why})`, c, frame, out, begins);
      return;
    }
    let gives = script.definitions.length > 0 || script.libraries.length > 0;
    eachCommand(script.top, null, (inner) => {
      if (inner.k === "for" || inner.k === "arith" || inner.k === "function") gives = true;
      if (inner.k !== "simple") return;
      const name = nameOf(inner.words);
      const assigning = ["read", "mapfile", "readarray", "printf", "getopts", "let", "export"];
      if (
        inner.assigns.length > 0 ||
        (name !== undefined && (declarers[name] === true || assigning.includes(name)))
      )
        gives = true;
      if (inner.words.some((w) => w.subs.length > 0)) gives = true;
    });
    if (gives) {
      this.unread(
        `${shown}, which gives a variable a value or defines a function, which the search does not follow`,
        c,
        frame,
        out,
        begins
      );
      return;
    }
    this.list(script.top, frame, out, begins);
  }

  private simple(c: Command & { k: "simple" }, frame: Frame, out: Step[], begins: string): void {
    const words = [...c.assigns.flatMap((a) => a.values), ...c.words];
    this.subs(words, frame, out, begins);
    // `builtin NAME` and `command NAME` run the command NAME and no function of that name, so the
    // search reads the command they run; `command -v` and `command -V` only describe it.
    const { run, builtin } = commandRun(c.words);
    const first = run[0];
    const name = nameOf(run) ?? (first === undefined ? undefined : literalOf(first));
    const self = [{ list: alone(c), ctx: frame.fn }];
    if (name === undefined) {
      if (first !== undefined && !first.text.includes("/")) {
        // A name bash knows only once it expands it may be `set`, `shopt`, `eval` or `source`; a
        // path, which holds a `/`, runs a program, which changes no option of this shell.
        this.unread(
          `${c.words.map((w) => w.text).join(" ")}, a command named by an expansion, which may be a builtin that turns errexit on or off`,
          c,
          frame,
          out,
          begins
        );
        return;
      }
      if (c.words.length > 0) {
        this.failure(c, c.words[0]?.text ?? "", self, frame, out, begins);
        return;
      }
      // Assignments alone have the status of their last command substitution, and none fails
      // where they hold none.
      const subs = words.flatMap((w) => w.subs.map((list) => ({ list, ctx: frame.fn })));
      if (subs.length > 0) {
        this.failure(c, `${c.assigns[0]?.name ?? ""}=$(…)`, subs, frame, out, begins);
      }
      return;
    }
    if (name === sweep || name === mark) {
      out.push(this.event(name === sweep ? "removal" : "mark", name, c.at, frame, begins));
      return;
    }
    const shown = run.map((w) => w.text).join(" ");
    if (name === "." || name === "source") {
      const lib = sourcedLibrary({ ...c, words: [...run] });
      const script = lib === undefined ? undefined : this.scripts.get(lib);
      if (script === undefined) {
        // A file the search does not read may turn errexit on or off, or end the start.
        this.unread(`${shown}, a file the search does not read`, c, frame, out, begins);
        return;
      }
      if (lib === undefined || this.sourced.has(lib)) return;
      // A library is read each time it is sourced, but not inside its own reading.
      this.sourced.add(lib);
      const through = frame.fn === null ? (frame.through ?? c.at) : frame.through;
      this.list(script.top, { ...frame, through }, out, undefined);
      this.sourced.delete(lib);
      return;
    }
    const status = run[1]?.text;
    if (name === "exit") {
      if (status !== "0") out.push(this.event("refusal", name, c.at, frame, begins));
      else frame.early.push(...frame.local);
      return;
    }
    if (name === "return" && frame.fn !== null) {
      if (status !== "0" && frame.plain) out.push(this.event("refusal", name, c.at, frame, begins));
      frame.early.push(...frame.local);
      frame.returns?.push(this.state);
      return;
    }
    const definitions = builtin ? undefined : this.reader.functions.get(name);
    if (definitions === undefined) {
      const args = run.slice(1).map(literalOf);
      const read =
        name === "set"
          ? this.set
          : name === "shopt"
            ? this.shopt
            : name === "trap"
              ? this.trap
              : undefined;
      if (read !== undefined) {
        if (!read.call(this, args)) {
          const why = args.includes(undefined)
            ? "whose words the search cannot read"
            : "which turns on aliases, which the search does not read";
          this.unread(`${shown}, ${why}`, c, frame, out, begins);
        }
        return;
      }
      if (name === "eval") {
        this.evaluate(c, run.slice(1), frame, out, begins);
        return;
      }
      if (name === "enable") {
        // `enable -n set` turns the builtin off, and a script's `set` then runs another program.
        this.unread(
          `${shown}, which may turn off a builtin the search reads`,
          c,
          frame,
          out,
          begins
        );
        return;
      }
      // `local -` makes the options of the function it runs in its own: bash gives them back when
      // the function returns. The first in the function's own shell counts, on every path where
      // no check and no `return` comes before it.
      if (
        name === "local" &&
        args.includes("-") &&
        frame.dash !== undefined &&
        frame.dash.saved === undefined &&
        frame.depth === this.depth
      ) {
        frame.dash.saved = this.state;
        frame.dash.sure = frame.local.length === 0 && frame.early.length === 0;
      }
      // `local`, `export` and the like give their own status, not that of what they assign.
      if (declarers[name] !== true) this.failure(c, name, self, frame, out, begins);
      return;
    }
    if (this.reader.reporter(name)) {
      out.push(this.event("refusal", name, c.at, frame, begins));
      return;
    }
    const key = `${name} ${frame.plain} ${frame.substitution} ${this.state.errexit} ${this.state.errTrap}`;
    let inner = this.memo.get(key);
    if (inner === undefined) {
      // A call of the function inside its own body ends with the options it began with.
      this.memo.set(key, { steps: [], exit: this.state });
      const entry = this.state;
      const exits: Options[] = [];
      const arms = definitions.map((d) => {
        this.state = entry;
        const steps: Step[] = [];
        const returns: Options[] = [];
        const own: Frame = {
          fn: name,
          local: [],
          early: [],
          plain: frame.plain,
          substitution: frame.substitution,
          through: undefined,
          returns,
          depth: this.depth,
          dash: { saved: undefined, sure: false },
        };
        // A body in braces runs in the caller's shell, and one in parentheses in a subshell of
        // its own, whose options are not the caller's after it.
        const body = d.body;
        if (body.k !== "group") this.list(alone(body), own, steps, undefined);
        else if (body.subshell) this.scoped(() => this.list(body.body, own, steps, undefined));
        else this.list(body.body, own, steps, undefined);
        const end = returns.reduce(join, this.state);
        const saved = own.dash?.saved;
        // Where a `local -` ran, errexit is what it was then; where it may have run, either.
        exits.push(
          saved === undefined
            ? end
            : {
                ...end,
                errexit: own.dash?.sure === true ? saved.errexit : end.errexit || saved.errexit,
              }
        );
        return steps;
      });
      inner = {
        steps: arms.length === 1 ? arms[0] : [{ k: "choice", arms }],
        exit: exits.reduce(join),
      };
      this.memo.set(key, inner);
    }
    this.state = inner.exit;
    const from = frame.fn === null ? (frame.through ?? c.at) : undefined;
    const prefix = [...frame.local, ...frame.early];
    const place = (steps: readonly Step[]): Step[] =>
      steps.map((s) => {
        if (s.k === "choice") return { k: "choice", arms: s.arms.map(place) };
        if (s.k === "loop") return { k: "loop", body: place(s.body) };
        const e = s.event;
        const via = [name, ...e.via];
        return {
          k: "event",
          event: {
            ...e,
            from: e.from ?? from,
            via,
            guards: e.own === true ? e.guards : [...prefix, ...e.guards],
          },
        };
      });
    out.push(...place(inner.steps));
  }
}

/** Whether steps reach a removal, or a mark, anywhere. */
function reaches(steps: readonly Step[], kind: "removal" | "mark"): boolean {
  return steps.some((s) =>
    s.k === "event"
      ? s.event.kind === kind
      : s.k === "choice"
        ? s.arms.some((arm) => reaches(arm, kind))
        : reaches(s.body, kind)
  );
}

/** The order of one driver's start, `own`, with the scripts it and its libraries are and the
 *  functions they define: every removal, mark and refusal it reaches, in the order of its text,
 *  and each refusal and mark before its removal on one of its paths, with where its first removal
 *  is; undefined where it reaches no removal. */
export function orderOf(
  own: Script,
  scripts: ReadonlyMap<string, Script>,
  functions: ReadonlyMap<string, readonly Definition[]>
):
  | { readonly calls: readonly Call[]; readonly before: readonly Call[]; readonly sweep: string }
  | undefined {
  const reader = new Reader(functions, new Flow([...scripts.values()], functions));
  const steps = new Walk(reader, scripts).start(own.top, own.options);
  if (!reaches(steps, "removal")) return undefined;
  const reads = new Map<Event, readonly string[]>();
  const readsOf = (e: Event): readonly string[] => {
    let known = reads.get(e);
    if (known === undefined) {
      known =
        e.kind !== "refusal"
          ? []
          : e.guards.length === 0
            ? ["nothing: no check decides it, so it ends every start that reaches it"]
            : reader.reads(e.guards);
      reads.set(e, known);
    }
    return known;
  };
  // A refusal whose check reads only inputs is no call of the class.
  const counts = (e: Event) => e.kind !== "refusal" || readsOf(e).length > 0;
  const call = (e: Event): Call => ({
    kind: e.kind,
    name: e.name,
    at: e.at,
    from: e.from ?? e.at,
    via: e.via,
    reads: readsOf(e),
    begins: e.begins,
  });
  const calls: Call[] = [];
  const flat = (s: readonly Step[]): void => {
    for (const step of s) {
      if (step.k === "event") {
        if (counts(step.event)) calls.push(call(step.event));
      } else if (step.k === "choice") for (const arm of step.arms) flat(arm);
      else flat(step.body);
    }
  };
  flat(steps);
  // Each path: a removal counts after a choice only where every arm of it removes.
  const before: Event[] = [];
  const judge = (s: readonly Step[], reached: boolean): boolean => {
    let now = reached;
    for (const step of s) {
      if (step.k === "event") {
        if (step.event.kind === "removal") now = true;
        else if (!now && counts(step.event)) before.push(step.event);
      } else if (step.k === "choice") {
        let all = true;
        for (const arm of step.arms) all = judge(arm, now) && all;
        now = all;
      } else judge(step.body, now);
    }
    return now;
  };
  judge(steps, false);
  const first = calls.find((c) => c.kind === "removal");
  if (first === undefined) return undefined;
  const seen = new Set<string>();
  const listed = before.map(call).filter((c) => {
    const key = `${c.name} ${c.at} ${c.from}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  return { calls, before: listed, sweep: first.at };
}
