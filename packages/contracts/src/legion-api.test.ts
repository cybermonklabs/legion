import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { z } from "zod";
import {
  type ContractRecords,
  type ContractShape,
  composeEntries,
  daemonApiRecords,
  entryFixtures,
} from "./daemon-api-records";
import * as legionApi from "./legion-api";
import {
  LEGION_FAILURE_CAUSES,
  LEGION_HELD_REASONS,
  LEGION_PHASE_BACKWARD_TARGETS,
  LEGION_PHASES,
  LEGION_WORKFLOW_PHASES,
  LegionActivityRequest,
  LegionAgentActionResponse,
  LegionCapabilityReport,
  LegionChildRequest,
  LegionClaimRequest,
  LegionCompactResponse,
  LegionControllerCredentialResponse,
  LegionControllerGrantRequest,
  LegionControllerRegisterResponse,
  LegionControllerSecretResponse,
  LegionEmptyResponse,
  LegionErrorResponse,
  LegionEscalateRequest,
  LegionFreshSessionResponse,
  LegionGateRegisterRequest,
  LegionGrantRequest,
  LegionGrantResponse,
  LegionHandoffCompleteRequest,
  LegionHandoffCompleteResponse,
  LegionHealthResponse,
  LegionInstant,
  LegionIntakeDiscardRequest,
  LegionIntakeDiscardResponse,
  LegionIntakeReplayRequest,
  LegionIntakeReplayResponse,
  LegionIssueRecordResponse,
  LegionIssueStatusRequest,
  LegionOperatorClaimResponse,
  LegionOperatorClaimsResponse,
  LegionOperatorProjectPauseRequest,
  LegionPauseFailureResponse,
  LegionPhaseBackwardRequest,
  LegionPhaseRetryRequest,
  LegionPlanRegisterRequest,
  LegionPlanRegisterResponse,
  LegionPodUpdateRequest,
  LegionPodUpdateResponse,
  LegionProjectPauseRequest,
  LegionProjectPauseResponse,
  LegionProjectResumeRequest,
  LegionProjectResumeResponse,
  LegionReadyForUpdateRequest,
  LegionReadyRequest,
  LegionRegisterResponse,
  LegionReleaseRecordsRequest,
  LegionReleaseRecordsResponse,
  LegionRootCloseRequest,
  LegionSessionActivity,
  LegionSignOffRequest,
  LegionStateResponse,
  LegionTaskStopRequest,
  LegionTurnOutcomeResponse,
  LegionWaveReleaseRequest,
  LegionWaveReleaseResponse,
} from "./legion-api";
import { DISPATCH_KEY_PATTERN, isLegionRole, LEGION_ROLES } from "./legion-roles";
import { PlanRatingCases } from "./plan-rating-cases";

test("a look's report names its check and carries no step's record or commit", () => {
  const report = {
    grantId: "grant",
    summary: "looked",
    verdict: "pending",
    checkOpenedAt: "2026-10-19T06:00:00.123456Z",
    wakeAt: "2026-10-19T09:00:00Z",
  };
  expect(LegionHandoffCompleteRequest.parse(report)).toEqual(report);
  for (const fields of [{ commit: "c0ffee" }, { handoffId: "handoff" }, { handoffVersion: 1 }]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...report, ...fields }).success).toBeFalse();
  }
});

const fixtureDir = path.join(import.meta.dir, "..", "fixtures", "daemon-api");

// `legionRepositories` exactly as contract 152 holds it (`legion-api.ts` before INFRA-4401, with
// its cache view and its two primitives beside it): the block a plugin of 152 parses, which
// requires `owners`, `mapped`, `refused` and `withoutWebhookApp` beside `caches` and strips any
// other key. Every plugin parses the state with the schema of the contract it was built at, and a
// Kubernetes deployment relaunches no stale session (INFRA-945 spec R10), so this reader is live in
// every pod still on the previous image. The route serves a caller below 202 the block in this
// shape, written from the entries of this contract's own (`api`'s `withoutRepositoriesLive`), and
// `state-narrowed-repositories.json` is that body: this is a copy and not an import, so a later
// edit of the current schema cannot move the reader it is held to.
const nonEmptyAt152 = z.string().min(1);
const timestampAt152 = z.iso.datetime({ offset: true });
const cacheViewAt152 = z.object({
  repo: nonEmptyAt152,
  sandbox: nonEmptyAt152,
  state: z.enum(["starting", "cloning", "ready", "failing", "refused"]),
  reason: nonEmptyAt152.optional(),
  ready: z.boolean(),
  address: nonEmptyAt152.optional(),
  head: nonEmptyAt152.optional(),
  lastFetchAt: timestampAt152.optional(),
  bytes: z.number().int().positive().optional(),
  healthError: nonEmptyAt152.optional(),
  retainedUntil: timestampAt152.optional(),
});
const appInstallationsAt152 = z.object({ implement: z.boolean(), review: z.boolean() });
const repositoriesAt152 = z.object({
  owners: z.array(nonEmptyAt152),
  mapped: z.array(
    z.object({
      repo: nonEmptyAt152,
      default: z.boolean(),
      apps: appInstallationsAt152.optional(),
      cache: cacheViewAt152.optional(),
    })
  ),
  refused: z.array(
    z.object({
      repo: nonEmptyAt152,
      reason: nonEmptyAt152,
      apps: appInstallationsAt152.optional(),
    })
  ),
  withoutWebhookApp: z.array(z.object({ repo: nonEmptyAt152, reason: nonEmptyAt152 })),
  caches: z.array(cacheViewAt152),
});

/** The state body a caller below 202 is served: this contract's state, every other key as
 * this schema reads it, with the repositories block in contract 152's shape. */
const stateWithRepositoriesAt152 = LegionStateResponse.extend({ repositories: repositoriesAt152 });

/** A claim's secret, as the requests below carry it. */
const CLAIM_SECRET = fixtureSecret("claim");

/** Every fixture the Go golden test writes, with the schema that must accept it. A new fixture
 * with no entry here fails the first test rather than going unchecked. */
const schemas: Record<string, z.ZodType> = {
  "state.json": LegionStateResponse,
  "state-stage3.json": LegionStateResponse,
  "state-operator-claim.json": LegionStateResponse,
  // Written by internal/projection's own golden test: what the state route projects while the
  // daemon's own controller (`controller: daemon`) holds its claim on no issue.
  "state-controller-claim.json": LegionStateResponse,
  // Written by internal/api's TestStateNarrowedRepositoriesGolden: the state route's body for a
  // caller whose plugin is at the contract before 202, its repositories block in contract 152's
  // shape.
  "state-narrowed-repositories.json": stateWithRepositoriesAt152,
  // One issue's record (`GET /legion/v1/issues/{key}/record`), which answers the very value the
  // state holds under `issues.<key>`: the issue schema the state response already carries.
  "issue-record.json": LegionIssueRecordResponse,
  // The typed health body (`api.Health`, `GET /healthz`): that the daemon is serving, and the
  // project it serves, which the controller's boot check reads.
  "health.json": LegionHealthResponse,
  // Written by internal/api's TestStateLastPauseGolden: the state of a project that runs after a
  // pause has ended, which carries `lastPause` and no `paused` (contract 178). state.json is the
  // state of a paused project and carries the other key.
  "state-last-pause.json": LegionStateResponse,
  "register.json": LegionRegisterResponse,
  // The claim wire's `claim.ReadyRequest` with a capability report: what a session posts on
  // `claims/ready` (contract 21).
  "ready.json": LegionReadyRequest,
  // `claim.ReadyForUpdateRequest`: the worker's answer to its pod's update message, saying it has
  // pushed everything it wants kept and its process may be replaced.
  "ready-for-update.json": LegionReadyForUpdateRequest,
  // `claim.TurnOutcomeResponse`: what a producer's report of one turn comes back as, the run of
  // refused turns the claim now ends with (`LegionTurnOutcomeResponse` says what `failed` holds).
  "turn-outcome.json": LegionTurnOutcomeResponse,
  // What a pod reads of its deployment's idle release: every release, resume and hold of the
  // window it named (`api.ReleaseRecordsResponse`).
  "release-records.json": LegionReleaseRecordsResponse,
  "register-controller.json": LegionControllerRegisterResponse,
  "controller-secret.json": LegionControllerSecretResponse,
  "controller-github-credential.json": LegionControllerCredentialResponse,
  "error.json": LegionErrorResponse,
  "operator-claim.json": LegionOperatorClaimResponse,
  "operator-forget-session.json": LegionOperatorClaimResponse,
  "operator-claims.json": LegionOperatorClaimsResponse,
  "grant.json": LegionGrantResponse,
  "handoff-complete.json": LegionHandoffCompleteResponse,
  "issue-status.json": LegionEmptyResponse,
  "fresh-session.json": LegionFreshSessionResponse,
  "compact.json": LegionCompactResponse,
  // Four of the controller's six agent actions, stop, retry, suspend and resume (the agent-actions
  // contract): the claim's state after each, and who holds a suspension over the agent.
  "agent-stop.json": LegionAgentActionResponse,
  "agent-retry.json": LegionAgentActionResponse,
  "agent-suspend.json": LegionAgentActionResponse,
  "agent-resume.json": LegionAgentActionResponse,
  // The controller's request for the replacement of one issue's pod (the agent-actions contract):
  // the update round over it, its reasons, and whether the request opened it.
  "issue-pod-update.json": LegionPodUpdateResponse,
  "gate-register.json": LegionEmptyResponse,
  "wave-release.json": LegionWaveReleaseResponse,
  "phase-backward.json": LegionEmptyResponse,
  "phase-retry.json": LegionEmptyResponse,
  "signoff.json": LegionEmptyResponse,
  "root-close.json": LegionEmptyResponse,
  "child-rerun.json": LegionEmptyResponse,
  // The owner's plan (contract 219): what register_plan took, and stop_task's empty answer.
  "plan-register.json": LegionPlanRegisterResponse,
  "task-stop.json": LegionEmptyResponse,
  // The project pause (contract 178), each written from what the route itself answers
  // (internal/api's TestPauseRouteGoldens): the pause a pause began, the pause a resume ended
  // with how many held rows it let out, and every refusal. A blank reason is the plain refusal;
  // the others name something beside the sentence: the setting that turns the pause off (none
  // from a daemon that runs no workflow), the pause that stands for a second pause and for a turn
  // refused while the project is paused, and the last pause for a resume of a project that runs.
  "project-pause.json": LegionProjectPauseResponse,
  "project-resume.json": LegionProjectResumeResponse,
  "project-pause-reason-required.json": LegionErrorResponse,
  "project-pause-unavailable.json": LegionPauseFailureResponse,
  "project-pause-no-workflow.json": LegionPauseFailureResponse,
  "project-pause-refused.json": LegionPauseFailureResponse,
  "project-paused.json": LegionPauseFailureResponse,
  "project-resume-refused.json": LegionPauseFailureResponse,
  // The replay and the discard of the intake's give-ups (contract 228), written by internal/api's
  // TestIntakeReplayAnswerGolden and TestIntakeDiscardAnswerGolden from what each route answers:
  // a replay with one record of each outcome and one left, and the ids a discard closed.
  "intake-replay.json": LegionIntakeReplayResponse,
  "intake-discard.json": LegionIntakeDiscardResponse,
  // Nor this one: the numbers of the numbered entries of `internal/api/version.go`, from the
  // serving floor to L, its greatest, ascending. No later change adds to it: a contract change is
  // an entry with one fixture under `contracts/`, so the number a build speaks is L plus its count
  // of entry fixtures (`daemon-api-records.ts`, and `daemonContract()` for a plugin's manifest).
  "released-contracts.json": z.array(z.number().int().positive()).min(1),
  // Nor this one: the numbers a worker's own stall watch runs on, written from the daemon's
  // copies of them (`internal/config/stalldefaults.go`) so that
  // `packages/pi-shared/src/stall-limits.test.ts` can hold the plugin's own numbers — the one
  // source — to the numbers the daemon computes its backstop floor from. The three defaults are
  // `src/stall-limits.ts`'s; `declaredDeadlineMostMs` is the hour the host clamps a call's own
  // declared deadline to (`src/stall-watchdog.ts`, DECLARED_DEADLINE_MOST_MS), the other half of
  // that floor.
  "stall-defaults.json": z.strictObject({
    callGraceMs: z.number().int().positive(),
    callBoundMs: z.number().int().positive(),
    stepMs: z.number().int().positive(),
    declaredDeadlineMostMs: z.number().int().positive(),
    callBoundMsByRole: z.record(
      z.enum([...LEGION_ROLES, "controller"]),
      z.number().int().nonnegative()
    ),
  }),
  // Nor this one: the table of answers a plugin parses (`internal/api/answers.go`), one row per
  // response schema. `routes` are the routes that answer it, empty for the refusal body, which
  // a client parses on every answer that is not a success. `since` is the contract the answer
  // arrived at, never below the serving floor. `noBody` says the answer carries no body at all,
  // which reads differently from a body whose set of key paths is empty. `typedSettings`, on the
  // state's row alone, is every top-level key of `settings` a plugin's reader types, from the
  // contract `since` to `until`, or to the daemon's own contract where `until` is absent: the
  // records of `contract-shapes.json` hold those settings keys and no other settings path, so a
  // new legion.yaml key is no contract change.
  "answers.json": z.array(
    z.strictObject({
      id: z.string().min(1),
      routes: z.array(z.string().min(1)),
      since: z.number().int().positive(),
      noBody: z.boolean(),
      typedSettings: z
        .array(
          z.strictObject({
            key: z.string().min(1),
            since: z.number().int().positive(),
            until: z.number().int().positive().optional(),
          })
        )
        .optional(),
    })
  ),
  // Nor this one: the table of the fields a plugin parses with a strict enumeration
  // (`internal/api/answers.go`'s `EnumeratedFields`), one row per field. `answer` is the answer
  // the field belongs to and `keyPath` its key path in that answer's body, written as
  // `contract-shapes.json` writes one. `values` is every value the field may take, each with
  // the contract it arrived at. It is recorded because a strict reader refuses a body for a
  // value its enumeration does not name as surely as for a key it does not know, so a widened
  // field is a shape change like an added key.
  "enumerated-fields.json": z.array(
    z.strictObject({
      answer: z.string().min(1),
      keyPath: z.string().min(1),
      values: z.array(
        z.strictObject({ value: z.string().min(1), since: z.number().int().positive() })
      ),
    })
  ),
  // Nor this one: what the daemon serves every contract it supports
  // (`internal/api/contract_shapes_test.go`). One record per answer per contract, from the
  // contract the answer arrived at up to the daemon's own, holding the sorted key paths of the
  // body a caller at that contract reads; under the state's `settings`, only the keys a reader
  // at that contract types (`typedSettings` in `answers.json`). A key path is one key of a body,
  // written as the dotted path from the body's root, with a map's key and a list's index written
  // as `*`, so `issues.*.pullRequest.fixAttempts` is one path, whichever issues the body holds
  // and whichever of them carries a pull request. `noBody` says the answer writes no body at
  // all, which reads differently from a body whose set of key paths is empty: the four routes
  // that answer 204 are `"noBody": true` with no path, and the empty object the fact routes
  // answer is `"noBody": false` with no path.
  //
  // `domains` is the other half of a record: for each field of that answer the daemon's table
  // declares enumerated (`enumerated-fields.json`), under the field's own key path, the sorted
  // values whose arrival is at or below that contract and that no entry at or below it retired
  // (an entry fixture's `removeValues`) — what a reader there admits, and not what any sample
  // body carries. An answer with no such field writes `{}`. The rule it stands
  // for is that a value refuses a reader as a key does: a body carrying a value the reader's
  // enumeration does not name is refused whole, which is why the record carries the domain and
  // the daemon serves an unadmitted value down.
  "contract-shapes.json": z.array(
    z.strictObject({
      contract: z.number().int().positive(),
      answer: z.string().min(1),
      noBody: z.boolean(),
      keyPaths: z.array(z.string().min(1)),
      domains: z.record(z.string().min(1), z.array(z.string().min(1))),
    })
  ),
  // Nor this one: the field paths of every request body a plugin sends
  // (`internal/api/request_shapes_test.go`), one record per route per contract from the daemon's
  // own contract on, each path written as a key path above is.
  "request-shapes.json": z.array(
    z.strictObject({
      contract: z.number().int().positive(),
      route: z.string().min(1),
      fieldPaths: z.array(z.string().min(1)),
    })
  ),
  // Nor this one: the closed set of words a claim can fail under
  // (`supervise.FailureCauses`), written by `internal/api`'s TestFailureCausesGolden so that
  // the Go roster and `LEGION_FAILURE_CAUSES` are held to each other here, by the case "the
  // cause set is the daemon's, word for word" below. A word the enum does not name fails the
  // parse of this fixture, which is what the gate above reports.
  "failure-causes.json": z.strictObject({ causes: z.array(z.enum(LEGION_FAILURE_CAUSES)) }),
  // Nor this one: the code of a completion refused for its phase entry and the words that
  // refusal tells the worker to act on (`staleEntryCode` and `staleEntryInstruction` in
  // `internal/api/handoff.go`), written by `internal/api`'s TestStaleEntryInstructionGolden so
  // that `packages/pi-shared/src/legion-phase-ops.test.ts` can hold the `legion` tool's
  // description for that code to the daemon's own text, word for word.
  "stale-entry.json": z.strictObject({
    code: z.literal("HANDOFF_STALE_ENTRY"),
    instruction: z.string().min(1),
  }),
  // Nor this one: the rules for ending a child (`record.EndChildRule` in
  // `internal/record/project.go`) and the sentence on a status the daemon has not taken yet
  // (`record.StatusNotTakenYet`), which the daemon's refusals and notices over children carry,
  // written by `internal/api`'s TestTheEndChildRuleGolden so that
  // `packages/pi-legion/src/tools.test.ts` can hold the `legion` tool's description to the
  // daemon's own text, word for word.
  "end-child-rule.json": z.strictObject({
    rule: z.string().min(1),
    notTakenYet: z.string().min(1),
  }),
  // `api.HandoffCompleteRequest` as the completion that ends the retro sends it, rated: the one
  // request of the route a plugin at contract 168 must send, written by `internal/api`'s
  // TestHandoffCompleteRequestRatedGolden from Go's own struct. The request schema is strict, so
  // a name of the rating Go writes and the contract does not read fails the gate above, and the
  // case "the rated completion the daemon reads parses under the contract's names" below.
  "handoff-complete-request-rated.json": LegionHandoffCompleteRequest,
  // Nor this one: the daemon's verdict on each case of a rating, which internal/api's
  // TestPlanRatingCasesGolden posts as its raw body to the completion that ends a retro and writes
  // down as the route answered it, so that the case "the request schema takes exactly the ratings
  // the daemon takes" below, and the plugin's tool in packages/pi-shared, hold their own rule to it.
  "plan-rating-cases.json": PlanRatingCases,
};

function fixture(name: string): unknown {
  return JSON.parse(readFileSync(path.join(fixtureDir, name), "utf8"));
}

function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** A golden read as the JSON object a route's body is. */
function fixtureBody(name: string): Record<string, unknown> {
  const body = fixture(name);
  if (!isJsonObject(body)) {
    throw new Error(`${name} is not a JSON object`);
  }
  return body;
}

/** The path of the first key the parse dropped: the first key present in `file` and absent from
 * `parsed`, descending objects by key and arrays by index. A Go field with no line in its schema
 * reads as its own path — `issues.LEGION-207.workers.implementer.claim.somethingNew` — rather
 * than as a diff of two large documents. */
function firstDroppedKey(file: unknown, parsed: unknown, path = ""): string | undefined {
  if (Array.isArray(file) && Array.isArray(parsed)) {
    for (const [index, element] of file.entries()) {
      const dropped = firstDroppedKey(element, parsed[index], `${path}[${index}]`);
      if (dropped !== undefined) {
        return dropped;
      }
    }
    return undefined;
  }
  if (!isJsonObject(file) || !isJsonObject(parsed)) {
    return undefined;
  }
  for (const [key, value] of Object.entries(file)) {
    const keyPath = path === "" ? key : `${path}.${key}`;
    if (!(key in parsed)) {
      return keyPath;
    }
    const dropped = firstDroppedKey(value, parsed[key], keyPath);
    if (dropped !== undefined) {
      return dropped;
    }
  }
  return undefined;
}

/** What the gate below reads of one state document: the path of the first key its parse dropped. */
function droppedFromState(state: unknown): string | undefined {
  return firstDroppedKey(state, LegionStateResponse.parse(state));
}

// The drift gate. The answer schemas strip a key they do not name, so a parse that reports no
// problem proves nothing on its own: a Go field with no line in the schema is dropped in silence
// and the parse still succeeds. Every key of a Go-written file has a line in its schema exactly
// when the parse equals the file by value, so that equality is the gate. By value and not by
// text: a stripping object rebuilds a value's keys in its shape's order, so a key-order
// comparison of the state goldens would be red before any drift.
test("every Go-written fixture parses to a value equal to the file", () => {
  const names = readdirSync(fixtureDir).filter((name) => name.endsWith(".json"));
  expect(names.length).toBeGreaterThan(0);

  for (const name of names) {
    const schema = schemas[name];
    expect(schema, `${name} has no schema in legion-api.test.ts`).toBeDefined();
    const file = fixture(name);
    const parsed = schema?.safeParse(file);
    expect(parsed?.error?.issues ?? [], `${name} failed the schema`).toEqual([]);
    const dropped = firstDroppedKey(file, parsed?.data);
    expect(dropped, `${name}: the Go field ${dropped} has no line in its schema`).toBeUndefined();
    expect(parsed?.data, `${name} parses to a value the file does not equal`).toEqual(file);
  }
});

// The other direction of that gate, and the hole a merge opened in it: the case above walks the
// goldens, so an answer schema no golden names is walked by nothing and a Go field it gains is
// dropped in silence — which is what `LegionReleaseRecordsResponse` was between the merge that
// brought it and the fixture above. Every answer schema the module exports must therefore be the
// schema of some golden, and the roster is derived (`exportedSchemas`), so the next answer
// schema added with no fixture fails here by its own name. `LegionEmptyResponse` is the schema
// of the fact-route goldens, so it is a value of the map once per fact route and passes as any
// other does.
test("every answer schema the module exports is the schema of a golden", () => {
  const held = new Set(Object.values(schemas));
  const roster = exportedSchemas("answer");
  const names = Object.keys(roster).sort();
  console.log(`answer roster (${names.length} schemas) held by a golden: ${names.join(", ")}`);

  // A roster that matched nothing would leave the assertion below holding vacuously.
  expect(names.length).toBeGreaterThan(0);
  const without = Object.entries(roster)
    .filter(([, schema]) => !held.has(schema))
    .map(([name]) => name)
    .sort();
  expect(
    without,
    `these answer schemas have no Go-written golden under fixtures/daemon-api: ${without.join(", ")}`
  ).toEqual([]);
});

