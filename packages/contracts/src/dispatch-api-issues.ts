import { z } from "zod";
import type { IssueCollaboratorPerson } from "./dispatch-api.generated";
import { ActorSchema, ExternalLinkSchema } from "./dispatch-api-core";
import { referenceChangesFields } from "./dispatch-api-refs";
import { ISSUE_COMPONENTS_MODES, type ISSUE_ROUTE_STATUSES } from "./dispatch-tools";

/**
 * The issue: the row, what resolves on each read of it, and the payloads of every `issue.*` and
 * `child.*` event. Each payload is `z.output` of the schema beside it, so the row and the event
 * that reports it are written once.
 */

/** An issue's coarse priority, `0` (`P0`, highest) through `3`. */
export const IssuePrioritySchema = z.union([
  z.literal(0),
  z.literal(1),
  z.literal(2),
  z.literal(3),
]);

export type IssuePriority = z.output<typeof IssuePrioritySchema>;

export type IssueComponentsMode = (typeof ISSUE_COMPONENTS_MODES)[number];

/**
 * An issue's effective component attachment, resolved on read: the nearest issue on its
 * parent chain (itself first) with its own attachment decides, and `inherited_from` names it
 * when that is an ancestor. Mode `inherit` with `inherited_from` null means no ancestor chose
 * (unassigned). `ids` is the effective set of live component ids; `unknown` the effective ids
 * a re-import has since retired; `reason` is set for mode `none`.
 */
export const IssueComponentsSchema = z
  .strictObject({
    mode: z.enum(ISSUE_COMPONENTS_MODES),
    ids: z.array(z.string()),
    unknown: z.array(z.string()),
    reason: z.string().nullable(),
    inherited_from: z.string().nullable(),
  })
  .readonly();

/**
 * The session (or human) working an issue: who claimed it and when. At most one claim exists
 * at a time, and it is not the issue's `route` (where messages go) or its `assignee` (the
 * human who answers its asks). A claim and the issue's status are separate records: claiming
 * never moves the status, and no status change claims.
 */
export const IssueClaimSchema = z
  .strictObject({
    actor: ActorSchema,
    /** RFC3339 timestamp of the claim. */
    at: z.string(),
  })
  .readonly();

/**
 * The state of the session holding an issue's claim, as an issue read reports it beside the
 * claim: `live`, `unresponsive` (past the live window, inside the gone horizon - stalled, not
 * gone, and still the holder), `gone` (past the horizon, or no registration at all), or
 * `unknown` when the Envoy listener gave the read no list to judge it by.
 */
export type IssueClaimHolderState = "live" | "unresponsive" | "gone" | "unknown";

/**
 * What an issue read says about the session holding its claim. It is resolved on read from the
 * server's copy of the listener's session list, at most 5 s old, stored in no table and never on
 * an event payload - which is why it sits beside `claim` rather than inside it: `IssueClaim` is
 * the payload of `issue.claimed` and `issue.released`, and a read-time state must not persist
 * into one. A claim write judges the holder again, against the listener's authoritative
 * per-session read, so the two can disagree by the age of that copy.
 *
 * All three are null when nobody holds the issue, and when a person does: a human holder has no
 * session to be live or gone, and is released by a person alone.
 */
export interface IssueClaimLiveness {
  readonly holder_state: IssueClaimHolderState | null;
  /** RFC3339 timestamp of the holder's last registration; null when the read cannot say. */
  readonly last_seen: string | null;
  /**
   * RFC3339 timestamp from which another agent may take or release the claim without `force`:
   * the holder's `last_seen` plus `ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS` while it is unresponsive,
   * the read's own time once the holder is gone, and null for a `live` holder, which no waiting
   * frees, and for an `unknown` one, which this read cannot judge.
   */
  readonly takeable_at: string | null;
}

