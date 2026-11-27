#!/usr/bin/env bun
/**
 * The `jj` and the `git` a case run's role finds first on its PATH: case-world.ts prepare writes
 * each into <run>/bin, which rig.sh's base_env puts first on the pane's PATH, as the shell script
 * recorderScript makes, in front of the real program it found on PATH. The script hands a call to
 * the real program at once, running nothing else, unless one of its arguments is `diff`, `show` or
 * `interdiff`; it hands such a call to this recorder, which runs the real program with its argv,
 * its working directory, its environment, stdin, stdout and stderr as they are, and exits with the
 * program's status, or ends by the signal that ended the program (`exit` then records 128 and the
 * signal's number). Where the call's subcommand reads a diff (`jj diff`, `jj show`,
 * `jj interdiff`, `git diff`, `git show`: diffCall), it appends one line to <run>/calls.jsonl once
 * the program has exited:
 *   {at, done, as: "jj" | "git", argv, exit, command, source, paths}
 * `at` and `done` are when the program started and ended, and `command` is the program and its
 * subcommand. Where the program exited 0, `source` is the checkout root the call read, as the
 * program names it under the call's own global options (`jj root`, `git rev-parse
 * --show-toplevel`), written as git-world.ts namedSource writes a staged Job's source
 * (`$LEGION_WORKSPACE`, `$TMPDIR/stage-a`), and `paths` is each path argument the call named,
 * relative to that root; both are null where it exited otherwise or the program names no root.
 * outcome.ts reads a `diff-read` action from the line.
 *
 *   bun diff-recorder.ts <run dir> <the real program> <jj | git> [argv...]
 */
import { appendFileSync, existsSync } from "node:fs";
import { constants } from "node:os";
import path from "node:path";
import { namedSource, realPath } from "./git-world";

export type Program = "jj" | "git";

/** The subcommands of each program that read a diff. */
const DIFF_READS: Record<Program, readonly string[]> = {
  jj: ["diff", "show", "interdiff"],
  git: ["diff", "show"],
};

/** The global options of each program whose value is the next argument where the option does not
 * join it (`-C dir`, not `--git-dir=dir`): git's stand before its subcommand, and jj's anywhere. */
const GLOBAL_VALUES: Record<Program, readonly string[]> = {
  git: [
    "-C",
    "-c",
    "--git-dir",
    "--work-tree",
    "--namespace",
    "--config-env",
    "--attr-source",
    "--super-prefix",
  ],
  jj: [
    "-R",
    "--repository",
    "--at-operation",
    "--at-op",
    "--color",
    "--config",
    "--config-toml",
    "--config-file",
  ],
};

/** The options of a diff read whose value is the next argument where the option does not join it,
 * so that the value is no path. */
const OPTION_VALUES: Record<Program, readonly string[]> = {
  git: [
    "-S",
    "-G",
    "-O",
    "-n",
    "--output",
    "--max-count",
    "--skip",
    "--since",
    "--after",
    "--until",
    "--before",
    "--author",
    "--committer",
    "--grep",
  ],
  jj: [
    "-r",
    "--revisions",
    "--revision",
    "-f",
    "--from",
    "-t",
    "--to",
    "--tool",
    "--context",
    "-T",
  ],
};

/** The kinds of a jj fileset pattern (`root:docs`), each one the root's or the working directory's. */
const ROOT_PATTERNS = ["root", "root-file", "root-glob", "root-glob-i"];
const CWD_PATTERNS = ["cwd", "cwd-file", "file", "cwd-glob", "glob", "cwd-glob-i", "glob-i"];

/** One path a diff read named: relative to the checkout root (`root`), or absolute. */
export interface NamedPath {
  readonly root: boolean;
  readonly path: string;
}

/** What a call that reads a diff names: the program and its subcommand, the global options that
 * choose the checkout it reads (for the program's own root query), and each path it named. */
export interface DiffCall {
  readonly command: string;
  readonly checkout: readonly string[];
  readonly paths: readonly NamedPath[];
}

/** The diff read `argv` of `program` makes in the working directory `cwd`, or undefined where its
 * subcommand reads no diff. git's global options stand before its subcommand, and `-C` moves the
 * directory its path arguments are read from. jj's may stand anywhere, and its path arguments are
 * filesets of the working directory, each a path or a pattern of a kind (`root:docs`, `glob:*.ts`).
 * A positional argument of `git diff` or `git show` before `--` is a path where it names one that
 * exists, as git takes one that is no revision (and every one after it), and each after `--` is a
 * path, `:/docs` from the root. `jj show`'s positional argument is a revision, never a path. A
 * `git show` whose every object is a file or a tree of a revision (`HEAD:README.md`) prints that
 * file and no diff, so it reads none. */
