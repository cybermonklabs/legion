import { existsSync, readFileSync, statSync } from "node:fs";
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
// A quoted word ends at the quote that closes it at the top level, which is what the shell reads,
// not at the last quote on the line: read to the last one, a trailing comment holding a quoted
// word named a path that does not exist, and the library behind it was neither read nor refused.
// Inside double quotes a `$(` opens a substitution whose own quotes do not close the word, so the
// `$(dirname …)` forms still end at their own closing quote. That quote ends the shell word only
// where a space, the line's end or one of `;&|<>()` follows it: bash joins a quoted part and
// whatever touches it into one word, so `"$lib/x.sh"y` is the file x.shy, and read to the quote
// it would name x.sh, a file the shell never sources. A word no quote closes, or one that goes on
// past its closing quote, keeps its opening quote, and `resolvedSource` refuses any word that
// still holds one. An unquoted word runs to the first space, as the shell reads it.
const sourceLine = /^[ \t]*(?:\.|source)[ \t]+(\S.*)$/;

/** The text of the quoted word `rest` starts with, up to the quote that closes it at the top
 *  level, or `undefined` when no quote closes it or the word goes on past that quote. */
function closedWord(rest: string, quote: string): string | undefined {
  let depth = 0;
  for (let index = 1; index < rest.length; index++) {
    if (quote === '"' && rest.startsWith("$(", index)) {
      depth++;
      index++;
      continue;
    }
    const char = rest[index];
    if (depth > 0) {
      if (char === ")") depth--;
      continue;
    }
    if (char !== quote) continue;
    return /^(?:$|[\s;&|<>()])/.test(rest.slice(index + 1)) ? rest.slice(1, index) : undefined;
  }
  return undefined;
}

/** The word each `.` or `source` line of `lines` names, with the line it is on (counting from 1)
 *  and the line itself, in order. A quoted word is given without its quotes; one no quote closes,
 *  or one that goes on past its closing quote, is given with its opening quote, so
 *  `resolvedSource` refuses it. */
function sourceWords(lines: readonly string[]): { word: string; at: number; line: string }[] {
  const found: { word: string; at: number; line: string }[] = [];
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index] ?? "";
    const match = sourceLine.exec(line);
    if (match === null) continue;
    const rest = match[1] ?? "";
    const quote = rest.startsWith('"') ? '"' : rest.startsWith("'") ? "'" : "";
    const word = quote === "" ? (/^\S+/.exec(rest)?.[0] ?? "") : (closedWord(rest, quote) ?? rest);
    found.push({ word, at: index + 1, line });
  }
  return found;
}

/** Whether a shell word ends where `after` begins: at the line's end, a space, or one of
 *  `;&|<>()`. */
function wordEnds(after: string): boolean {
  return /^(?:$|[\s;&|<>()])/.test(after);
}

/** The checkout, from this file in scripts/e2e/lib. */
const checkout = join(import.meta.dir, "..", "..", "..");

/** A line every planted control script carries, so a scan of the checkout can tell a test's
 *  plant from a script of the tree: a control left behind there by a run that was killed, or
 *  written there by a later test, is then refused by name rather than read as a real script. */
export const controlPlantMark = "# legion-scan-control: planted by a test, no script of the tree";

/** The script itself, as the tree's scripts write it: `"$0"` or `"${BASH_SOURCE[0]}"`. */
const scriptItself = String.raw`"(?:\$0|\$\{BASH_SOURCE\[0\]\})"`;
/** `$(dirname "…")` of the script itself, with or without `--`. */
const dirnameOfScript = String.raw`\$\(dirname (?:-- )?${scriptItself}\)`;
/** The script's own directory in each spelling the repository writes: `$(dirname …)`, or
 *  `$(cd "$(dirname …)<climb>" && pwd)` with or without `--` and `-P`, where <climb> is a path
 *  such as `/../..` the `cd` adds and the first group. `$(dirname "$0")` is the directory of the
 *  script that runs, which is the script itself for every script here that writes it. */
