import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { lstat, mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { LEGION_ROLES, type LegionRole } from "@legion/contracts";
import { LEGION_PHASES, type LegionPhase } from "@legion/contracts/legion-api";
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential";
import { DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { fakeLatestHandoff } from "@legion/envoy-client/test/fake-dispatch";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import {
  versionControlEnvironment,
  versionControlIdentity,
  versionControlNames,
} from "@legion/test-fixtures/version-control-environment";
import {
  findHandoffReport,
  findPushedBranchHead,
  type HandoffReport,
  type JjRunner,
  type LatestHandoff,
  paneLatestHandoff,
  pathJj,
} from "./handoff-record";

const ISSUE = "LEGION-208";
const STANDING = "c0de0000000000000000000000000000000000ff";
const TIP = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
/** The head a handoff record names, the commit its phase's work was done against. It is not the
 * branch's tip `TIP`, so a report carrying it carries what the record says and not what jj
 * answered. */
const HEAD = "9f2c81d4e6b70a35c9d8e1f4a2b6038d5c7e9a14";
const REMOTE = 'remote_bookmarks(exact:"legion/LEGION-208", exact:"origin")';
/** A handoff body as a file-backed role writes it: its stamps and the head its work was done
 * against, which `head` may be left out of or given any shape. */
const body = (head?: unknown) =>
  JSON.stringify({ schemaVersion: 1, issue: ISSUE, ...(head === undefined ? {} : { head }) });
const RECORD = { id: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13", version: 3, body: body(HEAD) };

const env = { LEGION_WORKSPACE: "/workspaces/LEGION-208", LEGION_ISSUE: ISSUE };

/** A jj that answers the report's commands — the commit the workspace stands on, `STANDING`, and
 * the branch's tip at origin, `TIP`, when the branch carries `STANDING`, which it does unless
 * `pushed` is false — and records each call's args. */
function fakeJj(pushed = true): { jj: JjRunner; calls: string[][] } {
  const calls: string[][] = [];
  const jj: JjRunner = async (args, cwd) => {
    expect(cwd).toBe(env.LEGION_WORKSPACE);
    calls.push([...args]);
    const revision = args[2];
    if (revision === "@-") return STANDING;
    if (revision === `${REMOTE} & ${STANDING}::`) return pushed ? TIP : "";
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
  return { jj, calls };
}

/** A Dispatch whose every phase answers `record`, recording each read. */
const recordingDispatch = (record: typeof RECORD = RECORD) => fakeLatestHandoff(async () => record);

/** The implementer's record as a round that pushed its work writes it: its head is the commit the
 * workspace stands on. */
const PUSHED_RECORD = { ...RECORD, body: body(STANDING) };

test("the implementer's round reports the newest implement record and the pushed @- it names", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch(PUSHED_RECORD);
  await expect(
    findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
  ).resolves.toEqual({ commit: STANDING, handoffId: RECORD.id, handoffVersion: RECORD.version });
  expect(reads).toEqual(["LEGION-208 implement"]);
  // The commit reported is the head the record names, which is the commit the workspace stands
  // on, behind the tip the branch stands at. jj is asked what the workspace stands on and whether
  // the branch carries it, and never about the record's head.
  expect(calls).toEqual([
    ["log", "-r", "@-", "--no-graph", "-T", "commit_id"],
    ["log", "-r", `${REMOTE} & ${STANDING}::`, "--no-graph", "-T", "commit_id"],
  ]);
});

test("every file-backed role reports the head its phase's record names", async () => {
  for (const [phase, role, word] of [
    ["planning", "planner", "plan"],
    ["testing", "tester", "test"],
    ["reviewing", "reviewer", "review"],
  ] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      commit: HEAD,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });
    expect(reads).toEqual([`LEGION-208 ${word}`]);
    // The head comes from the record: these three ask the workspace nothing, since a sibling's
    // push moves the branch under a worker whose evidence is older.
    expect(calls).toEqual([]);
  }
});

test("a record naming no head, or one of another shape, leaves those three without a commit", async () => {
  // The record is the worker's own text, so a body this cannot read is no exception: the
  // completion goes without a commit, and the daemon refuses a behind-entry one of theirs.
  const unreadable = [
    body(),
    body(HEAD.slice(0, 39)),
    body(HEAD.toUpperCase()),
    body(`${HEAD} `),
    body(42),
    body(null),
    '["head"]',
    `"${HEAD}"`,
    "not json at all",
    "",
  ];
  for (const text of unreadable) {
    for (const [phase, role] of [
      ["planning", "planner"],
      ["testing", "tester"],
      ["reviewing", "reviewer"],
    ] as const) {
      const { jj, calls } = fakeJj();
      const { latestHandoff } = recordingDispatch({ ...RECORD, body: text });
      await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
        handoffId: RECORD.id,
        handoffVersion: RECORD.version,
      });
      expect(calls).toEqual([]);
    }
  }
});

test("every other completion reports the commit the workspace stands on, and reads no record", async () => {
  // Retro and the merger's READY write no record. The architect leaves no phase, so it reports @-
  // wherever its issue stands, in another role's file-backed phase too.
  for (const [phase, role] of [
    ["retro", "implementer"],
    ["merging", "merger"],
    ["admitted", "architect"],
    ["accepting", "architect"],
    ["implementing", "architect"],
    ["reviewing", "architect"],
  ] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      commit: STANDING,
    });
    expect(calls).toEqual([["log", "-r", "@-", "--no-graph", "-T", "commit_id"]]);
    expect(reads).toEqual([]);
  }
});

