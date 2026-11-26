import { z } from "zod";
import type { ASK_URGENCIES } from "./dispatch-tools";
import { GATED_STEPS } from "./legion-roles";

/**
 * Who writes, what a write points at, and the records a project is made of: the shapes every
 * other area of the Dispatch API is built from.
 */

/**
 * A stored text read as the ProseMirror document a reader is shown (`pmdoc.ReadForDisplay`),
 * carried beside the markdown itself so a page renders what the server read rather than parsing
 * the markdown again. A response carries one only for a request that sent `Dispatch-Trees: 1`,
 * and every `<field>_tree` is absent otherwise; a `/ws/live` event frame carries its trees
 * always, since a browser can set no header on a socket.
 */
export type DisplayTree = {
  readonly type: string;
  readonly [key: string]: unknown;
};

/** `DisplayTree` for a schema that must hold one: a ProseMirror node, read as whatever its own
 *  node type names, so nothing the server's schema grows has to be re-declared here. */
export const DisplayTreeSchema: z.ZodType<DisplayTree> = z.looseObject({ type: z.string() });

export type AskUrgency = (typeof ASK_URGENCIES)[number];

export const ActorOriginSchema = z
  .strictObject({
    host: z.string().optional(),
    machine: z.string().optional(),
    cwd: z.string().optional(),
    tmux: z.string().optional(),
    pane: z.string().optional(),
    session_title: z.string().optional(),
  })
  .readonly();

export type ActorOrigin = z.output<typeof ActorOriginSchema>;

/** The OAuth scopes a Dispatch credential can hold, mirroring Go's `model.ScopePerson`,
 *  `model.ScopeApprove` and `model.ScopeP0` and the `agent_tokens.scopes` check that stores them.
 *  `person` acts as the credential's own person on every route a person reaches through the
 *  dashboard. `approve` settles what a person settles: a document review, a decision's Keep, the
 *  answer of an approval ask, a machine-login decision, a credential-grant, machine-login or
 *  sign-in revoke, and minting, changing or revoking a personal token. `p0` lets a credential make
 *  an issue's own priority 0 (`POST /api/v1/issues` and `PATCH /api/v1/issues/{key}`). A browser
 *  sign-in holds all three. A personal token holds `p0` only where its maker gave it, and may hold
 *  it alone: no mint gives `p0` by default. A credential holding `approve` holds `person` too, and
 *  no broker-issued token, shared token or service-account token holds `approve` or `p0`. */
export const DISPATCH_SCOPES = ["person", "approve", "p0"] as const;
export type DispatchScope = (typeof DISPATCH_SCOPES)[number];

/** The scope words a personal token can carry, which the agent token list names and `scopes` in
 *  `GET /api/v1/whoami` names for it: the three of `DISPATCH_SCOPES` and `read`. A token that
 *  holds `read` is read-only (`DISPATCH_READ_ONLY_TOKEN`): a personal token holds it alone or
 *  beside `person`, never beside `approve` or `p0`. No mint gives `p0` by default. `read` is a
 *  mark on the token and not a scope a credential acts under, so no act records it, and
 *  `DISPATCH_SCOPES` and `ActorActSchema` keep `person`, `approve` and `p0` alone.
 *  `GET /api/v1/whoami` also names `legion-status` for a Legion daemon's own
 *  broker token, which holds it alone (`WhoamiResponse`): it is that token's leave to write a
 *  Legion issue's status where Dispatch runs with `DISPATCH_LEGION_STATUS_SCOPE` on, so no act
 *  records it either, and no personal token can hold it. */
export const DISPATCH_TOKEN_SCOPES = ["person", "approve", "p0", "read"] as const;
export type DispatchTokenScope = (typeof DISPATCH_TOKEN_SCOPES)[number];

