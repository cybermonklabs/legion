import { DispatchGatewayError, DispatchServiceError, isNoRoute } from "../dispatch-http";
import { messageFor } from "../errors";

/**
 * A Dispatch refusal carrying its own code in the message the host shows: the code
 * (ISSUE_CLAIMED, CLAIM_CONTENDED, EXTERNAL_LINK_TAKEN, ...) is the part an agent acts on, and
 * the prose alone hides it. Only the message changes: every other field of the refusal
 * (`candidates`, `current`, `mismatches`) rides along, since a caller reads them off the error
 * it catches. An error that is not a refusal is returned as it is, so a caller's
 * `throw refusalWithCode(error)` rethrows it untouched. This returns rather than throws, so the
 * throw stays at the call site, where a reader of the handler sees that the path ends there.
 *
 * A gateway's answer stays a `DispatchGatewayError`, and the caller's account of what its call did
 * joins it in one sentence. Where the request may have reached Dispatch (`mayHaveReachedDispatch`),
 * the client's advice could judge only from the method (a write is told to check whether it took
 * effect), so that account, which knows what its call did, takes its place and must say what to do;
 * otherwise the request never reached Dispatch, and the account follows the client's advice.
 */
export function refusalWithCode(error: unknown, ...clauses: string[]): unknown {
  const suffix = clauses.filter((clause) => clause !== "").join("; ");
  const joined = suffix === "" ? "" : `; ${suffix}`;
  if (error instanceof DispatchGatewayError) {
    let told = error.message;
    if (joined !== "") {
      told = error.mayHaveReachedDispatch
        ? `${error.answer}${joined}`
        : `${error.answer}, so ${error.advice}${joined}`;
    }
    return new DispatchGatewayError(
      error.status,
      error.answer,
      error.advice,
      `${error.code}: ${told}`
    );
  }
  if (!(error instanceof DispatchServiceError)) return error;
  return new DispatchServiceError(
    error.code,
    error.status,
    `${error.code}: ${error.message}${joined}`,
    error.candidates,
    error.current,
    error.mismatches
  );
}

/** Whether Dispatch itself answered `status`; a gateway's answer of that status says nothing it decided. */
export function dispatchAnswered(error: unknown, status: number): error is DispatchServiceError {
  return error instanceof DispatchServiceError && error.fromDispatch && error.status === status;
}

/** The message an older build's comment edit refuses the `actor` this client sends with: its
 *  `decodeJSON` (`api/server.go`) writes "invalid JSON body: " and the decoder's error, which Go's
 *  `encoding/json` words `json: unknown field "actor"`. */
const OLDER_COMMENT_EDIT_ACTOR = 'invalid JSON body: json: unknown field "actor"';

/**
 * The answer of `call`, with an older Dispatch's refusal of a post correction replaced by the one
 * sentence that names the real cause. The plugins release from main as this merges while
 * Dispatch's own server is deployed by hand (dispatch://INFRA-1464), so for a while these
 * commands run against a server with no route by which an agent edits or deletes a post or reads
 * the bodies one replaced: its router answers `404 NOT_FOUND` "no route for …" (`isNoRoute`) to
 * both deletes, the message edit and the history reads. Its comment edit, the one route that was
 * there before, refuses this release's call in one of two ways, by the credential. It first reads
 * the person it acts for (`actingPerson`), which answers a credential without the person scope
 * `403 OAUTH_SCOPE_REQUIRED`; the route at this head names no scope, so only an older build
 * answers that code there. A credential with that scope, a person's own token included, passes,
 * and the body is then decoded into an input that holds only `body`, `base_revision` and `blocks`,
 * with unknown fields disallowed; the body this client sends always carries `actor`, so that build
 * answers `400 INVALID_JSON` naming the unknown field `actor` before it compares the author. This
 * release's input takes `actor`, so only an older build answers that. Only the comment edit reads
 * either code as the server's age, and only in those exact forms: an `INVALID_JSON` naming any
 * other field is passed through. Without this, an agent is told its credential lacks a scope, or
 * that its own request was malformed, and goes asking a person for one or rewriting the call.
 */
export async function throughPostCorrections<T>(
  tool: string,
  url: string,
  call: Promise<T>
): Promise<T> {
  try {
    return await call;
  } catch (error) {
    const commentEditIsAge =
      tool === "dispatch_edit_comment" &&
      ((dispatchAnswered(error, 403) && error.code === "OAUTH_SCOPE_REQUIRED") ||
        (dispatchAnswered(error, 400) &&
          error.code === "INVALID_JSON" &&
          error.message === OLDER_COMMENT_EDIT_ACTOR));
    if (!isNoRoute(error) && !commentEditIsAge) throw error;
    throw new Error(
      `This Dispatch (${url}) predates post corrections: it has no route by which an agent edits ` +
        "or deletes a post or reads the bodies one replaced. Until it is deployed with this " +
        "release's server, a correction is one more post saying what changed."
    );
  }
}

/**
 * The sentence a refusal of a deleted post's block carries, for a message written in this
 * command's own words to end with: ` This comment is deleted.`, ` This message is deleted.`, or
 * nothing where the server said no such thing. The server writes it as the tail of its 404
 * (`api/references.go`, `itemBlockNotFound`), and a read that quotes a post says that the post
 * is deleted wherever it is read, so a message that drops the server's words keeps this one.
 */
export function deletedPostClause(error: DispatchServiceError): string {
  for (const kind of ["comment", "message"] as const) {
    if (error.message.endsWith(`: this ${kind} is deleted`)) {
      return ` This ${kind} is deleted.`;
    }
  }
  return "";
}

/**
 * Whether a write that failed with `error` may have taken effect anyway. Only an answer sent
 * before the write could apply proves it did not: Dispatch's own refusal (a 4xx), or a gateway's
 * answer that never reached Dispatch (`mayHaveReachedDispatch`), which the client throws as such
 * only when no earlier attempt of the write may have reached Dispatch either. Dispatch's own 5xx
 * can come after it committed, and a timeout, a transport error or a `DispatchWriteUnsettledError`
 * (an earlier attempt may have reached Dispatch) says nothing either way.
 */
export function writeMayHaveLanded(error: unknown): boolean {
  if (error instanceof DispatchGatewayError) return error.mayHaveReachedDispatch;
  return !(error instanceof DispatchServiceError) || error.status >= 500;
}

/**
 * An error that is no refusal (a timeout, a transport error) with `account`, the caller's clause
 * on what its call did, after its message: joined to it with "; ", or as a sentence of its own
 * after a message that ends one ("The operation timed out.", "… able to access the url?").
 */
export function withAccount(error: unknown, account: string): Error {
  const message = messageFor(error);
  const told = /[.!?]$/.test(message)
    ? `${message} ${account.charAt(0).toUpperCase()}${account.slice(1)}`
    : `${message}; ${account}`;
  return new Error(told, { cause: error });
}
