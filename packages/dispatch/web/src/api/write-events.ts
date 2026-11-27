import type { ReadHold } from "./query-client";
import type { DispatchEvent } from "./types";

/**
 * The live event each write that holds back its queries' reads ends its hold on
 * (`ReadHold.endsOn`). Dispatch writes a write's event in the write's own transaction and the
 * feed reads it back only once committed, so the event says the write is in: a read asked after
 * it answers the write, and every change anyone made since. No write's answer names its event and
 * no event names a tab, so each predicate matches the entity the write changed, where the event
 * names one, and the viewer whose write it is. One event of the viewer's can still match the holds
 * of several writes of one kind still out, and it says only that one of them is in, so it ends
 * one hold alone (`hearLiveEvent`).
 */
type WriteEvent = NonNullable<ReadHold["endsOn"]>;

/**
 * Whether `login` is the signed-in viewer's, compared as the server compares logins. The event
 * map (`event-refresh.ts`) reads it too, and takes it from here rather than this module from
 * there: the map is a chunk of its own that the feed downloads once the page has drawn
 * (`event-refresh-load.ts`), and this module is in the entry chunk every page parses first - the
 * agent pins the shell draws (`features/agents/pins.ts`) and the Inbox's writes import it - so
 * one import of the map from here would put the whole map back in that entry.
 */
export function isViewer(login: string | undefined, viewer: string | undefined): boolean {
  return (
    login !== undefined &&
    viewer !== undefined &&
    login.trim().toLowerCase() === viewer.trim().toLowerCase()
  );
}

/** An agent's pin (`PUT /api/v1/me/agents/{session}/state`): that session's state, the viewer's.
 *  The event names the session, so a pin of another session leaves the hold; a read mark of the
 *  same session (the live view's, or Mark all read's on an Agent row) publishes the same event,
 *  and can end it. */
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

/** A snooze or an un-snooze of the viewer's (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`): its
 *  `user_state.updated` names `snoozed` and no ask, since Dispatch replays it to every signed-in
 *  feed (`askSnoozeEvent`, `ask_snooze.go`). So it matches every snooze of the viewer's, and two
 *  snoozes out each wait for one of these (`ReadHold.events` for a bulk snooze, one for each ask).
 *  A snooze that moves nothing publishes nothing, and its hold lets its reads go at its answer. */
export const snoozeWritten: WriteEvent = (event, viewer) =>
  event.type === "user_state.updated" &&
  isViewer(event.payload.login, viewer) &&
  event.payload.changed?.includes("snoozed") === true;

/** A Mark all read or a dismiss of the viewer's Inbox threads (`POST /api/v1/me/inbox/read`,
 *  `POST /api/v1/me/inbox/dismiss`): one thread-state event with no thread for each project it
 *  moved, since one event stands for every thread it marked there. So it matches every such press
 *  of the viewer's. It publishes an agent-state event for each session too, but so does an agent's
 *  pin or a read in the live view, so a press that moves only Agent rows lets its reads go at its
 *  answer. */
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
 *  `issue.updated` names the issue and `components` among what it changed. One that changed none
 *  lets its reads go at its answer; an earlier write of the issue's other fields, which goes out
 *  before it, does not end it. Two saves of one issue go out in press order (`issue:<KEY>`), so the
 *  first such event is the older save's. */
export function componentsWritten(issueKey: string): WriteEvent {
  return (event, viewer) =>
    event.type === "issue.updated" &&
    event.issue_key === issueKey &&
    event.payload.changed?.includes("components") === true &&
    byViewer(event, viewer);
}
