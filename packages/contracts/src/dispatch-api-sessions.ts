import { z } from "zod";
import { ActorSchema, InboundBySchema } from "./dispatch-api-core";

/**
 * The sessions Dispatch knows: the registry rows the Agents page lists, who is subscribed to an
 * owner's topics, and the payload of `subscription.remove_requested` and `subscription.removed`.
 */

/**
 * The registry's own answer for a session it still holds a record of: `live` within the
 * listener's live window, `unresponsive` past it and inside the gone horizon. These are the two
 * a list can carry; `gone` is a session no list names. `IssueClaimHolderState` adds `unknown`,
 * which is a read's own answer about a holder rather than anything the registry reports.
 */
export type AgentSessionState = "live" | "unresponsive";

/**
 * Where a session's process sits in tmux: its pane, and the window and tmux session holding it,
 * as the session's own Envoy client read them from the tmux server it is under. `layout` is
 * tmux's `window_layout` string, which draws how the window is split; it is absent when the
 * client dropped it because a finely split window had grown it past what a registration carries,
 * so a reader that draws the layout does without it rather than taking the place for missing.
 * Mirrors the listener's `session.TmuxPlace`.
 */
export interface TmuxPlace {
  readonly socket: string;
  readonly server_pid: string;
  readonly session_name: string;
  readonly window_id: string;
  readonly window_index: number;
  readonly window_name: string;
  readonly pane_id: string;
  readonly pane_index: number;
  readonly layout?: string;
  readonly zoomed: boolean;
}

/**
 * The longest `TmuxPlace.layout` either side handles: the cap the session's own client applies
 * before it sends a place (`packages/envoy-client/src/tmux-place.ts`, which drops a longer
 * layout and reports the place without it) and the cap a reader applies before it parses one
 * (`packages/dispatch/web/src/features/machines/tmux-layout.ts`). A layout grows with every
 * split, so a pathological window would make every heartbeat carry kilobytes a drawing can do
 * without; and because the listener stores whatever a session registers and Dispatch answers it
 * unchanged, a reader that trusted the sender's cap would parse a string no tmux wrote. One
 * number here, read by both, so neither side can drift from the other.
 */
export const TMUX_LAYOUT_MAX = 2_000;

export interface Agent {
  readonly session_id: string;
  readonly title: string;
  readonly dir: string;
  readonly machine_id: string;
  /**
   * The session's real machine, as its own Envoy plugin reported it (envoy-client's
   * machineID(): `ENVOY_MACHINE_ID`, then an agent box's `AGENTBOX_HOST`, falling back to the
   * kernel hostname). `machine_id` is
   * always the listener that registered the session and owns its delivery, which may be a
   * single shared listener for every machine; this is the one to show and filter on. The
   * listener falls back to `machine_id` itself for a session whose plugin predates this field,
   * so it is never empty.
   */
  readonly host: string;
  /**
   * Where the session sits in tmux, as its last registration reported it. Absent for a session
   * in no pane — one in Kubernetes, or any process started outside tmux — and for one a
   * listener or Dispatch that predates the field answered: a reader has no pane to draw for it,
   * and must not read the absence as a session that has stopped. Its `layout` is absent on its
   * own when the client dropped it for length.
   */
  readonly tmux_place?: TmuxPlace;
  readonly roles: string[];
  /**
   * Wire value from the Envoy registry: an open string list. The known values
   * are `DELIVERY_CAPABILITIES`; unknown strings persist and never match a mode.
   */
  readonly capabilities: string[];
  readonly last_seen: number;
  /**
   * The state the listener's own record puts the session in when it answered: `live`, or
   * `unresponsive` for one past the live window that has not reached the gone horizon - it has
   * missed heartbeats, it has not ended, and it keeps its roles and claims, so the list carries
   * it rather than dropping it (dispatch://INFRA-392). A `gone` session is listed by nobody.
   * Absent from a Dispatch server that predates the field, and a reader then has only the
   * membership it always had: every listed session is running.
   */
  readonly state?: AgentSessionState;
  readonly open_asks: number;
  readonly last_activity: string | null;
}

// Subscriber is a session whose persisted Envoy interests include an issue's
// or unlinked document's topic family, enriched with its live registry state
// when the session is currently connected. Removable is false when the only
// matching topic is broader than this owner (e.g. "notifications.dispatch.>"):
// removing it would silently unsubscribe the session from every other issue
// and document too, so the server refuses to remove it; `via` then names one
// such topic.
export interface Subscriber {
  readonly session_id: string;
  readonly title: string;
  readonly live: boolean;
  readonly last_seen: number;
  readonly topics: readonly string[];
  readonly removable: boolean;
  readonly via?: string;
}

// SubscriptionRemovedEventPayload is the wire payload of subscription.removed:
// a human unsubscribed session_id from topics on this issue or document, and
// this record is both the audit trail and the direct notice routed to that
// session's own agent topic regardless of the issue's route.
export const subscriptionRemovedEventPayloadFields = {
  session_id: z.string(),
  by: ActorSchema,
  topics: z.array(z.string()).readonly(),
  /** True only while Dispatch is retrying the listener-side removal. */
  pending: z.boolean().optional(),
  /** The remove-requested event this completion settles. */
  request_event_id: z.number().int().positive().optional(),
};

export const WrittenSubscriptionRemovedEventPayloadSchema = z
  .strictObject(subscriptionRemovedEventPayloadFields)
  .readonly();

export type SubscriptionRemovedEventPayload = z.output<
  typeof WrittenSubscriptionRemovedEventPayloadSchema
>;

/** What a reader of the bus makes of a `subscription.*` payload: every field optional, and the
 *  remover read as a frame may carry it. */
export const SubscriptionRemovedEventPayloadSchema = z.object({
  session_id: subscriptionRemovedEventPayloadFields.session_id.optional(),
  by: InboundBySchema.optional(),
  topics: subscriptionRemovedEventPayloadFields.topics.optional(),
  pending: subscriptionRemovedEventPayloadFields.pending,
  request_event_id: subscriptionRemovedEventPayloadFields.request_event_id,
});
