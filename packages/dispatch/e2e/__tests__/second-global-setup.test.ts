/** A second `globalSetup` in one process must read the environment the first one found, not
 *  what the first one wrote.
 *
 *  Playwright's UI mode is that process: a re-run calls `runGlobalTeardown()` and then the setup
 *  tasks again without leaving the runner (node_modules/.bun/playwright@1.64.1/node_modules/
 *  playwright/lib/runner/index.js:6896-6915). The hook used to leave stack 0's variables in
 *  `process.env` under their plain names, so the second start read the STOPPED stack's
 *  `DATABASE_URL` as though the caller had named it, took the "a run of one stack against the
 *  database the caller named starts no Postgres" branch (`e2e/harness-pool.ts`), and handed the
 *  server a database that was gone. The hook now writes the manifest and nothing else, and
 *  `e2e/global-teardown.ts` takes even that back out.
 *
 *  WHY A CHILD AND NOT A CALL FROM THIS RUNNER. The hook can be driven from a test - it takes a
 *  config object and reads `process.argv` and `process.env` - but the thing to prove is what it
 *  leaves in ONE process's environment across two runs, so the process has to be a fresh one
 *  whose environment this file controls, and it must be gone again afterwards. The child below
 *  is that process, and the reading is the one the finding is about: the second start's own
 *  Postgres, on a port of its own.
 *
 *  WHY IT IS SKIPPED WITHOUT A POSTGRES PROVIDER. It starts two whole stacks, each with a
 *  Postgres, a Go server and the fakes, which is minutes of work and needs
 *  `DISPATCH_E2E_PG_BIN` or `DISPATCH_E2E_PG_IMAGE` to make the databases at all
 *  (`e2e/harness-postgres.ts`). Without one it would prove nothing, so it is skipped and says
 *  so, as `e2e/__tests__/psql-session.test.ts`'s cases are. */

import { expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

import { stacksVariable } from "../harness-ports";
import { runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

const pgProvider = ["DISPATCH_E2E_PG_BIN", "DISPATCH_E2E_PG_IMAGE"].filter(
  (variable) => (process.env[variable] ?? "").trim() !== ""
);
const withoutPostgres = pgProvider.length === 0;
if (withoutPostgres) {
  console.error(
    "e2e/__tests__/second-global-setup.test.ts: skipped, since neither DISPATCH_E2E_PG_BIN nor " +
      "DISPATCH_E2E_PG_IMAGE names a Postgres this run can make two stacks' databases with."
  );
}

/** Two stacks, each a Go build and an initdb away, and the second reuses the first's build
 *  cache. Measured at about nine minutes cold on a four-core pod. */
const twoStacks = 1_800_000;

test.skipIf(withoutPostgres)(
  "a second globalSetup starts a Postgres of its own rather than reading the first's",
  async () => {
    // `filteredProjects` names one Chromium project, so the hook's browser refusal asks for
    // Chromium alone rather than for every engine of the mode, and `workers: 1` is one stack.
    const script =
      'const setup = (await import("./e2e/global-setup.ts")).default;' +
      'const teardown = (await import("./e2e/global-teardown.ts")).default;' +
      'const { stacksVariable } = await import("./e2e/harness-ports.ts");' +
      'const config = { filteredProjects: [{ name: "chromium" }], workers: 1 };' +
      "const urls = () => JSON.parse(process.env[stacksVariable]).map((s) => s.DATABASE_URL);" +
      "await setup(config);" +
      "const first = urls();" +
      "const plainAfterSetup = process.env.DATABASE_URL ?? null;" +
      "await teardown();" +
      "const manifestAfterTeardown = process.env[stacksVariable] ?? null;" +
      "await setup(config);" +
      "const second = urls();" +
      "await teardown();" +
      "console.log(JSON.stringify({ first, second, plainAfterSetup, manifestAfterTeardown }));";
    const { code, named, stderr, stdout } = await runBounded({
      argv: ["bun", "--eval", script],
      bound: "twoStackBuild",
      cwd: dispatchPackage,
      env: {
        PATH: process.env.PATH ?? "",
        // The caller's own HOME, unlike every other child in this directory: the stack builds
        // the Go server, and a home of its own would be a cold build cache and ten minutes.
        HOME: process.env.HOME ?? "",
        TMPDIR: tmpdir(),
        // Where this machine keeps its browsers, when it keeps them anywhere but under HOME:
        // the hook refuses a run whose engines are not downloaded before it starts anything,
        // and the child would otherwise be refused for browsers it can see perfectly well.
        // Playwright reads both of these - its registry takes `XDG_CACHE_HOME` over
        // `~/.cache` - so both travel when they are set.
        ...Object.fromEntries(
          ["PLAYWRIGHT_BROWSERS_PATH", "XDG_CACHE_HOME"]
            .filter((variable) => process.env[variable] !== undefined)
            .map((variable) => [variable, process.env[variable]])
        ),
        ...Object.fromEntries(pgProvider.map((variable) => [variable, process.env[variable]])),
      },
      from: import.meta.url,
    });
    expect(code, `${named} exited ${code}:\n${stderr}`).toBe(0);
    const read = JSON.parse(stdout) as {
      first: (string | undefined)[];
      second: (string | undefined)[];
      plainAfterSetup: string | null;
      manifestAfterTeardown: string | null;
    };

    // The finding itself: the second start made a database of its own. Had it read the first
    // stack's URL out of `process.env`, it would have started no Postgres and the two would be
    // the same string - the one naming a database the teardown had already dropped.
    expect(read.first[0], stderr).toMatch(/^postgres:\/\//);
    expect(read.second[0], stderr).not.toBe(read.first[0]);

    // Why it is clean: the hook writes the manifest and nothing else, and the teardown removes
    // the manifest. The child was given no `DATABASE_URL`, so a value under that plain name
    // could only have come from the hook.
    expect(read.plainAfterSetup).toBeNull();
    expect(read.manifestAfterTeardown, `${stacksVariable} outlived the teardown`).toBeNull();
  },
  twoStacks
);
