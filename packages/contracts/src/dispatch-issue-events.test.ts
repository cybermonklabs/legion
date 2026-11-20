import { expect, test } from "bun:test";
import { z } from "zod";
import {
  ActorActSchema,
  ActorOriginSchema,
  ActorSchema,
  ChangedReferenceSchema,
  DispatchEventSchema,
  ExternalLinkSchema,
  IssueClaimEventPayloadSchema,
  IssueClaimSchema,
  IssueComponentsSchema,
  IssueEventPayloadSchema,
  IssueReleasedEventPayloadSchema,
  PublishedIssueEventSchema,
  WrittenIssueEventPayloadSchema,
} from "./dispatch-api";
import { IssueEventEnvelopeSchema } from "./envelope";
import { ExampleWalk, type Form, type WalkedExample, type Wider } from "./example-walk.testing";
import { objectSchema, objectShape } from "./schema-inspection.testing";

/**
 * The issue event envelopes Dispatch's outbox publishes, shared with the Go writer
 * (`packages/envoy/internal/dispatch/outbox`, whose `TestTheBuiltIssueEnvelopesAreTheSharedFixtures`
 * requires the envelope it builds to equal each one, and the examples together to hold every field
 * it writes, set and in the form it writes when it holds nothing) and with the Legion daemon's
 * intake test, which requires every field the daemon reads.
 */
const EXAMPLES = [
  "issue-updated-every-field.json",
  "issue-claimed-every-field.json",
  "issue-claimed-forced.json",
  "issue-updated-no-field-moved.json",
  "issue-released-closed.json",
  "issue-updated-null-fields.json",
  "issue-claimed-first.json",
  "issue-released-null-claim.json",
  "issue-claimed-agent-token.json",
] as const;

type Example = (typeof EXAMPLES)[number];

/** The schema each event type's payload is written by (`DispatchEvent`), and for `issue.*` the
 *  tolerant schema a reader of the bus reads it with; a claim event's reader reads the written one. */
const PAYLOADS: Record<string, { written: unknown; read?: typeof IssueEventPayloadSchema }> = {
  "issue.updated": { written: WrittenIssueEventPayloadSchema, read: IssueEventPayloadSchema },
  "issue.claimed": { written: IssueClaimEventPayloadSchema },
  "issue.released": { written: IssueReleasedEventPayloadSchema },
};

const FORCED_CREATE = "on an issue.created alone";

/** Each key a schema declares that Dispatch never writes on these events, by the object that
 *  declares it, with the reason: the Go test's `neverWritten` says the same of the Go types. An
 *  example that carries one fails. */
const NEVER: Record<string, Record<string, string>> = {
  envelope: {
    expires_at: "the outbox's envelope() sets none",
    kept_at: "set on a role's kept copy (outbox/role.go), not the issue topic's copy these are",
    mention_dedupe_keys:
      "set on a role's kept copy (outbox/role.go), not the issue topic's copy these are",
    role: "the listener sets it on a direct send Dispatch's API makes to a role's holder (api/comment_delivery.go), never on the issue topic's copy the outbox builds, which these are",
    payload_ref: "the outbox's envelope() sets none",
    sender: "the outbox's envelope() sets none",
    in_reply_to: "set on an ask.answered, an ask.resolved, a comment and a message alone",
    supersedes: "the outbox's envelope() sets none",
    urgency: "set on an ask.* event alone",
    expects_reply: "the outbox's envelope() leaves it out of every event's envelope",
  },
  event: {
    trees: "the outbox publishes the event log's row, which holds no trees",
  },
  "issue.updated payload": {
    forced: FORCED_CREATE,
    references_changed_truncated:
      "an issue.updated names at most 41 issues: its parent and its blockers before and after the patch, 20 at most each, below the 100 of a kind that truncates",
    references_changed_truncated_kinds: "as references_changed_truncated",
  },
  "issue.updated payload read": { forced: FORCED_CREATE },
  "user actor": {
    act: "set by Dispatch's actingPerson alone, which no issue handler calls",
  },
  "changed reference": {
    issue_key: "an issue write names issue targets alone, which carry no address",
    artifact_id: "an issue write names issue targets alone, which carry no address",
    ref: "an issue write names issue targets alone, which carry no address",
  },
};

