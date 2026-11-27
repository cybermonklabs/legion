import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LEGION_PHASE_BACKWARD_TARGETS, type LegionPhase } from "@legion/contracts/legion-api";
import { namesMissing, namesWhole } from "@legion/test-fixtures/names-whole";
import { z } from "zod";
import { PlanRatingCases } from "../../contracts/src/plan-rating-cases";
import { stateAtPhase } from "../test/daemon-state";
import { createLegionDaemonClient } from "./daemon-client";
import { findHandoffReport, type HandoffReport, type StepCopy } from "./handoff-record";
import {
  type ClaimCapability,
  executePhaseOp,
  legionToolDescription,
  PHASE_OP_SENTENCES,
  PHASE_OPS,
  PhaseOpArguments,
  type PhaseOpField,
  type PhaseOpRuntime,
  phaseOpParameters,
  STALE_ENTRY_INSTRUCTION,
} from "./legion-phase-ops";

/** A contract the test's client states on every request, and the build's package names. */
const CONTRACT = 60;

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

const implementer: ClaimCapability = {
  sessionID: "ses_implementer",
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 5,
};

/** What the implementer's round reports in the pane: its record and its pushed tip. */
const REPORT = {
  commit: "a".repeat(40),
  handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
  handoffVersion: 2,
};

/** This build's own package, as a host passes it in (`PhaseOpRuntime.build`). */
const BUILD = { name: "@sjawhar/pi-legion", version: "18.14.1", pluginContract: CONTRACT } as const;

/** The pane's environment, as a host passes it in (`PhaseOpRuntime.env`): LEGION-208's own working
 * copy, laid out as the daemon lays one out, and its key. */
const PANE_ENV = {
  LEGION_WORKSPACE: "/state/workspaces/acme/widgets/legion-208",
  LEGION_ISSUE: "LEGION-208",
};

/** `/legion/v1/issues/<key>/record`, and the key it names. */
const RECORD_ROUTE = /^\/legion\/v1\/issues\/([^/]+)\/record$/;

/** The daemon's answer to the record route, over the issues of `state`: the issue's record, or
 * the 404 the daemon refuses an issue it records nothing of with. */
function recordAnswer(key: string, state: Record<string, unknown>): Response {
  const issues = state.issues as Record<string, unknown> | undefined;
  const record = issues?.[key];
  return record === undefined
    ? Response.json(
        { code: "RECORD_NOT_FOUND", error: `The daemon has no record for ${key}` },
        { status: 404 }
      )
    : Response.json(record);
}

/** What the implementer's retro reports in the pane: the commit alone, since a retro ends with no
 * handoff record and the daemon refuses a retro's completion that names one
 * (`HANDOFF_RECORD_MISMATCH`). */
const RETRO_REPORT = { commit: "c".repeat(40) };

/** A daemon refusal as its route answers it: the status, the code and the sentence. */
type DaemonRefusal = { readonly status: number; readonly code: string; readonly error: string };

/** A runtime over a stub daemon answering the golden fixtures, recording each route it is sent,
 * the bytes it answered each read with, and each body it is posted. `state` is what
 * `GET /legion/v1/state` answers, and the record route answers that state's issues;
 * `ratingRefusal`, when given, is what the daemon answers a completion that carries
 * `planRating`, and `report` what the pane reports for the phase it finds: `REPORT`, or the
 * commit alone that a phase with no handoff record reports (handoff-record's standingReport). */
function runtime(
  claim: ClaimCapability = implementer,
  state: Record<string, unknown> = daemonFixture("state.json"),
  ratingRefusal?: DaemonRefusal,
  report: HandoffReport = REPORT
) {
  const routes: string[] = [];
  const bytes: { readonly route: string; readonly bytes: number }[] = [];
  const posted: Record<string, unknown>[] = [];
  const lookups: string[] = [];
  const answers: Record<string, Record<string, unknown>> = {
    "/legion/v1/state": state,
    "/legion/v1/grants": daemonFixture("grant.json"),
    "/legion/v1/handoff/complete": daemonFixture("handoff-complete.json"),
  };
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    routes.push(route);
    const body: Record<string, unknown> | undefined =
      typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
    if (body !== undefined) posted.push(body);
    if (
      route === "/legion/v1/handoff/complete" &&
      ratingRefusal !== undefined &&
      body !== undefined &&
      "planRating" in body
    ) {
      return Response.json(
        { code: ratingRefusal.code, error: ratingRefusal.error },
        { status: ratingRefusal.status }
      );
    }
    const named = RECORD_ROUTE.exec(route);
    const answer =
      named === null
        ? Response.json(answers[route] ?? {})
        : recordAnswer(decodeURIComponent(named[1] ?? ""), state);
    bytes.push({ route, bytes: Buffer.byteLength(await answer.clone().text()) });
    return answer;
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async (step, role) => {
      lookups.push(`${step.phase} ${role}`);
      return report;
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
  return { ops, routes, bytes, posted, lookups };
}

const CHECK_OPENED = "2026-10-19T06:00:00.123456Z";

test("a look's report names its check and is posted as it is, with no record read", async () => {
  for (const phase of ["implementing", "done"] as const) {
    const state = stateAtPhase(phase);
    const { ops, routes, posted, lookups } = runtime({ ...implementer, role: "tester" }, state);
    await expect(
      executePhaseOp(ops, {
        op: "handoff_complete",
        summary: "still checking",
        verdict: "pending",
        wake_at: NEXT_LOOK,
        check_opened_at: CHECK_OPENED,
      })
    ).resolves.toEqual({});
    expect(routes).toEqual(["/legion/v1/grants", "/legion/v1/handoff/complete"]);
    expect(lookups).toEqual([]);
    expect(posted.at(-1)).toEqual({
      grantId: "grant-for-one-command",
      summary: "still checking",
      verdict: "pending",
      wakeAt: NEXT_LOOK,
      checkOpenedAt: CHECK_OPENED,
    });
  }
});

test("a call without check_opened_at is a step's completion on every phase", async () => {
  const state = stateAtPhase("done", checkRecord());
  const { ops, lookups, posted } = runtime({ ...implementer, role: "tester" }, state);
  await executePhaseOp(ops, { op: "handoff_complete", summary: "tests pass", verdict: "pass" });
  expect(lookups).toEqual(["done tester"]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "tests pass",
    verdict: "pass",
    ...REPORT,
  });
  for (const input of [{ verdict: "pending" }, { verdict: "pass", wake_at: NEXT_LOOK }]) {
    const fresh = runtime({ ...implementer, role: "tester" }, state);
    await expect(
      executePhaseOp(fresh.ops, { op: "handoff_complete", summary: "waiting", ...input })
    ).rejects.toThrow("check_opened_at");
    expect(fresh.routes).toEqual([]);
  }
});

test("a look's report refuses here only what no state decides", async () => {
  for (const fields of [
    {},
    { verdict: "fail", return_to: 1 },
    { verdict: "pass", wake_at: NEXT_LOOK },
  ]) {
    const { ops, routes } = runtime();
    await expect(
      executePhaseOp(ops, {
        op: "handoff_complete",
        summary: "looked",
        check_opened_at: CHECK_OPENED,
        ...fields,
      })
    ).rejects.toThrow();
    expect(routes).toEqual([]);
  }
  const { ops, posted } = closedRuntime(implementer, checkRecord({ last: true }), {
    status: 400,
    code: "CHECK_WAKE_UNEXPECTED",
    error: "no next look",
  });
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "looked",
      verdict: "pending",
      wake_at: NEXT_LOOK,
      check_opened_at: CHECK_OPENED,
    })
  ).rejects.toThrow("CHECK_WAKE_UNEXPECTED");
  expect(posted.at(-1)).toMatchObject({ checkOpenedAt: CHECK_OPENED, wakeAt: NEXT_LOOK });
});

test("a tester's step completion beside an open check of its role sends its report and return_to", async () => {
  const state = stateAtPhase("testing", checkRecord({ role: "tester" }));
  const { ops, posted, lookups } = runtime({ ...implementer, role: "tester" }, state);
  await executePhaseOp(ops, {
    op: "handoff_complete",
    summary: "failed",
    verdict: "fail",
    return_to: 1,
  });
  expect(lookups).toEqual(["testing tester"]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "failed",
    verdict: "fail",
    ...REPORT,
    returnTo: 1,
  });
});

test("the phase ops' fields, as a host declares them, are every field of every op, each optional, from the host's Zod", () => {
  const declared = z.toJSONSchema(z.object({ op: z.enum(PHASE_OPS), ...phaseOpParameters(z) }));
  expect(declared).toMatchObject({
    properties: {
      op: { enum: [...PHASE_OPS] },
      issue: { type: "string" },
      since: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "pending", "fail"] },
      wake_at: { type: "string" },
      plan_rating: { type: "integer", minimum: 1, maximum: 5 },
      plan_reason: { type: "string" },
      return_to: { type: "integer", minimum: 1 },
      to: { enum: [...LEGION_PHASE_BACKWARD_TARGETS] },
      reason: { type: "string" },
      task: { type: "string" },
      round: { type: "string" },
    },
    required: ["op"],
  });
  const fields = new Set(
    PhaseOpArguments.options
      .flatMap((option) => Object.keys(option.shape))
      .filter((f) => f !== "op")
  );
  expect(Object.keys(phaseOpParameters(z)).sort()).toEqual([...fields].sort());
});

// A host's own texts stand where the template puts them, and nothing else of the description
// moves: `before` right before handoff_complete's contract, `after` after that contract and after
// ready_for_update's part, which every host states, and the host's Envoy publish tool last. The
// texts and the tool's name are planted, so the case holds no word of the description.
test("the description sets the host's texts before and after handoff_complete's contract, and ends on the host's Envoy publish tool", () => {
  const before = "planted-before-7d31. ";
  const after = "planted-after-7d31. ";
  const publish = "planted_publish_7d31";
  const bare = legionToolDescription({ envoyPublish: publish });
  const placed = legionToolDescription({ envoyPublish: publish, before, after });
  expect(bare).not.toContain(before);
  expect(bare).not.toContain(after);
  // read_record and request_backward_move are operations only some hosts carry, so the template
  // names neither: a host that carries one names it in its own `before` or `after`.
  for (const op of ["read_record", "request_backward_move"]) {
    expect(namesWhole(bare, op), `the bare description names ${op}`).toBe(false);
  }
  const at = placed.indexOf(before);
  const later = placed.indexOf(after);
  expect(at, "the description holds no `before`").toBeGreaterThan(-1);
  expect(later, "the description holds no `after` after `before`").toBeGreaterThan(at);
  const without = placed.replace(before, "").replace(after, "");
  expect(without, "without the host's two texts it is not the bare description").toBe(bare);
  const contract = at + before.length;
  const op = placed.indexOf("handoff_complete", at);
  expect(op, "handoff_complete's contract does not follow `before`").toBe(contract);
  const update = placed.indexOf("ready_for_update", contract);
  expect(update, "ready_for_update's part is not after the contract").toBeGreaterThan(contract);
  expect(update, "ready_for_update's part is not before `after`").toBeLessThan(later);
  const updatePart = placed.slice(update, later);
  expect(namesWhole(updatePart, "`round`"), "ready_for_update's part names no `round`").toBe(true);
  const tool = placed.lastIndexOf(publish);
  expect(tool, "the Envoy publish tool is not named after `after`").toBeGreaterThan(later);
  expect(placed.slice(tool + publish.length)).toMatch(/^\W*$/);
});

