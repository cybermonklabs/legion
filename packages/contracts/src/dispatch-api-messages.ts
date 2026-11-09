import { z } from "zod";
import type { ReplyBlock, ShapedMessage } from "./dispatch-api.generated";
import { ActorSchema, type DisplayTree, InboundActorSchema } from "./dispatch-api-core";
import { DELIVERY_CAPABILITIES } from "./dispatch-api-limits";
import {
  carriedBodyFields,
  ReplyBlockSchema,
  type ReplyBlockWithTrees,
  referenceChangesFields,
  threadLoginsFields,
  WrittenReplyBlockSchema,
} from "./dispatch-api-refs";

/**
 * The message: what one session or person sends another, how each send was delivered, the
 * broadcast that groups many sends into one, and the payloads of every `message.*` event.
 */

export const messageDeliveryFields = {
  message_id: z.string(),
  attempt: z.number().int().positive(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string(),
  envelope_id: z.string().nullable(),
  /**
   * The stream already held this message when the attempt was sent: an earlier attempt landed,
   * so this one is recognised as a repeat and not handed to the recipient again
   * (`DELIVERY_DUPLICATE_WINDOW_MS` states where, and the limits). The attempt is still `sent` -
   * it reached the listener - but `envelope_id` is null, because the stream discarded the
   * envelope this send minted. Absent on a row written before the field existed, which reads as
   * false.
   */
  duplicate: z.boolean().optional(),
  /**
   * `pending` is an attempt Dispatch has committed but not yet sent, or whose send it never
   * learned the outcome of: the row is written before the listener call and settled by a
   * second transaction after it, so no Envoy send holds a database connection. A retry
   * resumes a pending attempt no live sender still holds, under its original idempotency key,
   * so the listener deduplicates a send that did land.
   */
  state: z.enum(["pending", "sent", "failed"]),
  error: z.string().nullable(),
  reply_id: z.string().nullable(),
  created_at: z.string(),
  /**
   * Whoever's send opened the attempt: the person who wrote or retried the message, or the session
   * a bearer's retry named. A resume keeps it. Null on an attempt written before Dispatch recorded
   * it, and absent from a Dispatch older than the field.
   */
  requested_by: ActorSchema.nullable().optional(),
  /**
   * The attempt's session took the message as its user's own turn
   * (`POST /api/v1/messages/{id}/deliveries/{attempt}/accept`), and when. At most one attempt of
   * a message is ever accepted; null on every other attempt, absent from a Dispatch older than the
   * fields. The session said it took the message, so this outranks a `failed` state the send
   * recorded afterwards.
   */
  accepted_as: z.literal("user_turn").nullable().optional(),
  accepted_at: z.string().nullable().optional(),
};

export const MessageDeliverySchema = z.strictObject(messageDeliveryFields).readonly();

export const messageFields = {
  id: z.string(),
  issue_key: z.string().nullable(),
  author: ActorSchema,
  body: z.string(),
  target: z.string().nullable(),
  in_reply_to: z.string().nullable(),
  /**
   * The broadcast this message is one recipient's copy of; null for every other message, and
   * absent from a Dispatch older than the field.
   */
  broadcast_id: z.string().nullable().optional(),
  deliveries: z.array(MessageDeliverySchema),
  created_at: z.string(),
  /** The block of the message this reply answers (`in_reply_to_block`); null on every other
   *  message, and absent from a Dispatch older than the field. */
  reply_block: WrittenReplyBlockSchema.nullable().optional(),
};

export const MessageSchema = z.strictObject(messageFields).readonly();

/** The message row without the tree a dashboard read carries: the body the route table holds
 *  (`ShapedMessage`) with the block it answers back to the shape the bus payload carries. */
export type Message = Omit<ShapedMessage, "body_tree" | "reply_block"> & {
  readonly reply_block: ReplyBlock | null;
};

/** A message as the web app reads it, shaped exactly as a comment is: the tree of its body, and
 *  the block it answers carrying the tree of that block's excerpt. */
export type MessageWithTrees = ShapedMessage;

/** `thread_logins` is carried by `message.created` and `message.answered`: on an issue message
 *  the logins with a Replies row on its thread root (up `in_reply_to`), and on a direct
 *  (issue-less) conversation's message, which keeps no Replies row, the thread root author's
 *  login, the one Inbox that conversation's Agent row sits in (`[]` when no person opened it). */
export const messageEventPayloadFields = {
  ...messageFields,
  /** First 160 characters of the reply target's body, or of the block it names (`reply_block`);
   *  empty otherwise. */
  reply_body: z.string().optional(),
  /** The target of a reply's thread root: the conversation the reply lands in, for a consumer
   *  that groups a thread under its root. A session replying to a message aimed at that
   *  session inherits no target of its own, so its `target` names no conversation. Absent on a
   *  root message, whose `target` already names one, and under an untargeted thread. It is the
   *  root's `target` column verbatim, so it is whatever a target may be - `session:<id>` or
   *  the `role:<role>` route the thread was aimed at - and never resolved to a session. */
  thread_target: z.string().optional(),
  ...referenceChangesFields,
  ...threadLoginsFields,
  ...carriedBodyFields,
};

export const WrittenMessageEventPayloadSchema = z
  .strictObject(messageEventPayloadFields)
  .readonly();

export type MessageEventPayload = z.output<typeof WrittenMessageEventPayloadSchema>;

/** What a reader of the bus makes of a `message.*` payload: the fields a renderer shows, each
 *  optional, the author's `kind` an open string, and everything else dropped. */
export const MessageEventPayloadSchema = z.object({
  id: messageFields.id.optional(),
  body: messageFields.body.optional(),
  target: messageFields.target.optional(),
  in_reply_to: messageFields.in_reply_to.optional(),
  reply_body: messageEventPayloadFields.reply_body,
  thread_target: messageEventPayloadFields.thread_target,
  author: InboundActorSchema.optional(),
  reply_block: ReplyBlockSchema.nullish(),
  thread_logins: threadLoginsFields.thread_logins,
  body_truncated: carriedBodyFields.body_truncated,
  payload_omitted: carriedBodyFields.payload_omitted,
});

export const messageDeliveryEventPayloadFields = {
  message_id: z.string(),
  attempt: z.number().int().positive(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string(),
  target: z.string().optional(),
  title: z.string(),
  state: z.enum(["sent", "failed"]),
  /** The stream already held this message: an earlier attempt landed, so this one is recognised as
   *  a repeat and not handed to the recipient again (`DELIVERY_DUPLICATE_WINDOW_MS`). Absent means
   *  false. */
  duplicate: z.boolean().optional(),
  error: z.string().optional(),
};

export const WrittenMessageDeliveryEventPayloadSchema = z
  .strictObject(messageDeliveryEventPayloadFields)
  .readonly();

export type MessageDeliveryEventPayload = z.output<typeof WrittenMessageDeliveryEventPayloadSchema>;

/** What a reader of the bus makes of a `message.delivery` receipt: every field optional. A
 *  stripping object: without `duplicate` declared the flag is dropped from every agent-bound
 *  `message.delivery` frame. */
export const MessageDeliveryEventPayloadSchema = z.object({
  message_id: messageDeliveryEventPayloadFields.message_id.optional(),
  attempt: messageDeliveryEventPayloadFields.attempt.optional(),
  delivery: messageDeliveryEventPayloadFields.delivery.optional(),
  session_id: messageDeliveryEventPayloadFields.session_id.optional(),
  target: messageDeliveryEventPayloadFields.target,
  title: messageDeliveryEventPayloadFields.title.optional(),
  state: messageDeliveryEventPayloadFields.state.optional(),
  duplicate: messageDeliveryEventPayloadFields.duplicate,
  error: messageDeliveryEventPayloadFields.error,
});

/** `message.accepted`: the session an attempt went to took the message as its user's own turn.
 *  It is never a second `message.delivery` receipt; the send still appends its own. `target` is
 *  the message's own, `session:<recipient>` for a direct message. */
export const MessageAcceptedEventPayloadSchema = z
  .strictObject({
    message_id: z.string(),
    attempt: z.number().int().positive(),
    session_id: z.string(),
    accepted_as: z.literal("user_turn"),
    target: z.string(),
  })
  .readonly();

export type MessageAcceptedEventPayload = z.output<typeof MessageAcceptedEventPayloadSchema>;