export function diffCall(
  program: Program,
  argv: readonly string[],
  cwd: string
): DiffCall | undefined {
  const checkout: string[] = [];
  const rest: string[] = [];
  let subcommand: string | undefined;
  let dir = cwd;
  for (let at = 0; at < argv.length; at += 1) {
    const arg = argv[at] ?? "";
    const global = program === "git" ? subcommand === undefined : !rest.includes("--");
    if (global && GLOBAL_VALUES[program].includes(arg)) {
      const value = argv[at + 1];
      if (value === undefined) return undefined;
      at += 1;
      if (program === "git") checkout.push(arg, value);
      else if (arg === "-R" || arg === "--repository") checkout.push("-R", value);
      if (program === "git" && arg === "-C") dir = path.resolve(dir, value);
      continue;
    }
    if (global && program === "jj" && /^(-R.|--repository=)/.test(arg)) {
      checkout.push(arg);
      continue;
    }
    if (subcommand === undefined) {
      if (arg.startsWith("-")) {
        if (program === "git") checkout.push(arg);
        continue;
      }
      subcommand = arg;
      continue;
    }
    rest.push(arg);
  }
  if (subcommand === undefined || !DIFF_READS[program].includes(subcommand)) return undefined;
  const paths: NamedPath[] = [];
  let dashes = false;
  let named = false;
  let objects = 0;
  let contents = 0;
  for (let at = 0; at < rest.length; at += 1) {
    const arg = rest[at] ?? "";
    if (!dashes && arg === "--") {
      dashes = true;
      continue;
    }
    if (!dashes && arg.startsWith("-") && arg !== "-") {
      if (OPTION_VALUES[program].includes(arg)) at += 1;
      continue;
    }
    if (program === "git") {
      if (!dashes && !named && !existsSync(path.resolve(dir, arg))) {
        objects += 1;
        if (/^[^:]+:/.test(arg)) contents += 1;
        continue;
      }
      named = true;
      paths.push(
        arg.startsWith(":/")
          ? { root: true, path: arg.slice(2) }
          : { root: false, path: path.resolve(dir, arg) }
      );
      continue;
    }
    if (subcommand === "show") continue;
    const [, kind, pattern = ""] = /^([a-z-]+):(.*)$/.exec(arg) ?? [];
    if (kind !== undefined && ROOT_PATTERNS.includes(kind))
      paths.push({ root: true, path: pattern });
    else if (kind !== undefined && CWD_PATTERNS.includes(kind))
      paths.push({ root: false, path: path.resolve(cwd, pattern) });
    else paths.push({ root: false, path: path.resolve(cwd, arg) });
  }
  if (program === "git" && subcommand === "show" && objects > 0 && contents === objects)
    return undefined;
  return { command: `${program} ${subcommand}`, checkout, paths };
}

/** The shell script that stands in front of `real`, the real `program`, in the run `runDir`: a
 * call that names none of `diff`, `show` and `interdiff` goes to the real program at once, and any
 * other goes to this recorder (`bun`, the bun that runs it). */
export function recorderScript(runDir: string, program: Program, real: string, bun: string) {
  const words = [bun, import.meta.path, runDir, real, program].map(
    (word) => `'${word.replaceAll("'", `'\\''`)}'`
  );
  return [
    "#!/bin/sh",
    "for arg do",
    '  case "$arg" in',
    `    diff | show | interdiff) exec ${words.join(" ")} "$@" ;;`,
    "  esac",
    "done",
    `exec ${words[3]} "$@"`,
    "",
  ].join("\n");
}

/** The checkout root `call` read, as the real program `real` names it in this working directory,
 * or undefined where it names none. */
function rootOf(program: Program, real: string, call: DiffCall): string | undefined {
  const query =
    program === "git"
      ? [real, ...call.checkout, "rev-parse", "--show-toplevel"]
      : [real, ...call.checkout, "--ignore-working-copy", "root"];
  const answer = Bun.spawnSync(query, { stdin: "ignore", stdout: "pipe", stderr: "ignore" });
  const root = answer.stdout.toString().trim();
  return answer.exitCode === 0 && root !== "" ? root : undefined;
}

if (import.meta.main) {
  const [runDir, real, program, ...argv] = Bun.argv.slice(2);
  if (runDir === undefined || real === undefined || (program !== "jj" && program !== "git")) {
    console.error("usage: bun diff-recorder.ts <run dir> <the real program> <jj | git> [argv...]");
    process.exit(2);
  }
  const call = diffCall(program, argv, process.cwd());
  const at = new Date().toISOString();
  const child = Bun.spawn([real, ...argv], {
    stdin: "inherit",
    stdout: "inherit",
    stderr: "inherit",
  });
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"] as const)
    process.on(signal, () => child.kill(signal));
  await child.exited;
  const exit =
    child.exitCode ??
    128 + (child.signalCode === null ? 0 : (constants.signals[child.signalCode] ?? 0));
  if (call !== undefined) {
    const done = new Date().toISOString();
    const root = exit === 0 ? rootOf(program, real, call) : undefined;
    let source: string | null = null;
    let paths: string[] | null = null;
    if (root !== undefined) {
      const top = realPath(root);
      source = namedSource(top, runDir);
      paths = call.paths.map(
        (named) =>
          (named.root ? path.normalize(named.path) : path.relative(top, realPath(named.path))) ||
          "."
      );
    }
    appendFileSync(
      path.join(runDir, "calls.jsonl"),
      `${JSON.stringify({ at, done, as: program, argv, exit, command: call.command, source, paths })}\n`
    );
  }
  // A program a signal ended ends this recorder by the same signal, so that the caller sees what
  // the real program's caller sees.
  if (child.signalCode !== null) {
    process.removeAllListeners(child.signalCode);
    process.kill(process.pid, child.signalCode);
    await Bun.sleep(1000);
  }
  process.exit(exit);
}
