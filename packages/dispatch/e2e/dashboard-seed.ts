/** Dashboard draws what the server decides about an issue, so its rows need the real stack: which
 *  open asks wait on the person and whose turn each is, which decisions wait for a review, which
 *  approvals a later version made stale and which one a request covers, which threads hold a
 *  write the person has not read, how each check stands at the pull request's head, how many
 *  handoffs each phase recorded, and the issue's Legion state as the sampler's rows give it. This
 *  module writes those records through the routes an agent and a person write through, and the
 *  Legion state as the sampler writes it, and answers what the rows read back. */

import { randomUUID } from "node:crypto";
import type { LegionClaimState, LegionPhase } from "@legion/contracts/legion-api";
import type { Actor } from "../web/src/api/types";
import { checkStanding } from "../web/src/features/issue-page/statement-rules";
import { compareTimestamps } from "../web/src/lib/timestamps";
import {
  answerAsk,
  baseUrl,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  declineAsk,
  editArtifact,
  getIssue,
  getIssueDecisions,
  getIssueResults,
  handAskOn,
  keepDecision,
  listMyReplies,
  mintAgentToken,
  patchIssue,
  putProjectDefaultRepository,
  recordCheckResult,
  requestApproval,
  reviewArtifact,
  uploadIssueFile,
  userHeaders,
  withOpenedAsk,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { awaitBlockAsk, pause } from "./issue-layout-seed-support";
import { LEGION_PROJECT, seedLegionDaemon } from "./legion-seed";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";

/** The person every Dashboard row signs in as: the assignee of each seeded issue, so every open
 *  question of it waits on her unless it is handed on. */
export const READER = "alice";

/** The project of every issue Legion runs: the one `e2e/legion-seed.ts` names a daemon for. */
export const DASHBOARD_PROJECT = LEGION_PROJECT;

/** A project that names no daemon, for the issue Legion does not run. */
export const QUIET_PROJECT = "QUIET";

/** The session the Legion daemon of the Dashboard project writes as (`legionDaemonSessionOf` in
 *  `packages/envoy/internal/dispatch/api/issue_role_holders.go`). */
export const DAEMON_SESSION = `legion-daemon:${DASHBOARD_PROJECT}`;

/** The daemon's state address: a port nothing listens on, so the sampler's own reads fail at
 *  once, write no observation, and leave the rows `seedLegionState` writes standing. */
const NOTHING_ANSWERS = "http://127.0.0.1:9";

/** The agent that plans the issue, asks its questions, writes its decisions and replies. */
const PLANNER: Actor = {
  id: "dashboard-planner",
  kind: "session",
  origin: { session_title: "Planner (dashboard)" },
};
const asPlanner = { actor: PLANNER, as: "agent" as const };

/** The agent that records the results. */
const TESTER: Actor = {
  id: "dashboard-tester",
  kind: "session",
  origin: { session_title: "Tester (dashboard)" },
};
const asTester = { actor: TESTER, as: "agent" as const };

/** The Legion daemon, which begins a thread the threads part leaves out. */
const DAEMON: Actor = { id: DAEMON_SESSION, kind: "session" };

/** The commit the seeded pull request's head is at, so a result recorded there stands. */
const HEAD_SHA = "d".repeat(40);
const PULL_NUMBER = 7;

/** The head of a second pull request of the issue, opened before `PULL_NUMBER`'s: a `before_merge`
 *  check compares with every head the issue holds, so a result recorded here stands at its
 *  check's basis too, and its check reads Passed on its own row. Every result here is recorded
 *  before any at `HEAD_SHA`, so `HEAD_SHA` is the commit the page names. */
const EARLIER_SHA = "e".repeat(40);
const EARLIER_PULL_NUMBER = 8;

// ---------------------------------------------------------------------------------------------
// Which main this is (R27)
// ---------------------------------------------------------------------------------------------

/** What the server this run addresses holds of the two parts that change Dashboard's reads. */
export interface MainHolds {
  /** dispatch://INFRA-1150 is on `main`: a new `decisions.md` is refused `409
   *  DECISIONS_ARE_NODES`, and a decision is a `decision` block of the spec. */
  readonly decisionsAreNodes: boolean;
  /** dispatch://INFRA-1253 is on `main`: `GET /api/v1/issues/{key}/tree` answers, and an issue is
   *  born converted. */
  readonly treeRead: boolean;
}

/** Asks the server which `main` it is: it writes a probe `decisions.md` on a scratch issue of a
 *  project of its own, and reads the scratch issue's tree. Each answer is read from the route
 *  itself, never from a version string. The scratch project is the reset's to remove. */
export async function mainHolds(): Promise<MainHolds> {
  const project = `P${randomUUID().replaceAll("-", "").slice(0, 8).toUpperCase()}`;
  await createProject({ key: project, name: "Which main this is" });
  const probe = await createIssue({ project, title: "A probe of the decisions route" });
  let decisionsAreNodes = false;
  try {
    await createIssueArtifact(probe.key, {
      content: "## A probe decision\n\nNothing rests on it.\n",
      name: "decisions.md",
    });
  } catch (refusal) {
    const said = refusal instanceof Error ? refusal.message : String(refusal);
    if (!(said.includes("failed: 409 ") && said.includes("DECISIONS_ARE_NODES"))) throw refusal;
    decisionsAreNodes = true;
  }
  const tree = await fetch(new URL(`/api/v1/issues/${probe.key}/tree`, baseUrl), {
    headers: await userHeaders(READER),
  });
  // A main without the route answers it as no route at all.
  if (tree.status !== 200 && tree.status !== 404) {
    throw new Error(`GET /api/v1/issues/${probe.key}/tree answered ${tree.status}`);
  }
  return { decisionsAreNodes, treeRead: tree.status === 200 };
}

// ---------------------------------------------------------------------------------------------
// The Legion state, as the sampler writes it
// ---------------------------------------------------------------------------------------------

/** A phase the daemon's list does not hold, for the row that reads "In another phase", and a
 *  claim state it does not hold, for the role that reads "in another state". */
export const UNLISTED = "unknown_state";

/** A phase as the sampler stores it: the daemon's own word (`LEGION_PHASES`); `awaiting_merge`,
 *  which a daemon before its contract 86 reports and Dispatch still names
 *  (`legion.PhaseAwaitingMerge`, `packages/envoy/internal/dispatch/legion/state.go`); or
 *  `UNLISTED`. */
export type SeededPhase = LegionPhase | "awaiting_merge" | typeof UNLISTED;

/** A claim state as the sampler stores it: the daemon's own word (`LEGION_CLAIM_STATES`), or
 *  `UNLISTED`. */
export type SeededClaimState = LegionClaimState | typeof UNLISTED;

export interface SeededRole {
  readonly role: string;
  readonly state: SeededClaimState;
}

/** One issue's Legion state. `phase` null is an issue the daemon records no phase of, so the
 *  issue's read answers `state` null; `configured` false leaves the project's daemon unnamed. */
export interface LegionStateSeed {
  readonly configured: boolean;
  readonly phase: SeededPhase | null;
  readonly roles: readonly SeededRole[];
}

/** Each claim state the daemon names (`LEGION_CLAIM_STATES`), in its order. The seed takes the
 *  contract's type alone: a Node run of Playwright resolves `@legion/contracts/legion-api` to a
 *  file that `build:contracts` does not write, and the `Record` keeps this list whole, since the
 *  type check refuses it where the contract gains or drops a state. */
const DAEMON_CLAIM_STATES: Readonly<Record<LegionClaimState, true>> = {
  queued: true,
  launch_uncertain: true,
  launching: true,
  shim_connected: true,
  registered: true,
  ready: true,
  working: true,
  idle: true,
  suspended: true,
  failed: true,
  retired: true,
};

/** A role holding each claim state the daemon names, and one it does not, in the order the
 *  daemon lists them: the agents part reads each in its role words, and leaves out `retired`. */
export const EVERY_CLAIM_STATE: readonly SeededRole[] = [
  ...(Object.keys(DAEMON_CLAIM_STATES) as LegionClaimState[]).map((state) => ({
    role: `role-${state.replaceAll("_", "-")}`,
    state,
  })),
  { role: "role-unlisted", state: UNLISTED },
];

/**
 * Writes `key`'s Legion state as the sampler writes a read of its project's daemon that answered
 * (`legion.RecordRead`, `packages/envoy/internal/dispatch/legion/store.go`): the project's daemon
 * setting, named with an address nothing answers (`e2e/legion-seed.ts`) and left out where
 * `configured` is false; one `legion_observations` row for each field that moved - `phase` with
 * the JSON string of the phase and `claim.<role>` with the JSON string of each role's claim state
 * (`legion.State.Observations`) - and a `null` row for each field of the issue whose newest row
 * holds a value and that this state does not carry (`recordObservations`), the trigger
 * `legion_observations_current` keeping `legion_current`; a `legion_reads` row that says the read
 * succeeded, in the same statement, so the two commit together as `RecordRead`'s transaction
 * makes them; and the notification an open page's live view of the issue moves on
 * (`legion.NotifyRead`).
 */
export async function seedLegionState(key: string, legion: LegionStateSeed): Promise<void> {
  const project = key.slice(0, key.indexOf("-"));
  if (legion.configured) {
    if (project !== LEGION_PROJECT) {
      throw new Error(`${key} is not of ${LEGION_PROJECT}, the project e2e/legion-seed.ts names`);
    }
    // Each row starts from an emptied database (`e2e/test.ts`'s `freshDatabase`), and seeds one
    // issue's Legion state, so the project names no daemon yet.
    await seedLegionDaemon(NOTHING_ANSWERS);
  }
  const observed = [
    ...(legion.phase === null ? [] : [{ field: "phase", value: legion.phase }]),
    ...legion.roles.map(({ role, state }) => ({ field: `claim.${role}`, value: state })),
  ];
  const fields = observed.map(({ field }) => sqlLiteral(field)).join(", ");
  const values = observed.map(({ value }) => sqlLiteral(JSON.stringify(value))).join(", ");
  const sent = Buffer.byteLength(JSON.stringify(observed));
  await sql(
    `WITH found AS (
       SELECT observed.field, observed.value::jsonb AS value
         FROM unnest(ARRAY[${fields}]::text[], ARRAY[${values}]::text[]) AS observed(field, value)
     ), newest AS (
       SELECT field, value FROM legion_current
        WHERE project = ${sqlLiteral(project)} AND issue_key = ${sqlLiteral(key)}
          AND value <> 'null'::jsonb
     ), written AS (
       INSERT INTO legion_observations (project, issue_key, field, value, observed_at)
       SELECT ${sqlLiteral(project)}, ${sqlLiteral(key)}, coalesce(found.field, newest.field),
              coalesce(found.value, 'null'::jsonb), now()
         FROM found FULL JOIN newest ON newest.field = found.field
        WHERE coalesce(newest.value, 'null'::jsonb) IS DISTINCT FROM coalesce(found.value, 'null'::jsonb)
       RETURNING 1
     )
     INSERT INTO legion_reads (project, read_at, ok, error, bytes)
     VALUES (${sqlLiteral(project)}, now(), true, NULL, ${sent})`,
    `SELECT pg_notify('dispatch_legion_state', ${sqlLiteral(project)})`
  );
}

// ---------------------------------------------------------------------------------------------
// Handoffs
// ---------------------------------------------------------------------------------------------

/** The phases a handoff is recorded under, which the track counts as Planning, Implementing,
 *  Testing and Reviewing. */
export type HandoffPhase = "plan" | "implement" | "test" | "review";

/** The rounds the seed records: Reviewing has none. */
export const ROUNDS: Readonly<Record<HandoffPhase, number>> = {
  implement: 3,
  plan: 2,
  review: 0,
  test: 1,
};

/** Records the next handoff of `phase` on `key` as an agent records one: under `token`, a
 *  personal agent token the reader minted, naming the planner's session, since the route refuses
 *  a cookie caller (`POST /api/v1/issues/{key}/handoffs/{phase}`, 403 HANDOFF_FORBIDDEN). */
export async function writeHandoff(key: string, phase: HandoffPhase, token: string): Promise<void> {
  const answer = await fetch(new URL(`/api/v1/issues/${key}/handoffs/${phase}`, baseUrl), {
    body: JSON.stringify({
      actor: PLANNER,
      body: JSON.stringify({ phase, schemaVersion: 1, summary: `A ${phase} round of ${key}.` }),
    }),
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    method: "POST",
  });
  if (answer.status !== 201) {
    throw new Error(
      `the ${phase} handoff of ${key} answered ${answer.status}: ${await answer.text()}`
    );
  }
}

/** The seed's rounds: Planning 2, Implementing 3, Testing 1 and Reviewing none, in that order,
 *  each under the one agent token the reader mints for them. */
export async function writeRounds(key: string): Promise<void> {
  const token = await mintAgentToken(`dashboard-rounds-${randomUUID().slice(0, 8)}`, READER);
  for (const phase of ["plan", "implement", "test", "review"] as const) {
    for (let round = 0; round < ROUNDS[phase]; round += 1) await writeHandoff(key, phase, token);
  }
}

// ---------------------------------------------------------------------------------------------
// The issues
// ---------------------------------------------------------------------------------------------

/** One issue of the Dashboard project, assigned to the reader, with its Legion state and no
 *  other record: what the phase rows and the agents row read. */
export async function seedPhaseIssue(legion: LegionStateSeed): Promise<string> {
  await createProject({ key: DASHBOARD_PROJECT, name: "Acme" });
  const issue = await createIssue({ project: DASHBOARD_PROJECT, title: "Draw the phase track" });
  await seedLegionState(issue.key, legion);
  return issue.key;
}

/** An issue Legion does not run, where nothing waits on the reader: its project names no daemon,
 *  and nobody wrote on it but the reader, who created it. */
export async function seedQuietIssue(): Promise<string> {
  await createProject({ key: QUIET_PROJECT, name: "Quiet" });
  const issue = await createIssue({
    project: QUIET_PROJECT,
    spec: ":::requirement{#r1}\nNothing on this issue waits on anyone.\n:::\n",
    title: "An issue Legion does not run",
  });
  return issue.key;
}

/** The kinds of what waits on the reader, as Dashboard's items name them. */
export type ItemKind = "approval" | "decision" | "question" | "stale" | "thread";

/** One item of what waits on the reader: its kind, and the id it is drawn under - an ask's id
 *  for a question and an approval request, a document's id for a stale approval, a decision's
 *  block id, a thread's root id. */
export interface WaitingItem {
  readonly id: string;
  readonly kind: ItemKind;
}

/** What one tile counts: the ids it counts, and the whole it counts them of. */
export interface TileCount {
  readonly ids: readonly string[];
  readonly whole: number;
}

export interface DashboardSeed {
  readonly holds: MainHolds;
  readonly key: string;
  /** The document the Changes of the issue's decisions are anchored in: `decisions.md` on a bare
   *  main, the spec where decisions are nodes. */
  readonly decisionsDocumentId: string;
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
  /** Every item that waits on the reader, newest first. Each was written after every write of
   *  the one before it in this order, so it is newest first by any time of its own records. */
  readonly items: readonly WaitingItem[];
  readonly asks: {
    /** The open question answered by an option, and that option's label. */
    readonly byOption: { readonly id: string; readonly option: string };
    /** The open question answered in words. */
    readonly inWords: string;
    /** The open ask whose thread the reader wrote in and the planner wrote in last. */
    readonly replied: string;
    /** The open question whose turn is the planner's: the reader replied last. */
    readonly agentsTurn: string;
    /** The open question handed on to bob. */
    readonly handedOn: string;
    /** The approval request on the spec, and the version it asks the reader to approve. */
    readonly approval: { readonly id: string; readonly version: number };
    /** The question the reader answered, which a person's decision names. */
    readonly answered: string;
    /** The open question asked at urgency `high` (`byOption`), whose item draws that urgency's
     *  word beside its kind. */
    readonly urgent: string;
  };
  readonly decisions: {
    /** The two that wait for the reader's review, oldest first: the Keep goes to `first`, the
     *  Change to `second`. Each by its block id, with its heading and the words a Change
     *  quotes. */
    readonly first: { readonly id: string; readonly heading: string; readonly words: string };
    readonly second: { readonly id: string; readonly heading: string; readonly words: string };
    readonly kept: string;
    readonly person: string;
    readonly replaced: string;
    /** The author line each decision card draws on this main: "Author unknown" on the bare
     *  `decisions` member, the planner's name where the read gives `decided_by`. */
    readonly author: string;
  };
  readonly threads: {
    /** A comment thread the reader began, which the planner answered. */
    readonly comment: string;
    /** A message thread the planner began, to which the reader replied and the planner after. */
    readonly message: string;
    /** A message thread the Legion daemon began, to which the reader replied and the planner
     *  after: unread, and no item. */
    readonly daemon: string;
  };
  /** What each tile counts, as the seed made it. */
  readonly tiles: {
    readonly met: TileCount;
    readonly passed: TileCount;
    readonly children: TileCount;
    readonly questions: TileCount;
  };
  /** The statement whose two checks passed at different commits, each standing at its check's
   *  basis: `older` at the earlier pull request's head, recorded first, and `newer` at
   *  `HEAD_SHA`, the commit the page names. The statement counts in no Met tile and `older` in no
   *  passed tile, while `older` reads Passed on its own row (R6). */
  readonly twoCommits: {
    readonly statement: string;
    readonly older: string;
    readonly newer: string;
  };
  /** The names a record of this issue carries, which Dashboard may draw: logins, sessions and
   *  their titles, keys, and the roles of its Legion state. */
  readonly names: readonly string[];
}

/** The Legion state of the Dashboard seed: the phase and the two claims this issue's own record
 *  read through the `legion` tool answered on 2026-11-25. */
export const SEEDED_LEGION: LegionStateSeed = {
  configured: true,
  phase: "planning",
  roles: [
    { role: "architect", state: "working" },
    { role: "planner", state: "idle" },
  ],
};

const REQUIREMENTS = {
  r1: "Dashboard lists what waits on the person, newest first.",
  r2: "Each item is answered where it is drawn.",
  r3: "The list draws five items at a time.",
  r4: "The count above the list is the number of its items.",
  r5: "The tiles count every check at one commit.",
} as const;
const R4_AGAIN = "The count above the list is the number of its items, and no total.";

/** The spec's requirement `id`, in the full form a Verification document's `serves` writes
 *  (`web/src/features/dashboard/fixtures/serves-forms.txt`). */
function statement(key: string, id: keyof typeof REQUIREMENTS): string {
  return `dispatch://${key}/spec#b-${id}`;
}

function specText(): string {
  return `${Object.entries(REQUIREMENTS)
    .map(([id, words]) => `:::requirement{#${id}}\n${words}\n:::`)
    .join("\n\n")}\n`;
}

/** A check of the Verification document: what it serves, how its one result reads, and the
 *  commit that result is recorded at, `HEAD_SHA` where it names none. */
interface SeededCheck {
  readonly id: string;
  readonly serves: readonly (keyof typeof REQUIREMENTS)[];
  readonly verdict: "fail" | "pass" | null;
  readonly commit?: string;
  readonly words: string;
}

/** Met: r1 (c1 and c5 passed) and r4 (c3 and c5 passed); not met: r2 (c2 failed); unknown: r3
 *  (c4 has no result) and r5 (c6 passed at `EARLIER_SHA`, which is not the commit the page names,
 *  and c7 at `HEAD_SHA`). Passed at that commit: c1, c3, c5 and c7 of seven. */
const CHECKS: readonly SeededCheck[] = [
  { id: "c1", serves: ["r1"], verdict: "pass", words: "The list holds each seeded item once." },
  { id: "c2", serves: ["r2"], verdict: "fail", words: "Each item takes its act in place." },
  { id: "c3", serves: ["r4"], verdict: "pass", words: "The count reads the number of items." },
  { id: "c4", serves: ["r3"], verdict: null, words: "The list draws five and then the next." },
  {
    id: "c5",
    serves: ["r1", "r4"],
    verdict: "pass",
    words: "The newest item stands first and is counted.",
  },
  {
    commit: EARLIER_SHA,
    id: "c6",
    serves: ["r5"],
    verdict: "pass",
    words: "The passed tile counts the checks that passed at the named commit.",
  },
  {
    id: "c7",
    serves: ["r5"],
    verdict: "pass",
    words: "The met tile counts the statements met at the named commit.",
  },
];

/** The commit `check`'s result is recorded at. */
function commitOf(check: SeededCheck): string {
  return check.commit ?? HEAD_SHA;
}

const MET = ["r1", "r4"] as const;
/** The statement of `DashboardSeed.twoCommits` and its two checks. */
const TWO_COMMITS = { newer: "c7", older: "c6", statement: "r5" } as const;
const VERIFICATION_LEAD = "How the Dashboard is proved.";
const VERIFICATION_LEAD_AGAIN = "How the Dashboard is proved, check by check.";
const RUN = "bun run e2e -- e2e/issue-dashboard-today.e2e.ts";

function verificationText(key: string): string {
  const blocks = CHECKS.map(
    (check) =>
      `:::check{#${check.id} serves="${check.serves.map((id) => statement(key, id)).join(" ")}"` +
      ` surface="browser" run="${RUN}" negative_control="Draw no Dashboard and the row fails."}\n` +
      `${check.words}\n:::`
  );
  return `${VERIFICATION_LEAD}\n\n${blocks.join("\n\n")}\n`;
}

/** The statuses of the seven children: three done of seven. */
const CHILD_STATUSES = [
  "done",
  "in_progress",
  "done",
  "todo",
  "needs_review",
  "done",
  "backlog",
] as const;

/** One seeded decision: its heading, and the words of its first paragraph. */
interface SeededDecision {
  readonly heading: string;
  readonly words: string;
}

/** The five seeded decisions: two that wait for review, one kept, one of kind `person`, which
 *  names the question `answered`, and one replaced. */
interface SeededDecisions {
  readonly first: SeededDecision;
  readonly kept: SeededDecision;
  readonly person: SeededDecision;
  readonly replaced: SeededDecision;
  readonly second: SeededDecision;
}

function seededDecisions(key: string, answered: string): SeededDecisions {
  return {
    first: {
      heading: "Draw five items at a time",
      words: `Chosen so the first screen holds the newest five. Shapes ${statement(key, "r1")} and ${statement(key, "r3")}.`,
    },
    kept: {
      heading: "Keep the list to what waits on the person",
      words: `Chosen so nothing that waits is missed. Shapes ${statement(key, "r1")}.`,
    },
    person: {
      heading: "Answer each question where it is drawn",
      words: `Decided in dispatch://${key}/ask/${answered}. Shapes ${statement(key, "r2")}.`,
    },
    replaced: {
      heading: "Draw every item at once",
      words: `Replaced by the five at a time above. Shapes ${statement(key, "r3")}.`,
    },
    second: {
      heading: "Count what waits by its items",
      words: `Chosen so the count and the list agree. Shapes ${statement(key, "r4")}.`,
    },
  };
}

/** The words a decision's first paragraph reads as drawn, which is what a Change quotes: each
 *  reference is drawn by its name, so the quote is the words before the first one. */
function quotedWords(words: string): string {
  return words.slice(0, words.indexOf(" Shapes ")).trim();
}

/** One decision as the seed reads it back from whichever decisions read this main answers. */
interface DecisionRead {
  readonly id: string;
  readonly heading: string;
  readonly kept: boolean;
  readonly textSha256: string;
  readonly waiting: boolean;
}

/** The issue's decisions: the bare member's `decisions`, or the `entries` of a main where
 *  decisions are nodes, read for the fields the seed waits on. */
async function readDecisions(key: string, holds: MainHolds): Promise<readonly DecisionRead[]> {
  const read = await getIssueDecisions(key);
  if (!holds.decisionsAreNodes) {
    return (read.decisions ?? []).map((decision) => ({
      heading: decision.heading,
      id: decision.block_id,
      kept: decision.kept !== null,
      textSha256: decision.text_sha256,
      waiting: decision.waiting_review && (decision.links ?? []).length > 0,
    }));
  }
  return (
    "entries" in read && Array.isArray(read.entries) ? read.entries.map(entryRead) : []
  ).filter((entry) => entry !== undefined);
}

/** One entry of dispatch://INFRA-1150's decisions read, which this main's types do not hold, read
 *  for the fields the seed waits on: undefined for an entry that is no block of a document, such
 *  as an ask's. */
function entryRead(entry: unknown): DecisionRead | undefined {
  if (typeof entry !== "object" || entry === null || !("document" in entry)) return undefined;
  const document = entry.document;
  if (typeof document !== "object" || document === null || !("block_id" in document)) {
    return undefined;
  }
  if (typeof document.block_id !== "string") return undefined;
  return {
    heading: "",
    id: document.block_id,
    kept: "kept" in entry && entry.kept !== null && entry.kept !== undefined,
    textSha256:
      "text_sha256" in entry && typeof entry.text_sha256 === "string" ? entry.text_sha256 : "",
    waiting: "waiting_review" in entry && entry.waiting_review === true,
  };
}

/** Waits until `ready` holds of the issue's decisions, as the document service indexes their
 *  links a moment after each write answers. A row that writes a decision of its own
 *  (`writeDecision`) waits for it here. */
export async function awaitDecisions(
  key: string,
  holds: MainHolds,
  what: string,
  ready: (decisions: readonly DecisionRead[]) => boolean
): Promise<readonly DecisionRead[]> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const decisions = await readDecisions(key, holds);
    if (ready(decisions)) return decisions;
    if (Date.now() > deadline) {
      throw new Error(`${key}'s decisions never read ${what}: ${JSON.stringify(decisions)}`);
    }
    await pause(100);
  }
}