test("a reviewer whose review moved its issue on reports its own newest review record and the head it names", async () => {
  // A request for changes moves the issue to implementing at once, an approval to accepting once
  // the pull request is mergeable, and the reviewer completes after it: the daemon records that
  // completion on reviewing, the phase the reviewer left, and reads its review record back.
  for (const phase of ["implementing", "accepting"] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(
      findHandoffReport({ phase, role: "reviewer", env, jj, latestHandoff })
    ).resolves.toEqual({ commit: HEAD, handoffId: RECORD.id, handoffVersion: RECORD.version });
    expect(reads).toEqual(["LEGION-208 review"]);
    expect(calls).toEqual([]);
  }
});

test("a tester whose issue moved on to reviewing reports its own newest test record", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "reviewing", role: "tester", env, jj, latestHandoff })
  ).resolves.toEqual({ commit: HEAD, handoffId: RECORD.id, handoffVersion: RECORD.version });
  expect(reads).toEqual(["LEGION-208 test"]);
  expect(calls).toEqual([]);
});

/** The commit the issue's one shared workspace stands on once the issue has moved on: a sibling
 * role's (the tester commits there), which `legion/LEGION-208@origin` does not carry and no
 * record of another role names. */
const SIBLING = "c0de".repeat(10);

type HandoffWord = "plan" | "implement" | "test" | "review";

/** The newest record of every file-backed phase's word, each naming a head that is not `SIBLING`. */
const RECORDS: Readonly<Record<HandoffWord, { id: string; version: number; head: string }>> = {
  plan: { id: "plan-record", version: 1, head: "a1".repeat(20) },
  implement: { id: "implement-record", version: 4, head: "b2".repeat(20) },
  test: { id: "test-record", version: 2, head: "c3".repeat(20) },
  review: { id: "review-record", version: 3, head: "d4".repeat(20) },
};

/** What a completion sends beside its summary: the newest record of a phase's word (its id, its
 * version and the head its body names), `@-` (the commit the workspace stands on, and no record),
 * or nothing at all, thrown with words that say why (a part of them). */
type Sends = { readonly record: HandoffWord } | "@-" | { readonly thrown: string };

/** What the completion reads of the shared workspace: nothing, `@-` alone, or `@-` and then
 * whether `legion/<issue>@origin` carries it, the implementer's workspace check. */
type WorkspaceReads = "nothing" | "@-" | "@- and origin";

/** What the daemon answers the completion sent, from a finished role (the table's doc comment). */
type DaemonAnswer =
  | "200"
  | "409 HANDOFF_NOT_CURRENT_PHASE"
  | "409 HANDOFF_RECORD_REQUIRED"
  | "409 ACCEPTANCE_IS_SIGN_OFF"
  | "409 CHECK_LOOK_NOT_SERVED"
  | "404 ISSUE_NOT_FOUND"
  | "no request";

interface CompletionRow {
  readonly sends: Sends;
  readonly reads: WorkspaceReads;
  readonly daemon: DaemonAnswer;
}

const row = (sends: Sends, reads: WorkspaceReads, daemon: DaemonAnswer): CompletionRow => ({
  sends,
  reads,
  daemon,
});

/** The part of the push refusal the implementer's round meets in implementing, where its workspace
 * stands on a commit `legion/LEGION-208@origin` does not carry. */
const PUSH_IT = "is not on legion/LEGION-208@origin: push it";

/**
 * Every role's completion while the issue stands in each phase it can stand in, with the shared
 * workspace standing on `SIBLING` and Dispatch holding a record of every file-backed word.
 *
 * `sends` and `reads` are what `findHandoffReport` does, and the test holds it to them. `daemon`
 * documents the contract and is not asserted here: what the daemon answers that completion from a
 * finished role, one whose completion of the phase it is of was already taken, read from
 * `packages/daemon/internal`. `phase.Of(role, at)` is the phase a completion is of
 * (`completedPhase` mirrors it). Where it is `at`, the role works the issue's phase
 * (`phase.RoleFor(at)`), and the engine takes a completion naming a new record, or for retro and
 * merging a commit: 200. Otherwise workflow's `leftItsPhase` records a left phase's completion
 * only while the role's phases row holds no completion of it and some role works the issue's
 * phase, so a finished role's is answered `409 HANDOFF_NOT_CURRENT_PHASE`, and so is every one
 * while no role works the issue's phase (admitted, awaiting_person, held, and integrating, which
 * the Go daemon has no phase for). Three answers come first: the completion route answers any
 * completion of a done issue from a session working no look `409 CHECK_LOOK_NOT_SERVED` (the
 * `legion` tool never asks this module there), one of an issue it does not record (`unrecorded`)
 * `404 ISSUE_NOT_FOUND`, and the engine answers every architect's `409 ACCEPTANCE_IS_SIGN_OFF`,
 * since the architect accepts with sign_off. A row that sends nothing makes no request.
 */
