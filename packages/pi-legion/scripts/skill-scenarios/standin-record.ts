// The issue record the daemon stand-in answers a `read_record` with (`GET /legion/v1/state`, the
// `legion` tool's `read_record`), and the state body around it, built from what the run's world
// says and nothing else. It is a module of its own, with no server and no file read in it but the
// label's own contract and the one daemon source the label's bodies are built by, which the check
// of that contract reads (`contractRefusal`), so three readers build the same record: the
// stand-in at each state read (daemon-standin.ts), a row that holds its shape to production's
// (daemon-standin.test.ts, production-shapes.json's `legion read_record`), and the check a batch
// makes of each label's own contract before any run starts (`contractRefusal`, rig.sh's
// services_up).
//
// The record is production's, field for field, as the daemon's projection builds it
// (packages/daemon/internal/projection/project.go, packages/daemon/internal/api/state.go): the
// tree's architect, a row for each role that holds a claim, the pull request, the design gate, the
// admission slot, the entry of the role working the phase, and the tree's quiet time. In INFRA-764
// round 25 the record held 8 keys and `workers: {}`, so every counted threads-closed run read it
// before any write and saw no implementer row, where rig.sh said the record held the implementer's
// completed round.
//
// Three members production's record holds are written by no claim here, each a fact of the rig and
// not a choice of shape (daemon-standin.test.ts's `WORLD_NEVER_HOLDS` names them to the row that
// holds the shape):
//   - `step`, what the daemon last saw a claim's agent do, is built from the tool frames of the
//     launcher's worker stream (packages/daemon/internal/stream/conn.go's `ToolStep`), and a rig
//     agent is `omp -p` in its own namespace with no launcher and no stream;
//   - `capabilities` is a session's own report on a `claims/ready`, which a session sends from an
//     idle tick of its interval, 30 s after it is registered at the earliest
//     (packages/pi-legion/src/capability-schedule.ts), so a claim carries one only once its
//     session sent it (`ClaimFacts.capabilities`): a `-p` run works from its boot until it exits,
//     and the sessions of the roles before it never ran in the rig;
//   - `lastTurnStart`, `lastTurnEnd` and `lastReady` of a claim no agent of the run booted: the
//     daemon records each when it happens, and the contract's own words for a claim that carries
//     none are "a claim whose last turn ran before the daemon recorded them".
import { createHash } from "node:crypto";
import path from "node:path";
import { type LegionRole, roleForPhase, roleToken } from "@legion/contracts";
import { repositoriesLive, servesFixAttempts, statesContract } from "./daemon-pane";

/** The issue's generation: one run of the workflow, which no world of the rig re-admits. */
export const GENERATION = 1;
/** The namespace of the issue pod a locator names. A deployment's own (`runtime.kubernetes.
 * namespace`, which has no default); the daemon's own fixtures name it so
 * (packages/daemon/internal/runtime/locator_test.go). */
const NAMESPACE = "legion";
/** The one container of an issue pod (packages/daemon/internal/runtime/locator.go's
 * `IssuePodContainer`). */
const CONTAINER = "roles";
/** A DNS label's length, which the daemon cuts a longer Sandbox name to with a hash of its own
 * (packages/daemon/internal/runtime/sandbox/names.go's `dnsName`): not modelled, so a longer name
 * is refused (`sandboxName`). Unreachable: a run's project is its world repository's letters and
 * 12 of a digest (rig.sh's run_project: `fernbrookwidgets` and 12, 28 characters) and its issue
 * the first its own Dispatch files (`WID-1` in each of round 25's runs), so the pod's name,
 * `legion-<project>-<issue>`, is 41 characters, and 63 needs an issue key of 27. */
const MAX_NAME = 63;

/** A session's capability report as the daemon keeps it (`api.CapabilityReportViewOf`): when it
 * measured, the rows its checks proved and the rows they did not, each with what the check found. */
export interface CapabilityFacts {
  readonly measuredAt: string;
  readonly ok: readonly string[];
  readonly open: readonly { readonly name: string; readonly detail: string }[];
}