/** Each object a strict walk reads whose schema takes a key it does not declare, with the reason.
 *  Go writes no key its type does not declare, so every other object these examples are read
 *  strictly by - the event, each payload and every object inside them - must refuse one: a strict
 *  schema made loose fails here by its name. */
const LOOSE: Record<string, string> = {
  envelope:
    "IssueEventEnvelopeSchema is EnvelopeSchema's, which reads every producer's envelope, and a field added to it is one every reader takes without knowing it (envelope.ts): it drops a key it does not declare, and the test's read of each example (toEqual) holds the example to the declared keys",
};

/** The token reasons of an actor's absent keys (api.brokerTokenActor): a broker-issued token names
 *  a person or a service, never both, and a project only beside a person. */
const SERVICE_CALLER = "its token names a service, so no person and no project";
const PERSON_CALLER = "its token names a person, so no service";
const PERSON_ONLY_CALLER =
  "its token names a person and no single project, so no service and no project";
const NO_PLACE = "its requests name no place";
/** A session a personal token's request body names (api.bearerSessionActor): the token proves its
 *  person, the actor's owner, and proves no session, service or project. */
const NAMED_BY_BODY =
  "a personal token's body named the session, and the token proves only its person";
/** The origin a session outside tmux sends: where it runs, and no tmux pane or title. */
const OUTSIDE_TMUX = "the session runs outside tmux, so its origin names no pane";
const PERSON_WROTE = "a person wrote it, so the envelope has no session source";
/** The shared agent token every Dispatch takes (api.bearerSessionActor): its actor is the session
 *  its body names, and the token proves no person, service, project or session. */
const SHARED_TOKEN =
  "the shared agent token proves no person, service, project or session, so the actor is the session its body names and no more";

/**
 * Each key a schema declares that one example does not hold, by the example and the key's path in
 * it, with the reason that write leaves it out. Every other key each object of an example's
 * schemas declares, NEVER's apart, must be in that object: so a key removed from an example fails
 * here, and so does an entry here that names a key its example holds or a path no schema reaches.
 * The paths start at the `envelope`, its `event` and that event's `payload`.
 */
const ABSENT: Record<Example, Record<string, string>> = {
  "issue-updated-every-field.json": {
    "event.actor.owner": SERVICE_CALLER,
    "event.actor.project": SERVICE_CALLER,
    "event.payload.claim.actor.owner": SERVICE_CALLER,
    "event.payload.claim.actor.project": SERVICE_CALLER,
  },
  "issue-claimed-every-field.json": {
    "event.actor.service": PERSON_CALLER,
    "event.payload.claim.actor.service": PERSON_CALLER,
    "event.payload.previous_claim.actor.owner": SERVICE_CALLER,
    "event.payload.previous_claim.actor.project": SERVICE_CALLER,
  },
  "issue-claimed-forced.json": {
    "event.actor.origin.tmux": OUTSIDE_TMUX,
    "event.actor.origin.pane": OUTSIDE_TMUX,
    "event.actor.origin.session_title": OUTSIDE_TMUX,
    "event.actor.service": NAMED_BY_BODY,
    "event.actor.project": NAMED_BY_BODY,
    "event.actor.proven": NAMED_BY_BODY,
    "event.payload.claim.actor.origin.tmux": OUTSIDE_TMUX,
    "event.payload.claim.actor.origin.pane": OUTSIDE_TMUX,
    "event.payload.claim.actor.origin.session_title": OUTSIDE_TMUX,
    "event.payload.claim.actor.service": NAMED_BY_BODY,
    "event.payload.claim.actor.project": NAMED_BY_BODY,
    "event.payload.claim.actor.proven": NAMED_BY_BODY,
    "event.payload.previous_claim.actor.service": PERSON_CALLER,
  },
  "issue-updated-no-field-moved.json": {
    "event.actor.service": PERSON_CALLER,
    "event.payload.references_changed": "the write moved no issue's edges",
    "event.payload.thread_logins": "the write moved neither the status nor the assignee",
  },
  "issue-released-closed.json": {
    "envelope.source_session": PERSON_WROTE,
    "event.payload.previous_claim.actor.service": PERSON_CALLER,
  },
  "issue-updated-null-fields.json": {
    "envelope.source_session": PERSON_WROTE,
    "event.payload.references_changed": "the write moved no issue's edges",
  },
  "issue-claimed-first.json": {
    "event.actor.origin": NO_PLACE,
    "event.actor.service": PERSON_ONLY_CALLER,
    "event.actor.project": PERSON_ONLY_CALLER,
    "event.payload.claim.actor.origin": NO_PLACE,
    "event.payload.claim.actor.service": PERSON_ONLY_CALLER,
    "event.payload.claim.actor.project": PERSON_ONLY_CALLER,
    "event.payload.previous_claim": "a first claim replaces no claim",
  },
  "issue-released-null-claim.json": {
    "event.actor.origin": NO_PLACE,
    "event.actor.service": PERSON_ONLY_CALLER,
    "event.actor.project": PERSON_ONLY_CALLER,
    "event.payload.previous_claim.actor.origin": NO_PLACE,
    "event.payload.previous_claim.actor.service": PERSON_ONLY_CALLER,
    "event.payload.previous_claim.actor.project": PERSON_ONLY_CALLER,
  },
  "issue-claimed-agent-token.json": {
    "event.actor.owner": SHARED_TOKEN,
    "event.actor.service": SHARED_TOKEN,
    "event.actor.project": SHARED_TOKEN,
    "event.actor.proven": SHARED_TOKEN,
    "event.payload.claim.actor.owner": SHARED_TOKEN,
    "event.payload.claim.actor.service": SHARED_TOKEN,
    "event.payload.claim.actor.project": SHARED_TOKEN,
    "event.payload.claim.actor.proven": SHARED_TOKEN,
    "event.payload.previous_claim": "a claim of an issue nobody holds replaces no claim",
  },
};

