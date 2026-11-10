/** A UI session of `e2e/playwright.config.ts` that has no stack to run against is refused, and
 *  told how to get one.
 *
 *  Playwright's UI mode runs no `globalSetup` that starts servers, so a session started with
 *  nothing up has no Dispatch server, no Postgres and no fake listener behind any of its rows.
 *  Measured on this head before the refusal existed: the tree held the `global-setup.ts` node
 *  alone, Run all ran nothing, and no error reached the UI, its page or its process
 *  (dispatch://INFRA-1410). At the merge base the same session could not load the config at all,
 *  because the unset-ports refusal fired, so the base gave a person a sentence where this head
 *  gave silence; the refusal below is that sentence, with the recipe in it.
 *
 *  The config is read in a `bun --eval` child carrying the argv under test, exactly as
 *  `e2e/__tests__/stub-web-server.test.ts` reads it: the question is asked of `process.argv`,
 *  which this test runner's own argv cannot stand in for.
 */

import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** A handful of spawns of about a second and a half each. */
const probeTimeout = 60_000;

/** What loading this config with `argv` and `env` did: exit 0 and the projects it declared, or
 *  the refusal it threw. Only HOME and PATH come from this process, so no `DISPATCH_E2E_*` of
 *  the developer's own can decide a row here. */
async function loadConfig(
  argv: readonly string[],
  env: Record<string, string> = {}
): Promise<{ code: number; stderr: string }> {
  const script =
    'const config = (await import("./e2e/playwright.config.ts")).default;' +
    "console.log(config.projects.length);";
  const child = Bun.spawn(["bun", "--eval", script, "--", ...argv], {
    cwd: dispatchPackage,
    env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "", ...env },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  return { code, stderr };
}

test(
  "a UI session with no stack up is refused, and told how to start one",
  async () => {
    const { code, stderr } = await loadConfig(["--ui"]);
    expect(code).not.toBe(0);
    // The three things the message has to carry: what is wrong, the command that fixes it, and
    // the variable that tells this config the fix is in place.
    expect(stderr).toContain("Playwright's UI mode starts no stack of its own");
    expect(stderr).toContain("bun e2e/harness-up.ts");
    expect(stderr).toContain("export DISPATCH_E2E_REUSE_SERVERS=1");
  },
  probeTimeout
);

test(
  "--ui-port and --ui-host are UI sessions too",
  async () => {
    // `--ui-port=0` is the spelling INFRA-1410's own check uses, and all three flags enter UI
    // mode (`opts.ui || opts.uiHost || opts.uiPort`, lib/cli/testActions.js:60), so a refusal
    // that read `--ui` alone would let two of them through.
    for (const flag of ["--ui-port=0", "--ui-host=127.0.0.1"]) {
      const { code, stderr } = await loadConfig([flag]);
      expect(code, `${flag} loaded the config instead of being refused`).not.toBe(0);
      expect(stderr).toContain("bun e2e/harness-up.ts");
    }
  },
  probeTimeout
);

test(
  "a UI session against a stack someone started loads",
  async () => {
    // The cure, and the whole point of the message: `e2e/harness-up.ts` prints this variable as
    // its last line, and `e2e/visual-run.sh` applies its output the same way.
    const { code, stderr } = await loadConfig(["--ui"], { DISPATCH_E2E_REUSE_SERVERS: "1" });
    expect(code, stderr).toBe(0);
  },
  probeTimeout
);

test(
  "a UI session of the stubbed layer alone loads",
  async () => {
    // A stubbed row replays a recording against this checkout's build and reaches no stack, so
    // there is nothing for it to be refused for want of.
    const { code, stderr } = await loadConfig(["--ui", "--project=stub"]);
    expect(code, stderr).toBe(0);
  },
  probeTimeout
);

test(
  "a run with no UI flag loads, whatever it names",
  async () => {
    // The refusal is the UI's alone: an ordinary run starts its stacks from `globalSetup`.
    const { code, stderr } = await loadConfig(["--project=chromium"]);
    expect(code, stderr).toBe(0);
  },
  probeTimeout
);