const COMPLETIONS: Readonly<Record<LegionRole, Readonly<Record<LegionPhase, CompletionRow>>>> = {
  planner: {
    admitted: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    awaiting_person: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    planning: row({ record: "plan" }, "nothing", "200"),
    implementing: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    testing: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    reviewing: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    accepting: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    retro: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    merging: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    integrating: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    done: row({ record: "plan" }, "nothing", "409 CHECK_LOOK_NOT_SERVED"),
    held: row({ record: "plan" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    unrecorded: row({ record: "plan" }, "nothing", "404 ISSUE_NOT_FOUND"),
  },
  implementer: {
    admitted: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    awaiting_person: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    planning: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    implementing: row({ thrown: PUSH_IT }, "@- and origin", "no request"),
    testing: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    reviewing: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    accepting: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    retro: row("@-", "@-", "200"),
    merging: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    integrating: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    done: row({ record: "implement" }, "nothing", "409 CHECK_LOOK_NOT_SERVED"),
    held: row({ record: "implement" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    unrecorded: row({ record: "implement" }, "nothing", "404 ISSUE_NOT_FOUND"),
  },
  tester: {
    admitted: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    awaiting_person: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    planning: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    implementing: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    testing: row({ record: "test" }, "nothing", "200"),
    reviewing: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    accepting: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    retro: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    merging: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    integrating: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    done: row({ record: "test" }, "nothing", "409 CHECK_LOOK_NOT_SERVED"),
    held: row({ record: "test" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    unrecorded: row({ record: "test" }, "nothing", "404 ISSUE_NOT_FOUND"),
  },
  reviewer: {
    admitted: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    awaiting_person: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    planning: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    implementing: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    testing: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    reviewing: row({ record: "review" }, "nothing", "200"),
    accepting: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    retro: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    merging: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    integrating: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    done: row({ record: "review" }, "nothing", "409 CHECK_LOOK_NOT_SERVED"),
    held: row({ record: "review" }, "nothing", "409 HANDOFF_NOT_CURRENT_PHASE"),
    unrecorded: row({ record: "review" }, "nothing", "404 ISSUE_NOT_FOUND"),
  },
  merger: {
    admitted: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    awaiting_person: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    planning: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    implementing: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    testing: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    reviewing: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    accepting: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    retro: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    merging: row("@-", "@-", "200"),
    integrating: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    done: row("@-", "@-", "409 CHECK_LOOK_NOT_SERVED"),
    held: row("@-", "@-", "409 HANDOFF_NOT_CURRENT_PHASE"),
    unrecorded: row("@-", "@-", "404 ISSUE_NOT_FOUND"),
  },
  architect: {
    admitted: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    awaiting_person: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    planning: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    implementing: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    testing: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    reviewing: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    accepting: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    retro: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    merging: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    integrating: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    done: row("@-", "@-", "409 CHECK_LOOK_NOT_SERVED"),
    held: row("@-", "@-", "409 ACCEPTANCE_IS_SIGN_OFF"),
    unrecorded: row("@-", "@-", "404 ISSUE_NOT_FOUND"),
  },
};

/** The role that works each phase, as the daemon's `phase.RoleFor` answers; a phase absent here is
 * one no role works. */
const WORKER: Readonly<Partial<Record<LegionPhase, LegionRole>>> = {
  planning: "planner",
  implementing: "implementer",
  testing: "tester",
  reviewing: "reviewer",
  accepting: "architect",
  retro: "implementer",
  merging: "merger",
};

/** The shared workspace once the issue has moved on: it stands on `SIBLING`, which
 * `legion/LEGION-208@origin` does not carry. `asked` names each read, `@-` or `origin`. */
function siblingWorkspace(): { jj: JjRunner; asked: string[] } {
  const asked: string[] = [];
  const jj: JjRunner = async (args, cwd) => {
    expect(cwd).toBe(env.LEGION_WORKSPACE);
    if (args[2] === "@-") {
      asked.push("@-");
      return SIBLING;
    }
    if (args[2] === `${REMOTE} & ${SIBLING}::`) {
      asked.push("origin");
      return "";
    }
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
  return { jj, asked };
}

/** A Dispatch holding `RECORDS`, recording each read. */
function everyRecord(): { latestHandoff: LatestHandoff; reads: string[] } {
  const reads: string[] = [];
  const latestHandoff: LatestHandoff = async (issue, word) => {
    reads.push(`${issue} ${word}`);
    const { id, version, head } = RECORDS[word as HandoffWord];
    return { id, version, body: body(head) };
  };
  return { latestHandoff, reads };
}

/** A report as one line: the record it names and its commit, or the commit alone. */
function sentLine(report: HandoffReport): string {
  return report.handoffId === undefined
    ? `commit ${report.commit}`
    : `record ${report.handoffId} version ${report.handoffVersion}, commit ${report.commit}`;
}

/** Whether `report` sends the workspace's `@-`, `SIBLING`, beside a record: what the module doc's
 * left-phase rule says no completion of a phase its role left does. */
function standingBesideRecord(report: HandoffReport | Error): boolean {
  return !(report instanceof Error) && report.handoffId !== undefined && report.commit === SIBLING;
}

test("every role in every phase: a phase the issue has left runs no workspace check, and reads its head from its record", async () => {
  const wrong: string[] = [];
  let rows = 0;
  for (const role of LEGION_ROLES) {
    for (const phase of LEGION_PHASES) {
      rows += 1;
      const want = COMPLETIONS[role][phase];
      const { sends } = want;
      const at = `the ${role} at ${phase}`;
      // The module doc's left-phase rule, read off the table: a role whose phase the issue has
      // left runs no workspace check, and a left phase with a record reports that record and
      // reads nothing of the workspace. Dispatch holds a record naming a head for every word here,
      // so neither shape the rule sends `@-` alone for is a row of this table (LEFT_WITHOUT_HEAD
      // below holds them): `@-` is sent for a left phase only where that phase writes no record
      // (the merger's merging, the implementer's retro).
      if (WORKER[phase] !== role) {
        if (want.reads === "@- and origin") wrong.push(`${at} runs the workspace check`);
        if (typeof sends === "object" && "record" in sends && want.reads !== "nothing") {
          wrong.push(`${at} sends its ${sends.record} record and reads ${want.reads}`);
        }
      }
      const { jj, asked } = siblingWorkspace();
      const dispatch = everyRecord();
      const report = await findHandoffReport({
        phase,
        role,
        env,
        jj,
        latestHandoff: dispatch.latestHandoff,
      }).catch((error: Error) => error);
      const sent = report instanceof Error ? `nothing: ${report.message}` : sentLine(report);
      // The rule read off what was sent: no row sends the workspace's `@-` beside a left phase's
      // record.
      if (WORKER[phase] !== role && standingBesideRecord(report)) {
        wrong.push(`${at} sends the workspace's @- beside its record: ${sent}`);
      }
      const reads = asked.length === 0 ? "nothing" : asked.join(" and ");
      let wanted: string;
      let right: boolean;
      if (sends === "@-") {
        wanted = sentLine({ commit: SIBLING });
        right = sent === wanted && dispatch.reads.length === 0;
      } else if ("record" in sends) {
        const { id, version, head } = RECORDS[sends.record];
        wanted = sentLine({ commit: head, handoffId: id, handoffVersion: version });
        right = sent === wanted && dispatch.reads.join() === `LEGION-208 ${sends.record}`;
      } else {
        wanted = `nothing: …${sends.thrown}…`;
        right = sent.startsWith("nothing: ") && sent.includes(sends.thrown);
      }
      if (!right || reads !== want.reads) {
        wrong.push(
          `${at} sent ${sent}, reading ${reads} of the workspace and [${dispatch.reads.join(", ")}] of Dispatch; want ${wanted}, reading ${want.reads}`
        );
      }
    }
  }
  expect(rows).toBe(LEGION_ROLES.length * LEGION_PHASES.length);
  expect(wrong).toEqual([]);
});

/** What Dispatch holds of the implement phase the implementer left, in the two shapes the
 * left-phase rule sends `@-` alone for (the module doc): no record at all, or a newest record
 * whose body names no readable head. */
type LeftImplementRecord = "no record" | { readonly body: string };

interface LeftWithoutHeadRow {
  readonly record: LeftImplementRecord;
  /** Where the issue stands when the implementer completes. */
  readonly phase: LegionPhase;
  /** Whether the implementer's completion of implementing was taken: true for one whose
   * completion moved the issue on to testing, false for one that left implementing before it
   * completed (it asked to move the issue back to planning). */
  readonly completed: boolean;
  readonly daemon: DaemonAnswer;
}

/**
 * The implementer's completion of the implement phase it left, in each shape the left-phase rule
 * sends `@-` alone for, with the shared workspace standing on `SIBLING`. It is a second table
 * beside COMPLETIONS, whose role-by-phase key holds one Dispatch for every cell (a record naming a
 * head for every word), so it cannot hold these. Each shape has a row for an implementer that left
 * implementing before it completed and one whose completion of implementing was taken; a record
 * naming no head is one written before records carried `head`, or one that is not JSON at all.
 * Every row sends `@-` and no record, reading `@-` alone of the workspace. `daemon` documents the
 * contract, as COMPLETIONS' does, and is not asserted here: the workflow records a left phase's
 * completion only for a role whose row holds no completion of it (`leftItsPhase`), so it refuses
 * the unfinished implementer's, which it would record and which names no record,
 * `409 HANDOFF_RECORD_REQUIRED`, and answers the finished one's `409 HANDOFF_NOT_CURRENT_PHASE`;
 * neither is recorded. internal/api's handoff_test.go holds the route to both
 * (TestHandoffCompleteRefusesAnImplementersLeftCompletionNamingNoRecord).
 */
const LEFT_WITHOUT_HEAD: readonly LeftWithoutHeadRow[] = [
  {
    record: "no record",
    phase: "planning",
    completed: false,
    daemon: "409 HANDOFF_RECORD_REQUIRED",
  },
  {
    record: "no record",
    phase: "testing",
    completed: true,
    daemon: "409 HANDOFF_NOT_CURRENT_PHASE",
  },
  {
    record: { body: body() },
    phase: "planning",
    completed: false,
    daemon: "409 HANDOFF_RECORD_REQUIRED",
  },
  {
    record: { body: body() },
    phase: "testing",
    completed: true,
    daemon: "409 HANDOFF_NOT_CURRENT_PHASE",
  },
  {
    record: { body: "not json at all" },
    phase: "planning",
    completed: false,
    daemon: "409 HANDOFF_RECORD_REQUIRED",
  },
  {
    record: { body: "not json at all" },
    phase: "testing",
    completed: true,
    daemon: "409 HANDOFF_NOT_CURRENT_PHASE",
  },
];

test("an implementer whose left implement phase has no record, or a record naming no head, sends @- alone", async () => {
  const wrong: string[] = [];
  for (const { record, phase, completed } of LEFT_WITHOUT_HEAD) {
    const holds =
      record === "no record" ? "no implement record" : `an implement record of body ${record.body}`;
    const at = `the implementer at ${phase} ${completed ? "after its completion was taken" : "before it completed"}, with ${holds}`;
    const { jj, asked } = siblingWorkspace();
    const dispatch = fakeLatestHandoff(async () => {
      if (record === "no record") {
        throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, "no implement handoff");
      }
      const { id, version } = RECORDS.implement;
      return { id, version, body: record.body };
    });
    const report = await findHandoffReport({
      phase,
      role: "implementer",
      env,
      jj,
      latestHandoff: dispatch.latestHandoff,
    }).catch((error: Error) => error);
    const sent = report instanceof Error ? `nothing: ${report.message}` : sentLine(report);
    if (standingBesideRecord(report)) {
      wrong.push(`${at} sends the workspace's @- beside its record: ${sent}`);
    }
    const reads = asked.length === 0 ? "nothing" : asked.join(" and ");
    const wanted = sentLine({ commit: SIBLING });
    if (sent !== wanted || reads !== "@-" || dispatch.reads.join() !== "LEGION-208 implement") {
      wrong.push(
        `${at} sent ${sent}, reading ${reads} of the workspace and [${dispatch.reads.join(", ")}] of Dispatch; want ${wanted}, reading @-`
      );
    }
  }
  expect(wrong).toEqual([]);
});

test("no record of the phase the role works, or a Dispatch that does not answer, is refused naming what to do", async () => {
  const refusals: ReadonlyArray<readonly [() => Promise<never>, string]> = [
    [
      async () => {
        throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, "no handoff for test");
      },
      "LEGION-208 has no test handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase test --body-file <file>`, then complete again",
    ],
    [
      async () => {
        throw new DispatchServiceError("UNAUTHORIZED", 401, "invalid token");
      },
      "handoff_complete could not read LEGION-208's newest test handoff from Dispatch: 401 UNAUTHORIZED: invalid token; complete again once Dispatch answers",
    ],
    [
      async () => {
        throw new TypeError("Unable to connect. Is the computer able to access the url?");
      },
      "handoff_complete could not read LEGION-208's newest test handoff from Dispatch: Unable to connect. Is the computer able to access the url?; complete again once Dispatch answers",
    ],
  ];
  for (const [answer, message] of refusals) {
    const { jj } = fakeJj();
    const { latestHandoff } = fakeLatestHandoff(answer);
    await expect(
      findHandoffReport({ phase: "testing", role: "tester", env, jj, latestHandoff })
    ).rejects.toThrow(new Error(message));
  }
  // A phase the role left is refused the same way when Dispatch does not answer, or refuses the
  // read: only a record Dispatch says is not there is the daemon's to answer (the test below).
  for (const [answer, message] of refusals.slice(1)) {
    const { jj, calls } = fakeJj();
    const { latestHandoff } = fakeLatestHandoff(answer);
    await expect(
      findHandoffReport({ phase: "held", role: "tester", env, jj, latestHandoff })
    ).rejects.toThrow(new Error(message));
    expect(calls).toEqual([]);
  }
});

test("a phase the role left with no record reports @- and no record, for the daemon to answer", async () => {
  // A role on a held or an admitted issue can meet one: the daemon answers such a completion
  // HANDOFF_NOT_CURRENT_PHASE, or HANDOFF_RECORD_REQUIRED where it would record it, so the tool
  // does not tell the worker to write a record first. Only `@-` is read, and never the branch.
  for (const [phase, role, word] of [
    ["held", "tester", "test"],
    ["admitted", "planner", "plan"],
    ["testing", "implementer", "implement"],
    ["implementing", "reviewer", "review"],
  ] as const) {
    const { jj, calls } = fakeJj(false);
    const { latestHandoff, reads } = fakeLatestHandoff(async () => {
      throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, `no handoff for ${word}`);
    });
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      commit: STANDING,
    });
    expect(reads).toEqual([`LEGION-208 ${word}`]);
    expect(calls).toEqual([["log", "-r", "@-", "--no-graph", "-T", "commit_id"]]);
  }
});

test("an implementer whose @- is not on its branch at origin is refused naming the push and the record", async () => {
  // A round that forgot its push: its record names the branch's tip at origin, which the branch
  // carries, and its own commits are under `@-` alone; once it pushes `@-`, that record names a
  // commit the round's work is not. A round whose record already names its unpushed `@-` is
  // refused the same way, and the record it is shown is the one it wrote.
  for (const named of [TIP, STANDING]) {
    const { jj, calls } = fakeJj(false);
    const { latestHandoff } = recordingDispatch({ ...RECORD, body: body(named) });
    await expect(
      findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
    ).rejects.toThrow(
      new Error(
        `the commit this workspace stands on, ${STANDING}, is not on legion/LEGION-208@origin: push it with \`jj bookmark set legion/LEGION-208 -r @- && jj git push --bookmark legion/LEGION-208\`; the handoff's \`head\` must name that pushed commit, and the newest implement handoff of LEGION-208 (version 3) names ${named}, so when that is another commit, write the handoff again with \`head\` set to ${STANDING} (\`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\`); then complete again`
      )
    );
    expect(calls).toEqual([
      ["log", "-r", "@-", "--no-graph", "-T", "commit_id"],
      ["log", "-r", `${REMOTE} & ${STANDING}::`, "--no-graph", "-T", "commit_id"],
    ]);
  }
});

test("an implementer whose pushed @- its record does not name is refused naming the rewrite", async () => {
  // The forgotten push's next completion: `@-` is pushed now and the record still names the
  // branch's older tip, a head the branch carries and the round's work was not done against. A
  // head the branch does not carry at all is no better.
  for (const named of [TIP, HEAD]) {
    const { jj, calls } = fakeJj();
    const { latestHandoff } = recordingDispatch({ ...RECORD, body: body(named) });
    await expect(
      findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
    ).rejects.toThrow(
      new Error(
        `the newest implement handoff of LEGION-208 (version 3) names ${named} as its head, not the commit this workspace stands on, ${STANDING}, which legion/LEGION-208@origin carries: write the handoff again with \`head\` set to ${STANDING} (\`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\`), then complete again`
      )
    );
    expect(calls).toEqual([
      ["log", "-r", "@-", "--no-graph", "-T", "commit_id"],
      ["log", "-r", `${REMOTE} & ${STANDING}::`, "--no-graph", "-T", "commit_id"],
    ]);
  }
});

test("an implementer whose record names no readable head is refused naming what to write", async () => {
  for (const text of [body(), body("the tip"), "not json at all"]) {
    const { jj, calls } = fakeJj();
    const { latestHandoff } = recordingDispatch({ ...RECORD, body: text });
    await expect(
      findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
    ).rejects.toThrow(
      new Error(
        'the newest implement handoff of LEGION-208 (version 3) names no head: write the handoff again with `head` set to the 40-character lower-case commit you pushed as `@-`, as `jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T commit_id` prints it once the push succeeded (`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>`), then complete again'
      )
    );
    // Nothing of the workspace is read: the record is what has to change.
    expect(calls).toEqual([]);
  }
});

test("the workspace and a well-formed issue key are required before jj or Dispatch is asked", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch();
  await expect(
    findHandoffReport({
      phase: "retro",
      role: "implementer",
      env: { ...env, LEGION_WORKSPACE: undefined },
      jj,
      latestHandoff,
    })
  ).rejects.toThrow("LEGION_WORKSPACE is not set");
  await expect(
    findHandoffReport({
      phase: "implementing",
      role: "implementer",
      env: { ...env, LEGION_ISSUE: "LEGION-208#x" },
      jj,
      latestHandoff,
    })
  ).rejects.toThrow('LEGION_ISSUE "LEGION-208#x" is not a Dispatch issue key like LEGION-1');
  expect(calls).toEqual([]);
  expect(reads).toEqual([]);
});

test("a failing jj surfaces its command and stderr", async () => {
  const jj: JjRunner = async (args) => {
    throw new Error(`jj ${args.join(" ")} failed: Error: There is no jj repo in "."`);
  };
  const { latestHandoff } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "retro", role: "implementer", env, jj, latestHandoff })
  ).rejects.toThrow(
    'jj log -r @- --no-graph -T commit_id failed: Error: There is no jj repo in "."'
  );
});

