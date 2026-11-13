import type {
  Actor,
  ArtifactVersionText,
  DocumentWriteAdvice,
  DuplicateCandidate,
  EditPreconditionCurrent,
  EditPreconditionMismatch,
  IssueDetails,
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
  /** The token of the version this upload is replacing, which `dispatch doc-read` prints. Every
   *  upload over a document the owner already holds names it, and one that names none is refused
   *  `400 PRECONDITION_REQUIRED`. A token behind the live document is refused
   *  `409 PRECONDITION_FAILED`, whose `current` carries the live token and `current.version` the
   *  live version number. A first upload under a name replaces no version, and a file is no
   *  document, so neither names one. An upload that names a token where the document has none
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
 * The code of the `409` Dispatch answers an id written as a prefix two items or more carry: a
 * page address, a `dispatch://` reference or a dashboard address in text, or a route that takes
 * an id in its path (INFRA-869). `candidates` then lists the items it names, at most ten, in id
 * order, for the caller to write more of the id or to choose one. Generated into Go as
 * `contracts.IDAmbiguous`.
 */
export const ID_AMBIGUOUS = "ID_AMBIGUOUS";

/**
 * One item an ambiguous id prefix names (`ID_AMBIGUOUS`). `id` is the whole id the prefix
 * shortens - the item's uuid, or the block id a document block is unique under - and `ref` its
 * whole `dispatch://` reference, empty for an item that has no address (a direct message, a
 * document of an agent's conversation). At most one of `issue_key` and `document` names its
 * owner: the issue that owns it, or the reference of the project document it is written on.
 * `first_line` is the head of what tells two candidates apart, absent for a direct message a
 * caller may not read.
 */
export interface IDCandidate {
  readonly kind: "ask" | "comment" | "message" | "artifact" | "block";
  readonly id: string;
  readonly ref: string;
  readonly issue_key?: string;
  readonly document?: string;
  readonly first_line?: string;
}

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[] | IDCandidate[];
  readonly current?: EditPreconditionCurrent;
  readonly mismatches?: EditPreconditionMismatch[];
}