/** Waits until every check reads the state its result seeds, by the page's own reading
 *  (`checkStanding`). */
async function awaitCheckStates(key: string): Promise<void> {
  const wanted = new Map(
    CHECKS.map((check) => [
      check.id,
      check.verdict === "pass" ? "passed" : check.verdict === "fail" ? "failed" : undefined,
    ])
  );
  const deadline = Date.now() + 20_000;
  for (;;) {
    const checks = (await getIssueResults(key)).checks ?? [];
    const seen = new Map(checks.map((check) => [check.block_id, checkStanding(check).state]));
    if (
      [...wanted].every(([id, state]) => state === undefined || seen.get(id) === state) &&
      seen.size === CHECKS.length
    ) {
      return;
    }
    if (Date.now() > deadline) {
      throw new Error(`${key}'s checks read ${JSON.stringify([...seen])}`);
    }
    await pause(100);
  }
}

/** Waits until the issue read gives the document `artifactId` the approval `state` by `by`. */
async function awaitApproval(
  key: string,
  artifactId: string,
  state: string,
  by?: string
): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const approval = ((await getIssue(key)).artifacts ?? []).find(
      (artifact) => artifact.id === artifactId
    )?.approval;
    if (approval?.state === state && (by === undefined || approval.by?.id === by)) return;
    if (Date.now() > deadline) {
      throw new Error(`${key}'s document ${artifactId} reads ${JSON.stringify(approval)}`);
    }
    await pause(100);
  }
}