/** The agent that made an act for the person it acts for: the session a broker-issued token
 *  proved, and the personal token presented, by id and name. It is absent wherever the person
 *  acted themselves, which is every act made at a browser.
 *
 *  `scopes` is the set of `DISPATCH_SCOPES` (`person`, `approve`, `p0`) that credential held at
 *  the act (a Legion daemon's `legion-status` and `read` are never recorded), recorded with it and
 *  never read back from the token. A person who narrows a token afterwards narrows what it may do
 *  next, and does not undo what it did while it held `approve` or `p0`. `p0` lets a credential
 *  make an issue's own priority 0, and no mint gives it by default.
 *
 *  `session_proven` is `true` only when the credential itself proved `session` (a broker-issued
 *  token's own session); a session a personal token's request named is recorded without it. */
export const ActorActSchema = z
  .strictObject({
    session: z.string().optional(),
    session_proven: z.literal(true).optional(),
    token_id: z.string().optional(),
    token_name: z.string().optional(),
    scopes: z.array(z.enum(DISPATCH_SCOPES)).optional(),
  })
  .readonly();

/** `proven` is `true` only when the write's credential itself proved the actor's `id`: a
 *  browser's person, a person a credential acts for, or a broker-issued token's session. The
 *  server sets it and never reads it from a body; an actor whose `id` the request only named (the
 *  shared agent token, a personal token or a service token naming a session) and every row
 *  written before the mark carry none, so a reader that trusts only proven writers trusts no
 *  claimed one. */
export const ActorSchema = z.union([
  z
    .strictObject({
      kind: z.literal("user"),
      id: z.string(),
      act: ActorActSchema.optional(),
      proven: z.literal(true).optional(),
    })
    .readonly(),
  z
    .strictObject({
      kind: z.literal("session"),
      id: z.string(),
      origin: ActorOriginSchema.optional(),
      owner: z.string().optional(),
      /** The subject of the verified service token that authenticated the write
       *  (`system:serviceaccount:<namespace>:<name>`); absent under any other bearer. */
      service: z.string().optional(),
      /** The one project key the write's credential may write. The server reads it from an
       *  access token the secrets broker issued for that project and never from a body, as it
       *  reads `service` from a service token; it is absent under every credential that names
       *  none, a browser cookie, a personal token and a broker token scoped to its person
       *  included. */
      project: z.string().optional(),
      proven: z.literal(true).optional(),
    })
    .readonly(),
  /** A server-owned writer, such as the architecture importer
   *  (`{kind: "system", id: "architecture-importer"}`). */
  z.strictObject({ kind: z.literal("system"), id: z.string() }).readonly(),
]);

/**
 * The actor and the act exactly as Dispatch writes them: what `ActorSchema` and `ActorActSchema`
 * parse, which is what every `DispatchEvent` payload of this package carries.
 *
 * They are deliberately not the contract's `Actor` and `ActorAct`, which are the OpenAPI
 * document's reading and keep the plain names: that reading's `kind` is a `string` and its
 * optional fields do not carry `undefined`, so neither type is assignable to the other
 * (`dispatch-api-spellings.test.ts`). A renderer handed an actor from a payload names these, as
 * one handed an actor from a served row names `Actor`.
 */
export type WrittenActorAct = z.output<typeof ActorActSchema>;

export type WrittenActor = z.output<typeof ActorSchema>;

/**
 * An actor as a bus frame carries it, in the three readings the inbound parsers need.
 *
 * `kind` is an open string in each: JetStream retains a frame for 72 hours, so a reader may still
 * see an actor kind the server no longer writes, and a renderer that refuses the frame over it
 * drops an event it could have shown. `InboundActorSchema` is a comment's or a message's author,
 * whose extra fields no reader of these frames carries; `InboundWriterSchema` keeps the fields it
 * does not declare, as the payloads holding it do; `InboundBySchema` is the writer of a record
 * whose id a frame may omit.
 */
export const InboundActorSchema = z.object({ kind: z.string(), id: z.string() });

export const InboundWriterSchema = InboundActorSchema.passthrough();

export const InboundBySchema = z
  .object({ kind: z.string(), id: z.string().optional() })
  .passthrough();

