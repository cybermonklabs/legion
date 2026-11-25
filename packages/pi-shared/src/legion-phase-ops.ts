import type { LegionRole } from "@legion/contracts";
import {
  LEGION_REVIEWER_VERDICTS,
  type LegionIssue,
  type LegionPhase,
} from "@legion/contracts/legion-api";
import type { z } from "zod";
import {
  type DaemonRestartRetry,
  LegionDaemonApiError,
  type LegionDaemonClient,
  retryWhileDaemonRestarts,
} from "./daemon-client";
import type { HandoffReport } from "./handoff-record";
import { isPhaseOp, PhaseOpArguments, REVIEW_STEP_ROLES } from "./legion-phase-op-declarations";

/** The claim a session registered, as the phase ops need it, and as the claim boot
 * (`legion-claim-core`) records it: its own id, the daemon's answer for tree, issue and role, the
 * secret every grant is minted with, and the claim token and launch generation the routes that
 * name the claim itself rather than a grant — `claims/ready`, `claims/turn-outcome`,
 * `claims/ready-for-update` — carry. The token and the secret are the pair the claim routes
 * authenticate a pod by, and they stay in this process: nothing writes either to disk. */
export interface ClaimCapability {
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
  readonly claimToken: string;
  readonly generation: number;
  /** The task agents the registration answered with (`LegionRegistration.promptAgents`), which
   * the session's own capability measurements check its discovered agents against long after the
   * boot returned. `bootLegionClaim` is this field's only producer, and every claim in production
   * carries the list: `packages/claude-envoy/src/legion-claim.ts`, the other host that records a
   * claim, records what that boot hands it. It is optional because a reader that finds no value
   * has a defined answer — it measures against no prompt agents — so a claim built without it,
   * which only a test does, is read rather than refused. */
  readonly promptAgents?: readonly string[];
}

/** The plugin build one process loaded, as its host's own `package.json` names it: the package
 * `name`, its `version`, and the daemon API contract that build speaks
 * (`legion.daemonApiVersion`, which the client states on every request as `pluginContract` and
 * `read_build` answers beside the daemon's own). It is the host's manifest and never a path on
 * the image: an Oh My Pi role's relaunch onto the daemon's contract installs the plugin pair
 * fresh under the role's state home, so the build at `/opt/legion/pi-legion` is another one. A
 * Claude Code role in a pod installs no plugin, and its build is its image's own claude-envoy. */
export interface PluginBuild {
  readonly name: string;
  readonly version: string;
  readonly pluginContract: number;
}

/** The three values of `PluginBuild` read off one host's manifest. `pi-shared` imports no
 * manifest and is bundled into both plugins, so each host calls this with the `package.json` it
 * imported (`packages/pi-legion/src/tools.ts`,
 * `packages/claude-envoy/src/legion-tool.ts`). */
export function pluginBuildOf(manifest: {
  readonly name: string;
  readonly version: string;
  readonly legion: { readonly daemonApiVersion: number };
}): PluginBuild {
  return {
    name: manifest.name,
    version: manifest.version,
    pluginContract: manifest.legion.daemonApiVersion,
  };
}

export interface PhaseOpRuntime {
  readonly daemon: LegionDaemonClient;
  readonly claim: ClaimCapability;
  /** The build this process loaded, which `read_build` answers with (`pluginBuildOf`). */
  readonly build: PluginBuild;
  /** What a completion by `role` reports beside its summary while the issue stands at `phase`
   * (`findHandoffReport` with the pane's environment and Dispatch): the newest record of the
   * phase the role works or, once the issue has moved on, of the phase it left, or `@-` alone where
   * that left phase has none or the implementer's left record names no head. Throws naming the remedy when the phase the role works has no
   * handoff record, Dispatch does not answer, or, while the issue stands in implementing, the
   * implementer's record names no head, the commit its workspace stands on is not pushed, or its
   * record's head is not that pushed commit. */
  readonly handoffReport: (phase: LegionPhase, role: LegionRole) => Promise<HandoffReport>;
  /** The head of `legion/<issue>@origin` the `ready_for_update` signal reports
   * (`findPushedBranchHead` with the pane's environment): `""` when the branch is not there. A
   * throw leaves the head out of the signal, which is still sent, and the throw's message goes
   * to the agent in the answer's `note`. */
  readonly branchHead: () => Promise<string>;
  /** How `handoff_complete` waits out a daemon restart (`retryWhileDaemonRestarts`): the host's
   * sentence to the agent, and a test's clock. */
  readonly restart?: DaemonRestartRetry;
  /** The clock `read_release_records` measures a release the daemon has not resumed yet against,
   * `Date` itself unless a test hands it another. */
  readonly now?: () => Date;
}