/**
 * The Dashboard seed of the Verification document's "The seed", on the `main` `holds` names: an
 * issue Legion runs, in the Dashboard project, assigned to the reader, with
 *
 * - its Legion state (`SEEDED_LEGION`, or `legion`) and the rounds of `ROUNDS`;
 * - seven children, three of them done;
 * - a Verification document of seven checks, each serving its statements by their full
 *   `dispatch://<KEY>/spec#b-<id>` address, with a result for six: one at the head of a second
 *   pull request of the issue, recorded first, and five at `HEAD_SHA`, the commit the page names;
 * - questions that wait on the reader (one with options, one without, and one whose thread the
 *   reader wrote in and the planner wrote in last), one whose turn is the planner's and one
 *   handed on to bob, and one the reader answered;
 * - decisions: two that wait for the reader's review, one she kept, one replaced and one of kind
 *   `person`;
 * - an approval request on the spec at a version newer than the one she approved;
 * - the Verification document she approved, then a result recorded, then a change of its text,
 *   which makes her approval stale;
 * - a comment thread and a message thread with a write she has not read, and a message thread
 *   the Legion daemon began with one too.
 *
 * The items are written in the order `items` reverses, each after every write of the one before
 * it, so newest first is one order by any time an item's records carry.
 */
export async function seedDashboard(
  holds: MainHolds,
  legion: LegionStateSeed = SEEDED_LEGION
): Promise<DashboardSeed> {
  await createProject({ key: DASHBOARD_PROJECT, name: "Acme" });
  // The server keeps what it last read of a repository for five minutes across rows, so each
  // seed reads one of its own (`issue-layout-seed-support.ts`, `seedIssueWithDocuments`).
  const repository = `acme/dashboard-${randomUUID().slice(0, 8)}`;
  await putProjectDefaultRepository(DASHBOARD_PROJECT, repository);
  const issue = await createIssue({
    project: DASHBOARD_PROJECT,
    spec: specText(),
    title: "Dashboard on today's issue page",
  });
  const key = issue.key;
  const specArtifactId = issue.primary_artifact_id ?? "";
  await seedFakeGithub({
    [repository]: {
      commit_pulls: { [EARLIER_SHA]: [EARLIER_PULL_NUMBER], [HEAD_SHA]: [PULL_NUMBER] },
      contents: "read",
      installation_id: 512,
      pull_requests: "read",
      pulls: {
        [EARLIER_PULL_NUMBER]: {
          head_sha: EARLIER_SHA,
          state: "open",
          title: `${key} the first tiles`,
        },
        [PULL_NUMBER]: { head_sha: HEAD_SHA, state: "open", title: `${key} the Dashboard` },
      },
    },
  });
  if (holds.treeRead) {
    // dispatch://INFRA-1253: the issue is born converted, and the rows read its converted path.
    const tree = await fetch(new URL(`/api/v1/issues/${key}/tree`, baseUrl), {
      headers: await userHeaders(READER),
    });
    const body: unknown = await tree.json();
    if (typeof body !== "object" || body === null || !("converted" in body)) {
      throw new Error(`${key}'s tree read answers no conversion`);
    }
    if (body.converted !== true) throw new Error(`${key} is not born converted`);
  }

  const children: string[] = [];
  for (const index of CHILD_STATUSES.keys()) {
    const child = await createIssue({
      parent: key,
      project: DASHBOARD_PROJECT,
      title: `Dashboard part ${index + 1}`,
    });
    children.push(child.key);
  }
  await Promise.all(
    children.map((child, index) => patchIssue(child, { status: CHILD_STATUSES[index] }))
  );
  const done = children.filter((_, index) => CHILD_STATUSES[index] === "done");

  await writeRounds(key);
  await seedLegionState(key, legion);

  // The questions that are no item: one the reader answered, which a person's decision names;
  // one whose turn is the planner's, since the reader wrote last; and one handed on to bob.
  const answered = await createAsk(
    key,
    { question: "Where does a person answer a question on this issue?" },
    asPlanner
  );
  const agentsTurn = await createAsk(
    key,
    { question: "Which reads does the first load send?" },
    asPlanner
  );
  await createComment(key, { ask_id: agentsTurn.id, body: "The issue, its asks and handoffs." });
  const handedOn = await createAsk(key, { question: "Who reviews the tile words?" }, asPlanner);
  await handAskOn(handedOn.id, "bob");

  // The questions that wait on the reader, oldest first.
  const byOption = await createAsk(
    key,
    {
      options: [
        { description: "Newest at the top.", label: "Newest first" },
        { description: "Oldest at the top.", label: "Oldest first" },
      ],
      question: "Which order does the list take?",
      urgency: "high",
    },
    asPlanner
  );
  const inWords = await createAsk(
    key,
    { question: "What does the line say when nothing waits?" },
    asPlanner
  );
  const replied = await createAsk(
    key,
    { question: "Does a reply from here mark the thread read?" },
    asPlanner
  );
  await createComment(key, { ask_id: replied.id, body: "Which thread do you mean?" });
  await createComment(
    key,
    { ask_id: replied.id, body: "The comment thread the item draws." },
    asPlanner
  );

  // The decisions, then the reader's answer that makes one a person's.
  const words = seededDecisions(key, answered.id);
  await answerAsk(answered.id, {
    expected_edited_at: null,
    selected: [],
    text: "Where the question is drawn.",
  });
  const written = await writeDecisions(key, specArtifactId, holds, words);
  const keptId = written.ids.kept;
  const kept = written.read.find((decision) => decision.id === keptId);
  await keepDecision(written.documentId, keptId, kept?.textSha256 ?? "");
  await awaitDecisions(key, holds, "the first waiting and one kept", (decisions) =>
    decisions.some((one) => one.id === keptId && one.kept && !one.waiting)
  );
  const second = await writeSecondDecision(key, specArtifactId, written.documentId, holds, words);

  // The approval request on the spec, at a version newer than the one the reader approved.
  await reviewArtifact(specArtifactId, { state: "approved" }, { login: READER });
  await editArtifact(
    specArtifactId,
    { ops: [{ find: REQUIREMENTS.r4, op: "replace", with: R4_AGAIN }] },
    asPlanner
  );
  const requested = withOpenedAsk(await requestApproval(specArtifactId, {}, asPlanner));
  const approval = { id: requested.ask.id, version: requested.version };
  await awaitApproval(key, specArtifactId, "awaiting");

  // The Verification document the reader approved, a result recorded after her approval, and a
  // change of its text, which makes her approval stale.
  const verification = await createIssueArtifact(
    key,
    { content: verificationText(key), name: "verification.md" },
    asPlanner
  );
  const verificationArtifactId = verification.artifact.id;
  await reviewArtifact(verificationArtifactId, { state: "approved" }, { login: READER });
  // The result at the earlier pull request's head first, so the newest result of the issue is at
  // `HEAD_SHA`, the commit the page names.
  const earlierFirst = [
    ...CHECKS.filter((check) => commitOf(check) !== HEAD_SHA),
    ...CHECKS.filter((check) => commitOf(check) === HEAD_SHA),
  ];
  for (const check of earlierFirst) {
    if (check.verdict === null) continue;
    await recordCheckResult(
      key,
      {
        check: check.id,
        commit: commitOf(check),
        failed: check.verdict === "fail" ? 1 : 0,
        negative_control: "Drew no Dashboard and the row failed.",
        observed: check.verdict === "fail" ? "1 failed" : "1 passed",
        role: "tester",
        run: RUN,
        surface: "browser",
        verdict: check.verdict,
      },
      asTester
    );
  }
  await awaitCheckStates(key);
  await editArtifact(
    verificationArtifactId,
    { ops: [{ find: VERIFICATION_LEAD, op: "replace", with: VERIFICATION_LEAD_AGAIN }] },
    asPlanner
  );
  await awaitApproval(key, verificationArtifactId, "stale", READER);

  // The threads, each with a write the reader has not read.
  const comment = await createComment(key, { body: "Does the count drop as soon as I act?" });
  await createComment(
    key,
    { body: "It drops when the write lands, with no reload.", reply_to: comment.id },
    asPlanner
  );
  const message = await createMessage(
    key,
    { body: "The tiles read the results and the coverage." },
    asPlanner
  );
  await createMessage(key, { body: "And the children?", in_reply_to: message.id });
  await createMessage(
    key,
    { body: "The children from the issue read.", in_reply_to: message.id },
    asPlanner
  );
  const daemon = await createMessage(
    key,
    { body: "Planning started." },
    { actor: DAEMON, as: "agent" }
  );
  await createMessage(key, { body: "Thank you.", in_reply_to: daemon.id });
  await createMessage(key, { body: "The plan is on its way.", in_reply_to: daemon.id }, asPlanner);
  await awaitUnread(key, [comment.id, message.id, daemon.id]);

  return {
    asks: {
      agentsTurn: agentsTurn.id,
      answered: answered.id,
      approval,
      byOption: { id: byOption.id, option: "Newest first" },
      handedOn: handedOn.id,
      inWords: inWords.id,
      replied: replied.id,
      urgent: byOption.id,
    },
    decisionsDocumentId: written.documentId,
    decisions: {
      author: holds.decisionsAreNodes ? (PLANNER.origin?.session_title ?? "") : "Author unknown",
      first: {
        heading: words.first.heading,
        id: written.ids.first,
        words: quotedWords(words.first.words),
      },
      kept: keptId,
      person: written.ids.person,
      replaced: written.ids.replaced,
      second: {
        heading: words.second.heading,
        id: second,
        words: quotedWords(words.second.words),
      },
    },
    holds,
    items: [
      { id: message.id, kind: "thread" },
      { id: comment.id, kind: "thread" },
      { id: verificationArtifactId, kind: "stale" },
      { id: approval.id, kind: "approval" },
      { id: second, kind: "decision" },
      { id: written.ids.first, kind: "decision" },
      { id: replied.id, kind: "question" },
      { id: inWords.id, kind: "question" },
      { id: byOption.id, kind: "question" },
    ],
    key,
    names: [
      READER,
      "bob",
      PLANNER.id,
      PLANNER.origin?.session_title ?? "",
      TESTER.id,
      TESTER.origin?.session_title ?? "",
      DAEMON_SESSION,
      key,
      DASHBOARD_PROJECT,
      ...children,
      ...legion.roles.map(({ role }) => role),
    ],
    specArtifactId,
    threads: { comment: comment.id, daemon: daemon.id, message: message.id },
    tiles: {
      children: { ids: done, whole: children.length },
      met: { ids: MET, whole: Object.keys(REQUIREMENTS).length },
      passed: {
        ids: CHECKS.filter((check) => check.verdict === "pass" && commitOf(check) === HEAD_SHA).map(
          (check) => check.id
        ),
        whole: CHECKS.length,
      },
      questions: { ids: [agentsTurn.id, handedOn.id], whole: 6 },
    },
    twoCommits: TWO_COMMITS,
    verificationArtifactId,
  };
}