/** How a verified service token's subject reads wherever an actor is shown.
 *
 *  A Kubernetes subject — exactly `system:serviceaccount:<namespace>:<name>` —
 *  renders `<namespace>/<name>`, because the namespace is the half that
 *  disambiguates: every namespace has a `default` service account, so the name
 *  alone names no one once a second namespace authenticates. Every other
 *  subject renders whole. Truncating a subject whose shape is unknown is the
 *  mistake this rule exists to undo, so the fallback shows all of it.
 *
 *  The persisted actor always carries the full subject; this is presentation. */
export function serviceSubjectLabel(subject: string): string {
  const parts = subject.split(":");
  if (
    parts.length === 4 &&
    parts[0] === "system" &&
    parts[1] === "serviceaccount" &&
    parts[2] !== "" &&
    parts[3] !== ""
  ) {
    return `${parts[2]}/${parts[3]}`;
  }
  return subject;
}

/** What a credential request's kind is, as the secrets broker names it: a secret an agent asked
 *  its person for, or a machine login a launcher asked to enrol. */
export type CredentialRequestKind = "agent_secret" | "launcher_credential";

export const anchorFields = {
  artifact_id: z.string(),
  /** Stable owner block for new anchors; null for legacy rows awaiting backfill. */
  block_id: z.string().nullable(),
  mark_id: z.string(),
  version: z.number().int(),
  quote: z.string(),
  orphaned: z.boolean(),
};

export const AnchorSchema = z.strictObject(anchorFields).readonly();

/** The exported type is this schema's reading rather than the generated alias, because the two
 *  are the same type and one name cannot come from both (`scripts/gen-api.ts`). */
export type Anchor = z.output<typeof AnchorSchema>;

/** One node on a block's path from the document's top-level block down to the block: its type,
 *  its block id ("" for a node the live document has not stamped yet) and its index among its
 *  parent's children. */
export const BlockPathEntrySchema = z
  .strictObject({ type: z.string(), id: z.string(), index: z.number().int() })
  .readonly();

export type BlockPathEntry = z.output<typeof BlockPathEntrySchema>;

/** Where a block stands in the table holding it. `row` is the row's index (0 is the header row)
 *  and `column` the cell's index in its row — the indexes `delete_row` and `delete_column` take.
 *  `row` is null for the table block itself; `column` and `header` are null for the table and for
 *  a row block. `header` is the text of the header cell drawn above the cell's column once colspans
 *  and rowspans take their places, "" where no header cell covers it. `cells` is each cell of the
 *  row as its opening words, by child index; null for the table block. */
export const TablePositionSchema = z
  .strictObject({
    id: z.string(),
    row: z.number().int().nullable(),
    column: z.number().int().nullable(),
    header: z.string().nullable(),
    cells: z.array(z.string()).readonly().nullable(),
  })
  .readonly();

/** Where a block stands in its document: `GET /api/v1/artifacts/{id}/blocks/{block_id}`'s answer,
 *  and a comment's or ask's `anchor_block`. `table` is present for a table block and for every
 *  block inside one. */
export const BlockPathSchema = z
  .strictObject({
    id: z.string(),
    type: z.string(),
    path: z.array(BlockPathEntrySchema).readonly(),
    table: TablePositionSchema.optional(),
  })
  .readonly();

/** Where meaning search stands on one block: every window of blocks covering it is embedded at
 *  the text it holds now, one of them is still waiting for its vector, or no window covers the
 *  block at all - a block the indexer leaves out of every window, and every block of an item
 *  whose windows are not written yet. It is read the same way on a document's block
 *  (`ArtifactBlock`) and on a comment's or a message's (`ItemBlock`), which is why it sits here
 *  rather than in either area. */
export type BlockEmbedding = "embedded" | "pending" | "none";

/** Where a comment's or an ask's anchor's block stands in the live document. Set on the
 *  single-record reads alone (`GET /api/v1/comments/{id}`, `GET /api/v1/asks/{id}`); lists and
 *  events never carry it. */
export const anchorPositionFields = {
  /** Absent when the anchor names no block, the block has left the document, the document could
   *  not be read (`anchor_block_error` then says why), or the server predates it. */
  anchor_block: BlockPathSchema.optional(),
  /** Why `anchor_block` is absent although the anchor names a block: the read could not read its
   *  document, named by the code the API answers that error with elsewhere. The read itself still
   *  answers. */
  anchor_block_error: z
    .enum(["DOC_SERVICE_UNAVAILABLE", "DOC_SCHEMA", "DOCUMENT_UNLOADABLE", "INTERNAL"])
    .optional(),
};