/** One claim as the world holds it. */
export interface ClaimFacts {
  /** The session the claim's agent registered, "" until one does. */
  readonly session: string;
  /** `supervise.ClaimState`'s word. */
  readonly state: string;
  /** The generation of the claim's process in the issue pod, the nth process the pod started;
   * undefined for a claim no process runs for, which carries no locator. */
  readonly generation?: number | undefined;
  readonly capabilities?: CapabilityFacts | undefined;
  /** Whether the claim has retired a confirmed task of the issue's run, which is when the daemon
   * writes `servingGeneration` (supervise.Claim.ServingGeneration). */
  readonly served: boolean;
  /** The task the claim holds: the phase it was queued for (none for an architect's) and when. */
  readonly pendingTask?: { readonly phase?: string; readonly queuedAt: string } | undefined;
  readonly lastTurnStart?: string | undefined;
  readonly lastTurnEnd?: string | undefined;
  readonly lastReady?: string | undefined;
}

/** One role's row: its claim and the handoff its completion of its current phase named. */
export interface RowFacts {
  readonly claim: ClaimFacts;
  readonly handoffCommit?: string | undefined;
  readonly handoffRecord?: { readonly id: string; readonly version: number } | undefined;
}

/** The world's pull request as gh says it: its number, the head its branch is at, its state and
 * merge state in GitHub's own upper-case words, and the review that last decided the round. */
export interface PullRequestFacts {
  readonly number: number;
  readonly head: string;
  readonly state: string;
  readonly mergeState?: string | undefined;
  readonly decision?: { readonly state: string; readonly head: string } | undefined;
}

/** The issue's spec as its Dispatch holds it: the document, its latest version, the version a
 * person approved (null while none did), and, where its reviewer asked for changes at the latest
 * version, the reason they gave ("" where Dispatch holds none). */
export interface GateFacts {
  readonly artifactId: string;
  readonly currentVersion: number;
  readonly approvedVersion: number | null;
  readonly changesReason?: string | undefined;
}

/** Everything the record is built from. */
export interface RecordFacts {
  readonly project: string;
  readonly issue: string;
  readonly phase: string;
  /** The Dispatch status the record holds: the one the last phase with a status of its own gave
   * it (`phaseStatus`). */
  readonly status: string;
  /** The status writes the record's moves owe the run's Dispatch and have not finished, oldest
   * first: the status, the one Dispatch showed when the move queued it, the sentence the move
   * posts on the issue before the status ("" for a move that posts none), the attempts made, and
   * why the last failed. */
  readonly statusWrites: readonly {
    readonly status: string;
    readonly observed: string;
    readonly reason: string;
    readonly attempts: number;
    readonly lastError?: string | undefined;
  }[];
  readonly repo: string;
  readonly defaultBranch: string;
  /** The contract the label's plugin declares at its registration, which every claim of the
   * issue's pod records: they all load the one plugin. */
  readonly pluginContract: number;
  /** The contract the label's daemon states on every answer and shapes its bodies to, which every
   * caller of a run is served the body of (daemon-pane.ts's `statesContract`, main's since #3659):
   * where it is one, the state is marked with it twice, as the daemon's and as the caller's
   * (`contract`), carries the lowest contract the claims holding a process record
   * (`lowestPluginContract`), and each claim its update state, `none`, since the rig opens no
   * update round (`updateState`). Undefined for a daemon before #3659, whose body carries none of
   * the three as the stand-in serves it. */
  readonly daemonContract?: number | undefined;
  /** Whether the label's daemon serves the repositories its live records name as the state's
   * `repositories` block (daemon-pane.ts's `repositoriesLive`, main's since #4493): where it does,
   * `live` holds the world's one repository, as the records keep it, with both role Apps
   * installed on it, since the daemon serves a repository only once GitHub answered that, and it
   * holds the issue; and where it does not, the block is the project's mapping, that repository
   * mapped as its default under its owner. */
  readonly repositoriesLive: boolean;
  readonly architect?: ClaimFacts | undefined;
  /** Each role that holds a claim, by role. */
  readonly workers: Readonly<Record<string, RowFacts>>;
  /** The implementer's counts (workflow's `recordPass` and `recordRound`): the moves back, the
   * review rounds a request for changes ended, and the moves by pair. */
  readonly pass: number;
  readonly rounds: number;
  readonly backwardMoves: Readonly<Record<string, number>>;
  readonly pullRequest?: PullRequestFacts | undefined;
  /** The count of fix attempts the label's daemon serves on the pull request, the heads pushed
   * onto a red check verdict (main `ff2a10ef`'s `api.PullRequestView.FixAttempts`, which that
   * label's contract requires), or undefined for a daemon that serves none, main's since contract
   * 59 (daemon-pane.ts's `servesFixAttempts`). */
  readonly fixAttempts?: number | undefined;
  /** The spec's gate, or undefined for an issue whose architect registered no document. */
  readonly gate?: GateFacts | undefined;
  /** When the issue took its admission slot. */
  readonly admittedAt: string;
  /** The entry each role stands on and the entry its claim was last given a task of. */
  readonly entries: Readonly<Record<string, { readonly entry: number; readonly given: number }>>;
  /** When the daemon last saw anything of the tree move, and the instant of this read. */
  readonly movedAt: string;
  readonly now: string;
}