export const issueFields = {
  key: z.string(),
  project: z.string(),
  number: z.number().int(),
  title: z.string(),
  status: z.string(),
  priority: IssuePrioritySchema.nullable(),
  rank: z.string(),
  labels: z.array(z.string()),
  parent: z.string().nullable(),
  /** Issue keys in the same project this issue waits on, sorted. */
  blocked_by: z.array(z.string()),
  /** Lowercase email of the person who answers this issue's asks; null when unassigned. */
  assignee: z.string().nullable(),
  /** The session or human working this issue, or null when nobody has claimed it. */
  claim: IssueClaimSchema.nullable(),
  components: IssueComponentsSchema,
  external_links: z.array(ExternalLinkSchema),
  route: z.string().nullable(),
  /** The issue's own repository, owner/name, a mapping of its project; null when it inherits. */
  repo: z.string().nullable(),
  /** The repository Dispatch resolves for the issue: its own, else its nearest ancestor's, else the
   *  project's default mapping, else null. Legion records it when it first records the issue. */
  resolved_repo: z.string().nullable(),
  /** Null for issues created before Dispatch recorded their creator. */
  created_by: ActorSchema.nullable(),
  created_at: z.string(),
  updated_at: z.string(),
  closed_at: z.string().nullable(),
  primary_artifact_id: z.string(),
  /** The issue's Verification document, the one created as `verification.md`, its Plan, the one
   *  created as `plan.md`, and its decisions document, the one created as `decisions.md`
   *  (`Artifact.role`); `""` when the issue holds none. Absent from a Dispatch older than
   *  document roles, and `decisions_artifact_id` from one older than the design gate's steps. */
  verification_artifact_id: z.string().optional(),
  plan_artifact_id: z.string().optional(),
  decisions_artifact_id: z.string().optional(),
  last_seq: z.number().int(),
};

export const IssueSchema = z.strictObject(issueFields).readonly();

/**
 * Whether an issue's route reaches a running session right now: `live` when a live session
 * holds the role or the routed session is live, `no_holder` when nobody live holds the role (it
 * is unclaimed or its holder's session lapsed) or the session is not live, `unknown` when the
 * Envoy listener gave no list to judge it by (never read as live, nor as `no_holder`).
 */
export type IssueRouteStatus = (typeof ISSUE_ROUTE_STATUSES)[number];

/**
 * Why a route reads `unknown`: `refilling` when the Envoy listener answered that its agent list
 * is refilling (it will be back in a moment, and a missing holder there says nothing), and
 * `unreachable` when the listener is not configured, did not answer, or failed otherwise.
 */
export type IssueRouteStatusReason = "refilling" | "unreachable";

/**
 * Every issue read (the detail, the list, the pinned list) and every ask read (`AskReach`, for
 * the ask's issue's route) resolves this from the server's copy of the listener's session list,
 * at most 5 s old. It is stored in no table and is never on an event payload, which is why it is
 * not a field of `Issue`. All three are null when there is no route.
 */
export interface IssueRouteReach {
  readonly route_status: IssueRouteStatus | null;
  /** The live session the route reaches; set only when `route_status` is `live`. */
  readonly route_holder: string | null;
  /** Why the route could not be judged; set exactly when `route_status` is `unknown`. */
  readonly route_status_reason: IssueRouteStatusReason | null;
}

/** When a check runs: before the merge, after it, or against the default branch. The closed
 *  enum `pmdoc/schema/blocks.json` declares under the `when` attribute's `choices`. */
export const CheckWhenSchema = z.enum(["before_merge", "after_merge", "default_branch"]);

export type CheckWhen = z.output<typeof CheckWhenSchema>;

/** How an issue counts for a component; the strongest wins when several apply. */
export type ArchitectureTreeAttachment = "direct" | "inherited" | "contained";

/** One way a collaborator worked an issue, in the order `reasons` lists them. The reason set is
 *  the handler's own (`issue_collaborators.go`'s `collaboratorReasons`), so the document carries
 *  it as a closed union on each `reasons` array rather than as a schema of its own; this
 *  projects one element of that array, which is the name a consumer imports. */
export type CollaboratorReason = NonNullable<IssueCollaboratorPerson["reasons"]>[number];

/** What an `issue.created`, `issue.updated` or `issue.closed` write changed, read in its own
 *  transaction. Each field is absent from every other `issue.*` type and from an event written
 *  before the fields existed, which a reader takes as "anything may have changed". */
