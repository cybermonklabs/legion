import {
  DispatchGatewayError,
  DispatchServiceError,
  type DispatchWriteUnsettledError,
} from "../dispatch-http";
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

/**
 * Whether Dispatch itself answered `status`; a gateway's answer of that status says nothing it
 * decided. A write Dispatch answered with its own 5xx is no refusal but a
 * `DispatchWriteUnsettledError` (the write may have committed first), so a caller that reads such
 * a 5xx asks this of that error's `last`.
 */
export function dispatchAnswered(error: unknown, status: number): error is DispatchServiceError {
  return error instanceof DispatchServiceError && error.fromDispatch && error.status === status;
}

/**
 * Whether a write that failed with `error` may have taken effect anyway. Only an answer sent
 * before the write could apply proves it did not: Dispatch's own refusal (a 4xx), or a gateway's
 * answer that never reached Dispatch (`mayHaveReachedDispatch`), which the client throws as such
 * only when no earlier attempt of the write may have reached Dispatch either. Dispatch's own 5xx
 * can come after it committed, so the client throws it as a `DispatchWriteUnsettledError`, as it
 * does a transport failure and a deadline after an attempt may have reached Dispatch; that error,
 * and a timeout or a transport error thrown as it came, says nothing either way.
 */
export function writeMayHaveLanded(error: unknown): boolean {
  if (error instanceof DispatchGatewayError) return error.mayHaveReachedDispatch;
  return !(error instanceof DispatchServiceError) || error.status >= 500;
}

/**
 * A write that may or may not have taken effect (`DispatchWriteUnsettledError`) told with
 * `clauses`, the caller's account of what its call did and what to do, in place of the client's
 * own words on it: an account that knows what the call wrote says that once, as `refusalWithCode`
 * lets one replace a gateway's advice. What the write got leads: a transport failure's, quoted
 * whole, carries its own advice for a server that may have moved.
 */
export function unsettledWithAccount(
  error: DispatchWriteUnsettledError,
  ...clauses: string[]
): Error {
  const account = clauses.filter((clause) => clause !== "").join("; ");
  return new Error(`${error.answer}; ${account}`, { cause: error });
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