/** The answer schema this module parses each answer of the daemon's table with, by the id the
 * table writes it under (`internal/api/answers.go`). An answer whose id no entry here names
 * fails the case below, so an enumerated field of that answer is read rather than skipped. */
const ANSWER_SCHEMAS: Record<string, z.ZodType> = {
  state: LegionStateResponse,
  "issue-record": LegionIssueRecordResponse,
  register: LegionRegisterResponse,
  "intake-replay": LegionIntakeReplayResponse,
  // Four of the controller's six agent actions, stop, retry, suspend and resume (the agent-actions
  // contract): the claim's `state`, and who holds a suspension over it.
  "agent-action": LegionAgentActionResponse,
};

/** The schemas this module parses each answer of the daemon's table that writes a body with, by
 * the id the table writes it under: the answer schemas above and every other. A client parses
 * the refusal on every answer that is not a success, as the error or as a pause's refusal, so its
 * key paths are those the two name. An answer whose id no entry here names fails the case that
 * reads them, so a key path of that answer is held rather than skipped. */
const RECORD_SCHEMAS: Record<string, readonly z.ZodType[]> = {
  ...Object.fromEntries(Object.entries(ANSWER_SCHEMAS).map(([id, schema]) => [id, [schema]])),
  health: [LegionHealthResponse],
  "register-controller": [LegionControllerRegisterResponse],
  "release-records": [LegionReleaseRecordsResponse],
  grant: [LegionGrantResponse],
  "handoff-complete": [LegionHandoffCompleteResponse],
  "empty-object": [LegionEmptyResponse],
  "fresh-session": [LegionFreshSessionResponse],
  compact: [LegionCompactResponse],
  "turn-outcome": [LegionTurnOutcomeResponse],
  "wave-release": [LegionWaveReleaseResponse],
  refusal: [LegionErrorResponse, LegionPauseFailureResponse],
  "project-pause": [LegionProjectPauseResponse],
  "project-resume": [LegionProjectResumeResponse],
  "plan-register": [LegionPlanRegisterResponse],
  "intake-discard": [LegionIntakeDiscardResponse],
  "pod-update": [LegionPodUpdateResponse],
};

/** The reason the refusal's `reasons` is named by no schema of this module. */
const PLAN_REASONS_STRIPPED =
  "a refused plan's `error` holds every reason in order, and the `legion` tool hands the refusal to its agent whole, so `LegionErrorResponse` strips the list by design (`LegionPlanRegisterResponse` says so)";

/** Every key path a record of the daemon serves that this module's schema of its answer leaves
 * unnamed by design, by `<answer> <keyPath>`, with why. The case that reads it fails an entry
 * whose path the record does not hold or the schema names, so a stale entry is red rather than
 * hiding the next key path no schema names. */
const UNNAMED_KEY_PATHS: Record<string, string> = {
  "refusal reasons": PLAN_REASONS_STRIPPED,
  "refusal reasons.*": PLAN_REASONS_STRIPPED,
  "refusal reasons.*.reason": PLAN_REASONS_STRIPPED,
  "refusal reasons.*.task": PLAN_REASONS_STRIPPED,
};

/** One step of a path from a schema's root to a node below it: the field `key` of an object,
 * the element of a list, the value of a record, or the option `index` of a union. */
type SchemaStep =
  | { readonly kind: "key"; readonly key: string }
  | { readonly kind: "element" }
  | { readonly kind: "value" }
  | { readonly kind: "option"; readonly index: number };

/** A path as the object censuses below write it: `.key` for a field, `[]` for a list's element,
 * `{}` for a record's value and `|0`, `|1` for the options of a union. */
function censusPath(steps: readonly SchemaStep[]): string {
  return steps
    .map((step) => {
      if (step.kind === "key") return `.${step.key}`;
      if (step.kind === "option") return `|${step.index}`;
      return step.kind === "element" ? "[]" : "{}";
    })
    .join("");
}

/** A path as the daemon's records write one: the keys joined by `.`, a record's value and a list's
 * element both `*` (`issues.*.phase`, `capabilities.*.status`), and the options of a union at the
 * path of the union itself, since `waitingOn.answeredBy` is one field of both the `ask` and the
 * `gate` shape (`keyPathRule` in `internal/api/contract_shapes_test.go`). */
function recordKeyPath(steps: readonly SchemaStep[]): string {
  return steps
    .flatMap((step) => {
      if (step.kind === "key") return [step.key];
      return step.kind === "option" ? [] : ["*"];
    })
    .join(".");
}

/** The node kinds the schemas of either side are built from that hold no node below them. A
 * kind outside this list and the branches of `walkSchema` throws: a `z.tuple`, a
 * `z.intersection`, a `.pipe()` or a `z.lazy()` added later would otherwise take its whole
 * subtree out of every census in silence. */
const CENSUS_LEAF_KINDS: Record<string, true> = {
  boolean: true,
  enum: true,
  literal: true,
  number: true,
  string: true,
  unknown: true,
};

/** The one walk of the schema tree that every census of this file reads. It calls `visit` on
 * `node` and on every node below it, with the steps from the root. It descends an object's
 * shape, a list's element, a record's value type and a union's options (`.refine()` wraps
 * nothing, so a refined object is still an object node and a refined discriminated union still a
 * union node). It visits the inner type of an optional or a nullable at the path of the wrapper,
 * and the wrapper not at all. A node two paths reach is visited at each of them, as the claim
 * view is at an issue's `architect` and under `workers{}.claim`. The schemas hold no `z.lazy`,
 * so the walk ends: `CENSUS_LEAF_KINDS` refuses one if it arrives. `root` names the schema in
 * the sentence the walk throws. */
function walkSchema(
  root: string,
  node: z.core.$ZodType,
  visit: (node: z.core.$ZodType, steps: readonly SchemaStep[]) => void,
  steps: readonly SchemaStep[] = []
): void {
  if (node instanceof z.ZodOptional || node instanceof z.ZodNullable) {
    walkSchema(root, node.def.innerType, visit, steps);
    return;
  }
  visit(node, steps);
  if (node instanceof z.ZodObject) {
    for (const [key, value] of Object.entries(node.def.shape)) {
      walkSchema(root, value, visit, [...steps, { kind: "key", key }]);
    }
    return;
  }
  if (node instanceof z.ZodArray) {
    walkSchema(root, node.def.element, visit, [...steps, { kind: "element" }]);
    return;
  }
  if (node instanceof z.ZodRecord) {
    walkSchema(root, node.def.valueType, visit, [...steps, { kind: "value" }]);
    return;
  }
  if (node instanceof z.ZodUnion) {
    for (const [index, option] of node.def.options.entries()) {
      walkSchema(root, option, visit, [...steps, { kind: "option", index }]);
    }
    return;
  }
  const kind = node._zod.def.type;
  if (CENSUS_LEAF_KINDS[kind] !== true) {
    throw new Error(
      `the census reached a ${kind} node it does not walk, at ${root}${censusPath(steps)}`
    );
  }
}

/** Every `z.enum` the answer schemas of `roots` read, with its members, by `<answer> <key path>`,
 * the key path written as the daemon's table writes one (`recordKeyPath`). A node two paths reach
 * is read at both of them, as the claim view is at `architect` and at `workers.*.claim`.
 *
 * `problems` names each path that holds no one list to compare: a path at which two
 * enumerations with different members stand, a path at which a `z.enum` stands beside a node
 * of another kind, and a `z.enum` with a member that is no string, since the daemon's table
 * writes every value as a word. */
function enumeratedFields(roots: Record<string, z.ZodType>): {
  readonly members: ReadonlyMap<string, readonly string[]>;
  readonly problems: readonly string[];
} {
  const members = new Map<string, readonly string[]>();
  const others = new Map<string, string>();
  const problems: string[] = [];
  for (const [answer, schema] of Object.entries(roots)) {
    walkSchema(answer, schema, (node, steps) => {
      const field = `${answer} ${recordKeyPath(steps)}`;
      if (!(node instanceof z.ZodEnum)) {
        // A union and its options stand at one path, so only a kind other than those two is
        // one that could stand where an enumeration does.
        if (!(node instanceof z.ZodUnion) && !(node instanceof z.ZodObject)) {
          others.set(field, node._zod.def.type);
        }
        return;
      }
      const words = node.options.filter((member): member is string => typeof member === "string");
      if (words.length !== node.options.length) {
        problems.push(`${field} is a z.enum whose members are not all strings`);
        return;
      }
      const before = members.get(field);
      const sorted = [...words].sort().join(", ");
      if (before !== undefined && [...before].sort().join(", ") !== sorted) {
        problems.push(
          `${field} is read with two enumerations: ${[...before].sort().join(", ")} and ${sorted}`
        );
        return;
      }
      members.set(field, words);
    });
  }
  for (const [field, kind] of others) {
    if (members.has(field))
      problems.push(`${field} is a z.enum in one place and a ${kind} in another`);
  }
  return { members, problems };
}

// The two statements of one domain, held to each other. The daemon serves a value down to a
// reader whose contract does not admit it, and it reads the arrival of each value from its own
// table; this module refuses a value its `z.enum` does not name. A value added on one side and
// not the other would make the daemon either refuse a body it writes or strip a value this
// build reads, so the two lists are compared here rather than left to agree by habit.
//
// Both sides are read rather than restated. The compared side is one map of every `z.enum` the
// answer schemas read, by answer and key path, collected in one walk (`enumeratedFields`), so a
// change to any enumeration of legion-api.ts is a change to what this case compares. The join is
// the whole key path, not its tail, so a row can only meet the field it names. Neither is a list
// kept by hand beside the schemas: a field the daemon adds to its table is compared without a
// line being added here, and a `z.enum` this module reads at a path the daemon declares nothing
// for fails the last assertion rather than passing unheld.
//
// One kind of row has no field of this module to meet: a field the daemon serves only below its
// own contract, in a block an older reader parses with its own schema, which this build no longer
// holds. `repositories.mapped.*.cache.state` is one: contract 202 replaced that block with
// `live`, and the daemon writes it, in contract 152's shape, for a caller below 202 alone. Such a
// row is told apart by the records: the record of this build's own contract carries no such key
// path and the record of an earlier contract does. That earlier reader's own enumeration holds
// it, so the row is passed over here; a row no record carries at all is compared as any other,
// and fails.
//
// One kind of value has no member of this module to meet either: a value an entry retired, by its
// `removeValues`. The daemon's table still declares it, for a reader below that entry, and this
// build's enumeration no longer names it. Such a value is told apart by the records too: the
// record of L admits it and the record of this build's own contract does not. Only such a value is
// passed over; every other declared value is compared, so one this build's enumeration does not
// name fails whether or not a record admits it.
//
// The rows and the records are this build's (`daemon-api-records.ts`): `enumerated-fields.json`
// and `contract-shapes.json`, which hold the numbered entries', with the rows and the records
// each entry fixture adds over them, so a value an entry adds is compared as one the file holds.
test("every enumerated field the daemon declares holds this build's own values", () => {
  const build = daemonApiRecords(fixtureDir);
  const rows = build.enumeratedFields;
  expect(rows.length).toBeGreaterThan(0);
  const { members, problems } = enumeratedFields(ANSWER_SCHEMAS);
  expect(problems).toEqual([]);
  // A walk that read no enumeration would leave both directions below holding vacuously.
  expect(members.size).toBeGreaterThan(0);
  // Each answer's key paths, as `<answer> <keyPath>`, in the record of this build's own contract
  // and in the record of any earlier one.
  const ownPaths = new Set<string>();
  const earlierPaths = new Set<string>();
  // Each answer's domains, by key path, in the record of L and in that of this build's own
  // contract: a value the first admits and the second does not is one an entry retired.
  const lastDomains = new Map<string, ContractShape["domains"]>();
  const ownDomains = new Map<string, ContractShape["domains"]>();
  for (const record of build.records) {
    const paths = record.contract === build.contract ? ownPaths : earlierPaths;
    for (const keyPath of record.keyPaths) {
      paths.add(`${record.answer} ${keyPath}`);
    }
    if (record.contract === build.last) lastDomains.set(record.answer, record.domains);
    if (record.contract === build.contract) ownDomains.set(record.answer, record.domains);
  }

  for (const row of rows) {
    const field = `${row.answer} ${row.keyPath}`;
    if (!ownPaths.has(field) && earlierPaths.has(field)) {
      continue;
    }
    expect(
      ANSWER_SCHEMAS[row.answer],
      `the daemon declares \`${row.keyPath}\` of the ${row.answer} answer enumerated and ANSWER_SCHEMAS names no schema for that answer`
    ).toBeDefined();
    const read = members.get(field);
    expect(
      read,
      `the daemon declares \`${row.keyPath}\` of the ${row.answer} answer enumerated and this module reads it with no z.enum`
    ).toBeDefined();
    if (read === undefined) {
      continue;
    }
    const own = ownDomains.get(row.answer)?.[row.keyPath] ?? [];
    const retired = (lastDomains.get(row.answer)?.[row.keyPath] ?? []).filter(
      (value) => !own.includes(value)
    );
    expect(
      row.values
        .map((value) => value.value)
        .filter((value) => !retired.includes(value))
        .sort(),
      `the daemon's \`${row.keyPath}\` of the ${row.answer} answer and this build's enumeration name different values`
    ).toEqual([...read].sort());
  }

  // The other direction: a field this module reads with a `z.enum` that the daemon's table does
  // not declare is a value the daemon could add without serving it down, and the loop above
  // would never reach it.
  const declared = new Set(rows.map((row) => `${row.answer} ${row.keyPath}`));
  const undeclared = [...members.keys()].filter((field) => !declared.has(field)).sort();
  expect(
    undeclared,
    `this module reads these fields with a z.enum and the daemon's table declares none of them: ${undeclared.join(", ")}`
  ).toEqual([]);
});

// The keys of the other half of a record, held to the schemas: every key path the daemon serves
// an answer at this build's own contract is one this module's schema of that answer names, but
// those `UNNAMED_KEY_PATHS` gives a reason for. The drift gate above holds the keys the goldens
// carry; this holds the keys of the record, which is taken from bodies richer than any golden, and
// which is the one an entry fixture composes over L's (`daemon-api-records.ts`). So an entry whose
// fixture adds a key path to an answer is red here until the schema names the key, before any
// golden carries it.
test("every key path the daemon serves this build's contract is one its schema names", () => {
  const build = daemonApiRecords(fixtureDir);
  const newest = build.records.filter((record) => record.contract === build.contract);
  expect(newest.length).toBeGreaterThan(0);
  const unnamed: string[] = [];
  const held = new Set<string>();
  const named = new Set<string>();
  for (const record of newest) {
    if (record.noBody) {
      continue;
    }
    const schemas = RECORD_SCHEMAS[record.answer];
    expect(schemas, `the ${record.answer} answer has no schema in RECORD_SCHEMAS`).toBeDefined();
    for (const schema of schemas ?? []) {
      walkSchema(record.answer, schema, (_node, steps) => {
        named.add(`${record.answer} ${recordKeyPath(steps)}`);
      });
    }
    for (const keyPath of record.keyPaths) {
      const field = `${record.answer} ${keyPath}`;
      held.add(field);
      if (!named.has(field) && UNNAMED_KEY_PATHS[field] === undefined) {
        unnamed.push(field);
      }
    }
  }
  expect(
    unnamed,
    `the daemon serves these key paths at contract ${build.contract} and this module's schema of their answer names none of them: ${unnamed.join(", ")}`
  ).toEqual([]);
  for (const field of Object.keys(UNNAMED_KEY_PATHS).sort()) {
    expect(held.has(field), `UNNAMED_KEY_PATHS names ${field}, which no record holds`).toBeTrue();
    expect(named.has(field), `UNNAMED_KEY_PATHS names ${field}, which a schema names`).toBeFalse();
  }
});

// The composition itself, held to the example fixtures it shares with the daemon's shape gate
// (`testdata/entry-fixture-examples/`, `internal/api`'s TestTheCompositionOfTheExampleFixtures):
// `base.json` stands for L's record, each `<id>_<slug>.json` is an entry fixture, and
// `expected.json` is the one record the newest of them composes, which both languages reach or are
// red. The examples make every operation a fixture holds, and the last removes what the first and
// the second add, so their order matters: in the reverse order the composition is refused, naming
// the entry and the operation, as the gate refuses a fixture an order puts before the entry whose
// key it changes.
test("the example entry fixtures compose the one record expected of them", () => {
  const examples = path.join(import.meta.dir, "..", "testdata", "entry-fixture-examples");
  const read = (name: string): ContractRecords =>
    JSON.parse(readFileSync(path.join(examples, name), "utf8"));
  const base = read("base.json");
  const files = entryFixtures(examples, false);
  expect(files.length).toBeGreaterThan(1);
  const numbered = (order: typeof files) =>
    order.map((file, rank) => ({
      name: file.name,
      number: base.contract + rank + 1,
      fixture: file.fixture,
    }));
  expect(composeEntries(base, numbered(files)).at(-1)).toEqual(read("expected.json"));

  const reversed = numbered([...files].reverse());
  expect(() => composeEntries(base, reversed)).toThrow(
    `entry ${reversed[0]?.name} (contract ${base.contract + 1}) removes the note answer, and the record before it holds no such answer`
  );
});

// A row an entry adds to `enumerated-fields.json` names each of its values once. The daemon's gate
// grows the row value by value (`withEnumeratedRows` in `internal/api`), so it refuses a value the
// added row names twice as surely as one the row before it holds, naming the entry; the
// composition here must refuse it too, or the two would compose different records from one
// fixture.
test("an entry row that adds one enumerated value twice is refused, naming the entry", () => {
  const examples = path.join(import.meta.dir, "..", "testdata", "entry-fixture-examples");
  const base: ContractRecords = JSON.parse(readFileSync(path.join(examples, "base.json"), "utf8"));
  expect(base.enumeratedFields.map((row) => row.keyPath)).toEqual(["issues.*.phase"]);
  // A row the base holds, and a row no record before it declares.
  for (const keyPath of ["issues.*.phase", "issues.*.kind"]) {
    const entry = {
      name: "20261126000000_twice",
      number: base.contract + 1,
      fixture: {
        answers: [],
        routes: [],
        answerRows: [],
        enumeratedRows: [{ answer: "state", keyPath, values: ["merging", "merging"] }],
      },
    };
    expect(() => composeEntries(base, [entry])).toThrow(
      `entry 20261126000000_twice (contract ${base.contract + 1}) adds \`merging\` to \`${keyPath}\` of the state answer in enumerated-fields.json, and that row holds it already`
    );
  }
});

// What that equality catches, and the one place it is blind. A key no schema names is dropped
// wherever an object names its keys, at the top level and at the depth INFRA-1048's incident
// took, and the gate names its path. Under `settings` (a `z.looseObject`) and under a pending
// status write's `payload` (a `z.record`) nothing is dropped, so the gate sees nothing there:
// both blocks are loose by design — legion.yaml's keys and the workflow's payload are not this
// wire's shape — and that is the limit of what the gate can hold.
test("the gate names a key no schema names, and is blind under the loose blocks", () => {
  const topLevel = fixtureBody("state.json");
  topLevel.workerAdmission = { queue: [] };
  expect(droppedFromState(topLevel)).toBe("workerAdmission");

  const underClaim = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { claim: Record<string, unknown> }> }>;
  };
  const claim = underClaim.issues["LEGION-207"]?.workers.implementer?.claim;
  if (claim === undefined) {
    throw new Error("state fixture lacks LEGION-207's implementer claim");
  }
  claim.somethingNew = "a field the Go view grew";
  expect(droppedFromState(underClaim)).toBe(
    "issues.LEGION-207.workers.implementer.claim.somethingNew"
  );

  const settings = fixture("state.json") as { settings: Record<string, unknown> };
  settings.settings.a_later_legion_yaml_key = "ridden loosely";
  const parsedSettings = LegionStateResponse.parse(settings);
  expect(firstDroppedKey(settings, parsedSettings)).toBeUndefined();
  expect<unknown>(parsedSettings).toEqual(settings);

  const payload = fixture("state-stage3.json") as {
    pendingStatusWrites: { payload: Record<string, unknown> }[];
  };
  const write = payload.pendingStatusWrites[0];
  if (write === undefined) {
    throw new Error("state-stage3 fixture lacks its pending status write");
  }
  write.payload.somethingNew = "the workflow owns this shape";
  const parsedPayload = LegionStateResponse.parse(payload);
  expect(firstDroppedKey(payload, parsedPayload)).toBeUndefined();
  expect<unknown>(parsedPayload).toEqual(payload);
});

// `workers` is keyed by a plain string, so the schema no longer holds the role names and the gate
// above cannot see a role the Go side adds. The key list it held is held here instead: every
// worker key of every golden is one of this build's roles.
test("every golden's worker keys are roles this build knows", () => {
  const states = Object.keys(schemas).filter((name) => schemas[name] === LegionStateResponse);
  expect(states.length).toBeGreaterThan(0);

  let seen = 0;
  for (const name of states) {
    const state = LegionStateResponse.parse(fixture(name));
    for (const [key, issue] of Object.entries(state.issues)) {
      for (const role of Object.keys(issue.workers ?? {})) {
        expect(isLegionRole(role), `${name}: ${key} carries the worker ${role}`).toBeTrue();
        seen += 1;
      }
    }
  }
  expect(seen).toBeGreaterThan(0);
});

// A role key this build does not know arrives under `issues.<key>.workers` the day the daemon
// grows a role. It used to refuse the whole document, so every reader lost its state over a key
// it merely did not know; now the unknown entry is read beside the known roles, whose views come
// through unchanged.
test("a worker role this build does not know leaves the rest of the state readable", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, unknown> }>;
  };
  const workers = state.issues["LEGION-208"]?.workers;
  if (workers?.planner === undefined || workers.implementer === undefined) {
    throw new Error("state fixture lacks LEGION-208's planner and implementer");
  }
  workers.auditor = workers.planner;

  const parsed = LegionStateResponse.parse(state).issues["LEGION-208"]?.workers;
  expect<unknown>(parsed?.planner).toEqual(workers.planner);
  expect<unknown>(parsed?.implementer).toEqual(workers.implementer);
  expect<unknown>(parsed?.auditor).toEqual(workers.planner);
});