export const issueEventChangesFields = {
  /** Each field whose value the write moved, of `title`, `status`, `closed_at` (for a close or a
   *  reopen), `rank`, `priority`, `assignee`, `parent`, `labels`, `route`, `repo`, `components`,
   *  `blocked_by` and `external_links`; for `issue.created`, every field the new issue holds a
   *  value in. */
  changed: z.array(z.string()).optional(),
  /** The issue's parent chain after the write, parent first up to the root; `[]` for a root. */
  ancestors: z.array(z.string()).optional(),
  /** Present only when the status or the assignee moved: for a close or a reopen every login
   *  with a Replies row on one of the issue's open asks, for an assignee move the assignee the
   *  issue was taken from and the one it was given to - a side holding nobody named as the
   *  project's default assignee, the person the issue's questions reach while nobody holds it -
   *  for the two together the sorted union of both, and for any other status move `[]`. */
  thread_logins: z.array(z.string()).optional(),
  /** On an `issue.created` whose create went past the near-copy refusal with `force`: `past`
   *  names the issues that refusal named, and is the empty list when the check did not answer.
   *  A forced create the refusal would not have stopped carries no `forced` at all. */
  forced: z
    .strictObject({ past: z.array(z.string()) })
    .readonly()
    .optional(),
};

export const IssueEventChangesSchema = z.strictObject(issueEventChangesFields).readonly();

export type IssueEventChanges = z.output<typeof IssueEventChangesSchema>;

/** Every `issue.*` event payload is the issue row, flat, plus what the write moved in the
 *  reference graph: a new issue's spec text is indexed in the creating transaction. */
export const WrittenIssueEventPayloadSchema = z
  .strictObject({ ...issueFields, ...referenceChangesFields, ...issueEventChangesFields })
  .readonly();

export type IssueEventPayload = z.output<typeof WrittenIssueEventPayloadSchema>;

/**
 * What a reader of the bus makes of an `issue.*` payload: the ten fields Dispatch clients read
 * today, each optional because an event retained from before a field existed carries none, and
 * everything else dropped. A frame is untrusted, so a malformed display field must not discard a
 * valid event header, and `forced` is read loosely for the same reason.
 */
export const IssueEventPayloadSchema = z.object({
  title: issueFields.title.optional(),
  labels: issueFields.labels.optional(),
  status: issueFields.status.optional(),
  priority: issueFields.priority.optional(),
  rank: issueFields.rank.optional(),
  route: issueFields.route.optional(),
  changed: issueEventChangesFields.changed,
  ancestors: issueEventChangesFields.ancestors,
  thread_logins: issueEventChangesFields.thread_logins,
  forced: z.object({ past: z.array(z.string()) }).optional(),
});

/**
 * `issue.claimed` and `issue.released`: who is working the issue now, whose claim this one
 * replaced or cleared, and why. `previous_claim` is present on a takeover (the holder's
 * session was no longer live), a release, and a close; the previous claimant's own agent
 * topic receives the event so it learns it no longer holds the work.
 */
export const issueClaimEventPayloadFields = {
  key: z.string(),
  status: z.string(),
  /** The issue's `repo` and `resolved_repo`, as on every issue event. */
  repo: z.string().nullable(),
  resolved_repo: z.string().nullable(),
  claim: IssueClaimSchema.nullable(),
  previous_claim: IssueClaimSchema.optional(),
  reason: z.enum(["claimed", "takeover", "forced", "released", "closed"]),
};

export const IssueClaimEventPayloadSchema = z.strictObject(issueClaimEventPayloadFields).readonly();

export type IssueClaimEventPayload = z.output<typeof IssueClaimEventPayloadSchema>;

/**
 * `issue.released` always says whose claim it cleared: the server appends the event only with a
 * claim in hand, whether the holder released it, a human did, or a close did. So readers of a
 * release never have to handle a missing previous claim, and must not pretend to.
 */
export const IssueReleasedEventPayloadSchema = z
  .strictObject({ ...issueClaimEventPayloadFields, previous_claim: IssueClaimSchema })
  .readonly();

export type IssueReleasedEventPayload = z.output<typeof IssueReleasedEventPayloadSchema>;