// The daemon refuses a completion behind its phase entry HANDOFF_STALE_ENTRY and ends that refusal
// with an instruction of its own (`staleEntryInstruction` in
// packages/daemon/internal/api/handoff.go), and the tool composes the same instruction into its
// description, so a worker reads one instruction in its tool and in the refusal. The daemon's
// golden test (`packages/daemon/internal/api`, TestStaleEntryInstructionGolden) writes its code and
// its constant to `stale-entry.json`: this case holds the tool's constant to the daemon's, and the
// description to ending that code's clause, `HANDOFF_STALE_ENTRY (` up to its `), `, with the
// constant. It holds no word of the description.
test("the description ends HANDOFF_STALE_ENTRY's clause with the instruction the daemon's refusal ends with", () => {
  const { code, instruction } = daemonFixture("stale-entry.json") as {
    readonly code: string;
    readonly instruction: string;
  };
  expect(code).toBe("HANDOFF_STALE_ENTRY");
  expect(STALE_ENTRY_INSTRUCTION).toBe(instruction);
  const description = legionToolDescription({ envoyPublish: "envoy_publish" });
  const opens = description.indexOf(`${code} (`);
  expect(opens, `the description names no ${code} (`).toBeGreaterThan(-1);
  const clause = description.slice(opens, description.indexOf("), ", opens));
  expect(clause).toEndWith(STALE_ENTRY_INSTRUCTION);
});

test("each op refuses its arguments whole before any daemon request, naming every problem", async () => {
  const { ops, routes } = runtime();
  const refusals = [
    [{ op: "handoff_complete" }, "handoff_complete requires summary"],
    [{ op: "handoff_complete", summary: "  " }, "handoff_complete requires summary"],
    [
      { op: "handoff_complete", summary: "x", issue: 44 },
      "handoff_complete's issue is an issue key, as LEGION-208 is",
    ],
    [
      { op: "handoff_complete", summary: "x", verdict: "maybe" },
      "handoff_complete's verdict is pass, pending or fail",
    ],
    [
      { op: "handoff_complete", summary: "x", verdict: "pending", wake_at: " " },
      "handoff_complete requires wake_at",
    ],
    [
      { op: "handoff_complete", summary: "x", verdict: "pending", wake_at: "tomorrow" },
      "handoff_complete's wake_at is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
    [
      // Minute precision: Go's `time.Time` decode refuses it, so this tool refuses it too, by the
      // field's name rather than as the daemon's generic invalid-body answer (LegionInstant).
      { op: "handoff_complete", summary: "x", verdict: "pending", wake_at: "2026-11-06T10:00Z" },
      "handoff_complete's wake_at is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
    // The rating of the plan is a whole number from 1 to 5 with a reason the contract takes: a
    // string, not blank, and at most 2,000 characters counted as the daemon counts them. Each is
    // refused by its field's name, before the client's own parse of the request would refuse it
    // unnamed.
    [
      { op: "handoff_complete", summary: "x", plan_rating: 0, plan_reason: "r" },
      "handoff_complete's plan_rating is a whole number from 1 to 5",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_rating: 6, plan_reason: "r" },
      "handoff_complete's plan_rating is a whole number from 1 to 5",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_rating: 4.5, plan_reason: "r" },
      "handoff_complete's plan_rating is a whole number from 1 to 5",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_rating: "4", plan_reason: "r" },
      "handoff_complete's plan_rating is a whole number from 1 to 5",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_rating: 4, plan_reason: " \t\n" },
      "handoff_complete requires plan_reason",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_rating: 4, plan_reason: 5 },
      "handoff_complete requires plan_reason",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_rating: 4, plan_reason: "é".repeat(2001) },
      "handoff_complete's plan_reason holds at most 2,000 characters",
    ],
    // The two come together: one alone is refused naming the other.
    [
      { op: "handoff_complete", summary: "x", plan_rating: 4 },
      "handoff_complete names plan_rating without plan_reason",
    ],
    [
      { op: "handoff_complete", summary: "x", plan_reason: "The plan fit the work." },
      "handoff_complete names plan_reason without plan_rating",
    ],
    // A failed step names the step its task returns to by its place, 1 for its first, and only a
    // completion with the verdict fail names one (INVALID_RETURN_TO, RETURN_TO_NEEDS_FAIL).
    ...[0, -1, 1.5, "2"].map(
      (returnTo) =>
        [
          { op: "handoff_complete", summary: "x", verdict: "fail", return_to: returnTo },
          "handoff_complete's return_to is a whole number of 1 or more: the place of a step of your task, 1 for its first",
        ] as const
    ),
    ...[{ verdict: "pass" }, {}].map(
      (verdict) =>
        [
          { op: "handoff_complete", summary: "x", ...verdict, return_to: 1 },
          "handoff_complete names return_to without the verdict fail: only the completion of a step that failed, with the verdict fail, names the step its task returns to; send it again without return_to",
        ] as const
    ),
    // The pair is the schema's own rule, so it is named with the call's other problems at once.
    [
      { op: "handoff_complete", summary: " ", plan_rating: 4 },
      "handoff_complete requires summary; handoff_complete names plan_rating without plan_reason",
    ],
    [
      { op: "handoff_complete", summary: "x", phase: "plan", other: 1 },
      'handoff_complete does not accept field "phase"; handoff_complete does not accept field "other"',
    ],
    [{ op: "read_record", issue: 44 }, "read_record's issue is an issue key, as LEGION-208 is"],
    [{ op: "read_release_records" }, "read_release_records requires since"],
    [{ op: "read_release_records", since: " " }, "read_release_records requires since"],
    [
      { op: "read_release_records", since: "yesterday" },
      "read_release_records' since is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
    ],
    [
      { op: "read_release_records", since: "2026-11-01T12:00:00Z", issue: "LEGION-208" },
      'read_release_records does not accept field "issue"',
    ],
    [{ op: "request_backward_move", reason: "r" }, "request_backward_move requires to"],
    [{ op: "request_backward_move", to: " ", reason: "r" }, "request_backward_move requires to"],
    [{ op: "request_backward_move", to: 44, reason: "r" }, "request_backward_move requires to"],
    [
      { op: "request_backward_move", to: "implementing", reason: "r", issue: 44 },
      "request_backward_move's issue is an issue key, as LEGION-208 is",
    ],
    [
      { op: "request_backward_move" },
      "request_backward_move requires to; request_backward_move requires reason",
    ],
    [
      { op: "request_backward_move", to: "integrating", reason: "r" },
      `request_backward_move's to is one of ${LEGION_PHASE_BACKWARD_TARGETS.join(", ")}`,
    ],
    [
      { op: "set_status" },
      "op is one of read_record, read_release_records, handoff_complete, request_backward_move",
    ],
    [{}, "op is one of read_record, read_release_records, handoff_complete, request_backward_move"],
  ] as const;
  for (const [input, message] of refusals) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(message);
  }
  expect(routes).toEqual([]);
});

test("handoff_complete reads the issue's phase, finds the record and commit for it and the claim's role, then mints its grant and posts them", async () => {
  const { ops, routes, posted, lookups } = runtime();
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(lookups).toEqual(["implementing implementer"]);
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Done.",
    verdict: "pass",
    ...REPORT,
  });
});

// The agent that does the retro rates the plan the issue ran in the completion that ends the
// retro. Which completion that is, is the daemon's to say (PLAN_RATING_UNEXPECTED,
// PLAN_RATING_REQUIRED), so the tool sends the two fields as given, under the daemon's names.
test("the retro's completion sends plan_rating and plan_reason as planRating and planReason", async () => {
  const { ops, routes, posted, lookups } = runtime(
    implementer,
    stateAtPhase("retro"),
    undefined,
    RETRO_REPORT
  );
  const reason = "The tester found the bug the plan had left out.";
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Retro done.",
      plan_rating: 4,
      plan_reason: reason,
    })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(lookups).toEqual(["retro implementer"]);
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Retro done.",
    verdict: "",
    ...RETRO_REPORT,
    planRating: 4,
    planReason: reason,
  });
  // 2,000 characters of two UTF-16 units each is a reason of 2,000 characters as the daemon
  // counts them, and it is sent; so are both ends of the scale.
  for (const [rating, sent] of [
    [1, "😀".repeat(2000)],
    [5, "The plan fit the work."],
  ] as const) {
    await executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Retro done.",
      plan_rating: rating,
      plan_reason: sent,
    });
    expect(posted.at(-1)).toMatchObject({ planRating: rating, planReason: sent });
  }
});