/** Whether `value` is a zod schema this walk can read. */
function isSchema(value: unknown): value is z.ZodType {
  return (
    typeof value === "object" &&
    value !== null &&
    "def" in value &&
    "safeParse" in value &&
    typeof value.safeParse === "function"
  );
}

/** The module's exported schemas of one side of the wire, keyed by export name: the answer side
 * is every exported schema whose name ends in `Response`, the request side every one whose name
 * does not. The roster is derived rather than listed so that a schema a later change adds is
 * held by the cases that walk it without anyone adding a row — a list somebody must remember to
 * extend is the very defect this issue is about, and a merge has already brought a schema no
 * list named. The exports that are no schema at all (`LEGION_PHASES` and the other frozen
 * lists) are left out by `isSchema`, so the suffix alone is not trusted. */
function exportedSchemas(side: "answer" | "request"): Record<string, z.ZodType> {
  const roster: Record<string, z.ZodType> = {};
  for (const [name, value] of Object.entries(legionApi)) {
    if (isSchema(value) && name.endsWith("Response") === (side === "answer")) {
      roster[name] = value;
    }
  }
  return roster;
}

/** What zod 4.3.6 marks an object node with, read rather than built: a stripping object has no
 * `catchall`, a refusing one a `never`, a loose one an `unknown`. */
type ObjectMark = "strips" | "refuses" | "loose";

function objectMark(node: z.ZodObject): ObjectMark {
  const catchall = node.def.catchall?._zod.def.type;
  if (catchall === undefined) {
    return "strips";
  }
  return catchall === "never" ? "refuses" : "loose";
}

/** Every object node of the given schemas, by mark and by path, read off `walkSchema`. A path is
 * written as `censusPath` writes it. A node two paths reach is one node, counted at the first
 * path that reaches it: the claim view under `architect`, the cache view under
 * `repositories.live[].cache`. */
function objectCensus(roots: Record<string, z.ZodType>): Record<ObjectMark, string[]> {
  const found: Record<ObjectMark, string[]> = { strips: [], refuses: [], loose: [] };
  const seen = new Set<z.core.$ZodType>();
  for (const [name, schema] of Object.entries(roots)) {
    walkSchema(name, schema, (node, steps) => {
      if (!(node instanceof z.ZodObject) || seen.has(node)) {
        return;
      }
      seen.add(node);
      found[objectMark(node)].push(`${name}${censusPath(steps)}`);
    });
  }
  for (const paths of Object.values(found)) {
    paths.sort();
  }
  return found;
}

// The whole answer side strips, not only what a golden passes through: a body reaches the eight
// or so objects its own route carries, so a partial edit that left one nested view refusing would
// pass every other case here. The census walks every answer schema the module exports — the
// roster is derived, so a schema a later change adds is walked without anyone adding a row — and
// every node below them, and reads zod's own mark on each object. What it holds is the shape:
// nothing refuses, and `settings` alone is loose. The roster and the number of nodes walked are
// printed rather than pinned, since a count goes stale at every addition.
test("every answer-side object strips a key it does not name", () => {
  // The mark first, on three nodes built here: were a zod release to stop setting `catchall`,
  // every node would read as stripping and the census below would pass proving nothing.
  expect({
    strict: objectMark(z.strictObject({})),
    stripping: objectMark(z.object({})),
    loose: objectMark(z.looseObject({})),
  }).toEqual({ strict: "refuses", stripping: "strips", loose: "loose" });

  const roster = exportedSchemas("answer");
  const found = objectCensus(roster);
  const names = Object.keys(roster).sort();
  const nodes = found.strips.length + found.loose.length + found.refuses.length;
  console.log(
    `answer roster (${names.length} schemas, ${nodes} object nodes): ${names.join(", ")}`
  );

  // A predicate that matched nothing would leave every assertion below holding vacuously.
  expect(names.length).toBeGreaterThan(0);
  expect(found.refuses).toEqual([]);
  // `settings` is the one loose block (contract 35). A pending status write's `payload` is a
  // record, not an object, and is no node of this census.
  expect(found.loose).toEqual(["LegionStateResponse.settings"]);
  expect(found.strips.length).toBeGreaterThan(0);
});

test("state accepts optional fields emitted by later workflow slices", () => {
  const current = fixture("state.json");
  expect(LegionStateResponse.safeParse(current).success).toBeTrue();

  const later = fixture("state.json") as {
    admission: Record<string, unknown>;
    issues: Record<string, { phase: string; slot?: Record<string, unknown> }>;
  };
  const laterIssue = later.issues["LEGION-208"];
  if (laterIssue === undefined || laterIssue.slot === undefined) {
    throw new Error("state fixture lacks LEGION-208 or its slot");
  }
  later.admission.free = 1;
  laterIssue.phase = "integrating";
  laterIssue.slot.lentTo = "LEGION-209";

  expect(LegionStateResponse.safeParse(later).success).toBeTrue();
});

test("state rejects malformed optional workflow fields", () => {
  const negativeFree = fixture("state.json") as { admission: Record<string, unknown> };
  negativeFree.admission.free = -1;
  expect(LegionStateResponse.safeParse(negativeFree).success).toBeFalse();

  const malformedLender = fixture("state.json") as {
    issues: Record<string, { slot?: Record<string, unknown> }>;
  };
  const malformedLenderIssue = malformedLender.issues["LEGION-208"];
  if (malformedLenderIssue === undefined || malformedLenderIssue.slot === undefined) {
    throw new Error("state fixture lacks LEGION-208 or its slot");
  }
  malformedLenderIssue.slot.lentTo = "not an issue key";
  expect(LegionStateResponse.safeParse(malformedLender).success).toBeFalse();
});

test("state accepts the existing done phase", () => {
  const completed = fixture("state.json") as {
    issues: Record<string, { phase: string }>;
  };
  const completedIssue = completed.issues["LEGION-208"];
  if (completedIssue === undefined) {
    throw new Error("state fixture lacks LEGION-208");
  }
  completedIssue.phase = "done";

  expect(LegionStateResponse.safeParse(completed).success).toBeTrue();
});

test("a backward move rejects future read-only phases", () => {
  expect(
    LegionPhaseBackwardRequest.safeParse({
      grantId: "grant-208",
      to: "integrating",
      reason: "test failed",
    }).success
  ).toBeFalse();
});

// Contract 40: `awaiting_person`, a root that keeps its slot while its architect waits on a
// person, follows `admitted`, and no backward move reaches it. Contract 33: the architect accepts
// before the merge, in `accepting`, which the list holds between `reviewing` and `retro`; the merger merges in
// `merging`, the daemon closes the issue when it reads that merge (`done`), and the production
// check after the close is no phase, so no phase follows `integrating` but the close itself.
test("the phase lists are the daemon's, with awaiting_person after admitted and accepting before retro", () => {
  expect(LEGION_WORKFLOW_PHASES).toEqual([
    "admitted",
    "awaiting_person",
    "awaiting_children",
    "planning",
    "implementing",
    "testing",
    "reviewing",
    "accepting",
    "retro",
    "merging",
    "integrating",
    "done",
    "held",
  ]);
  expect(LEGION_PHASE_BACKWARD_TARGETS).toEqual([
    "admitted",
    "planning",
    "implementing",
    "testing",
    "reviewing",
    "accepting",
    "retro",
    "merging",
    "done",
    "held",
  ]);
  expect(LEGION_PHASES).toEqual([...LEGION_WORKFLOW_PHASES, "unrecorded"]);
  expect(
    LegionPhaseBackwardRequest.safeParse({
      grantId: "grant-208",
      to: "accepting",
      reason: "retro reopened the review",
    }).success
  ).toBeTrue();
});

// The held words are the daemon's `record.HeldReasons`, in its order: dispatch://INFRA-3320 added
// `awaiting_children`, a parent waiting on its open children, between `root_awaiting_person` and
// `design_gate`, and contract 219 added `children_before_retro` and `task_stopped` after
// `accepted_ancestor`.
test("the held words are the daemon's, with awaiting_children before design_gate", () => {
  expect(LEGION_HELD_REASONS).toEqual([
    "tree_closed",
    "tree_in_backlog_or_icebox",
    "label_off",
    "ancestry",
    "root_awaiting_person",
    "awaiting_children",
    "design_gate",
    "dispatch_blocker",
    "children_first",
    "accepted_ancestor",
    "children_before_retro",
    "task_stopped",
    "at_work",
    "unclassified",
  ]);
});

// Contract 40: the state golden carries LEGION-212, a root waiting on a person whose question the
// person answered. Both shapes of `waitingOn` parse, an answer comes with the act that gave it,
// and a wait half answered, an act no answer names, or a `kind` no shape has is refused.
test("the state golden carries what an awaiting_person root waits on", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { phase: string; waitingOn?: Record<string, unknown> }>;
  };
  const waiting = state.issues["LEGION-212"];
  if (waiting === undefined || waiting.waitingOn === undefined) {
    throw new Error("state fixture lacks LEGION-212 or its waitingOn");
  }
  expect(waiting.phase).toBe("awaiting_person");
  expect(waiting.waitingOn).toEqual({
    kind: "ask",
    ask: "9a8fd348-55d5-4d72-a793-3cd037f75f13",
    since: "2026-09-22T08:05:00Z",
    answeredAt: "2026-09-22T09:21:00Z",
    answeredBy: "ask",
  });

  const parses = (waitingOn: Record<string, unknown>) => {
    waiting.waitingOn = waitingOn;
    return LegionStateResponse.safeParse(state).success;
  };
  expect(parses({ kind: "ask", ask: "ask-7" })).toBeTrue();
  expect(parses({ kind: "gate", gateVersion: 3, artifactId: "spec-doc" })).toBeTrue();
  expect(
    parses({
      kind: "gate",
      gateVersion: 3,
      artifactId: "spec-doc",
      answeredAt: "2026-09-22T09:21:00Z",
      answeredBy: "approved",
    })
  ).toBeTrue();
  expect(parses({ kind: "ask", ask: "ask-7", answeredAt: "2026-09-22T09:21:00Z" })).toBeFalse();
  expect(parses({ kind: "ask", ask: "ask-7", answeredBy: "ask" })).toBeFalse();
  expect(
    parses({ kind: "ask", ask: "ask-7", answeredAt: "2026-09-22T09:21:00Z", answeredBy: "guess" })
  ).toBeFalse();
  // `artifactId` is no field this plugin reads, so a gate wait without it is read, not refused;
  // the discriminator is the one key the union itself requires.
  expect(parses({ kind: "gate", gateVersion: 3 })).toBeTrue();
  expect(parses({ kind: "other", ask: "ask-7" })).toBeFalse();
});

// Contract 49: the state golden carries LEGION-208, a root drawn into a memory group, and
// LEGION-209, a child of that tree, each with the group, the mode it was drawn under and the
// instant of the draw. The three are a tree's and are shown on every issue of it; an issue of a
// tree no draw has reached carries none of them, so each is optional. A daemon before this
// contract sent none, and its state must still parse.
test("the state golden carries each issue's tree memory draw", () => {
  const state = fixture("state.json") as {
    issues: Record<string, Record<string, unknown>>;
  };
  for (const key of ["LEGION-208", "LEGION-209"]) {
    const issue = state.issues[key];
    if (issue === undefined) {
      throw new Error(`state fixture lacks ${key}`);
    }
    expect({
      memoryGroup: issue.memoryGroup,
      memoryDrawMode: issue.memoryDrawMode,
      memoryDrawnAt: issue.memoryDrawnAt,
    }).toEqual({
      memoryGroup: "memory",
      memoryDrawMode: "random",
      memoryDrawnAt: "2026-09-20T11:04:05Z",
    });
  }

  const drawn = state.issues["LEGION-208"];
  const parses = () => LegionStateResponse.safeParse(state).success;
  expect(parses()).toBeTrue();

  // A state from a daemon before contract 49, and one served to a pod below it.
  for (const key of ["memoryGroup", "memoryDrawMode", "memoryDrawnAt"]) {
    delete drawn[key];
  }
  expect(parses()).toBeTrue();

  // A word no draw ever wrote, and a draw time that is no time.
  drawn.memoryGroup = "both";
  expect(parses()).toBeFalse();
  drawn.memoryGroup = "none";
  expect(parses()).toBeTrue();
  drawn.memoryDrawMode = "sometimes";
  expect(parses()).toBeFalse();
  drawn.memoryDrawMode = "off";
  drawn.memoryDrawnAt = "the morning";
  expect(parses()).toBeFalse();
});

// Contract 141: the state golden carries LEGION-208, a root, and LEGION-209, a child of its
// tree, each under two experiments — the tree experiment `memory`, whose answer every issue of
// the tree shows, and the issue experiment `plan-length`, drawn for each issue on its own, so
// the two stand in different arms of it — and LEGION-210, whose hand-over settled it at no arm:
// an empty variant, an empty mode and no instant. An issue no experiment holds an answer for
// carries no key at all, as does every issue of the body served a caller before 141, so the
// field is optional; a daemon before this contract sent none, and its state must still parse.
test("the state golden carries the experiments each issue was drawn for", () => {
  const state = fixture("state.json") as {
    issues: Record<string, Record<string, unknown>>;
  };
  const issueOf = (key: string) => {
    const issue = state.issues[key];
    if (issue === undefined) {
      throw new Error(`state fixture lacks ${key}`);
    }
    return issue;
  };
  const drawn = issueOf("LEGION-208");
  expect(drawn.assignments).toEqual([
    {
      experiment: "memory",
      variant: "memory",
      mode: "random",
      drawnAt: "2026-09-20T11:04:05Z",
    },
    {
      experiment: "plan-length",
      variant: "short",
      mode: "random",
      drawnAt: "2026-09-21T18:42:11Z",
    },
  ]);
  // The issue experiment's arm is the issue's own: the child of that tree stands in the other.
  expect((issueOf("LEGION-209").assignments as Record<string, unknown>[])[1]?.variant).toBe("long");
  // The hand-over's answer: an arm and a mode, both empty, and no instant at all.
  expect(issueOf("LEGION-210").assignments).toEqual([
    { experiment: "plan-length", variant: "", mode: "" },
  ]);
  // An issue no experiment has reached carries no key.
  expect(issueOf("LEGION-211").assignments).toBeUndefined();
  // The group an experiment that sets how its issues are done gave the child, as its draw
  // recorded it: the parse keeps it, and a group that is no string is refused.
  const planned = LegionStateResponse.parse(state).issues["LEGION-209"]?.assignments?.find(
    (row) => row.experiment === "planning"
  );
  expect(planned?.plan).toBe("default");

  const parses = () => LegionStateResponse.safeParse(state).success;
  expect(parses()).toBeTrue();

  // A state from a daemon before contract 141, and one served to a pod below it.
  const held = drawn.assignments;
  delete drawn.assignments;
  expect(parses()).toBeTrue();
  drawn.assignments = held;
  expect(parses()).toBeTrue();

  // A row naming no experiment, a variant that is no string, and a draw time that is no time.
  drawn.assignments = [{ experiment: "", variant: "memory", mode: "random" }];
  expect(parses()).toBeFalse();
  drawn.assignments = [{ experiment: "memory", variant: 1, mode: "random" }];
  expect(parses()).toBeFalse();
  drawn.assignments = [
    { experiment: "memory", variant: "memory", mode: "random", drawnAt: "the morning" },
  ];
  expect(parses()).toBeFalse();
  // A row that leaves out `variant` or `mode` is no row this daemon writes, and no plugin code
  // reads either field (LEGION_ANSWER_READS), so the reader takes the row with the key lost
  // rather than refuse the whole state over a field it never reads (dispatch://INFRA-2694, R4).
  drawn.assignments = [{ experiment: "memory", mode: "random" }];
  expect(parses()).toBeTrue();
  drawn.assignments = [{ experiment: "memory", variant: "memory" }];
  expect(parses()).toBeTrue();
  // A group that is no string, and an empty one: the daemon leaves the key off where the arm
  // named no group.
  drawn.assignments = [{ experiment: "planning", variant: "b", mode: "random", plan: 1 }];
  expect(parses()).toBeFalse();
  drawn.assignments = [{ experiment: "planning", variant: "b", mode: "random", plan: "" }];
  expect(parses()).toBeFalse();
  drawn.assignments = held;
  expect(parses()).toBeTrue();
});

// Contract 246: the state golden carries LEGION-219, whose quiet pod the idle rule has held for
// background work since the evening before: the reason in the rule's own word, the first and the
// newest pass of the streak, and the kind the session reported. An issue whose pod no hold stands
// on carries no key at all, as does every issue of the body served a caller before 246, so the
// field is optional; a daemon before this contract sent none, and its state must still parse.
test("the state golden carries a held pod's standing hold", () => {
  const state = fixture("state.json") as {
    issues: Record<string, Record<string, unknown>>;
  };
  const kept = state.issues["LEGION-219"];
  if (kept === undefined) {
    throw new Error("state fixture lacks LEGION-219");
  }
  expect(kept.podHold).toEqual({
    reason: "background_work",
    firstAt: "2026-09-21T22:04:00Z",
    at: "2026-09-22T09:17:00Z",
    detail: "async-job",
  });
  expect(state.issues["LEGION-208"]?.podHold).toBeUndefined();

  const parses = () => LegionStateResponse.safeParse(state).success;
  expect(parses()).toBeTrue();

  // A state from a daemon before contract 246, and one served to a caller below it.
  const held = kept.podHold;
  delete kept.podHold;
  expect(parses()).toBeTrue();
  // A reason a later daemon adds is read rather than refused, and a hold with no detail is the
  // shape of every hold whose record holds none.
  kept.podHold = {
    reason: "a_later_hold",
    firstAt: "2026-09-21T22:04:00Z",
    at: "2026-09-22T09:17:00Z",
  };
  expect(parses()).toBeTrue();
  // An empty reason, a reason that is no string, and a pass time that is no time.
  kept.podHold = { reason: "", firstAt: "2026-09-21T22:04:00Z", at: "2026-09-22T09:17:00Z" };
  expect(parses()).toBeFalse();
  kept.podHold = { reason: 7, firstAt: "2026-09-21T22:04:00Z", at: "2026-09-22T09:17:00Z" };
  expect(parses()).toBeFalse();
  kept.podHold = { reason: "image", firstAt: "the evening", at: "2026-09-22T09:17:00Z" };
  expect(parses()).toBeFalse();
  kept.podHold = held;
  expect(parses()).toBeTrue();
});

// The state golden carries LEGION-207, closed by its merge, with its production check open: the
// repository, pull request and merge commit the check looks for, the role that works it, its
// looks, the wake that started the latest and when the next look is due. Its role is the
// implementer, a check opened before the release that made the role the project's choice, which
// the migration leaves on every check already open. The check is present only while open, so an
// issue without one parses, and a wake with no kind, or a check with no role, is refused.
test("the state golden carries a done issue's open production check", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { phase: string; check?: Record<string, unknown> }>;
  };
  const closed = state.issues["LEGION-207"];
  if (closed === undefined || closed.check === undefined) {
    throw new Error("state fixture lacks LEGION-207 or its check");
  }
  expect(closed.phase).toBe("done");
  expect(closed.check).toEqual({
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
  });

  // Before the first look wakes there is no wake, and a merge commit the daemon never recorded is
  // empty: nothing fills it now, and the checking agent reads it from GitHub.
  const { lastWake: _, ...unwoken } = closed.check;
  closed.check = { ...unwoken, mergeCommit: "", looks: 0, lookOpen: false };
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  closed.check = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  closed.check = { ...unwoken, lastWake: { kind: "", detail: "", at: "2026-09-22T09:10:51Z" } };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();

  closed.check = { ...unwoken, role: "" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

// The state golden carries two reviewing issues whose rounds wait on GitHub's merge state for
// their heads: LEGION-218, waiting for a first answer — the head it waits on, when GitHub last
// moved that head and no settled check run on it yet, with no state read and no told marks — and
// LEGION-219, stuck, carrying the state read and both marks the telling left. A round waiting on
// nothing from GitHub has no `mergeWait` at all, and a merge state no GitHub answer spells, or a
// state read that is no time, is refused.
test("the state golden carries the rounds waiting on GitHub's merge state", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { phase: string; mergeWait?: Record<string, unknown> }>;
  };
  const first = state.issues["LEGION-218"];
  const stuck = state.issues["LEGION-219"];
  if (first?.mergeWait === undefined || stuck?.mergeWait === undefined) {
    throw new Error("state fixture lacks LEGION-218 or LEGION-219 or their merge waits");
  }
  expect(first.phase).toBe("reviewing");
  expect(stuck.phase).toBe("reviewing");

  // LEGION-218's fixture: no merge state, no read time, and neither told mark.
  expect(first.mergeWait).toEqual({
    waitsOn: "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063",
    headUpdatedAt: "2026-09-22T08:58:40Z",
    checkRuns: 0,
  });
  // Stuck: GitHub answers that the head is behind its base, a state the round does not end on;
  // the architect and the reviewer have been told, and the marks name the head and state told of.
  expect(stuck.mergeWait).toEqual({
    waitsOn: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
    mergeState: "behind",
    stateReadAt: "2026-09-22T09:16:12Z",
    headUpdatedAt: "2026-09-20T14:05:33Z",
    checkRuns: 2,
    toldHead: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
    toldState: "behind",
  });

  const told = stuck.mergeWait;
  // A round that waits on nothing from GitHub, and the body the route serves a caller below the
  // contract that added the merge wait (api's mergeWaitContract).
  first.mergeWait = undefined;
  stuck.mergeWait = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  stuck.mergeWait = { ...told, mergeState: "stuck", toldState: "stuck" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();

  stuck.mergeWait = { ...told, stateReadAt: "the morning" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();

  stuck.mergeWait = { ...told, waitsOn: "" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

// The checking role's report carries no commit (the look pushed nothing) and a verdict the
// daemon holds to pass, pending or fail; a phase completion keeps its commit. A pending report
// names when the agent asks to be woken for its next look, an offset timestamp.
test("a check report is a completion without a commit", () => {
  const report = { grantId: "grant-207", summary: "Driven in production." };
  for (const verdict of ["pass", "pending", "fail"]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...report, verdict }).success).toBeTrue();
  }
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...report, verdict: "pass", commit: "" }).success
  ).toBeFalse();
  expect(
    LegionHandoffCompleteRequest.safeParse({
      ...report,
      verdict: "pending",
      wakeAt: "2026-10-19T09:00:00Z",
    }).success
  ).toBeTrue();
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...report, verdict: "pending", wakeAt: "tomorrow" })
      .success
  ).toBeFalse();
  expect(
    LegionHandoffCompleteRequest.safeParse({
      ...report,
      verdict: "pending",
      wake_at: "2026-10-19T09:00:00Z",
    }).success
  ).toBeFalse();
});