/** A grant for one write of the claim's session, minted on `/grants`' session form with the
 * claim's secret: every write of the `legion` tool posts a fresh one with its request. */
export async function mintClaimGrant(
  daemon: LegionDaemonClient,
  claim: ClaimCapability
): Promise<string> {
  const { grantId } = await daemon.grant({
    sessionId: claim.sessionID,
    secret: claim.secret,
    tree: claim.tree,
    issue: claim.issue,
  });
  return grantId;
}

/** One issue's record, read from the record route (`LegionDaemonClient.record`), which answers
 * about two kilobytes where the whole-project body answers megabytes. The words for an issue
 * the daemon records nothing of are the ones `read_record` answered while it read the project.
 * Every other refusal of the route throws as it came. */
async function recordFrom(daemon: LegionDaemonClient, issue: string): Promise<LegionIssue> {
  const record = await daemon.record(issue);
  if (record === undefined) throw new Error(`The daemon has no record for ${issue}`);
  return record;
}

/** The issue a call of `op` sends, by the claim's role. An architect's grant serves its whole
 * tree, so its call names the issue it acts on, which is sent. A phase worker's grant names its own
 * issue, the only one it acts on, so its call leaves `issue` out or names that one, and none is
 * sent; one naming another issue is refused here in `otherIssue`'s words, as the daemon would
 * refuse it (ISSUE_NOT_GRANTS). */
function issueToSend(
  op: "request_backward_move" | "review_verdict",
  claim: ClaimCapability,
  named: string | undefined,
  otherIssue: (named: string) => string
): string | undefined {
  const issue = named?.trim() ?? "";
  if (claim.role === "architect") {
    if (issue === "") throw new Error(`${op} requires issue`);
    return issue;
  }
  if (issue !== "" && issue !== claim.issue) throw new Error(otherIssue(issue));
  return undefined;
}

const ROOT_COMPLETION_REFUSAL =
  "handoff_complete is not available to a root architect session, which runs no phase";

/** The three words `read_build` answers under `daemonContractNote`, where `daemonContract` is
 * null: the daemon answered the health route and stated no contract, which a daemon older than
 * the header does; the daemon answered the route and refused it; or no answer reached this
 * process at all, which is what a daemon that is down or unreachable leaves. */
const DAEMON_STATED_NO_CONTRACT = "the daemon stated no contract";
const DAEMON_REFUSED_ROUTE = "the daemon refused the health route";
const DAEMON_DID_NOT_ANSWER = "the daemon did not answer at all";

/** The contract the daemon states on its answer to the health route, with the word for an
 * answer that carries no number. `daemonContract` throws on a refusal and on the transport
 * failure of a daemon that is down, which is the moment a role most needs to know which build
 * it runs, so both are answered as the note, and the note says which of the two happened: the
 * package name, the version and this build's own contract need no request and are reported
 * whatever the daemon does. */
async function statedDaemonContract(
  daemon: LegionDaemonClient
): Promise<{ readonly contract: number | null; readonly note: string | null }> {
  let stated: number | undefined;
  try {
    stated = await daemon.daemonContract();
  } catch (error) {
    // The route is read without a schema, so the client raises no contract error for it: the
    // daemon answered and refused (`LegionDaemonApiError`, which carries the status), or the
    // request reached no answer at all and the transport threw.
    return {
      contract: null,
      note: error instanceof LegionDaemonApiError ? DAEMON_REFUSED_ROUTE : DAEMON_DID_NOT_ANSWER,
    };
  }
  return stated === undefined
    ? { contract: null, note: DAEMON_STATED_NO_CONTRACT }
    : { contract: stated, note: null };
}

