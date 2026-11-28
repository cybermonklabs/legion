// How the rig's test files start the programs a run starts: each program built once per suite
// into a bundle the rows spawn, and every child a row waits on synchronously read through one
// check that turns a child bun test killed into an error that says so. Test files import this;
// no run's world does.
//
// Why a bundle. A row that spawns `bun gh-standin.ts` pays for bun transpiling the stand-in and
// every module it imports on each call, and some rows make a dozen calls. Measured on 2026-11-15
// on a 2-CPU pod running two suites of this directory at once, `gh pr view` through the
// stand-in's source took a median of 225 ms (467 ms at most) and through its bundle 162 ms
// (293 ms). The bundle is built from the source in the suite's `beforeAll`, so every row still
// runs the code of this checkout, and bun only loads it. The build is no more than 0.7 s of the
// hook's bound on that host (score.ts's, the largest).
//
// Why the check. When a row outlives its bound, bun test kills the processes the row started
// and the row's code runs on: a `Bun.spawnSync` returns with no exit code (`null`, which its
// type does not admit) and a signal, so a row read `expect(read.code).toBe(1)` as
// `Received: null` and a reader took it for a stand-in that answered nothing. A row that waits
// on a `Bun.spawn` child is another matter: bun test reports it timed out and runs none of it
// after the await, so nothing it reads of the killed child reaches the output, and no check of
// that child would either.
import { beforeAll } from "bun:test";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { BunPlugin, SpawnOptions, SyncSubprocess } from "bun";
import { DAEMON_MODULE, LABEL_SOURCES, labelGhSelectors } from "./daemon-pane";
import { RUN_SELECTORS } from "./gh-selectors";

/** `cmds` short enough for an error: a `bash -c` script is the rig's own functions, hundreds of
 * lines, of which the first is enough to name it. */
function named(cmds: readonly string[]): string {
  return cmds
    .slice(0, 6)
    .map((word) => {
      const line = word.split("\n")[0] ?? "";
      return line.length > 60 || line !== word ? `${line.slice(0, 60)}…` : word;
    })
    .concat(cmds.length > 6 ? ["…"] : [])
    .join(" ");
}

/** `Bun.spawnSync(cmds, options)`, or an error naming the signal when the child ended by one
 * (with `exitCode` null), so no row reads a killed child's `null` as the program's status. */
export function spawnChecked<
  const In extends SpawnOptions.Writable = "ignore",
  const Out extends SpawnOptions.Readable = "pipe",
  const Err extends SpawnOptions.Readable = "pipe",
>(cmds: string[], options?: SpawnOptions.SpawnSyncOptions<In, Out, Err>): SyncSubprocess<Out, Err> {
  const made = Bun.spawnSync(cmds, options);
  // The type says `exitCode: number`; bun hands back null for a child a signal ended.
  const code: number | null = made.exitCode;
  if (made.signalCode !== undefined || code === null)
    throw new Error(
      `${named(cmds)} ended by ${made.signalCode ?? "no signal bun reported"} with no exit code, so it gave no answer to read. bun test kills the processes a row started when the row outlives its bound: 5000 ms, Bun's default, unless the row, the file's setDefaultTimeout or \`bun test --timeout\` names another`
    );
  return made;
}

/** Each place a module names itself, and what that is for the module at `file`. */
const LOCATION = /\bimport\.meta\.(dirname|dir|filename|file|path|url)\b/g;
function locationOf(file: string, key: string): string {
  if (key === "dir" || key === "dirname") return path.dirname(file);
  if (key === "file") return path.basename(file);
  if (key === "url") return pathToFileURL(file).href;
  return file;
}
/** A bundle runs from its own directory, so a module's `import.meta.dir` there names the
 * bundle's and not the module's: daemon-pane.ts finds the Go programs it runs beside itself and
 * score.ts the checkout two levels up. This writes each such name as the literal the source
 * would have read where it lies, so the bundled module reaches what the source does. Bun's
 * bundler already answers `import.meta.main` false in every module but the entry. */
const OWN_LOCATION: BunPlugin = {
  name: "own-location",
  setup(build) {
    build.onLoad({ filter: /\.[cm]?[jt]sx?$/ }, async ({ path: file, loader }) => {
      const text = await Bun.file(file).text();
      if (text.search(LOCATION) < 0) return undefined;
      return {
        contents: text.replace(LOCATION, (_, key: string) => JSON.stringify(locationOf(file, key))),
        loader,
      };
    });
  },
};

/** The path of a bundle of the program at `source`, built from it in a `beforeAll` into a
 * directory of the suite's own that its `afterAll` removes. Call it where `temporaryDirectory`
 * may be called — a test file's module scope or a `describe` body — and spawn the path with
 * `bun` as the source was: a `-t` filter that selects no row there builds nothing. The build
 * takes the hook bound of the scope it is called in. */