// What `z.iso.datetime({ offset: true })` alone would take and Go's `time.Time` decode refuses:
// a time with no seconds. The plugin's `wake_at` validates against this same schema
// (`packages/pi-shared/src/legion-phase-ops.ts`), so both sides refuse the same strings and the
// agent is told which field is wrong rather than reading the daemon's generic invalid-body error.
test("wakeAt refuses a time with no seconds, as the daemon's decode does", () => {
  const report = {
    grantId: "grant-207",
    summary: "Driven in production.",
    verdict: "pending",
  };
  const refused = LegionHandoffCompleteRequest.safeParse({
    ...report,
    wakeAt: "2026-11-06T10:00Z",
  });
  expect(refused.success).toBeFalse();
  expect(refused.error?.issues.map((issue) => issue.path.join("."))).toEqual(["wakeAt"]);
  expect(LegionInstant.safeParse("2026-11-06T10:00Z").success).toBeFalse();
  for (const wakeAt of [
    "2026-11-06T10:00:00Z",
    "2026-11-06T10:00:00.5Z",
    "2026-11-06T10:00:00+02:00",
  ]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...report, wakeAt }).success).toBeTrue();
  }
});

// A file-backed phase's completion names the Dispatch handoff record the worker wrote: the
// planner, tester and reviewer report it alone, the implementer with the commit it pushed. A record
// is named by a non-empty id and a positive version.
test("a phase completion names its handoff record", () => {
  const planner = {
    grantId: "grant-208",
    summary: "Planned.",
    verdict: "",
    handoffId: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b",
    handoffVersion: 1,
  };
  expect(LegionHandoffCompleteRequest.safeParse(planner).success).toBeTrue();
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...planner, commit: "abc123" }).success
  ).toBeTrue();
  expect(LegionHandoffCompleteRequest.safeParse({ ...planner, handoffId: "" }).success).toBeFalse();
  for (const handoffVersion of [0, -1, 1.5]) {
    expect(
      LegionHandoffCompleteRequest.safeParse({ ...planner, handoffVersion }).success
    ).toBeFalse();
  }
});

// The completion that ends the retro rates the plan the issue ran: `planRating`, a whole number from
// 1 to 5, and `planReason`, why, in one to 2,000 characters counted as the daemon counts them
// (Unicode code points) and not all blank, the two together. A completion that carries neither
// stays what it was.
test("the retro's completion rates the plan from 1 to 5 with a reason", () => {
  const completion = { grantId: "g", summary: "s", verdict: "" };
  const retro = { ...completion, planRating: 4, planReason: "r" };
  expect(LegionHandoffCompleteRequest.parse(retro)).toEqual(retro);
  expect(LegionHandoffCompleteRequest.safeParse(completion).success).toBeTrue();
  for (const planRating of [1, 5]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...retro, planRating }).success).toBeTrue();
  }
  for (const planRating of [0, 6, -1, 4.5]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...retro, planRating }).success).toBeFalse();
  }
  for (const planReason of ["", " \t\n", "a".repeat(2001)]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...retro, planReason }).success).toBeFalse();
  }
  // 2,000 code points of two UTF-16 units each is 2,000 characters, as the daemon counts them.
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...retro, planReason: "😀".repeat(2000) }).success
  ).toBeTrue();
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...completion, planRating: 4 }).success
  ).toBeFalse();
  expect(
    LegionHandoffCompleteRequest.safeParse({ ...completion, planReason: "r" }).success
  ).toBeFalse();
});

// Go writes the rated completion from its own struct, so this is the one place its JSON names of
// the rating meet the contract's: every other test of either side writes its own side's names.
// The request schema is strict, so a name one side renames alone fails the parse, and the parse
// keeps the rating and its reason under the contract's names, values no default and no other
// field of the request holds.
test("the rated completion the daemon reads parses under the contract's names", () => {
  const request = LegionHandoffCompleteRequest.parse(
    fixture("handoff-complete-request-rated.json")
  );
  expect(request.planRating).toBe(3);
  expect(request.planReason).toBe("The tester found the bug the plan had left out.");
});

// A rating's rule is the daemon's, and the request schema states it again: the scale, the pair,
// blank and the bound. Blank is Go's strings.TrimSpace on the daemon's side, so the schema reads
// it by goBlankPattern rather than by JavaScript's trim, and `./go-blank.ts`'s doc comment names
// the two characters on which the two differ. Every case the daemon's golden test answered is run
// through the schema, and the schema must take exactly the ones the daemon accepted.
test("the request schema takes exactly the ratings the daemon takes", () => {
  const { cases } = PlanRatingCases.parse(fixture("plan-rating-cases.json"));
  const parted = cases
    .filter((c) => {
      const { name: _name, verdict, ...rating } = c;
      const taken = LegionHandoffCompleteRequest.safeParse({
        grantId: "g",
        summary: "s",
        verdict: "",
        ...rating,
      }).success;
      return taken !== (verdict === "accepted");
    })
    .map((c) => `${c.name}: the daemon answers ${c.verdict}`);
  expect(parted).toEqual([]);
});

// Contract 219: a step that failed names the step its task returns to, `returnTo`, by its place in
// the task, 1 for its first. Only a completion with the verdict fail names one
// (RETURN_TO_NEEDS_FAIL), and a place below 1 is none (INVALID_RETURN_TO). A completion that
// names none stays what it was.
test("a failed step's completion names the step its task returns to", () => {
  const failed = { grantId: "g", summary: "s", verdict: "fail", returnTo: 2 };
  expect(LegionHandoffCompleteRequest.parse(failed)).toEqual(failed);
  expect(LegionHandoffCompleteRequest.safeParse({ ...failed, returnTo: 1 }).success).toBeTrue();
  for (const returnTo of [0, -1, 1.5]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...failed, returnTo }).success).toBeFalse();
  }
  for (const verdict of ["pass", "pending", ""]) {
    expect(LegionHandoffCompleteRequest.safeParse({ ...failed, verdict }).success).toBeFalse();
    const { returnTo: _, ...without } = { ...failed, verdict };
    expect(LegionHandoffCompleteRequest.safeParse(without).success).toBeTrue();
  }
});

// Contract 219: register_plan answers the plan it took, its version and each task with the steps
// its block names as the plan writes them (`role` or `role:key`). A task whose block names none
// runs the project's default steps and is answered with no step, an empty list and never null.
// No plugin code reads a field of the answer, so the reader takes each as optional and refuses
// only a value of the wrong type.
test("register_plan answers each task of the plan it took with its steps", () => {
  expect(LegionPlanRegisterResponse.parse(fixture("plan-register.json"))).toEqual({
    plan: { version: 4, tasks: [{ id: "a", steps: ["implementer", "reviewer"] }] },
  });
  const none = { plan: { version: 1, tasks: [{ id: "a", steps: [] }] } };
  expect(LegionPlanRegisterResponse.parse(none)).toEqual(none);
  for (const task of [
    { id: "a", steps: null },
    { id: "", steps: [] },
  ]) {
    expect(
      LegionPlanRegisterResponse.safeParse({ plan: { version: 1, tasks: [task] } }).success
    ).toBeFalse();
  }
  expect(
    LegionPlanRegisterResponse.safeParse({ plan: { version: 0, tasks: none.plan.tasks } }).success
  ).toBeFalse();
});

// Contract 219 widens one value set: a held issue's `holdReason` is `escalated`, `task-stopped`
// (its owner stopped its task with stop_task) or `children` (a parent whose task's last step is
// not the owner's acceptance waits, before its retro, for the issues beneath it). A state with a stopped
// task and a held parent parses, each reason kept; a reason outside the three is refused.
test("a state with a stopped task and a parent held for its children parses", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { phase: string; holdReason?: string; heldFrom?: string }>;
  };
  const stopped = state.issues["LEGION-210"];
  if (stopped === undefined) throw new Error("state fixture lacks LEGION-210");
  const parent = { ...stopped, key: "LEGION-230", holdReason: "children", heldFrom: "retro" };
  state.issues["LEGION-230"] = parent;
  stopped.holdReason = "task-stopped";
  const parsed = LegionStateResponse.parse(state);
  expect(parsed.issues["LEGION-210"]?.holdReason).toBe("task-stopped");
  expect(parsed.issues["LEGION-230"]?.holdReason).toBe("children");
  expect(parsed.issues["LEGION-230"]?.heldFrom).toBe("retro");
  stopped.holdReason = "stopped";
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

// Contract 219 gives each of those two holds its own word in `admission.held`:
// `children_before_retro`, a parent held before its retro, whose `heldBy` names each issue beneath
// it still in the workflow, and `task_stopped`, a stopped task that keeps no slot, with no
// `heldBy`. A state whose held rows carry them parses, each row kept as served; a word outside
// `LEGION_HELD_REASONS` is refused.
test("a state whose held rows name a parent held before its retro and a stopped task parses", () => {
  const state = fixture("state.json") as {
    admission: { held: { issue: string; reason: string; heldBy?: string[] }[] };
  };
  type HeldRows = NonNullable<
    NonNullable<z.output<typeof LegionStateResponse>["admission"]>["held"]
  >;
  const rows = [
    { issue: "LEGION-230", reason: "children_before_retro", heldBy: ["LEGION-231"] },
    { issue: "LEGION-232", reason: "task_stopped" },
  ] satisfies HeldRows;
  state.admission.held.push(...rows);
  expect(LegionStateResponse.parse(state).admission?.held?.slice(-2)).toEqual(rows);
  state.admission.held.push({ issue: "LEGION-233", reason: "stopped" });
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

test("the state parser accepts the new held reason and rejects its retired spelling", () => {
  const state = fixture("state.json") as {
    admission: { held: { issue: string; reason: string; heldBy?: string[] }[] };
  };
  const held = {
    issue: "LEGION-231",
    reason: "tree_in_backlog_or_icebox" as const,
    heldBy: ["LEGION-230"],
  };
  state.admission.held.push(held);
  expect(LegionStateResponse.parse(state).admission?.held?.at(-1)).toEqual(held);
  state.admission.held[state.admission.held.length - 1] = { ...held, reason: "tree_parked" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
});

// The state shows the record a worker's completion of its current file-backed phase named, its id
// and positive version; a worker that named none shows no field.
test("the state golden carries a worker's handoff record", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { handoffRecord?: unknown }> }>;
  };
  const planner = state.issues["LEGION-208"]?.workers.planner;
  if (planner === undefined) {
    throw new Error("state fixture lacks LEGION-208's planner");
  }
  expect(planner.handoffRecord).toEqual({ id: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", version: 1 });

  planner.handoffRecord = { id: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", version: 0 };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  planner.handoffRecord = { id: "", version: 1 };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  planner.handoffRecord = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);
});

// The state shows what the daemon last saw a claim's agent do: the turn it is in, the call it is
// in, and the length and fingerprint of that call's arguments, never their text. A call still
// open carries `startedAt` and no `endedAt`, which is the shape a turn that stopped moving
// leaves behind; a claim whose agent has not stepped carries no `step` at all, as does every
// claim view served to a pod below contract 57.
test("the state golden carries what the daemon last saw an agent do", () => {
  const state = fixture("state.json") as {
    issues: Record<
      string,
      {
        architect?: { step?: Record<string, unknown> };
        workers: Record<string, { claim: { step?: Record<string, unknown> } }>;
      }
    >;
  };
  const implementer = state.issues["LEGION-208"]?.workers.implementer?.claim;
  const architect = state.issues["LEGION-208"]?.architect;
  if (implementer === undefined || architect === undefined) {
    throw new Error("state fixture lacks LEGION-208's architect and implementer");
  }
  expect(implementer.step).toEqual({
    at: "2026-09-22T09:17:12Z",
    turnStartedAt: "2026-09-22T09:16:30Z",
    tool: "bash",
    callId: "toolu_impl_11",
    argsBytes: 128,
    argsHash: "9b2e45c0d1f8a673",
    startedAt: "2026-09-22T09:17:12Z",
  });
  expect(architect.step?.endedAt).toBe("2026-09-22T09:16:58Z");

  implementer.step = { argsBytes: 0 };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  implementer.step = { tool: "" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  implementer.step = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);
});

// The only surface a controller's record reaches is the operator's claim listing: the state puts
// the controller's claim on no issue, so no claim view of it can carry one.
test("an operator's claim carries what the daemon last saw its agent do", () => {
  const claim = fixture("operator-claim.json") as { step?: Record<string, unknown> };
  expect(claim.step).toEqual({
    at: "2026-09-22T09:31:22Z",
    turnStartedAt: "2026-09-22T09:31:06Z",
    tool: "eval",
    callId: "toolu_impl_209_4",
    argsBytes: 412,
    argsHash: "c40d9e1b7a52f368",
    startedAt: "2026-09-22T09:31:22Z",
  });

  claim.step = { argsBytes: -1 };
  expect(LegionOperatorClaimResponse.safeParse(claim).success).toBeFalse();
  claim.step = undefined;
  expect(LegionOperatorClaimResponse.safeParse(claim).error?.issues ?? []).toEqual([]);
});

// Contract 75: a claim the daemon holds a cause for says why it failed — the one word of
// `supervise.FailureCause`, the sentence beside it, the moment and the counts it failed with —
// and a held issue says which phase its hold stopped, beside the reason the hold may have. The
// state golden carries LEGION-210, held from implementing after its implementer failed, and the
// forget-session golden carries the operator's shape of the same record. A claim that never
// failed carries no `failure`, a backfilled one carries no `at`, and a word the seven do not
// name is refused rather than read.
test("a failed claim carries its cause", () => {
  const state = fixture("state.json") as {
    issues: Record<
      string,
      {
        holdReason?: string;
        heldFrom?: string;
        workers: Record<string, { claim: { failure?: Record<string, unknown> } }>;
      }
    >;
  };
  const held = state.issues["LEGION-210"];
  const failed = held?.workers.implementer?.claim;
  if (held === undefined || failed === undefined) {
    throw new Error("state fixture lacks LEGION-210's implementer");
  }
  expect({ holdReason: held.holdReason, heldFrom: held.heldFrom }).toEqual({
    holdReason: "escalated",
    heldFrom: "implementing",
  });
  const record = {
    cause: "died" as const,
    detail: "launch failures ran out",
    at: "2026-09-22T08:47:03Z",
    attempts: { launchFailures: 4, deaths: 1, promptFailures: 0, promptRetires: 0 },
  };
  expect(failed.failure).toEqual(record);

  // Both keys survive the parse: the answer side strips what it does not name, so a reader of
  // the state sees them only because `legion-api.ts` names both.
  const parsed = LegionStateResponse.parse(state).issues["LEGION-210"];
  expect(parsed?.heldFrom).toBe("implementing");
  expect(parsed?.workers?.implementer?.claim?.failure).toEqual(record);

  // A failure the migration backfilled carries no moment, and the record outlives the claim's
  // retirement; a cause outside the seven words, or a held phase outside the workflow's, is
  // refused.
  failed.failure = {
    cause: "unrecorded",
    detail: "launch failures ran out",
    attempts: { launchFailures: 4, deaths: 1, promptFailures: 0, promptRetires: 0 },
  };
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);
  failed.failure = { ...failed.failure, cause: "exploded" };
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  failed.failure = undefined;
  held.heldFrom = "unrecorded";
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  held.heldFrom = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  const claim = fixture("operator-forget-session.json") as { failure?: Record<string, unknown> };
  expect(claim.failure).toEqual({
    cause: "launch_refused",
    detail: "launch failures ran out: resume the pane: the session table holds no such session",
    at: "2026-09-22T09:28:41Z",
    attempts: { launchFailures: 3, deaths: 0, promptFailures: 0, promptRetires: 0 },
  });
  // A failure record with no `attempts` is read as the two keys it carries, not refused: every
  // field this build does not name in its own read set is optional, so a daemon that stops
  // serving one loses that key rather than the body (daemon-client.test.ts).
  claim.failure = { cause: "died", detail: "it died" };
  expect(LegionOperatorClaimResponse.parse(claim).failure).toEqual({
    cause: "died",
    detail: "it died",
  });
  claim.failure = undefined;
  expect(LegionOperatorClaimResponse.safeParse(claim).error?.issues ?? []).toEqual([]);
});

// The set those words come from is closed, and it is written twice: the constants of
// `supervise.FailureCause` and `LEGION_FAILURE_CAUSES` here. Nothing compares two hand-kept
// lists, and a Go word with no word here would make this enum refuse the whole state body of
// every plugin that reads it. So Go writes its own roster into a fixture
// (`internal/api`'s TestFailureCausesGolden) and this holds the enum to it, word for word and
// in the daemon's own order: a cause added to one side alone fails here, and a cause renamed
// on either side does too.
test("the cause set is the daemon's, word for word", () => {
  const written = fixture("failure-causes.json") as { causes: string[] };
  expect(written.causes).toEqual([...LEGION_FAILURE_CAUSES]);
});

// The rest of contract 75: every field of the claim record and of the issue record is served or
// withheld by a decision (the daemon's `internal/api/recordfields.go`), and these are the seven
// keys that decision added beside `failure` and `heldFrom`. The schema must name each, since a
// key it does not name is stripped from the body and the seat never sees it: the state golden
// carries all seven, and this reads them before the parse and after it. The task's own text is
// in none of them — `pendingTask` is five facts, and the operator routes serve the text whole —
// so a `task` key under it would be a leak this catches.
test("a claim and an issue carry the fields the record decides they serve", () => {
  type claimView = {
    servingGeneration?: number;
    updateReasons?: string;
    providerErrorRun?: number;
    pendingTask?: Record<string, unknown>;
    pendingCause?: string;
  };
  const state = fixture("state.json") as {
    issues: Record<
      string,
      { lingerUntil?: string; ancestry?: string[]; workers: Record<string, { claim: claimView }> }
    >;
  };
  const working = state.issues["LEGION-208"]?.workers.implementer?.claim;
  const failed = state.issues["LEGION-210"]?.workers.implementer?.claim;
  const released = state.issues["LEGION-211"]?.workers.implementer?.claim;
  if (working === undefined || failed === undefined || released === undefined) {
    throw new Error(
      "state fixture lacks the implementers of LEGION-208, LEGION-210 and LEGION-211"
    );
  }
  expect({
    servingGeneration: working.servingGeneration,
    providerErrorRun: working.providerErrorRun,
    updateReasons: working.updateReasons,
    pendingTask: working.pendingTask,
  }).toEqual({
    servingGeneration: 3,
    providerErrorRun: 2,
    updateReasons: "the pod template changed: image ghcr.io/cybermonklabs/legion-worker moved",
    pendingTask: { phase: "implementing", queuedAt: "2026-09-22T09:16:58Z" },
  });
  // The sends that never reached a turn ride the task a released claim still holds.
  expect(released.pendingTask).toEqual({
    phase: "implementing",
    queuedAt: "2026-09-22T08:54:20Z",
    sendFailures: 2,
    lastSendError: "post the prompt: dial tcp 10.42.0.17:13371: connect: connection refused",
  });
  // The cause of the death whose relaunch the runtime refused, which stands beside `failure`.
  expect(failed.pendingCause).toBe("died");
  expect(state.issues["LEGION-207"]?.lingerUntil).toBe("2026-09-22T10:10:12Z");
  expect(state.issues["LEGION-211"]?.ancestry).toEqual(["LEGION-209", "LEGION-208"]);

  // All seven survive the parse: the answer side strips what it does not name.
  const parsed = LegionStateResponse.parse(state);
  const parsedWorking = parsed.issues["LEGION-208"]?.workers?.implementer?.claim;
  expect({
    servingGeneration: parsedWorking?.servingGeneration,
    providerErrorRun: parsedWorking?.providerErrorRun,
    updateReasons: parsedWorking?.updateReasons,
    pendingTask: parsedWorking?.pendingTask,
  }).toEqual({
    servingGeneration: 3,
    providerErrorRun: 2,
    updateReasons: "the pod template changed: image ghcr.io/cybermonklabs/legion-worker moved",
    pendingTask: { phase: "implementing", queuedAt: "2026-09-22T09:16:58Z" },
  });
  expect(parsed.issues["LEGION-210"]?.workers?.implementer?.claim?.pendingCause).toBe("died");
  expect(parsed.issues["LEGION-207"]?.lingerUntil).toBe("2026-09-22T10:10:12Z");
  expect(parsed.issues["LEGION-211"]?.ancestry).toEqual(["LEGION-209", "LEGION-208"]);
  // The pending task is the five facts and the text is not one of them, here or anywhere in the
  // body: the operator routes are the only surface that serves it.
  expect(Object.keys(parsedWorking?.pendingTask ?? {}).sort()).toEqual(["phase", "queuedAt"]);
  expect(JSON.stringify(parsed)).not.toContain('"task"');

  // A cause outside the seven words, a linger that is no instant and an empty key in the chain
  // are each refused rather than read.
  failed.pendingCause = "exploded";
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  failed.pendingCause = undefined;
  const lingering = state.issues["LEGION-207"];
  if (lingering === undefined) {
    throw new Error("state fixture lacks LEGION-207");
  }
  lingering.lingerUntil = "2026-09-22";
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  lingering.lingerUntil = undefined;
  const root = state.issues["LEGION-211"];
  if (root === undefined) {
    throw new Error("state fixture lacks LEGION-211");
  }
  root.ancestry = [""];
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  root.ancestry = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);
});

// Contract 87: a claim whose launch the runtime refused because the shared public leak check
// could not be installed in the role's pod carries that sentence under a key of its own, on both
// shapes the daemon serves a claim in — the state's claim views, which every worker and architect
// reads, and the operator claim, which is where the controller's own is read. The schema must
// name the key on both, since a key it does not name is stripped and the seat never sees it: the
// goldens carry it, and this reads it before the parse and after. A daemon narrows it away for a
// caller below 87 (`api.withoutLeakCheckCause`), so a body without it parses, and an empty
// sentence — which the daemon never writes, leaving the key off instead — is refused.
test("a claim carries the leak check's cause beside its failure", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { claim: { leakCheckCause?: string } }> }>;
  };
  const refused = state.issues["LEGION-210"]?.workers.implementer?.claim;
  if (refused === undefined) {
    throw new Error("state fixture lacks LEGION-210's implementer");
  }
  const sentence =
    "the shared public leak check could not be installed: GitHub answered the read of example-org/deployment at leak-check-v3 with HTTP 404: Not Found";
  expect(refused.leakCheckCause).toBe(sentence);
  expect(
    LegionStateResponse.parse(state).issues["LEGION-210"]?.workers?.implementer?.claim
      ?.leakCheckCause
  ).toBe(sentence);

  refused.leakCheckCause = "";
  expect(LegionStateResponse.safeParse(state).success).toBeFalse();
  refused.leakCheckCause = undefined;
  expect(LegionStateResponse.safeParse(state).error?.issues ?? []).toEqual([]);

  const listing = fixture("operator-claims.json") as {
    claims: { token: string; leakCheckCause?: string }[];
  };
  const launching = listing.claims.find(
    (claim) => claim.token === "legion-legion-legion-211-tester"
  );
  if (launching === undefined) {
    throw new Error("operator-claims fixture lacks legion-legion-legion-211-tester");
  }
  const pending =
    "the shared public leak check could not be installed: the pod's launcher did not answer the install of leak-check-v3 within 2m0s";
  expect(launching.leakCheckCause).toBe(pending);
  expect(
    LegionOperatorClaimsResponse.parse(listing).claims.find(
      (claim) => claim.token === "legion-legion-legion-211-tester"
    )?.leakCheckCause
  ).toBe(pending);
  launching.leakCheckCause = "";
  expect(LegionOperatorClaimsResponse.safeParse(listing).success).toBeFalse();
});