/** The Dispatch status an issue standing in `phase` shows (packages/daemon/internal/workflow/
 * table.go's `phaseStatus`), which is what the run's own Dispatch shows: the seeds write
 * `testing` as the implementer's phase ends and `needs_review` as the tester's does
 * (seed-handoffs.ts's PHASE_SEED), and a move of the record writes the status of the phase it
 * enters (daemon-standin.ts's `enter`). The record answered `in_progress` in every phase, so a
 * tester's record and its issue disagreed. "" for a phase with no status of its own (`admitted`,
 * `awaiting_person`, `held`), in which an issue keeps the one it arrived with. */
export function phaseStatus(phase: string): string {
  switch (phase) {
    case "planning":
    case "implementing":
      return "in_progress";
    case "testing":
      return "testing";
    case "reviewing":
    case "accepting":
      return "needs_review";
    case "retro":
    case "merging":
      return "retro";
    case "done":
      return "done";
    default:
      return "";
  }
}

/** Whether an issue standing in `phase` holds one of the cap's slots (packages/daemon/internal/
 * record/project.go's `PastReview`): every phase of the work up to its review, and none past it. */
export function keepsSlot(phase: string): boolean {
  return !["accepting", "retro", "merging", "done"].includes(phase);
}

/** The Sandbox and pod of the issue, the one every role's claim shares (sandbox/names.go's
 * `SandboxName`): a role's claim token without its role. */
function sandboxName(project: string, issue: string): string {
  const name = roleToken(project, issue, "architect").slice(0, -"-architect".length);
  if (name.length > MAX_NAME)
    throw new Error(
      `the issue pod of ${issue} would be named ${name}, past the ${MAX_NAME} characters of a DNS label, which the daemon shortens with a hash this stand-in does not model`
    );
  return name;
}

/** The uid of the issue's pod: a UUID, as Kubernetes gives a pod one, drawn from the project and
 * the issue so that both stand-ins of one run name one pod and no two runs share it (each run has
 * a project of its own, rig.sh's run_project). */
