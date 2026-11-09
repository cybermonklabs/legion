import { z } from "zod";
import type { WhoamiAgent, WhoamiUser } from "./dispatch-api.generated";

/**
 * The signed-in viewer: who Dispatch resolves a request to, and the per-viewer state the
 * dashboard keeps for an issue, an agent's conversation and a thread, with the events that say
 * one of them moved.
 */

/** `GET /auth/whoami`'s answer for a person signed in to the dashboard. */
export interface AuthenticatedUser {
  readonly kind: "user";
  readonly login: string;
  /** The decimal id at or below which every event is final (committed or rolled back for good)
   *  when Dispatch answered. Every read the page makes after this answer holds every event at or
   *  below it, so the event feed resumes from it on its first
   *  subscription and replays exactly what committed since. Always answered: a server that cannot
   *  answer the head answers a 503 rather than a whoami without one (`EVENT_HEAD_UNAVAILABLE` while
   *  no event id is proven final yet, which ends on its own; `EVENT_HEAD_READ_FAILED` when its
   *  database does not answer), and a
   *  server older than the field serves no `/ws/live` either, so no page of this release ever
   *  reads one of its answers. */
  readonly event_head: string;
}

/** GET /api/v1/whoami: a person by lowercase email (`login`), or an agent with its personal
 *  token's owner (a lowercase email) — null under the shared token or a verified service token —
 *  and that service token's subject, null unless one authenticated the request. `session` is the
 *  session the credential itself proved, which only an access token the secrets broker issued
 *  does, and `project` the one project key that credential lets the caller write, null where it
 *  names none. Both are absent from a Dispatch older than broker-issued tokens, which a client
 *  reads as an older build rather than as null.
 *
 *  `scopes` is the OAuth scopes the caller's own credential holds, which decide the routes it may
 *  call: both of them for a person at a browser, the set its row names for a personal token, and
 *  `[]` for the shared token and a verified service token, which act as nobody. The kind stays
 *  `agent` for a personal token holding `person`, because the kind names the credential and not
 *  the act; the person it acts for is `owner`. */
export type WhoamiResponse = WhoamiUser | WhoamiAgent;

export const UserIssueStateSchema = z
  .strictObject({
    pinned: z.boolean(),
    last_read_seq: z.number().int(),
    dismissed: z.array(z.string()),
    seq: z.number().int(),
  })
  .readonly();

export const UserStateUpdatedEventPayloadSchema = z
  .strictObject({
    login: z.string(),
    /** The viewer's row for one issue as the write left it. Absent from a snooze's event, which
     *  every signed-in feed carries and so names no issue of the writer's. */
    state: UserIssueStateSchema.optional(),
    /** Which of `pinned`, `last_read_seq`, `dismissed` and `snoozed` the write moved (`[]` when it
     *  moved none), so another tab refreshes its pinned list for a pin, its whole Inbox for a snooze
     *  or an un-snooze (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`, whose event carries only `login`
     *  and `["snoozed"]`), and neither for a read mark. Absent from an event written before the
     *  field existed, which a reader takes as "any of pinned, last_read_seq and dismissed may have
     *  moved". */
    changed: z.array(z.string()).optional(),
  })
  .readonly();

export type UserStateUpdatedEventPayload = z.output<typeof UserStateUpdatedEventPayloadSchema>;

/** `user_agent_state.updated`: one viewer's Clear or read mark on one session's conversation
 *  changed. It carries no state; the viewer's other tabs and devices refetch theirs. */
export const UserAgentStateUpdatedEventPayloadSchema = z
  .strictObject({ login: z.string(), session_id: z.string() })
  .readonly();

export type UserAgentStateUpdatedEventPayload = z.output<
  typeof UserAgentStateUpdatedEventPayloadSchema
>;

/** `user_thread_state.updated`: one viewer's read of the Inbox's threads moved. It carries no
 *  state; the viewer's other tabs and devices refetch theirs. `thread_id` names the thread when
 *  one thread was marked (`PUT /api/v1/me/threads/{id}/read`); a mark-all
 *  (`POST /api/v1/me/inbox/read`) names no thread, since one event stands for every thread it
 *  marked in that project. */
export const UserThreadStateUpdatedEventPayloadSchema = z
  .strictObject({ login: z.string(), thread_id: z.string().optional() })
  .readonly();

export type UserThreadStateUpdatedEventPayload = z.output<
  typeof UserThreadStateUpdatedEventPayloadSchema
>;
