import { z } from "zod";
import { ActorSchema, InboundBySchema } from "./dispatch-api-core";

/**
 * The sessions Dispatch knows: the registry rows the Agents page lists, who is subscribed to an
 * owner's topics, the payload of `subscription.remove_requested` and `subscription.removed`, and
 * the payload of `role.undelivered`, an event's copy for a role that reached no session.
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

/**
 * `role.undelivered`: an event's copy for a role reached no session. Dispatch's outbox kept the
 * copy on the role's kept lane for an hour, up to three times, and no session took it
 * (`reason: "expired"`), or Envoy no longer held the copy, so there was nothing to keep again
 * (`reason: "copy_gone"`). The outbox appends it on that event's own owner - an issue, a project
 * document or a project - as a system actor (`{kind: "system", id: "dispatch-outbox"}`), so it
 * wakes nobody: a person reads it in the owner's history, and a session that follows the owner's
 * topic is sent it.
 *
 * `event_id` and `event_type` name the event whose copy reached no session and `role` the role
 * it was routed to; `keeps` is how many times its copy was kept, `kept_at` when the last copy was
 * kept and `dropped_at` when Envoy dropped it, both RFC 3339 times.
 */
export const RoleUndeliveredEventPayloadSchema = z
  .strictObject({
    event_id: z.number().int().positive(),
    event_type: z.string(),
    role: z.string(),
    keeps: z.number().int().positive(),
    kept_at: z.string(),
    dropped_at: z.string(),
    reason: z.enum(["expired", "copy_gone"]),
  })
  .readonly();

export type RoleUndeliveredEventPayload = z.output<typeof RoleUndeliveredEventPayloadSchema>;