/** `read_build`: the package name, the version and the contract of the build this process
 * loaded, and the contract the daemon states on its answer to the health route, null where it
 * states none or does not answer, with `daemonContractNote` saying which. The three local
 * values need no request. Every claim session reaches it through `executePhaseOp`, and the
 * controller's session, which holds no claim, calls it here
 * (`packages/pi-legion/src/tools.ts`). */
export async function readBuild(runtime: {
  readonly daemon: LegionDaemonClient;
  readonly build: PluginBuild;
}): Promise<Readonly<Record<string, unknown>>> {
  const stated = await statedDaemonContract(runtime.daemon);
  return {
    plugin: runtime.build.name,
    version: runtime.build.version,
    pluginContract: runtime.build.pluginContract,
    daemonContract: stated.contract,
    daemonContractNote: stated.note,
  };
}

/**
 * Runs one phase op and answers its JSON result: `{ record }` for `read_record`, of the issue it
 * names or of the claim's own when it names none, the window's records and their counts for
 * `read_release_records`, the daemon's answer for `handoff_complete`, `{}` for
 * `request_backward_move`, `{}` for `review_verdict`, this process's build and the daemon's
 * stated contract for `read_build`, and `{}` for `ready_for_update`, or `{ note }` when its
 * branch-head lookup failed. A refusal throws before any write, minting nothing and posting
 * nothing: each problem `PhaseOpArguments` finds, joined by `; `, a field the operation does not
 * take named first, then an operation this session's claim may not run (a root architect's
 * completion, and a `review_verdict` from any claim but a review step's agent's, whichever host
 * runs it), then each problem with the operation's own fields in the order it takes them, and a
 * completion's `plan_rating` or `plan_reason` without the other; then the issue a completion, a
 * move or a verdict names against the claim (`issueToSend` for the move and the verdict); then
 * what the record decides (a check report's verdict, its claim's role against the check's own,
 * its `wake_at`, a verdict from any role but the reviewer, a `pending` or a `wake_at` on a phase
 * completion, a completion's `return_to` beside a verdict or on a done issue),
 * then a refused handoff report (`findHandoffReport`). A daemon refusal throws its code and
 * sentence.
 *
 * A completion is a phase's or a look's report on the closed issue's production check. The daemon
 * tells them apart by the caller's own live assignment (`api.handoffComplete`,
 * `supervise.Claim.ServingCheck`), which this tool cannot read: it shapes the call by the issue's
 * phase instead, a call on a `done` issue carrying what a check report carries and every other
 * call what its phase's completion carries, and the daemon's answer decides. A check report names
 * its verdict (`pass`, `pending` or `fail`), and a `pending` whose look is not the check's last
 * the `wake_at` of its next look; no handoff was written and nothing pushed, so no record or commit
 * is looked up and none is sent. A phase completion carries no verdict but the reviewer's
 * approve or changes, since Dispatch's check states at the tested head decide the testing phase,
 * and no `wake_at`: each other verdict, a `pending` and a `wake_at` are refused there, before the
 * report is looked up, and so is a `return_to` beside the reviewer's verdict, since only a
 * tester's completion, which carries none, names the step its task returns to. A phase
 * completion with no verdict sends its `return_to` as given: which role may name one is the
 * daemon's to hold (`RETURN_TO_NEEDS_FAIL`). A rating of the plan is sent as given on either: which
 * completion ends the retro, and so takes one, is the daemon's to say (`PLAN_RATING_UNEXPECTED`,
 * `PLAN_RATING_REQUIRED`).
 */
