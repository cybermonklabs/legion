/** Which runs of this config are given the stubbed layer's web server, and which must not be.
 *
 *  `e2e/playwright.config.ts` declares `bun e2e/stub/static-server.ts` as a `webServer`, and
 *  Playwright starts a `webServer` for every run of a config but a listing - whatever projects
 *  the run selects. Two jobs of `.github/workflows/dispatch-browser.yaml` run this config
 *  without a stubbed row in sight, and one of them cannot start that command at all: `visual`
 *  runs Playwright inside the pinned container through `e2e/visual-run.sh`, and that image
 *  carries the three browser engines and node, not bun. A run given the command there dies
 *  before its first row with `[WebServer] /bin/sh: 1: bun: not found` and `Error: Process from
 *  config.webServer was not able to start. Exit code: 127`.
 *
 *  It is the full lane that this costs, and the full lane is what the merge queue runs, so a
 *  visual leg that cannot start is a merge that cannot happen; the pull-request lane skips
 *  `visual`, so no run on the pull request says a word about it. Measured on
 *  legion/INFRA-1407 at 7095aa36: the `full / visual` job of runs 42597757591 and 42576471811
 *  failed with those two lines, while main's own nightly 42596473565 was green, main declaring
 *  no `webServer` at all.
 *
 *  So the rule this holds the config to: the run that needs the static file server is given it,
 *  and a run that selects no stub project is given no server to start.
 */

import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** Two spawns of about a second and a half each. */
const probeTimeout = 60_000;

/** The `command` of the web server this config declares for a run whose argv is `argv`, or null
 *  where it declares none. The config reads the selection from `process.argv`, so the reading
 *  is made in a child that carries that argv rather than this test runner's. */
async function webServerCommand(argv: readonly string[]): Promise<string | null> {
  const script =
    'const config = (await import("./e2e/playwright.config.ts")).default;' +
    "const server = config.webServer ?? null;" +
    "const declared = Array.isArray(server) ? (server[0] ?? null) : server;" +
    "console.log(JSON.stringify(declared === null ? null : declared.command));";
  const child = Bun.spawn(["bun", "--eval", script, "--", ...argv], {
    cwd: dispatchPackage,
    env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "" },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  if (code !== 0) {
    throw new Error(`reading the config with ${argv.join(" ")} exited ${code}:\n${stderr}`);
  }
  return JSON.parse(stdout) as string | null;
}

test(
  "a run that selects no stub project is given no web server to start",
  async () => {
    expect(
      await webServerCommand(["--project=visual", "--project=visual-phone"]),
      "The containerised `visual` job runs this config with no bun on its PATH, so a `webServer` " +
        "declared for a run that holds no stubbed row kills the full lane with exit 127 before " +
        "its first row. Declare the stubbed layer's static file server for the runs that select " +
        "a stub project, and for no other run."
    ).toBeNull();
  },
  probeTimeout
);

test(
  "a run of the stubbed layer is given the static file server",
  async () => {
    // The other direction, so the rule above is not met by declaring no server at all: a stub
    // row replays its API from a recording and loads the build under test from this server.
    expect(
      await webServerCommand(["--project=stub", "--project=stub-phone"]),
      "A stub row has no page without `e2e/stub/static-server.ts`: the recording answers " +
        "/api/v1/** and /auth/whoami and nothing else."
    ).toBe("bun e2e/stub/static-server.ts");
  },
  probeTimeout
);