const OTHER_SOURCE = "another producer's envelope: Dispatch's outbox writes source dispatch alone";
const ISSUE_TARGETS = "an issue write names issue targets alone (api.issueEdgeChanges)";

/**
 * Each value of a closed set a schema declares that no example holds, by the place it is read at
 * (`<object>.<key>`) and the value's JSON, with the reason. Every other value of every such set -
 * an enum, a literal, a union of them, a boolean, a union member no example takes - must be held
 * by an example: so a schema that drops a value Dispatch writes fails to read the example holding
 * it, and one that gains a value fails here until an example holds it or this names it.
 */
const UNHELD: Record<string, Record<string, string>> = {
  "envelope.source": {
    '"agent"': OTHER_SOURCE,
    '"human"': OTHER_SOURCE,
    '"envoy"': OTHER_SOURCE,
    '"github"': OTHER_SOURCE,
    '"slack"': OTHER_SOURCE,
    '"whatsapp"': OTHER_SOURCE,
    '"ghostwispr"': OTHER_SOURCE,
  },
  "event.quiet": {
    true: "events.Quiet holds none of these events quiet: it names delivery receipts, anchor refreshes, settings, targeted messages, quiet ask edits and unnamed versions",
  },
  "system actor.kind": {
    '"system"':
      "no server-owned writer appends an issue.updated, issue.claimed or issue.released: the system actors (the architecture importer, document settlement, the delivery timeline and attribution) write other events",
  },
  "changed reference.kind": {
    '"ask"': ISSUE_TARGETS,
    '"artifact"': ISSUE_TARGETS,
    '"block"': ISSUE_TARGETS,
  },
  "issue.updated payload.priority": {
    "0": "two issue.updated examples hold a priority and one holds none; dispatch-api.test.ts reads each of the four",
    "3": "two issue.updated examples hold a priority and one holds none; dispatch-api.test.ts reads each of the four",
  },
};

const OLDER = (what: string) =>
  `the written schema reads an event retained from a Dispatch older than ${what}, which carries none`;
const OMITTED = (field: string) =>
  `Go writes it only when it holds something (${field}, omitempty), and no example needs it empty`;
/** A frame a reader takes, by the schema that reads it tolerantly, with the type whose writes the
 *  strict schema beside it holds the examples to. */
const TOLERANT = (reader: string, written: string) =>
  `${reader} is a reader's tolerant reading, which takes a frame lacking it so a valid header is not discarded; ${written}, which Go writes on every event, is held to the examples`;
const OWNERLESS =
  "Go's model.Event.IssueKey is a pointer, null on an event no issue owns (a priority's write, DispatchEvent's priority.* members); an issue event names its issue";
