import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { LegionStateResponse } from "@legion/contracts/legion-api";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import { DAEMON_MODULE, runDaemonPane } from "./daemon-pane";

// The stand-in asks the daemon's own functions for the phase its role works, which `go run`
// compiles, so a host with no go runs none of this.
const skip = skipWithout("go");

// The run directory, reserved at module scope: `temporaryDirectory` registers the hook that
// makes it, and a hook cannot be registered from inside a test.
const work = temporaryDirectory("skill-scenarios-standin");

/** The daemon's own refusal of a completion of a phase `role` does not run, as daemon-pane.go
 * reads it out of the daemon's source: its code, then its message. The stand-in answers from
 * that same reading, and daemon-pane.test.ts holds the reading to what the daemon's own
 * engine.go writes, so no copy of the sentence lives in this rig. */
function notCurrentPhase(role: string, phase: string, issue: string) {
  const [code = "", message = ""] = runDaemonPane(DAEMON_MODULE, [
    "refusal",
    "not-current-phase",
    role,
    phase,
    issue,
  ]).split("\n");
  return { code, error: message };
}

/** The repository rig.sh's worker world is on and the branch its pull request is open against
 * (its `worker_repo` and `worker_base`, which fixture_write writes into every world.json): the
 * rows below start the stand-in on a world of those, so the record they read is the one a run's
 * agents read. That rig.sh hands the stand-in the run's directory rather than either name, and
 * so can name neither a second time, is rig.test.ts's row "the stand-in is given the run's
 * project, the run's own directory and the phase". */
const WORLD_REPO = "example/widgets";
const WORLD_BASE = "main";

/** The stand-in, started on a port the kernel picks, with its own log and boot token file, on a
 * run directory holding `world`, which defaults to the world rig.sh builds, and in `phase` when
 * the caller names one. It prints
 * `listening on http://127.0.0.1:<port>` once the socket is open, which is what this waits
 * for. */
