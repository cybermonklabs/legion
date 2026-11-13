import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { LegionActivityRequest, LegionStateResponse } from "@legion/contracts/legion-api";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import { DAEMON_MODULE, notCurrentPhase, refusalIn } from "./daemon-pane";

// The stand-in asks the daemon's own functions for the phase its role works, which `go run`
// compiles, so a host with no go runs none of this.
const skip = skipWithout("go");

// The run directory, reserved at module scope: `temporaryDirectory` registers the hook that
// makes it, and a hook cannot be registered from inside a test.
const work = temporaryDirectory("skill-scenarios-standin");

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
 * the caller names one. The run directory is named as rig.sh's cmd_run names a run of `label`,
 * which is how the stand-in reads the label whose daemon it answers as (completion-answers.ts's
 * `labelOf`); `completed` is the roles the run's world says completed their phase with a commit
 * (rig.sh's worker_boot COMPLETED). It prints
 * `listening on http://127.0.0.1:<port>` once the socket is open, which is what this waits
 * for. */
let started = 0;
async function standin(
  role: string,
  startIn = "",
  world = { repo: WORLD_REPO, base: WORLD_BASE },
  { label = "head", completed = "" } = {}
) {
  // One directory per start, so a second stand-in of the same role writes its own log: a test
  // that reads the log reads its own run's lines and no earlier run's.
  started += 1;
  const dir = path.join(work, `${role}-${label}-${started}`);
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
      completed,
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
   * no record; `extra` is any key a label's own contract sends beside them. */
  const complete = async (handoff: boolean, extra: Record<string, unknown> = {}) => {
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
        ...(handoff ? { handoffId: "h-review", handoffVersion: 1 } : {}),
        ...extra,
      }),
    });
  };
  /** A POST of `payload` to the stand-in's `route`. */
  const post = (route: string, payload: unknown) =>
    fetch(`${url}${route}`, { method: "POST", body: JSON.stringify(payload) });
  /** Every line of this stand-in's log, parsed, in the order it wrote them. */
  const logged = () =>
    readFileSync(log, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  return { dir, body, phase, record, complete, post, settings, logged, stop: () => child.kill() };
}