// Which completion ends the retro is the daemon's to say from phase.Of, never from the issue's
// phase: the implementer's late completion of retro, once the issue has moved on to merging, is
// still of retro, so the daemon reads its rating (and its workflow then refuses it
// HANDOFF_NOT_CURRENT_PHASE, since the retro's completion already stands, and writes no rating of
// it), and a completion of implementing that carries one is the daemon's to refuse
// (PLAN_RATING_UNEXPECTED). So the tool sends a rating on a completion at any phase, since a filter
// on the record's phase would drop a rating the agent believes it gave.
test("a rating is sent on a completion at any phase: the late retro's at merging, and one at implementing that the daemon refuses PLAN_RATING_UNEXPECTED", async () => {
  // A retro writes no handoff record, so the pane reports the commit alone for it.
  const lateHead = "b".repeat(40);
  const late = runtime(implementer, stateAtPhase("merging"), undefined, { commit: lateHead });
  const lateReason = "The reviewer's two rounds were the test step the plan had left out.";
  await expect(
    executePhaseOp(late.ops, {
      op: "handoff_complete",
      summary: "Retro done.",
      plan_rating: 2,
      plan_reason: lateReason,
    })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(late.lookups).toEqual(["merging implementer"]);
  expect(late.posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Retro done.",
    verdict: "",
    commit: lateHead,
    planRating: 2,
    planReason: lateReason,
  });

  const early = runtime(implementer, stateAtPhase("implementing"), {
    status: 400,
    code: "PLAN_RATING_UNEXPECTED",
    error:
      "this completion of phase implementing of LEGION-208 names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded",
  });
  const earlyReason = "The plan named every file this round changed.";
  await expect(
    executePhaseOp(early.ops, {
      op: "handoff_complete",
      summary: "Implemented.",
      plan_rating: 3,
      plan_reason: earlyReason,
    })
  ).rejects.toThrow("400 PLAN_RATING_UNEXPECTED");
  expect(early.posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Implemented.",
    verdict: "",
    ...REPORT,
    planRating: 3,
    planReason: earlyReason,
  });
  // The same completion with no rating is taken, so the refusal above is the rating's.
  await expect(
    executePhaseOp(early.ops, { op: "handoff_complete", summary: "Implemented." })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(early.posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Implemented.",
    verdict: "",
    ...REPORT,
  });
});

// A rating's rule is the daemon's, and this tool states it again so that it refuses a rating by
// the field's name before any request. Every case the daemon's golden test answered
// (`plan-rating-cases.json`, internal/api's TestPlanRatingCasesGolden), parsed with the schema the
// contract's own test parses it with, goes through the executor as the retro's completion: the
// tool takes exactly the cases the daemon accepted and posts each rating and reason as it was
// given, and refuses every other case naming a rating field, with no request sent. Blank is Go's
// strings.TrimSpace on the daemon's side, not JavaScript's trim; `goBlankPattern`'s doc comment
// (`@legion/contracts/go-blank`) names the two characters on which the two differ.
test("the tool takes exactly the ratings the daemon takes, and sends a refused one nowhere", async () => {
  const { cases } = PlanRatingCases.parse(daemonFixture("plan-rating-cases.json"));
  const parted: string[] = [];
  for (const c of cases) {
    const { ops, routes, posted } = runtime(
      implementer,
      stateAtPhase("retro"),
      undefined,
      RETRO_REPORT
    );
    const rating = {
      ...(c.planRating === undefined ? {} : { plan_rating: c.planRating }),
      ...(c.planReason === undefined ? {} : { plan_reason: c.planReason }),
    };
    const refusal = await executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Retro done.",
      ...rating,
    }).then(
      () => undefined,
      (error: unknown) => (error instanceof Error ? error.message : String(error))
    );
    const told = `${c.name}: the daemon answers ${c.verdict}, the tool`;
    if (refusal === undefined) {
      const sent = posted.at(-1);
      const want = {
        grantId: "grant-for-one-command",
        summary: "Retro done.",
        verdict: "",
        ...RETRO_REPORT,
        planRating: c.planRating,
        planReason: c.planReason,
      };
      if (c.verdict !== "accepted") parted.push(`${told} took it`);
      else if (!Bun.deepEquals(sent, want)) parted.push(`${told} sent ${JSON.stringify(sent)}`);
    } else if (c.verdict === "accepted") parted.push(`${told} refused it: ${refusal}`);
    else if (routes.length > 0 || !/plan_rating|plan_reason/.test(refusal)) {
      parted.push(`${told} refused it after ${routes.join(", ") || "no request"}: ${refusal}`);
    }
  }
  expect(parted).toEqual([]);
});

// The description is where every host's agent reads what the completion that ends the retro
// carries: the two fields, the skill that states the rule, and the daemon's refusals of a rating,
// each by name. The scale's two numbers stand elsewhere in the description as well, so a check of
// them here could not fail by its cause; the refusals of 0 and 6 and the daemon's golden cases
// above hold the scale.
test("the description names the retro's rating fields, its skill and the daemon's refusals of a rating", () => {
  const description = legionToolDescription({ envoyPublish: "envoy_publish" });
  const missing = namesMissing(description, [
    "`plan_rating`",
    "`plan_reason`",
    "skill://legion-retro",
    "PLAN_RATING_UNEXPECTED",
    "PLAN_RATING_INPUT",
    "PLAN_RATING_REQUIRED",
  ]);
  expect(missing).toEqual([]);
});

// Contract 219: a tester's fail names the step its task returns to, `return_to`, which goes on the
// wire as the daemon's `returnTo`; a completion that names none sends none.
test("a tester's fail sends return_to as returnTo", async () => {
  const tester = { ...implementer, role: "tester" as const };
  const { ops, routes, posted, lookups } = runtime(tester, stateAtPhase("testing"));
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "The proof is thin.",
      verdict: "fail",
      return_to: 1,
    })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(lookups).toEqual(["testing tester"]);
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "The proof is thin.",
    verdict: "fail",
    ...REPORT,
    returnTo: 1,
  });
  await executePhaseOp(ops, { op: "handoff_complete", summary: "Red.", verdict: "fail" });
  expect(posted.at(-1)).not.toHaveProperty("returnTo");
});

/** The claim of a role of the deployment, `checker`, on LEGION-208 (contract 221). */
const checker: ClaimCapability = {
  ...implementer,
  role: "checker",
  claimToken: "legion-legion-legion-208-checker",
};

// Contract 221: a role of the deployment ends its step with its verdict and the commit its
// workspace stands on, `@-`; it writes no handoff record, so none is looked up in Dispatch.
test("a deployment role's completion sends its verdict and the commit it stands on, and no handoff record", async () => {
  const standing = "c".repeat(40);
  const looked: string[] = [];
  const { ops, routes, posted } = runtime(checker, stateAtPhase("step"));
  const pane: PhaseOpRuntime = {
    ...ops,
    handoffReport: (step, role) =>
      findHandoffReport({
        step,
        role,
        env: PANE_ENV,
        jj: async () => standing,
        latestHandoff: async (issue, word) => {
          looked.push(`${issue} ${word}`);
          throw new Error("a deployment role's step writes no handoff record");
        },
      }),
  };
  await expect(
    executePhaseOp(pane, { op: "handoff_complete", summary: "Checked.", verdict: "pass" })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(looked).toEqual([]);
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Checked.",
    verdict: "pass",
    commit: standing,
  });
});

// The daemon's record of LEGION-208 serves its tasks: b's review step is at work under the agent
// `reviewer` while the issue stands in implementing, the phase of its first running task. A
// completion by the session whose claim token ends in that agent is of b's step.
test("handoff_complete reports the step its claim's agent runs, as the issue's record serves it", async () => {
  const reviewer: ClaimCapability = {
    ...implementer,
    sessionID: "ses_reviewer",
    role: "reviewer",
    claimToken: "legion-legion-legion-208-reviewer",
  };
  const { ops, posted } = runtime(reviewer);
  const steps: StepCopy[] = [];
  await expect(
    executePhaseOp(
      {
        ...ops,
        handoffReport: async (step) => {
          steps.push(step);
          return REPORT;
        },
      },
      { op: "handoff_complete", summary: "Approved." }
    )
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  expect(steps).toEqual([
    {
      workspace: "/state/workspaces/acme/widgets/legion-208.b",
      bookmark: "legion/LEGION-208.b",
      phase: "reviewing",
    },
  ]);
  expect(posted.at(-1)).toMatchObject({ summary: "Approved.", ...REPORT });
});

// Agent k has a step that waits, task c's, and one at work, task d's; its update signal reports
// the head of d's branch.
test("ready_for_update reports the head of the branch of the step its claim's agent runs", async () => {
  const k: ClaimCapability = { ...implementer, claimToken: "legion-legion-legion-208-k" };
  const state = daemonFixture("state.json");
  const posted: { route: string; body: unknown }[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    posted.push({
      route,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    const named = RECORD_ROUTE.exec(route);
    if (named !== null) return recordAnswer(decodeURIComponent(named[1] ?? ""), state);
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const head = "4e8b1a359f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d";
  const steps: StepCopy[] = [];
  await expect(
    executePhaseOp(
      {
        daemon: createLegionDaemonClient("http://daemon.test", {
          pluginContract: CONTRACT,
          fetch: stub,
        }),
        claim: k,
        build: BUILD,
        env: PANE_ENV,
        handoffReport: async () => {
          throw new Error("no handoff report expected");
        },
        branchHead: async (step) => {
          steps.push(step);
          return head;
        },
      },
      { op: "ready_for_update", round: "plugin:45" }
    )
  ).resolves.toEqual({});
  expect(steps).toEqual([
    {
      workspace: "/state/workspaces/acme/widgets/legion-208.d",
      bookmark: "legion/LEGION-208.d",
      phase: "implementing",
    },
  ]);
  expect(posted.at(-1)).toEqual({
    route: "/legion/v1/claims/ready-for-update",
    body: {
      claimToken: "legion-legion-legion-208-k",
      sessionId: "ses_implementer",
      secret: "U2VjcmV0",
      generation: 5,
      round: "plugin:45",
      head,
    },
  });
});

test("ready_for_update signals without a head when the daemon does not answer the issue's record, and says so", async () => {
  const posted: string[] = [];
  const stub = (async (input) => {
    const route = new URL(input.toString()).pathname;
    posted.push(route);
    if (RECORD_ROUTE.test(route)) {
      return Response.json(
        { code: "RECORD_UNAVAILABLE", error: "could not read the issue record" },
        { status: 500 }
      );
    }
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const answer = (await executePhaseOp(
    {
      daemon: createLegionDaemonClient("http://daemon.test", {
        pluginContract: CONTRACT,
        fetch: stub,
      }),
      claim: implementer,
      build: BUILD,
      env: PANE_ENV,
      handoffReport: async () => {
        throw new Error("no handoff report expected");
      },
      branchHead: async () => {
        throw new Error("no branch-head lookup expected without a step");
      },
    },
    { op: "ready_for_update", round: "plugin:46" }
  )) as { note?: string };
  expect(answer.note).toStartWith(
    "The daemon has your ready_for_update signal. The signal reports no branch head, because the branch-head lookup failed."
  );
  expect(answer.note).toContain("could not read the issue record");
  expect(posted).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/claims/ready-for-update",
  ]);
});

// Contract 221: a move back may name the step it returns to, `return_to`, which goes on the wire
// as the daemon's `returnTo`, as a failed completion's does; a move that names none sends none.
test("request_backward_move sends return_to as returnTo", async () => {
  const { ops, routes, posted } = runtime(checker, stateAtPhase("step"));
  await expect(
    executePhaseOp(ops, {
      op: "request_backward_move",
      to: "step",
      reason: "The check needs the earlier checker's report again.",
      return_to: 2,
    })
  ).resolves.toEqual({});
  expect(routes).toEqual(["/legion/v1/grants", "/legion/v1/phase/backward"]);
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    to: "step",
    reason: "The check needs the earlier checker's report again.",
    returnTo: 2,
  });
  await executePhaseOp(ops, { op: "request_backward_move", to: "implementing", reason: "Red." });
  expect(posted.at(-1)).not.toHaveProperty("returnTo");
  await expect(
    executePhaseOp(ops, { op: "request_backward_move", to: "step", reason: "r", return_to: 0 })
  ).rejects.toThrow(
    "request_backward_move's return_to is a whole number of 1 or more: the place of a step of your task, 1 for its first"
  );
});

// Contract 224: a move back may name the task of the issue's plan it is of, `task`, which goes on
// the wire as the daemon's own `task`: an architect's names the task it returns from accepting
// where two or more tasks stand there, and a phase worker's may name the task of its own step. A
// move that names none sends none, and a blank one is refused before any request.
test("request_backward_move sends the task it names as task", async () => {
  const architect = runtime({ ...implementer, role: "architect" });
  await expect(
    executePhaseOp(architect.ops, {
      op: "request_backward_move",
      issue: "LEGION-208",
      to: "implementing",
      reason: "Task b's change misses its spec.",
      task: "b",
    })
  ).resolves.toEqual({});
  expect(architect.routes).toEqual(["/legion/v1/grants", "/legion/v1/phase/backward"]);
  expect(architect.posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    to: "implementing",
    reason: "Task b's change misses its spec.",
    issue: "LEGION-208",
    task: "b",
  });
  const worker = runtime();
  await executePhaseOp(worker.ops, {
    op: "request_backward_move",
    to: "implementing",
    reason: "Red.",
    task: " a ",
  });
  expect(worker.posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    to: "implementing",
    reason: "Red.",
    task: "a",
  });
  await executePhaseOp(worker.ops, {
    op: "request_backward_move",
    to: "implementing",
    reason: "Red.",
  });
  expect(worker.posted.at(-1)).not.toHaveProperty("task");
  const sent = worker.routes.length;
  for (const task of [" ", 7]) {
    await expect(
      executePhaseOp(worker.ops, {
        op: "request_backward_move",
        to: "implementing",
        reason: "r",
        task,
      })
    ).rejects.toThrow(
      "request_backward_move's task is the id of a task of the issue's plan, as a is"
    );
  }
  expect(worker.routes).toHaveLength(sent);
});