// The head `ready_for_update` reports: the issue branch at the remote, read with the pane's jj.
test("the branch head is the issue bookmark at origin, and empty when the remote has no such branch", async () => {
  const calls: string[][] = [];
  const answer = async (head: string) => {
    const jj: JjRunner = async (args, cwd) => {
      expect(cwd).toBe(env.LEGION_WORKSPACE);
      calls.push([...args]);
      return head;
    };
    return findPushedBranchHead({ env, jj });
  };
  await expect(answer(TIP)).resolves.toBe(TIP);
  expect(calls).toEqual([["log", "-r", REMOTE, "--no-graph", "-T", "commit_id"]]);
  // A planner before its first push: no branch at the remote, and no error.
  await expect(answer("")).resolves.toBe("");

  // The issue key names the branch, and a worker sets its own environment.
  await expect(
    findPushedBranchHead({ env: { ...env, LEGION_ISSUE: "../x" }, jj: async () => TIP })
  ).rejects.toThrow(new Error('LEGION_ISSUE "../x" is not a Dispatch issue key like LEGION-1'));
  await expect(
    findPushedBranchHead({ env: { ...env, LEGION_WORKSPACE: "" }, jj: async () => TIP })
  ).rejects.toThrow(
    new Error(
      "LEGION_WORKSPACE is not set, so ready_for_update cannot find the branch head to report"
    )
  );
});

