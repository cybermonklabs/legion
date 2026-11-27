/** The one `playwright test --list` subprocess the bun tests of this directory read a listing
 *  from, the helper that keeps a listing started ahead of its test from being reported as an
 *  unhandled rejection, the command `bun run e2e:embedder` runs, which two of them list, and the
 *  listings that give the files whose rows the quarantine cannot take.
 *
 * It lives here rather than in `e2e/configurations.ts` because that module's header promises it
 * imports no Playwright and spawns nothing, and the contract this file holds is one subprocess's
 * own: the argv, the working directory, the environment floor and what a non-zero exit says. How
 * the pipes are read, the bound and the cost line are `runBounded`'s, in
 * `e2e/__tests__/bounded-subprocess.ts`. Two copies of the argv drift apart the next time the
 * config refuses a run.
 *
 * `bun test` collects only `*.test.ts` and its kin, so a plain `.ts` module under `__tests__/`
 * runs nowhere — the same thing `e2e/__tests__/fixtures/` relies on for the configs and specs it
 * holds.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { type Listing, rowsOfListing } from "../configurations";
import { twoProcessRuns, type UnquarantinableFiles } from "../quarantine";
import { runBounded } from "./bounded-subprocess";

/** The working directory every listing runs in, and what a config path is read against. */
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** What `playwright test --list` reports for the config at `configPath`, relative to
 *  packages/dispatch, run under `environment` and nothing else of the shell's, with `extra`
 *  before the `--list`. The run starts no server and no browser. What one listing cost is in the
 *  line this run prints on its own stderr.
 *
 *  `from` is the caller's `import.meta.url`, which is the file that cost line names. It is
 *  needed because four callers start the same default listing with the same config path and the
 *  same empty environment, and nothing else in the command tells those four apart.
 *
 *  `extra` is where a caller names the projects or the grep whose selection it is reading -
 *  `e2e/__tests__/pull-request-lane.test.ts` lists the lane the shard job runs. It goes before
 *  `--list` because Playwright reads the `--list` of `--grep --list` as that option's value.
 *
 *  `reporters` are loaded beside the JSON one, which stays first so stdout is still the listing:
 *  `e2e/__tests__/shard-split.test.ts` names `./e2e/shard-reporter.ts`, which decides what a
 *  `--shard` listing holds and is in the config's own list only, which a `--reporter` replaces. */
export async function listing(
  from: string,
  configPath: string,
  environment: Record<string, string>,
  extra: readonly string[] = [],
  reporters: readonly string[] = []
): Promise<Listing> {
  const { code, named, stderr, stdout } = await runBounded({
    argv: [
      "bunx",
      "playwright",
      "test",
      "--config",
      configPath,
      ...extra,
      "--list",
      `--reporter=${["json", ...reporters].join(",")}`,
    ],
    bound: "listing",
    cwd: dispatchPackage,
    // Nothing of the shell's environment reaches the listing, and nothing needs to: a listing
    // runs no `globalSetup`, so it starts no stack and reads no port.
    env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "", ...environment },
    from,
  });
  if (code !== 0) {
    throw new Error(`listing ${named} exited ${code}:\n${stderr}\n${stdout}`);
  }
  return JSON.parse(stdout) as Listing;
}

/** The `--project` arguments naming `projects` on such a command line, in the list's own order.
 *  The five readers of a listing that names its projects - this directory's `no-browser-rows`,
 *  `pull-request-lane`, `reading-walk` and `row-configurations` tests, and
 *  `e2e/tools/row-map.ts` - spell them through this rather than each its own way, and they spell
 *  them attached, which is the one spelling this suite writes: the option is variadic, so
 *  `--project <name>` reads whatever follows it as a second name (`packages/dispatch/README.md`,
 *  "Projects"). */
export function projectArguments(projects: readonly string[]): string[] {
  return projects.map((project) => `--project=${project}`);
}

/** Keeps a rejection of `promise` from being reported as unhandled before the test that awaits
 *  it runs: the handler is attached the moment the promise is made, and the promise itself, which
 *  still rejects for whoever awaits it, is what this answers. */
export function retained<T>(promise: Promise<T>): Promise<T> {
  promise.catch(() => {});
  return promise;
}

/** The `playwright test` command `bun run e2e:embedder` ends in, read from this package's
 *  `package.json` rather than written again, so a listing of it holds exactly the rows the
 *  embedder job runs: the variables set before `playwright`, and the arguments after the config.
 *  `e2e/__tests__/pull-request-lane.test.ts` counts those rows into the lane's budget, and
 *  `listUnquarantinableFiles` below reads their files, which the quarantine cannot take. A
 *  script of any other shape is refused by name rather than read as a listing of nothing. */
export function embedderCommand(): { environment: Record<string, string>; extra: string[] } {
  // Read as `e2e/__tests__/row-configurations.test.ts` reads `e2e:clock`.
  const manifest = JSON.parse(
    readFileSync(new URL("../../package.json", import.meta.url), "utf8")
  ) as { scripts: Record<string, string> };
  const script = manifest.scripts["e2e:embedder"] ?? "";
  const words = (script.split("&&").at(-1) ?? "").trim().split(/\s+/);
  const start = words.indexOf("playwright");
  const variables = words.slice(0, Math.max(start, 0));
  if (
    start === -1 ||
    words.slice(start, start + 4).join(" ") !==
      "playwright test --config e2e/playwright.config.ts" ||
    variables.some((word) => !word.includes("="))
  ) {
    throw new Error(
      `package.json's e2e:embedder script no longer ends in \`[NAME=value ...] playwright test ` +
        `--config e2e/playwright.config.ts ...\`: ${JSON.stringify(script)}. Read its rows here ` +
        "the way the embedder job now runs them."
    );
  }
  return {
    environment: Object.fromEntries(
      variables.map((word) => [word.slice(0, word.indexOf("=")), word.slice(word.indexOf("=") + 1)])
    ),
    extra: words.slice(start + 4),
  };
}

/** The files whose rows the quarantine cannot take (`UnquarantinableFiles` of
 *  `e2e/quarantine.ts`), read from listings of this checkout, and the two-process listings they
 *  are read from (`twoProcess`). The embedder job's files are those of a listing of
 *  `embedderCommand`, and the two-process job's those of a listing under each of
 *  `twoProcessRuns`. `e2e/__tests__/quarantine.test.ts` and `e2e/tools/quarantine-check.ts` both
 *  read the files through this, so the two read the same files; the tool reads the two-process
 *  job's rows from `twoProcess` too. `from` is the caller's `import.meta.url`, as `listing`
 *  takes it. The three listings run at once. */
export async function listUnquarantinableFiles(
  from: string
): Promise<{ files: UnquarantinableFiles; twoProcess: Listing[] }> {
  const config = "e2e/playwright.config.ts";
  const embedder = embedderCommand();
  const [embedderRows, ...twoProcess] = await Promise.all([
    listing(from, config, embedder.environment, embedder.extra),
    ...twoProcessRuns.map((environment) => listing(from, config, environment)),
  ]);
  return {
    files: {
      embedder: [...new Set(rowsOfListing(embedderRows).map((row) => row.file))],
      twoProcess: [
        ...new Set(twoProcess.flatMap((listed) => rowsOfListing(listed).map((row) => row.file))),
      ],
    },
    twoProcess,
  };
}
