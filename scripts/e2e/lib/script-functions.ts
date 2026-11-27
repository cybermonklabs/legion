import { existsSync, readFileSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";

/**
 * Reads the shell script at `path` once and returns `fn`, which gives the definition of the
 * function `name` exactly as the script writes it at the start of a line: `name() { … }` when it
 * is one line, else from `name() {` to the first line that is `}` alone. `fn` throws, naming the
 * script by its path under scripts/e2e, when the script defines no such function.
 */
export function scriptFunctions(path: string): (name: string) => string {
  const script = readFileSync(path, "utf8");
  const shown = relative(dirname(import.meta.dir), path);
  return (name) => {
    const found = new RegExp(`^${name}\\(\\) \\{(?:.*\\}$|[\\s\\S]*?\\n\\}$)`, "m").exec(script);
    if (found === null) throw new Error(`${shown} defines no ${name}()`);
    return found[0];
  };
}

/**
 * Reads the shell script at `path` once and returns `assignment`, which gives the assignment of
 * the variable `name` exactly as the script writes it at the start of a line: `name='…'` through
 * the first line that ends in a quote when the value is single-quoted, `name=(` through the first
 * line that is `)` alone when the value is an array written over several lines, else `name=…` to
 * the end of its line, an array written on one line among them. `assignment` throws, naming the
 * script by its path under scripts/e2e, when the script assigns no such variable.
 */
export function scriptAssignments(path: string): (name: string) => string {
  const script = readFileSync(path, "utf8");
  const shown = relative(dirname(import.meta.dir), path);
  return (name) => {
    const found = new RegExp(`^${name}=(?:'[\\s\\S]*?'|\\(\\n[\\s\\S]*?\\n\\)|.*)$`, "m").exec(
      script
    );
    if (found === null) throw new Error(`${shown} assigns no ${name}`);
    return found[0];
  };
}

// Every function a script defines at the start of a line, one-line definitions included.
const definition = /^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm;
// A library a script sources: `.` or `source`, then a word, at the start of a line. The word may
// be quoted or not and the line may go on (`|| exit 1`, `; f`, a comment), so one matcher reads
// every such line and both readers below use it: a line only one of them matched would be walked
// and not refused, or neither, which is a file read by nobody and named by nobody.
//
// The word is a shell word, and a path holding `..` names a different file when it is read by its
// last segments, so it is resolved whole. A word this file cannot resolve without running the
// shell is no library it can read: `unreadableSources` names those lines, and a scan that walks
// what a script sources fails on them rather than passing over a file it never read.
// A quoted word runs to the last quote of its kind on the line, since the `$(dirname …)` forms
// hold quotes of their own and shell nesting is not a thing a pattern can read; an unquoted word
// runs to the first space, as the shell reads it. A line whose closing quote this misreads ends
// as a word that resolves to nothing, which is refused rather than passed over.
const sourceLine = /^[ \t]*(?:\.|source)[ \t]+(\S.*)$/;

/** The word each `.` or `source` line of `text` names, with the line it is on (counting from 1)
 *  and the line itself, in order. The word is unquoted where the line quoted it. */
function sourceWords(text: string): { word: string; at: number; line: string }[] {
  const found: { word: string; at: number; line: string }[] = [];
  const lines = text.split("\n");
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index] ?? "";
    const match = sourceLine.exec(line);
    if (match === null) continue;
    const rest = match[1] ?? "";
    const quote = rest.startsWith('"') ? '"' : rest.startsWith("'") ? "'" : "";
    const word =
      quote === ""
        ? (/^\S+/.exec(rest)?.[0] ?? "")
        : rest.slice(1, rest.lastIndexOf(quote) > 0 ? rest.lastIndexOf(quote) : undefined);
    found.push({ word, at: index + 1, line });
  }
  return found;
}

/** The checkout, from this file in scripts/e2e/lib. */
const checkout = join(import.meta.dir, "..", "..", "..");

/** A path a script writes for its own directory: `$(dirname "$0")` and the `BASH_SOURCE` form
 *  beside it, which every script here uses for that and nothing else. */
const ownDirectory =
  /^(?:\$\(dirname "\$0"\)|\$\(cd "\$\(dirname "\$\{BASH_SOURCE\[0\]\}"\)" && pwd -P\))\/(.+)$/;

/** Where `written` reaches from the script at `from`, or `undefined` when reading it would mean
 *  running the shell: a word that is a variable whole (`. "$lib"`), a glob, or any other command
 *  substitution. Such a line is named by `unreadableSources` instead. */
