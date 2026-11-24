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

/** The session id `recordedFor` and `requestOf` use unless given one. */
const SESSION_ID = "ses_a";

/** What each request's `record` wrote, by its session object and then its session id, in the
 *  order written: the entries a session's branch holds after the items its fixture branch lists.
 *  A module-level `WeakMap`, so every request built with the same `session` object reads what the
 *  ones before it recorded, as a real session's `getBranch` does after `pi.appendEntry`. */
const recorded = new WeakMap<object, Map<string, unknown[]>>();

/** The data of every entry the requests of `session` (and `id`) recorded, in the order written:
 *  what a case reads to check what a request recorded. */
export function recordedFor(session: object, id = SESSION_ID): readonly unknown[] {
  return recorded.get(session)?.get(id) ?? [];
}

/** The branch entry `pi.appendEntry("envoy-answers-sent", data)` writes, with `id`: how a branch
 *  lists an answers message a request sent. */
export function answersSentEntry(data: unknown, id: string): unknown {
  return { type: "custom", customType: "envoy-answers-sent", id, data };
}

/** A request of `session` (a session of its own unless given), not compacting unless told, whose
 *  `record` keeps each entry for `session` and `id` and whose `branch` returns `branch`'s entries
 *  followed by one `custom` entry (`envoy-answers-sent`) per entry recorded for them, in the order
 *  recorded, so the next request of the same `session` object reads what the one before it wrote,
 *  as a session does. A fresh `session` object over the same entries, as a resumed process sees
 *  them, is a `branch` that lists them itself. It drops what `readFailed` is told. */
export function requestOf(
  read: () => Promise<CarriedAnswers | undefined>,
  branch: () => readonly unknown[],
  session: object = {},
  compacting = false,
  id = SESSION_ID
): SessionAnswersRequest {
  return {
    read,
    branch: () => [
      ...branch(),
      ...recordedFor(session, id).map((data, index) => answersSentEntry(data, `a${index}`)),
    ],
    session,
    id,
    compacting: () => compacting,
    record: (entry) => {
      const sessions = recorded.get(session) ?? new Map<string, unknown[]>();
      recorded.set(session, sessions);
      const entries = sessions.get(id) ?? [];
      sessions.set(id, entries);
      entries.push(entry);
    },
    readFailed: () => undefined,
  };
}