export const AnchorPositionSchema = z.strictObject(anchorPositionFields).readonly();

export type AnchorPosition = z.output<typeof AnchorPositionSchema>;

export const ExternalLinkSchema = z
  .strictObject({ url: z.string(), kind: z.string().optional() })
  .readonly();

export type ExternalLink = z.output<typeof ExternalLinkSchema>;

/** A design-gate step a Legion deployment gates, as Dispatch stores it: `decisions` names the
 *  root issue's decisions document, `criteria` its Verification document, `spec` its primary
 *  document. */
export type GatedStep = (typeof GATED_STEPS)[number];

/** `project.created` and `project.updated` carry the project row itself. */
export const ProjectSchema = z
  .strictObject({
    key: z.string(),
    name: z.string(),
    open_asks: z.number().int().optional(),
    /** The design-gate steps the Legion deployment that serves this project gates, in the order
     *  it gates them, as its daemon last published them. The empty list is a deployment that
     *  gates nothing and one that has not published yet alike: neither holds a step waiting for
     *  a person. */
    gated_steps: z.array(z.enum(GATED_STEPS)).optional(),
    /** The project's default repository, `owner/name`: an issue that names no repository, and
     *  whose parent chain names none, resolves to it.
     *  `PUT /api/v1/projects/{key}/default-repository` sets and clears it. Absent when the
     *  project has none, and from a Dispatch older than the field. */
    default_repo: z.string().optional(),
    /** Whether the project needs a credential that proves the session: when it is on, an
     *  architect's write of an issue's spec or Verification document passes only on a credential
     *  that proves the architect's session, and a role-holder publication only on one that proves
     *  `legion-daemon:<project>`. Off by default. Dispatch always answers it. */
    proven_sessions: z.boolean(),
    created_at: z.string(),
  })
  .readonly();

/** A row of `GET /api/v1/settings/repo-projects`, the one route that still reads the repository
 *  mapping. Nothing writes the rows now: they stay as the migration of `Project.default_repo`
 *  left them, for the Legion daemons that read the route at boot, and the next release removes
 *  the route. The retained event `settings.repo_project.updated` carries one as `mapping`. */
export const RepoProjectSchema = z
  .strictObject({
    repo: z.string(),
    project: z.string(),
    /** Whether the row was its project's default repository. The migration copied each default
     *  to `Project.default_repo`, which is what an issue resolves to now. */
    default: z.boolean(),
    /** The full `owner/name` this repository is called today, null for one that was never
     *  renamed. Dispatch reads each pair once at boot: it writes the new name in place of the old
     *  one in every stored repository field, and compares a repository under the new name.
     *  Optional here because a client written before the field existed builds this shape
     *  without it; Dispatch always answers it. */
    renamed_to: z.string().nullable().optional(),
    created_by: ActorSchema,
    created_at: z.string(),
  })
  .readonly();

/**
 * One repository a project's architecture documents are imported from (directory
 * fixed at .dispatch/architecture/), on its branch. A project may have several
 * sources, one per repository, and its component model is every source's components
 * together. The GitHub App installation id backing the access check is a server
 * detail and never appears here. The sync fields stay null until the importer runs;
 * `components` is how many components this source currently projects.
 */
export const ArchitectureSourceSchema = z
  .strictObject({
    project: z.string(),
    repo: z.string(),
    branch: z.string(),
    enabled: z.boolean(),
    created_by: ActorSchema,
    created_at: z.string(),
    last_sync_at: z.string().nullable(),
    last_commit: z.string().nullable(),
    last_error: z.string().nullable(),
    components: z.number().int(),
  })
  .readonly();

/** `settings.repo_project.updated`: a write of the repository mapping, which no build makes
 *  now. The events an earlier build stored stay as history, so the type stays for them.
 *  `deleted` is true for a write that removed the mapping, and false for one that set it. */