const ownDirectoryForm = String.raw`(?:${dirnameOfScript}|\$\(cd (?:-- )?"${dirnameOfScript}((?:/[^"$\x60]*)?)" && pwd(?: -P)?\))`;
/** A source word that starts with the script's own directory: the climb, then the rest. */
const ownDirectory = new RegExp(String.raw`^${ownDirectoryForm}/(.+)$`);
/** A value assigned to a variable that is the script's own directory, or a path under it. */
const ownDirectoryValue = new RegExp(String.raw`^${ownDirectoryForm}(?:/(.*))?$`);
/** A value assigned to a variable that is `$root`, or a path under it. */
const rootValue = /^\$\{?root\}?(?:\/(.*))?$/;
// The one-line `case` on `${BASH_SOURCE[0]}` that sets NAME to the part before its last `/`, or to
// `.` where it holds none: the script's own directory, whether it ran by a path or from that
// directory. The group is NAME.
const ownDirectoryCase =
  /^[ \t]*case \$\{BASH_SOURCE\[0\]\} in \*\/\*\) ([A-Za-z_]\w*)=\$\{BASH_SOURCE\[0\]%\/\*\} ;; \*\) \1=\. ;; esac[ \t]*$/;

/** The file `written` reaches from the script at `from`, whose lines are `lines`, in the checkout
 *  `tree`, or `undefined` when this reader cannot name one: a word that is a variable whole
 *  (`. "$lib"`), a glob, any other command substitution, a word that still holds a quote, which
 *  is one no quote closed or one that goes on past its quote, or a path that names no file of the
 *  tree, a directory among them. `$root` is the checkout, as every script here sets it. A path
 *  built on any other variable is read through that variable's assignment in the same script,
 *  and only there (`assignedDirectory`): a variable the script does not assign, assigns twice or
 *  assigns some other directory names one only the running shell knows, as `. "$drivers/x.sh"`
 *  with `drivers=/elsewhere` does, so the line is refused rather than read as the file of the
 *  same name beside the script. Such a line is named by `unreadableSources`, and the walk reads
 *  only a path this returns. */
function resolvedSource(
  from: string,
  written: string,
  lines: readonly string[],
  tree: string
): string | undefined {
  const own = ownDirectory.exec(written);
  if (own !== null) {
    const rest = own[2] ?? "";
    return rest.includes("$") ? undefined : regularFile(resolve(climbed(from, own[1]), rest));
  }
  // The own-directory forms hold quotes inside their substitution and are read above; any other
  // word that holds a quote is one sourceWords could not close, so it names no file this reads.
  if (written.includes('"') || written.includes("'")) return undefined;
  if (written.includes("$(") || written.includes("`") || written.includes("*")) return undefined;
  const against = /^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?\/(.+)$/.exec(written);
  if (against === null) {
    if (written.includes("$")) return undefined;
    return regularFile(isAbsolute(written) ? written : resolve(dirname(from), written));
  }
  const [, name = "", rest = ""] = against;
  if (rest.includes("$")) return undefined;
  if (name === "root") return regularFile(resolve(tree, rest));
  const directory = assignedDirectory(name, lines, from, tree);
  return directory === undefined ? undefined : regularFile(resolve(directory, rest));
}

/** Where a command starts: the line's start, after one of `;&|(){}`, or after a keyword that
 *  starts a command list, with a `!` that negates it. A word such as `read` anywhere else is text,
 *  not a command. */
const commandStart = String.raw`(?:^|[;&|(){}]|\b(?:then|do|else|elif|if|while|until)\b)[ \t]*(?:![ \t]+)?`;

/** The directory the variable `name` holds in the script at `from`, whose lines are `lines`, or
 *  `undefined` unless this reader is sure of it. It is sure where every line that assigns `name`
 *  gives it one directory, in a spelling of the script's own directory or as a path under `$root`,
 *  and at least one line does. A line that assigns it any other way, or in a way this reader does
 *  not read (`read`, `for` or `printf -v` at a command's start, `${name:=…}`, an assignment after
 *  other words), makes it unsure. */