// Contract 221: the daemon takes a verdict from a role of the deployment too, and the one
// description every role reads says so.
test("the description gives a verdict to the tester or a role of the deployment", () => {
  expect(legionToolDescription({ envoyPublish: "envoy_publish" })).toContain(
    "`verdict` pass|fail from the tester or a role of the deployment on a phase completion"
  );
});

// The description is where the tester reads what its fail may carry and what the daemon refuses:
// the field and the daemon's three codes, each by name.
test("the description names return_to and the daemon's three refusals of it", () => {
  const description = legionToolDescription({ envoyPublish: "envoy_publish" });
  const missing = namesMissing(description, [
    "`return_to`",
    "RETURN_TO_NEEDS_FAIL",
    "INVALID_RETURN_TO",
    "RETURN_REFUSED",
  ]);
  expect(missing).toEqual([]);
});

// A host that carries request_backward_move or read_build states it in its own text with the
// sentence `PHASE_OP_SENTENCES` gives it. The move's sentence names the move's fields, each
// backticked, and accepting, the phase an architect moves its issue back from; the build's names
// null, the contract read_build answers where the daemon states none. Each name is held in its
// own sentence: a host's whole description names `issue` and accepting in other sentences too
// (read_record's and sign_off's), where a check of them could not fail.
test("request_backward_move's sentence names its fields and the phase an architect moves from, and read_build's names null", () => {
  const fields: readonly PhaseOpField[] = ["to", "reason", "issue"];
  const accepting: LegionPhase = "accepting";
  const move = [...fields.map((field) => `\`${field}\``), accepting];
  expect({
    move: namesMissing(PHASE_OP_SENTENCES.requestBackwardMove, move),
    build: namesMissing(PHASE_OP_SENTENCES.readBuild, ["null"]),
  }).toEqual({ move: [], build: [] });
});

// Workers named their own issue on handoff_complete and left it off read_record, and both were
// refused: 1,515 failed calls in the 48 h to 2026-11-10 17:40Z in INFRA's sessions alone
// (dispatch://INFRA-2824). A read that names no issue now reads the claim's own, a read that names
// one still reads that one, and a completion takes the claim's own issue and refuses any other.
test("read_record with no issue answers the claim's own record; handoff_complete takes its own issue and refuses another", async () => {
  const { ops, routes, posted } = runtime();
  const state = daemonFixture("state.json") as { issues: Record<string, unknown> };
  await expect(executePhaseOp(ops, { op: "read_record" })).resolves.toEqual({
    record: state.issues["LEGION-208"],
  });
  await expect(executePhaseOp(ops, { op: "read_record", issue: " " })).resolves.toEqual({
    record: state.issues["LEGION-208"],
  });
  for (const issue of ["LEGION-209", " LEGION-209\n"]) {
    await expect(executePhaseOp(ops, { op: "read_record", issue })).resolves.toEqual({
      record: state.issues["LEGION-209"],
    });
  }
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", issue: "LEGION-209" })
  ).rejects.toThrow(
    new Error(
      "handoff_complete reports this session's own issue LEGION-208; leave issue out, or name LEGION-208, not LEGION-209"
    )
  );
  // The refusal read no record and minted no grant: the four routes are the four reads', each
  // one of the record route the read names its issue on.
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-209/record",
    "/legion/v1/issues/LEGION-209/record",
  ]);
  expect(posted).toEqual([]);
  // A completion naming its own issue posts exactly what one leaving it out posts.
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" })
  ).resolves.toEqual(daemonFixture("handoff-complete.json"));
  const unnamed = posted.at(-1);
  expect(unnamed).toMatchObject({ summary: "Done.", verdict: "pass" });
  for (const issue of ["LEGION-208", " LEGION-208 "]) {
    await expect(
      executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass", issue })
    ).resolves.toEqual(daemonFixture("handoff-complete.json"));
    expect(posted.at(-1)).toEqual(unnamed);
  }
});

/** `state.json` with a key no schema names at four depths: the top level, one issue, one worker's
 * claim — the path INFRA-1048's incident took, where one key of a daemon a contract ahead
 * failed every `legion` call — and the locator below that claim. */
function stateWithKeysNoSchemaNames(): Record<string, unknown> {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;
  const issue = issues["LEGION-208"];
  const workers = issue.workers as Record<string, Record<string, unknown>>;
  const worker = workers.implementer;
  const claim = worker.claim as Record<string, unknown>;
  return {
    ...state,
    workerAdmission: { queue: ["LEGION-211"] },
    issues: {
      ...issues,
      "LEGION-208": {
        ...issue,
        escalation: { reason: "a field this build's contract has no name for" },
        workers: {
          ...workers,
          implementer: {
            ...worker,
            claim: {
              ...claim,
              lease: { holder: "daemon" },
              locator: { ...(claim.locator as Record<string, unknown>), zone: "us-east-1a" },
            },
          },
        },
      },
    },
  };
}

test("a record carrying keys no schema names at three depths is read, and read_record and handoff_complete hand none of them on", async () => {
  const golden = daemonFixture("state.json") as { issues: Record<string, unknown> };
  const { ops, routes } = runtime(implementer, stateWithKeysNoSchemaNames());

  // Awaited rather than `.resolves`, so a refusal of a key the reader must ignore is reported
  // with the path it named.
  expect(await executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" })).toEqual({
    record: golden.issues["LEGION-208"],
  });
  expect(
    await executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" })
  ).toEqual(daemonFixture("handoff-complete.json"));
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
});

test("a root architect's handoff_complete is refused as unavailable before its own fields are checked, after any field it never takes", async () => {
  const root = runtime({ ...implementer, role: "architect" });
  const refusal =
    "handoff_complete is not available to a root architect session, which runs no phase";
  for (const input of [
    { op: "handoff_complete" },
    { op: "handoff_complete", summary: "x" },
    { op: "handoff_complete", summary: " ", verdict: "maybe" },
  ]) {
    await expect(executePhaseOp(root.ops, input)).rejects.toThrow(new Error(refusal));
  }
  await expect(executePhaseOp(root.ops, { op: "handoff_complete", phase: "plan" })).rejects.toThrow(
    new Error(
      `handoff_complete does not accept field "phase"; it takes summary, issue, verdict, wake_at, check_opened_at, plan_rating, plan_reason, return_to; ${refusal}`
    )
  );
  expect(root.routes).toEqual([]);
});

// A refusal that named only the field refused left the agent to guess the call again. An
// architect sent a `reason` to an operation that took only `issue`, and a worker sent
// ready_for_update an `issue`. Each was told no more than that the field is refused. A refusal
// names, once after every field it refuses, the fields the operation takes.
test("a field an op does not take is refused naming the fields the op takes", async () => {
  const { ops, routes } = runtime();
  const refusals = [
    [
      { op: "ready_for_update", round: "r1", issue: "LEGION-208" },
      'ready_for_update does not accept field "issue"; it takes round',
    ],
    [
      { op: "read_record", issue: "LEGION-208", reason: "why" },
      'read_record does not accept field "reason"; it takes issue',
    ],
    [
      { op: "handoff_complete", summary: "x", phase: "plan", other: 1 },
      'handoff_complete does not accept field "phase"; handoff_complete does not accept field "other"; it takes summary, issue, verdict, wake_at, check_opened_at, plan_rating, plan_reason, return_to',
    ],
  ] as const;
  for (const [input, message] of refusals) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(new Error(message));
  }
  expect(routes).toEqual([]);
});

/** LEGION-208 closed by its merge, its production check open on its second look, as the daemon's
 * state shows it (`check` on the issue record): `role` is the role the check records as the one
 * that works it, and `last` false says a look follows this one. */
const openCheck = {
  repo: "acme/widgets",
  pullRequest: 1239,
  mergeCommit: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60",
  openedAt: "2026-09-22T08:40:12Z",
  role: "implementer",
  looks: 2,
  lookOpen: true,
  lastWake: {
    kind: "issue.deployed",
    detail: "run 38230245571 completed 2026-09-22T09:10:44Z",
    at: "2026-09-22T09:10:51Z",
  },
  nextWakeAt: "2026-10-06T08:40:12Z",
  last: false,
};

