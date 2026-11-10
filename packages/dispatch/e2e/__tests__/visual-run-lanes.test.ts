import { describe, expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

// `e2e/visual-run.sh` seeds the review corpus on the host and then runs the two visual projects
// in the pinned container. The seed runs outside any Playwright worker, so `TEST_PARALLEL_INDEX`
// is unset and it writes LANE 0 alone (`e2e/harness-ports.ts`); the script forwards every
// `DISPATCH_E2E_*` name into the container, the lane count among them. A run of more than one
// lane would therefore put the rows of the second lane on a database nothing seeded - and an
// empty Inbox draws the heading its readiness locator waits for, so `--update-snapshots`, which
// `packages/dispatch/AGENTS.md` gives as the way a person takes a baseline, would write one of an
// empty corpus and nothing would say so.
//
// The script refuses that run. Only the CI job's own `DISPATCH_E2E_WORKERS=1` kept it from
// happening before, and nothing held the job to that, which is why the refusal is here: it is
// the one guard of this layer that no browser row can reach, since a row that ran at all would
// mean the guard had already let the run through.
const script = fileURLToPath(new URL("../visual-run.sh", import.meta.url));

/** Runs the script with `env` added, from the package root, and answers its status and stderr.
 *  Every case below is refused before the seed, so none starts a harness or reaches `docker`. */
async function run(env: Record<string, string>): Promise<{ code: number; stderr: string }> {
  const child = Bun.spawn(["sh", script], {
    cwd: fileURLToPath(new URL("../..", import.meta.url)),
    env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "", ...env },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  return { code, stderr };
}

/** The per-test limit of the rows below. Each of them spawns a real process, so what a row costs
 *  follows the machine rather than the work: measured on this pod - 2 CPUs, 8 runs under 6 busy
 *  loops (3 x nproc) - the worst reading was 9.46 s, and 30 s is about three times that worst
 *  reading. It is a limit sized from measurement rather than a hang guard, though a row that
 *  hangs is ended by it. Bun's unwritten 5 s default is under the worst reading, so a busy
 *  machine read as a defect of whatever change was under test. */
const SPAWN_TEST_MS = 30_000;

describe("the visual run's one-lane rule", () => {
  test(
    "refuses a run of more than one lane, naming the variable to set",
    async () => {
      const { code, stderr } = await run({ DISPATCH_E2E_LANES: "2" });
      expect(code).toBe(1);
      expect(stderr).toContain("DISPATCH_E2E_WORKERS=1");
      // The reason, not only the remedy: a reader who meets this has to know the second lane's
      // rows would compare against a database nothing seeded.
      expect(stderr).toContain("lane 0");
    },
    SPAWN_TEST_MS
  );

  test(
    "reads the worker count where the lane count is unset, as the harness does",
    async () => {
      const { code, stderr } = await run({ DISPATCH_E2E_WORKERS: "4" });
      expect(code).toBe(1);
      expect(stderr).toContain("DISPATCH_E2E_WORKERS=1");
    },
    SPAWN_TEST_MS
  );

  test(
    "lets one lane through to the seed",
    async () => {
      // One lane passes the rule, so the script goes on and fails at the first thing this host
      // has not got - the harness's ports, or `docker`. What it must not do is print the refusal.
      const { stderr } = await run({ DISPATCH_E2E_LANES: "1" });
      expect(stderr).not.toContain("this run has 1 lanes");
      expect(stderr).not.toContain("the visual review takes one");
    },
    SPAWN_TEST_MS
  );
});