export function bundled(source: string): string {
  const name = path.basename(source, path.extname(source));
  const directory = temporaryDirectory(`bundle-${name}`);
  beforeAll(async () => {
    const built = await Bun.build({
      entrypoints: [source],
      target: "bun",
      outdir: directory,
      naming: "[name].js",
      plugins: [OWN_LOCATION],
    });
    if (!built.success) throw new AggregateError(built.logs, `bun build ${source} failed`);
  });
  return path.join(directory, `${name}.js`);
}

/** This checkout's gh selectors as a run holds them, read once a process. */
let ownSelectors: string | undefined;
/** Writes this checkout's gh selectors into the run under `runDir` (gh-selectors.ts's
 * RUN_SELECTORS), as rig.sh's worker_boot writes its label's before any agent starts: the gh
 * stand-in refuses an owner outside a world's project by them, and answers no call of a world
 * that has none. Read through daemon-pane.ts's `labelGhSelectors`, which runs gh-selectors.go. */
export function writeGhSelectors(runDir: string): void {
  ownSelectors ??= `${JSON.stringify(labelGhSelectors(path.resolve(DAEMON_MODULE, "..", "..")))}\n`;
  writeFileSync(path.join(runDir, RUN_SELECTORS), ownSelectors);
}

/** Each commit `sourcesCommit` made, by its repository, checkout and sources. */
const commits = new Map<string, string>();
/** A commit of the label sources `checkout` holds (daemon-pane.ts's LABEL_SOURCES, or those of
 * `sources`; none for a checkout that is not there, an empty tree), made in the bare repository
 * `repo` (made here when it is not there), as rig.sh's checkout_commit records the commit a label
 * was built from in its built-from: what health and the scorer rebuild a run's label's sources
 * from, given `--repo` (`recordedCheckout`), and never the checkout itself. Made once per
 * repository, checkout and sources; nothing is written under `checkout`. */
export function sourcesCommit(
  repo: string,
  checkout: string,
  sources: readonly string[] = LABEL_SOURCES
): string {
  const key = [repo, checkout, ...sources].join("\0");
  const known = commits.get(key);
  if (known !== undefined) return known;
  const index = mkdtempSync(path.join(os.tmpdir(), "sources-index-"));
  const env = {
    ...process.env,
    GIT_INDEX_FILE: path.join(index, "index"),
    GIT_AUTHOR_NAME: "rig",
    GIT_AUTHOR_EMAIL: "rig@example.invalid",
    GIT_AUTHOR_DATE: "2026-11-18T00:00:00Z",
    GIT_COMMITTER_NAME: "rig",
    GIT_COMMITTER_EMAIL: "rig@example.invalid",
    GIT_COMMITTER_DATE: "2026-11-18T00:00:00Z",
  };
  const git = (...args: string[]) => {
    const done = spawnChecked(["git", `--git-dir=${repo}`, ...args], { env });
    if (done.exitCode !== 0)
      throw new Error(`git ${args.join(" ")}: ${done.stderr.toString().trim()}`);
    return done.stdout.toString().trim();
  };
  try {
    if (!existsSync(repo)) {
      const made = spawnChecked(["git", "init", "-q", "--bare", repo], { env });
      if (made.exitCode !== 0) throw new Error(`git init ${repo}: ${made.stderr.toString()}`);
    }
    const held = sources.filter((source) => existsSync(path.join(checkout, source)));
    if (held.length > 0) git(`--work-tree=${checkout}`, "-C", checkout, "add", "--", ...held);
    const commit = git("commit-tree", git("write-tree"), "-m", `the label sources of ${checkout}`);
    commits.set(key, commit);
    return commit;
  } finally {
    rmSync(index, { recursive: true, force: true });
  }
}

/** The shell `script`'s definition of `name`, from `name() {` to the `}` that closes it, with the
 * lines of each here-document it holds skipped: `scriptFunctions` ends a definition at the first
 * line that is `}` alone, which a here-document's own text can be — rig.sh's fixture_cli writes
 * greet.ts, whose functions end so. Read so, a world a row builds writes the files the rig's own
 * world does. */
export function scriptDefinition(script: string, name: string): string {
  const lines = script.split("\n");
  const start = lines.indexOf(`${name}() {`);
  if (start < 0) throw new Error(`the script defines no ${name}()`);
  let delimiter: string | undefined;
  for (const [index, line] of lines.entries()) {
    if (index <= start) continue;
    if (delimiter !== undefined) {
      if (line === delimiter) delimiter = undefined;
    } else if (line === "}") return lines.slice(start, index + 1).join("\n");
    else delimiter = /(?<!<)<<(?!<)-?\s*(['"]?)(\w+)\1/.exec(line)?.[2];
  }
  throw new Error(`the script's ${name}() has no closing }`);
}