/** The record of a closed issue whose check is open, `fields` over the check above. */
const checkRecord = (fields: Record<string, unknown> = {}) => ({
  check: { ...openCheck, ...fields },
});

/** The time a pending report names for its next look. */
const NEXT_LOOK = "2026-09-29T09:00:00Z";

/** A runtime whose daemon shows LEGION-208 in `done` with `record` on it, recording each request
 * body posted. `refusal` is what the daemon answers the completion with, success when none. */
function closedRuntime(
  claim: ClaimCapability,
  record: Record<string, unknown>,
  refusal?: DaemonRefusal
) {
  const posted: Record<string, unknown>[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    if (route === "/legion/v1/issues/LEGION-208/record") {
      const { issues } = stateAtPhase("done", record) as { issues: Record<string, object> };
      return Response.json(issues["LEGION-208"]);
    }
    if (typeof init?.body === "string") posted.push(JSON.parse(init.body));
    if (route === "/legion/v1/grants") return Response.json(daemonFixture("grant.json"));
    if (route === "/legion/v1/handoff/complete" && refusal !== undefined) {
      return Response.json(
        { code: refusal.code, error: refusal.error },
        { status: refusal.status }
      );
    }
    return Response.json({});
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async (step, role) => {
      throw new Error(`no handoff report expected for the ${role} at ${step.phase}`);
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
  return { ops, posted };
}

test("a report on the production check the claim's own role works carries a verdict and no commit, and a pending its next look", async () => {
  const { ops, posted } = closedRuntime(implementer, checkRecord());
  // Each of the three verdicts is posted with a fresh grant, the summary and no commit: no
  // handoff was written and nothing pushed, so the lookup is never asked. The pending names the
  // next look, which travels as the daemon's own `wakeAt`.
  for (const verdict of ["pending", "pass", "fail"]) {
    const wake = verdict === "pending" ? { wake_at: NEXT_LOOK } : {};
    await expect(
      executePhaseOp(ops, {
        op: "handoff_complete",
        summary: `Look 2: ${verdict}.`,
        verdict,
        check_opened_at: CHECK_OPENED,
        ...wake,
      })
    ).resolves.toEqual({});
    expect(posted.at(-1)).toEqual({
      grantId: "grant-for-one-command",
      summary: `Look 2: ${verdict}.`,
      verdict,
      checkOpenedAt: CHECK_OPENED,
      ...(verdict === "pending" ? { wakeAt: NEXT_LOOK } : {}),
    });
  }
  expect(posted).toHaveLength(6);
});

test("a tester's pending report on its closed issue's check reaches the daemon, carrying the wake it named", async () => {
  const { ops, posted } = closedRuntime(
    { ...implementer, role: "tester" },
    checkRecord({ role: "tester" })
  );
  const summary = "Look 2: production does not carry the merge yet.";
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary,
      verdict: "pending",
      wake_at: NEXT_LOOK,
      check_opened_at: CHECK_OPENED,
    })
  ).resolves.toEqual({});
  expect(posted).toEqual([
    { sessionId: "ses_implementer", secret: "U2VjcmV0", tree: "LEGION-208", issue: "LEGION-208" },
    {
      grantId: "grant-for-one-command",
      summary,
      verdict: "pending",
      wakeAt: NEXT_LOOK,
      checkOpenedAt: CHECK_OPENED,
    },
  ]);
});

test("a named check absent from the record is still posted for the daemon's CHECK_NOT_OPEN", async () => {
  const { ops, posted } = closedRuntime(
    { ...implementer, role: "tester" },
    {},
    {
      status: 409,
      code: "CHECK_NOT_OPEN",
      error: "the named check is not open",
    }
  );
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Looked.",
      verdict: "pass",
      check_opened_at: CHECK_OPENED,
    })
  ).rejects.toThrow("409 CHECK_NOT_OPEN: the named check is not open");
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Looked.",
    verdict: "pass",
    checkOpenedAt: CHECK_OPENED,
  });
});

// A rating belongs to the completion that ends the retro alone, and a report on a production check
// is not that completion. The tool sends it all the same, so the daemon refuses it by its code,
// rather than the tool dropping a rating the agent believes it gave.
test("a rating on a production check's report is sent, and the daemon's PLAN_RATING_UNEXPECTED reaches the agent", async () => {
  const { ops, posted } = closedRuntime(implementer, checkRecord(), {
    status: 400,
    code: "PLAN_RATING_UNEXPECTED",
    error:
      "this report on look 2 of LEGION-208's production check names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded",
  });
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Look 2: pass.",
      verdict: "pass",
      check_opened_at: CHECK_OPENED,
      plan_rating: 4,
      plan_reason: "The plan fit the work.",
    })
  ).rejects.toThrow("400 PLAN_RATING_UNEXPECTED");
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Look 2: pass.",
    verdict: "pass",
    checkOpenedAt: CHECK_OPENED,
    planRating: 4,
    planReason: "The plan fit the work.",
  });
});

test("a named report reaches the daemon for its role refusal, without a record read", async () => {
  const { ops, posted } = closedRuntime({ ...implementer, role: "tester" }, checkRecord(), {
    status: 409,
    code: "CHECK_ROLE_MISMATCH",
    error: "only the check's role reports it",
  });
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Looked.",
      verdict: "pass",
      check_opened_at: CHECK_OPENED,
    })
  ).rejects.toThrow("409 CHECK_ROLE_MISMATCH");
  expect(posted).toHaveLength(2);
});

test("a check report's state-independent wrong shapes are refused before any call", async () => {
  const check = closedRuntime(implementer, checkRecord());
  for (const [input, message] of [
    [{ op: "handoff_complete", summary: "Looked." }, "names a verdict"],
    [
      { op: "handoff_complete", summary: "Looked.", verdict: "pass", wake_at: NEXT_LOOK },
      "no look follows a pass or fail",
    ],
    [
      { op: "handoff_complete", summary: "Looked.", verdict: "pending", wake_at: "tomorrow" },
      "wake_at is an RFC 3339 time",
    ],
    [
      { op: "handoff_complete", summary: "Looked.", verdict: "fail", return_to: 1 },
      "return_to names the step",
    ],
  ] as const) {
    await expect(
      executePhaseOp(check.ops, { ...input, check_opened_at: CHECK_OPENED })
    ).rejects.toThrow(message);
  }
  expect(check.posted).toEqual([]);
});

test("the daemon, not the record's last-look flag, decides whether a pending needs a wake", async () => {
  const { ops, posted } = closedRuntime(implementer, checkRecord(), {
    status: 400,
    code: "CHECK_WAKE_REQUIRED",
    error: "the next look needs wakeAt",
  });
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Looked.",
      verdict: "pending",
      check_opened_at: CHECK_OPENED,
    })
  ).rejects.toThrow("400 CHECK_WAKE_REQUIRED");
  expect(posted.at(-1)).toMatchObject({ verdict: "pending", checkOpenedAt: CHECK_OPENED });
});

test("the check's last look asks for no wake: a pending there is posted without one", async () => {
  const { ops, posted } = closedRuntime(implementer, checkRecord({ last: true }));
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Look 2: production never carried the merge.",
      verdict: "pending",
      check_opened_at: CHECK_OPENED,
    })
  ).resolves.toEqual({});
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    summary: "Look 2: production never carried the merge.",
    verdict: "pending",
    checkOpenedAt: CHECK_OPENED,
  });
});

test("pending without check_opened_at is refused before a record is read", async () => {
  const { ops, routes, lookups } = runtime({ ...implementer, role: "tester" });
  await expect(
    executePhaseOp(ops, { op: "handoff_complete", summary: "Waiting.", verdict: "pending" })
  ).rejects.toThrow(
    new Error(
      "handoff_complete's pending verdict reports a production check. Send check_opened_at with the time from the look task or check.openedAt from read_record. To complete a step, send pass or fail."
    )
  );
  expect(routes).toEqual([]);
  expect(lookups).toEqual([]);
});

test("wake_at is a check report's: a phase completion carrying one is refused, since the daemon reads none there", async () => {
  const { ops, routes, lookups } = runtime({ ...implementer, role: "tester" });
  await expect(
    executePhaseOp(ops, {
      op: "handoff_complete",
      summary: "Green.",
      verdict: "pass",
      wake_at: NEXT_LOOK,
    })
  ).rejects.toThrow(
    new Error(
      "handoff_complete's wake_at reports a production check. Send check_opened_at with the time from the look task or check.openedAt from read_record. A step's completion carries no wake."
    )
  );
  expect(routes).toEqual([]);
  expect(lookups).toEqual([]);
});

test("request_backward_move names the issue an architect returns from accepting, and only a phase worker's own issue", async () => {
  const posted: Record<string, unknown>[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    if (typeof init?.body === "string") posted.push(JSON.parse(init.body));
    if (route === "/legion/v1/grants") return Response.json(daemonFixture("grant.json"));
    return Response.json({});
  }) as typeof fetch;
  const runtimeFor = (claim: ClaimCapability): PhaseOpRuntime => ({
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async () => REPORT,
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  });
  // The tree's architect (its grant serves the whole tree) names the child it sends back; a
  // sub-architect's role is architect too.
  for (const architect of [
    { ...implementer, role: "architect" as const },
    { ...implementer, role: "architect" as const, issue: "LEGION-209" },
  ]) {
    await expect(
      executePhaseOp(runtimeFor(architect), {
        op: "request_backward_move",
        to: "testing",
        reason: "the proof is thin",
        issue: "LEGION-210",
      })
    ).resolves.toEqual({});
    expect(posted.at(-1)).toEqual({
      grantId: "grant-for-one-command",
      to: "testing",
      reason: "the proof is thin",
      issue: "LEGION-210",
    });
    await expect(
      executePhaseOp(runtimeFor(architect), {
        op: "request_backward_move",
        to: "testing",
        reason: "the proof is thin",
      })
    ).rejects.toThrow(new Error("request_backward_move requires issue"));
  }
  // A phase worker leaves the issue out, or names its own; another issue is refused before any
  // request, as the daemon would refuse it (ISSUE_NOT_GRANTS).
  const before = posted.length;
  await expect(
    executePhaseOp(runtimeFor(implementer), {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-208",
    })
  ).resolves.toEqual({});
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    to: "implementing",
    reason: "test failed",
  });
  await expect(
    executePhaseOp(runtimeFor(implementer), {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: " LEGION-208 ",
    })
  ).resolves.toEqual({});
  expect(posted.at(-1)).toEqual({
    grantId: "grant-for-one-command",
    to: "implementing",
    reason: "test failed",
  });
  await expect(
    executePhaseOp(runtimeFor(implementer), {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-209",
    })
  ).rejects.toThrow(
    new Error("a phase worker moves only its own issue LEGION-208, not LEGION-209")
  );
  expect(posted).toHaveLength(before + 4);
});