// The mark of the narrowing, the last key of contract 75: the body the daemon serves a caller at
// or above the contract carries the two numbers — the contract the daemon declares and the one
// it shaped the body to — and the body it narrows for a caller below it carries no such key at
// all, since a key added to a narrowed body is exactly what the narrowing exists to prevent. The
// daemon writes that narrowing to its own log instead (`api.recordShaping`). From contract 193 on,
// a reader served the body whole, the operator's `legion state --json` among them, is told the
// daemon's own number as the caller's too. Before contract 193 a daemon from 75 on served that
// reader the daemon's number alone, and the schema reads that mark as well.
//
// The golden carries no mark: `TestStateGolden` takes out each one that names the daemon's own
// number, which an entry moves, and holds the two to that number apart, so no golden changes with
// the build's number. The marks are written here, on copies of the golden, with the number of the
// build this checkout is (`daemon-api-records.ts`).
test("the state says which contract it was built at", () => {
  const { contract } = daemonApiRecords(fixtureDir);
  const golden = fixture("state.json") as { contract?: Record<string, unknown> };
  expect(golden.contract).toBeUndefined();

  const marked = fixture("state.json") as { contract?: Record<string, unknown> };
  marked.contract = { daemon: contract, caller: contract };
  expect(LegionStateResponse.parse(marked).contract).toEqual({
    daemon: contract,
    caller: contract,
  });

  const older = fixture("state.json") as { contract?: Record<string, unknown> };
  older.contract = { daemon: contract };
  expect(LegionStateResponse.parse(older).contract).toEqual({ daemon: contract });

  // A narrowed body carries no mark, and parses: the readers of this schema are never served one
  // — a caller below 75 reads with the schema of its own contract — and the state as
  // internal/projection answers it, before the route marks it, carries none either.
  expect(LegionStateResponse.safeParse(golden).error?.issues ?? []).toEqual([]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).contract).toBeUndefined();

  // A number that is no contract is refused rather than read. A mark naming the caller alone is
  // read instead: every field of an answer this build does not name in its own read set is
  // optional, so a daemon that leaves `daemon` off loses that key here and not the whole body
  // (daemon-client.test.ts holds that rule over every answer schema).
  const wrong = fixture("state.json") as { contract?: Record<string, unknown> };
  wrong.contract = { daemon: 0 };
  expect(LegionStateResponse.safeParse(wrong).success).toBeFalse();
  wrong.contract = { caller: contract };
  expect(LegionStateResponse.parse(wrong).contract).toEqual({ caller: contract });
});

// An architect's backward move names the issue it moves from accepting; a phase worker's may
// leave it out, since its grant names its own.
test("a backward move may name its issue", () => {
  const move = { grantId: "grant-208", to: "testing", reason: "the proof is thin" };
  expect(LegionPhaseBackwardRequest.safeParse(move).success).toBeTrue();
  expect(LegionPhaseBackwardRequest.safeParse({ ...move, issue: "LEGION-209" }).success).toBeTrue();
  expect(LegionPhaseBackwardRequest.safeParse({ ...move, issue: "" }).success).toBeFalse();
});

// Check c4: every schema the module exports whose name does not end in `Response` is a body the
// plugin sends or a part of one, and each refuses a body carrying a key it does not name — at its
// own level and at every object below it. The roster is derived from the module's exports, so a
// request shape a later change adds is held to this without anyone adding a row, and the walk
// reaches each nested object, the capability report's `rows` among them, rather than a row of the
// case below standing in for one.
test("every request-side object refuses a key it does not name", () => {
  const roster = exportedSchemas("request");
  const found = objectCensus(roster);
  const names = Object.keys(roster).sort();
  const nodes = found.strips.length + found.loose.length + found.refuses.length;
  console.log(
    `request roster (${names.length} schemas, ${nodes} object nodes): ${names.join(", ")}`
  );

  // A predicate that matched nothing would leave every assertion below holding vacuously.
  expect(names.length).toBeGreaterThan(0);
  expect(found.strips).toEqual([]);
  expect(found.loose).toEqual([]);
  expect(found.refuses.length).toBeGreaterThan(0);
});

// What only a real body can prove, route by route: the body each route sends parses, and a body
// missing a required field is refused. That a body carrying a key no schema names is refused is
// held at every depth by the census above, for every request schema the module exports.
test("every request the plugin sends parses and names its required fields", () => {
  const ready = fixtureBody("ready.json");
  const readyForUpdate = fixtureBody("ready-for-update.json");
  const report = LegionCapabilityReport.parse(ready.capabilities);
  const activity = {
    at: "2026-11-05T05:10:00Z",
    backgroundWork: { running: true, kinds: ["subagent"] },
    phase: { nextReminderAt: null },
  };
  const activityRequest = {
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer_208",
    secret: CLAIM_SECRET,
    activity,
  };
  // A route and a body it must accept, whose first key is a field the schema requires.
  const requests: ReadonlyArray<readonly [string, z.ZodType, Record<string, unknown>]> = [
    [
      "grant",
      LegionGrantRequest,
      { sessionId: "ses_208", secret: CLAIM_SECRET, tree: "LEGION-208", issue: "LEGION-209" },
    ],
    [
      "controller grant",
      LegionControllerGrantRequest,
      { sessionId: "ses_controller", secret: "s" },
    ],
    [
      "handoff complete",
      LegionHandoffCompleteRequest,
      {
        grantId: "grant-208",
        summary: "completed",
        verdict: "",
        commit: "abc123",
        handoffId: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b",
        handoffVersion: 2,
      },
    ],
    [
      "issue status",
      LegionIssueStatusRequest,
      { grantId: "grant-208", issue: "LEGION-208", status: "todo" },
    ],
    [
      "project pause",
      LegionProjectPauseRequest,
      { grantId: "controller-grant-1", reason: "the model provider is refusing every turn" },
    ],
    ["project resume", LegionProjectResumeRequest, { grantId: "controller-grant-1" }],
    // Each give-up route takes one record by its id, or a consumer through a stream sequence.
    [
      "replay one give-up",
      LegionIntakeReplayRequest,
      { grantId: "controller-grant-1", giveUp: 41 },
    ],
    [
      "replay a consumer's give-ups",
      LegionIntakeReplayRequest,
      { grantId: "controller-grant-1", consumer: "legion-go-LEGION-dispatch", throughSeq: 6044896 },
    ],
    [
      "discard one give-up",
      LegionIntakeDiscardRequest,
      {
        grantId: "controller-grant-1",
        giveUp: 41,
        reason: "the listener published a non-JSON body",
      },
    ],
    [
      "discard a consumer's give-ups",
      LegionIntakeDiscardRequest,
      {
        grantId: "controller-grant-1",
        consumer: "legion-go-LEGION-dispatch",
        throughSeq: 6044896,
        reason: "the listener published a non-JSON body",
      },
    ],
    // The operator's pause is the CLI's body, not the plugin's; it is held here as the mirror of
    // the Go shape, as the escalation below is.
    [
      "operator project pause",
      LegionOperatorProjectPauseRequest,
      { reason: "the model provider is refusing every turn" },
    ],
    [
      "claim recovery",
      LegionClaimRequest,
      { grantId: "grant-208", issue: "LEGION-209", role: "implementer" },
    ],
    ["pod update", LegionPodUpdateRequest, { grantId: "grant-208", issue: "LEGION-209" }],
    [
      "gate register",
      LegionGateRegisterRequest,
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
    ],
    ["wave release", LegionWaveReleaseRequest, { grantId: "grant-208", issues: ["LEGION-209"] }],
    [
      "phase backward",
      LegionPhaseBackwardRequest,
      { grantId: "grant-208", to: "implementing", reason: "test failed" },
    ],
    [
      "phase retry",
      LegionPhaseRetryRequest,
      { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
    ],
    [
      "escalate",
      LegionEscalateRequest,
      { grantId: "grant-208", issue: "LEGION-208", reason: "needs controller triage" },
    ],
    ["signoff", LegionSignOffRequest, { grantId: "grant-208", issue: "LEGION-208" }],
    [
      "root close",
      LegionRootCloseRequest,
      { grantId: "grant-208", issue: "LEGION-208", reason: "no change" },
    ],
    ["child", LegionChildRequest, { grantId: "grant-208", issue: "LEGION-209" }],
    [
      "register plan",
      LegionPlanRegisterRequest,
      {
        grantId: "grant-208",
        issue: "LEGION-209",
        artifactId: "8f14e45f-ceea-467f-a0e6-1d8b2f8c9d11",
        version: 4,
      },
    ],
    ["stop task", LegionTaskStopRequest, { grantId: "grant-208", issue: "LEGION-209", task: "a" }],
    // The claim wire's own bodies (contracts 21 and 46), read from the goldens the Go side
    // writes rather than invented here.
    ["ready", LegionReadyRequest, ready],
    ["ready for update", LegionReadyForUpdateRequest, readyForUpdate],
    ["capability report", LegionCapabilityReport, report],
    // The claim wire's activity report and the window a pod reads its release records over.
    ["activity", LegionActivityRequest, activityRequest],
    ["activity report", LegionSessionActivity, activity],
    [
      "release records",
      LegionReleaseRecordsRequest,
      {
        claimToken: "legion-legion-legion-208-implementer",
        secret: CLAIM_SECRET,
        since: "2026-11-01T12:00:00Z",
      },
    ],
  ];

  for (const [name, schema, request] of requests) {
    expect(schema.safeParse(request).success, `${name} accepts its route request`).toBeTrue();
    const [required] = Object.keys(request);
    const missing = { ...request };
    delete missing[required ?? ""];
    expect(
      schema.safeParse(missing).success,
      `${name} refuses a missing required field`
    ).toBeFalse();
  }
});

test("an escalation rejects empty required fields", () => {
  const request = { grantId: "grant-208", issue: "LEGION-208", reason: "needs controller triage" };
  for (const field of ["grantId", "issue", "reason"] as const) {
    expect(LegionEscalateRequest.safeParse({ ...request, [field]: "" }).success).toBeFalse();
  }
});

test("a workflow refusal preserves its stable code and message", () => {
  expect(
    LegionErrorResponse.safeParse({
      code: "GATE_UNAPPROVED",
      error: "the current spec version needs approval",
    }).success
  ).toBeTrue();
});

// `LegionEmptyResponse` names no field, so it accepts any JSON object a fact route answers; a
// field a fact route grows is caught by the drift gate above, not here. What it refuses is a body
// that is not an object at all.
test("the empty answer refuses a body that is not an object", () => {
  expect(LegionEmptyResponse.safeParse({}).success).toBeTrue();
  expect(LegionEmptyResponse.safeParse({ note: "ignored" }).success).toBeTrue();
  expect(LegionEmptyResponse.safeParse([]).success).toBeFalse();
  expect(LegionEmptyResponse.safeParse("{}").success).toBeFalse();
});

// Contract 60: the reader requires only the fields plugin code reads (`LEGION_ANSWER_READS`),
// so a body that leaves out a field no plugin code reads is read rather than refused — which is
// what the daemon serves a caller at a contract the field came after — and a body that leaves
// out a field the plugin reads is refused by that field's own path.
test("a field the plugin reads cannot be dropped, and a field it does not read can", () => {
  const unread = fixture("state.json") as { admission: Record<string, unknown> };
  delete unread.admission.waiting;
  expect(LegionStateResponse.safeParse(unread).success).toBeTrue();

  const read = fixture("state.json") as Record<string, unknown>;
  delete read.issues;
  const refused = LegionStateResponse.safeParse(read);
  expect(refused.success).toBeFalse();
  expect(refused.error?.issues[0]?.path.join(".")).toBe("issues");
});

// Contract 38: the state names every issue Legion is waiting to start behind a Dispatch issue
// nobody has finished — a child held at `admitted` and a waiting root alike — by issue key, each
// with the keys it waits on, among them keys the state's `issues` does not record. No plugin
// code reads either key, so a body without `blocked` is read and its value carries none, and
// `waiting` keeps its own meaning beside it: the admission line, which since contract 51
// carries children as well as roots. A `blockedBy` of a type the schema does not describe is
// still refused.
test("the state golden names what every blocked issue waits on", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  expect(state.admission?.blocked).toEqual([
    { issue: "LEGION-209", blockedBy: ["LEGION-210", "LEGION-213"] },
    { issue: "LEGION-211", blockedBy: ["LEGION-210"] },
  ]);
  expect(state.admission?.waiting).toEqual(["LEGION-212", "LEGION-211"]);

  const dropped = fixture("state.json") as { admission: Record<string, unknown> };
  delete dropped.admission.blocked;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();

  const malformed = fixture("state.json") as { admission: { blocked: unknown[] } };
  malformed.admission.blocked = [{ issue: "LEGION-209", blockedBy: "LEGION-210" }];
  expect(LegionStateResponse.safeParse(malformed).success).toBeFalse();
});

// Contract 51: the cap counts the issues at work, so `active` and `waiting` name issues of any
// depth and say nothing about which trees run. `trees` says it instead — the root of every live
// tree — and each issue's `tree` names its own root, so a child is told from a root. The computed
// golden carries a child: LEGION-209 stands in LEGION-208's tree, and all three live trees are
// named while one issue alone holds a slot. Neither key is required: a daemon at this contract
// strips both from the state it serves a caller whose pod's plugin is older than 51
// (`api.stateFor`, `withoutTrees`). So a body without them parses, and a reader of it
// learns nothing about which trees run rather than losing the whole state; what is refused is a
// `tree` that is no issue key. A daemon older than this plugin is kept away by the boot gate and
// `legion probe-image`, not by this schema.
test("the state names the live trees and each issue's own tree", () => {
  const state = LegionStateResponse.parse(fixture("state-controller-claim.json"));

  expect(state.admission?.active).toEqual(["LEGION-208"]);
  expect(state.admission?.trees).toEqual(["LEGION-208", "LEGION-210", "LEGION-211"]);
  expect(state.issues["LEGION-209"]?.tree).toBe("LEGION-208");
  expect(state.issues["LEGION-208"]?.tree).toBe("LEGION-208");

  const stripped = fixture("state.json") as {
    admission: Record<string, unknown>;
    issues: Record<string, { tree?: unknown }>;
  };
  delete stripped.admission.trees;
  for (const issue of Object.values(stripped.issues)) {
    delete issue.tree;
  }
  const served = LegionStateResponse.safeParse(stripped);
  expect(served.success).toBeTrue();
  expect(served.data?.admission?.trees).toBeUndefined();

  const malformed = fixture("state.json") as { issues: Record<string, { tree?: unknown }> };
  const issue = malformed.issues["LEGION-208"];
  if (issue === undefined) {
    throw new Error("state fixture lacks LEGION-208");
  }
  issue.tree = "not an issue key";
  expect(LegionStateResponse.safeParse(malformed).success).toBeFalse();
});

// Contract 38: a release is accepted whatever its children's links say, so the answer says which
// of them it started and which it left held, with what each held child waits on. No plugin code
// reads the answer — the `legion` tool hands it to the agent whole — so a body with `released`
// alone is read, and `held` carries the same `{issue, blockedBy}` row the state's `blocked` does.
test("a wave release answers what it started and what it held", () => {
  expect(LegionWaveReleaseResponse.parse(fixture("wave-release.json"))).toEqual({
    released: ["LEGION-209"],
    started: [],
    held: [],
  });
  expect(
    LegionWaveReleaseResponse.safeParse({
      released: ["LEGION-209", "LEGION-210"],
      started: ["LEGION-210"],
      held: [{ issue: "LEGION-209", blockedBy: ["LEGION-300"] }],
    }).success
  ).toBeTrue();
  expect(LegionWaveReleaseResponse.safeParse({ released: ["LEGION-209"] }).success).toBeTrue();
  expect(LegionWaveReleaseResponse.safeParse({ released: "LEGION-209" }).success).toBeFalse();
});

test("the Stage 3 state golden carries the record status and pending Dispatch status write", () => {
  const state = LegionStateResponse.parse(fixture("state-stage3.json"));

  expect(state.issues["LEGION-208"]?.status).toBe("needs_review");
  expect(state.pendingStatusWrites).toEqual([
    {
      issue: "LEGION-208",
      payload: { status: "needs_review" },
      attempts: 2,
      nextAt: "2026-09-22T17:32:00Z",
      lastError: "Dispatch unavailable",
    },
  ]);
});

// An issue's `status` is no field plugin code reads: the issue record is handed to the agent
// with it, and no line of the plugin takes its value. A body without it is read, and an issue
// whose `phase` is no phase this build knows is still refused.
test("a Stage 3 issue without its Dispatch status is read, and one without a phase is refused", () => {
  const state = fixture("state-stage3.json") as {
    issues: Record<string, Record<string, unknown>>;
  };
  delete state.issues["LEGION-208"]?.status;
  expect(LegionStateResponse.safeParse(state).success).toBeTrue();

  const phaseless = fixture("state-stage3.json") as {
    issues: Record<string, Record<string, unknown>>;
  };
  delete phaseless.issues["LEGION-208"]?.phase;
  expect(LegionStateResponse.safeParse(phaseless).success).toBeFalse();
});

/** The keys a `live` entry of the repositories block may carry at contract 202, sorted: `repo`,
 * and each other only where it holds something. Dispatch's webhook App is not among them: the
 * block's `withoutWebhookApp` is its one report. */
const LIVE_ENTRY_KEYS = ["apps", "cache", "readError", "repo", "servedAs"];

/** Each raw `live` entry of a written repositories block, as the file holds it. */
function liveEntries(block: unknown): unknown[] {
  const live = isJsonObject(block) ? block.live : undefined;
  return Array.isArray(live) ? live : [];
}

/** The keys of each raw `live` entry of a written repositories block that are not entry keys
 * (LIVE_ENTRY_KEYS), one list per entry in `live`'s order: a list is empty where its entry's keys
 * are a subset of the entry keys, and an entry that is no JSON object lists itself. */
function keysOutsideTheEntryKeys(block: unknown): string[][] {
  return liveEntries(block).map((entry) =>
    isJsonObject(entry)
      ? Object.keys(entry).filter((key) => !LIVE_ENTRY_KEYS.includes(key))
      : [JSON.stringify(entry)]
  );
}

/** Every key the raw `live` entries of a written repositories block carry, the union over the
 * entries, once each and sorted. */
function liveEntryKeys(block: unknown): string[] {
  const keys = liveEntries(block).flatMap((entry) =>
    isJsonObject(entry) ? Object.keys(entry) : []
  );
  return [...new Set(keys)].sort();
}

// Contract 31: every recorded issue carries the repository it targets and its default branch, and
// its pull request the repository it is in. Contract 32: a repository's cache under the Kubernetes
// runtime, Ready with its last health read or the read's failure, and the caches of repositories no
// live record names, listed with when they go. Contract 202: the block is `live`, `caches` and
// `withoutWebhookApp` and nothing else. `live` is one entry for each repository a live record
// names, sorted, with the name its pair serves it as where the rename rule serves it under the
// other name, each role App's installation once GitHub answered both reads, why GitHub's last read
// did not name it, and its cache, each absent until it holds something, and nothing else: each
// raw entry's keys are a subset of those keys, the union over the fixture's entries is all of them,
// so its entries carry each optional key between them, and an entry carrying a copy of the
// webhook App state, `webhookApp`, fails both. `withoutWebhookApp` is the block's one report of that
// state and keeps contract 152's shape for every caller, since a `legion state` of 152 reads its
// webhook App lines from it. A state without the block, without `live`, without
// `withoutWebhookApp` or without its caches is read: no plugin code reads any of them
// (`LEGION_ANSWER_READS`). An entry whose read error is empty is refused.
test("the state golden carries each issue's repository and the repositories its live records name", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  expect(state.issues["LEGION-208"]?.repo).toBe("acme/widgets");
  expect(state.issues["LEGION-208"]?.pullRequest?.repo).toBe("acme/widgets");
  expect(state.issues["LEGION-210"]?.defaultBranch).toBe("release/v7.1");
  // The file's own keys, not the parse's: the parse rebuilds the block from the schema's shape,
  // so its keys say nothing of what Go wrote.
  const written = fixtureBody("state.json").repositories;
  expect(isJsonObject(written) ? Object.keys(written).sort() : written).toEqual([
    "caches",
    "live",
    "withoutWebhookApp",
  ]);
  expect(keysOutsideTheEntryKeys(written)).toEqual(liveEntries(written).map(() => []));
  expect(liveEntryKeys(written)).toEqual(LIVE_ENTRY_KEYS);
  // What the comparisons refuse: one entry carrying `webhookApp` beside the block's list.
  const copied = fixture("state.json") as { repositories: { live: Record<string, unknown>[] } };
  Object.assign(copied.repositories.live[0] ?? {}, { webhookApp: "the webhook App check failed" });
  expect(keysOutsideTheEntryKeys(copied.repositories)[0]).toEqual(["webhookApp"]);
  expect(liveEntryKeys(copied.repositories)).not.toEqual(LIVE_ENTRY_KEYS);
  const both = { implement: true, review: true };
  expect(state.repositories?.live).toEqual([
    { repo: "acme/gadgets", apps: { implement: true, review: false } },
    {
      repo: "acme/widgets",
      apps: both,
      cache: {
        repo: "acme/widgets",
        sandbox: "legion-legion-cache-acme-widgets-d782c874",
        state: "ready",
        ready: true,
        address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418",
        head: "34a7ac80e9c3389ffbc1bbeece371ccacc043992",
        lastFetchAt: "2026-10-16T19:20:30Z",
        bytes: 6612345678,
      },
    },
    { repo: "sjawhar/legion", servedAs: "cybermonklabs/legion", apps: both },
    {
      repo: "zeta/archive",
      readError:
        "the implement App's installation on zeta/archive could not be read: GitHub answered 502; Legion reads it again",
    },
    {
      repo: "zeta/tools",
      apps: both,
      cache: {
        repo: "zeta/tools",
        sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e",
        state: "failing",
        ready: false,
        reason:
          "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)",
      },
    },
  ]);
  expect(state.repositories?.live?.[3]?.apps).toBeUndefined();
  expect(state.repositories?.caches).toEqual([
    {
      repo: "acme/legacy",
      sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d",
      state: "starting",
      ready: false,
      retainedUntil: "2026-10-23T09:00:00Z",
    },
  ]);

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.repositories;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();
  const noLive = fixture("state.json") as { repositories: Record<string, unknown> };
  delete noLive.repositories.live;
  expect(LegionStateResponse.safeParse(noLive).success).toBeTrue();
  const noCaches = fixture("state.json") as { repositories: Record<string, unknown> };
  delete noCaches.repositories.caches;
  expect(LegionStateResponse.safeParse(noCaches).success).toBeTrue();
  const noWebhookGaps = fixture("state.json") as { repositories: Record<string, unknown> };
  delete noWebhookGaps.repositories.withoutWebhookApp;
  expect(LegionStateResponse.safeParse(noWebhookGaps).success).toBeTrue();
  const refused = fixture("state.json") as {
    repositories: { caches: Array<Record<string, unknown>> };
  };
  Object.assign(refused.repositories.caches[0] ?? {}, {
    state: "refused",
    reason: "its Service legion-legion-cache-acme-legacy-0a1b2c3d has no controller",
  });
  expect(LegionStateResponse.parse(refused).repositories?.caches?.[0]?.state).toBe("refused");
  Object.assign(refused.repositories.caches[0] ?? {}, { state: "gone" });
  expect(LegionStateResponse.safeParse(refused).success).toBeFalse();
  const emptyError = fixture("state.json") as {
    repositories: { live: Array<Record<string, unknown>> };
  };
  Object.assign(emptyError.repositories.live[3] ?? {}, { readError: "" });
  expect(LegionStateResponse.safeParse(emptyError).success).toBeFalse();
});

