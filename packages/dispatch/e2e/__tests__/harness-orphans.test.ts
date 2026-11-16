/** What is left of a harness whose parent dies without stopping it: nothing.
 *
 *  Every listener the harness starts is the leader of a process group of its own
 *  (`Supervisor`, e2e/local-instance-support.ts), so a stop reaches it and what it starts in turn.
 *  A parent that is killed - SIGKILL, an out-of-memory kill, a crashed runner - runs no stop, and
 *  before this the fakes outlived it: fake-envoy, fake-github, fake-broker, the plain-HTTP proxy
 *  and the dev OIDC issuer were found running hours after the run that started them, holding
 *  memory on a shared machine. The row starts a real harness in a parent process of its own, kills
 *  that parent with SIGKILL, and reads each listener's pid: none may survive.
 *
 *  The harness is a deployed run's (`PLAYWRIGHT_BASE_URL`), which starts the fake Envoy and the
 *  plain-HTTP proxy and no Dispatch server, so the row needs no database and no Go build. Each is
 *  started by `startHarness` exactly as a local run starts it. */
import { afterEach, expect, test } from "bun:test";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { harnessVariables, stacksVariable } from "../harness-ports";

const makeDirectory = temporaryDirectories("harness-orphans");
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const harnessStart = fileURLToPath(new URL("../harness-start.ts", import.meta.url));

/** A hang guard: a cold `bun` start of two listeners takes a few seconds, and the reaping that
 *  follows the kill is bounded below at 15 s. */
const ROW_MS = 60_000;

/** True while any process of group `pgid` is alive: signal 0 to the group reaches every member. */
function groupAlive(pgid: number): boolean {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Polls `ready` until it holds or `ms` pass: what this waits for is another process's write or
 *  the kernel reaping a killed one, which raise no event here. */
async function eventually(ready: () => boolean | Promise<boolean>, ms: number): Promise<boolean> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await ready()) return true;
    await Bun.sleep(50);
  }
  return ready();
}

/** Groups a failing row found alive, killed after it so the row leaks nothing itself. */
const survivors: number[] = [];
afterEach(() => {
  for (const pgid of survivors.splice(0)) {
    try {
      process.kill(-pgid, "SIGKILL");
    } catch {
      // ESRCH: the group is gone.
    }
  }
});

test(
  "a harness whose parent is killed leaves none of its listeners running",
  async () => {
    const pidsFile = join(makeDirectory(), "pids");
    // The parent's environment: this one's, with no port and no stack manifest a lane set, so
    // every listener binds a port the OS chooses, and a deployed run's base URL, which nothing
    // here dials.
    const env: Record<string, string | undefined> = {
      ...process.env,
      PLAYWRIGHT_BASE_URL: "http://127.0.0.1:9",
      [stacksVariable]: undefined,
    };
    for (const variable of Object.values(harnessVariables)) env[variable] = undefined;
    const parent = Bun.spawn(
      [
        "bun",
        "-e",
        `import { writeFileSync } from "node:fs";
         import { startHarness } from ${JSON.stringify(harnessStart)};
         const harness = await startHarness();
         writeFileSync(${JSON.stringify(pidsFile)},
           harness.processes.map((started) => started.child.pid).join("\\n") + "\\n");
         setInterval(() => {}, 1 << 30);`,
      ],
      { cwd: dispatchPackage, env, stderr: "ignore", stdout: "ignore" }
    );
    const written = await eventually(
      async () =>
        /\n$/u.test(
          await Bun.file(pidsFile)
            .text()
            .catch(() => "")
        ),
      ROW_MS / 2
    );
    if (!written) {
      parent.kill("SIGKILL");
      throw new Error("the parent started no harness: it wrote no pids");
    }
    const pids = (await Bun.file(pidsFile).text()).trim().split("\n").map(Number);
    survivors.push(...pids);
    expect(pids.length).toBe(2);
    expect(pids.every(groupAlive)).toBe(true);

    parent.kill("SIGKILL");
    await parent.exited;

    await eventually(() => !pids.some(groupAlive), 15_000);
    expect(pids.filter(groupAlive)).toEqual([]);
  },
  ROW_MS
);
