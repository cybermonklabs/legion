// What a test does with a temporary path whose name holds a character some tool cannot take: a
// space, a `'` or a `?` in TMPDIR, as in a TMPDIR under a directory named `tmp with space`,
// `tmp-sq'x` or `tmp-q?x`.
//
// A shell takes any path as one single-quoted word (`shellQuote`). Three kinds of path cannot be
// fixed that way, and are made here instead, under a base whose path holds none of the characters:
//
//   - a path Bun loads a module from. Bun, which Oh My Pi runs on, loads no module from a path that
//     holds a '?', in any form (`bun <dir>/tmp-q?x/m.js` answers "Module not found"), so Oh My Pi's
//     HOME, whose natives it loads, an extension file, a plugin's dist and a fake whose shebang is
//     bun hold none of MODULE_PATH_REFUSES (`ompScratch`, `moduleScratch`, `moduleBase`,
//     `moduleDirectory`);
//   - a path a test keeps as one unquoted word, where quoting it would change what is tested: a
//     command line whose form a parser under test reads (AskGate's grammar, score.ts's skill
//     reads). Those hold none of UNQUOTED_WORD_REFUSES (`wordDirectory`);
//   - a path a script takes only when it is plain: rig.sh and the model route installers it runs
//     write a path into a `!command` or a mount, or split a list of them on blanks. Those hold none
//     of PLAIN_WORD_REFUSES (the general forms, with bases of the test's own).
import { afterAll, beforeAll } from "bun:test";
import { randomBytes } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, realpathSync, rmdirSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

/** `value` as one single-quoted shell word: each `'` closed, escaped and opened again. */
export function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

/** What Bun takes in no path it loads a module from: a `?`. */
export const MODULE_PATH_REFUSES = /\?/g;

/** What a path kept as one unquoted word must not hold: a blank, which splits it, a `'`, which
 * opens a quote, and a `?`, which globs (and is MODULE_PATH_REFUSES, so Bun loads from it too). */
