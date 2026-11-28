import { splitTrailing } from "./context-request";

/**
 * Which message of a model request is the approval message, as both plugins' `context` handlers
 * read it: pi-legion places it, and pi-envoy puts its people's-answers message before it. Neither
 * plugin's shipped code imports the other's, so what both read lives here, one body for both.
 */

/**
 * The custom type of the approval message: the message pi-legion places in a request of a root
 * architect's session while a person's approval of its spec stands. The message goes after every
 * message another plugin adds, so pi-envoy puts its answers message before a trailing run of
 * approval messages and never anchors one on it: the approval message stays the request's last
 * message whichever plugin's `context` handler runs first.
 */
export const APPROVAL_MESSAGE_TYPE = "legion-approval";

/** Whether `message` is the approval message: a custom message of `APPROVAL_MESSAGE_TYPE`. */
export function isApprovalMessage(message: unknown): boolean {
  return (
    typeof message === "object" &&
    message !== null &&
    "role" in message &&
    message.role === "custom" &&
    "customType" in message &&
    message.customType === APPROVAL_MESSAGE_TYPE
  );
}

/**
 * `messages` cut before its trailing run of approval messages (`isApprovalMessage`), as
 * `splitTrailing` cuts it: `head`, every message up to the last one that is not an approval
 * message, and `tail`, the approval messages after it, in order. A request that does not end with
 * one is all `head`, the same array.
 */
export function splitTrailingApproval(messages: readonly unknown[]): {
  readonly head: readonly unknown[];
  readonly tail: readonly unknown[];
} {
  return splitTrailing(messages, isApprovalMessage);
}