// The state's capability report (contract 16): the golden carries a decided row with the operator's
// reason and an open row with the legion.yaml line that records a decision, beside the present
// (codegraph among them: the image carries the tooling and a pod's launch loads it with extension
// discovery on; claude-code too, since one worker image carries the claude-envoy plugin and the
// pin a pod installs Claude Code from for a role whose worker_backends names it) and withheld
// rows, and the six live rows unchecked — no session has reported, so
// the deployment renders each as `unchecked` with its citation (contract 21) — and the report is
// never absent from a state. `installed` left the status set at contract 25: it was codegraph's
// until LEGION-629, and no row has carried it since. Each deployment row carries `source`
// (contract 25): `configuration` (what legion.yaml alone decides) or `boot` (what boot measured),
// or, on `repository-cache-room`, `caches` (contract 213), what the repository caches' last health
// reads said of their room: the golden's one cache ran out of room, so that row is open too. A
// live row carries none.
test("the state golden carries the deployment's capability report", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));

  const rows = Object.fromEntries((state.capabilities ?? []).map((row) => [row.name, row]));
  expect(rows.secrets).toEqual({
    name: "secrets",
    status: "decided",
    detail: "runtime.kubernetes.agent_secrets is not configured",
    decision:
      "pods are enrolled with the secrets broker once dispatch://LEGION-205 lands; until then no pod reads a secret",
    source: "configuration",
  });
  expect(rows["resource-limits"]).toEqual({
    name: "resource-limits",
    status: "open",
    detail:
      "no CPU and memory requests and limits under runtime.kubernetes.resources for the issue pod",
    configLine: 'capabilities.decided.resource-limits: "<reason>"',
    source: "configuration",
  });
  expect(rows.codegraph).toEqual({
    name: "codegraph",
    status: "present",
    detail: "checked by the daemon's probe of the worker image, which passed",
  });
  // A capability `name` is a plain non-empty string, so a row the daemon adds — claude-code at
  // dispatch://INFRA-220 — parses against the schema already published; the golden carries it.
  expect(rows["claude-code"]).toEqual({
    name: "claude-code",
    status: "present",
    detail: "checked by the daemon's probe of the worker image, which passed",
  });
  expect(rows.subagents).toEqual({
    name: "subagents",
    status: "unchecked",
    detail:
      "no session has reported yet (dispatch://LEGION-663): dispatches task subagents, each on the model its role configures",
  });
  const statuses = (state.capabilities ?? []).map((row) => row.status);
  for (const status of ["present", "unchecked", "withheld", "decided", "open"] as const) {
    expect(statuses).toContain(status);
  }
  expect(statuses).not.toContain("installed");

  expect(rows["repository-cache-room"]).toMatchObject({
    status: "open",
    configLine: 'capabilities.decided.repository-cache-room: "<reason>"',
    source: "caches",
  });
  expect(rows["repository-cache-room"].detail).toContain("acme/widgets on a 20.0 GiB volume");

  const deploymentRows = [
    rows.secrets,
    rows["model-fallback"],
    rows["resource-limits"],
    rows["pool-capacity"],
    rows["repository-cache-room"],
  ];
  for (const row of deploymentRows) {
    expect(["configuration", "boot", "caches"]).toContain(row.source ?? "");
  }

  const emptied = fixture("state-stage3.json") as { capabilities: unknown[] };
  expect(emptied.capabilities).toEqual([]);
});

// `source` is a finite set, so the schema names its three words and no other: a fourth word is
// refused with the whole state, as `caches` was before it joined the enum at contract 213.
test("a capability row whose source is a word the schema does not name is refused", () => {
  const unknownSource = fixture("state.json") as { capabilities: { source?: string }[] };
  unknownSource.capabilities = [{ ...unknownSource.capabilities[0], source: "volume" }];
  expect(LegionStateResponse.safeParse(unknownSource).success).toBeFalse();
});

test("a capability row's status is one the report renders, and the report may be left out", () => {
  const unknownStatus = fixture("state.json") as { capabilities: { status: string }[] };
  unknownStatus.capabilities = [{ ...unknownStatus.capabilities[0], status: "missing" }];
  expect(LegionStateResponse.safeParse(unknownStatus).success).toBeFalse();

  // Contract 16's `live` promised a check to come; since contract 21 a live row is `present`,
  // `open` or `unchecked` from the sessions' reports, and a daemon still rendering `live` is one
  // the plugin was not built against.
  const live = fixture("state.json") as { capabilities: { status: string }[] };
  live.capabilities = [{ ...live.capabilities[0], status: "live" }];
  expect(LegionStateResponse.safeParse(live).success).toBeFalse();

  // No plugin code reads the report, so a body without it is read rather than refused.
  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.capabilities;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();
});

// A session's own report (contract 21): `claims/ready` carries what the session measured of the
// live rows, and the state shows each claim's latest report partitioned into the names that passed
// and the gaps that did not, with the incarnation of the process that reported.
test("the register golden names the task agents the role's prompts dispatch", () => {
  const registration = LegionRegisterResponse.parse(fixture("register.json"));

  expect(registration.promptAgents).toEqual([
    "deep-worker",
    "oracle",
    "plan-gap-analyst",
    "plan-reviewer",
    "thermonuclear-code-quality",
    "thermonuclear-deep-review",
  ]);

  const mutated = fixture("register.json") as Record<string, unknown>;
  delete mutated.promptAgents;
  expect(LegionRegisterResponse.safeParse(mutated).success).toBeFalse();
});

test("the ready golden carries the session's capability report", () => {
  const ready = LegionReadyRequest.parse(fixture("ready.json"));

  expect(ready.capabilities?.rows).toHaveLength(3);
  expect(ready.capabilities?.rows.map((row) => [row.name, row.ok])).toEqual([
    ["subagents", true],
    ["dispatch-envoy-tools", true],
    ["github", false],
  ]);

  // A passing check with nothing to add sends an empty detail; a report is optional on the wire
  // (the controller's ready carries none).
  const bare = fixture("ready.json") as { capabilities: { rows: Record<string, unknown>[] } };
  bare.capabilities.rows = [{ name: "github", ok: true, detail: "" }];
  expect(LegionReadyRequest.safeParse(bare).success).toBeTrue();
  const withoutReport = fixture("ready.json") as Record<string, unknown>;
  delete withoutReport.capabilities;
  expect(LegionReadyRequest.safeParse(withoutReport).success).toBeTrue();
});

// The idle rule reads these three values of a session and nothing else, so each is held here: the
// moment of the reading, whether work of the session's own was running and what kinds, and the
// armed wake, which is null when no reminder is armed.
test("a session's activity report carries its reading, its work and its wake", () => {
  const report = {
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer_208",
    secret: CLAIM_SECRET,
    activity: {
      at: "2026-11-01T12:00:00.000Z",
      backgroundWork: { running: true, kinds: ["async-job", "subagent"] },
      phase: { nextReminderAt: "2026-11-01T12:05:00.000Z" },
    },
  };
  expect(LegionActivityRequest.safeParse(report).success).toBeTrue();

  const quiet = {
    ...report,
    activity: {
      at: report.activity.at,
      backgroundWork: { running: false, kinds: [] },
      phase: { nextReminderAt: null },
    },
  };
  expect(LegionActivityRequest.safeParse(quiet).success).toBeTrue();

  // The wake may be absent only as null, never as a missing key or a bare date, and `at` is an
  // instant: the daemon stores both as timestamps.
  const noWakeKey = { ...quiet, activity: { ...quiet.activity, phase: {} } };
  expect(LegionActivityRequest.safeParse(noWakeKey).success).toBeFalse();
  const badWake = {
    ...quiet,
    activity: { ...quiet.activity, phase: { nextReminderAt: "2026-11-01" } },
  };
  expect(LegionActivityRequest.safeParse(badWake).success).toBeFalse();
  const badAt = { ...quiet, activity: { ...quiet.activity, at: "now" } };
  expect(LegionActivityRequest.safeParse(badAt).success).toBeFalse();
  // No generation: the secret is the whole fence, and an added key is refused.
  expect(LegionActivityRequest.safeParse({ ...report, generation: 3 }).success).toBeFalse();
});

// What a pod reads of its deployment's idle release: a release with the roles it suspended and
// the bundle it saved, the wake that ended it with the stamps observed so far, and a pod the rule
// kept with the reason it read. The three lists are always present, so a window with nothing in
// it is readable without a missing-key check.
test("the release records carry a release, a resume and a hold", () => {
  const records = {
    releases: [
      {
        id: 1,
        issue: "LEGION-208",
        generation: 3,
        roles: ["implementer", "tester"],
        at: "2026-11-01T12:00:00Z",
        bundleBytes: 4096,
      },
    ],
    resumes: [
      {
        id: 1,
        release: 1,
        issue: "LEGION-208",
        role: "implementer",
        kind: "delivery",
        at: "2026-11-01T12:30:00Z",
        podReadyAt: "2026-11-01T12:31:00Z",
        stuck: false,
      },
    ],
    holds: [
      {
        id: 1,
        issue: "LEGION-209",
        reason: "background_work",
        detail: "subagent",
        firstAt: "2026-11-01T12:50:00Z",
        at: "2026-11-01T13:00:00Z",
        passes: 11,
      },
    ],
  };
  expect(LegionReleaseRecordsResponse.safeParse(records).success).toBeTrue();

  const empty = { releases: [], resumes: [], holds: [] };
  expect(LegionReleaseRecordsResponse.safeParse(empty).success).toBeTrue();

  // A release that saved nothing records a zero bundle, never a missing one, and a pod nothing
  // has released yet carries no release row for its wake.
  const nothingSaved = {
    ...records,
    releases: [{ ...records.releases[0], roles: [], bundleBytes: 0 }],
    resumes: [{ ...records.resumes[0], release: 0 }],
  };
  expect(LegionReleaseRecordsResponse.safeParse(nothingSaved).success).toBeTrue();

  // A list left out, a negative bundle and an unobserved stamp sent as null rather than left out
  // are each refused. A key no schema names is not: this answer strips one, as every answer-side
  // shape does, and the drift gate above is what sees a Go field with no line here.
  const noHolds = { releases: records.releases, resumes: records.resumes };
  expect(LegionReleaseRecordsResponse.safeParse(noHolds).success).toBeFalse();
  const negative = { ...records, releases: [{ ...records.releases[0], bundleBytes: -1 }] };
  expect(LegionReleaseRecordsResponse.safeParse(negative).success).toBeFalse();
  const nulled = { ...records, resumes: [{ ...records.resumes[0], firstTurnAt: null }] };
  expect(LegionReleaseRecordsResponse.safeParse(nulled).success).toBeFalse();
});

// A wake a project pause held (contract 178): the golden's third resume carries the pause that
// held it and pairs with no release, so a reader of these records counts it apart from the wakes
// of released pods. A wake no pause held carries no `pause` at all, and a `pause` that is no
// pause's id is refused.
test("the release records golden marks the wake a pause held", () => {
  const records = LegionReleaseRecordsResponse.parse(fixture("release-records.json"));
  expect(records.resumes.map((resume) => [resume.id, resume.release, resume.pause])).toEqual([
    [201, 101, undefined],
    [202, 0, undefined],
    [203, 0, 7],
  ]);

  const body = fixture("release-records.json") as { resumes: Record<string, unknown>[] };
  const held = body.resumes[2];
  if (held === undefined) {
    throw new Error("release-records fixture lacks its held resume");
  }
  for (const refused of [0, -7, 1.5, "7", null]) {
    held.pause = refused;
    expect(LegionReleaseRecordsResponse.safeParse(body).success, `${refused}`).toBeFalse();
  }
});

// The request a pod sends to read them: its claim, that claim's secret and the window's start,
// in the body. The secret is a credential, so the route is a POST — a query string reaches every
// proxy and access log between the pod and the daemon — and the window is required, since the
// whole table is not an answer.
test("the release records are read with the claim credential in the body", () => {
  const request = {
    claimToken: "legion-legion-legion-208-implementer",
    secret: CLAIM_SECRET,
    since: "2026-11-01T12:00:00Z",
  };
  expect(LegionReleaseRecordsRequest.safeParse(request).error?.issues ?? []).toEqual([]);

  for (const missing of ["claimToken", "secret", "since"]) {
    const without: Record<string, unknown> = { ...request };
    delete without[missing];
    expect(LegionReleaseRecordsRequest.safeParse(without).success, missing).toBeFalse();
  }
  expect(
    LegionReleaseRecordsRequest.safeParse({ ...request, since: "yesterday" }).success
  ).toBeFalse();
  // The session sends `since`, and the daemon decodes it with Go's `time.Parse(time.RFC3339, …)`,
  // which refuses a time with no seconds. The field is a `LegionInstant`, which refuses every
  // string that parse refuses and a few more besides, so the plugin never sends one the daemon
  // would answer for after a round trip.
  expect(
    LegionReleaseRecordsRequest.safeParse({ ...request, since: "2026-11-01T12:00Z" }).success
  ).toBeFalse();
  expect(
    LegionReleaseRecordsRequest.safeParse({ ...request, since: "2026-11-01T12:00:00.5+02:00" })
      .success
  ).toBeTrue();
  expect(
    LegionReleaseRecordsRequest.safeParse({ ...request, sessionId: "ses" }).success
  ).toBeFalse();
});

// `legionStrandedClaim` exactly as contract 45 holds it (`legion-v19.0.0`, this branch's base on
// main): three keys, strict. Every plugin parses the state with the schema of the contract it
// was built at, and a Kubernetes deployment relaunches no stale session (INFRA-945 spec R10),
// so this reader is live in every pod still on the previous image.
const strandedClaimAt45 = z.strictObject({
  issue: z.string().regex(DISPATCH_KEY_PATTERN),
  role: z.enum(LEGION_ROLES),
  state: z.enum(["suspended", "failed"]),
});

test("a reader at the previous contract parses the state body this daemon serves", () => {
  const state = fixture("state.json") as { strandedClaims: { state: string }[] };
  // The golden carries all three kinds: a failed claim, a released one whose wake never brought
  // its pod back — the entry a `suspendedBy` would have ridden on — and the retired claim
  // contract 56 added. Every entry but the retired one is read by the 45 reader as it stands.
  const older = state.strandedClaims.filter((stranded) => stranded.state !== "retired");
  expect(older.length).toBeGreaterThan(1);
  for (const stranded of older) {
    expect(strandedClaimAt45.safeParse(stranded).error?.issues ?? []).toEqual([]);
  }

  // A retired entry is the one this reader cannot take: its state enum names two values, so the
  // entry — and with it the whole state body, since `strandedClaims` is an array this reader
  // parses whole — is refused over the third. That is why the state route serves a pod below
  // contract 56 the report without its retired entries (`api`'s `withoutUnadmittedStrandedStates`,
  // pinned on the Go side) rather than the body it would refuse: what such a pod is served is the
  // list above, which parses.
  const retired = state.strandedClaims.filter((stranded) => stranded.state === "retired");
  expect(retired.length).toBeGreaterThan(0);
  for (const stranded of retired) {
    const refused = strandedClaimAt45.safeParse(stranded);
    expect(refused.success).toBeFalse();
    expect(refused.error?.issues[0]?.path).toEqual(["state"]);
  }

  // What serving it would do, and why it is not served: the 45 reader does not know the key, and
  // a strict object refuses the entry — and so the whole state body — over it.
  const widened = strandedClaimAt45.safeParse({
    issue: "LEGION-208",
    role: "implementer",
    state: "suspended",
    suspendedBy: "released",
  });
  expect(widened.success).toBeFalse();
  expect(widened.error?.issues[0]?.code).toBe("unrecognized_keys");

  // This contract's own reader strips the key rather than refusing it, as every answer-side
  // shape does, so what stops either end serving it quietly is the drift gate above: the moment
  // the Go side writes `suspendedBy` into a golden, the parse stops equalling the file and the
  // gate names the key by its own path.
  const served = fixture("state.json") as { strandedClaims: Record<string, unknown>[] };
  const stranded = served.strandedClaims[0];
  if (stranded === undefined) {
    throw new Error("state fixture lacks a stranded claim");
  }
  stranded.suspendedBy = "released";
  expect(droppedFromState(served)).toBe("strandedClaims[0].suspendedBy");
});

// The gate-basis contract entry: a root's gated step may stand open on an ancestor's approval (`open_on_basis`) or
// wait, closed, on an ancestor's open approval request (`pending_on_basis`), each with the `basis`
// that names the approval or the request, and every root the daemon records says whether its
// tree runs (`treeRuns`). The state golden carries LEGION-207's step open on LEGION-200's
// approval, LEGION-214's waiting on LEGION-200's request, and `treeRuns` on every root and on no
// child. What a caller below the contract is served — no `treeRuns`, and no `gatedSteps` on an
// issue with a step on a basis — still parses, and a word no step reads, a basis that names no
// version and a `treeRuns` that is no boolean are refused.
test("the state golden carries a step on an ancestor's basis and whether each root's tree runs", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.issues["LEGION-207"]?.gatedSteps).toEqual([
    {
      step: "spec",
      artifactId: "0c7e5a31-9d84-4b26-a0f3-5e81c2d47b90",
      currentVersion: 4,
      approvedVersion: 4,
      status: "open_on_basis",
      basis: {
        issue: "LEGION-200",
        artifactId: "f3b8d1a6-5c27-4e90-8a14-6e9c2d7b0f58",
        version: 5,
        approver: "user:alice",
        approvedAt: "2026-09-20T15:02:41Z",
      },
    },
  ]);
  expect(state.issues["LEGION-214"]?.gatedSteps).toEqual([
    {
      step: "spec",
      artifactId: "a4c9e2f7-1b3d-4e68-9f05-7d2b8c6e1a39",
      currentVersion: 2,
      approvedVersion: null,
      status: "pending_on_basis",
      basis: {
        issue: "LEGION-200",
        artifactId: "f3b8d1a6-5c27-4e90-8a14-6e9c2d7b0f58",
        version: 6,
        askId: "c81e5d3a-7f42-4b96-a0d8-3e6f9b1c2a75",
      },
    },
  ]);
  const runs = Object.fromEntries(
    Object.entries(state.issues).map(([key, issue]) => [key, issue.treeRuns])
  );
  expect(runs).toEqual({
    "LEGION-207": false,
    "LEGION-208": true,
    "LEGION-209": undefined,
    "LEGION-210": true,
    "LEGION-211": false,
    "LEGION-212": true,
    "LEGION-214": false,
    "LEGION-218": true,
    "LEGION-219": true,
  });
  expect("treeRuns" in (state.issues["LEGION-209"] ?? {})).toBeFalse();

  type Served = {
    issues: Record<string, { treeRuns?: unknown; gatedSteps?: Record<string, unknown>[] }>;
  };
  const served = () => fixture("state.json") as Served;
  const issueOf = (body: Served, key: string) => {
    const issue = body.issues[key];
    if (issue === undefined) {
      throw new Error(`state fixture lacks ${key}`);
    }
    return issue;
  };
  const stepOf = (body: Served, key: string) => {
    const step = issueOf(body, key).gatedSteps?.[0];
    if (step === undefined) {
      throw new Error(`state fixture lacks a gated step of ${key}`);
    }
    return step;
  };

  // A daemon before the contract, and a caller below it (`api`'s `withoutGateBasis`).
  const older = served();
  for (const issue of Object.values(older.issues)) {
    delete issue.treeRuns;
    if (issue.gatedSteps?.some((step) => step.basis !== undefined)) {
      delete issue.gatedSteps;
    }
  }
  expect(LegionStateResponse.safeParse(older).error?.issues ?? []).toEqual([]);

  const unknownWord = served();
  stepOf(unknownWord, "LEGION-207").status = "approved_on_basis";
  expect(LegionStateResponse.safeParse(unknownWord).success).toBeFalse();
  const noVersion = served();
  delete (stepOf(noVersion, "LEGION-214").basis as Record<string, unknown>).version;
  expect(LegionStateResponse.safeParse(noVersion).success).toBeFalse();
  const notBoolean = served();
  issueOf(notBoolean, "LEGION-208").treeRuns = "yes";
  expect(LegionStateResponse.safeParse(notBoolean).success).toBeFalse();
});

