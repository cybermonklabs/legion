import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
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

/** The repository rig.sh's worker world is on (its `worker_repo`, which fixture_write writes into
 * every world.json), read from rig.sh itself so this file holds no second copy of it: the record
 * the stand-in serves and the world the run's gh calls reach are one name or they are two
 * worlds. */
const WORLD_REPO =
  /^worker_repo=(\S+)$/m.exec(readFileSync(path.join(import.meta.dir, "rig.sh"), "utf8"))?.[1] ??
  "";
if (WORLD_REPO === "") throw new Error("rig.sh names no worker_repo");

/** The stand-in, started on a port the kernel picks, with its own log and boot token file, on the
 * world's repository and in `phase` when the caller names one. It prints
 * `listening on http://127.0.0.1:<port>` once the socket is open, which is what this waits
 * for. */
let started = 0;
async function standin(role: string, startIn = "") {
  // One directory per start, so a second stand-in of the same role writes its own log: a test
  // that reads the log reads its own run's lines and no earlier run's.
  started += 1;
  const dir = path.join(work, `${role}-${started}`);
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
      WORLD_REPO,
      role,
      startIn,
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
  const record = async () => {
    const state = LegionStateResponse.parse(await (await fetch(`${url}/legion/v1/state`)).json());
    return { issue: state.issues["LWEVAL-1"], repositories: state.repositories };
  };
  const phase = async () => (await record()).issue?.phase;
  /** The settings the state read serves, as the body carries them: read before this checkout's
   * schema, which keeps unknown keys, so a key an older contract requires is visible here. */
  const settings = async () =>
    z
      .looseObject({ settings: z.record(z.string(), z.unknown()) })
      .parse(await (await fetch(`${url}/legion/v1/state`)).json()).settings;
  /** `POST /legion/v1/handoff/complete` on a grant minted for it, naming a handoff record when
   * `handoff` and naming none otherwise, which is the completion of a worker that has written
   * no record. */
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
  return { phase, record, complete, settings, stop: () => child.kill() };
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

// Catches: a stand-in that accepts a completion naming no record for a phase that ends with
// one. The daemon refuses it 400 HANDOFF_RECORD_REQUIRED and names the `dispatch handoff-write`
// that cures it (packages/daemon/internal/api/handoff.go), and the skill has the worker write
// the record and complete again: a 200 here teaches an agent that a phase ends without the
// record the next role reads.
test.skipIf(skip)(
  "a completion of a phase that ends with a record, naming none, is refused as the daemon refuses it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const refused = await daemon.complete(false);
      expect(refused.status).toBe(400);
      expect(await refused.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining("dispatch handoff-write --issue LWEVAL-1 --phase review"),
      });
      // Nothing moved: the round has not ended, so the issue stands in `reviewing`.
      expect(await daemon.phase()).toBe("reviewing");
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

// Catches: a stand-in that serves the settings of this checkout's contract alone, which breaks
// every label whose contract requires a key this one dropped — silently, in that label alone
// (the measurement that found it is told once, at `daemon-standin.ts`'s `settings` line).
test.skipIf(skip)(
  "the state read serves every settings key a label's contract requires",
  async () => {
    const daemon = await standin("reviewer");
    try {
      // `max_fix_attempts` is main ff2a10ef's (contract 58, dropped by 59) and
      // `worker_boot_timeout_seconds` is this checkout's; both are positive integers in each.
      expect(await daemon.settings()).toMatchObject({
        max_fix_attempts: expect.any(Number),
        worker_boot_timeout_seconds: expect.any(Number),
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that serves the role's own phase whatever the run is, so the loop after an
// approval, which runs in `accepting` where the daemon starts no phase, reads `implementing` to
// its implementer and `reviewing` to its reviewer. Each then reads a record saying it holds a
// phase it does not hold, and the step the rule gives for `accepting` is unreachable
// (review-threads.md step 1; rig.sh's run_threads_closed boots both halves there).
test.skipIf(skip)(
  "a boot names the phase the issue stands in, for either half of the loop after an approval",
  async () => {
    for (const role of ["implementer", "reviewer"]) {
      const daemon = await standin(role, "accepting");
      try {
        expect(await daemon.phase()).toBe("accepting");
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in that accepts a completion of a phase no run of the issue serves. The real
// daemon refuses one it can attribute to no run, 409 HANDOFF_NO_RUN
// (packages/daemon/internal/api/handoff.go, noRunCompletion), and the loop after an approval
// holds no task at all: a stand-in answering 200 there teaches an agent that a phase it is in no
// phase of completes, and the refusal the rule has it quote to its architect never arrives.
test.skipIf(skip)(
  "a completion in a phase the role works no run of is refused as the daemon refuses it",
  async () => {
    const daemon = await standin("implementer", "accepting");
    try {
      const refused = await daemon.complete(false);
      expect(refused.status).toBe(409);
      expect(await refused.json()).toMatchObject({
        code: "HANDOFF_NO_RUN",
        error: expect.stringContaining("belongs to no run of LWEVAL-1"),
      });
      // Nothing moved: the issue stands where it stood, as the refusal says.
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in whose record names a repository the run's world has not got. Agents read
// the record for the repository their `gh` calls name, so a second name is a second world: six of
// the forty runs of 2026-11-10 submitted their review to `sjawhar/legion-smoke`, which this
// stand-in named while the world was rig.sh's `worker_repo`, and the queries of the other
// answered 404. The repository is the world's own, read from rig.sh here so the two cannot
// disagree.
test.skipIf(skip)(
  "the record names the repository the run's world is on, in the issue and in the mapping",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const { issue, repositories } = await daemon.record();
      expect(issue?.repo).toBe(WORLD_REPO);
      expect(repositories.mapped).toEqual([{ repo: WORLD_REPO, default: true }]);
      expect(repositories.owners).toEqual([WORLD_REPO.split("/")[0]]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);