const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});
afterEach(forgetBrokerClients);

test("the pane's Dispatch reads the phase's newest record as the session's config names it", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-record-"));
  scratch.push(root);
  const file = { path: path.join(root, "envoy.json"), error: null } as const;
  const requests: Array<{ url: string; authorization: string | null }> = [];
  const answers: Response[] = [];
  const dispatch = (async (input, init) => {
    requests.push({
      url: input.toString(),
      authorization: new Headers(init?.headers).get("authorization"),
    });
    return answers.shift() ?? Response.json({ error: "unexpected" }, { status: 500 });
  }) as typeof fetch;
  const paneEnv = {
    ...env,
    DISPATCH_URL: "http://dispatch.test",
    DISPATCH_TOKEN: "pane-dispatch-token",
  };
  const latestHandoff = paneLatestHandoff(file, paneEnv, root, "pane-session", dispatch);
  const report = () =>
    findHandoffReport({ phase: "reviewing", role: "reviewer", env: paneEnv, latestHandoff });

  answers.push(
    Response.json({
      ...RECORD,
      issue_key: ISSUE,
      phase: "review",
      author: { kind: "agent", session_id: "ses_reviewer" },
      created_at: "2026-10-26T09:00:00Z",
      body: JSON.stringify({ schemaVersion: 1, phase: "review", decision: "approved", head: HEAD }),
    })
  );
  // The head travels in the record's body, from Dispatch's own answer to the completion.
  await expect(report()).resolves.toEqual({
    commit: HEAD,
    handoffId: RECORD.id,
    handoffVersion: RECORD.version,
  });
  expect(requests).toEqual([
    {
      url: "http://dispatch.test/api/v1/issues/LEGION-208/handoffs/review/latest",
      authorization: "Bearer pane-dispatch-token",
    },
  ]);

  // Dispatch's own 404 for a phase with none is the missing-record refusal.
  answers.push(
    Response.json(
      { error: "no review handoff on LEGION-208", code: "HANDOFF_NOT_FOUND" },
      { status: 404 }
    )
  );
  await expect(report()).rejects.toThrow(
    "LEGION-208 has no review handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase review --body-file <file>`, then complete again"
  );

  // A pane whose Dispatch is off says so, and asks nothing.
  const off = paneLatestHandoff(file, env, root, "pane-session", dispatch);
  await expect(
    findHandoffReport({ phase: "reviewing", role: "reviewer", env, latestHandoff: off })
  ).rejects.toThrow(
    /^handoff_complete could not read LEGION-208's newest review handoff from Dispatch: Dispatch is off for this pane: .*Dispatch is off; complete again once Dispatch answers$/
  );
  expect(requests).toHaveLength(2);
});