/** A `decision` block of the spec, as every write of a decision where decisions are nodes formats
 *  it: the statements it shapes, its heading and its words, and `replaced` where a later
 *  decision took its place. */
function decisionBlock(
  id: string,
  shaped: string,
  decision: SeededDecision,
  { replaced = false }: { readonly replaced?: boolean } = {}
): string {
  return `:::decision{#${id} shaped="${shaped}"${replaced ? ' replaced="true"' : ""}}\n### ${decision.heading}\n\n${decision.words}\n:::`;
}

/** A decision as a section of `decisions.md` on a bare main: its heading and its words, whose own
 *  `Shapes <address>` is the tie. */
function decisionSection(decision: SeededDecision): string {
  return `## ${decision.heading}\n\n${decision.words}`;
}

/** The decisions as first written: on a bare main a `decisions.md`, and where decisions are
 *  nodes `decision` blocks of the spec's `## Decisions` part, with the person's decision an
 *  answered ask block that carries a `shaped` tie (dispatch://INFRA-1150). */
async function writeDecisions(
  key: string,
  specArtifactId: string,
  holds: MainHolds,
  words: SeededDecisions
): Promise<{
  readonly documentId: string;
  readonly ids: {
    readonly first: string;
    readonly kept: string;
    readonly person: string;
    readonly replaced: string;
  };
  readonly read: readonly DecisionRead[];
}> {
  if (holds.decisionsAreNodes) {
    const part = [
      "## Decisions",
      decisionBlock("d-kept", statement(key, "r1"), words.kept),
      decisionBlock("d-first", `${statement(key, "r1")} ${statement(key, "r3")}`, words.first),
      decisionBlock("d-replaced", statement(key, "r3"), words.replaced, { replaced: true }),
      `:::ask{#a-person urgency="med" shaped="${statement(key, "r2")}"}\n${words.person.heading}?\n:::`,
    ].join("\n\n");
    await editArtifact(
      specArtifactId,
      { ops: [{ after: "end", markdown: part, op: "insert" }] },
      asPlanner
    );
    const person = await awaitBlockAsk(key, "a-person");
    await answerAsk(person, { expected_edited_at: null, selected: [], text: words.person.words });
    const read = await awaitDecisions(key, holds, "each decision block", (decisions) =>
      ["d-kept", "d-first", "d-replaced"].every((id) => decisions.some((one) => one.id === id))
    );
    return {
      documentId: specArtifactId,
      ids: { first: "d-first", kept: "d-kept", person, replaced: "d-replaced" },
      read,
    };
  }
  const decisions = await createIssueArtifact(
    key,
    {
      content: `${[
        decisionSection(words.kept),
        decisionSection(words.person),
        decisionSection(words.first),
        `## Replaced\n\n### ${words.replaced.heading}\n\n${words.replaced.words}`,
      ].join("\n\n")}\n`,
      name: "decisions.md",
    },
    asPlanner
  );
  const idOf = (decisions: readonly DecisionRead[], heading: string) =>
    decisions.find((one) => one.heading === heading)?.id ?? "";
  const read = await awaitDecisions(key, holds, "the kept and the first waiting", (decisions) =>
    [words.kept.heading, words.first.heading].every((heading) =>
      decisions.some((one) => one.heading === heading && one.waiting)
    )
  );
  return {
    documentId: decisions.artifact.id,
    ids: {
      first: idOf(read, words.first.heading),
      kept: idOf(read, words.kept.heading),
      person: idOf(read, words.person.heading),
      replaced: idOf(read, words.replaced.heading),
    },
    read,
  };
}

/** One decision to write with `writeDecision`. */
interface DecisionWrite extends SeededDecision {
  /** The block id of its `decision` block of the spec, where decisions are nodes. A section of
   *  `decisions.md` on a bare main is named by the read, so this is unused there. */
  readonly id: string;
  /** The statements it shapes, by address, as a block's `shaped` writes them. On a bare main the
   *  tie is the `Shapes <address>` the caller wrote into `words`, and nothing is added to it. */
  readonly shaped: string;
}

/** The documents a decision is written into. */
interface DecisionDocuments {
  /** The issue's `decisions.md` on a bare main, where it has one: the decision is added at its
   *  end. Undefined where there is none, and the write makes the document. Unused where
   *  decisions are nodes. */
  readonly decisions?: string;
  /** The spec, where decisions are nodes: the decision is a `decision` block at its end. */
  readonly spec: string;
}

/**
 * One decision written as `writer` after the issue holds its first ones, and only the write: the
 * caller waits for the read to hold it (`awaitDecisions`). Where decisions are nodes it is a
 * `decision` block at the end of the spec (`decisionBlock`). On a bare main it is a section of
 * `decisions.md` (`decisionSection`): the whole document, made with it, where the issue has none,
 * else a section added at the end of the document, whose `##` heading closes the `## Replaced`
 * section before it. `writeDecisions` writes the first ones in one edit or one create, so that no
 * row reads them half written, and does not come through here.
 */
export async function writeDecision(
  key: string,
  documents: DecisionDocuments,
  holds: MainHolds,
  decision: DecisionWrite,
  writer: { readonly actor: Actor; readonly as: "agent" }
): Promise<void> {
  if (holds.decisionsAreNodes) {
    const block = decisionBlock(decision.id, decision.shaped, decision);
    await editArtifact(
      documents.spec,
      { ops: [{ after: "end", markdown: block, op: "insert" }] },
      writer
    );
    return;
  }
  if (documents.decisions === undefined) {
    await createIssueArtifact(
      key,
      { content: `${decisionSection(decision)}\n`, name: "decisions.md" },
      writer
    );
    return;
  }
  await editArtifact(
    documents.decisions,
    { ops: [{ after: "end", markdown: decisionSection(decision), op: "insert" }] },
    writer
  );
}

/** The second decision that waits for review, written after the first (`writeDecision`): a new
 *  section at the end of `decisions.md`, or a `decision` block at the end of the spec where
 *  decisions are nodes. Answers its block id. */
async function writeSecondDecision(
  key: string,
  specArtifactId: string,
  documentId: string,
  holds: MainHolds,
  words: SeededDecisions
): Promise<string> {
  await writeDecision(
    key,
    { decisions: documentId, spec: specArtifactId },
    holds,
    { ...words.second, id: "d-second", shaped: statement(key, "r4") },
    asPlanner
  );
  if (holds.decisionsAreNodes) {
    await awaitDecisions(key, holds, "the second decision block", (decisions) =>
      decisions.some((one) => one.id === "d-second")
    );
    return "d-second";
  }
  const read = await awaitDecisions(key, holds, "the second waiting", (decisions) =>
    decisions.some((one) => one.heading === words.second.heading && one.waiting)
  );
  return read.find((one) => one.heading === words.second.heading)?.id ?? "";
}

/** Waits until `me/replies` lists each of `threads` as unread for the reader. */
async function awaitUnread(key: string, threads: readonly string[]): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const rows = (await listMyReplies({ issue: key, state: "unread" })).rows ?? [];
    if (threads.every((thread) => rows.some((row) => row.thread_id === thread))) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${key}'s unread threads read ${JSON.stringify(rows.map((row) => row.thread_id))}`
      );
    }
    await pause(100);
  }
}