function podUid(project: string, issue: string): string {
  const hex = createHash("sha256").update(`${project}\0${issue}`).digest("hex");
  const variant = ((Number.parseInt(hex.slice(16, 17), 16) & 0x3) | 0x8).toString(16);
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-4${hex.slice(13, 16)}-${variant}${hex.slice(17, 20)}-${hex.slice(20, 32)}`;
}

/** The incarnation of a role's process: `<pod uid>/<generation>` (`runtime.Locator`). */
function incarnation(facts: RecordFacts, generation: number): string {
  return `${podUid(facts.project, facts.issue)}/${generation}`;
}

/** A claim as `api.ClaimView` writes it, in the order production's record holds its keys. */
function claimView(facts: RecordFacts, role: LegionRole, claim: ClaimFacts) {
  const { generation, capabilities, pendingTask } = claim;
  return {
    session: claim.session,
    state: claim.state,
    ...(generation === undefined
      ? {}
      : {
          locator: {
            runtime: "sandbox",
            claim: roleToken(facts.project, facts.issue, role),
            incarnation: incarnation(facts, generation),
            sandbox: {
              namespace: NAMESPACE,
              name: sandboxName(facts.project, facts.issue),
              podUid: podUid(facts.project, facts.issue),
              container: CONTAINER,
              generation,
            },
          },
        }),
    ...(capabilities === undefined
      ? {}
      : {
          capabilities: {
            measuredAt: capabilities.measuredAt,
            // The reporting process's, empty for a report of a claim no process runs for.
            incarnation: generation === undefined ? "" : incarnation(facts, generation),
            ok: capabilities.ok,
            open: capabilities.open,
          },
        }),
    ...(claim.served ? { servingGeneration: GENERATION } : {}),
    pluginContract: facts.pluginContract,
    // Where the round over the claim stands (updatestate.go's `UpdateStateOf`): no rig opens one.
    ...(facts.daemonContract === undefined ? {} : { updateState: "none" }),
    ...(pendingTask === undefined ? {} : { pendingTask }),
    ...(claim.lastTurnStart === undefined ? {} : { lastTurnStart: claim.lastTurnStart }),
    ...(claim.lastTurnEnd === undefined ? {} : { lastTurnEnd: claim.lastTurnEnd }),
    ...(claim.lastReady === undefined ? {} : { lastReady: claim.lastReady }),
  };
}

/** GitHub's word for a pull request's state or merge state in the daemon's spelling, lower-case
 * (`record.MergeState`, `record.PullRequestState`). */
const lower = (word: string) => word.toLowerCase();

/** The merge states the issue may advance on (`record.MergeState.Mergeable`), in the daemon's
 * spelling. */
const MERGEABLE = ["clean", "unstable", "has_hooks"];

/** The record of the issue, as production's `read_record` answers it. */
export function issueRecord(facts: RecordFacts): Record<string, unknown> {
  const { pullRequest: pr, gate } = facts;
  const working = roleForPhase(facts.phase);
  const entry = working === undefined ? undefined : facts.entries[working];
  const mergeState = pr?.mergeState === undefined ? undefined : lower(pr.mergeState);
  const decided = pr?.decision === undefined ? undefined : lower(pr.decision.state);
  // What the review round waits on from GitHub (classify's `RoundWaitsOnMergeState`): the issue
  // is in reviewing, its pull request is open, the approval stands on the head it is at, and the
  // merge state for that head is not one the issue may advance on. `stateReadAt` is this read,
  // since the stand-in reads the world's merge state at each one; `checkRuns` counts the runs a
  // CI settlement brought the record, and no settlement reaches a stand-in, which is the reading
  // the contract gives 0 ("no evidence of what GitHub holds").
  const waits =
    pr !== undefined &&
    facts.phase === "reviewing" &&
    lower(pr.state) === "open" &&
    decided === "approved" &&
    pr.decision?.head === pr.head &&
    (mergeState === undefined || !MERGEABLE.includes(mergeState));
  return {
    key: facts.issue,
    generation: GENERATION,
    phase: facts.phase,
    status: facts.status,
    tree: facts.issue,
    repo: facts.repo,
    defaultBranch: facts.defaultBranch,
    ...(facts.architect === undefined
      ? {}
      : { architect: claimView(facts, "architect", facts.architect) }),
    // Keyed by role in byte order, as Go marshals the map.
    workers: Object.fromEntries(
      Object.keys(facts.workers)
        .sort()
        .map((role) => {
          const row = facts.workers[role] as RowFacts;
          const counted = role === "implementer";
          return [
            role,
            {
              claim: claimView(facts, role as LegionRole, row.claim),
              ...(row.handoffCommit === undefined ? {} : { handoffCommit: row.handoffCommit }),
              ...(row.handoffRecord === undefined ? {} : { handoffRecord: row.handoffRecord }),
              pass: counted ? facts.pass : 0,
              rounds: counted ? facts.rounds : 0,
              // Absent, never `{}`, for an issue that has had no move back.
              ...(counted && Object.keys(facts.backwardMoves).length > 0
                ? { backwardMoves: facts.backwardMoves }
                : {}),
            },
          ];
        })
    ),
    ...(pr === undefined
      ? {}
      : {
          pullRequest: {
            repo: facts.repo,
            number: pr.number,
            head: pr.head,
            state: lower(pr.state),
            ...(mergeState === undefined ? {} : { mergeState }),
            ...(decided === undefined ? {} : { reviewDecision: decided }),
            // Last, where that older daemon's view declares it.
            ...(facts.fixAttempts === undefined ? {} : { fixAttempts: facts.fixAttempts }),
          },
        }),
    ...(gate === undefined
      ? {}
      : {
          designGate: {
            artifactId: gate.artifactId,
            currentVersion: gate.currentVersion,
            approvedVersion: gate.approvedVersion,
          },
        }),
    // One entry for each step the deployment gates (`api.GatedStepViewsOf`): the rig's daemon has
    // no legion.yaml, so it gates the one step a daemon gates by default, the spec.
    gatedSteps: [
      gate === undefined
        ? { step: "spec", approvedVersion: null, status: "unregistered" }
        : {
            step: "spec",
            artifactId: gate.artifactId,
            currentVersion: gate.currentVersion,
            approvedVersion: gate.approvedVersion,
            status:
              gate.approvedVersion === gate.currentVersion
                ? "approved"
                : gate.changesReason === undefined
                  ? "waiting"
                  : "changes_requested",
            ...(gate.approvedVersion !== gate.currentVersion && gate.changesReason
              ? { changesReason: gate.changesReason }
              : {}),
          },
    ],
    ...(keepsSlot(facts.phase) ? { slot: { index: 0, admittedAt: facts.admittedAt } } : {}),
    ...(waits && pr !== undefined
      ? {
          mergeWait: {
            waitsOn: pr.head,
            ...(mergeState === undefined ? {} : { mergeState }),
            stateReadAt: facts.now,
            checkRuns: 0,
          },
        }
      : {}),
    // Each is absent where it is 0: a role no entry was raised for, and one given no task of one.
    ...(entry === undefined || entry.entry === 0 ? {} : { phaseEntry: entry.entry }),
    ...(entry === undefined || entry.given === 0 ? {} : { givenEntry: entry.given }),
    treeMovedAt: facts.movedAt,
    quietHours: Math.max(0, Date.parse(facts.now) - Date.parse(facts.movedAt)) / 3_600_000,
  };
}

/** `GET /legion/v1/state`: the one issue the worker holds, on the run's own world. `capabilities`
 * is the deployment's capability report, `strandedClaims` the claims nothing will wake,
 * `heldOutboxRows` the lifecycle rows held behind their issues' running ones, `consumers` the lag
 * of the daemon's durable consumers and `repositories` the repositories the live records name, or
 * the project's mapping before #4493 (`RecordFacts`' `repositoriesLive`), each of which the
 * reader requires; the rig has no deployment to report on, its one claim runs, it holds no row
 * behind another and has no NATS, so the first four are the empty lists the daemon's MarshalJSON
 * emits when there is none. `pendingStatusWrites` is the status writes the record's moves owe
 * the run's Dispatch and have not finished (`api.PendingStatusWrite`), each with the payload of
 * an ordinary status row (`record.StatusWrite`), none once each has run. `wakesWaiting`, the
 * wakes the pacing holds until their turn, is optional to the reader; the rig carries it empty,
 * as a daemon at this contract serves a caller in no pod holding no wake. `settings` is the
 * daemon's resolved legion.yaml with no credential in it, also required; the rig has no file, so
 * it carries the keys the reader types at the daemon's defaults. `admission` holds the issue in
 * `active` while it holds its slot (`keepsSlot`), and names its tree whatever phase it stands
 * in. */
export function stateBody(facts: RecordFacts, startedAt: string): Record<string, unknown> {
  const [owner = ""] = facts.repo.split("/");
  // `withoutWebhookApp` and `caches` are empty on either block, as this has served them on every
  // label: no world reports a check of Dispatch's webhook App or a repository cache.
  const repositories = facts.repositoriesLive
    ? { live: [{ repo: facts.repo, apps: { implement: true, review: true } }] }
    : { owners: [owner], mapped: [{ repo: facts.repo, default: true }], refused: [] };
  // The lowest contract among the claims whose locator names a pod and that record one
  // (shaping.go's `lowestPluginContract`, `recordsContract`): every claim records the label
  // plugin's, and a claim with no generation has no locator.
  const located = [facts.architect, ...Object.values(facts.workers).map((row) => row.claim)].some(
    (claim) => claim?.generation !== undefined
  );
  return {
    daemon: {
      project: facts.project,
      schemaVersion: 1,
      boots: 1,
      firstBootAt: startedAt,
      startedAt,
    },
    ...(facts.daemonContract === undefined
      ? {}
      : { contract: { daemon: facts.daemonContract, caller: facts.daemonContract } }),
    admission: {
      cap: 1,
      active: keepsSlot(facts.phase) ? [facts.issue] : [],
      waiting: [],
      trees: [facts.issue],
      blocked: [],
    },
    issues: { [facts.issue]: issueRecord(facts) },
    pendingStatusWrites: facts.statusWrites.map((owed) => ({
      issue: facts.issue,
      // `record.StatusWrite` as Go marshals it: `reason` is omitted where it is empty.
      payload: {
        kind: "ordinary",
        status: owed.status,
        observedStatus: owed.observed,
        ...(owed.reason === "" ? {} : { reason: owed.reason }),
      },
      attempts: owed.attempts,
      nextAt: facts.now,
      ...(owed.lastError === undefined ? {} : { lastError: owed.lastError }),
    })),
    heldOutboxRows: [],
    strandedClaims: [],
    wakesWaiting: [],
    capabilities: [],
    repositories: { ...repositories, withoutWebhookApp: [], caches: [] },
    // Every key any label's contract requires, not this checkout's alone: a label's agents load
    // the plugin of their own checkout, and `legionSettings` is a loose object in each, so a body
    // carrying both keys is described by both contracts while one carrying either alone is
    // refused by the other. Measured on 2026-11-10: the base label, then built from main
    // `ff2a10ef`, whose contract requires `settings.max_fix_attempts`, had every state read of
    // all 30 of its runs refused `a body the daemon API contract does not describe`, so its
    // agents spent turns on the daemon and no reviewer of that label could reach the
    // `read_record` the rule asks for — the negative control of k1 and k2 measured in a world of
    // its own. A key a later contract drops stays here for as long as a label the rig builds
    // still requires it.
    settings: { max_fix_attempts: 3, review_round_cap: 3, worker_boot_timeout_seconds: 120 },
    consumers: [],
    ...(facts.daemonContract === undefined || !located || facts.pluginContract === 0
      ? {}
      : { lowestPluginContract: facts.pluginContract }),
  };
}

/** A claim of the kinds a world of the rig holds, for `sampleFacts`. */
function sampleClaim(state: string, generation: number | undefined, more: Partial<ClaimFacts>) {
  return {
    session: "01a1b3f2-6c4e-7a18-9d2b-5e8f0c71a4d3",
    state,
    generation,
    served: true,
    ...more,
  };
}

/** Every kind of record a stand-in of the rig can serve, each named: one for each phase a
 * scenario boots in or the round moves the issue to (rig.sh's worker_boots, daemon-standin.ts's
 * `roundOutcome` and backward move), each holding every member the record ever writes in that
 * phase — the claim of each state a stand-in gives one, a handoff commit and record, a pull
 * request in each merge state a world reports and under each decision, a gate approved, waiting
 * and with changes requested, and an issue with no history at all — and the count of fix attempts
 * `fixAttempts` on each pull request, for a label whose daemon serves one, the contract
 * `daemonContract` the bodies are marked with, for a label whose daemon states one, and the
 * repositories block in the label daemon's shape, `repositoriesLive` (`RecordFacts`).
 * `contractRefusal` hands each to a label's own contract, so a member a label's schema refuses is
 * found before any run of the label starts rather than in its agents' turns. */
export function sampleFacts(
  fixAttempts: number | undefined,
  daemonContract: number | undefined,
  repositoriesLive: boolean
): { name: string; facts: RecordFacts }[] {
  const at = "2026-11-17T12:00:00.000Z";
  const head = "804a7ad10da2154015508a1bbc4a5b2ac9e98b81";
  const record = { id: "a1ae938f-0520-47a1-8778-12b5f3bf9f4f", version: 1 };
  const idle = sampleClaim("idle", 2, {});
  const base = {
    project: "fernbrookwidgets0123456789ab",
    issue: "WID-1",
    repo: "fernbrook/widgets",
    defaultBranch: "main",
    pluginContract: 146,
    fixAttempts,
    daemonContract,
    repositoriesLive,
    admittedAt: at,
    movedAt: at,
    now: "2026-11-17T12:30:00.000Z",
    status: "needs_review",
    statusWrites: [],
    pass: 0,
    rounds: 0,
    backwardMoves: {},
    entries: {
      architect: { entry: 2, given: 2 },
      planner: { entry: 1, given: 1 },
      implementer: { entry: 1, given: 1 },
      tester: { entry: 1, given: 1 },
      reviewer: { entry: 1, given: 0 },
    },
  } as const;
  const history = {
    architect: sampleClaim("idle", 1, { pendingTask: { queuedAt: at } }),
    workers: {
      planner: { claim: idle, handoffRecord: record },
      implementer: { claim: idle, handoffCommit: head, handoffRecord: record },
      tester: {
        claim: sampleClaim("working", 4, {
          served: false,
          pendingTask: { phase: "testing", queuedAt: at },
          lastReady: at,
          lastTurnStart: at,
          lastTurnEnd: at,
          capabilities: { measuredAt: at, ok: ["github"], open: [{ name: "mcp", detail: "off" }] },
        }),
        handoffCommit: head,
        handoffRecord: record,
      },
      reviewer: { claim: sampleClaim("registered", 5, { served: false }) },
      merger: { claim: sampleClaim("queued", undefined, { session: "", served: false }) },
    },
  };
  const gates: (GateFacts | undefined)[] = [
    { artifactId: record.id, currentVersion: 1, approvedVersion: 1 },
    { artifactId: record.id, currentVersion: 2, approvedVersion: 1 },
    { artifactId: record.id, currentVersion: 2, approvedVersion: null, changesReason: "no" },
    undefined,
  ];
  const pulls: (PullRequestFacts | undefined)[] = [
    { number: 1001, head, state: "OPEN" },
    {
      number: 1001,
      head,
      state: "OPEN",
      mergeState: "BLOCKED",
      decision: { state: "APPROVED", head },
    },
    {
      number: 1001,
      head,
      state: "OPEN",
      mergeState: "CLEAN",
      decision: { state: "CHANGES_REQUESTED", head },
    },
    ...["UNSTABLE", "HAS_HOOKS", "DIRTY", "BEHIND", "DRAFT", "UNKNOWN"].map((mergeState) => ({
      number: 1001,
      head,
      state: "OPEN",
      mergeState,
    })),
    { number: 1001, head, state: "MERGED", mergeState: "UNKNOWN" },
    { number: 1001, head, state: "CLOSED" },
    undefined,
  ];
  const samples: { name: string; facts: RecordFacts }[] = [];
  const phases = ["planning", "implementing", "testing", "reviewing", "accepting"];
  for (const [index, phase] of phases.entries())
    for (const [which, pullRequest] of pulls.entries())
      samples.push({
        name: `${phase}, pull request ${which}`,
        facts: {
          ...base,
          ...history,
          phase,
          status: phaseStatus(phase),
          // A move's status write that has not run, once with the sentence its move posts and the
          // reason its last attempt gave.
          statusWrites: [
            { status: phaseStatus(phase), observed: "needs_review", reason: "", attempts: 0 },
            {
              status: "in_progress",
              observed: "testing",
              reason:
                "Legion moved this issue from testing back to implementing at the tester's request: the fix needs code.",
              attempts: 2,
              lastError: "no answer",
            },
          ].slice(0, which % 3),
          pullRequest,
          gate: gates[(index + which) % gates.length],
          ...(phase === "implementing"
            ? { pass: 2, rounds: 1, backwardMoves: { "reviewing-implementing": 1 } }
            : {}),
        },
      });
  for (const state of ["launching", "ready", "idle", "failed"])
    samples.push({
      name: `no history, the claim ${state}`,
      facts: {
        ...base,
        phase: "reviewing",
        entries: { reviewer: { entry: 1, given: 1 } },
        workers: {
          reviewer: {
            claim: sampleClaim(state, state === "failed" ? undefined : 1, {
              session: state === "launching" ? "" : "01a1e961-088f-70f1-ac91-c57b385f1508",
              served: false,
            }),
          },
        },
      },
    });
  return samples;
}

/** A schema as a reader of a state body uses it, which is all this needs of another checkout's
 * zod: its answer, and the sentence a refusal carries. */
interface BodyReader {
  safeParse: (body: unknown) => { success: boolean; error?: { message: string } };
}

/** The count of fix attempts a world's pull request carries where its label's daemon serves one
 * (`RecordFacts`' `fixAttempts`): that daemon raises it only at a push onto a head whose check
 * verdict is red, and every check of every world's head concludes `success` (rig.sh's
 * `concluded_checks`), so it stays 0. */
export const WORLD_FIX_ATTEMPTS = 0;

/** Why the contract of `checkout` refuses a state body the stand-in serves, or undefined where
 * it describes every one (`sampleFacts`): that checkout's own `LegionStateResponse`
 * (packages/contracts/src/legion-api.ts), the schema the plugin its label's agents load reads
 * every state answer with. The bodies are the ones the stand-in serves that label: their pull
 * request carries `fixAttempts` where the checkout's daemon serves it (daemon-pane.ts's
 * `servesFixAttempts`, read out of its source), and none where it does not; they carry the
 * contract mark, the claims' lowest contract and each claim's update state where the checkout's
 * daemon states its contract (daemon-pane.ts's `statesContract`), and none of them where it does
 * not; and their repositories block is the repositories the live records name where the
 * checkout's daemon serves that (daemon-pane.ts's `repositoriesLive`), and the project's mapping
 * where it does not. The module is another checkout's, at a path this process is handed at run
 * time, so no static import reaches it; a checkout that has none, or one that exports no such
 * schema, is a refusal too, naming the file, since a label whose contract cannot be read is never
 * answered as some other contract would describe it; and so is a daemon source of no shape,
 * naming each marker and its file. */
export async function contractRefusal(checkout: string): Promise<string | undefined> {
  const contract = path.join(checkout, "packages", "contracts", "src", "legion-api.ts");
  let loaded: unknown;
  try {
    loaded = await import(contract);
  } catch (error) {
    return `${contract} could not be loaded: ${error instanceof Error ? error.message : String(error)}`;
  }
  const schema =
    typeof loaded === "object" && loaded !== null && "LegionStateResponse" in loaded
      ? (loaded.LegionStateResponse as Partial<BodyReader> | undefined)
      : undefined;
  if (typeof schema?.safeParse !== "function")
    return `${contract} exports no LegionStateResponse to read a state body with`;
  let counted: boolean;
  let stated: number | undefined;
  let live: boolean;
  try {
    counted = servesFixAttempts(checkout);
    stated = statesContract(checkout);
    live = repositoriesLive(checkout);
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  const fixAttempts = counted ? WORLD_FIX_ATTEMPTS : undefined;
  for (const { name, facts } of sampleFacts(fixAttempts, stated, live)) {
    const read = schema.safeParse(stateBody(facts, facts.admittedAt));
    if (!read.success)
      return `${contract} does not describe the state body the stand-in serves (${name}): ${read.error?.message ?? "refused"}`;
  }
  return undefined;
}

if (import.meta.main) {
  const [command, label, checkout, ...rest] = Bun.argv.slice(2);
  if (command !== "contract" || label === undefined || checkout === undefined || rest.length > 0) {
    console.error("usage: bun standin-record.ts contract <label> <checkout>");
    process.exit(2);
  }
  const refusal = await contractRefusal(checkout);
  if (refusal !== undefined) {
    console.error(`standin-record contract: label ${label}: ${refusal}`);
    process.exit(1);
  }
}
