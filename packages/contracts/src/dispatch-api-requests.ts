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

interface CreateArtifactOptions {
  readonly name: string;
  readonly summary?: string;
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

/** How the message of a Dispatch process's `404 NOT_FOUND` starts when the process has no route for
 *  the request ("no route for GET /api/v1/…"). While a release rolls out, two builds of Dispatch
 *  answer, and the older one answers this way for a route only the newer one serves. No handler
 *  ran, so a client may send the request again whatever its method. A `404 NOT_FOUND` for a
 *  resource the server does not know says "not found" instead. */
export const NO_ROUTE_MESSAGE_PREFIX = "no route for ";

/** The waits before each new attempt of a request a Dispatch process answered with no route. Each
 *  wait is jittered by ±25 % (`jitteredDelay`), so the attempts of many clients spread out, and
 *  each attempt may reach another process. */
export const NO_ROUTE_RETRY_DELAYS_MS: readonly number[] = [1_000, 2_000, 4_000];

/** What a client says when every attempt of a request met a Dispatch process with no route for it. */
export const NO_ROUTE_ROLLING_RELEASE_MESSAGE =
  "The Dispatch process that answered does not offer this action yet, because a new release of " +
  "Dispatch is in progress. Try again in one minute.";

/** Waits `ms` milliseconds, or until `signal` aborts. A client takes one so that a test does not
 *  wait. */
export type Sleep = (ms: number, signal?: AbortSignal | null) => Promise<void>;

/** The `Sleep` a client waits with when its caller gives none: a timer. When `signal` aborts,
 *  before or during the wait, the wait rejects at once with the signal's reason, so an aborted
 *  request sends nothing more. */
export const sleep: Sleep = (ms, signal) => {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal?.aborted) {
    reject(signal.reason);
    return promise;
  }
  const abort = () => {
    clearTimeout(timer);
    reject(signal?.reason);
  };
  const timer = setTimeout(() => {
    signal?.removeEventListener("abort", abort);
    resolve();
  }, ms);
  signal?.addEventListener("abort", abort, { once: true });
  return promise;
};

/** `ms` jittered by ±25 %: a wait of `NO_ROUTE_RETRY_DELAYS_MS`, as a client waits it. */
function jitteredDelay(ms: number): number {
  return ms * (0.75 + Math.random() * 0.5);
}

/**
 * Runs `attempt` until a Dispatch process that serves the route answers. An attempt `isNoRoute`
 * recognises is made again after each of `NO_ROUTE_RETRY_DELAYS_MS`, jittered (`jitteredDelay`)
 * and waited with `options.sleep` (a timer, `sleep`, when the caller names none): while a release
 * rolls out, two builds of Dispatch answer, and the next attempt may reach the build that serves
 * the route. No handler ran, so this is safe for every method, and every attempt is the same
 * request. After the last attempt it throws what `options.exhausted` builds, which is the
 * caller's own error for `NO_ROUTE_ROLLING_RELEASE_MESSAGE`. Anything else `attempt` throws is
 * thrown on at once, and an abort of `options.signal` during a wait rejects with the abort's
 * reason, so nothing more is sent.
 */
export async function retryNoRoute<T>(
  attempt: () => Promise<T>,
  isNoRoute: (error: unknown) => boolean,
  options: {
    readonly exhausted: () => Error;
    readonly sleep?: Sleep;
    readonly signal?: AbortSignal | null;
  }
): Promise<T> {
  const wait = options.sleep ?? sleep;
  for (const delay of [...NO_ROUTE_RETRY_DELAYS_MS, undefined]) {
    try {
      return await attempt();
    } catch (error) {
      if (!isNoRoute(error)) throw error;
      if (delay === undefined) break;
    }
    await wait(jitteredDelay(delay), options.signal);
  }
  throw options.exhausted();
}
