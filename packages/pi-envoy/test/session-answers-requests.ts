// The messages and requests the `withSessionAnswers` cases are built from. Two suites share them:
// `src/session-answers-context.test.ts`, whose reads are scripted, and
// `src/session-answers-read.test.ts`, whose reads go through the real `readSessionAnswers`.
import type { SessionAnswer } from "@legion/contracts";
import { SESSION_ANSWERS_MARKER } from "@legion/envoy-client/session-answers";
import type { CarriedAnswers, SessionAnswersRequest } from "../src/session-answers-context";

// Message shapes as Oh My Pi hands them to a `context` handler.
export const LIST = `${SESSION_ANSWERS_MARKER}\n\n2026-10-27T10:00:00.000Z · alice answered your ask\nShip it.`;
export const skill = {
  role: "user",
  content: [{ type: "text", text: "<dispatch-first-skill>\nSearch Dispatch first." }],
  timestamp: 0,
};
export const ask = {
  role: "user",
  content: [{ type: "text", text: "Fix the bug." }],
  timestamp: 1,
};
export const readCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-1", name: "read", arguments: { path: "x.ts" } }],
  timestamp: 2,
};
export const readResult = {
  role: "toolResult",
  toolCallId: "call-1",
  toolName: "read",
  content: [{ type: "text", text: LIST }],
  isError: false,
  timestamp: 3,
};
export const grepCall = {
  role: "assistant",
  content: [{ type: "toolCall", id: "call-2", name: "grep", arguments: { pattern: "bug" } }],
  timestamp: 5,
};
export const grepResult = {
  role: "toolResult",
  toolCallId: "call-2",
  toolName: "grep",
  content: [{ type: "text", text: "x.ts:3: the bug" }],
  isError: false,
  timestamp: 6,
};

/** The session's branch entries for `items`, as `sessionManager.getBranch` lists them: a message
 *  in a message entry of its own, an entry (it has a `type`) as it is. */
export function branchOf(...items: readonly unknown[]): () => readonly unknown[] {
  return () =>
    items.map((item, index) =>
      typeof item === "object" && item !== null && "type" in item
        ? item
        : { type: "message", id: `e${index}`, message: item }
    );
}

export const noBranch = branchOf();

/** A person's message to the session, the fields every fixture item shares. */
export const ITEM: SessionAnswer = {
  kind: "message",
  relation: "sent_you_a_message",
  id: "",
  at: "",
  author: { kind: "user", id: "person" },
  text: "",
  owner: null,
  ref: null,
  href: "/agents/ses/live",
};

/** A request of `session` (a session of its own unless given), not compacting unless told, that
 *  drops what `unshownFailure` is told. */
export function requestOf(
  read: () => Promise<CarriedAnswers | undefined>,
  branch: () => readonly unknown[],
  session: object = {},
  compacting = false,
  id = "ses_a"
): SessionAnswersRequest {
  return {
    read,
    branch,
    session,
    id,
    compacting: () => compacting,
    unshownFailure: () => undefined,
  };
}