const DOCUMENTLESS =
  "Go's model.Event.ArtifactID is a pointer, a string on an event an unlinked document owns (api.documentOwner); an issue owns these events (api.issueOwner), so each holds null";
/** A string or a whole number whose schema states no length or sign: the reason it takes "", or
 *  zero and a negative number, which no example's write holds. Where the daemon's decoder refuses
 *  that form at the same place, DAEMON says so too. */
const NO_LENGTH =
  "the schema states no length for the string, as Go's string has none, so it takes \"\", which no example's write holds";
const NO_SIGN =
  "the schema states a whole number with no sign, as Go's int has none, so it takes zero and a negative one, which no example's write holds";
const UNTAKEN_SYSTEM =
  "no example takes the system actor (UNHELD's reason), so the walk holds it with a value built from its own schema, which no example holds";
const P0 =
  "0 is P0, a value of the closed set no example holds (UNHELD's reason for issue.updated payload.priority)";

/**
 * Each form that a schema the examples are read through takes and no example holds, by the place
 * (`<object>.<key>`, `<place>[]` for a list's element, `<place>.*` for a map's value, the object's
 * name alone for the value a walk starts at) and the form (`absent`, `null`, a string, an empty
 * string, a whole number above zero, zero, a negative one, a fraction, one past the exact range, a
 * boolean, an object, a list, or `any` value), with the reason the schema takes it. Which forms a
 * schema takes is asked of zod's parse of each probe, never read from its definition. Every other
 * form such a schema takes - the envelope's, the event's, a payload's and a tolerant reader's, at
 * every depth and at each walk's start - must be one an example holds: so a key made optional or
 * nullable where Go always writes it fails here, and so does a value given another type, a
 * list's element made nullable, a refinement dropped or a fallback added, whether it is written
 * as a union, a coercion or a preprocess.
 */