test("read_record answers the record of any issue the caller names, or the claim's own when it names none, and names an issue the daemon has none for; request_backward_move posts with a fresh grant", async () => {
  const { ops, routes } = runtime();
  const state = daemonFixture("state.json") as { issues: Record<string, unknown> };
  await expect(executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" })).resolves.toEqual({
    record: state.issues["LEGION-208"],
  });
  // The claim's own issue, which the daemon already told it: a worker reads the issue it works
  // far more often than another, and naming none is no refusal.
  await expect(executePhaseOp(ops, { op: "read_record" })).resolves.toEqual({
    record: state.issues["LEGION-208"],
  });
  await expect(executePhaseOp(ops, { op: "read_record", issue: "  " })).resolves.toEqual({
    record: state.issues["LEGION-208"],
  });
  await expect(
    executePhaseOp(
      { ...ops, claim: { ...implementer, issue: "LEGION-210" } },
      { op: "read_record" }
    )
  ).resolves.toEqual({ record: state.issues["LEGION-210"] });
  // An architect reads a child's record on the same route: the route answers whatever issue the
  // caller names, and the claim's own issue decides nothing here.
  const architect = runtime({ ...implementer, role: "architect" });
  await expect(
    executePhaseOp(architect.ops, { op: "read_record", issue: "LEGION-209" })
  ).resolves.toEqual({ record: state.issues["LEGION-209"] });
  expect(architect.routes).toEqual(["/legion/v1/issues/LEGION-209/record"]);
  // The route's own 404 for an issue the daemon records nothing of, in the words read_record
  // answered while it read the whole project.
  await expect(executePhaseOp(ops, { op: "read_record", issue: "LEGION-999" })).rejects.toThrow(
    "The daemon has no record for LEGION-999"
  );
  await expect(
    executePhaseOp(ops, {
      op: "request_backward_move",
      to: "planning",
      reason: "The plan misses X.",
    })
  ).resolves.toEqual({});
  expect(routes).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-210/record",
    "/legion/v1/issues/LEGION-999/record",
    "/legion/v1/grants",
    "/legion/v1/phase/backward",
  ]);
});

// The defect dispatch://INFRA-2694 reports: each of these readers asked for the whole project
// body, so a field changed on another issue's view refused a role's own call. The stub records
// every path each reader asks for and the bytes it answered with.
test("read_record and handoff_complete ask for their own issue's record and never for the project", async () => {
  const { ops, routes, bytes } = runtime();
  const project = Buffer.byteLength(JSON.stringify(daemonFixture("state.json")));

  await executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" });
  await executePhaseOp(ops, { op: "handoff_complete", summary: "Done.", verdict: "pass" });

  expect(routes).not.toContain("/legion/v1/state");
  const records = bytes.filter(({ route }) => route.endsWith("/record"));
  expect(records.map(({ route }) => route)).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/issues/LEGION-208/record",
  ]);
  for (const read of records) expect(read.bytes).toBeLessThan(project);
  console.log(
    `read_record and handoff_complete each read ${records.map(({ bytes: read }) => read).join(" and ")} bytes, where the whole project body is ${project} bytes`
  );
});

test("read_record hands on the place of an issue standing in the admission line", async () => {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;
  const admission = state.admission as Record<string, unknown>;
  const waitingLine = { place: 1, length: 1 };
  const { ops } = runtime(implementer, {
    ...state,
    admission: { ...admission, waiting: ["LEGION-212"] },
    issues: { ...issues, "LEGION-212": { ...issues["LEGION-212"], waitingLine } },
  });

  const standing = (await executePhaseOp(ops, { op: "read_record", issue: "LEGION-212" })) as {
    record: Record<string, unknown>;
  };
  expect(standing.record.waitingLine).toEqual(waitingLine);
  // The issue holding a slot stands in no line, so its record carries no place at all.
  const holding = (await executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" })) as {
    record: Record<string, unknown>;
  };
  expect(holding.record).not.toHaveProperty("waitingLine");
});

test("read_record hands on what a reviewing issue's round waits on from GitHub", async () => {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;
  const architect = {
    ...implementer,
    tree: "LEGION-219",
    issue: "LEGION-219",
    role: "architect" as const,
  };
  const { ops } = runtime(architect, state);

  const stuck = (await executePhaseOp(ops, { op: "read_record", issue: "LEGION-219" })) as {
    record: Record<string, unknown>;
  };
  expect(stuck.record.mergeWait).toEqual(issues["LEGION-219"].mergeWait);
  expect(stuck.record.mergeWait).toEqual({
    waitsOn: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
    mergeState: "behind",
    stateReadAt: "2126-09-22T09:16:12Z",
    headUpdatedAt: "2126-09-20T14:05:33Z",
    checkRuns: 2,
    toldHead: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
    toldState: "behind",
  });
  // LEGION-218's fixture holds no merge state, no read time and neither told mark.
  const first = (await executePhaseOp(ops, { op: "read_record", issue: "LEGION-218" })) as {
    record: Record<string, unknown>;
  };
  expect(first.record.mergeWait).toEqual({
    waitsOn: "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063",
    headUpdatedAt: "2126-09-22T08:58:40Z",
    checkRuns: 0,
  });
  // An issue whose round waits on nothing from GitHub has no such wait at all.
  const working = (await executePhaseOp(ops, { op: "read_record", issue: "LEGION-208" })) as {
    record: Record<string, unknown>;
  };
  expect(working.record).not.toHaveProperty("mergeWait");
});

// Contract 75: the daemon's state says why a claim failed and which phase a hold stopped, and
// both seats read them through the one parse this client makes (`LegionStateResponse`): a key
// the schema does not name is stripped before the agent sees it, so a record the daemon serves
// reaches a worker only because `packages/contracts/src/legion-api.ts` names it. The body here
// is the Go golden itself, not an object written beside it, so this row is red until the
// daemon's own fixture carries the keys.
test("a failed claim carries its cause", async () => {
  const { ops } = runtime();
  const golden = daemonFixture("state.json") as { issues: Record<string, Record<string, unknown>> };
  const failure = {
    cause: "died" as const,
    detail: "launch failures ran out",
    at: "2126-09-22T08:47:03Z",
    attempts: { launchFailures: 4, deaths: 1, promptFailures: 0, promptRetires: 0 },
  };

  const answered = (await executePhaseOp(ops, { op: "read_record", issue: "LEGION-210" })) as {
    record: Record<string, unknown>;
  };
  expect(answered.record).toEqual(golden.issues["LEGION-210"]);
  const held = answered.record as {
    heldFrom?: string;
    holdReason?: string;
    workers: Record<string, { claim: { failure?: Record<string, unknown> } }>;
  };
  expect(held.workers.implementer?.claim.failure).toEqual(failure);
  expect({ heldFrom: held.heldFrom, holdReason: held.holdReason }).toEqual({
    heldFrom: "implementing",
    holdReason: "escalated",
  });

  // The controller's read_state is this same body whole, the record beside the limits in
  // `settings` that its counts are read against.
  const state = await ops.daemon.state();
  expect(state.issues["LEGION-210"] as unknown).toEqual(golden.issues["LEGION-210"]);
  expect(state.issues["LEGION-210"]?.workers?.implementer?.claim?.failure).toEqual(failure);
  expect(state.settings).toMatchObject({
    launch_failure_limit: 4,
    prompt_failure_limit: 5,
    prompt_retire_limit: 3,
  });
});

/** A window the daemon served: three releases — one woken five minutes later, one woken three
 * hours later and one still released — three resumes of them, the last with no first turn, and
 * two holds of two reasons. */
const RELEASE_RECORDS = {
  releases: [
    {
      id: 1,
      issue: "LEGION-208",
      generation: 2,
      roles: ["implementer"],
      at: "2026-11-01T12:00:00Z",
      bundleBytes: 1024,
    },
    {
      id: 2,
      issue: "LEGION-209",
      generation: 1,
      roles: ["implementer", "tester"],
      at: "2026-11-01T13:00:00Z",
      bundleBytes: 0,
    },
    {
      id: 3,
      issue: "LEGION-210",
      generation: 1,
      roles: ["planner"],
      at: "2026-11-01T17:00:00Z",
      bundleBytes: 512,
    },
  ],
  resumes: [
    {
      id: 1,
      release: 1,
      issue: "LEGION-208",
      role: "implementer",
      kind: "task",
      at: "2026-11-01T12:05:00Z",
      podReadyAt: "2026-11-01T12:06:00Z",
      firstTurnAt: "2026-11-01T12:07:00Z",
      stuck: false,
    },
    {
      id: 2,
      release: 2,
      issue: "LEGION-209",
      role: "implementer",
      kind: "message",
      at: "2026-11-01T16:00:00Z",
      podReadyAt: "2026-11-01T16:02:00Z",
      firstTurnAt: "2026-11-01T16:06:00Z",
      stuck: false,
    },
    {
      id: 3,
      release: 2,
      issue: "LEGION-209",
      role: "tester",
      kind: "message",
      at: "2026-11-01T16:00:30Z",
      podReadyAt: "2026-11-01T16:02:00Z",
      stuck: true,
    },
  ],
  holds: [
    {
      id: 1,
      issue: "LEGION-211",
      reason: "background_work",
      detail: "async-job",
      firstAt: "2026-11-01T12:50:00Z",
      at: "2026-11-01T13:00:00Z",
      passes: 11,
    },
    {
      id: 2,
      issue: "LEGION-212",
      reason: "turn_in_progress",
      firstAt: "2026-11-01T13:10:00Z",
      at: "2026-11-01T13:10:00Z",
      passes: 1,
    },
  ],
};

/** A runtime whose daemon answers `records` on the release-records route and nothing else,
 * recording what was posted, with `asOf` as the clock an open release is measured against. */
