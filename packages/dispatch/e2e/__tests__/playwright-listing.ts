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
// `Response` on globalThis (e2e/__tests__/harness-lanes.test.ts reads a child the same way).
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** What `playwright test --list` reports for the config at `configPath`, relative to
 *  packages/dispatch, run under `environment` and nothing else of the shell's. The run starts no
 *  server and no browser; it costs about five seconds. */
export async function listing(
  configPath: string,
  environment: Record<string, string>
): Promise<Listing> {
  const child = Bun.spawn(
    ["bunx", "playwright", "test", "--config", configPath, "--list", "--reporter=json"],
    {
      cwd: dispatchPackage,
      // No harness port or lane reaches the listing, so it is the plain one the config exempts
      // from its unset-port refusal whatever the shell that started this test holds.
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
      Object.entries(environment)
        .map(([key, value]) => `${key}=${value}`)
        .join(" ") || "the default run";
    throw new Error(`listing ${configPath} ${named} exited ${code}:\n${stderr}\n${stdout}`);
  }
  return JSON.parse(stdout) as Listing;
}

/** Keeps a rejection of `promise` from being reported as unhandled before the test that awaits
 *  it runs: the handler is attached the moment the promise is made, and the promise itself, which
 *  still rejects for whoever awaits it, is what this answers. */
export function retained<T>(promise: Promise<T>): Promise<T> {
  promise.catch(() => {});
  return promise;
}
