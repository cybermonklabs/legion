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
  WritingCheckFinding,
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

interface CreateArtifactOptions {
  readonly name: string;
  readonly summary?: string;
  readonly actor?: Actor;
  /** The writing check's override, sent as the JSON upload's field or, with a file, as the
   *  multipart form value of the same name. */
  readonly override_writing_check?: WritingCheckOverride;
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

/**
 * The values `override_writing_check` takes on every write whose prose Dispatch's writing check
 * reads (INFRA-3231), which the OpenAPI document widens to `string`: `agent` sends a write the
 * check refused through as the agent's own judgement, which only an agent's explicit choice sends
 * (`dispatch <command> --override-writing-check`), and `generated` sends text a plugin generated
 * rather than an agent wrote, which no agent and no CLI call ever sends. Any other value is
 * `400 INVALID_WRITING_CHECK_OVERRIDE`.
 */
export const WRITING_CHECK_OVERRIDES = ["agent", "generated"] as const;

export type WritingCheckOverride = (typeof WRITING_CHECK_OVERRIDES)[number];

/** The code of the `422` Dispatch answers a write its writing check refused: nothing was written,
 *  and the refusal's `findings` name each sentence, the rule it breaks and how to fix it. */
export const WRITING_CHECK_REFUSED = "WRITING_CHECK_REFUSED";

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[] | IDCandidate[];
  readonly current?: EditPreconditionCurrent;
  readonly mismatches?: EditPreconditionMismatch[];
  /** A `WRITING_CHECK_REFUSED` refusal's findings, in the order the refused text holds them. */
  readonly findings?: WritingCheckFinding[] | null;
  /** A `WRITING_CHECK_REFUSED` refusal's name of the field that sends the write through. */
  readonly override_field?: string;
}
