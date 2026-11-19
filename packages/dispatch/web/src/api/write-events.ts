import { isViewer } from "./event-refresh";
import type { ReadHold } from "./query-client";
import type { DispatchEvent } from "./types";

/**
 * The live event each write that holds back its queries' reads ends its hold on
 * (`ReadHold.endsOn`). Dispatch writes a write's event in the write's own transaction and the
 * feed reads it back only once committed, so the event says the write is in: a read asked after
 * it answers the write, and every change anyone made since. No write's answer names its event and
 * no event names a tab, so each predicate matches the entity the write changed and, where the
 * event names no entity, the viewer whose state it is.
 */
type WriteEvent = NonNullable<ReadHold["endsOn"]>;

/** An agent's pin (`PUT /api/v1/me/agents/{session}/state`): that session's state, the viewer's. */
export function agentStateWritten(sessionId: string): WriteEvent {
  return (event, viewer) =>
    event.type === "user_agent_state.updated" &&
    event.payload.session_id === sessionId &&
    isViewer(event.payload.login, viewer);
}

/** Whether `event`'s actor is the viewer: a write from this page is the signed-in person's. */
function byViewer(event: DispatchEvent, viewer: string | undefined): boolean {
  return event.actor.kind === "user" && isViewer(event.actor.id, viewer);
}

/** A write of one ask by the viewer - an answer (`ask.answered`), a decline (`ask.resolved`), a
 *  hand-on (`ask.asked_of`) - by the event types it publishes. */
export function askWritten(
  askId: string,
  types: readonly ("ask.answered" | "ask.asked_of" | "ask.resolved")[]
): WriteEvent {
  return (event, viewer) =>
    (event.type === "ask.answered" ||
      event.type === "ask.asked_of" ||
      event.type === "ask.resolved") &&
    types.includes(event.type) &&
    event.payload.id === askId &&
    byViewer(event, viewer);
}

/** Whether `event` is a snooze or an un-snooze of the viewer's: the event names no ask. */
function isSnooze(event: DispatchEvent, viewer: string | undefined): boolean {
  return (
    event.type === "user_state.updated" &&
    isViewer(event.payload.login, viewer) &&
    event.payload.changed?.includes("snoozed") === true
  );
}

/** `count` snoozes of the viewer's (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`), one event each:
 *  the event names no ask, so a set of them ends its hold on as many. A snooze that moves nothing
 *  publishes nothing, and its hold ends with the writes instead. */
export function snoozesWritten(count: number): WriteEvent {
  let heard = 0;
  return (event, viewer) => {
    if (!isSnooze(event, viewer)) return false;
    heard += 1;
    return heard >= count;
  };
}

/** A Mark all read or a dismiss of the viewer's Inbox threads (`POST /api/v1/me/inbox/read`,
 *  `POST /api/v1/me/inbox/dismiss`): one thread-state event with no thread for each project it
 *  moved. It publishes an agent-state event for each session too, but so does an agent's pin or a
 *  read in the live view, so a press that moves only Agent rows ends its hold with its answer. */
export const inboxThreadsWritten: WriteEvent = (event, viewer) =>
  event.type === "user_thread_state.updated" &&
  event.payload.thread_id === undefined &&
  isViewer(event.payload.login, viewer);

/** Each comment action's event: a resolve, a reopen, and a suggestion's accept or reject. */
const commentActionEvents = {
  accept: "suggestion.accepted",
  reject: "suggestion.rejected",
  reopen: "comment.reopened",
  resolve: "comment.resolved",
} as const;

/** A comment action by the viewer (`features/margin/useCommentActionQueue.ts`) on comment
 *  `commentId`. */
export function commentWritten(
  commentId: string,
  kind: keyof typeof commentActionEvents
): WriteEvent {
  return (event, viewer) =>
    event.type === commentActionEvents[kind] &&
    (event.type === "comment.reopened" ||
      event.type === "comment.resolved" ||
      event.type === "suggestion.accepted" ||
      event.type === "suggestion.rejected") &&
    event.payload.id === commentId &&
    byViewer(event, viewer);
}

/** A components save by the viewer (`PATCH /api/v1/issues/{key}` with `components`): its
 *  `issue.updated` names `components` among what it changed. One that changed none ends its hold
 *  with its answer; an earlier write of the issue's other fields, which goes out before it, does
 *  not end it. */
export function componentsWritten(issueKey: string): WriteEvent {
  return (event, viewer) =>
    event.type === "issue.updated" &&
    event.issue_key === issueKey &&
    event.payload.changed?.includes("components") === true &&
    byViewer(event, viewer);
}