const WIDER: Wider = [
  {
    "event.artifact_id": { string: DOCUMENTLESS },
    "event.payload.*": {
      any: "PublishedIssueEventSchema reads the payload as PublishedEventSchema does, a map of any value; the schema of the event's type reads it (PAYLOADS), and the walk holds that schema's forms",
    },
    "frame.issue_key": { null: OWNERLESS },
    "frame.artifact_id": {
      absent: TOLERANT(
        "DispatchEventSchema",
        "the event's artifact_id (PublishedIssueEventSchema)"
      ),
      string: DOCUMENTLESS,
    },
    "frame.project": {
      absent: TOLERANT("DispatchEventSchema", "the event's project (PublishedIssueEventSchema)"),
    },
    "frame.actor.id": {
      absent: TOLERANT("DispatchEventSchema", "the actor's id (ActorSchema, model.Actor.ID)"),
    },
    "issue.updated payload.advances": {
      absent: OLDER("priorities"),
      null: "Go writes a nil list as null (model.Issue.Advances has no omitempty); an issue read fills it, so these events hold []",
    },
    "issue.updated payload.advances_revision": { absent: OLDER("priorities") },
    "issue.updated payload.verification_artifact_id": { absent: OLDER("document roles") },
    "issue.updated payload.plan_artifact_id": { absent: OLDER("document roles") },
    "issue.updated payload.decisions_artifact_id": {
      absent: OLDER("the design gate's steps"),
      string:
        "Go writes the id of the issue's decisions document where it has one (model.Issue.DecisionsArtifactID); no example's issue has one, so each holds \"\"",
    },
    "issue.updated payload.changed": { absent: OLDER("the changed list") },
    "issue.updated payload.ancestors": { absent: OLDER("the parent chain") },
    "issue.updated payload.created_by": {
      null: "Go's model.Issue.CreatedBy is a pointer, which the API document declares nullable (the spellings test pairs the two); every issue holds its creator since migration 0001 (the Go test's neverEmpty)",
    },
    "issue.updated payload.priority": { zero: P0 },
    ...Object.fromEntries(
      ["title", "labels", "status", "priority", "rank", "route"].map((key) => [
        `issue.updated payload read.${key}`,
        {
          absent: TOLERANT(
            "IssueEventPayloadSchema",
            `the payload's ${key} (WrittenIssueEventPayloadSchema)`
          ),
        },
      ])
    ),
    // The reader reads these two with the written payload's own schema, which takes them absent
    // for the reason the written payload's entries above give.
    "issue.updated payload read.changed": { absent: OLDER("the changed list") },
    "issue.updated payload read.ancestors": { absent: OLDER("the parent chain") },
    "issue.released payload.repo": {
      string:
        "Go writes the issue's own repo (model.IssueClaimEventPayload.Repo, model.Issue.Repo): a string where the issue names one, null where it names none; neither release example's issue names one, and two claim examples hold a string",
    },
    "issue.released payload.claim": {
      object:
        "the release payload reads claim as the issue's own field (issueFields.claim), the type the API document declares for an issue and the spellings test holds it to; a release clears the claim, so Go writes null (api.claimEvent with the issue after the release)",
    },
    "user actor.proven": {
      absent:
        "an actor stored before the proven mark carries none (model.Actor.Proven), and a payload carries stored actors: an older issue's created_by, a claim taken before the mark",
    },
    "system actor.kind": { string: UNTAKEN_SYSTEM },
    "system actor.id": { string: UNTAKEN_SYSTEM },
    "origin.host": { absent: OMITTED("model.ActorOrigin.Host") },
    "origin.machine": { absent: OMITTED("model.ActorOrigin.Machine") },
    "origin.cwd": { absent: OMITTED("model.ActorOrigin.Cwd") },
    "external link.kind": { absent: OMITTED("model.ExternalLink.Kind") },
  },
  // The reader's priority, whose entry above is its tolerance alone.
  { "issue.updated payload read.priority": { zero: P0 } },
  // Every plain string of these schemas: none states a length.
  ...[
    "changed reference.id",
    "claim.at",
    "components.ids[]",
    "components.inherited_from",
    "components.reason",
    "components.unknown[]",
    "envelope.source_session",
    "event.artifact_id",
    "event.project",
    "external link.kind",
    "external link.url",
    "frame.actor.id",
    "frame.actor.kind",
    "frame.artifact_id",
    "frame.issue_key",
    "frame.project",
    "frame.type",
    "issue.claimed payload.repo",
    "issue.claimed payload.resolved_repo",
    "issue.claimed payload.status",
    "issue.released payload.repo",
    "issue.released payload.resolved_repo",
    "issue.released payload.status",
    "issue.updated payload read.ancestors[]",
    "issue.updated payload read.changed[]",
    "issue.updated payload read.labels[]",
    "issue.updated payload read.rank",
    "issue.updated payload read.route",
    "issue.updated payload read.status",
    "issue.updated payload read.thread_logins[]",
    "issue.updated payload read.title",
    "issue.updated payload.advances[].id",
    "issue.updated payload.ancestors[]",
    "issue.updated payload.assignee",
    "issue.updated payload.blocked_by[]",
    "issue.updated payload.changed[]",
    "issue.updated payload.closed_at",
    "issue.updated payload.created_at",
    "issue.updated payload.labels[]",
    "issue.updated payload.primary_artifact_id",
    "issue.updated payload.project",
    "issue.updated payload.rank",
    "issue.updated payload.repo",
    "issue.updated payload.resolved_repo",
    "issue.updated payload.route",
    "issue.updated payload.status",
    "issue.updated payload.thread_logins[]",
    "issue.updated payload.title",
    "issue.updated payload.updated_at",
    "origin.cwd",
    "origin.host",
    "origin.machine",
    "origin.pane",
    "origin.session_title",
    "origin.tmux",
    "session actor.owner",
    "session actor.project",
    "session actor.service",
    "system actor.id",
    "user actor.id",
  ].map((place) => ({ [place]: { "empty string": NO_LENGTH } })),
  // Every whole number of these schemas: none states a sign but the event's id and seq and the
  // envelope's time, which the event log and every producer keep above 0, and the revision, which
  // no example holds below one.
  ...["issue.updated payload.number", "issue.updated payload.last_seq"].map((place) => ({
    [place]: { zero: NO_SIGN, negative: NO_SIGN },
  })),
  { "issue.updated payload.advances_revision": { negative: NO_SIGN } },
].reduce<Record<string, Partial<Record<Form, string>>>>((wider, table) => {
  for (const [place, forms] of Object.entries(table)) wider[place] = { ...wider[place], ...forms };
  return wider;
}, {});