function assignedDirectory(
  name: string,
  lines: readonly string[],
  from: string,
  tree: string
): string | undefined {
  const assigns = new RegExp(
    String.raw`(?:^|[\s;&|(){}])${name}\+?=|\$\{${name}:?=|${commandStart}(?:read|mapfile|readarray)\b[^#'"]*\b${name}\b|${commandStart}for[ \t]+${name}[ \t]|${commandStart}printf[ \t]+-v[ \t]+${name}\b`
  );
  const plain = new RegExp(
    String.raw`^[ \t]*(?:(?:local|readonly|export|declare(?:[ \t]+-[A-Za-z]+)*)[ \t]+)?${name}=(.*)$`
  );
  const directories = new Set<string>();
  for (const line of lines) {
    if (/^[ \t]*#/.test(line)) continue;
    if (ownDirectoryCase.exec(line)?.[1] === name) {
      directories.add(dirname(from));
      continue;
    }
    if (!assigns.test(line)) continue;
    const value = assignedValue(plain.exec(line)?.[1]);
    const directory = value === undefined ? undefined : valueDirectory(value, from, tree);
    if (directory === undefined) return undefined;
    directories.add(directory);
  }
  return directories.size === 1 ? [...directories][0] : undefined;
}

/** The shell word an assignment's `rest` starts with, unquoted, or `undefined` where the word
 *  goes on past its end or `rest` is not given. */
function assignedValue(rest: string | undefined): string | undefined {
  if (rest === undefined) return undefined;
  if (rest.startsWith('"')) return closedWord(rest, '"');
  if (rest.startsWith("$(")) {
    let depth = 0;
    for (let index = 0; index < rest.length; index++) {
      if (rest[index] === "(") depth++;
      if (rest[index] === ")" && --depth === 0) {
        return wordEnds(rest.slice(index + 1)) ? rest.slice(0, index + 1) : undefined;
      }
    }
    return undefined;
  }
  const word = /^[^\s;&|<>()'"`]*/.exec(rest)?.[0] ?? "";
  return wordEnds(rest.slice(word.length)) ? word : undefined;
}

/** The directory an assigned `value` names from the script at `from`, or `undefined` where it is
 *  not the script's own directory or a path under `$root`, or holds another variable. */
function valueDirectory(value: string, from: string, tree: string): string | undefined {
  const own = ownDirectoryValue.exec(value);
  if (own !== null) {
    const rest = own[2] ?? "";
    return rest.includes("$") ? undefined : resolve(climbed(from, own[1]), rest);
  }
  const rooted = rootValue.exec(value);
  if (rooted === null) return undefined;
  const rest = rooted[1] ?? "";
  return rest.includes("$") ? undefined : resolve(tree, rest);
}

/** The directory of the script at `from`, and then the path `climb` a `cd` form adds to it, such
 *  as `/../..`: relative to that directory, though it starts with a slash. */
function climbed(from: string, climb: string | undefined): string {
  return resolve(dirname(from), (climb ?? "").replace(/^\/+/, ""));
}

/** `path` where it names a regular file, or a link to one, else `undefined`. A directory is no
 *  library: bash's `.` refuses one, and the walk would throw on reading it rather than name the
 *  line. A path this process cannot stat is no file it can read either. */
function regularFile(path: string): string | undefined {
  try {
    return statSync(path).isFile() ? path : undefined;
  } catch {
    return undefined;
  }
}

/** Every line of the script at `path` that sources a library this file cannot name, as
 *  `<path under tree>:<line>: <the line>`, where `tree` is the checkout the script is read in.
 *  A scan that walks what a script sources must fail on these: the file behind such a line is
 *  never read, so anything in it is unseen. */
export function unreadableSources(path: string, tree: string = checkout): string[] {
  const found: string[] = [];
  const lines = readFileSync(path, "utf8").split("\n");
  for (const { word, at, line } of sourceWords(lines)) {
    if (resolvedSource(path, word, lines, tree) !== undefined) continue;
    found.push(`${relative(tree, path)}:${at}: ${line.trim()}`);
  }
  return found;
}

/**
 * Every script among `paths`, every library one of them sources (`. "<path>"`), wherever in the
 * checkout that path reaches, and the libraries those source in turn, each path once. A harness
 * reads the functions a script reaches through it, and a test the libraries a script runs with.
 */
export function sourcedLibraries(...paths: string[]): string[] {
  return sourcedLibrariesIn(checkout, paths);
}

/** {@link sourcedLibraries} for scripts read in the checkout `tree`, where `$root` names `tree`:
 *  the form a test takes for a tree of its own, laid out as the checkout is. */
export function sourcedLibrariesIn(tree: string, paths: readonly string[]): string[] {
  const read = new Set<string>();
  const pending = [...paths];
  for (let path = pending.pop(); path !== undefined; path = pending.pop()) {
    if (read.has(path) || !existsSync(path)) continue;
    read.add(path);
    const lines = readFileSync(path, "utf8").split("\n");
    for (const { word } of sourceWords(lines)) {
      const reaches = resolvedSource(path, word, lines, tree);
      // A line this reader cannot resolve, a path that names no file among them, is named by
      // unreadableSources, which reads the same lines this walk does, so no line falls between
      // the two; the walk goes on rather than ending on it.
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
 * extracted function silences (`helper >/dev/null 2>&1`) is named too. Every library a script
 * among `paths` sources, wherever in the checkout its path reaches, counts as one of them, and the
 * libraries it sources in turn (sourcedLibraries), so a harness passes only the scripts it
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
