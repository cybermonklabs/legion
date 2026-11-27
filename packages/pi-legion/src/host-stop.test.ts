import { afterEach, describe, expect, test } from "bun:test";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess } from "bun";

// R16 of dispatch://INFRA-3589, against Oh My Pi's own exit callbacks (`postmortem` of
// `@oh-my-pi/pi-utils`), not a stand-in for them. Each claim runs in an OS process of its own
// (`test/host-stop-pane.ts`). This file sends it a real SIGTERM, as the worker shim does when the
// daemon's registration deadline retires a claim. In RPC mode the SIGTERM emits no
// `session_shutdown`: Oh My Pi runs pi-envoy's stop from its exit callbacks, and the claim has not
// heard of that stop when the daemon refuses its ready. The claim's exit waits for the stop's
// DELETE (`ompHostStop`, `exitAfterHostStop`), and Oh My Pi then ends the process itself, with 143.
// With no stop begun, the claim exits at once with 1. The rows below take real time
// because the process, its signal and Oh My Pi's handler are real; each waits on what the process
// writes, never on a set delay. The bound on the wait is held by the rows of
// `claim-session.test.ts` and `controller-session.test.ts`, on fake timers.

const CHILD = path.join(import.meta.dir, "..", "test", "host-stop-pane.ts");
const SCRATCH = temporaryDirectory("host-stop-pane");

/** How long one row may take: a process start, which loads the claim modules, and its stop. */
const ROW_TIMEOUT_MS = 30_000;

const children = new Set<Subprocess>();

afterEach(async () => {
  for (const child of children) {
    child.kill("SIGKILL");
    await child.exited;
  }
  children.clear();
});

type Kind = "pane" | "controller";

/**
 * Starts one claim's process: a phase worker's claim boot (`pane`) or the daemon-launched
 * controller's claim (`controller`). The daemon refuses its ready once the stop has begun
 * (`at-stop`) or at once (`now`). `ready` settles once the process wrote its first line, and
 * `lines` with every line it wrote, once it has ended.
 */
function startClaim(kind: Kind, ready: "at-stop" | "now") {
  const child = Bun.spawn([process.execPath, CHILD], {
    cwd: SCRATCH,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: SCRATCH,
      TMPDIR: SCRATCH,
      XDG_STATE_HOME: path.join(SCRATCH, "state"),
      XDG_DATA_HOME: path.join(SCRATCH, "data"),
      XDG_CACHE_HOME: path.join(SCRATCH, "cache"),
      LEGION_BOOT_TOKEN: fixtureSecret("boot"),
      LEGION_STATE_DIR: SCRATCH,
      LEGION_PROJECT: "legion",
      LEGION_SLACK_CHANNELS: "",
      LEGION_DAEMON_URL: "http://daemon.test",
      ...(kind === "controller" ? { LEGION_CONTROLLER: "1" } : {}),
      HOST_STOP_PANE_KIND: kind,
      HOST_STOP_PANE_READY: ready,
    },
    stdin: "ignore",
    stdout: "pipe",
    stderr: "ignore",
  });
  children.add(child);
  const first = Promise.withResolvers<void>();
  const lines = (async () => {
    const decoder = new TextDecoder();
    let text = "";
    for await (const chunk of child.stdout) {
      text += decoder.decode(chunk, { stream: true });
      if (text.includes("\n")) first.resolve();
    }
    first.resolve();
    return text.split("\n").filter((line) => line !== "");
  })();
  return { child, ready: first.promise, lines };
}

/** Waits for `child` to end, and answers its status. */
async function ended(child: Subprocess): Promise<number> {
  const status = await child.exited;
  children.delete(child);
  return status;
}

describe("a claim the daemon refuses while Oh My Pi's stop runs", () => {
  test.each(["pane", "controller"] as const)(
    "a %s exits only after pi-envoy's stop has sent its DELETE",
    async (kind) => {
      const claim = startClaim(kind, "at-stop");
      await claim.ready;
      claim.child.kill("SIGTERM");
      const status = await ended(claim.child);

      expect(await claim.lines).toEqual(["ready", "release", "delete"]);
      // Oh My Pi ends the process itself once its stop has ended, before the claim's own exit runs.
      expect(status).toBe(143);
    },
    ROW_TIMEOUT_MS
  );

  test.each(["pane", "controller"] as const)(
    "a %s with no stop begun exits at once, with status 1",
    async (kind) => {
      const claim = startClaim(kind, "now");
      const status = await ended(claim.child);

      // `process.exit` starts the exit callbacks and waits for none, as on main: the stop begins
      // after the exit and is cut short at its release.
      expect(await claim.lines).toEqual(["ready", "exit 1", "release"]);
      expect(status).toBe(1);
    },
    ROW_TIMEOUT_MS
  );
});