/**
 * `issue.deployed`: the delivery timeline's word that the issue's pull request reached production,
 * written once per pull request the moment the timeline first knows the production run that
 * shipped it (by intake or by the reconcile), with the system actor `delivery-timeline`, and
 * published on the issue's subject (`notifications.dispatch.issue.<KEY>.issue.deployed`) as every
 * issue event is. `repo` and `number` name the pull request, `run_id` the production run, and
 * `completed_at` when that run's production job completed, in UTC. The Legion daemon no longer
 * reads it: nothing a deployment publishes wakes a production check. Strict: the Go writer
 * (`packages/envoy/internal/dispatch/delivery/deployed.go`) emits exactly these four fields.
 */
export const IssueDeployedEventPayloadSchema = z.strictObject({
  repo: z.string(),
  number: z.number().int().positive(),
  run_id: z.number().int().positive(),
  completed_at: z.iso.datetime(),
});

export type IssueDeployedEventPayload = z.output<typeof IssueDeployedEventPayloadSchema>;

export const childStatusEventPayloadFields = {
  child_key: z.string(),
  from: z.string(),
  to: z.string(),
};

export const WrittenChildStatusEventPayloadSchema = z
  .strictObject(childStatusEventPayloadFields)
  .readonly();

export type ChildStatusEventPayload = z.output<typeof WrittenChildStatusEventPayloadSchema>;

/** What a reader of the bus makes of a `child.status` payload: each field optional, because a
 *  frame is untrusted and a missing one must not discard the event. */
export const ChildStatusEventPayloadSchema = z.object({
  child_key: childStatusEventPayloadFields.child_key.optional(),
  from: childStatusEventPayloadFields.from.optional(),
  to: childStatusEventPayloadFields.to.optional(),
});

export const ChildEventPayloadSchema = z.strictObject({ child_key: z.string() }).readonly();

export type ChildEventPayload = z.output<typeof ChildEventPayloadSchema>;

/** `result.recorded`: one result recorded against a check, appended on the **check's** issue and
 *  naming in `issue` the issue it was recorded on. */
export const CheckResultEventPayloadSchema = z
  .strictObject({
    result_id: z.string(),
    issue: z.string(),
    check_issue: z.string(),
    check_artifact_id: z.string(),
    check_block_id: z.string(),
    check_version: z.number().int(),
    when: CheckWhenSchema,
    verdict: z.enum(["pass", "fail"]),
    role: z.string(),
    commit_sha: z.string(),
  })
  .readonly();

export type CheckResultEventPayload = z.output<typeof CheckResultEventPayloadSchema>;

/** `result.retracted`: one recorded result marked wrong, appended on the **check's** issue as
 *  `result.recorded` is, and naming in `issue` the issue the result was recorded on. The result
 *  itself is neither updated nor deleted, and `reason` is why it is wrong. */
export const CheckResultRetractedEventPayloadSchema = z
  .strictObject({
    result_id: z.string(),
    issue: z.string(),
    check_issue: z.string(),
    check_block_id: z.string(),
    commit_sha: z.string(),
    reason: z.string(),
  })
  .readonly();

export type CheckResultRetractedEventPayload = z.output<
  typeof CheckResultRetractedEventPayloadSchema
>;

/** `issue.pull_request_attribution_changed`: a pull request Dispatch credited to one issue now
 *  belongs to another, which changes what that issue's results are measured against. One event
 *  goes on the issue it left and one on the issue it reached, each carrying this payload;
 *  `from` and `to` are those two issues, either null and never both, and `source` is what
 *  decided the new one (`external_link`, `github_issue_link`, `branch`, `title_body`,
 *  `commit_message`, or `none` where the pull request is now credited to nobody). */
export const IssuePullRequestAttributionEventPayloadSchema = z
  .strictObject({
    repo: z.string(),
    number: z.number().int(),
    url: z.string(),
    from: z.string().nullable(),
    to: z.string().nullable(),
    source: z.string(),
  })
  .readonly();

export type IssuePullRequestAttributionEventPayload = z.output<
  typeof IssuePullRequestAttributionEventPayloadSchema
>;
