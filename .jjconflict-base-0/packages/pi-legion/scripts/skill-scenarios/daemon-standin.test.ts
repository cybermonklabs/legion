import { expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { LegionStateResponse } from "@legion/contracts/legion-api";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";

// The stand-in asks the daemon's own functions for the phase its role works, which `go run`
// compiles, so a host with no go runs none of this.
const skip = skipWithout("go");

// The run directory, reserved at module scope: `temporaryDirectory` registers the hook that
// makes it, and a hook cannot be registered from inside a test.
const work = temporaryDirectory("skill-scenarios-standin");

/** The stand-in, started on a port the kernel picks, with its own log and boot token file. It
 * prints `listening on http://127.0.0.1:<port>` once the socket is open, which is what this
 * waits for. */
async function standin(role: string) {
  const dir = path.join(work, role);
  mkdirSync(dir, { recursive: true });
  const token = path.join(dir, "boot-token");
  writeFileSync(token, "rig-boot-token\n");
  const child = Bun.spawn(
    [
      "bun",
      path.join(import.meta.dir, "daemon-standin.ts"),
      "0",
      path.join(dir, "daemon.log"),
      token,
      "skills1",
      "LWEVAL-1",
      role,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  const reader = child.stdout.getReader();
  let printed = "";
  while (!printed.includes("listening on")) {
    const { value, done } = await reader.read();
    if (done) throw new Error(`the stand-in exited before it listened: ${printed}`);
    printed += new TextDecoder().decode(value);
  }
  const url = /listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(printed)?.[1];
  if (url === undefined) throw new Error(`no address in ${printed}`);
  const phase = async () => {
    const state = LegionStateResponse.parse(await (await fetch(`${url}/legion/v1/state`)).json());
    return state.issues["LWEVAL-1"]?.phase;
  };
  const complete = async (handoff: boolean) => {
    const minted = await (
      await fetch(`${url}/legion/v1/grants`, {
        method: "POST",
        body: JSON.stringify({
          sessionId: "01a0f2cc",
          secret: "rig-secret",
          tree: "LWEVAL-1",
          issue: "LWEVAL-1",
        }),
      })
    ).json();
    const grant = z.object({ grantId: z.string() }).parse(minted);
    return await fetch(`${url}/legion/v1/handoff/complete`, {
      method: "POST",
      body: JSON.stringify({
        grantId: grant.grantId,
        summary: "the round is in",
        verdict: "pass",
        ready: false,
        ...(handoff ? { handoffId: "h-review", handoffVersion: 1 } : {}),
      }),
    });
  };
  return { phase, complete, stop: () => child.kill() };
}

// Catches: a stand-in that serves one frozen state, where a reviewer that obeys its own rule —
// tell the implementer once `read_record` shows the issue in `accepting`
// (review-threads.md step 1) — reads `reviewing` for ever and never sends, so the scenario
// scores a correct agent as one that skipped the step. Measured on the batch of
// 2026-11-08T13:08Z: five of five head runs approved correctly and every one read
// `toldImplementer=false`.
test.skipIf(skip)(
  "an accepted completion of reviewing leaves the issue in accepting, as the daemon does",
  async () => {
    const daemon = await standin("reviewer");
    try {
      expect(await daemon.phase()).toBe("reviewing");
      expect((await daemon.complete(true)).status).toBe(200);
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that moves every phase to `accepting`, which would tell a tester its own
// phase had ended. Only the review round ends in `accepting`; every other phase's completion
// ends the run, and no scenario reads the record after it.
test.skipIf(skip)(
  "a completion of another phase leaves the issue where it was",
  async () => {
    const daemon = await standin("tester");
    try {
      expect(await daemon.phase()).toBe("testing");
      expect((await daemon.complete(true)).status).toBe(200);
      expect(await daemon.phase()).toBe("testing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);