/** The questions and decisions that the ask body and card rows read on today's pages. */
export const ASK_TODAY = {
  anchor: "Each item is answered where it is drawn.",
  blockId: "rollout-choice",
  blockQuestion: "Where should we start the rollout?",
  changeAnswer: "Start with the second batch.",
  declined: "Should we change the release date?",
  optionQuestion: "Which order does the list take?",
  options: [
    { description: "Newest at the top.", label: "Newest first" },
    { description: "Oldest at the top.", label: "Oldest first" },
  ],
  picture: "Rollout map",
  questionShaped: "What should the next batch contain?",
  reply: "Both orders work for the next batch.",
  unsure: "Unsure: we have not measured how frequently the list changes.",
} as const;

export interface AskTodayDecision {
  readonly case: "first" | "second" | "kept" | "person" | "replaced";
  readonly heading: string;
  readonly section: "Waiting for your review" | "Your decisions" | "Replaced";
  readonly ties: readonly string[];
  readonly text: string;
}

export interface AskTodaySeed extends DashboardSeed {
  readonly blockPath: string;
  readonly notesId: string;
  readonly pictureSlug: string;
  readonly plannerName: string;
  readonly records: Readonly<
    Record<
      "anchored" | "block" | "changed" | "declined" | "orphaned" | "questionShaped" | "thread",
      string
    >
  >;
  readonly decisionCases: readonly AskTodayDecision[];
}