// The route the reviewer's `handoff_complete` failure came from: a pane whose Dispatch has no
// bearer at all and trades its own identity at the secrets broker. There is one credential field,
// so this pane presents the DPoP token the trade minted rather than an empty bearer.
test("a pane whose Dispatch is the broker's reads the record under its own session's token", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-broker-"));
  scratch.push(root);
  const file = { path: path.join(root, "envoy.json"), error: null } as const;
  const trades: URLSearchParams[] = [];
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      trades.push(new URLSearchParams(await request.text()));
      return Response.json({
        access_token: "broker-access-token",
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 3600,
      });
    },
  });
  const identityFile = path.join(root, "projected-token");
  await Bun.write(identityFile, "projected.service.account.token\n");
  const asked: Array<{ authorization: string | null; proof: string | null }> = [];
  const dispatch = (async (_input, init) => {
    const headers = new Headers(init?.headers);
    asked.push({
      authorization: headers.get("authorization"),
      proof: headers.get("dpop"),
    });
    return Response.json(RECORD);
  }) as typeof fetch;
  const paneEnv = {
    ...env,
    DISPATCH_URL: "http://dispatch.test",
    AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: identityFile,
  };
  try {
    const latest = paneLatestHandoff(file, paneEnv, root, "pane-session", dispatch);
    await latest("LEGION-208", "review");
  } finally {
    broker.stop(true);
  }

  expect(trades.map((form) => form.get("session"))).toEqual(["pane-session"]);
  expect(asked.map((one) => one.authorization)).toEqual(["DPoP broker-access-token"]);
  expect(asked[0]?.proof).toBeString();
});

