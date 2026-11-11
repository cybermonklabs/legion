/** The one `playwright test --list` subprocess the bun tests of this directory read a listing
 *  from, and the helper that keeps a listing started ahead of its test from being reported as an
 *  unhandled rejection.
 *
 * It lives here rather than in `e2e/configurations.ts` because that module's header promises it
 * imports no Playwright and spawns nothing, and the contract this file holds is one subprocess:
 * the argv, the working directory, the environment floor, how the pipes are read and what a
 * non-zero exit says. Two copies of it drift apart the next time the config refuses a run.
 *
 * `bun test` collects only `*.test.ts` and its kin, so a plain `.ts` module under `__tests__/`
 * runs nowhere — the same thing `e2e/__tests__/fixtures/` relies on for the configs and specs it
 * holds.
 */

import { fileURLToPath } from "node:url";

import type { Listing } from "../configurations";

// A child's pipes are read with the global `Response`, which is Bun's own here: this half of the
// package's `bun test` loads no preload and registers no DOM, so nothing puts happy-dom's
// `Response` on globalThis (e2e/__tests__/harness-stacks.test.ts reads a child the same way).
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** What `playwright test --list` reports for the config at `configPath`, relative to
 *  packages/dispatch, run under `environment` and nothing else of the shell's, with `extra`
 *  before the `--list`. The run starts no server and no browser. Measured at this head on this
 *  two-core pod: a listing of `e2e/playwright.config.ts` that names no project takes about three
 *  and a half seconds (3.44, 3.59 and 3.50 seconds in three runs), and one naming the three
 *  wall-clock projects about one (1.18 and 1.20 seconds in two).
 *
 *  `extra` is where a caller names the projects or the grep whose selection it is reading -
 *  `e2e/__tests__/pull-request-lane.test.ts` lists the lane the shard job runs. It goes before
 *  `--list` because Playwright reads the `--list` of `--grep --list` as that option's value. */
export async function listing(
  configPath: string,
  environment: Record<string, string>,
  extra: readonly string[] = []
): Promise<Listing> {
  const child = Bun.spawn(
    ["bunx", "playwright", "test", "--config", configPath, ...extra, "--list", "--reporter=json"],
    {
      cwd: dispatchPackage,
      // Nothing of the shell's environment reaches the listing, and nothing needs to: a listing
      // runs no `globalSetup`, so it starts no stack and reads no port.
      env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "", ...environment },
      stderr: "pipe",
      stdout: "pipe",
    }
  );
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  if (code !== 0) {
    const named =
      [...extra, ...Object.entries(environment).map(([key, value]) => `${key}=${value}`)].join(
        " "
      ) || "the default run";
    throw new Error(`listing ${configPath} ${named} exited ${code}:\n${stderr}\n${stdout}`);
  }
  return JSON.parse(stdout) as Listing;
}

/** The `--project` arguments naming `projects` on such a command line, in the list's own order.
 *  The four readers of a listing that names its projects - this directory's `no-browser-rows`,
 *  `pull-request-lane` and `row-configurations` tests, and `e2e/tools/row-map.ts` - spell them
 *  through this rather than each its own way, and they spell them attached, which is the one
 *  spelling this suite writes: the option is variadic, so `--project <name>` reads whatever
 *  follows it as a second name (`packages/dispatch/AGENTS.md`, "Write a project as
 *  `--project=<name>` and never `--project <name>`"). */
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