function recordsRuntime(records: unknown, asOf = "2026-11-01T18:00:00Z") {
  const posted: { readonly path: string; readonly body: unknown }[] = [];
  const stub = (async (input, init) => {
    posted.push({
      path: new URL(input.toString()).pathname,
      body: init?.body == null ? undefined : JSON.parse(init.body.toString()),
    });
    return Response.json(records);
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim: implementer,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
    now: () => new Date(asOf),
  };
  return { ops, posted };
}

test("read_release_records posts the claim's own token and secret, and counts the window for a report", async () => {
  const { ops, posted } = recordsRuntime(RELEASE_RECORDS);

  await expect(
    executePhaseOp(ops, { op: "read_release_records", since: "2026-11-01T00:00:00Z" })
  ).resolves.toEqual({
    since: "2026-11-01T00:00:00Z",
    asOf: "2026-11-01T18:00:00.000Z",
    ...RELEASE_RECORDS,
    counts: {
      releases: 3,
      releasesSuspendingNoRole: 0,
      resumes: 3,
      resumesByKind: { task: 1, message: 2 },
      holds: 2,
      holdsByReason: { background_work: 1, turn_in_progress: 1 },
    },
    // Five minutes, three hours, and the hour the third release has been open at `now`.
    podHoursReleased: 4.08,
    wokenWithinTenMinutes: { releases: 1, share: 0.333 },
    // 120 and 360 seconds; the third resume has no first turn, so it is counted, not measured.
    wakeToFirstTurnSeconds: {
      measured: 2,
      withoutFirstTurn: 1,
      heldByPause: 0,
      median: 120,
      p90: 360,
      max: 360,
    },
  });
  expect(posted).toEqual([
    {
      path: "/legion/v1/claims/release-records",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        secret: "U2VjcmV0",
        since: "2026-11-01T00:00:00Z",
      },
    },
  ]);
});

test("a window with nothing in it counts as nothing, and measures no wake", async () => {
  const { ops } = recordsRuntime({ releases: [], resumes: [], holds: [] });

  await expect(
    executePhaseOp(ops, { op: "read_release_records", since: "2026-11-01T00:00:00Z" })
  ).resolves.toEqual({
    since: "2026-11-01T00:00:00Z",
    asOf: "2026-11-01T18:00:00.000Z",
    releases: [],
    resumes: [],
    holds: [],
    counts: {
      releases: 0,
      releasesSuspendingNoRole: 0,
      resumes: 0,
      resumesByKind: {},
      holds: 0,
      holdsByReason: {},
    },
    podHoursReleased: 0,
    wokenWithinTenMinutes: { releases: 0, share: 0 },
    wakeToFirstTurnSeconds: {
      measured: 0,
      withoutFirstTurn: 0,
      heldByPause: 0,
      median: null,
      p90: null,
      max: null,
    },
  });
});

/** A window with one release of each kind: one that suspended a role and was woken five minutes
 * later, and one that suspended no role, which no resume row can ever name. */
const ONE_RELEASE_OF_EACH_KIND = {
  releases: [
    {
      id: 1,
      issue: "LEGION-208",
      generation: 2,
      roles: ["implementer"],
      at: "2026-11-01T12:00:00Z",
      bundleBytes: 1024,
    },
    {
      id: 2,
      issue: "LEGION-209",
      generation: 1,
      roles: [],
      at: "2026-11-01T13:00:00Z",
      bundleBytes: 0,
    },
  ],
  resumes: [
    {
      id: 1,
      release: 1,
      issue: "LEGION-208",
      role: "implementer",
      kind: "task",
      at: "2026-11-01T12:05:00Z",
      podReadyAt: "2026-11-01T12:06:00Z",
      firstTurnAt: "2026-11-01T12:07:00Z",
      stuck: false,
    },
  ],
  holds: [],
};

test("a release that suspended no role is counted as a release and measured in nothing else", async () => {
  const { ops } = recordsRuntime(ONE_RELEASE_OF_EACH_KIND);

  await expect(
    executePhaseOp(ops, { op: "read_release_records", since: "2026-11-01T00:00:00Z" })
  ).resolves.toEqual({
    since: "2026-11-01T00:00:00Z",
    asOf: "2026-11-01T18:00:00.000Z",
    ...ONE_RELEASE_OF_EACH_KIND,
    counts: {
      releases: 2,
      releasesSuspendingNoRole: 1,
      resumes: 1,
      resumesByKind: { task: 1 },
      holds: 0,
      holdsByReason: {},
    },
    // The five minutes of the woken release alone, though the other has stood open five hours
    // at `now`: a release no wake can reach is charged nothing.
    podHoursReleased: 0.08,
    // One of one, not one of two: the release that suspended no role is in neither half.
    wokenWithinTenMinutes: { releases: 1, share: 1 },
    // The timings read the resume rows, which name the woken release alone.
    wakeToFirstTurnSeconds: {
      measured: 1,
      withoutFirstTurn: 0,
      heldByPause: 0,
      median: 120,
      p90: 120,
      max: 120,
    },
  });
});

// The window the daemon's own golden wrote (`release-records.json`): a release woken after half
// an hour, a release that suspended no role, a backstop wake with no first turn yet, and a wake
// a project pause held, which carries `pause` and pairs with no release. The held wake reached
// its first turn 31 minutes after it was taken, and those minutes are the pause's: it is counted
// as `heldByPause` and is in neither the measured seconds nor `withoutFirstTurn`, while the
// resume counts still name it. Its issue has no release in the window, so it ends none.
test("a wake a project pause held is counted apart and left out of the wake-to-first-turn seconds", async () => {
  const golden = daemonFixture("release-records.json");
  const { ops } = recordsRuntime(golden);

  await expect(
    executePhaseOp(ops, { op: "read_release_records", since: "2026-11-01T00:00:00Z" })
  ).resolves.toEqual({
    since: "2026-11-01T00:00:00Z",
    asOf: "2026-11-01T18:00:00.000Z",
    ...golden,
    counts: {
      releases: 2,
      releasesSuspendingNoRole: 1,
      resumes: 3,
      resumesByKind: { delivery: 1, backstop: 1, pause: 1 },
      holds: 2,
      holdsByReason: { background_work: 1, overtaken: 1 },
    },
    // The half hour of the one release a wake ended; the held wake's issue has no release.
    podHoursReleased: 0.5,
    wokenWithinTenMinutes: { releases: 0, share: 0 },
    // The delivery wake's 120 seconds alone. The held wake's 1860 are not among them.
    wakeToFirstTurnSeconds: {
      measured: 1,
      withoutFirstTurn: 1,
      heldByPause: 1,
      median: 120,
      p90: 120,
      max: 120,
    },
  });

  // A held wake with no first turn yet is held all the same: it is not one the pod is slow for.
  const resumes = golden.resumes as Record<string, unknown>[];
  const waiting = resumes.map((resume) =>
    resume.pause === undefined
      ? resume
      : { ...resume, podReadyAt: undefined, firstTurnAt: undefined }
  );
  const unturned = (await executePhaseOp(recordsRuntime({ ...golden, resumes: waiting }).ops, {
    op: "read_release_records",
    since: "2026-11-01T00:00:00Z",
  })) as { readonly wakeToFirstTurnSeconds: Record<string, unknown> };
  expect(unturned.wakeToFirstTurnSeconds).toEqual({
    measured: 1,
    withoutFirstTurn: 1,
    heldByPause: 1,
    median: 120,
    p90: 120,
    max: 120,
  });
});

// A pod released before a pause and woken during it. The wake is held, so its resume names no
// release (`release` 0), and no resume ever names that release: the role launches at the
// project's resume, on the held wake. The held wake ends the release all the same, at its own
// moment, so the release is charged to that wake and not to the moment of every later read, and
// it counts as woken, within ten minutes where the wake came that soon.
test("a wake a project pause held ends the releases of its issue that came before it", async () => {
  const release = (id: number, issue: string, at: string) => ({
    id,
    issue,
    generation: 1,
    roles: ["implementer", "tester"],
    at,
    bundleBytes: 0,
  });
  const heldWake = (id: number, issue: string, at: string) => ({
    id,
    release: 0,
    issue,
    role: "implementer",
    kind: "message",
    at,
    stuck: false,
    pause: 7,
  });
  const records = {
    releases: [
      // Woken six minutes later by a held wake, and by nothing else.
      release(1, "LEGION-208", "2026-11-01T12:00:00Z"),
      // Woken an hour later by a held wake, then again at 15:00 by a wake of its other role, which
      // names the release: the first of the two ends it.
      release(2, "LEGION-209", "2026-11-01T12:00:00Z"),
    ],
    resumes: [
      heldWake(1, "LEGION-208", "2026-11-01T12:06:00Z"),
      heldWake(2, "LEGION-209", "2026-11-01T13:00:00Z"),
      {
        id: 3,
        release: 2,
        issue: "LEGION-209",
        role: "tester",
        kind: "message",
        at: "2026-11-01T15:00:00Z",
        firstTurnAt: "2026-11-01T15:02:00Z",
        stuck: false,
      },
    ],
    holds: [],
  };
  const summary = async (window: unknown, asOf: string) =>
    (await executePhaseOp(recordsRuntime(window, asOf).ops, {
      op: "read_release_records",
      since: "2026-11-01T00:00:00Z",
    })) as {
      readonly podHoursReleased: number;
      readonly wokenWithinTenMinutes: { readonly releases: number; readonly share: number };
      readonly wakeToFirstTurnSeconds: Record<string, unknown>;
    };

  // Six minutes and one hour, at a read six hours after the releases and at one a day after
  // them: neither release is open, so neither grows with the clock.
  const atSixHours = await summary(records, "2026-11-01T18:00:00Z");
  const atADay = await summary(records, "2026-11-02T12:00:00Z");
  expect([atSixHours.podHoursReleased, atADay.podHoursReleased]).toEqual([1.1, 1.1]);
  // The first release was wanted again within ten minutes, and the second was not.
  expect(atSixHours.wokenWithinTenMinutes).toEqual({ releases: 1, share: 0.5 });
  // The held wakes are still measured in no wake-to-first-turn second.
  expect(atSixHours.wakeToFirstTurnSeconds).toEqual({
    measured: 1,
    withoutFirstTurn: 0,
    heldByPause: 2,
    median: 120,
    p90: 120,
    max: 120,
  });

  // A held wake ends no release that came after it: this pod was released an hour after the
  // wake, and nothing has woken it since, so it is open at the read, four hours later.
  const releasedAfter = {
    releases: [release(3, "LEGION-210", "2026-11-01T14:00:00Z")],
    resumes: [heldWake(4, "LEGION-210", "2026-11-01T13:00:00Z")],
    holds: [],
  };
  const open = await summary(releasedAfter, "2026-11-01T18:00:00Z");
  expect(open.podHoursReleased).toBe(4);
  expect(open.wokenWithinTenMinutes).toEqual({ releases: 0, share: 0 });

  // Nor does it end a release of another issue.
  const otherIssue = {
    releases: [release(5, "LEGION-211", "2026-11-01T12:00:00Z")],
    resumes: [heldWake(6, "LEGION-212", "2026-11-01T12:06:00Z")],
    holds: [],
  };
  expect((await summary(otherIssue, "2026-11-01T18:00:00Z")).podHoursReleased).toBe(6);
});