function resolvedSource(from: string, written: string): string | undefined {
  const own = ownDirectory.exec(written);
  if (own !== null) return resolve(dirname(from), own[1] ?? "");
  if (written.includes("$(") || written.includes("`") || written.includes("*")) return undefined;
  const against = /^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?\/(.+)$/.exec(written);
  if (against === null) {
    if (written.includes("$")) return undefined;
    return isAbsolute(written) ? written : resolve(dirname(from), written);
  }
  const [, name, rest = ""] = against;
  if (rest.includes("$")) return undefined;
  if (name === "root") return resolve(checkout, rest);
  const beside = resolve(dirname(from), rest);
  return existsSync(beside) ? beside : resolve(checkout, rest);
}

/** Every line of the script at `path` that sources a library this file cannot resolve, as
 *  `<path under the checkout>:<line>: <the line>`. A scan that walks what a script sources must
 *  fail on these: the file behind such a line is never read, so anything in it is unseen. */
export function unreadableSources(path: string): string[] {
  const found: string[] = [];
  for (const { word, at, line } of sourceWords(readFileSync(path, "utf8"))) {
    // A path that resolves but names no file of the tree is one the script makes as it runs, as
    // nats-callout-sessions.sh does with its drivers; the walk skips it and says so here only
    // where it cannot resolve the path at all, which is the shape that hides a library.
    if (resolvedSource(path, word) !== undefined) continue;
    found.push(`${relative(checkout, path)}:${at}: ${line.trim()}`);
  }
  return found;
}

/**
 * Every script among `paths`, every library one of them sources (`. "<path>"`), wherever in the
 * checkout that path reaches, and the libraries those source in turn, each path once. A harness
 * reads the functions a script reaches through it, and a test the libraries a script runs with.
 */
export function sourcedLibraries(...paths: string[]): string[] {
  const read = new Set<string>();
  const pending = [...paths];
  for (let path = pending.pop(); path !== undefined; path = pending.pop()) {
    if (read.has(path) || !existsSync(path)) continue;
    read.add(path);
    for (const { word } of sourceWords(readFileSync(path, "utf8"))) {
      const reaches = resolvedSource(path, word);
      // A line this reader cannot resolve is named by unreadableSources, which reads the same
      // lines this walk does, so no line falls between the two; the walk goes on rather than
      // ending on it, and one whose file this tree does not hold is skipped at the top.
      if (reaches !== undefined) pending.push(reaches);
    }
  }
  return [...read];
}

/**
 * The shell a harness puts above the functions it extracts from `paths`, so that a helper one of
 * them calls that the harness neither extracts nor stubs is named rather than left as a bare
 * `exit 127` the caller must trace: bash's command_not_found_handle prints
 * `harness: no definition for <name>` for a name one of those scripts defines, and leaves every
 * other missing command (a binary the machine lacks) as it was, still exit 127. The line goes to
 * the harness's own stderr, saved in a file descriptor when the guard is set up, so a call the
 * extracted function silences (`helper >/dev/null 2>&1`) is named too. Every library under
 * scripts/e2e/lib a script among `paths` sources (`. "…/lib/<name>"`) counts as one of them, and
 * the libraries it sources in turn (sourcedLibraries), so a harness passes only the scripts it
 * extracts from. `expectNoMissingHelpers` reads those lines out of a run's output for the
 * harness to fail on.
 */
export function nameMissingHelpers(...paths: string[]): string {
  const names = new Set<string>();
  for (const path of sourcedLibraries(...paths)) {
    for (const found of readFileSync(path, "utf8").matchAll(definition)) names.add(found[1]);
  }
  return `exec {harness_stderr}>&2
command_not_found_handle() {
  case " ${[...names].join(" ")} " in
    *" $1 "*) printf 'harness: no definition for %s\\n' "$1" >&"$harness_stderr" ;;
  esac
  return 127
}`;
}

/**
 * Throws, naming each one, when a run's output shows the harness left a helper of the extracted
 * functions undefined. A harness calls it on every stream a run's shell writes.
 */
export function expectNoMissingHelpers(...outputs: string[]): void {
  const named = outputs.flatMap((out) =>
    [...out.matchAll(/harness: no definition for (\S+)/g)].map((found) => found[1])
  );
  const missing = [...new Set(named)];
  if (missing.length > 0) {
    throw new Error(
      `the harness neither extracts nor stubs ${missing.join(", ")}, which the functions it ` +
        "extracts call: add each one beside them"
    );
  }
}