export async function executePhaseOp(
  runtime: PhaseOpRuntime,
  input: unknown
): Promise<Readonly<Record<string, unknown>>> {
  const { daemon, claim } = runtime;
  const parsed = PhaseOpArguments.safeParse(input);
  const issues = parsed.success ? [] : parsed.error.issues;
  const unknownFields = issues.filter((issue) => issue.code === "unrecognized_keys");
  const fieldProblems = issues.filter((issue) => issue.code !== "unrecognized_keys");
  // The op the call names, as a phase op, so the names below are checked against `PhaseOp`.
  const sent = typeof input === "object" && input !== null && "op" in input ? input.op : undefined;
  const op = typeof sent === "string" && isPhaseOp(sent) ? sent : undefined;
  const unavailable =
    op === "handoff_complete" && claim.role === "architect" && claim.issue === claim.tree
      ? ROOT_COMPLETION_REFUSAL
      : op === "review_verdict" && !REVIEW_STEP_ROLES.includes(claim.role)
        ? `review_verdict is the verdict of a review step's agent, the reviewer or the architect that owns the issue, and this session is the ${claim.role}'s`
        : undefined;
  if (unavailable !== undefined) {
    throw new Error([...unknownFields.map((issue) => issue.message), unavailable].join("; "));
  }
  if (!parsed.success) {
    throw new Error([...unknownFields, ...fieldProblems].map((issue) => issue.message).join("; "));
  }
  const args = parsed.data;
  switch (args.op) {
    case "read_record": {
      const named = args.issue?.trim() ?? "";
      const issue = named !== "" ? named : claim.issue;
      return { record: await recordFrom(daemon, issue) };
    }
    case "read_release_records":
      return releaseRecords(runtime, args.since);
    case "handoff_complete": {
      // The claim names the one issue a completion reports, so an `issue` in the call is only
      // checked against it and never sent: one naming another issue is refused here, before the
      // record is read or a grant minted.
      const named = args.issue?.trim() ?? "";
      if (named !== "" && named !== claim.issue) {
        throw new Error(
          `handoff_complete reports this session's own issue ${claim.issue}; leave issue out, or name ${claim.issue}, not ${named}`
        );
      }
      // The rating of the plan is built here, where the call was parsed: `PhaseOpArguments`' pair
      // refines hold the two fields together, so this is where the two become the one value the
      // completion carries, under the daemon's names, or nothing.
      const { plan_rating: planRating, plan_reason: planReason } = args;
      const rating =
        planRating === undefined || planReason === undefined
          ? undefined
          : { planRating, planReason };
      // Retried whole while the daemon restarts: the record, the handoff report and a fresh grant
      // are read again on each attempt. A repeat never applies twice, but it cannot tell a landed
      // attempt from a refused one (the daemon records both under the completion's key), so its
      // refusal is passed on, marked as possibly following an applied attempt.
      return retryWhileDaemonRestarts(
        "handoff_complete",
        () => completeHandoff(runtime, args, rating),
        runtime.restart
      );
    }
    case "request_backward_move": {
      // An architect's move names the issue it returns from accepting, and a phase worker's leaves
      // it out (`issueToSend`). The architect's names the head it sends back too, its acceptance's
      // changes there, and a phase worker's names none. The tester's move out of testing is the
      // daemon's to refuse (BACKWARD_REFUSED), and the refusal's sentence reaches the agent: a
      // testing step ends by its checks' states alone, so the tester records a fail of each check
      // it cannot verify and completes. The issue's phase and its pull request's head are the
      // daemon's record, which this call does not read (VERDICT_HEAD_NOT_CURRENT).
      const issue = issueToSend(
        "request_backward_move",
        claim,
        args.issue,
        (named) => `a phase worker moves only its own issue ${claim.issue}, not ${named}`
      );
      const commit = args.commit?.trim() ?? "";
      if (claim.role === "architect" && commit === "") {
        throw new Error("request_backward_move requires commit");
      }
      if (claim.role !== "architect" && commit !== "") {
        throw new Error(
          "a phase worker's request_backward_move names no commit: only the architect that owns an issue in accepting names the head it sends back"
        );
      }
      await daemon.phaseBackward({
        grantId: await mintClaimGrant(daemon, claim),
        to: args.to,
        reason: args.reason,
        ...(issue === undefined ? {} : { issue }),
        ...(commit === "" ? {} : { commit }),
      });
      return {};
    }
    case "review_verdict": {
      // The verdict of a review step's agent on a head its pull request moved to once a review step
      // of the issue's task has passed: no completion and no handoff, so no record is read and no
      // report looked up, and it ends no phase. The owning architect's names the issue it accepted,
      // and the reviewer's leaves it out (`issueToSend`). Whether this claim ran a review step of
      // the issue's task, whether that step has passed, never the step that runs now, and whether
      // the head is current is the daemon's to hold (VERDICT_CALLER_FORBIDDEN,
      // VERDICT_NOT_PAST_REVIEW, VERDICT_HEAD_NOT_CURRENT). Not retried: each request is a verdict
      // of its own.
      const issue = issueToSend(
        "review_verdict",
        claim,
        args.issue,
        (named) => `a reviewer sends its verdict on its own issue ${claim.issue}, not ${named}`
      );
      await daemon.reviewVerdict({
        grantId: await mintClaimGrant(daemon, claim),
        ...(issue === undefined ? {} : { issue }),
        verdict: args.verdict,
        commit: args.commit,
        summary: args.summary,
      });
      return {};
    }
    case "ready_for_update": {
      // The claim authenticates the signal, as it does `claims/ready`: no grant is minted. The
      // round is the agent's own word, read from the update message it answers, and the daemon
      // takes the answer for that round alone — an answer for a round its claim has left is
      // refused naming both. The head is the plugin's to resolve, and a branch not yet on the
      // remote is left out. A workspace whose jj cannot answer leaves it out too, and is told so:
      // the round ends before its bound only once every claim of the pod has answered, no turn
      // runs and no claim's activity report holds it (the daemon's `roundEnd.holding`), so a
      // worker whose lookup fails must still be able to answer. The daemon verifies no head, so
      // the missing one costs the record a line and the round nothing.
      let head = "";
      let failure: string | undefined;
      try {
        head = await runtime.branchHead();
      } catch (error) {
        failure = error instanceof Error ? error.message : String(error);
      }
      await daemon.readyForUpdate({
        claimToken: claim.claimToken,
        sessionId: claim.sessionID,
        secret: claim.secret,
        generation: claim.generation,
        round: args.round,
        ...(head === "" ? {} : { head }),
      });
      // The answer names the signal the daemon has and the head it misses, and carries the
      // lookup's own message, so the worker can repair what it names.
      return failure === undefined
        ? {}
        : {
            note:
              "The daemon has your ready_for_update signal. The signal reports no branch head, " +
              "because the branch-head lookup failed. The daemon checks no head, so your signal " +
              "counts without one. Repair what the lookup's message names before your next jj " +
              `command. The lookup said: ${failure}`,
          };
    }
    case "read_build":
      // The one read of this operation, and it asks for no project state: the health route,
      // read for its `Legion-Daemon-Contract` header alone (`LegionDaemonClient.daemonContract`
      // parses no body, so a daemon whose health body this build cannot read still answers the
      // number). The build is the host's own manifest, passed in at the tool.
      return readBuild(runtime);
  }
}

