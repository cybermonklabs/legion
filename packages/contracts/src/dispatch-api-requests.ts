import type {
  Actor,
  ArtifactVersionText,
  DocumentWriteAdvice,
  DuplicateCandidate,
  EditBlockPrecondition,
  EditPreconditionCurrent,
  EditPreconditionMismatch,
  IssueDetails,
  ParentStatusConflictIssue,
  StatusChangeEntry,
  TargetCandidate,
  WriteAdvice,
} from "./dispatch-api.generated";
import type { DispatchEvent } from "./dispatch-api-events";

/**
 * What a caller sends and what Dispatch answers: the write bodies of every route, the advice a
 * write comes back with, and the read shapes assembled from several areas at once.
 */

/** Response-only; never on an event payload. The advice an issue's write carries, which reports
 *  the issue's status, the session's writes since the last human event and its open asks. */
export type Advised<T> = T & { readonly advice?: WriteAdvice };

/** Response-only. The advice a **project** document's write carries: a project document stands
 *  under no issue, so there is no issue status, no writes-since-human count and no issue-scoped
 *  open-ask list to report, and the server sends the document's block report alone
 *  (`withRawAdvice`, the server's `documentWriteAdvice`). `Advised` would promise fields that
 *  write never sends. */
export type DocumentAdvised<T> = T & { readonly advice?: DocumentWriteAdvice };

/** The token of the version a whole-document upload is replacing, which `dispatch doc-read`
 *  prints as `Document token: sha256:…`. It is not an edit's `EditPrecondition`: an upload
 *  rewrites the whole tree, so a `blocks` guard says nothing about what it replaces and the
 *  server refuses one (`400 INVALID_PRECONDITION`, `docs.ValidateUploadPrecondition`). */
export interface UploadPrecondition {
  readonly document: string;
}

interface CreateArtifactOptions {
  readonly name: string;
  readonly summary?: string;
  /** The token of the version this upload is replacing, which `dispatch doc-read` prints. An
   *  upload over a document the owner already holds names it, and Dispatch checks it under the
   *  write's own lock. One that names none is taken for now without a check, since older
   *  `dispatch` commands cannot name one; a later release refuses it `400 PRECONDITION_REQUIRED`.
   *  A token behind the live document is refused
   *  `409 PRECONDITION_FAILED`, whose `current` carries the live token and `current.version` the
   *  live version number. An upload over a file or a picture names its standing version's token
   *  the same way, which `dispatch doc-read` prints in the same line and the artifact's
   *  `versions[].token` states, and is refused the same way for a token of a version since
   *  replaced; one naming none is taken unchecked. A first upload under a name replaces no
   *  version, so it names none. An upload that names a token where the document has none
   *  for it to name — a first upload under a name its owner holds nothing under, and the repair
   *  of a document whose live tree is outside the Proof schema — is refused
   *  `409 PRECONDITION_FAILED` carrying `code` and `error` alone: there is no live token to put
   *  in a `current`, and no mismatch to report, so the refusal asks for the upload again naming
   *  no token. */
  readonly precondition?: UploadPrecondition;
  readonly actor?: Actor;
}

/** What a create carries: a file, inline content, or neither. A create carrying a name alone
 *  makes an empty document, which a person or an agent writes in afterwards with
 *  `dispatch doc-edit`; it goes as JSON with the name, as inline content does, and never as
 *  multipart, which carries its bytes as its file part and still needs one. */
export type CreateArtifactInput = CreateArtifactOptions &
  (
    | { readonly file: Blob; readonly content?: undefined }
    | { readonly content: string; readonly file?: undefined }
    | { readonly file?: undefined; readonly content?: undefined }
  );

/** What `DispatchClient.read` composes from `GET /api/v1/issues/{key}` and that issue's events
 *  page: no route answers it. The events are `DispatchEvent`, whose payload this package types
 *  per event type; the route table's own `Event` leaves `payload` as `unknown`. */
export interface IssueRead {
  readonly issue: IssueDetails;
  readonly events: DispatchEvent[];
}

export interface ArtifactVersionBlob {
  readonly blob: Blob;
  readonly mime: string;
  readonly version: number;
}

export type ArtifactVersionContent = ArtifactVersionText | ArtifactVersionBlob;

/**
 * One item an ambiguous id prefix names (`ID_AMBIGUOUS`). `id` is the whole id the prefix
 * shortens - the item's uuid, or the block id a document block is unique under - and `ref` its
 * whole `dispatch://` reference, empty for an item that has no address (a direct message, a
 * document of an agent's conversation). At most one of `issue_key` and `document` names its
 * owner: the issue that owns it, or the reference of the project document it is written on.
 * `first_line` is the head of what tells two candidates apart, absent for a direct message a
 * caller may not read. `edited_at` is the time a comment's or a message's author last edited it,
 * and `deleted_at` with `deleted_by` the mark of one its author deleted, whose `first_line` the
 * cleared body leaves absent; each is absent on a post that stands unedited, on every other kind,
 * and from a Dispatch older than the fields.
 */
export interface IDCandidate {
  readonly kind: "ask" | "comment" | "message" | "artifact" | "block";
  readonly id: string;
  readonly ref: string;
  readonly issue_key?: string;
  readonly document?: string;
  readonly first_line?: string;
  readonly edited_at?: string;
  readonly deleted_at?: string;
  readonly deleted_by?: Actor;
}

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[] | IDCandidate[];
  readonly current?: EditPreconditionCurrent;
  readonly mismatches?: EditPreconditionMismatch[];
  /** On a `409 PRECONDITION_FAILED` of an edit under a block guard, each block the edit would
   *  remove that the guard does not name, with the token it carries now: a token to compare with
   *  the one the caller's own read listed, never one to send. Dispatch's refusal, in `error`, says
   *  how to answer it (`unguardedReason` in packages/envoy/internal/dispatch/docs/edits.go). */
  readonly unguarded_blocks?: EditBlockPrecondition[];
  /** Where to look next, which Dispatch adds to one refusal only: its answer to a path no route
   *  serves (`404 NOT_FOUND`, "no route for …", the router's `staticHandler`), which says that
   *  `GET /api/v1` lists every route. */
  readonly hint?: string;
  /** On a `409 PARENT_STATUS_CONFLICT`: each issue whose status the write would leave out of line
   *  with its parent's or its children's, with its status and whether it is the parent or a child. */
  readonly issues?: ParentStatusConflictIssue[] | null;
  /** On every refusal of `POST /api/v1/issues/{key}/status-change`: the entry of each step that
   *  wrote before the refusal, in the order the steps ran, and `[]` when none did. */
  readonly changed?: StatusChangeEntry[] | null;
}