/** Why the daemon's decoder takes a form a schema refuses, at the event's actor and the issue
 *  payload's fields it reads into Go strings, lists and structs. */
const GO_ZERO =
  "encoding/json reads a null, or a key the event leaves out, into the daemon's Go string, list, struct or time as its zero value, and the daemon judges no such value there; Dispatch writes the field on every event, never null, and the schema refuses both";
const OLDER_CLAIM =
  "the daemon reads an issue event with no claim as one retained from a Dispatch older than claims, which says nothing of the claim (dispatchIssuePayload.Claim, issueEventClaim); Dispatch writes the claim on every issue event";

/**
 * Each form the Legion daemon's decoder takes where a producer's schema refuses it, at a place an
 * example holds, by the place as WIDER names it (`<place> @ <path>` for one path of a place), then
 * the form, with the reason: rule 2 of `daemonAgreement`. Rule 1 has no table: a producer's schema
 * (`IssueEventEnvelopeSchema`, `PublishedIssueEventSchema`, each payload's) never takes a form
 * the decoder refuses (daemon-verdicts.json, which the daemon's
 * TestTheDaemonDecoderTakesWhatItsVerdictsSay holds to its decoder), so a refinement dropped, a
 * coercion or a fallback fails by its place whatever its spelling, and so does a decoder that
 * starts refusing a form a schema takes. Each entry here is a rule stricter than the decoder that
 * every producer keeps: the decoder reads a Go zero value (a null, a key the event leaves out) the
 * schema refuses (`GO_ZERO`), or reads a value an older Dispatch left out. A place where the
 * decoder takes every form is one the daemon reads nothing of (its own walk test holds which it
 * reads), and is held by WIDER alone.
 */
const DAEMON: Wider = {
  "envelope.issued_at": {
    negative:
      "the daemon refuses only an issued_at of 0 (envoyEnvelope.valid); every producer stamps a time after 1970, which the schema holds above 0",
  },
  "event.created_at": { absent: GO_ZERO, null: GO_ZERO },
  "event.actor": { absent: GO_ZERO, null: GO_ZERO },
  "frame.actor": { absent: GO_ZERO, null: GO_ZERO },
  "frame.actor.kind": { absent: GO_ZERO, null: GO_ZERO },
  "frame.actor.id": { null: GO_ZERO },
  "session actor.kind": { absent: GO_ZERO, null: GO_ZERO, "empty string": GO_ZERO },
  "session actor.id": {
    absent: GO_ZERO,
    null: GO_ZERO,
    "empty string":
      "the daemon reads the event's actor only to ask whether it is a session other than its own (decodeDispatchEvent), so it takes an empty id; every session actor Dispatch records names its session (bearerSessionActor refuses an empty one), which the schema holds",
  },
  "user actor.kind": { absent: GO_ZERO, null: GO_ZERO, "empty string": GO_ZERO },
  "user actor.id": { absent: GO_ZERO, null: GO_ZERO },
  "issue.updated payload.parent": { absent: GO_ZERO },
  "issue.updated payload.resolved_repo": {
    absent:
      "the daemon reads an event with no resolved_repo as one retained from a Dispatch older than it, and decodes it with its repository missing (decodeDispatchEvent); Dispatch writes it on every issue event",
  },
  "issue.updated payload.claim": { absent: OLDER_CLAIM },
  "issue.claimed payload.claim": { absent: OLDER_CLAIM },
  "issue.released payload.claim": { absent: OLDER_CLAIM },
  ...Object.fromEntries(
    ["title", "status", "rank", "labels", "blocked_by"].map((key) => [
      `issue.updated payload.${key}`,
      { absent: GO_ZERO, null: GO_ZERO },
    ])
  ),
  ...Object.fromEntries(
    ["title", "status", "rank", "labels"].map((key) => [
      `issue.updated payload read.${key}`,
      { null: GO_ZERO },
    ])
  ),
  "issue.updated payload.labels[]": { null: GO_ZERO },
  "issue.updated payload read.labels[]": { null: GO_ZERO },
  "issue.updated payload.blocked_by[]": { null: GO_ZERO },
};

/**
 * Each form a reader's schema - the frame's tolerant `DispatchEventSchema` and the tolerant
 * `IssueEventPayloadSchema`, which read every event a reader of the bus takes, not only the
 * daemon's issue events - takes at a place an example holds where the daemon's decoder refuses
 * it, with the reason the reader takes it: rule 2 of `daemonAgreement`, for readers.
 */