/** A release woken again within this of its release is churn: the pod was given up and paid for
 * again at once (`docs/kubernetes.md`, "Idle release"). */
const WOKEN_SOON_MS = 10 * 60 * 1000;

/** How many rows of each `key`, the key's own order kept. */
const countedBy = <Row>(
  rows: readonly Row[],
  key: (row: Row) => string
): Readonly<Record<string, number>> => {
  const counts: Record<string, number> = {};
  for (const row of rows) counts[key(row)] = (counts[key(row)] ?? 0) + 1;
  return counts;
};

/** The nearest-rank percentile of `sorted`, ascending: the smallest value `share` of the rows
 * are at or below. Null for no rows, which is a window nothing was measured in rather than a
 * zero. */
const percentile = (sorted: readonly number[], share: number): number | null =>
  sorted.length === 0 ? null : (sorted[Math.max(Math.ceil(share * sorted.length), 1) - 1] ?? null);

/**
 * `read_release_records`: the window's records from the daemon, with the numbers a report of the
 * deployment's idle release is written from, so an agent counts nothing itself. The claim's own
 * token and secret authenticate the read, as every other claim route is authenticated, and they
 * never leave this process.
 *
 * A release that suspended no role is known by the empty `roles` list the record already carries:
 * every release of the rule that reclaims an issue pod no claim can use is one, and so is the
 * rare idle release whose stops suspended nobody. No role of it is ever woken, so nothing records
 * when its issue's pod came back, and the three measurements of a release's life leave it out.
 * `counts.releases` counts every release and `counts.releasesSuspendingNoRole` counts these.
 *
 * `podHoursReleased` is how long the released pods were gone: from each release that suspended a
 * role to the first wake of its pod, and to `asOf` for one nothing has woken yet, since that pod
 * is gone now. A pod's roles wake one resume each, so the first of them ends the release. A
 * resume names the release it ended, except a wake a project pause held, which names none
 * (`release` 0, `PodRecords.TakePodWake` in `packages/daemon/internal/store/wake.go`). A held wake
 * therefore ends every release of its issue that came before it, where no resume that names the
 * release came sooner: a pod released before a pause and woken during it would otherwise be
 * charged to `asOf` at every read, for ever. The release ends at the wake's own moment, which is
 * when the pod was wanted again. The pod stays gone from there until the project runs again, and
 * that time is the pause's, as the wake's seconds to its first turn are. A release that
 * suspended no role is charged nothing, because charging it that way would bill the fleet from
 * its moment to the moment of every later read, for ever, though an operator's retry may have
 * rebuilt the pod a minute later.
 * `wokenWithinTenMinutes` counts the releases a wake followed that soon, a held wake as any
 * other, the churn of releasing a pod that was wanted again at once, over the releases that
 * suspended a role alone: a release no wake can reach belongs in neither half of that share,
 * which it would otherwise lower for ever. `wakeToFirstTurnSeconds` measures each resume no pause
 * held that the daemon has seen a first turn for, `withoutFirstTurn` counting the rest of the
 * resumes no pause held, and reports the nearest-rank median and 90th percentile and the
 * maximum; it reads the resume rows, of which a release that suspended no role has none, so it
 * leaves those releases out by its own shape. A resume that carries `pause` is a wake a project
 * pause held (`api.ResumeRecord`): it waited until the project ran again, so its seconds to the
 * first turn would measure the pause and not the pod's return. `heldByPause` counts those
 * resumes, and they are in neither `measured` nor `withoutFirstTurn`.
 */