// The revsets are jj's, so a fake cannot prove them: one real repository with a bare origin, the
// round's change committed and the branch pushed, run through PATH's jj, as a pane is. A host
// with no jj runs neither row and says so, once for the file. The jj the rows below run is the one
// this walk found, so no spawn of theirs walks PATH again.
const jjProgram = hostTool("jj");
const withoutJj = skipWithout("jj");

// `isolatedHome` puts these on the test process as well, so the jj that the code under test
// (`pathJj`) starts reads the test's home rather than the developer's.
restoreEnvironment(versionControlNames);

/**
 * A stand-in shared public leak check under `root`, whose path {@link isolatedHome} names: a
 * script that reads its standard input, as the real check does, so nothing is left in a pipe when
 * it exits, and clears the write it was asked about. It answers what a passing check answers, and
 * nothing else.
 *
 * The first of the two rows below pushes, and a push runs under whatever `git` is first on
 * `PATH`. In an issue pod that is Legion's wrapper (`/opt/legion/bin/git`), which points a push at
 * a hook that runs the leak guard, and the guard refuses a push it can resolve no shared check
 * for. With no check named, that push stops with `LEAK CHECK SETUP ERROR` and the row fails
 * before it tests anything, while the same row passes on a runner whose git has no such wrapper.
 * The stand-in makes the two one run.
 */
function passingLeakCheck(root: string): string {
  const check = path.join(root, "public-leak-check.sh");
  writeFileSync(check, "#!/bin/sh\ncat >/dev/null 2>&1\nexit 0\n", { mode: 0o700 });
  return check;
}

/**
 * The environment this file's real jj and git run in, and the one it puts on the test process: the
 * shared isolation fixture over a home under `root`, so neither the developer's
 * `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor the system gitconfig decides a result. It
 * reaches the test process too, because `pathJj` — the code under test — starts jj with the
 * process's own environment.
 *
 * The leak check is named for the children alone, not put on this process: it is a file under
 * `root`, which this file's own cleanup removes, and a name left on the process would outlive it
 * for whatever test file runs next.
 */
function isolatedHome(root: string): Record<string, string> {
  const isolated = versionControlEnvironment(path.join(root, "home"));
  Object.assign(process.env, isolated);
  return {
    PATH: process.env.PATH ?? "",
    ...isolated,
    PUBLIC_LEAK_CHECK: passingLeakCheck(root),
  };
}