const READER_WIDER: Wider = {
  "frame.type": {
    "empty string":
      "the frame's reader takes any event's header so a valid frame is not discarded; the daemon refuses an issue event with no type, which PublishedIssueEventSchema refuses too",
  },
  "frame.issue_key": {
    null: OWNERLESS,
    "empty string":
      "the frame's reader takes any event's header so a valid frame is not discarded; the daemon refuses an issue event with no issue key, which PublishedIssueEventSchema refuses too",
  },
};

/**
 * Each place a schema the examples are read through reads a closed set of values at - an enum, a
 * literal, a union of them - by the place (`<object>.<key>`, as WIDER names it). Go writes each
 * of these from a closed set of its own, so each must read its set alone and refuse a value
 * outside it (the walk's `closedSets`): so a set opened to any string, or joined with an open
 * type, fails here by its place, and so does a new closed set until this names it. Which values
 * of each set the examples hold is UNHELD's.
 */
const CLOSED: readonly string[] = [
  "envelope.source",
  "user actor.kind",
  "user actor.proven",
  "session actor.kind",
  "session actor.proven",
  "system actor.kind",
  "issue.updated payload.priority",
  "issue.updated payload read.priority",
  "components.mode",
  "changed reference.kind",
  "issue.claimed payload.reason",
  "issue.released payload.reason",
];

/** The name each object of the schemas is known by in NEVER, UNHELD, WIDER and the problems; an
 *  object not named here (an `advances` entry) is named by where it is. Each name is one schema's:
 *  a reader's schema has a name of its own, so a WIDER reason for what a tolerant reader takes
 *  never excuses the same form in the written schema (the walk refuses one name for two). */
const LABELS = new Map<unknown, string>([
  [objectSchema(IssueEventEnvelopeSchema, "IssueEventEnvelopeSchema"), "envelope"],
  [objectSchema(PublishedIssueEventSchema, "PublishedIssueEventSchema"), "event"],
  [objectSchema(DispatchEventSchema, "DispatchEventSchema"), "frame"],
  [
    objectSchema(WrittenIssueEventPayloadSchema, "WrittenIssueEventPayloadSchema"),
    "issue.updated payload",
  ],
  [objectSchema(IssueEventPayloadSchema, "IssueEventPayloadSchema"), "issue.updated payload read"],
  [
    objectSchema(IssueClaimEventPayloadSchema, "IssueClaimEventPayloadSchema"),
    "issue.claimed payload",
  ],
  [
    objectSchema(IssueReleasedEventPayloadSchema, "IssueReleasedEventPayloadSchema"),
    "issue.released payload",
  ],
  [objectSchema(IssueClaimSchema, "IssueClaimSchema"), "claim"],
  [objectSchema(IssueComponentsSchema, "IssueComponentsSchema"), "components"],
  [objectSchema(ExternalLinkSchema, "ExternalLinkSchema"), "external link"],
  [objectSchema(ActorOriginSchema, "ActorOriginSchema"), "origin"],
  [objectSchema(ActorActSchema, "ActorActSchema"), "act"],
  [objectSchema(ChangedReferenceSchema, "ChangedReferenceSchema"), "changed reference"],
  ...ActorSchema.options.map((option): [unknown, string] => {
    const kind = objectShape(option, "ActorSchema")?.kind;
    const values = kind instanceof z.core.$ZodLiteral ? kind._zod.def.values : [];
    return [objectSchema(option, "ActorSchema"), `${String(values[0])} actor`];
  }),
]);

/** The value at a path of zod's issues in `value`, as JSON, for a problem to show. */
function heldAt(value: unknown, path: readonly PropertyKey[]): string {
  let held: unknown = value;
  for (const step of path) {
    held =
      held !== null && typeof held === "object"
        ? (held as Record<PropertyKey, unknown>)[step]
        : undefined;
  }
  return held === undefined ? "nothing" : JSON.stringify(held);
}

/** What `schema` makes of an example's value, or undefined and a problem for each place it
 *  refuses, naming what the example holds there: a schema that drops a value an example holds
 *  names it. */