test("the pod hours of a release that suspended no role do not grow with the clock", async () => {
  const records = {
    releases: [
      {
        id: 1,
        issue: "LEGION-208",
        generation: 2,
        roles: [],
        at: "2026-11-01T12:00:00Z",
        bundleBytes: 0,
      },
    ],
    resumes: [],
    holds: [],
  };
  const summary = async (asOf: string) =>
    (await executePhaseOp(recordsRuntime(records, asOf).ops, {
      op: "read_release_records",
      since: "2026-11-01T00:00:00Z",
    })) as {
      readonly podHoursReleased: number;
      readonly counts: Record<string, number>;
      readonly wokenWithinTenMinutes: { readonly releases: number; readonly share: number };
    };

  // Six hours after the release, then a day after it. An unresumed release that suspended a role
  // is charged to the moment of the read, so charging this one would report more hours at every
  // later read, for ever, though an operator's retry may have rebuilt the pod a minute later.
  const atSixHours = await summary("2026-11-01T18:00:00Z");
  const atADay = await summary("2026-11-02T12:00:00Z");
  expect([atSixHours.podHoursReleased, atADay.podHoursReleased]).toEqual([0, 0]);
  expect(atSixHours.counts).toMatchObject({ releases: 1, releasesSuspendingNoRole: 1 });
  expect(atSixHours.wokenWithinTenMinutes).toEqual({ releases: 0, share: 0 });
});

test("ready_for_update answers for the round its task named, with the branch head, and leaves the head out when the branch is not on the remote", async () => {
  const posted: { route: string; body: unknown }[] = [];
  const state = daemonFixture("state.json");
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    posted.push({
      route,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    const named = RECORD_ROUTE.exec(route);
    if (named !== null) return recordAnswer(decodeURIComponent(named[1] ?? ""), state);
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const head = "9f1c0a7d4b2e6f8a3c5d1e7b9a0f2c4d6e8b1a35";
  const round = "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22";
  const runtimeFor = (branchHead: (step: StepCopy) => Promise<string>): PhaseOpRuntime => ({
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim: implementer,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead,
  });
  // The claim authenticates the signal, so no grant is minted: the requests are the read of the
  // issue's record, whose step names the branch, and the signal. The round is the agent's own
  // word, read from the update message it answers, and it travels as it was given.
  await expect(
    executePhaseOp(
      runtimeFor(async () => head),
      { op: "ready_for_update", round }
    )
  ).resolves.toEqual({});
  expect(posted).toEqual([
    { route: "/legion/v1/issues/LEGION-208/record", body: undefined },
    {
      route: "/legion/v1/claims/ready-for-update",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        sessionId: "ses_implementer",
        secret: "U2VjcmV0",
        generation: 5,
        round,
        head,
      },
    },
  ]);

  // A branch no push has created yet (a planner's) carries no head at all.
  await expect(
    executePhaseOp(
      runtimeFor(async () => ""),
      { op: "ready_for_update", round }
    )
  ).resolves.toEqual({});
  expect(posted.at(-1)?.body).toEqual({
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer",
    secret: "U2VjcmV0",
    generation: 5,
    round,
  });

  // The round is the one field the agent passes: the head is the plugin's to resolve.
  await expect(
    executePhaseOp(
      runtimeFor(async () => head),
      { op: "ready_for_update", round, head }
    )
  ).rejects.toThrow(new Error('ready_for_update does not accept field "head"; it takes round'));
  expect(posted).toHaveLength(4);
});

// The answer counts for the round the worker read and for no other, so the daemon refuses one
// naming another round (`supervise.ErrForeignUpdateRound`). A call that names no round at all
// cannot be sent: the daemon would have nothing to hold the answer to, and the signal would be
// filed against whatever round the claim happens to stand in. It is refused here instead, before
// any daemon request.
test("ready_for_update with no round is refused before any daemon request", async () => {
  const posted: string[] = [];
  const stub = (async (input) => {
    posted.push(new URL(input.toString()).pathname);
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const ops: PhaseOpRuntime = {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim: implementer,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
  for (const input of [{ op: "ready_for_update" }, { op: "ready_for_update", round: "  " }]) {
    await expect(executePhaseOp(ops, input)).rejects.toThrow(
      new Error("ready_for_update requires round")
    );
  }
  expect(posted).toEqual([]);
});

test("ready_for_update signals without a head when the branch-head lookup fails, and says so", async () => {
  const posted: { route: string; body: unknown }[] = [];
  const state = daemonFixture("state.json");
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    posted.push({
      route,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    const named = RECORD_ROUTE.exec(route);
    if (named !== null) return recordAnswer(decodeURIComponent(named[1] ?? ""), state);
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const failure =
    'jj log -r remote_bookmarks(exact:"legion/LEGION-208", exact:"origin") failed: Error: The lockfile at \'/workspace/.jj/working_copy/working_copy.lock\' might need manual deletion';
  // A workspace whose jj cannot answer still signals: the round waits for every claim of the pod,
  // and the daemon verifies no head. The answer carries the lookup's own message.
  await expect(
    executePhaseOp(
      {
        daemon: createLegionDaemonClient("http://daemon.test", {
          pluginContract: CONTRACT,
          fetch: stub,
        }),
        claim: implementer,
        build: BUILD,
        env: PANE_ENV,
        handoffReport: async () => {
          throw new Error("no handoff report expected");
        },
        branchHead: async () => {
          throw new Error(failure);
        },
      },
      { op: "ready_for_update", round: "plugin:44" }
    )
  ).resolves.toEqual({
    note:
      "The daemon has your ready_for_update signal. The signal reports no branch head, because " +
      "the branch-head lookup failed. The daemon checks no head, so your signal counts without " +
      `one. Repair what the lookup's message names before your next jj command. The lookup said: ${failure}`,
  });
  expect(posted).toEqual([
    { route: "/legion/v1/issues/LEGION-208/record", body: undefined },
    {
      route: "/legion/v1/claims/ready-for-update",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        sessionId: "ses_implementer",
        secret: "U2VjcmV0",
        generation: 5,
        round: "plugin:44",
      },
    },
  ]);
});

/** The phase-op runtime over `stub`, which is the only thing a `read_build` case needs of a
 * daemon: no report and no branch head is looked up. */
function opsOver(stub: typeof fetch): PhaseOpRuntime {
  return {
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: stub,
    }),
    claim: implementer,
    build: BUILD,
    env: PANE_ENV,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead: async () => {
      throw new Error("no branch-head lookup expected");
    },
  };
}

/** A daemon answering the health route alone, stating `contract` on it, and recording every
 * route it is asked for. A daemon that states none answers without the header. */
function healthDaemon(contract: number | undefined) {
  const routes: string[] = [];
  const stub = (async (input) => {
    routes.push(new URL(input.toString()).pathname);
    return new Response(JSON.stringify({ status: "ok", project: "legion" }), {
      headers: {
        "Content-Type": "application/json",
        ...(contract === undefined ? {} : { "Legion-Daemon-Contract": String(contract) }),
      },
    });
  }) as typeof fetch;
  return { ops: opsOver(stub), routes };
}

/** A daemon that does not answer the health route: one that refuses the request with a status
 * and a sentence, and one that is down, whose request fails in the transport. */
function unansweringDaemon(how: "refuses" | "is down") {
  const routes: string[] = [];
  const stub = (async (input) => {
    routes.push(new URL(input.toString()).pathname);
    if (how === "is down")
      throw new TypeError("Unable to connect. Is the computer able to access the url?");
    return Response.json({ error: "the daemon is restarting" }, { status: 503 });
  }) as typeof fetch;
  return { ops: opsOver(stub), routes };
}

test("read_build answers the build this process loaded and the contract the daemon states, and reads no project state", async () => {
  const { ops, routes } = healthDaemon(CONTRACT);

  await expect(executePhaseOp(ops, { op: "read_build" })).resolves.toEqual({
    plugin: BUILD.name,
    version: BUILD.version,
    pluginContract: CONTRACT,
    daemonContract: CONTRACT,
    daemonContractNote: null,
  });
  expect(routes).toEqual(["/healthz"]);
});

test("read_build shows both numbers where the two builds disagree, and says so where the daemon states none", async () => {
  const ahead = healthDaemon(CONTRACT + 1);
  await expect(executePhaseOp(ahead.ops, { op: "read_build" })).resolves.toMatchObject({
    pluginContract: CONTRACT,
    daemonContract: CONTRACT + 1,
    daemonContractNote: null,
  });

  const behind = healthDaemon(CONTRACT - 1);
  await expect(executePhaseOp(behind.ops, { op: "read_build" })).resolves.toMatchObject({
    pluginContract: CONTRACT,
    daemonContract: CONTRACT - 1,
    daemonContractNote: null,
  });

  const silent = healthDaemon(undefined);
  await expect(executePhaseOp(silent.ops, { op: "read_build" })).resolves.toMatchObject({
    pluginContract: CONTRACT,
    daemonContract: null,
    daemonContractNote: "the daemon stated no contract",
  });
  expect(silent.routes).toEqual(["/healthz"]);
});

test("read_build still answers this build where the daemon refuses the health route or is down", async () => {
  // The three local values need no request, and a role asked which build it runs is most often
  // asked while the daemon is the thing that is wrong. The note says which of the two happened:
  // the daemon answered the route and refused it, or no answer reached the process at all.
  const notes = {
    refuses: "the daemon refused the health route",
    "is down": "the daemon did not answer at all",
  } as const;
  for (const how of ["refuses", "is down"] as const) {
    const { ops, routes } = unansweringDaemon(how);
    await expect(executePhaseOp(ops, { op: "read_build" })).resolves.toEqual({
      plugin: BUILD.name,
      version: BUILD.version,
      pluginContract: CONTRACT,
      daemonContract: null,
      daemonContractNote: notes[how],
    });
    expect(routes).toEqual(["/healthz"]);
  }
});

test("read_build takes no field, and refuses one it is given before any request", async () => {
  const { ops, routes } = healthDaemon(CONTRACT);

  await expect(executePhaseOp(ops, { op: "read_build", issue: "LEGION-208" })).rejects.toThrow(
    'read_build does not accept field "issue"'
  );
  expect(routes).toEqual([]);
});