/** Runs jj in `cwd` with the test's own environment, failing on a non-zero exit. */
async function jj(cwd: string, env: Record<string, string>, ...args: string[]): Promise<string> {
  const child = Bun.spawn([jjProgram ?? "jj", ...args], {
    cwd,
    env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

test.skipIf(withoutJj)(
  "against a real jj workspace, the implementer's report refuses an @- the branch does not carry and a record naming another commit, and names @- once it is pushed and recorded",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-record-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const childEnv = isolatedHome(root);
    const paneEnv = { ...versionControlIdentity, LEGION_WORKSPACE: workspace, LEGION_ISSUE: ISSUE };
    await Bun.spawn(["git", "init", "--bare", "--quiet", path.join(root, "origin.git")], {
      env: childEnv,
    }).exited;
    await jj(root, childEnv, "git", "init", workspace);
    await jj(workspace, childEnv, "git", "remote", "add", "origin", path.join(root, "origin.git"));
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    await jj(workspace, childEnv, "describe", "-m", "implement: the change");
    await jj(workspace, childEnv, "new");
    const change = await jj(
      workspace,
      childEnv,
      "log",
      "-r",
      "@-",
      "--no-graph",
      "-T",
      "commit_id"
    );
    expect(change).toMatch(/^[0-9a-f]{40}$/);
    // The head the implementer's record names, as that round wrote it.
    let head = change;
    const { latestHandoff } = fakeLatestHandoff(async () => ({ ...RECORD, body: body(head) }));
    const report = () =>
      findHandoffReport({
        phase: "implementing",
        role: "implementer",
        env: paneEnv,
        jj: pathJj,
        latestHandoff,
      });

    // Committed but not pushed: the branch does not exist at origin yet, so it does not carry the
    // commit the workspace stands on.
    await expect(report()).rejects.toThrow(
      `the commit this workspace stands on, ${change}, is not on legion/${ISSUE}@origin`
    );

    // Pushed: the head the record names, the commit the workspace stands on, is reported with the
    // record.
    await jj(workspace, childEnv, "bookmark", "set", `legion/${ISSUE}`, "-r", "@-");
    await jj(workspace, childEnv, "git", "push", "--bookmark", `legion/${ISSUE}`);
    await expect(report()).resolves.toEqual({
      commit: change,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });

    // A head this repository has never seen is the rewrite's refusal, not a jj error about a
    // revision: jj is asked about the commit the workspace stands on, never about the record's
    // head.
    head = "0123456789abcdef0123456789abcdef01234567";
    await expect(report()).rejects.toThrow(
      `the newest implement handoff of ${ISSUE} (version ${RECORD.version}) names ${head} as its head, not the commit this workspace stands on, ${change}, which legion/${ISSUE}@origin carries`
    );

    // A later commit not pushed yet is refused naming the push, whether the record names that
    // commit or, as a round that forgot the push writes it, the branch's tip at origin; the
    // refusal shows the head the record names. Once the push lands, the record still naming that
    // older tip is refused naming the rewrite: the branch carries its head, and the round's work
    // was done against the pushed commit. A push that moves the branch ahead of a worker's
    // evidence decides nothing for the three roles that read no workspace; the implementer's
    // round is the commit its workspace stands on, so its record is held to that commit, and a
    // record naming it is reported.
    await writeFile(path.join(workspace, "main.py"), 'print("bye")\n');
    await jj(workspace, childEnv, "describe", "-m", "implement: the fix");
    await jj(workspace, childEnv, "new");
    const fix = await jj(workspace, childEnv, "log", "-r", "@-", "--no-graph", "-T", "commit_id");
    head = fix;
    await expect(report()).rejects.toThrow(
      `the commit this workspace stands on, ${fix}, is not on legion/${ISSUE}@origin`
    );
    head = change;
    const forgotten = report();
    await expect(forgotten).rejects.toThrow(
      `the commit this workspace stands on, ${fix}, is not on legion/${ISSUE}@origin`
    );
    await expect(forgotten).rejects.toThrow(
      `names ${change}, so when that is another commit, write the handoff again with \`head\` set to ${fix}`
    );
    await jj(workspace, childEnv, "bookmark", "set", `legion/${ISSUE}`, "-r", "@-");
    await jj(workspace, childEnv, "git", "push", "--bookmark", `legion/${ISSUE}`);
    await expect(report()).rejects.toThrow(
      `the newest implement handoff of ${ISSUE} (version ${RECORD.version}) names ${change} as its head, not the commit this workspace stands on, ${fix}, which legion/${ISSUE}@origin carries`
    );
    head = fix;
    await expect(report()).resolves.toEqual({
      commit: fix,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });

    // Retro reports the commit the workspace stands on, whatever the record names: here the older
    // change.
    head = change;
    await expect(
      findHandoffReport({
        phase: "retro",
        role: "implementer",
        env: paneEnv,
        jj: pathJj,
        latestHandoff,
      })
    ).resolves.toEqual({ commit: fix });
  },
  60_000
);

// A git lock an interrupted git or jj left in the workspace fails every jj that snapshots a change
// into the working copy (jj rewrites the git index then). In a pod the roles container's launcher
// sweeps such a lock once it is more than 15 minutes old and no process of the container holds it
// open, so the note tells the role to run the command again; this test removes nothing either.
// The failure names the lock and its age, and the lock stays.
test.skipIf(withoutJj)(
  "a jj that fails on a stale git lock names it, its age, and when it goes",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-lock-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const childEnv = isolatedHome(root);
    await jj(root, childEnv, "git", "init", "--colocate", workspace);
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    const lock = path.join(workspace, ".git", "index.lock");
    await writeFile(lock, "");
    const written = new Date(Date.now() - 31 * 60 * 1000);
    await utimes(lock, written, written);

    const failure = pathJj(["diff", "-r", "@", "--name-only"], workspace);
    await expect(failure).rejects.toThrow("Could not acquire lock for index file");
    await expect(failure).rejects.toThrow(
      new RegExp(
        `git lock ${lock.replaceAll(/[.*+?^${}()|[\]\\]/g, "\\$&")} was last written 31m\\d+s ago and blocks this command\\. The launcher of the issue's pod removes a git lock .* Under the tmux runtime nothing removes it: .* Run the command again in about 16 minutes\\. If the lock is still there after that time, tell the issue's architect\\.`
      )
    );
    expect((await lstat(lock)).isFile()).toBe(true);
  },
  60_000
);