export const UNQUOTED_WORD_REFUSES = /[ '?]/g;

/** What rig.sh and scripts/e2e/lib's model route installers refuse in a path they are given:
 * anything but `[A-Za-z0-9/._-]` (rig.sh's judge_keys, install-key-file-model-route.sh,
 * install-model-gateway.sh). It holds MODULE_PATH_REFUSES and UNQUOTED_WORD_REFUSES. */
export const PLAIN_WORD_REFUSES = /[^A-Za-z0-9/._-]/g;

/** The longest unix socket path Linux takes: its sun_path holds 108 bytes with the terminating
 * zero (the Go tests' testtmp.maxSocketPath). */
const MAX_SOCKET_PATH = 107;

/**
 * `<package>/.cache/<name>`, where the package is the nearest directory at or above `from` that
 * holds a package.json: a base of the package's own, which the repository's .gitignore (`.cache`)
 * leaves out. A caller passes its own `import.meta.dir`.
 */
export function packageCache(from: string, name: string): string {
  for (let directory = from; ; directory = path.dirname(directory)) {
    if (existsSync(path.join(directory, "package.json"))) {
      return path.join(directory, ".cache", name);
    }
    if (path.dirname(directory) === directory) throw new Error(`no package.json above ${from}`);
  }
}

/**
 * A fresh directory, made now with `prefix`, whose path holds no character `refuses` matches:
 * under the first of `bases` that holds none, takes a directory (each but os.tmpdir() is made when
 * it is missing), when `socket` names the longest path relative to the directory that the test
 * binds a unix socket at, leaves that path within the 107 bytes Linux takes, and passes `accept`,
 * which answers why a directory will not do (a hardlink that cannot reach it) or undefined. With
 * none, it throws naming what each base answered; it never skips. The caller removes the directory.
 */
export function scratchWithout(
  refuses: RegExp,
  bases: readonly string[],
  prefix: string,
  options: {
    readonly socket?: string;
    readonly accept?: (directory: string) => string | undefined;
  } = {}
): string {
  const answers: string[] = [];
  for (const base of new Set(bases)) {
    const held = [...new Set(base.match(refuses))];
    if (held.length > 0) {
      answers.push(`${base} holds ${held.map((char) => JSON.stringify(char)).join(" and ")}`);
      continue;
    }
    let directory: string;
    try {
      if (base !== os.tmpdir()) mkdirSync(base, { recursive: true, mode: 0o700 });
      directory = mkdtempSync(path.join(base, prefix));
    } catch (error) {
      answers.push(`${base} made no directory (${(error as Error).message})`);
      continue;
    }
    if (options.socket !== undefined) {
      const bytes = Buffer.byteLength(path.join(directory, options.socket));
      if (bytes > MAX_SOCKET_PATH) {
        rmdirSync(directory);
        answers.push(`${base} gives ${options.socket} a path of ${bytes} bytes`);
        continue;
      }
    }
    const refusal = options.accept?.(directory);
    if (refusal !== undefined) {
      rmdirSync(directory);
      answers.push(`${base} made a directory that will not do (${refusal})`);
      continue;
    }
    return directory;
  }
  const fits =
    options.socket === undefined
      ? ""
      : ` and leaves ${options.socket} within the ${MAX_SOCKET_PATH} bytes a unix socket path holds`;
  throw new Error(
    `no base gives a directory whose path holds no character of ${refuses.source}${fits}: ${answers.join("; ")}`
  );
}

/** The base `scratchWithout` would make its directory under, found by making one there and
 * removing it at once: the `root` a @legion/test-fixtures `temporaryDirectories` maker takes. */
export function baseWithout(
  refuses: RegExp,
  bases: readonly string[],
  accept?: (directory: string) => string | undefined
): string {
  const probe = scratchWithout(refuses, bases, "probe-", { accept });
  rmdirSync(probe);
  return path.dirname(probe);
}

/**
 * `scratchWithout` in the shape of @legion/test-fixtures' `temporaryDirectory`, for one directory
 * a whole file or `describe` shares: call it where that fixture is called, never in a test body.
 * The base is chosen at the call (`baseWithout`); the path under it is reserved then, made in the
 * `beforeAll` this registers and removed in its `afterAll`. `real` joins the name onto the base's
 * realpath.
 */
export function directoryWithout(
  refuses: RegExp,
  bases: readonly string[],
  prefix: string,
  options: {
    readonly real?: boolean;
    readonly accept?: (directory: string) => string | undefined;
  } = {}
): string {
  const chosen = baseWithout(refuses, bases, options.accept);
  const base = options.real === true ? realpathSync(chosen) : chosen;
  const directory = path.join(base, `${prefix}${randomBytes(4).toString("hex")}`);
  beforeAll(() => {
    mkdirSync(directory, { mode: 0o700 });
  });
  afterAll(() => {
    rmSync(directory, { recursive: true, force: true });
  });
  return directory;
}

/** The bases of the forms below: os.tmpdir(), then its closest ancestor that holds no character
 * `refuses` matches, then the caller's package's `.cache/test-tmp`, so a base outside the
 * repository comes first wherever one qualifies. `from` is the caller's `import.meta.dir`. */
function packageBases(from: string, refuses: RegExp): string[] {
  return [os.tmpdir(), ...cleanAncestor(os.tmpdir(), refuses), packageCache(from, "test-tmp")];
}

/** `scratchWithout` for a directory Bun loads a module from. */
export function moduleScratch(from: string, prefix: string): string {
  return scratchWithout(MODULE_PATH_REFUSES, packageBases(from, MODULE_PATH_REFUSES), prefix);
}

/** `baseWithout` for directories Bun loads a module from, as a `temporaryDirectories` root. */
export function moduleBase(from: string): string {
  return baseWithout(MODULE_PATH_REFUSES, packageBases(from, MODULE_PATH_REFUSES));
}

/** `directoryWithout` for a directory Bun loads a module from. */
export function moduleDirectory(from: string, prefix: string): string {
  return directoryWithout(MODULE_PATH_REFUSES, packageBases(from, MODULE_PATH_REFUSES), prefix);
}

/** The repository's root, three directories above this file. */
const REPOSITORY_ROOT = path.resolve(import.meta.dir, "../../..");

/** Why `directory` will not do for a module that must resolve nothing from `checkout`'s
 * node_modules, or undefined: it is the checkout or below it. The two are compared by their real
 * paths, so a symlink into the checkout counts. */
export function insideCheckout(
  directory: string,
  checkout: string = REPOSITORY_ROOT
): string | undefined {
  const root = realpathSync(checkout);
  const relative = path.relative(root, realpathSync(directory));
  if (relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
    return undefined;
  }
  return `it is inside the checkout ${root}, whose node_modules would resolve what the module must carry`;
}

/** The bases of `outsideModuleDirectory`: os.tmpdir() when it holds no '?', else its closest
 * ancestor that holds none, else /tmp, each taken only outside `checkout` (`insideCheckout`). */
export function outsideModuleBase(checkout: string = REPOSITORY_ROOT): string {
  const bases = [os.tmpdir(), ...cleanAncestor(os.tmpdir(), MODULE_PATH_REFUSES), "/tmp"];
  return baseWithout(MODULE_PATH_REFUSES, bases, (directory) =>
    insideCheckout(directory, checkout)
  );
}

/**
 * `directoryWithout` for a directory Bun loads a module from that must sit outside the repository:
 * under `outsideModuleBase`, never the package's `.cache`. A module there resolves no package from
 * the repository's node_modules, so a test of what a packed plugin carries reads only what the
 * plugin carries; with Bun's auto-install off (`--no-install`), nothing else fills a gap either.
 */
export function outsideModuleDirectory(prefix: string): string {
  return directoryWithout(MODULE_PATH_REFUSES, [outsideModuleBase()], prefix);
}

/** `directoryWithout` for a directory whose paths a test keeps as unquoted words. */
export function wordDirectory(from: string, prefix: string): string {
  return directoryWithout(UNQUOTED_WORD_REFUSES, packageBases(from, UNQUOTED_WORD_REFUSES), prefix);
}

/**
 * `scratchWithout` for a root the real Oh My Pi is handed (its HOME, a plugin root, an extension
 * file): os.tmpdir() when it holds no '?', else its closest ancestor that holds none, which is most
 * often on its filesystem, else this package's `.cache/test-omp`, else /tmp. A root a case binds a
 * unix socket under names that socket's path relative to it (`socket`), and takes /tmp, the
 * shortest base, before the package's `.cache`, and only a base that leaves the socket's path
 * within the bytes Linux takes. A root a HOME's natives are linked into passes `accept` too
 * (omp-harness.ts's `ompRoot`), so it shares the natives cache's filesystem.
 */
export function ompScratch(
  prefix: string,
  options: {
    readonly socket?: string;
    readonly accept?: (directory: string) => string | undefined;
  } = {}
): string {
  const cache = packageCache(import.meta.dir, "test-omp");
  const first = [os.tmpdir(), ...cleanAncestor(os.tmpdir(), MODULE_PATH_REFUSES)];
  const bases =
    options.socket === undefined ? [...first, cache, "/tmp"] : [...first, "/tmp", cache];
  return scratchWithout(MODULE_PATH_REFUSES, bases, prefix, options);
}

/** The closest directory above `directory`, below the root, whose path holds no character
 * `refuses` matches, as a list of none or one: none when `directory` holds none itself, or when
 * only the root would. */
export function cleanAncestor(directory: string, refuses: RegExp): string[] {
  if ((directory.match(refuses) ?? []).length === 0) return [];
  for (
    let parent = path.dirname(directory);
    parent !== path.dirname(parent);
    parent = path.dirname(parent)
  ) {
    if ((parent.match(refuses) ?? []).length === 0) return [parent];
  }
  return [];
}