async function releaseRecords(
  runtime: PhaseOpRuntime,
  since: string
): Promise<Readonly<Record<string, unknown>>> {
  const { claim } = runtime;
  const records = await runtime.daemon.releaseRecords({
    claimToken: claim.claimToken,
    secret: claim.secret,
    since,
  });
  const asOf = runtime.now?.() ?? new Date();
  const wokenAt = new Map<number, number>();
  const heldWakes = new Map<string, number[]>();
  for (const resume of records.resumes) {
    const at = Date.parse(resume.at);
    // A wake a pause held names no release, so it is kept under its issue for the releases of
    // that issue below.
    if (resume.pause !== undefined) {
      const held = heldWakes.get(resume.issue);
      if (held === undefined) heldWakes.set(resume.issue, [at]);
      else held.push(at);
      continue;
    }
    if (resume.release === 0) continue;
    const first = wokenAt.get(resume.release);
    if (first === undefined || at < first) wokenAt.set(resume.release, at);
  }
  let releasedMs = 0;
  let wokenSoon = 0;
  let suspendedNoRole = 0;
  for (const release of records.releases) {
    // The empty role list is the whole mark of a release no wake can reach, so it is counted
    // here and measured nowhere.
    if (release.roles.length === 0) {
      suspendedNoRole += 1;
      continue;
    }
    const from = Date.parse(release.at);
    let woken = wokenAt.get(release.id);
    for (const at of heldWakes.get(release.issue) ?? []) {
      if (at >= from && (woken === undefined || at < woken)) woken = at;
    }
    releasedMs += Math.max((woken ?? asOf.getTime()) - from, 0);
    if (woken !== undefined && woken - from <= WOKEN_SOON_MS) wokenSoon += 1;
  }
  const wakeable = records.releases.length - suspendedNoRole;
  let heldByPause = 0;
  let withoutFirstTurn = 0;
  const waits: number[] = [];
  for (const resume of records.resumes) {
    // A wake a project pause held is counted here and measured nowhere, whether or not its
    // first turn has come: its wait is the pause's.
    if (resume.pause !== undefined) heldByPause += 1;
    else if (resume.firstTurnAt === undefined) withoutFirstTurn += 1;
    else waits.push((Date.parse(resume.firstTurnAt) - Date.parse(resume.at)) / 1000);
  }
  waits.sort((one, other) => one - other);
  const whole = (value: number | null) => (value === null ? null : Math.round(value));
  return {
    since,
    asOf: asOf.toISOString(),
    releases: records.releases,
    resumes: records.resumes,
    holds: records.holds,
    counts: {
      releases: records.releases.length,
      releasesSuspendingNoRole: suspendedNoRole,
      resumes: records.resumes.length,
      resumesByKind: countedBy(records.resumes, (resume) => resume.kind),
      holds: records.holds.length,
      holdsByReason: countedBy(records.holds, (hold) => hold.reason),
    },
    podHoursReleased: Math.round((releasedMs / 3_600_000) * 100) / 100,
    wokenWithinTenMinutes: {
      releases: wokenSoon,
      share: wakeable === 0 ? 0 : Math.round((wokenSoon / wakeable) * 1000) / 1000,
    },
    wakeToFirstTurnSeconds: {
      measured: waits.length,
      withoutFirstTurn,
      heldByPause,
      median: whole(percentile(waits, 0.5)),
      p90: whole(percentile(waits, 0.9)),
      max: whole(waits.at(-1) ?? null),
    },
  };
}

