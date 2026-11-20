import { z } from "zod";
import { ActorSchema, InboundBySchema } from "./dispatch-api-core";

/**
 * The sessions Dispatch knows: the registry rows the Agents page lists, who is subscribed to an
 * owner's topics, and the payload of `subscription.remove_requested` and `subscription.removed`.
 */

/**
 * The longest `TmuxPlace.layout` either side handles: the cap the session's own client applies
 * before it sends a place (`packages/envoy-client/src/tmux-place.ts`, which drops a longer
 * layout and reports the place without it) and the cap a reader applies before it parses one
 * (`packages/dispatch/web/src/features/machines/tmux-layout.ts`). A layout grows with every
 * split, so a pathological window would make every heartbeat carry kilobytes a drawing can do
 * without; and because the listener stores whatever a session registers and Dispatch answers it
 * unchanged, a reader that trusted the sender's cap would parse a string no tmux wrote. One
 * number here, read by both, so neither side can drift from the other.
 *
 * The number is set by how many panes a layout may hold, not by its characters. 2,000
 * characters of tmux's version 1 form hold at most 129 tiled panes (a 200x50 window, the most of
 * five window sizes measured on tmux 3.8). tmux 3.8's JSON form spends about four times the
 * characters on a pane, and that same window of 129 panes is 7,592 characters long in it, so
 * 8,000 holds in the JSON form every window 2,000 held in version 1, and holds more in version 1.
 */
export const TMUX_LAYOUT_MAX = 8_000;

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