function parsed(
  schema: z.core.$ZodType,
  value: unknown,
  what: string,
  problems: string[]
): unknown {
  const result = z.safeParse(schema, value);
  if (result.success) return result.data;
  for (const issue of result.error.issues) {
    problems.push(
      `${what} is refused at ${issue.path.map(String).join(".") || "its top"}, which holds ${heldAt(value, issue.path)}: ${issue.message}`
    );
  }
  return undefined;
}

test("the issue events Dispatch publishes are envelopes, events and payloads these schemas read, every key, value and form declared", async () => {
  const walk = new ExampleWalk({
    never: NEVER,
    labels: LABELS,
    absentTable: "ABSENT",
    loose: LOOSE,
  });
  const examples: WalkedExample[] = EXAMPLES.map((file) => ({ name: file, absent: ABSENT[file] }));
  // The walk holds each key of every schema an example is read through to the forms the examples
  // hold (WIDER): one that takes a key absent or null where Go always writes it fails, the
  // envelope's, the event's and a tolerant reader's as well as a payload's.
  const strict = { tolerant: false };
  const reader = { tolerant: true };
  for (const example of examples) {
    const file = example.name;
    const path = new URL(`../../daemon/internal/intake/testdata/dispatch/${file}`, import.meta.url);
    const envelope = JSON.parse(await Bun.file(path).text());
    // The envelope schema keeps only what it declares, so an undeclared key is dropped here.
    const readEnvelope = parsed(
      IssueEventEnvelopeSchema,
      envelope,
      `${file}'s envelope`,
      walk.problems
    );
    if (readEnvelope !== undefined) expect(readEnvelope).toEqual(envelope);
    const event = JSON.parse(envelope.payload);
    const readEvent = parsed(PublishedIssueEventSchema, event, `${file}'s event`, walk.problems);
    if (readEvent !== undefined) expect(readEvent).toEqual(event);
    // A reader of the bus takes the frame by its tolerant schema, the payload by its type's own.
    const frame = parsed(DispatchEventSchema, event, `${file}'s frame`, walk.problems);
    if (frame !== undefined) {
      expect(frame).toMatchObject({ issue_key: event.issue_key, type: event.type });
    }
    const payload = PAYLOADS[event.type];
    if (payload === undefined)
      throw new Error(`${file} is a ${event.type}, which PAYLOADS names no schema for`);
    const written = payload.written as z.core.$ZodType;
    const readPayload = parsed(written, event.payload, `${file}'s payload`, walk.problems);
    if (readPayload !== undefined) expect(readPayload).toEqual(event.payload);
    walk.walk(IssueEventEnvelopeSchema, envelope, example, "envelope", "envelope", strict);
    walk.walk(PublishedIssueEventSchema, event, example, "event", "event", strict);
    walk.walk(DispatchEventSchema, event, example, "event", "frame", reader);
    walk.walk(written, event.payload, example, "event.payload", `${event.type} payload`, strict);
    if (payload.read !== undefined) {
      // The reading schema keeps each key it declares, as Dispatch wrote it, and each key it
      // declares is one the example holds, unless NEVER or ABSENT names it: so a reader of a key
      // Dispatch does not write fails here.
      const shape = objectShape(payload.read, "IssueEventPayloadSchema") ?? {};
      const kept = Object.fromEntries(
        Object.entries(event.payload).filter(([key]) => key in shape)
      );
      expect(payload.read.parse(event.payload)).toEqual(kept);
      walk.walk(
        payload.read,
        event.payload,
        example,
        "event.payload",
        `${event.type} payload`,
        reader
      );
    }
  }
  walk.staleExceptions(examples);
  walk.unheldValues(UNHELD);
  walk.closedSets(CLOSED);
  walk.widenings(WIDER);
  // Each schema that reads a place an example holds takes there the forms the daemon's decoder
  // takes, and no other, unless DAEMON names the difference (daemon-verdicts.json, which the
  // daemon's TestTheDaemonDecoderTakesWhatItsVerdictsSay holds to its decoder).
  const verdicts = new URL(
    "../../daemon/internal/intake/testdata/dispatch/daemon-verdicts.json",
    import.meta.url
  );
  walk.daemonAgreement(JSON.parse(await Bun.file(verdicts).text()), DAEMON, READER_WIDER);
  expect(walk.problems).toEqual([]);
});