/** Runs git in `dir` under an identity of its own and no configuration of the host's. */
function git(dir: string, ...args: string[]): string {
  const result = Bun.spawnSync(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** The pull request of the run under `dir`, in the shapes rig.sh's fixture_write writes it: a
 * bare remote whose issue branch is at the head, no review on it, the create route every world
 * answers a review with, and the merge state GitHub reports while the pull request is at that
 * head, `state` (`CLEAN` unless the row names another), every other head being `CLEAN` (rig.sh's
 * `blocked_at`, gh-call.ts's `mergeStateOn`). `push` puts a commit above the tip and returns it;
 * `approve` and `requestChanges` submit a review of a head through the gh stand-in itself, so the
 * review the daemon stand-in reads is the one the world answered the call with. */
function pullRequest(dir: string, state = "CLEAN") {
  const work = path.join(dir, "work");
  mkdirSync(work);
  git(work, "init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 1;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "the approved head");
  const head = git(work, "rev-parse", "HEAD");
  git(dir, "clone", "-q", "--bare", work, path.join(dir, "remote.git"));
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      mergeStates: { [head]: state },
      routes: [
        {
          match: "^gh api .*pulls/1001/reviews",
          methods: ["POST"],
          stdout: {
            id: 6001,
            html_url: "https://example.invalid/pull/1001#pullrequestreview-6001",
          },
        },
        { match: "^gh api .*pulls/1001/(reviews|comments)", stdout: [] },
      ],
    })
  );
  const review = (event: string, commit: string) => {
    const body = path.join(dir, `review-${event}.json`);
    writeFileSync(body, JSON.stringify({ commit_id: commit, event, body: "the round's review" }));
    const call = Bun.spawnSync(
      [
        "bun",
        path.join(import.meta.dir, "gh-standin.ts"),
        "gh",
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews",
        "--input",
        body,
      ],
      { env: { ...process.env, SKILL_SCENARIO_RUN: dir } }
    );
    if (call.exitCode !== 0) throw new Error(`the review was refused: ${call.stderr.toString()}`);
  };
  return {
    head,
    push: () => {
      writeFileSync(path.join(work, "docs.md"), `the text fix ${Math.random()}\n`);
      git(work, "add", "-A");
      git(work, "commit", "-qm", "docs: the text fix");
      git(work, "push", "-q", path.join(dir, "remote.git"), "legion/LWEVAL-1");
      return git(work, "rev-parse", "HEAD");
    },
    approve: (commit = head) => review("APPROVE", commit),
    requestChanges: (commit = head) => review("REQUEST_CHANGES", commit),
  };
}

// Catches: a stand-in that moves the issue to `accepting` on the reviewer's completion. Main's
// engine ends the round on the review, not the completion (review.go's `reviewRound`,
// review_test.go's "after the review alone"), so a completed round with no review on the pull
// request is still `reviewing`. The stand-in that moved on any accepted completion
// (`daemon-standin.ts`, round 14) hid the case review 5887659447's finding 3 is about.
test.skipIf(skip)(
  "an accepted completion with no approval leaves the issue in reviewing",
  async () => {
    const daemon = await standin("reviewer");
    try {
      pullRequest(daemon.dir);
      expect(await daemon.phase()).toBe("reviewing");
      expect((await daemon.complete(true)).status).toBe(200);
      expect(await daemon.phase()).toBe("reviewing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that ends the round on the approval whatever GitHub reports for the head.
// The engine moves an approval only on a merge state the issue may advance on
// (`record.MergeState.Mergeable`), and waits in `reviewing` on every other: `BLOCKED`, which a
// check the base requires holds the head in while it runs — the normal case while CI runs at the
// approved head — and `DIRTY`, a head that conflicts with its base.
test.skipIf(skip)(
  "an approval of a head GitHub reports BLOCKED or DIRTY leaves the issue in reviewing",
  async () => {
    for (const state of ["BLOCKED", "DIRTY"]) {
      const daemon = await standin("reviewer");
      try {
        pullRequest(daemon.dir, state).approve();
        expect((await daemon.complete(true)).status).toBe(200);
        expect([state, await daemon.phase()]).toEqual([state, "reviewing"]);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in whose record never leaves `reviewing`, where a reviewer that reads it
// after an approval the engine would have taken reads a round that never ended; and one that
// takes a narrower set of states than `Mergeable` does. An approval of the head GitHub reports
// `CLEAN`, `UNSTABLE` or `HAS_HOOKS` ends the round with no completion at all.
test.skipIf(skip)(
  "an approval of a head GitHub reports CLEAN, UNSTABLE or HAS_HOOKS moves the issue to accepting",
  async () => {
    for (const state of ["CLEAN", "UNSTABLE", "HAS_HOOKS"]) {
      const daemon = await standin("reviewer");
      try {
        pullRequest(daemon.dir, state).approve();
        expect([state, await daemon.phase()]).toEqual([state, "accepting"]);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in that reads a change request as no decision, or as one of the head alone.
// `reviewRound` ends the round rejected on a change request whatever head it names and whatever
// the merge state, and the issue goes back to `implementing` (`TriggerReviewRejected`), which
// reviewer-text-findings' defect case reaches in `reviewing`; an approval after it moves nothing.
test.skipIf(skip)(
  "a change request sends the issue back to implementing, and an approval after it moves nothing",
  async () => {
    const daemon = await standin("reviewer");
    try {
      const pull = pullRequest(daemon.dir);
      pull.requestChanges("0".repeat(40));
      expect(await daemon.phase()).toBe("implementing");
      pull.approve();
      expect(await daemon.phase()).toBe("implementing");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that reads an approval of any head as an approval of the pull request's.
// A push while the record is `reviewing` leaves the approval on a head that is no longer the
// pull request's, which sets it aside (`classify.ApprovalSetAside`, `stuckApprovedOtherCode`),
// and only an approval of the new head ends the round. Once the record is in `accepting` a push
// lands outside any round and moves nothing (review_test.go's "the approval of the head, then a
// push"). The approved head is `BLOCKED` and the fix head `CLEAN`, as threads-closed's
// text-fixes world serves them (rig.sh's approved_world).
test.skipIf(skip)(
  "a push after the approval leaves the issue in reviewing until the new head is approved",
  async () => {
    const daemon = await standin("implementer", "reviewing");
    try {
      const pull = pullRequest(daemon.dir, "BLOCKED");
      pull.approve();
      expect(await daemon.phase()).toBe("reviewing");
      const fixed = pull.push();
      expect(await daemon.phase()).toBe("reviewing");
      pull.approve(fixed);
      expect(await daemon.phase()).toBe("accepting");
      pull.push();
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that takes the request keys of this checkout's contract alone. The base
// label, built from main `ff2a10ef`, sends `ready` on every completion, and #2539 took it out of
// this checkout's strict request, so every base completion of 2026-11-12 was answered 400
// `unrecognized_keys` and that arm measured a world of its own (review 5887659447, finding 1).
// A key no label sends is still refused, as the daemon refuses an undecodable body.
test.skipIf(skip)(
  "a completion carrying the base label's ready is answered as that label's daemon answered it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      expect((await daemon.complete(true, { ready: true })).status).toBe(200);
      expect((await daemon.complete(true, { pushed: true })).status).toBe(400);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a log whose refusal lines carry no code, where a reader cannot tell the daemon's own
// refusal of a run's completion from a body the stand-in could not take at all (score.ts's
// `unscored`, which makes the second a rig error and keeps the first a reading of the run).
test.skipIf(skip)(
  "a refusal's log line carries its code, and a body the stand-in cannot take carries none",
  async () => {
    const daemon = await standin("reviewer");
    try {
      await daemon.complete(false);
      await daemon.complete(true, { pushed: true });
      const completions = daemon
        .logged()
        .filter((line) => line.path === "/legion/v1/handoff/complete");
      expect(completions).toMatchObject([
        { status: 400, code: "HANDOFF_RECORD_REQUIRED", role: "reviewer" },
        { status: 400, role: "reviewer" },
      ]);
      expect(completions[1]).not.toHaveProperty("code");
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

/** The daemon's own refusal of `role`'s late completion while the issue stands in `phase`, read
 * out of its source by the reader the stand-in answers from (daemon-pane.ts's
 * `notCurrentPhase`); daemon-pane.test.ts holds that reading to what the daemon's own engine.go
 * writes, so no copy of the sentence lives in this rig. */
function lateRefusal(role: string, phase: string) {
  const refusal = notCurrentPhase(DAEMON_MODULE, role, phase, "LWEVAL-1");
  expect(refusal).toBeDefined();
  return { code: refusal?.code, error: refusal?.message };
}

// Catches: a stand-in that takes a completion from threads-closed's implementer, whose
// implementing round the run's world says already stands (rig.sh's run_threads_closed passes
// COMPLETED), and one that refuses it with an answer the daemon does not give. Either label's
// engine refuses a late role whose row holds a handoff commit 409 HANDOFF_NOT_CURRENT_PHASE
// (engine.go's `leftItsPhase`, `row.HandoffCommit != ""`; `ff2a10ef`'s `RoleFor(issue.Phase) !=
// fact.Role`), with the daemon's own sentence for the phase the issue stands in, which the skill
// has the worker quote to its architect. The route's earlier HANDOFF_NO_RUN fence is not what a
// rig half meets: each half is a live registered worker serving generation 1.
test.skipIf(skip)(
  "threads-closed's implementer, whose round stands, is refused as the daemon refuses it",
  async () => {
    for (const [label, phase] of [
      ["head", "accepting"],
      ["head", "reviewing"],
      ["base", "accepting"],
    ] as const) {
      const daemon = await standin("implementer", phase, undefined, {
        label,
        completed: "implementer",
      });
      try {
        const refused = await daemon.complete(false, { commit: "0".repeat(40) });
        expect([label, phase, refused.status]).toEqual([label, phase, 409]);
        expect(await refused.json()).toEqual(lateRefusal("implementer", phase));
        // Nothing moved: the issue stands where it stood, as the refusal says.
        expect(await daemon.phase()).toBe(phase);
      } finally {
        daemon.stop();
      }
    }
  },
  600_000
);

// Catches: a stand-in that reads "the implementer has completed" off the phase it was booted in
// rather than off what the run's world says. On the head label an implementer whose row holds no
// handoff commit is one `leftItsPhase` takes, and its completion is then of the implementing
// phase it left, whose record it must name: engine.go refuses one naming none 409
// HANDOFF_RECORD_REQUIRED through `refused`.
test.skipIf(skip)(
  "on the head label, a late implementer the world gives no completed round must name its record",
  async () => {
    const daemon = await standin("implementer", "reviewing");
    try {
      const refused = await daemon.complete(false, { commit: "0".repeat(40) });
      expect(refused.status).toBe(409);
      expect(await refused.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining(
          "the implementer's completion of phase implementing of LWEVAL-1 names no handoff record"
        ),
      });
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that decides "record required" from the phase the role was booted in
// rather than the phase the issue stands in now, which answered every completion after a
// reviewer's own REQUEST_CHANGES 400 HANDOFF_RECORD_REQUIRED (the batch at a61c46a7). After the
// round sends the issue to `implementing`, the route does not hold the reviewer to a record:
// `recordBacked := handoffPhase && writer == grant.Role` is false, the implementer being the
// writer there. This checkout's engine then takes the reviewer's late completion
// (`leftItsPhase`), as one of `reviewing`, the phase it left, whose record it must name: naming
// none is refused 409 HANDOFF_RECORD_REQUIRED by the engine, never 400 by the route; naming one
// is taken and moves nothing. A taken completion with a commit puts that commit on the
// reviewer's row, so a second late completion is refused 409 HANDOFF_NOT_CURRENT_PHASE.
test.skipIf(skip)(
  "on the head label, a reviewer's completion after its change request is answered as leftItsPhase answers it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      pullRequest(daemon.dir).requestChanges();
      expect(await daemon.phase()).toBe("implementing");
      const unnamed = await daemon.complete(false);
      expect(unnamed.status).toBe(409);
      expect(await unnamed.json()).toMatchObject({
        code: "HANDOFF_RECORD_REQUIRED",
        error: expect.stringContaining(
          "the reviewer's completion of phase reviewing of LWEVAL-1 names no handoff record: write it with `dispatch handoff-write --issue LWEVAL-1 --phase review --body-file <file>`"
        ),
      });
      expect((await daemon.complete(true, { commit: "0".repeat(40) })).status).toBe(200);
      expect(await daemon.phase()).toBe("implementing");
      const again = await daemon.complete(true, { commit: "0".repeat(40) });
      expect(again.status).toBe(409);
      expect(await again.json()).toEqual(lateRefusal("reviewer", "implementing"));
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: the same boot-phase decision after an approval that moves the issue to `accepting`,
// which writes no record (`phase.Handoff`). The route refuses a completion naming one there 409
// HANDOFF_RECORD_MISMATCH before the engine reads it; one naming none passes the route, and this
// checkout's engine holds it to the record of `reviewing`, the phase it left: 409
// HANDOFF_RECORD_REQUIRED.
test.skipIf(skip)(
  "on the head label, a reviewer's completion after an approval that moved the issue is answered as the daemon answers it",
  async () => {
    const daemon = await standin("reviewer");
    try {
      pullRequest(daemon.dir).approve();
      expect(await daemon.phase()).toBe("accepting");
      const unnamed = await daemon.complete(false);
      expect(unnamed.status).toBe(409);
      expect(await unnamed.json()).toMatchObject({ code: "HANDOFF_RECORD_REQUIRED" });
      const named = await daemon.complete(true);
      expect(named.status).toBe(409);
      expect(await named.json()).toMatchObject({ code: "HANDOFF_RECORD_MISMATCH" });
      expect(await daemon.phase()).toBe("accepting");
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a completion's log line that does not say which phase it was answered on. Health
// allows the head reviewer's 409 HANDOFF_RECORD_MISMATCH in `accepting` alone, since the route
// gives that code a record of another issue, phase or version in a record-backed phase too
// (completion-answers.ts's `ANSWERS`), so the line must name the phase the answer was given on,
// which is the one the stand-in served then and not the one its role was booted in.
test.skipIf(skip)(
  "a completion's log line names the phase it was answered on",
  async () => {
    const daemon = await standin("reviewer");
    try {
      expect((await daemon.complete(true)).status).toBe(200);
      pullRequest(daemon.dir).approve();
      expect((await daemon.complete(true)).status).toBe(409);
      expect(
        daemon
          .logged()
          .filter((line) => line.path === "/legion/v1/handoff/complete")
          .map((line) => [line.status, line.code, line.phase])
      ).toEqual([
        [200, undefined, "reviewing"],
        [409, "HANDOFF_RECORD_MISMATCH", "accepting"],
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a base-label stand-in answered as this checkout's engine answers. Main `ff2a10ef`
// has no `leftItsPhase`: its `handoff` refuses every completion from a role that does not run
// the issue's phase 409 HANDOFF_NOT_CURRENT_PHASE, whatever record it names, once the route,
// which is the same, has let it through. In `accepting` the route refuses a named record 409
// HANDOFF_RECORD_MISMATCH first.
test.skipIf(skip)(
  "on the base label, a reviewer's late completion is refused HANDOFF_NOT_CURRENT_PHASE",
  async () => {
    const rejected = await standin("reviewer", "", undefined, { label: "base" });
    try {
      pullRequest(rejected.dir).requestChanges();
      expect(await rejected.phase()).toBe("implementing");
      for (const handoff of [false, true]) {
        const refused = await rejected.complete(handoff);
        expect([handoff, refused.status]).toEqual([handoff, 409]);
        expect(await refused.json()).toEqual(lateRefusal("reviewer", "implementing"));
      }
    } finally {
      rejected.stop();
    }
    const approved = await standin("reviewer", "", undefined, { label: "base" });
    try {
      pullRequest(approved.dir).approve();
      expect(await approved.phase()).toBe("accepting");
      const unnamed = await approved.complete(false);
      expect(unnamed.status).toBe(409);
      expect(await unnamed.json()).toEqual(lateRefusal("reviewer", "accepting"));
      const named = await approved.complete(true);
      expect(named.status).toBe(409);
      expect(await named.json()).toMatchObject({ code: "HANDOFF_RECORD_MISMATCH" });
    } finally {
      approved.stop();
    }
  },
  600_000
);

// Catches: a stand-in with no `claims/activity` route, which answers the report the agents'
// plugin posts after each turn 404 where the daemon answers it 204 (`s.activity` in
// packages/daemon/internal/api/claims.go). The daemon authenticates the report by the claim's
// secret, as it does `claims/ready`, and refuses a wrong one with claim.InvalidSecret.
test.skipIf(skip)(
  "a registered claim's activity report is answered as the daemon answers it",
  async () => {
    const daemon = await standin("implementer");
    try {
      const registered = z.object({ claimToken: z.string(), secret: z.string() }).parse(
        await (
          await daemon.post("/legion/v1/claims/register", {
            bootToken: "rig-boot-token",
            sessionId: "01a0f2cc",
          })
        ).json()
      );
      const report = LegionActivityRequest.parse({
        claimToken: registered.claimToken,
        sessionId: "01a0f2cc",
        secret: registered.secret,
        activity: {
          at: "2026-11-13T18:00:00.000Z",
          backgroundWork: { running: false, kinds: [] },
          phase: { nextReminderAt: null },
        },
      });
      const answered = await daemon.post("/legion/v1/claims/activity", report);
      expect([answered.status, await answered.text()]).toEqual([204, ""]);
      const wrong = await daemon.post("/legion/v1/claims/activity", {
        ...report,
        secret: "not-the-secret",
      });
      expect([wrong.status, await wrong.json()]).toEqual([
        403,
        { error: "Invalid session secret" },
      ]);
    } finally {
      daemon.stop();
    }
  },
  600_000
);

// Catches: a stand-in that answers a run of a label the rig models no daemon for as some other
// label's daemon would. It reads the label from the run directory's name
// (completion-answers.ts's `labelOf`) and refuses to boot for one `LABEL_DAEMONS` does not name.
test("a run of a label the rig models no daemon for fails the boot", () => {
  const dir = path.join(work, "reviewer-ablate-1");
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "boot-token"), "rig-boot-token\n");
  writeFileSync(
    path.join(dir, "world.json"),
    JSON.stringify({
      key: "LWEVAL-1",
      branch: "legion/LWEVAL-1",
      pr: 1001,
      head: "0".repeat(40),
      repo: WORLD_REPO,
      base: WORLD_BASE,
    })
  );
  const boot = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "daemon-standin.ts"),
    "0",
    path.join(dir, "daemon.log"),
    path.join(dir, "boot-token"),
    "skills1",
    "LWEVAL-1",
    dir,
    "reviewer",
  ]);
  expect(boot.exitCode).toBe(2);
  expect(boot.stderr.toString()).toContain(
    "reviewer-ablate-1 names no label this rig models a daemon for"
  );
});

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

// What daemon-pane.go's `refusal not-current-phase` prints is two lines, the code and then the
// daemon's message, and the stand-in answers a completion of a phase its role does not run with
// exactly that pair or refuses to boot (`refusalAtBoot`). Catches: a printout half read taken
// for a refusal — no message, an empty code, or a message that runs onto a third line, each a
// sentence the daemon never sends — which no row that starts the stand-in on today's daemon can
// reach, since that daemon prints the two lines. No go is needed: this reads the printout alone.
test("a refusal printout is the code and the message on two lines of their own, or no refusal", () => {
  const message = "issue LWEVAL-1 is in accepting, which implementer does not run";
  expect(refusalIn(`HANDOFF_NOT_CURRENT_PHASE\n${message}\n`)).toEqual({
    code: "HANDOFF_NOT_CURRENT_PHASE",
    message,
  });
  for (const printed of [
    "",
    "HANDOFF_NOT_CURRENT_PHASE\n",
    "HANDOFF_NOT_CURRENT_PHASE\n\n",
    `\n${message}\n`,
    `HANDOFF_NOT_CURRENT_PHASE\n${message}`,
    `HANDOFF_NOT_CURRENT_PHASE\n${message}\nand a third line\n`,
  ])
    expect([printed, refusalIn(printed)]).toEqual([printed, undefined]);
});