/** The rating of the plan a completion carries, under the daemon's names: the two together, which
 * `PhaseOpArguments` takes only together. */
interface PlanRatingGiven {
  readonly planRating: number;
  readonly planReason: string;
}

/** One attempt of `handoff_complete` (`executePhaseOp`'s doc comment), with the rating of the plan
 * the call gave, or none. */
async function completeHandoff(
  runtime: PhaseOpRuntime,
  args: Extract<z.infer<typeof PhaseOpArguments>, { op: "handoff_complete" }>,
  rating: PlanRatingGiven | undefined
): Promise<Readonly<Record<string, unknown>>> {
  const { daemon, claim } = runtime;
  // The record is read before anything is minted: it says what this call has to carry. An issue
  // no role works (`done`) has no handoff for anyone to write, so no record or commit is looked
  // up, and the call carries what a check report carries; it is posted all the same where the
  // record shows no open check, for the daemon's own answer. Which of the two the call is, is the
  // daemon's to say, and it says it from the caller's own live assignment (`api/handoff.go`,
  // `supervise.Claim.ServingCheck`), which the record does not show: a session working a look of
  // its issue's production check reports on that look, and every other session's completion is
  // its phase's, which the daemon refuses on a `done` issue whatever it carries
  // (`CHECK_LOOK_NOT_SERVED`). So the two can disagree: a look's report from an issue back in
  // the workflow is refused here as a phase completion, and a phase's completion that outlived
  // the close is posted here as a report and refused `CHECK_LOOK_NOT_SERVED`, before the phase
  // path.
  // The report's own refusals apply while the record shows the check it is of: the record carries
  // that object only while the check is open. A refusal here posts nothing and mints nothing.
  const record = await recordFrom(daemon, claim.issue);
  if (record.phase === "done") {
    // A completion on a done issue is a report on its production check or one the daemon refuses
    // as a phase's (CHECK_LOOK_NOT_SERVED); neither names a step to return to, which the daemon
    // refuses on a check report whatever its verdict (RETURN_TO_NEEDS_FAIL).
    if (args.return_to !== undefined) {
      throw new Error(
        "handoff_complete's return_to names the step a tester's task returns to, should the check states at the head it tested fail its step, and a report on a production check names none; send it again without return_to"
      );
    }
    const check = record.check;
    if (check !== undefined) {
      if (args.verdict === undefined) {
        throw new Error(
          "handoff_complete on a production check names a verdict of pass, pending or fail"
        );
      }
      if ((LEGION_REVIEWER_VERDICTS as readonly string[]).includes(args.verdict)) {
        throw new Error(
          `handoff_complete on a production check names a verdict of pass, pending or fail, not ${args.verdict}, which is the reviewer's verdict on a phase completion`
        );
      }
      // The check records the role that works it, and takes a report from that role alone, as
      // the daemon does (CHECK_ROLE_MISMATCH).
      if (claim.role !== check.role) {
        throw new Error(
          `${claim.issue}'s production check is the ${check.role}'s, and this session is the ${claim.role}'s: only the role the check records reports on it`
        );
      }
      // The checking agent names its own next look, and nothing else starts one, so a pending
      // carries a wake wherever a look follows it (CHECK_WAKE_REQUIRED). A pass and a fail close
      // the check, and the last look's pending closes it as not run, so none of them carries one
      // (CHECK_WAKE_UNEXPECTED).
      if (args.verdict === "pending" && args.wake_at === undefined && !check.last) {
        throw new Error(
          `handoff_complete's pending report names wake_at, when the next look of ${claim.issue}'s production check is due`
        );
      }
      if (args.wake_at !== undefined && (args.verdict !== "pending" || check.last)) {
        const why =
          args.verdict === "pending"
            ? `look ${check.looks} is the check's last, after which the check closes as not run`
            : `a ${args.verdict} report closes the check`;
        throw new Error(
          `handoff_complete names wake_at and no look follows it: ${why}; send it again without a time`
        );
      }
    }
    return daemon.handoffComplete({
      grantId: await mintClaimGrant(daemon, claim),
      summary: args.summary,
      verdict: args.verdict ?? "",
      ...(args.wake_at === undefined ? {} : { wakeAt: args.wake_at }),
      ...rating,
    });
  }
  // A phase completion carries no verdict but the reviewer's, the one review step's agent that
  // completes a phase (the owner accepts with sign_off): Dispatch's check states at the tested head
  // decide the testing phase, and the daemon refuses any other role's phase completion that names
  // one (VERDICT_NOT_TAKEN from the tester, VERDICT_ROLE_FORBIDDEN from every other role). This
  // says so without the round trip, whatever the verdict.
  if (args.verdict !== undefined && claim.role !== "reviewer") {
    throw new Error(
      "handoff_complete carries no verdict on a phase completion but the reviewer's: Dispatch's check states decide the testing phase"
    );
  }
  // pending is a production check's word, never the reviewer's; which of its other words the
  // reviewer sends is the daemon's to hold (REVIEWER_VERDICT_REQUIRED).
  if (args.verdict === "pending") {
    throw new Error(
      "handoff_complete's verdict on the reviewer's completion is approve or changes; pending is a production check's"
    );
  }
  // Only a tester's completion names the step its task returns to, and it carries no verdict, so
  // the daemon refuses a return_to beside the reviewer's verdict (RETURN_TO_NEEDS_FAIL); this
  // refuses it first, in the daemon's words. Without a verdict it is sent as given, and the
  // daemon holds it to the tester's completion.
  if (args.verdict !== undefined && args.return_to !== undefined) {
    throw new Error(
      "handoff_complete names return_to beside a verdict, and only a tester's completion names the step its task returns to, should the check states at the head it tested fail its step: send it again without return_to"
    );
  }
  // The daemon refuses a phase completion that carries a wake (CHECK_WAKE_UNEXPECTED); this says
  // so without the round trip.
  if (args.wake_at !== undefined) {
    throw new Error(
      "handoff_complete's wake_at is a production check's pending report alone; a phase completion carries none"
    );
  }
  // The record and the commit are found before the grant is minted. The phase is the issue
  // record's, the role the claim's.
  const report = await runtime.handoffReport(record.phase, claim.role);
  return daemon.handoffComplete({
    grantId: await mintClaimGrant(daemon, claim),
    summary: args.summary,
    verdict: args.verdict ?? "",
    ...report,
    ...rating,
    ...(args.return_to === undefined ? {} : { returnTo: args.return_to }),
  });
}