export const RepoProjectUpdatedEventPayloadSchema = z
  .strictObject({ mapping: RepoProjectSchema, deleted: z.boolean() })
  .readonly();

export type RepoProjectUpdatedEventPayload = z.output<typeof RepoProjectUpdatedEventPayloadSchema>;

/** `settings.project_default_assignee.updated`: a person moved a project's default assignee to
 *  another person. The payload is the stored row - no write clears a default, so `assignee`
 *  names somebody - plus `thread_logins`, the previous default and the new one, sorted and each
 *  named once. A project's default is the last step of the chain that decides who a question
 *  reaches, so the move carries every question on an issue nobody holds between those two
 *  Inboxes, and a reader refreshes the lists of both. An event retained from before every
 *  project named a default can record a clear, `assignee: null`, and names no `thread_logins`. */
export const ProjectDefaultAssigneeUpdatedEventPayloadSchema = z
  .strictObject({
    project: z.string(),
    assignee: z.string().nullable(),
    thread_logins: z.array(z.string()).optional(),
  })
  .readonly();

export type ProjectDefaultAssigneeUpdatedEventPayload = z.output<
  typeof ProjectDefaultAssigneeUpdatedEventPayloadSchema
>;

export const ArchitectureSourceUpdatedEventPayloadSchema = z
  .strictObject({ source: ArchitectureSourceSchema, deleted: z.boolean() })
  .readonly();

export type ArchitectureSourceUpdatedEventPayload = z.output<
  typeof ArchitectureSourceUpdatedEventPayloadSchema
>;

/** `architecture.synced`: the importer replaced one source's part of the project's model.
 *  `repo` and `branch` name that source; events retained from before projects had several
 *  sources lack them. */
export const ArchitectureSyncedEventPayloadSchema = z
  .strictObject({
    repo: z.string().optional(),
    branch: z.string().optional(),
    commit: z.string(),
    components: z.number().int(),
  })
  .readonly();

export type ArchitectureSyncedEventPayload = z.output<typeof ArchitectureSyncedEventPayloadSchema>;

/** `architecture.sync_failed`: one source's import was rejected or failed; that source's
 *  previous components stay up and its row carries the same text in `last_error`. `repo` and
 *  `branch` name that source; events retained from before projects had several sources lack
 *  them. */
export const ArchitectureSyncFailedEventPayloadSchema = z
  .strictObject({
    repo: z.string().optional(),
    branch: z.string().optional(),
    error: z.string(),
  })
  .readonly();

export type ArchitectureSyncFailedEventPayload = z.output<
  typeof ArchitectureSyncFailedEventPayloadSchema
>;

/** `priority.updated`: one write of one priority committed - a create, a PATCH of its title,
 *  description, owner or place in the order, or a delete, which writes the state gone. It names
 *  the priority and which of the three writes it was, and carries no row: a reader refetches its
 *  priorities reads on every write, and on a delete also the work under that priority, the work
 *  under none and every issue read or listing whose rows advance it.
 *  A priority belongs to no project, so the event names no owner (`issue_key` null, `project`
 *  empty), wakes nobody (`notify` false) and the outbox publishes it on no subject. */
export const PriorityUpdatedEventPayloadSchema = z
  .strictObject({ id: z.string(), change: z.enum(["created", "updated", "deleted"]) })
  .readonly();

export type PriorityUpdatedEventPayload = z.output<typeof PriorityUpdatedEventPayloadSchema>;

/** `project.access_changed`: one write of a project's owner, visibility, access list or Slack
 *  channels committed. It names the project and which change it was, and nothing else: no email,
 *  group name or channel id, since every reader of the project's events reads it. It is owned by
 *  the project and wakes nobody. */
export const ProjectAccessChangedEventPayloadSchema = z
  .strictObject({
    project: z.string(),
    change: z.enum([
      "owner",
      "visibility",
      "person_added",
      "person_removed",
      "group_added",
      "group_removed",
      "slack_channels",
    ]),
  })
  .readonly();

export type ProjectAccessChangedEventPayload = z.output<
  typeof ProjectAccessChangedEventPayloadSchema
>;