// `legionGatedStepView`'s status as contracts 55 to 275 hold it: four words, in an enum. Every
// plugin parses the state with the schema of the contract it was built at, so this reader is live
// in every pod still on an image before the gate-basis entry. It reads every step of the golden that stands on no
// basis, and refuses the two words of that entry — and with them the whole state body, since
// `gatedSteps` is an array it parses whole — which is why the state route serves a caller below
// that entry's contract no `gatedSteps` on an issue with a step on a basis (`api`'s `withoutGateBasis` and
// `withoutUnadmittedStepStatuses`, pinned on the Go side): neither `waiting` nor `approved` is true
// of such a step, so it serves no entry of that issue rather than a wrong one.
const gatedStepStatusBeforeBasis = z.object({
  status: z.enum(["unregistered", "waiting", "approved", "changes_requested"]),
});

test("a reader before the basis words reads every gated step but those on a basis", () => {
  const state = fixture("state.json") as {
    issues: Record<string, { gatedSteps?: { status: string }[] }>;
  };
  const steps = Object.values(state.issues).flatMap((issue) => issue.gatedSteps ?? []);
  const words = new Set(["open_on_basis", "pending_on_basis"]);
  const older = steps.filter((step) => !words.has(step.status));
  const onBasis = steps.filter((step) => words.has(step.status));
  expect(older.length).toBeGreaterThan(1);
  expect(onBasis.length).toBe(2);
  for (const step of older) {
    expect(gatedStepStatusBeforeBasis.safeParse(step).error?.issues ?? []).toEqual([]);
  }
  for (const step of onBasis) {
    const refused = gatedStepStatusBeforeBasis.safeParse(step);
    expect(refused.success).toBeFalse();
    expect(refused.error?.issues[0]?.path).toEqual(["status"]);
  }
});

// The architect's registration on an ancestor's basis (the gate-basis entry): the three fields come
// together, the document is a UUID, the version positive and the reason a sentence; a request
// that names none of them is the request it was.
test("a gate registration names a basis by its document, its version and a reason", () => {
  const request = {
    grantId: "grant-208",
    issue: "LEGION-208",
    artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
    version: 7,
    step: "spec",
    basisArtifactId: "f3b8d1a6-5c27-4e90-8a14-6e9c2d7b0f58",
    basisVersion: 5,
    reason: "LEGION-208 delivers r1 of LEGION-200 in this repository.",
  };
  expect(LegionGateRegisterRequest.safeParse(request).error?.issues ?? []).toEqual([]);
  expect(
    LegionGateRegisterRequest.safeParse({ ...request, basisArtifactId: "spec" }).success
  ).toBeFalse();
  expect(LegionGateRegisterRequest.safeParse({ ...request, basisVersion: 0 }).success).toBeFalse();
  expect(LegionGateRegisterRequest.safeParse({ ...request, reason: "" }).success).toBeFalse();
  const { grantId, issue, artifactId, version } = request;
  expect(
    LegionGateRegisterRequest.safeParse({ grantId, issue, artifactId, version }).success
  ).toBeTrue();
});

// A plugin from 152 to 189 reads the repositories block with contract 152's schema
// (`repositoriesAt152`), which requires `owners`, `mapped`, `refused` and `withoutWebhookApp`: the
// block this daemon writes refuses there, and with it the whole state. A plugin from 193 on reads
// every key of the block as optional (#3659), and the route serves every caller below 202 the
// block in 152's shape all the same, written from the same entries, and leaves every other key as
// the contract it serves the caller at writes it. `state-narrowed-repositories.json` is the body
// served to a caller that states the contract before 202 (`internal/api`'s
// TestStateNarrowedRepositoriesGolden), and the drift gate above holds every key of it to that
// copy. The entries land where 152 put them: `owners` and `mapped` are the ones both role Apps are
// installed on, under the name each is served under, every `default` false; `refused` is each
// other entry with why; `withoutWebhookApp` is contract 202's own list.
test("a reader at contract 152 parses the repositories block served below 202", () => {
  // The caller states the contract before 202, the one whose entry made the block live
  // (`repositoriesLiveContract` in `internal/api/shaping.go`), and the `contract` mark below names
  // the highest contract the records hold below 202 (`recordedContractAtOrBelow`). The bound is
  // 202 and not this build's own contract: the records hold an entry at 202 itself, so a bound at
  // the daemon's own contract would name 202 as soon as a later contract lands.
  const repositoriesLiveContract = 202;
  const served = fixtureBody("state-narrowed-repositories.json");
  const written = served.repositories;
  expect(isJsonObject(written) ? Object.keys(written).sort() : written).toEqual([
    "caches",
    "mapped",
    "owners",
    "refused",
    "withoutWebhookApp",
  ]);
  const block = repositoriesAt152.safeParse(written);
  expect(block.error?.issues ?? []).toEqual([]);
  // The entries the narrowed block is written from, read from the state golden with the cache
  // view of 152 itself: the current schema takes every key of a cache as optional, which no reader
  // of 152 does, so an entry that lost a key would pass that schema and fail this one.
  const live = z
    .object({
      live: z.array(z.object({ repo: nonEmptyAt152, cache: cacheViewAt152.optional() })),
      caches: z.array(cacheViewAt152),
    })
    .parse(fixtureBody("state.json").repositories);
  const caches = new Map(live.live.map((entry) => [entry.repo, entry.cache]));
  const both = { implement: true, review: true };
  expect(block.data?.owners).toEqual(["acme", "cybermonklabs", "zeta"]);
  expect(block.data?.mapped).toEqual([
    { repo: "acme/widgets", default: false, apps: both, cache: caches.get("acme/widgets") },
    { repo: "cybermonklabs/legion", default: false, apps: both },
    { repo: "zeta/tools", default: false, apps: both, cache: caches.get("zeta/tools") },
  ]);
  expect(block.data?.refused).toEqual([
    {
      repo: "acme/gadgets",
      reason: "the review App is not installed on acme/gadgets",
      apps: { implement: true, review: false },
    },
    {
      repo: "zeta/archive",
      reason:
        "the implement App's installation on zeta/archive could not be read: GitHub answered 502; Legion reads it again",
    },
  ]);
  expect(block.data?.withoutWebhookApp).toEqual([
    {
      repo: "zeta/tools",
      reason:
        "Dispatch's GitHub App is not installed on zeta/tools: GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed",
    },
  ]);
  expect(block.data?.caches).toEqual(live.caches);

  // Every other key is the state golden's own, the `contract` mark, `lowestPluginContract` and
  // `intakeGiveUps` aside: the mark names the contract the caller is served at, 199, the recorded
  // contract below the 201 the caller states (`recordedContractAtOrBelow`), INFRA-4605's, which
  // moved no answer's shape, and the lowest plugin contract reports the claims of the snapshot the
  // route projected the body from, a pod at 201, which `state.json`'s own fixture does not hold.
  // The mark holds no `daemon`: `TestStateNarrowedRepositoriesGolden` takes out the daemon's own
  // number, which an entry moves, and holds it to that number apart.
  // Seven bands stand above 199: the repositories block's at 202; contract 220's, which takes
  // `plan` off each assignment; the intake's give-ups' at 228, so the narrowing leaves
  // `intakeGiveUps` off; the pod hold's at 246, which leaves `podHold` off every issue
  // (`withoutPodHold`); contract 250's, which takes `priority` off each issue and off its
  // `waitingLine`; contract 256's, which takes `lastDeathAt` and `lastStartFailureAt` off each
  // claim view; and contract 275's, which takes `dispatchCredential` off the state. Above them
  // stands the gate-basis entry's band, which takes every root's `treeRuns` and the `gatedSteps`
  // of each issue with a step on an ancestor's basis (`api`'s `withoutGateBasis`). So the golden's
  // issues, rows and claim views are compared without them. One value strip stands above 199 as
  // well: `caches`, the source of the repository-cache-room row, arrived at contract 213, so that
  // row is served here without its `source`, every other field of it and every other row as the
  // state golden holds them. The gate-basis entry's value strips take nothing more here: the strip
  // of its two step statuses takes the `gatedSteps` its band has taken already, and the golden
  // holds no issue in `awaiting_children` and no held row of it.
  const recordedBelow = daemonApiRecords(fixtureDir)
    .records.map((record) => record.contract)
    .filter((contract) => contract < repositoriesLiveContract);
  expect(served.contract).toEqual({ caller: Math.max(...recordedBelow) });
  expect("intakeGiveUps" in served).toBeFalse();
  const {
    repositories: _narrowedBlock,
    contract: _narrowedMark,
    lowestPluginContract: _narrowedLowest,
    ...narrowedRest
  } = served;
  const {
    repositories: _block,
    contract: _mark,
    lowestPluginContract: _lowest,
    dispatchCredential: _credential,
    intakeGiveUps: _giveUps,
    ...rest
  } = fixtureBody("state.json");
  if (!isJsonObject(rest.issues)) {
    throw new Error("state fixture lacks its issues");
  }
  for (const issue of Object.values(rest.issues)) {
    if (!isJsonObject(issue)) {
      throw new Error("state fixture holds an issue that is not an object");
    }
    delete issue.treeRuns;
    const steps = issue.gatedSteps;
    if (Array.isArray(steps) && steps.some((step) => isJsonObject(step) && "basis" in step)) {
      delete issue.gatedSteps;
    }
  }
  const withoutCauseTimes = (view: unknown): unknown => {
    if (!isJsonObject(view)) {
      return view;
    }
    const { lastDeathAt: _deathAt, lastStartFailureAt: _startFailureAt, ...kept } = view;
    return kept;
  };
  const issues = Object.fromEntries(
    Object.entries(rest.issues as Record<string, Record<string, unknown>>).map(([key, issue]) => {
      const { podHold: _podHold, priority: _priority, ...kept } = issue;
      const narrowed: Record<string, unknown> = { ...kept };
      const line = kept.waitingLine;
      if (isJsonObject(line)) {
        const { priority: _placed, ...placed } = line;
        narrowed.waitingLine = placed;
      }
      if (kept.assignments !== undefined) {
        narrowed.assignments = (kept.assignments as Record<string, unknown>[]).map(
          ({ plan: _plan, ...row }) => row
        );
      }
      if (kept.architect !== undefined) {
        narrowed.architect = withoutCauseTimes(kept.architect);
      }
      if (isJsonObject(kept.workers)) {
        narrowed.workers = Object.fromEntries(
          Object.entries(kept.workers).map(([role, worker]) => [
            role,
            isJsonObject(worker) && worker.claim !== undefined
              ? { ...worker, claim: withoutCauseTimes(worker.claim) }
              : worker,
          ])
        );
      }
      return [key, narrowed];
    })
  );
  const capabilities = z.array(z.record(z.string(), z.unknown())).parse(rest.capabilities);
  expect(capabilities.some((row) => row.source === "caches")).toBeTrue();
  const withoutCachesSource = capabilities.map((row) => {
    if (row.source !== "caches") {
      return row;
    }
    const { source: _source, ...kept } = row;
    return kept;
  });
  expect(narrowedRest).toEqual({ ...rest, issues, capabilities: withoutCachesSource });

  // What serving the current block would do: the reader of 152 refuses it for the three keys it
  // requires beside the `withoutWebhookApp` and `caches` the block keeps, so a caller from 152 to
  // 189 would lose its whole state.
  const current = repositoriesAt152.safeParse(fixtureBody("state.json").repositories);
  expect(current.success).toBeFalse();
  expect(current.error?.issues.map((issue) => issue.path[0]).sort()).toEqual([
    "mapped",
    "owners",
    "refused",
  ]);
});

/** The responses an operator's bearer opens, the only ones of the operator's that may name a
 * suspension. */
const operatorSchemas: Record<string, true> = {
  LegionOperatorClaimResponse: true,
  LegionOperatorClaimsResponse: true,
};

/** The one session-facing exception: four of the controller's six agent actions
 * (`POST /legion/v1/agents/stop|retry|suspend|resume`) answer who holds a suspension,
 * because those routes are where the caller reads whether its own resume_agent lifts it. The
 * reason the guard exists does not reach them: they arrived with the agent-actions contract, so only a plugin at
 * that contract or later calls them, and no reader of these bodies predates the key. The schema
 * and the four fixtures it pins are named here, and nothing else; a new session-facing body that
 * names a suspender still fails the walk below. */
const agentActionSchemas: Record<string, true> = { LegionAgentActionResponse: true };
const agentActionFixtures: Record<string, true> = {
  "agent-stop.json": true,
  "agent-retry.json": true,
  "agent-suspend.json": true,
  "agent-resume.json": true,
};

/** Every key `schema` can carry, at any depth, through objects, arrays, unions, records, optional
 * and nullable wrappers, pipes and lazy references. Zod holds each of those on its definition
 * under a member of its own, so the walk reads the definition as the open record it is. */
function schemaKeys(
  schema: unknown,
  found: Set<string> = new Set(),
  seen: Set<unknown> = new Set()
): Set<string> {
  if (!isSchema(schema) || seen.has(schema)) return found;
  seen.add(schema);
  const def: Record<string, unknown> = { ...schema.def };
  const shape = def.shape;
  if (typeof shape === "object" && shape !== null) {
    for (const [name, member] of Object.entries(shape)) {
      found.add(name);
      schemaKeys(member, found, seen);
    }
    return found;
  }
  for (const key of ["element", "innerType", "valueType", "keyType", "in", "out"]) {
    schemaKeys(def[key], found, seen);
  }
  const getter = def.getter;
  if (typeof getter === "function") schemaKeys(getter(), found, seen);
  for (const option of Array.isArray(def.options) ? def.options : [])
    schemaKeys(option, found, seen);
  return found;
}

// The guard the INFRA-945 plan's departure 1 chose in place of serving two shapes of one body: a
// session reads whatever this daemon serves with the schema of its own contract, so a key no
// older contract knows refuses the whole body at a plugin whose answer side predates INFRA-1048,
// and is dropped at every plugin after it, which loses the field rather than the body.
// `suspendedBy` is served to the operator's bearer and, from the agent-actions contract, to the
// controller's four agent actions (agentActionSchemas), and nowhere else; this holds that for every
// schema the module exports, present and future.
test("no session-facing response carries suspendedBy", () => {
  let walked = 0;
  for (const [name, schema] of Object.entries(legionApi)) {
    if (!isSchema(schema) || operatorSchemas[name] === true || agentActionSchemas[name] === true)
      continue;
    walked += 1;
    expect([...schemaKeys(schema)], `${name} carries suspendedBy`).not.toContain("suspendedBy");
  }
  expect(walked).toBeGreaterThan(20);
  // The walk finds the key where it does live, so an empty walk cannot pass for a clean one.
  expect([...schemaKeys(LegionOperatorClaimResponse)]).toContain("suspendedBy");
  expect([...schemaKeys(LegionAgentActionResponse)]).toContain("suspendedBy");

  // And no body the daemon writes carries it outside the operator's own and the four routes'.
  // The fixtures of this directory are bodies, and main's frozen tables, none of which names it.
  // The daemon's records of every answer's key paths and value domains are the frozen tables and
  // then each entry's fixture under `contracts/`, composed as `daemonApiRecords` composes them: they
  // name `suspendedBy` at the four routes' answer, `agent-action`, and must name it at no other.
  const fixtures = readdirSync(fixtureDir);
  for (const name of fixtures.filter((entry) => entry.endsWith(".json"))) {
    if (name.startsWith("operator-") || agentActionFixtures[name] === true) continue;
    expect(readFileSync(path.join(fixtureDir, name), "utf8"), name).not.toContain("suspendedBy");
  }
  const build = daemonApiRecords(fixtureDir);
  const naming = [...build.records, ...build.enumeratedFields].filter((row) =>
    JSON.stringify(row).includes("suspendedBy")
  );
  // The composition finds the key where it does live, so an empty read cannot pass for a clean one.
  expect(naming.length).toBeGreaterThan(0);
  for (const row of naming) {
    expect(row.answer, `a record names suspendedBy at the ${row.answer} answer`).toBe(
      "agent-action"
    );
  }
  // The exception is the four routes' and no wider: each of their fixtures exists.
  for (const name of Object.keys(agentActionFixtures)) {
    expect(fixtures, name).toContain(name);
  }
});

test("the state golden shows a claim's capability report beside its locator", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  const implementer = state.issues["LEGION-208"]?.workers?.implementer?.claim;

  expect(implementer?.capabilities).toEqual({
    measuredAt: "2026-09-22T09:17:03Z",
    incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/5",
    ok: ["subagents", "dispatch-envoy-tools"],
    open: [{ name: "github", detail: "gh api user: HTTP 401: Bad credentials" }],
  });
  expect(implementer?.capabilities?.incarnation).toBe(implementer?.locator?.incarnation);
  // The planner has reported nothing: a claim view without `capabilities` parses.
  expect(state.issues["LEGION-208"]?.workers?.planner?.claim?.capabilities).toBeUndefined();

  const withoutReport = fixture("state.json") as {
    issues: Record<string, { workers: Record<string, { claim: Record<string, unknown> }> }>;
  };
  delete withoutReport.issues["LEGION-208"]?.workers.implementer?.claim.capabilities;
  expect(LegionStateResponse.safeParse(withoutReport).success).toBeTrue();

  // The implementer's report in a parsed copy of the golden, altered one member at a time.
  const parsedReport = () => {
    const document = LegionStateResponse.parse(fixture("state.json"));
    const report = document.issues["LEGION-208"]?.workers?.implementer?.claim?.capabilities;
    if (report === undefined) throw new Error("state.json: the implementer has no report");
    return { document, report };
  };

  // A report the API recorded from a claim whose machine held no process names no incarnation.
  const unlocated = parsedReport();
  unlocated.report.incarnation = "";
  expect(LegionStateResponse.safeParse(unlocated.document).success).toBeTrue();

  // The daemon floors an open row's detail (`capabilities.Normalize`); the schema stays the guard.
  const blank = parsedReport();
  blank.report.open = [{ name: "github", detail: "" }];
  expect(LegionStateResponse.safeParse(blank.document).success).toBeFalse();
});

// The claims nothing will wake (contract 23): the golden's list is written by hand, one entry for
// each claim state the rule can name (internal/api's state_golden_test.go says so): LEGION-210's
// failed implementer, whose issue is held, LEGION-211's suspended one, and LEGION-212's retired
// tester, which only a start of the phase it owns — one no rule the daemon runs queues — would
// relaunch (contract 56) — three keys each, neither the suspended nor the retired one wider than
// the failed one — and a state with none carries an empty list rather than none.
test("the state golden names the claims nothing will wake, and the list may be left out", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.strandedClaims).toEqual([
    { issue: "LEGION-210", role: "implementer", state: "failed" },
    { issue: "LEGION-211", role: "implementer", state: "suspended" },
    { issue: "LEGION-212", role: "tester", state: "retired" },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).strandedClaims).toEqual([]);

  const running = fixture("state.json") as { strandedClaims: unknown[] };
  running.strandedClaims = [{ issue: "LEGION-210", role: "implementer", state: "idle" }];
  expect(LegionStateResponse.safeParse(running).success).toBeFalse();
  // No plugin code reads the list, so a body without it is read rather than refused.
  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.strandedClaims;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();
});

// The lifecycle rows held behind their issues' running ones (contract 28): the golden carries
// LEGION-208's supervise row 802 waiting behind row 801 for two and a half minutes, a state with
// none carries an empty list rather than none, and a row's wait is never negative. No plugin
// code reads the list, so a body without it is read rather than refused.
test("the state golden names the held lifecycle rows, and the list may be left out", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.heldOutboxRows).toEqual([
    { id: 802, kind: "supervise", issue: "LEGION-208", waitedSeconds: 150, waitsOn: 801 },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).heldOutboxRows).toEqual([]);

  const negative = fixture("state.json") as { heldOutboxRows: unknown[] };
  negative.heldOutboxRows = [
    { id: 802, kind: "supervise", issue: "LEGION-208", waitedSeconds: -1, waitsOn: 801 },
  ];
  expect(LegionStateResponse.safeParse(negative).success).toBeFalse();

  const unnamed = fixture("state.json") as { heldOutboxRows: unknown[] };
  unnamed.heldOutboxRows = [
    { id: 802, kind: "supervise", issue: "LEGION-208", waitedSeconds: 150, waitsOn: "801" },
  ];
  expect(LegionStateResponse.safeParse(unnamed).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.heldOutboxRows;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();
});

// The wakes the pacing holds (contract 53): the golden names one of each kind the daemon paces,
// in claim order, and a state outside a wave carries an empty list rather than none. The key is
// not required: a daemon at this contract leaves it off the state it serves a caller whose pod's
// plugin is older than 53, and off no other body it serves (`api.stateFor`,
// `withoutWakesWaiting`). So a body without the key parses, and a reader of it learns nothing
// about which wakes wait rather than losing the whole state; what is refused is a wake with no
// kind at all.
test("the state golden names the wakes the pacing holds, and the key may be absent", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.wakesWaiting).toEqual([
    {
      claim: "legion-legion-legion-207-implementer",
      issue: "LEGION-207",
      role: "implementer",
      kind: "plugin-refresh",
      dueAt: "2026-09-22T09:17:56Z",
    },
    {
      claim: "legion-legion-legion-208-architect",
      issue: "LEGION-208",
      role: "architect",
      kind: "catch-up",
      dueAt: "2026-09-22T09:18:03Z",
    },
    {
      claim: "legion-legion-legion-208-planner",
      issue: "LEGION-208",
      role: "planner",
      kind: "launch",
      dueAt: "2026-09-22T09:18:11Z",
    },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).wakesWaiting).toEqual([]);

  const stripped = fixture("state.json") as Record<string, unknown>;
  delete stripped.wakesWaiting;
  const served = LegionStateResponse.safeParse(stripped);
  expect(served.success).toBeTrue();
  expect(served.data?.wakesWaiting).toBeUndefined();

  const unnamed = fixture("state.json") as { wakesWaiting: unknown[] };
  unnamed.wakesWaiting = [
    {
      claim: "legion-legion-legion-207-implementer",
      issue: "LEGION-207",
      role: "implementer",
      kind: "",
      dueAt: "2026-09-22T09:17:56Z",
    },
  ];
  expect(LegionStateResponse.safeParse(unnamed).success).toBeFalse();
});