export async function seedAskToday(holds: MainHolds): Promise<AskTodaySeed> {
  const seed = await seedDashboard(holds);
  const { key } = seed;
  const picture = await uploadIssueFile(key, {
    bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
    contentType: "image/png",
    name: "rollout.png",
  });
  const oldWords = "The first batch starts at dawn.";
  const notes = await createIssueArtifact(
    key,
    { content: `${oldWords}\n`, name: "rollout.md" },
    asPlanner
  );
  await editArtifact(
    seed.specArtifactId,
    {
      ops: [
        {
          after: "end",
          markdown: [
            `:::ask{#${ASK_TODAY.blockId} urgency="high" multiple="false" state="open"}`,
            ASK_TODAY.blockQuestion,
            "",
            `![${ASK_TODAY.picture}](dispatch://${key}/artifact/${picture.artifact.slug}@v1)`,
            "",
            ...ASK_TODAY.options.map(({ label, description }) => `- ${label}: ${description}`),
            ":::",
          ].join("\n"),
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  const notesId = notes.artifact.id;
  const block = await awaitBlockAsk(key, ASK_TODAY.blockId);
  const anchored = await createAsk(
    key,
    {
      anchor: { artifact: "spec", quote: ASK_TODAY.anchor },
      options: [...ASK_TODAY.options],
      question: "How should the reader order these items?",
    },
    asPlanner
  );
  const orphaned = await createAsk(
    key,
    { anchor: { artifact: notesId, quote: oldWords }, question: "Is this the start time?" },
    asPlanner
  );
  await editArtifact(
    notesId,
    { ops: [{ find: oldWords, op: "replace", with: "No time is set for the next batch." }] },
    asPlanner
  );

  // This open ask belongs to another reader. A new reply lists it as a thread, not as an ask
  // waiting on the signed-in reader.
  const thread = await createAsk(
    key,
    {
      multiple: true,
      options: [...ASK_TODAY.options],
      question: "Which orders work for the next batch?",
    },
    asPlanner
  );
  await createComment(key, { ask_id: thread.id, body: "Does the next batch use the same order?" });
  await handAskOn(thread.id, "bob");
  await createComment(key, { ask_id: thread.id, body: ASK_TODAY.reply }, asPlanner);
  await awaitUnread(key, [thread.id]);

  const changed = await createAsk(
    key,
    { options: [...ASK_TODAY.options], question: "Which batch should run first?" },
    asPlanner
  );
  const first = await answerAsk(changed.id, {
    expected_edited_at: null,
    selected: [ASK_TODAY.options[0].label],
    text: "Start with the first batch.",
  });
  if (first.answer === null) throw new Error("The first answer is missing from the write response");
  await answerAsk(changed.id, {
    expected_answer_at: first.answer.at,
    expected_edited_at: null,
    selected: [ASK_TODAY.options[1].label],
    text: ASK_TODAY.changeAnswer,
  });
  const declined = await createAsk(key, { question: ASK_TODAY.declined }, asPlanner);
  await declineAsk(declined.id, { note: "The release owner decides this one." });
  const questionShaped = await createAsk(key, { question: ASK_TODAY.questionShaped }, asPlanner);

  // The second decision already shapes r4, which seedDashboard edited after the tie.
  // Change the decision after that edit, but keep its first paragraph for the Change quote.
  const words = seededDecisions(key, seed.asks.answered);
  await editArtifact(
    seed.decisionsDocumentId,
    {
      ops: [
        {
          find: words.second.words,
          op: "replace",
          with: `${words.second.words}\n\n${ASK_TODAY.unsure}`,
        },
      ],
    },
    asPlanner
  );
  if (!holds.decisionsAreNodes) {
    const deadline = Date.now() + 20_000;
    for (;;) {
      const changed = (await getIssueDecisions(key)).decisions?.find(
        (decision) => decision.heading === words.second.heading
      );
      const tie = changed?.links?.find(({ to }) => to.block_id === "r4");
      if (
        changed?.waiting_review &&
        changed.unsure &&
        tie?.stale &&
        tie.to.changed_at != null &&
        compareTimestamps(tie.to.changed_at, tie.created_at) > 0
      ) {
        break;
      }
      if (Date.now() > deadline) {
        throw new Error(
          `The second decision does not show both link changes: ${JSON.stringify(changed)}`
        );
      }
      await pause(100);
    }
  }
  // The inserted ask opens a new spec version. Renew the request for that version.
  const approval = withOpenedAsk(await requestApproval(seed.specArtifactId, {}, asPlanner));
  await awaitApproval(key, seed.specArtifactId, "awaiting");
  return {
    ...seed,
    asks: { ...seed.asks, approval: { id: approval.ask.id, version: approval.version } },
    blockPath: `/issues/${key}/spec`,
    notesId,
    pictureSlug: picture.artifact.slug,
    plannerName: PLANNER.origin?.session_title ?? PLANNER.id,
    records: {
      anchored: anchored.id,
      block,
      changed: changed.id,
      declined: declined.id,
      orphaned: orphaned.id,
      questionShaped: questionShaped.id,
      thread: thread.id,
    },
    decisionCases: [
      {
        case: "first",
        heading: words.first.heading,
        section: "Waiting for your review",
        ties: [REQUIREMENTS.r1, REQUIREMENTS.r3],
        text: seed.decisions.first.words,
      },
      {
        case: "second",
        heading: words.second.heading,
        section: "Waiting for your review",
        ties: [R4_AGAIN],
        text: seed.decisions.second.words,
      },
      {
        case: "kept",
        heading: words.kept.heading,
        section: "Your decisions",
        ties: [REQUIREMENTS.r1],
        text: quotedWords(words.kept.words),
      },
      {
        case: "person",
        heading: words.person.heading,
        section: "Your decisions",
        ties: [REQUIREMENTS.r2],
        text: "Decided in",
      },
      {
        case: "replaced",
        heading: words.replaced.heading,
        section: "Replaced",
        ties: [REQUIREMENTS.r3],
        text: quotedWords(words.replaced.words),
      },
    ] as const,
  };
}
