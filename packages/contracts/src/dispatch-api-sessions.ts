import { z } from "zod";
import { ActorSchema, InboundBySchema } from "./dispatch-api-core";

/**
 * The sessions Dispatch knows: the registry rows the Agents page lists, who is subscribed to an
 * owner's topics, and the payload of `subscription.remove_requested` and `subscription.removed`.
 */

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