// The count of claims the stale-plugin lane has left (contract 73): the golden names everything
// that lane has left to do, the claim whose wake it already holds among them, and a
// state whose lane has nothing left carries 0 rather than no key. The key is not required for
// the same reason `wakesWaiting` is not: a daemon at this contract leaves it off the state it
// serves a caller whose pod's plugin is older than 73, and off no other body it serves
// (`api.stateFor`, `withoutClaimsToRefresh`). So a body without the key parses and a reader of
// it learns nothing about what the lane has left, rather than losing the whole state; what is
// refused is a count that is not a whole number of claims.
test("the state golden counts the claims the stale-plugin lane has left, and the key may be absent", () => {
  const state = fixture("state.json") as Record<string, unknown>;
  expect(LegionStateResponse.parse(state).claimsToRefresh).toBe(4);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).claimsToRefresh).toBe(0);

  const { claimsToRefresh: _withoutTheCount, ...stripped } = state;
  const served = LegionStateResponse.safeParse(stripped);
  expect(served.success).toBeTrue();
  expect(served.data?.claimsToRefresh).toBeUndefined();

  for (const refused of [-1, 1.5, "4", null]) {
    expect(
      LegionStateResponse.safeParse({ ...state, claimsToRefresh: refused }).success
    ).toBeFalse();
  }
});

// The daemon's Dispatch credential under `dispatch_token_broker` (contract 275): the golden names
// the daemon's session and the two counts a deployment reads before it removes
// `dispatch_token_transition_file`, and the time of the last transition launch is null before the
// first. The key is not required: a daemon without the setting writes none, and a daemon at this
// contract leaves it off the state it serves a caller older than 275, whether the caller states
// its contract or is read from its pod (`api.stateFor`, `withoutDispatchCredential`). No plugin
// reads a field of it, so each field is optional; what is refused is a count that is not a whole
// number of launches, or an empty session.
test("the state golden names the daemon's Dispatch credential, and the key may be absent", () => {
  const state = fixture("state.json") as Record<string, unknown>;
  expect(LegionStateResponse.parse(state).dispatchCredential).toEqual({
    session: "legion-daemon:LEGION",
    transitionLaunches: 2,
    lastTransitionLaunchAt: "2026-09-22T09:17:03Z",
    liveLaunchesWithDispatchToken: 3,
  });
  const credential = state.dispatchCredential as Record<string, unknown>;
  expect(
    LegionStateResponse.parse({
      ...state,
      dispatchCredential: { ...credential, transitionLaunches: 0, lastTransitionLaunchAt: null },
    }).dispatchCredential?.lastTransitionLaunchAt
  ).toBeNull();

  const { dispatchCredential: _withoutTheCredential, ...stripped } = state;
  const served = LegionStateResponse.safeParse(stripped);
  expect(served.success).toBeTrue();
  expect(served.data?.dispatchCredential).toBeUndefined();

  for (const refused of [
    { ...credential, liveLaunchesWithDispatchToken: -1 },
    { ...credential, transitionLaunches: 1.5 },
    { ...credential, session: "" },
  ]) {
    expect(
      LegionStateResponse.safeParse({ ...state, dispatchCredential: refused }).success
    ).toBeFalse();
  }
});

// The project's pause (contract 178). state.json is the state of a paused project: it names the
// pause that stands, who paused, the claim whose stop the pause still waits on and the two rows
// it holds, one for a claim and one that names none. state-last-pause.json is the state of a
// project that runs after a pause has ended: it names that pause with when and by whom it was
// ended. A state carries one of the two at most, and neither key is required: the daemon writes
// none before the project's first pause, and the route strips both for a caller in a pod whose
// plugin is older than 178 (`api.stateFor`, `withoutPause`). What is refused is a pause by
// nobody the daemon records, a controller's with no session, a pause with no reason or without
// one of its lists, and a last pause with no end.
test("the state goldens carry the pause that stands and the last pause, and both keys may be absent", () => {
  const paused = LegionStateResponse.parse(fixture("state.json"));
  expect(paused.paused).toEqual({
    id: 7,
    since: "2026-09-22T09:09:00Z",
    by: { kind: "controller", session: "ses_controller" },
    reason: "the model provider is refusing every turn",
    waitsOn: [
      {
        claim: "legion-legion-legion-208-implementer",
        issue: "LEGION-208",
        role: "implementer",
        askedAt: "2026-09-22T09:09:00Z",
      },
    ],
    held: [
      {
        id: 811,
        kind: "supervise",
        issue: "LEGION-211",
        claim: "legion-legion-legion-211-implementer",
        waitedSeconds: 412,
      },
      { id: 814, kind: "notice", issue: "LEGION-209", waitedSeconds: 95 },
    ],
  });
  expect(paused.lastPause).toBeUndefined();

  const running = LegionStateResponse.parse(fixture("state-last-pause.json"));
  expect(running.paused).toBeUndefined();
  expect(running.lastPause).toEqual({
    id: 7,
    since: "2026-11-15T09:00:00Z",
    by: { kind: "operator" },
    reason: "the model provider is refusing every turn",
    endedAt: "2026-11-15T09:41:30Z",
    endedBy: { kind: "controller", session: "ses_controller" },
  });

  // A project never paused, and the body the route serves a caller below the contract.
  const { paused: standing, ...neither } = fixtureBody("state.json");
  const served = LegionStateResponse.safeParse(neither);
  expect(served.error?.issues ?? []).toEqual([]);

  if (!isJsonObject(standing)) {
    throw new Error("state fixture lacks its pause");
  }
  // The operator acts from no session, so the parse carries none even where a body names one.
  const byOperator = { ...standing, by: { kind: "operator", session: "ses_operator" } };
  expect(LegionStateResponse.parse({ ...neither, paused: byOperator }).paused?.by).toEqual({
    kind: "operator",
  });
  const { waitsOn: _waitsOn, ...withoutWaitsOn } = standing;
  const { held: _held, ...withoutHeld } = standing;
  const refusedPauses = [
    { ...standing, by: { kind: "controller" } },
    { ...standing, by: { kind: "controller", session: "" } },
    { ...standing, by: { kind: "architect", session: "ses_architect" } },
    { ...standing, reason: "" },
    withoutWaitsOn,
    withoutHeld,
  ];
  for (const refused of refusedPauses) {
    expect(
      LegionStateResponse.safeParse({ ...neither, paused: refused }).success,
      JSON.stringify(refused)
    ).toBeFalse();
  }

  const { lastPause: last, ...runningWithout } = fixtureBody("state-last-pause.json");
  if (!isJsonObject(last)) {
    throw new Error("state-last-pause fixture lacks its last pause");
  }
  const { endedAt: _endedAt, ...withoutEndedAt } = last;
  const { endedBy: _endedBy, ...withoutEndedBy } = last;
  for (const refused of [withoutEndedAt, withoutEndedBy]) {
    expect(
      LegionStateResponse.safeParse({ ...runningWithout, lastPause: refused }).success,
      JSON.stringify(refused)
    ).toBeFalse();
  }
});

// What the pause routes answer (contract 178). A pause answers the pause it began, which names
// no end. A resume answers the pause it ended, with who ended it, and how many held rows it let
// out. No plugin code reads either answer: the controller hands each to its agent whole, so the
// reader takes every field of it as optional (`LEGION_ANSWER_READS`), and a body that names no
// end is read rather than refused. What is refused is a field of a type the contract does not
// describe, and a controller's entry with no session: the actor view is the state's own, whose
// session the read set names at every path that reaches it. Each refusal names what its code
// names beside its sentence, and nothing else: the setting that turns the pause off, the pause
// that stands, or the last pause. The daemon client reads a refusal through the plain refusal
// shape, which keeps the code and the sentence of every one of them.
test("the pause goldens answer the pause begun and the pause ended, and each refusal names what its code names", () => {
  const pause = {
    id: 7,
    since: "2026-11-15T09:00:00Z",
    by: { kind: "operator" },
    reason: "the model provider is refusing every turn",
  };
  const ended = {
    ...pause,
    endedAt: "2026-11-15T09:41:30Z",
    endedBy: { kind: "controller", session: "ses_controller" },
  };
  expect<unknown>(LegionProjectPauseResponse.parse(fixture("project-pause.json"))).toEqual({
    pause,
  });
  expect<unknown>(LegionProjectResumeResponse.parse(fixture("project-resume.json"))).toEqual({
    pause: ended,
    released: 3,
  });
  expect(
    LegionProjectResumeResponse.safeParse({ ...fixtureBody("project-pause.json"), released: 0 })
      .success
  ).toBeTrue();
  for (const refused of [
    { pause: ended, released: -1 },
    { pause: { ...ended, endedAt: "yesterday" }, released: 3 },
    { pause: { ...ended, endedBy: { kind: "controller" } }, released: 3 },
  ]) {
    expect(
      LegionProjectResumeResponse.safeParse(refused).success,
      JSON.stringify(refused)
    ).toBeFalse();
  }

  const refusals: ReadonlyArray<readonly [string, string, Record<string, unknown>]> = [
    ["project-pause-unavailable.json", "PAUSE_UNAVAILABLE", { setting: "runtime: tmux" }],
    ["project-pause-no-workflow.json", "PAUSE_UNAVAILABLE", {}],
    ["project-pause-refused.json", "PROJECT_ALREADY_PAUSED", { pause }],
    ["project-paused.json", "PROJECT_PAUSED", { pause }],
    ["project-resume-refused.json", "PROJECT_NOT_PAUSED", { lastPause: ended }],
  ];
  for (const [name, code, named] of refusals) {
    const { code: refusedWith, error, ...rest } = LegionPauseFailureResponse.parse(fixture(name));
    expect(refusedWith, name).toBe(code);
    expect<unknown>(rest, name).toEqual(named);
    expect(Object.keys(rest), name).toEqual(Object.keys(named));
    expect(LegionErrorResponse.parse(fixture(name)), name).toEqual({ code, error });
  }
  expect(LegionErrorResponse.parse(fixture("project-pause-reason-required.json")).code).toBe(
    "PAUSE_REASON_REQUIRED"
  );
});

// A pause says why: the daemon refuses a blank reason (`PAUSE_REASON_REQUIRED`), and neither the
// controller's request nor the operator's carries an empty one that far.
test("a pause rejects an empty reason", () => {
  expect(
    LegionProjectPauseRequest.safeParse({ grantId: "controller-grant-1", reason: "" }).success
  ).toBeFalse();
  expect(LegionOperatorProjectPauseRequest.safeParse({ reason: "" }).success).toBeFalse();
});

// The intake's open give-ups (contract 228). The golden counts two consumers' and lists three
// records, one that did not decode, whose event id is null. Every other state golden is of an
// intake with none open and carries two empty lists rather than no key, but for
// state-narrowed-repositories.json, a body served below 228, which carries no key and still
// parses. A count with no newest sequence is refused, since the controller's summary prints it
// (`LEGION_ANSWER_READS`); a listed record with no id is read, since no plugin code reads one; and
// a record whose reason the enumeration does not name is refused.
test("the state golden counts and lists the intake's open give-ups, and a state without them parses", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.intakeGiveUps?.consumers).toEqual([
    { name: "legion-go-LEGION-dispatch", open: 2, newestSeq: 6044896 },
    { name: "legion-go-LEGION-github", open: 1, newestSeq: 812004 },
  ]);
  expect(state.intakeGiveUps?.newest?.map((giveUp) => [giveUp.id, giveUp.eventId])).toEqual([
    [43, null],
    [42, "github-9b2c71e0"],
    [41, "dispatch-6044801"],
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).intakeGiveUps).toEqual({
    consumers: [],
    newest: [],
  });

  expect(
    stateWithRepositoriesAt152.parse(fixture("state-narrowed-repositories.json")).intakeGiveUps
  ).toBeUndefined();

  const countless = fixture("state.json") as {
    intakeGiveUps: { consumers: Record<string, unknown>[] };
  };
  delete countless.intakeGiveUps.consumers[0]?.newestSeq;
  expect(LegionStateResponse.safeParse(countless).success).toBeFalse();
  const idless = fixture("state.json") as { intakeGiveUps: { newest: Record<string, unknown>[] } };
  delete idless.intakeGiveUps.newest[0]?.id;
  expect(LegionStateResponse.safeParse(idless).success).toBeTrue();
  const unnamed = fixture("state.json") as {
    intakeGiveUps: { newest: Record<string, unknown>[] };
  };
  const first = unnamed.intakeGiveUps.newest[0];
  if (first === undefined) {
    throw new Error("state fixture lists no give-up");
  }
  first.reason = "a reason no build names";
  expect(LegionStateResponse.safeParse(unnamed).success).toBeFalse();
});

// A replay or a discard names one record by its id, or a consumer through a stream sequence, and
// never both or neither, as the daemon takes it (`GIVE_UP_REQUEST_INVALID`); a discard says why.
test("a replay or a discard names one form of selection, and a discard names its reason", () => {
  const grantId = "controller-grant-1";
  const reason = "the listener published a non-JSON body";
  const refused = [
    { grantId },
    { grantId, giveUp: 41, consumer: "legion-go-LEGION-dispatch", throughSeq: 6044896 },
    { grantId, consumer: "legion-go-LEGION-dispatch" },
    { grantId, throughSeq: 6044896 },
    { grantId, consumer: "legion-go-LEGION-dispatch", throughSeq: 0 },
    { grantId, giveUp: 0 },
  ];
  for (const selection of refused) {
    expect(
      LegionIntakeReplayRequest.safeParse(selection).success,
      JSON.stringify(selection)
    ).toBeFalse();
    expect(
      LegionIntakeDiscardRequest.safeParse({ ...selection, reason }).success,
      JSON.stringify(selection)
    ).toBeFalse();
  }
  expect(LegionIntakeDiscardRequest.safeParse({ grantId, giveUp: 41 }).success).toBeFalse();
  expect(
    LegionIntakeDiscardRequest.safeParse({ grantId, giveUp: 41, reason: "" }).success
  ).toBeFalse();
  expect(LegionIntakeReplayRequest.safeParse({ grantId, giveUp: 41, reason }).success).toBeFalse();
});

// The daemon's resolved legion.yaml (contract 35): the golden carries the file's
// `review_round_cap` and `worker_boot_timeout_seconds` and its GitHub Apps' ids and
// installations with no key, a Stage 3 state carries the defaults, the two typed keys are
// positive integers and the rest rides loosely. No plugin code reads the block, so a body
// without it is read rather than refused. `max_fix_attempts` rode here until contract 59
// removed the key.
test("the state golden carries the daemon's settings with no credential", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.settings?.review_round_cap).toBe(4);
  expect(state.settings?.max_fix_attempts).toBeUndefined();
  expect(state.settings?.worker_boot_timeout_seconds).toBe(180);
  const apps = state.settings?.github_apps as { implement: Record<string, unknown> };
  expect(Object.keys(apps.implement).sort()).toEqual(["app_id", "installations"]);

  const stage3 = LegionStateResponse.parse(fixture("state-stage3.json"));
  expect(stage3.settings?.review_round_cap).toBe(3);
  expect(stage3.settings?.max_fix_attempts).toBeUndefined();
  expect(stage3.settings?.worker_boot_timeout_seconds).toBe(120);

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.settings;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();

  const zero = fixture("state.json") as { settings: Record<string, unknown> };
  zero.settings.review_round_cap = 0;
  expect(LegionStateResponse.safeParse(zero).success).toBeFalse();

  const typed = fixture("state.json") as { settings: Record<string, unknown> };
  typed.settings.review_round_cap = "5";
  expect(LegionStateResponse.safeParse(typed).success).toBeFalse();

  const text = readFileSync(path.join(fixtureDir, "state.json"), "utf8");
  expect(text).not.toMatch(/BEGIN .*PRIVATE KEY/);
  expect(text).not.toMatch(/postgres:\/\//);
  expect(text).not.toMatch(/:\/\/[^/@\s]+:[^/@\s]+@/);
});

// The lag of the daemon's durable consumers (contract 43): the golden carries a caught-up
// consumer, one whose info NATS did not answer and one stalled with messages waiting; a state
// with no workflow carries an empty list; the oldest and the last handled are null rather than
// absent, and a negative count is refused. No plugin code reads the list, so a body without it
// is read rather than refused.
test("the state golden carries each durable consumer's lag", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  expect(state.consumers).toEqual([
    {
      name: "legion-go-LEGION-dispatch",
      waiting: 0,
      oldestAt: null,
      oldestAgeSeconds: null,
      lastHandledAt: "2026-09-22T09:17:50Z",
      stalled: false,
    },
    {
      name: "legion-go-LEGION-github",
      waiting: 412,
      oldestAt: "2026-09-22T09:05:34Z",
      oldestAgeSeconds: 754,
      lastHandledAt: "2026-09-22T09:16:41Z",
      stalled: true,
    },
    {
      name: "legion-go-LEGION-wake",
      waiting: 0,
      oldestAt: null,
      oldestAgeSeconds: null,
      lastHandledAt: null,
      stalled: false,
      unavailable: "read the consumer's info: context deadline exceeded",
    },
  ]);
  expect(LegionStateResponse.parse(fixture("state-stage3.json")).consumers).toEqual([]);

  const absent = fixture("state.json") as { consumers: Record<string, unknown>[] };
  delete absent.consumers[0]?.oldestAt;
  expect(LegionStateResponse.safeParse(absent).success).toBeTrue();

  const negative = fixture("state.json") as { consumers: Record<string, unknown>[] };
  negative.consumers = [{ ...negative.consumers[0], waiting: -1 }];
  expect(LegionStateResponse.safeParse(negative).success).toBeFalse();

  const dropped = fixture("state.json") as Record<string, unknown>;
  delete dropped.consumers;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();
});

// Each repository a live record names that Dispatch's GitHub App is not known to be installed on
// is listed with why in `withoutWebhookApp` (contracts 43, 202), in `live`'s order and in contract
// 152's shape, which a `legion state` of 152 reads its webhook App lines from, and the list is the
// block's one report of it: the golden lists one, the same list a reader of 152 is served below
// 202; no live entry carries a copy of it (the golden test above holds the entry keys); a state
// whose live records name nothing carries an empty `live` and an empty `withoutWebhookApp`; and an
// empty reason is refused. No plugin code reads the list (`LEGION_ANSWER_READS`), so an entry
// without its reason and a body without the list are both read rather than refused.
test("the state golden names each live repository without the webhook App", () => {
  const state = LegionStateResponse.parse(fixture("state.json"));
  const gaps = [
    {
      repo: "zeta/tools",
      reason:
        "Dispatch's GitHub App is not installed on zeta/tools: GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed",
    },
  ];
  expect(state.repositories?.withoutWebhookApp).toEqual(gaps);
  expect(state.repositories?.live?.map((entry) => entry.repo)).toContain("zeta/tools");
  const narrowed = repositoriesAt152.parse(
    fixtureBody("state-narrowed-repositories.json").repositories
  );
  expect(narrowed.withoutWebhookApp).toEqual(gaps);
  const empty = LegionStateResponse.parse(fixture("state-stage3.json")).repositories;
  expect(empty?.live).toEqual([]);
  expect(empty?.withoutWebhookApp).toEqual([]);

  const reasonless = fixture("state.json") as {
    repositories: { withoutWebhookApp: Record<string, unknown>[] };
  };
  const gap = reasonless.repositories.withoutWebhookApp[0];
  if (gap === undefined) {
    throw new Error("state fixture lacks a repository without the webhook App");
  }
  gap.reason = "";
  expect(LegionStateResponse.safeParse(reasonless).success).toBeFalse();
  delete gap.reason;
  expect(LegionStateResponse.safeParse(reasonless).success).toBeTrue();

  const dropped = fixture("state.json") as { repositories: Record<string, unknown> };
  delete dropped.repositories.withoutWebhookApp;
  expect(LegionStateResponse.safeParse(dropped).success).toBeTrue();
});

// An operator spawns a claim on an issue no workflow records — `legion claims spawn`, which is
// Stage 2's shape and the merge-queue holder's in production — and state lists that issue for the
// claim's sake. The document parsed here is the one a live daemon served with such a claim on it:
// the whole document was refused, so every strict client lost state reading for as long as that
// claim lived, over a row that is a legitimate state rather than corruption.
test("state carrying an operator's claim on an issue no workflow records is parsed", () => {
  const state = LegionStateResponse.parse(fixture("state-operator-claim.json"));

  const spawned = state.issues["AC91849371-900"];
  expect(spawned?.phase).toBe("unrecorded");
  expect(spawned?.status).toBe("unrecorded");
  expect(spawned?.architect?.state).toBe("idle");
});

// The locator is the runtime's nested shape. No plugin code reads a locator's members, so the
// flat shape a daemon might write instead is read and its flat keys are dropped, and a body
// whose backend member is not the one its runtime names keeps nothing of that member. The
// discriminator is the one key the union requires, and a runtime the schema does not describe
// is still refused.
test("a locator keeps the runtime's nested shape and drops what is not in it", () => {
  const flat = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  flat.locator = {
    runtime: "tmux",
    claim: "legion-legion-legion-209-implementer",
    incarnation: "40217:9551230",
    window: "@7",
    pane: "%23",
  };
  expect(LegionOperatorClaimResponse.parse(flat).locator).toEqual({
    runtime: "tmux",
    claim: "legion-legion-legion-209-implementer",
    incarnation: "40217:9551230",
  });

  const sandbox = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  sandbox.locator = { ...sandbox.locator, runtime: "sandbox" };
  expect(LegionOperatorClaimResponse.parse(sandbox).locator).not.toHaveProperty("tmux");

  const unnamed = fixture("operator-claim.json") as { locator: Record<string, unknown> };
  unnamed.locator = { ...unnamed.locator, runtime: "docker" };
  expect(LegionOperatorClaimResponse.safeParse(unnamed).success).toBeFalse();
});

test("a claim state is one the supervisor has", () => {
  const mutated = fixture("operator-claim.json") as Record<string, unknown>;
  mutated.state = "running";

  expect(LegionOperatorClaimResponse.safeParse(mutated).success).toBeFalse();
});

test("a register response without its secret is refused", () => {
  const mutated = fixture("register.json") as Record<string, unknown>;
  delete mutated.secret;

  expect(LegionRegisterResponse.safeParse(mutated).success).toBeFalse();
});

test("a claim's registration and the controller's are told apart by their schemas", () => {
  const claim = fixture("register.json");
  const controller = fixture("register-controller.json");

  expect(LegionControllerRegisterResponse.safeParse(controller).success).toBeTrue();
  expect(LegionRegisterResponse.safeParse(controller).success).toBeFalse();
  expect(LegionControllerRegisterResponse.safeParse(claim).success).toBeFalse();
});