let started = 0;
async function standin(role: string, startIn = "", world = { repo: WORLD_REPO, base: WORLD_BASE }) {
  // One directory per start, so a second stand-in of the same role writes its own log: a test
  // that reads the log reads its own run's lines and no earlier run's.
  started += 1;
  const dir = path.join(work, `${role}-${started}`);
  mkdirSync(dir, { recursive: true });
  const token = path.join(dir, "boot-token");
  writeFileSync(token, "rig-boot-token\n");
  // The world the run keeps, in the shape fixture_write writes it: the stand-in reads the
  // repository its record names, and the branch it gives as the default, out of this file.
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({
      key: "LWEVAL-1",
      branch: "legion/LWEVAL-1",
      pr: 1001,
      head: "0".repeat(40),
      ...world,
    })
  );
  const log = path.join(dir, "daemon.log");
  const child = Bun.spawn(
    [
      "bun",
      path.join(import.meta.dir, "daemon-standin.ts"),
      "0",
      log,
      token,
      "skills1",
      "LWEVAL-1",
      dir,
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
  /** The state body as the stand-in serves it, read through no schema: what a label's own
   * contract is handed, which is the thing a row below reads with two of them. */
  const body = async (): Promise<unknown> => await (await fetch(`${url}/legion/v1/state`)).json();
  const record = async () => {
    const state = LegionStateResponse.parse(await body());
    return { issue: state.issues["LWEVAL-1"], repositories: state.repositories };
  };
  const phase = async () => (await record()).issue?.phase;
  /** The settings the state read serves, as the body carries them: read before this checkout's
   * schema, which keeps unknown keys, so a key an older contract requires is visible here. */
  const settings = async () =>
    z.looseObject({ settings: z.record(z.string(), z.unknown()) }).parse(await body()).settings;
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
  /** Every line of this stand-in's log, parsed, in the order it wrote them. */
  const logged = () =>
    readFileSync(log, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  return { body, phase, record, complete, settings, logged, stop: () => child.kill() };
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

// Catches: a state body only this checkout's contract describes. The row above reads two keys
// by name, which is a reader's memory of what the base label requires; this one hands the body
// to that label's own schema. A label's agents load the plugin of their own checkout, so a body
// the head's schema takes and the base's refuses breaks the base arm alone and silently — every
// state read of all 30 base runs of 2026-11-09 was refused `a body the daemon API contract does
// not describe`, and that arm measured a world of its own.
//
// The base label's schema is read from its checkout's own
// `packages/contracts/src/legion-api.ts`, under the directory SKILL_SCENARIOS_BASE_CHECKOUT
// names — the checkout `rig.sh profile base <checkout>` builds that label from. With none named
// there is no second contract on this host to read, and the row says what it wants and skips,
// as a row of this rig does for a program its host has not got.
const baseCheckout = process.env.SKILL_SCENARIOS_BASE_CHECKOUT;
if (baseCheckout === undefined)
  console.warn(
    `Skipped the base label's contract row in ${import.meta.path}. ` +
      "SKILL_SCENARIOS_BASE_CHECKOUT names no checkout to read its " +
      "packages/contracts/src/legion-api.ts from. Name the checkout `rig.sh profile base` was " +
      "built from and run the tests again."
  );
/** A schema as a reader of the served body uses it, which is all this needs of another
 * checkout's zod: the answer, and the sentence a refusal carries. */
type BodyReader = {
  safeParse: (body: unknown) => { success: boolean; error?: { message: string } };
};
test.skipIf(skip || baseCheckout === undefined)(
  "the state body is described by the base label's own contract as well as this checkout's",
  async () => {
    const contract = path.join(baseCheckout ?? "", "packages", "contracts", "src", "legion-api.ts");
    // No static import reaches it: the module is another checkout's, at a path this process is
    // handed at run time.
    const loaded: unknown = await import(contract);
    if (typeof loaded !== "object" || loaded === null || !("LegionStateResponse" in loaded))
      throw new Error(`${contract} exports no LegionStateResponse`);
    /** That checkout's own schema object, built by its own zod and read here for its answer
     * alone, which no type of this checkout's zod describes. */
    const base = loaded.LegionStateResponse as BodyReader;
    const daemon = await standin("reviewer");
    try {
      const served = await daemon.body();
      // This checkout's reader first, so a body neither contract describes is read as that
      // rather than as the base label's refusal.
      const here = LegionStateResponse.safeParse(served);
      expect(here.success ? "described" : here.error.message).toBe("described");
      const there = base.safeParse(served);
      expect(there.success ? "described" : (there.error?.message ?? "refused")).toBe("described");
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

// Catches: a stand-in that accepts a completion of a phase its role does not run, and one that
// refuses it with an answer the daemon does not give. The workflow refuses such a completion
// 409 HANDOFF_NOT_CURRENT_PHASE (packages/daemon/internal/workflow/engine.go, RoleFor), and the
// sentence it sends is the daemon's own: this row builds it from that file, as the stand-in
// does through daemon-pane.go's `refusal`, so neither this test nor the stand-in holds a copy
// of it to go stale. The
// route's earlier HANDOFF_NO_RUN fence is not what a rig half meets: each half is a live
// registered worker serving generation 1, so the route attributes the completion to a run and
// the phase is the workflow's to refuse. A stand-in answering 200 here teaches an agent that a
// phase it is in no phase of completes, and a stand-in answering some other refusal teaches it
// to quote its architect a code and a sentence the daemon never sends.
test.skipIf(skip)(
  "a completion of a phase the role does not run is refused as the daemon refuses it",
  async () => {
    const daemon = await standin("implementer", "accepting");
    try {
      const refused = await daemon.complete(false);
      expect(refused.status).toBe(409);
      expect(await refused.json()).toEqual(notCurrentPhase("implementer", "accepting", "LWEVAL-1"));
      // Nothing moved: the issue stands where it stood, as the refusal says.
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in whose record names a repository or a default branch the run's world has
// not got. Agents read the record for the repository their `gh` calls name and for the branch
// their work goes above, so a second name is a second world, which cost recorded runs their
// review (rig.sh's worker world comment holds that story). Both are the world's own, read
// out of the run's world.json: the branch here is not `main`, so a stand-in that wrote the
// rig's usual default rather than reading the world fails this row.
test.skipIf(skip)(
  "the record names the repository and the default branch of the run's own world",
  async () => {
    const daemon = await standin("reviewer", "", { repo: "widgets-org/widgets", base: "trunk" });
    try {
      const { issue, repositories } = await daemon.record();
      expect(issue?.repo).toBe("widgets-org/widgets");
      expect(issue?.defaultBranch).toBe("trunk");
      expect(repositories.mapped).toEqual([{ repo: "widgets-org/widgets", default: true }]);
      expect(repositories.owners).toEqual(["widgets-org"]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// And the world rig.sh builds is served as it stands, so no row above reads a world of this
// file's own making.
test.skipIf(skip)(
  "the record names rig.sh's own worker world",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const { issue } = await daemon.record();
      expect(issue?.repo).toBe(WORLD_REPO);
      expect(issue?.defaultBranch).toBe(WORLD_BASE);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a log nothing writes until the agent's first call, where a run whose agent never
// reached the daemon leaves no daemon.log and scores as the rig error of a record nothing
// wrote, rather than as the run that moved nothing (score.ts's `unscored`). The line is the
// stand-in's own, written when it starts listening, as record-publishes.ts writes its
// `recording` line, and it is the record's first line, before any answer of the run's.
test.skipIf(skip)(
  "the log opens with the stand-in's own line, written before any request is answered",
  async () => {
    const daemon = await standin("reviewer");
    try {
      expect(daemon.logged()).toMatchObject([{ opened: "daemon.log" }]);
      expect(await daemon.phase()).toBe("reviewing");
      const after = daemon.logged();
      expect(after[0]).toMatchObject({ opened: "daemon.log" });
      expect(after[1]).toMatchObject({ path: "/legion/v1/state", status: 200 });
    } finally {
      daemon.stop();
    }
  },
  600_000
);
