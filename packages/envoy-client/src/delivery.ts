import { isDeepStrictEqual } from "node:util";
import {
  ArtifactCreatedEventPayloadSchema as ArtifactCreatedPayloadSchema,
  ArtifactHumanEditedEventPayloadSchema as ArtifactHumanEditedPayloadSchema,
  ArtifactVersionEventPayloadSchema as ArtifactVersionPayloadSchema,
  AskEditedEventPayloadSchema as AskEditedPayloadSchema,
  AskFollowerEventPayloadSchema as AskFollowerPayloadSchema,
  AskEventPayloadSchema as AskPayloadSchema,
  ChildStatusEventPayloadSchema as ChildStatusPayloadSchema,
  CommentEventPayloadSchema as CommentPayloadSchema,
  type CommentRead,
  DELIVERY_DUPLICATE_WINDOW_MS,
  type DeliveryCapability,
  DISPATCH_DOCUMENT_TOPIC_PREFIX,
  DISPATCH_TOPIC_PREFIX,
  type InboundDispatchEvent as DispatchEvent,
  DispatchEventSchema,
  type DispatchTargetedCommentDeliverySchema,
  DispatchTargetedCommentPayloadSchema,
  DispatchTargetedDeliverySchema,
  DispatchTargetedMessagePayloadSchema,
  DispatchTargetedResourceIDSchema,
  dedupeKeyNamesItsEvent,
  EnvelopeSchema,
  EXPECTS_REPLY_VALUES,
  type ExpectsReply,
  IssueEventPayloadSchema as IssuePayloadSchema,
  legacyAgentSubject,
  MessageDeliveryEventPayloadSchema as MessageDeliveryPayloadSchema,
  MessageEventPayloadSchema as MessagePayloadSchema,
  type MessageRead,
  type SessionAnswerRelation,
  SubscriptionRemovedEventPayloadSchema as SubscriptionRemovedPayloadSchema,
  topicForms,
} from "@legion/contracts";
import { encode, encodeLines, escapeString } from "@toon-format/toon";
import { z } from "zod";
import { deletedMark, withEditedMark } from "./actor-text";
import { askAnswerText, dispatchWordsText, textHead } from "./ask-answer";
import { commandLine } from "./dispatch-command";
import type { ActiveDispatchConfig } from "./dispatch-config";
import { checkedDispatchClient, type DispatchClient } from "./dispatch-http";
import { dispatchChildRef, dispatchDocumentRef, dispatchIssueRef } from "./dispatch-owner";
import { pictureAddresses } from "./dispatch-pictures";
import { messageFor } from "./errors";
import { parseFrameJSON } from "./frame-json";
import { humanEditDiffBlock, humanEditHead } from "./human-edit";
import { sessionAnswerRelationPhrase } from "./session-answer-relations";

const KNOWN_SOURCES: Readonly<Record<string, unknown>> = EnvelopeSchema.shape.source.enum;
const FOREIGN_SESSION_ID = /\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b/g;
const AskAuthorPayloadSchema = z.object({
  author: z.object({ kind: z.string(), id: z.string() }).passthrough(),
});

const InboundSenderSchema = z.object({
  session_id: z.string().optional(),
  machine: z.string().optional(),
  cwd: z.string().optional(),
  title: z.string().optional(),
  roles: z.array(z.string()).optional(),
});

/**
 * What an inbound frame may say about the reply it wants, read as the envelope's one vocabulary:
 * the string `"none"` the field carried before the literal `false` replaced it parses as
 * `false`. A frame the stream stored before that change and a session on an older plugin build
 * still write that word, so the card reads it rather than discarding the field. A session reads
 * every frame from its own consumer through `renderInbound` below, which parses with
 * `InboundEnvelopeSchema`, so this is the one reader of the field and no reader of a parsed
 * frame is ever shown the old spelling. `EnvelopeSchema` itself stays strict: it is what a
 * producer of this repository is held to.
 */
export const InboundExpectsReplySchema = z
  .literal([...EXPECTS_REPLY_VALUES, "none"] as const)
  .transform((value): ExpectsReply => (value === "none" ? false : value));

// Parsed only from `parseFrameJSON`'s value: this schema passes unknown keys through by
// assignment, so a `__proto__` key that JSON.parse kept would become the envelope's prototype.
const InboundEnvelopeSchema = z
  .object({
    event_id: z.string().optional(),
    source: z.string(),
    source_event_id: z.string().optional(),
    source_session: z.string().optional(),
    topic: z.string().optional(),
    dedupe_key: z.string().optional(),
    issued_at: z.number().int().optional(),
    expires_at: z.number().int().optional(),
    // Set only on a copy the listener forwards from a role's kept lane: the message waited for
    // this session to claim the role, so the card says when it was kept.
    kept_at: z.number().int().optional(),
    payload_summary: z.string().optional(),
    payload: z.string().optional(),
    sender: InboundSenderSchema.optional(),
    in_reply_to: z.string().optional(),
    supersedes: z.string().optional(),
    urgency: z.string().optional(),
    expects_reply: InboundExpectsReplySchema.optional(),
  })
  .passthrough();

function tolerantShape(shape: z.ZodRawShape): z.ZodRawShape {
  return Object.fromEntries(
    Object.entries(shape).map(([key, schema]) => [key, z.catch(schema, undefined)])
  );
}

const TolerantInboundEnvelopeSchema = z
  .object({
    ...tolerantShape(InboundEnvelopeSchema.shape),
    // `source` is required on valid envelopes but omitted/malformed frames still render as unknown.
    source: InboundEnvelopeSchema.shape.source.optional().catch(undefined),
    // A malformed sender member must not discard valid sender members.
    sender: z.object(tolerantShape(InboundSenderSchema.shape)).optional().catch(undefined),
  })
  .passthrough();

export type InboundEnvelope = z.infer<typeof InboundEnvelopeSchema>;

export type DispatchDelivery = {
  readonly resource: "message" | "comment";
  readonly id: string;
  readonly attempt: number;
  readonly mode: DeliveryCapability;
  readonly replyPath: string;
  readonly replyFields: Readonly<Record<string, string>>;
  readonly issueKey: string | null;
  readonly body: string;
  /** `body` is the first part of the item's body and not all of it, as the event payload's
   *  `body_truncated` says (`CarriedBodyPayload` in `@legion/contracts`). A reader that needs
   *  the rest reads the item itself through `DispatchClient`. */
  readonly bodyTruncated?: boolean;
  /** The broadcast a message frame says its message is one recipient's copy of, when it names
   *  one. The frame is untrusted: this can only rule a user turn out, never in. */
  readonly broadcastId?: string;
};

function dispatchReplyPath(resource: DispatchDelivery["resource"], id: string): string {
  const collection = resource === "message" ? "messages" : "comments";
  return `/api/v1/${collection}/${encodeURIComponent(id)}/reply`;
}

/**
 * Answers a targeted Dispatch delivery on the sender's behalf: the reply body when the
 * host delivered it, or the error when it could not (so the sender sees the attempt fail
 * instead of waiting on a reply that never comes). A write through `DispatchClient`, so it
 * carries a key of its own, is asked again under it while Dispatch gives no definitive answer,
 * and throws Dispatch's refusal as a `DispatchServiceError`.
 *
 * `followUp` is the ask that a second answer to an attempt already settled by the first is
 * posted rather than handed back (`?follow_up=true`). It travels as the route's query parameter,
 * never in the body, which the route decodes strictly.
 */
export async function postDeliveryReply(
  config: Pick<ActiveDispatchConfig, "url" | "credential">,
  sessionId: string,
  delivery: DispatchDelivery,
  result: {
    readonly body?: string;
    readonly error?: string;
    readonly followUp?: boolean;
  }
): Promise<void> {
  const { followUp, ...reply } = result;
  const client = checkedDispatchClient(config.url, config.credential);
  await client.deliveryReply(
    delivery.resource,
    delivery.id,
    {
      actor: { kind: "session", id: sessionId },
      attempt: delivery.attempt,
      ...delivery.replyFields,
      ...reply,
    },
    followUp === undefined ? {} : { followUp }
  );
}

/** A message a subscription yielded, and when it arrived. */
export interface Arrival<Message> {
  readonly message: Message;
  /** When the subscription yielded it (`now`), however long the messages before it took. */
  readonly receivedAt: number;
}

/**
 * Reads `subscription` as its messages arrive and yields them in arrival order, each stamped with
 * when it arrived. nats.js pushes a message into a subscription's iterator as it arrives, and this
 * always waits on that iterator, so the stamp is the arrival time whatever the caller is doing
 * with the messages before it (a Dispatch accept, pictures, a side turn): a message waits here
 * until the caller asks for the next one. Once the subscription ends or throws, every message
 * already read is still yielded, in order, and only then does the caller's next read end or throw
 * that error, as nats.js's own iterator yields the messages it took before it stops. A caller that
 * stops reading early returns the subscription's iterator, which stops the subscription once the
 * iterator's pending read settles (nats.js takes a return only after the read in flight).
 */
export async function* readArrivals<Message>(
  subscription: AsyncIterable<Message>,
  now: () => number = Date.now
): AsyncGenerator<Arrival<Message>, void, undefined> {
  const source = subscription[Symbol.asyncIterator]();
  const arrived: Arrival<Message>[] = [];
  let stopped: { readonly error?: unknown; readonly failed: boolean } | undefined;
  let left = false;
  let wake: (() => void) | undefined;
  const signal = () => {
    wake?.();
    wake = undefined;
  };
  void (async () => {
    try {
      for (;;) {
        const read = await source.next();
        if (read.done === true || left) break;
        arrived.push({ message: read.value, receivedAt: now() });
        signal();
      }
      stopped = { failed: false };
    } catch (error) {
      stopped = { error, failed: true };
    }
    signal();
  })();
  try {
    for (;;) {
      const next = arrived.shift();
      if (next !== undefined) {
        yield next;
        continue;
      }
      if (stopped?.failed === true) throw stopped.error;
      if (stopped !== undefined) return;
      const { promise, resolve } = Promise.withResolvers<void>();
      wake = resolve;
      await promise;
    }
  } finally {
    if (stopped === undefined) {
      left = true;
      // The subscription's own end, or the reader's error, says how it stopped; a rejected return
      // adds nothing a caller that left could act on.
      void Promise.resolve(source.return?.()).catch(() => undefined);
    }
  }
}

/**
 * The part of an inbound frame a host recognises a repeat by: its `dedupe_key`, which a re-send
 * shares with the frame that came first, and its `event_id`, which only copies of one publish
 * share (the listener mints a new one for every send). Hosts parse it with this schema, so a field
 * the rule reads cannot go missing from one host's parse.
 */
export const DedupeIdentitySchema = z.object({
  event_id: z.string().min(1).optional(),
  dedupe_key: z.string().min(1).optional(),
  source: z.string().optional(),
  source_event_id: z.string().optional(),
});

export type DedupeIdentity = z.infer<typeof DedupeIdentitySchema>;

/**
 * What one claim recorded. `release` undoes exactly that, and only once. `settle` says the host is
 * done with the frame without undoing the record: it was handed over, or the host chose not to
 * show it. Until one of the two, the frame is pending (`DeliveryDedupe.pending`), and a card the
 * ledger asks for on that same frame waits for the delivery in flight rather than stand beside it.
 */
export interface DeliveryClaim {
  release(): void;
  settle(): void;
}

/**
 * A host's record of the frames it handed its agent, which drops a repeat. The whole promise it is
 * part of is stated once, on `DELIVERY_DUPLICATE_WINDOW_MS` in `@legion/contracts`; every host the
 * listener relays a consumer to (the Oh My Pi extension, the Claude Code channel) keeps one.
 *
 * A host claims a frame before anything it awaits, so a repeat that arrives while the first is
 * still being delivered (a frame the relay delivers again because the first delivery was never
 * answered, or behind a Dispatch reply in flight) is already a repeat. It releases the claim when
 * its agent was not handed the frame after all: the hand-off threw, or the host answered the frame
 * with an error instead (a BTW whose side turn failed, a frame it could not read). Dispatch records
 * that attempt failed, and a same-mode re-send of it must then reach the agent. Otherwise it
 * settles the claim once it is done with the frame, shown or not: a claim left open reads as
 * `pending` forever, and a card the ledger raises for that frame then waits on a delivery that
 * ended.
 */
export interface DeliveryDedupe {
  /**
   * Records the frame and answers whether the agent should be handed it: `undefined` for a repeat
   * (a key that names its event and was claimed inside the window, or the event id of a copy
   * claimed already), otherwise the claim, whose `release` undoes what this call recorded.
   */
  claim(frame: DedupeIdentity | undefined): DeliveryClaim | undefined;
  /**
   * Whether a claim of the frame is still being delivered: neither settled nor released. A claim
   * of its event id, or of a key that names its event, is that frame's claim, by the same identity
   * rule `claim` drops a repeat by. A settled claim says nothing of whether the frame was shown:
   * what was shown is in the delivery ledger (`DeliveryLedger.delivered`), which the card the
   * ledger raises reads first.
   */
  pending(frame: DedupeIdentity): boolean;
}

/**
 * The most keys a host's record holds. Past it a claim forgets the oldest key first, so a host's
 * memory stays bounded however many fresh keys a followed topic carries, and a re-send of a
 * forgotten key reaches the agent again: one of the places the promise fails
 * (`DELIVERY_DUPLICATE_WINDOW_MS`). The stream bounds its own record by the window alone. This
 * bound is two and a half times what the whole production stream stored in one 72-hour window
 * (100,028 messages, 2026-09-30), and the stream carries every subject a host follows but the role
 * lanes, so a host reaches it only once traffic grows past that. Full, the record holds about
 * 70 MB of heap (up to 272 bytes a key across the key shapes measured, Bun 1.3.14).
 */
export const DELIVERY_DEDUPE_KEY_LIMIT = 250_000;

/**
 * How many event ids a host's record holds at most, in two generations: when the newer one fills
 * to half of this it becomes the older one, and the older one is dropped whole, so a claim costs
 * the same however long the host has run. Copies of one publish arrive together, one per
 * overlapping subscription, so a copy is recognised unless more than half this many other frames
 * were claimed between them. The ids are held apart from the keys and spend none of
 * `DELIVERY_DEDUPE_KEY_LIMIT`.
 */
const DELIVERY_EVENT_ID_LIMIT = 10_000;

/**
 * A key is recorded only when it names its event (`dedupeKeyNamesItsEvent` in `@legion/contracts`),
 * for `DELIVERY_DUPLICATE_WINDOW_MS`, the window the retry promise is made for. Any other key is not
 * a dedupe key, and dropping on it would lose a distinct event that shares it. A frame's event id
 * is recorded whatever its key: distinct events never share one, so it recognises a frame the
 * relay delivered again after an unanswered delivery, which a key that does not name its event
 * cannot.
 *
 * A key is kept no longer than the window, because a repeat never refreshes it and every claim
 * first evicts the keys past the window, so the record holds at most the keys the host was handed
 * in the last 72 hours, and never more than `DELIVERY_DEDUPE_KEY_LIMIT`. A clock that steps
 * backwards keeps a key longer by the size of the step.
 */
export function createDeliveryDedupe(now: () => number = Date.now): DeliveryDedupe {
  // Insertion order is claim order, so the keys past the window, and the oldest key the limit
  // evicts, are always at the front. A key maps to when it was claimed, an event id to the number
  // of the claim that recorded it, so a release removes neither once another claim took it. The
  // pending maps hold only the claims neither settled nor released yet, each by its number.
  const keys = new Map<string, number>();
  let events = new Map<string, number>();
  let olderEvents = new Map<string, number>();
  const pendingKeys = new Map<string, number>();
  const pendingEvents = new Map<string, number>();
  let claims = 0;
  return {
    claim(frame) {
      const eventId = frame?.event_id;
      if (eventId !== undefined && (events.has(eventId) || olderEvents.has(eventId))) {
        return undefined;
      }
      const key = frame?.dedupe_key;
      const named = frame !== undefined && key !== undefined && dedupeKeyNamesItsEvent(frame);
      const at = now();
      if (named) {
        const claimedAt = keys.get(key);
        if (claimedAt !== undefined && at - claimedAt < DELIVERY_DUPLICATE_WINDOW_MS) {
          return undefined;
        }
        for (const [oldest, deliveredAt] of keys) {
          if (at - deliveredAt < DELIVERY_DUPLICATE_WINDOW_MS) break;
          keys.delete(oldest);
        }
        keys.delete(key);
        if (keys.size >= DELIVERY_DEDUPE_KEY_LIMIT) {
          const oldest = keys.keys().next();
          if (oldest.done !== true) keys.delete(oldest.value);
        }
        keys.set(key, at);
      }
      const claim = ++claims;
      if (eventId !== undefined) {
        if (events.size >= DELIVERY_EVENT_ID_LIMIT / 2) {
          olderEvents = events;
          events = new Map();
        }
        events.set(eventId, claim);
        pendingEvents.set(eventId, claim);
      }
      if (named) pendingKeys.set(key, claim);
      let open = true;
      const close = () => {
        if (!open) return;
        open = false;
        if (eventId !== undefined && pendingEvents.get(eventId) === claim) {
          pendingEvents.delete(eventId);
        }
        if (named && pendingKeys.get(key) === claim) pendingKeys.delete(key);
      };
      let released = false;
      return {
        release() {
          close();
          if (released) return;
          released = true;
          if (named && keys.get(key) === at) keys.delete(key);
          if (eventId === undefined) return;
          if (events.get(eventId) === claim) events.delete(eventId);
          if (olderEvents.get(eventId) === claim) olderEvents.delete(eventId);
        },
        settle: close,
      };
    },
    pending(frame) {
      const eventId = frame.event_id;
      const key = frame.dedupe_key;
      return (
        (eventId !== undefined && pendingEvents.has(eventId)) ||
        (key !== undefined && dedupeKeyNamesItsEvent(frame) && pendingKeys.has(key))
      );
    },
  };
}

function isCommentTargetedDelivery(
  delivery: z.infer<typeof DispatchTargetedDeliverySchema>
): delivery is z.infer<typeof DispatchTargetedCommentDeliverySchema> {
  return "comment_id" in delivery;
}

const DispatchTargetedFrameSchema = z
  .object({
    event: DispatchEventSchema,
    delivery: DispatchTargetedDeliverySchema,
  })
  .superRefine(({ event, delivery }, context) => {
    if (
      event.type === "message.created" &&
      !isCommentTargetedDelivery(delivery) &&
      DispatchTargetedMessagePayloadSchema.safeParse(event.payload).success
    ) {
      return;
    }
    if (
      event.type === "comment.created" &&
      isCommentTargetedDelivery(delivery) &&
      DispatchTargetedCommentPayloadSchema.safeParse(event.payload).success
    ) {
      return;
    }
    context.addIssue({
      code: "custom",
      message:
        event.type === "comment.created"
          ? "targeted delivery requires a complete comment.created event"
          : "targeted delivery requires a complete message.created event",
    });
  });

const RecoverableDispatchDeliveryFailureSchema = z
  .object({
    event: z
      .object({
        type: z.enum(["message.created", "comment.created"]),
        issue_key: z.string().nullable(),
        payload: z.object({ id: z.string(), body: z.string().optional() }).passthrough(),
      })
      .passthrough(),
    delivery: DispatchTargetedDeliverySchema,
  })
  .refine(
    ({ event, delivery }) =>
      event.type === "comment.created"
        ? isCommentTargetedDelivery(delivery)
        : !isCommentTargetedDelivery(delivery) &&
          DispatchTargetedResourceIDSchema.safeParse(event.payload.id).success,
    { message: "delivery resource must match its event type" }
  );

export type DeliveryEnvelope = Pick<
  InboundEnvelope,
  "source" | "source_session" | "payload" | "sender" | "event_id"
>;

export type RenderInboundResult = {
  readonly skip: boolean;
  readonly content: string;
  readonly envelope?: InboundEnvelope;
  /** Actor of a Dispatch event that passed the wire schema. */
  readonly dispatchActor?: DispatchEvent["actor"];
  readonly delivery?: DispatchDelivery;
  readonly rejectedDelivery?: DispatchDelivery;
  readonly malformedDelivery?: true;
  /** The Dispatch addresses of the pictures the delivered message, comment, ask question, answer
   *  or resolution reason embeds, in writing order; absent when it embeds none. */
  readonly pictures?: readonly string[];
  /** The comment or message whose body this event carried only in part, or not at all: the item
   *  a host reads whole (`wholeCarriedBody`) before it renders the frame again with that body.
   *  Absent when the event carried the body whole, and from a render given that body. */
  readonly cutItem?: CarriedItem;
  /** Why the card shows the payload as the text its sender sent rather than laid out: what the
   *  card's encoder threw on the parsed value (one nested past the depth its recursion reaches),
   *  or that its layout passes the card's bound (`INBOUND_CARD_MAX_BYTES`, or twice the text it
   *  lays out when that is more). Absent when the card lays the payload
   *  out. A host logs it once, with the message's id, for a frame it shows. */
  readonly payloadShownAsText?: string;
};

/** The pictures the text a Dispatch event brings embeds: a message's or comment's body, an opened
 *  or reworded ask's question, an answer's typed text, a resolution's reason. */
function eventPictures(event: DispatchEvent): string[] {
  let text: string | null | undefined;
  if (event.type.startsWith("message.")) {
    const parsed = MessagePayloadSchema.safeParse(event.payload);
    text = parsed.success ? parsed.data.body : undefined;
  } else if (event.type.startsWith("comment.")) {
    const parsed = CommentPayloadSchema.safeParse(event.payload);
    text = parsed.success ? parsed.data.body : undefined;
  } else if (event.type.startsWith("ask.")) {
    const parsed = (
      event.type === "ask.edited" ? AskEditedPayloadSchema : AskPayloadSchema
    ).safeParse(event.payload);
    if (parsed.success) {
      switch (event.type) {
        case "ask.answered":
          text = parsed.data.answer?.text;
          break;
        case "ask.resolved":
          text = parsed.data.resolution?.reason;
          break;
        case "ask.opened":
        case "ask.edited":
          text = parsed.data.question;
          break;
      }
    }
  }
  return typeof text === "string" ? pictureAddresses(text) : [];
}

/** The comment or message a Dispatch event names, as the item whose whole body a reader can go
 *  and read. */
export interface CarriedItem {
  readonly resource: "comment" | "message";
  readonly id: string;
  readonly issueKey: string | null;
}

/** The item a `comment.*`, `suggestion.*` or `message.*` event names; undefined for every other
 *  event, and for one whose payload names no id. */
export function carriedItem(
  event: Pick<DispatchEvent, "type" | "issue_key"> & { readonly payload: unknown }
): CarriedItem | undefined {
  const resource =
    event.type.startsWith("comment.") || event.type.startsWith("suggestion.")
      ? ("comment" as const)
      : event.type.startsWith("message.")
        ? ("message" as const)
        : undefined;
  if (resource === undefined || !isObject(event.payload)) return undefined;
  const id = event.payload["id"];
  if (typeof id !== "string" || id === "") return undefined;
  return { resource, id, issueKey: event.issue_key };
}

/** Whether an event payload is a stand-in: the bus refused the event and the outbox published it
 *  with its long fields dropped, except the few it keeps whole (`payload_omitted`, whose doc in
 *  `@legion/contracts` names them). */
function payloadWasOmitted(payload: unknown): boolean {
  return isObject(payload) && payload["payload_omitted"] === true;
}

/** Whether an event payload says the body on it is not the item's body whole: the first part of
 *  it (`CarriedBodyPayload.body_truncated` in `@legion/contracts`), or none of it, because the
 *  bus refused the event and the outbox published a stand-in with its long fields dropped
 *  (`payload_omitted`). Either way the item itself answers the rest. */
export function bodyWasCut(payload: unknown): boolean {
  return (isObject(payload) && payload["body_truncated"] === true) || payloadWasOmitted(payload);
}

/** The Dispatch route that answers the whole item an event names. */
function wholeItemRoute(item: CarriedItem): string {
  return `GET /api/v1/${item.resource === "comment" ? "comments" : "messages"}/${item.id}`;
}

/** The note a reader is given when an event carries less than the item's body - its first part,
 *  or nothing of it - naming the route that answers the item whole. Undefined when the event
 *  carries the body whole. */
function carriedBodyNote(event: DispatchEvent): string | undefined {
  if (!bodyWasCut(event.payload)) return undefined;
  const what = payloadWasOmitted(event.payload)
    ? "this event carries no body: it was too large for the bus"
    : "the body is its first part only";
  const item = carriedItem(event);
  return item === undefined ? what : `${what}; read it whole with ${wholeItemRoute(item)}`;
}

/** The whole body of a comment or message, read from the server. Undefined when the read fails
 *  or its answer does not hold the item, so the caller keeps the part its event carried: a body
 *  the server will not answer is still better read in part than not at all. A direct message,
 *  which belongs to no issue, is read through the session it was delivered to. An item its
 *  author deleted since answers the deleted line every read prints for one (`deletedMark`) in
 *  place of the body the delete cleared. An item its author edited answers its current body
 *  followed by the mark every line prints for one (` · edited <time>`, `editedMark`): the card a
 *  host renders with this body re-read the post, so it prints the words the post holds now, which
 *  an edit since the event may have changed. A card that prints only what its frame carried reads
 *  nothing here, and keeps the delivery Dispatch sent as it was sent. */
export async function wholeCarriedBody(
  client: DispatchClient,
  item: CarriedItem,
  session?: string
): Promise<string | undefined> {
  try {
    let posts: readonly (CommentRead["comment"] | MessageRead["message"])[];
    if (item.resource === "comment") {
      const read = await client.getComment(item.id);
      posts = [read.comment, ...(read.replies ?? [])];
    } else {
      if (item.issueKey === null && session === undefined) return undefined;
      const read =
        item.issueKey === null
          ? await client.getMessageThread(item.id, session)
          : await client.getMessage(item.issueKey, item.id);
      posts = [read.message, ...(read.replies ?? [])];
    }
    const post = posts.find((candidate) => candidate.id === item.id);
    if (post === undefined) return undefined;
    if (post.deleted_at != null) return deletedMark(post.deleted_at, post.deleted_by);
    return withEditedMark(post.body, post.edited_at);
  } catch {
    return undefined;
  }
}

export function senderLabel(envelope: DeliveryEnvelope): string {
  const sender = envelope.source_session ?? envelope.source ?? "unknown";
  return envelope.sender?.title === undefined ? sender : `${sender} (${envelope.sender.title})`;
}

/**
 * A ready-to-issue reply any host can act on: an Envoy tool call (OMP devices and claude-envoy's
 * MCP tools share these names), or the `dispatch` command line that answers a Dispatch event.
 */
export type ReplyHint =
  | { readonly tool: string; readonly args: Readonly<Record<string, string>> }
  | { readonly command: string };

export function replyWith(envelope: DeliveryEnvelope): ReplyHint | undefined {
  if (envelope.source !== "agent" || envelope.source_session === undefined) return undefined;
  return {
    tool: "envoy_send",
    args: {
      session_id: envelope.source_session,
      ...(envelope.event_id === undefined ? {} : { in_reply_to: envelope.event_id }),
      message: "...",
    },
  };
}

// Dispatch event payloads follow the wire contract: issue.* carries the Issue,
// ask.opened/anchor_refreshed/handed_back/asked_of/answered/resolved the Ask, ask.edited the
// edited Ask plus its prior fields and editor, message.created the Message,
// comment.*/suggestion.* the Comment plus artifact_name, artifact.created {artifact},
// artifact.version {artifact_id, name, version, diff?},
// artifact.human_edited {artifact_id, name, from_version, to_version, editors, diff, diff_truncated},
// and child.status {child_key, from, to}.
// Bus frames are untrusted, so each schema below declares exactly the fields
// this renderer surfaces and a frame that disagrees renders without them.

// Each Dispatch event type validates its payload with the schema that matches the
// wire contract documented above. A frame whose payload disagrees with its type's
// schema keeps the raw (already-parsed) payload object rather than dropping data.
const DISPATCH_PAYLOAD_SCHEMAS: Readonly<Record<string, z.ZodType>> = {
  "issue.created": IssuePayloadSchema,
  "issue.updated": IssuePayloadSchema,
  "issue.closed": IssuePayloadSchema,
  "artifact.created": ArtifactCreatedPayloadSchema,
  "artifact.version": ArtifactVersionPayloadSchema,
  "artifact.human_edited": ArtifactHumanEditedPayloadSchema,
  "ask.opened": AskPayloadSchema,
  "ask.anchor_refreshed": AskPayloadSchema,
  "ask.edited": AskEditedPayloadSchema,
  "ask.handed_back": AskPayloadSchema,
  "ask.asked_of": AskPayloadSchema,
  "ask.answered": AskPayloadSchema,
  "ask.resolved": AskPayloadSchema,
  "comment.created": CommentPayloadSchema,
  "comment.anchor_refreshed": CommentPayloadSchema,
  "comment.resolved": CommentPayloadSchema,
  "comment.reopened": CommentPayloadSchema,
  "comment.edited": CommentPayloadSchema,
  "comment.deleted": CommentPayloadSchema,
  "suggestion.accepted": CommentPayloadSchema,
  "suggestion.rejected": CommentPayloadSchema,
  "message.created": MessagePayloadSchema,
  "message.delivery": MessageDeliveryPayloadSchema,
  "message.answered": MessagePayloadSchema,
  "message.edited": MessagePayloadSchema,
  "message.deleted": MessagePayloadSchema,
  "child.status": ChildStatusPayloadSchema,
};
function topicDocument(
  topic: string | undefined
): { readonly project: string; readonly slug: string } | undefined {
  if (topic?.startsWith(DISPATCH_DOCUMENT_TOPIC_PREFIX) !== true) return undefined;
  const [project, slug] = topic.slice(DISPATCH_DOCUMENT_TOPIC_PREFIX.length).split(".", 3);
  return project !== undefined && project !== "" && slug !== undefined && slug !== ""
    ? { project, slug }
    : undefined;
}

function dispatchOwner(event: DispatchEvent, topic: string | undefined): string {
  if (event.issue_key !== null) return event.issue_key;
  const document = topicDocument(topic);
  if (document !== undefined) return `${document.project} / ${document.slug}`;
  return event.artifact_id ?? "unknown";
}

type CommentPayload = z.infer<typeof CommentPayloadSchema>;

function dispatchCommentReplyWith(
  event: DispatchEvent,
  topic: string | undefined,
  comment: CommentPayload | undefined
): ReplyHint | undefined {
  if (comment === undefined || comment.id === undefined) return undefined;

  const reply = comment.ask_id;
  const thread =
    reply === undefined || reply === null || reply === "" ? "reply_to" : "reply_to_ask";
  const threadID = thread === "reply_to" ? comment.id : String(reply);
  if (event.issue_key !== null) {
    return {
      command: commandLine("dispatch_comment", {
        issue: event.issue_key,
        [thread]: threadID,
        body: "...",
      }),
    };
  }

  let project = comment.project_key;
  let artifact = comment.artifact_slug;
  if (project === undefined || project === "" || artifact === undefined || artifact === "") {
    [project, artifact] = dispatchOwner(event, topic).split(" / ", 2);
  }
  if (project === undefined || project === "" || artifact === undefined || artifact === "")
    return undefined;
  return {
    command: commandLine("dispatch_comment", {
      project,
      artifact,
      [thread]: threadID,
      body: "...",
    }),
  };
}

/**
 * Whether the session reading a reply in an ask's thread (a `comment.created` with an `ask_id`)
 * owns the ask: asked it or follows it, as the payload's `ask_sessions` says. Every copy of the
 * event carries the same bytes, whatever brought it to this session, so the reader decides. The
 * outbox's stand-in for an event the bus refused (`payload_omitted`) keeps the list whole,
 * whatever its size, so a stand-in decides the same way. Undefined for a comment in no ask's
 * thread, and for a payload that carries no list: a Dispatch older than the field wrote it, its
 * stand-ins among them, or a resend replayed an event stored before it, and the card says what it
 * said before the field.
 */
function ownsRepliedAsk(
  comment: CommentPayload | undefined,
  sessionID: string
): boolean | undefined {
  if (comment?.ask_id === undefined || comment.ask_id === null) return undefined;
  return comment.ask_sessions?.includes(sessionID);
}

/**
 * What a reply in an ask's thread is to the session reading it, by the rule Dispatch's answers
 * read names the same reply by (`scan` in
 * packages/envoy/internal/dispatch/sessionanswers/sessionanswers.go), from the same facts: a
 * reply on its own ask for a session that owns the ask, which outranks a mention; else a mention,
 * when the frame is the delivery a mention sent this session; else what the comment is: a reply
 * to another comment, a comment on a document (written on a project document, anchored in one,
 * or on no issue), or a comment on an issue.
 */
function repliedAskRelation(
  event: DispatchEvent,
  comment: CommentPayload,
  owns: boolean,
  mentioned: boolean
): SessionAnswerRelation {
  if (owns) return "replied_on_your_ask";
  if (mentioned) return "mentioned_you";
  if (comment.reply_to !== undefined && comment.reply_to !== null) {
    return "replied_in_a_comment_thread";
  }
  if (
    (comment.artifact_id !== undefined && comment.artifact_id !== null) ||
    comment.anchor?.artifact_id !== undefined ||
    event.issue_key === null
  ) {
    return "commented_on_a_document";
  }
  return "commented_on_an_issue";
}

// A decision's events are a conversation between an agent and a human, and the agent has the
// ask (it wrote it, or a reference names it), so the frame carries what moved — never the ask
// row again. `question` is the head only: enough to recognise the ask, the full text is one
// `dispatch read` away. Every other event type keeps its validated payload (`dispatchPayload`).
// `askId` lets the caller add the ask's ref when the envelope carries no `in_reply_to` (the
// producer correlates answers and replies, not openings or edits).
type CompactDispatchRecord = Readonly<Record<string, string | readonly string[]>>;
interface CompactDispatch {
  readonly record: CompactDispatchRecord;
  readonly askId: string | undefined;
}

// How a closed ask reads on the card: the kind, the person's own reason where the record holds
// one, and the words Dispatch wrote for the asking agent where it holds those (a decline), under
// the label every reader of them names them with (`dispatchWordsText`), so nobody reads
// Dispatch's instruction as the person's note. A decline the person wrote no note with is the
// ordinary one, so an empty reason leaves no separator behind.
function askResolutionText(
  resolution: z.infer<typeof AskPayloadSchema>["resolution"]
): string | undefined {
  if (resolution === null || resolution === undefined) return undefined;
  const kind = resolution.kind ?? "resolved";
  const reason = resolution.reason ?? "";
  const closed = reason === "" ? kind : `${kind}: ${reason}`;
  const words = dispatchWordsText(resolution.dispatch_words);
  return words === undefined ? closed : `${closed} · ${words}`;
}

function dispatchCompact(
  event: DispatchEvent,
  comment: CommentPayload | undefined,
  ownsAsk: boolean | undefined
): CompactDispatch | undefined {
  if (event.type === "artifact.human_edited") {
    // A person's edit names its document, versions and editors in the card; its diff follows the
    // card as a fenced block (humanEditDiffBlock), where a reader sees it as the diff it is.
    const edit = ArtifactHumanEditedPayloadSchema.safeParse(event.payload);
    if (!edit.success) return undefined;
    return { askId: undefined, record: { edit: humanEditHead(edit.data) } };
  }
  if (event.type === "comment.created") {
    if (comment?.ask_id === undefined || comment.ask_id === null) return undefined;
    const question = dispatchAskQuestion(comment);
    // `state` tells a clarification on an open ask from discussion after the answer;
    // `waiting_on` says whose move it is now, which is a move for the ask's own sessions alone
    // (`ownsRepliedAsk`): a session the reply reached for another reason is shown no turn.
    return {
      askId: comment.ask_id,
      record: {
        ...(question === undefined ? {} : { question }),
        reply: comment.body ?? "",
        ...(comment.ask_state === undefined ? {} : { state: comment.ask_state }),
        ...(comment.ask_waiting_on === undefined || ownsAsk === false
          ? {}
          : { waiting_on: comment.ask_waiting_on }),
      },
    };
  }
  if (!event.type.startsWith("ask.")) return undefined;
  const ask = (event.type === "ask.edited" ? AskEditedPayloadSchema : AskPayloadSchema).safeParse(
    event.payload
  );
  if (!ask.success) return undefined;
  let record: CompactDispatchRecord = { question: textHead(ask.data.question ?? "") };
  switch (event.type) {
    case "ask.opened":
    case "ask.edited": {
      // A new or reworded question is read by sessions that did not write it: the choices
      // and where it is anchored are what they need to follow it.
      const options = (ask.data.options ?? [])
        .map((option) => option.label ?? "")
        .filter((label) => label !== "");
      const quote = ask.data.anchor?.quote;
      const { project, slug } = ask.data.anchor_artifact ?? {};
      const document =
        project !== undefined && project !== "" && slug !== undefined && slug !== ""
          ? `${project}/${slug}`
          : undefined;
      const previous =
        "previous" in ask.data && ask.data.previous !== undefined
          ? textHead(ask.data.previous.question)
          : undefined;
      record = {
        ...record,
        ...(options.length === 0 ? {} : { options }),
        ...(quote === undefined || quote === "" ? {} : { quote: textHead(quote) }),
        ...(document === undefined ? {} : { document }),
        ...(previous === undefined ? {} : { previous }),
      };
      break;
    }
    case "ask.answered": {
      const previousAnswer = ask.data.previous_answer;
      record = {
        ...record,
        answer: askAnswerText(ask.data.answer),
        ...(previousAnswer === null || previousAnswer === undefined
          ? {}
          : { previous_answer: askAnswerText(previousAnswer) }),
      };
      break;
    }
    case "ask.asked_of": {
      // Whom the ask waits on from now on, and who handed it there. An empty `asked_of` is a
      // hand-on to the assignee of the ask's own issue: the ask follows that issue again, and
      // the payload names no login for the person, so the line says where it went instead.
      const handedOnBy = ask.data.asked_of_by;
      record = {
        ...record,
        asked_of: ask.data.asked_of ?? "the assignee of its issue",
        ...(handedOnBy === null || handedOnBy === undefined || handedOnBy === ""
          ? {}
          : { handed_on_by: handedOnBy }),
      };
      break;
    }
    case "ask.resolved": {
      const resolved = askResolutionText(ask.data.resolution);
      record = resolved === undefined ? record : { ...record, resolved };
      break;
    }
    default:
      break;
  }
  return { askId: ask.data.id, record };
}

function dispatchPayload(event: DispatchEvent): unknown {
  // `Object.hasOwn`: the map inherits `Object.prototype`, so a frame typed `constructor` or
  // `toString` would read one of its functions as its schema.
  const schema = Object.hasOwn(DISPATCH_PAYLOAD_SCHEMAS, event.type)
    ? DISPATCH_PAYLOAD_SCHEMAS[event.type]
    : undefined;
  if (schema === undefined) return event.payload;
  const parsed = schema.safeParse(event.payload);
  return parsed.success ? parsed.data : event.payload;
}

/**
 * The fields of a Dispatch payload that are Dispatch's own bookkeeping, which its outbox and its
 * web clients read and an agent has no use for: the delivery attempts of a comment's mentions,
 * the routes a comment's mentions took a copy from (`model.CommentEventPayload`'s route
 * suppression), the Inboxes a write moved (`thread_logins`), the backlink counts it moved
 * (`references_changed` and its truncation marks), and an issue's place on the board (`rank`, a
 * key of some hundred characters that only sorts).
 */
const DISPATCH_BOOKKEEPING: ReadonlySet<string> = new Set([
  "deliveries",
  "mentions_decide_routes",
  "suppress_route",
  "suppressed_route",
  "suppressed_route_session_id",
  "suppressed_authors",
  "thread_logins",
  "references_changed",
  "references_changed_truncated",
  "references_changed_truncated_kinds",
  "rank",
]);

/**
 * The payload a Dispatch card prints: the event's payload with no field that states nothing, none
 * that repeats the card, and none of Dispatch's bookkeeping (`DISPATCH_BOOKKEEPING`). `issuedAt`
 * is when Envoy issued the envelope, which the card's `at` gives to the second.
 *
 * A field states nothing when it is `null`, `""`, `false` or `[]`, or an object whose every field
 * states nothing: Dispatch writes those for a field that is unset, and reads a boolean it does not
 * find as `false`. The rule holds at every depth, for each field of an object; a list keeps each
 * of its items, since an item's place in the list is part of what it says.
 *
 * A field whose value is the fact even when it is empty stays as Dispatch wrote it: each field an
 * update's `changed` names (a cleared `route: null` or `labels: []` is what it was cleared to; a
 * create's `changed` names every field it set, so it names no news), a present `suggestion` (an
 * empty `replace_with` proposes deleting the quoted text), and a present `forced` (a create's
 * `past: []` says its near-copy check did not answer, where no `forced` says it was not forced).
 * A field an update's `changed` names stays even when it is bookkeeping or repeats the card: the
 * change is the news.
 *
 * A field repeats the card when the card already says it: `issue_key` is the card's `owner`,
 * `author` or `actor` the card's `actor` when each of its fields is the event's actor's (whole,
 * before the card cuts a session actor's `origin` to its title), `created_at` the card's `at`
 * when it is less than a second from `issuedAt` (the two differ by milliseconds, often across a
 * second's edge), and a `target` that names this session the card's `to:`, which
 * `toReader` says the card prints. On a comment, `thread_root_id` always names the comment
 * `reply_to` names, and `answers` names it on a reply to a thread's root.
 *
 * A `target` that names this session on a card with no `to:` reads `you (<target>)`.
 */
function dispatchPayloadForReader(
  event: DispatchEvent,
  sessionID: string,
  issuedAt: number | undefined,
  toReader: boolean
): unknown {
  const whole = dispatchPayload(event);
  const changed =
    event.type !== "issue.created" && isObject(whole) && Array.isArray(whole["changed"])
      ? whole["changed"]
      : [];
  const payload = withoutEmptyFields(
    whole,
    (key, item) =>
      changed.includes(key) ||
      ((key === "suggestion" || key === "forced") && item !== null && item !== undefined)
  );
  if (!isObject(payload) || Array.isArray(payload)) return payload;
  const { reply_to: replyTo } = payload;
  const actor: Readonly<Record<string, unknown>> = event.actor;
  const repeatsTheCard = (key: string, value: unknown): boolean => {
    switch (key) {
      case "issue_key":
        return value === event.issue_key;
      case "author":
      case "actor":
        return (
          isObject(value) &&
          Object.entries(value).every(([field, held]) => isDeepStrictEqual(held, actor[field]))
        );
      case "created_at": {
        if (typeof value !== "string") return false;
        const timestamp = Date.parse(value);
        return issuedAt !== undefined && Math.abs(timestamp - issuedAt) < 1_000;
      }
      case "thread_root_id":
      case "answers":
        return typeof replyTo === "string" && value === replyTo;
      case "target":
        return toReader && value === `session:${sessionID}`;
      default:
        return DISPATCH_BOOKKEEPING.has(key);
    }
  };
  const shown: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(payload)) {
    if (!changed.includes(key) && repeatsTheCard(key, value)) continue;
    shown[key] = key === "target" && value === `session:${sessionID}` ? `you (${value})` : value;
  }
  return shown;
}

/**
 * A copy of `value` with no field that states nothing (`dispatchPayloadForReader`), at any
 * depth: an object left with no field after its own fields went states nothing too. A field of
 * `value` itself that `keptWhole` names stays as it is. Nothing of `value` is written to. The walk
 * keeps the objects and lists still to copy on a stack of its own, so a payload too deep for the
 * card's encoder fails in the encoder, where the card shows it as text (`payloadShownAsText`), and
 * not here. `value` is `parseFrameJSON`'s, so no key of it is `__proto__` and each is set by
 * assignment.
 */
function withoutEmptyFields(
  value: unknown,
  keptWhole: (key: string, item: unknown) => boolean
): unknown {
  if (!isObject(value)) return value;
  // A list's indices are its keys, so one type holds each copy, list or object.
  const root = (Array.isArray(value) ? [] : {}) as Record<string, unknown>;
  const pending: [from: Readonly<Record<string, unknown>>, to: Record<string, unknown>][] = [
    [value, root],
  ];
  // Each object copied into an object's field, in the order the walk placed it: a field's object
  // is placed after the object that holds it, so the list read from its end sees every object
  // after the objects it holds.
  const placed: [holder: Record<string, unknown>, key: string, held: Record<string, unknown>][] =
    [];
  for (let next = pending.pop(); next !== undefined; next = pending.pop()) {
    const [from, to] = next;
    const inList = Array.isArray(from);
    for (const [key, item] of Object.entries(from)) {
      if (from === value && !inList && keptWhole(key, item)) {
        to[key] = item;
        continue;
      }
      const statesNothing =
        item === null ||
        item === undefined ||
        item === "" ||
        item === false ||
        (Array.isArray(item) && item.length === 0);
      if (!inList && statesNothing) continue;
      if (!isObject(item)) {
        to[key] = item;
        continue;
      }
      const copy = (Array.isArray(item) ? [] : {}) as Record<string, unknown>;
      pending.push([item, copy]);
      if (!inList && !Array.isArray(item)) placed.push([to, key, copy]);
      to[key] = copy;
    }
  }
  for (const [holder, key, held] of placed.reverse()) {
    if (Object.keys(held).length === 0) delete holder[key];
  }
  return root;
}

// A comment.created reply to an ask carries the question text (ask_question) alongside
// the ask id, so the frame names the question head the way an ask event does; `re:` stays
// the ask's ref.
function dispatchAskQuestion(comment: CommentPayload | undefined): string | undefined {
  return comment?.ask_question !== undefined && comment.ask_question !== ""
    ? textHead(comment.ask_question)
    : undefined;
}

// The thread a correlated frame continues, as a dispatch:// ref rather than the text it
// names: the ask an ask.* event or an ask reply (comment.created with ask_id) belongs to,
// or the message a message.* reply answers. A document-owned ask's ref needs the document
// slug, which only the comment payload or a document topic carries; a frame without either
// keeps the bare id Dispatch correlated it with, as does a frame of any other type.
function dispatchReplyRef(
  event: DispatchEvent,
  topic: string | undefined,
  inReplyTo: string,
  comment: CommentPayload | undefined
): string {
  let kind: "ask" | "message";
  let document = topicDocument(topic);
  if (event.type.startsWith("ask.")) {
    kind = "ask";
  } else if (event.type.startsWith("message.")) {
    kind = "message";
  } else if (event.type === "comment.created") {
    if (comment === undefined || comment.ask_id === undefined || comment.ask_id === null) {
      return inReplyTo;
    }
    kind = "ask";
    const { project_key: project, artifact_slug: slug } = comment;
    if (project !== undefined && project !== "" && slug !== undefined && slug !== "") {
      document = { project, slug };
    }
  } else {
    return inReplyTo;
  }
  if (event.issue_key !== null) {
    return dispatchChildRef(dispatchIssueRef(event.issue_key), kind, inReplyTo);
  }
  if (document === undefined) return inReplyTo;
  return dispatchChildRef(dispatchDocumentRef(document.project, document.slug), kind, inReplyTo);
}

// A Dispatch bus frame's JSON payload either matches the wire contract documented
// above (`event`) or it doesn't — invalid JSON, or a JSON value that disagrees with
// `DispatchEventSchema` — in which case the raw parsed value (object or string) is
// kept (`raw`) so no data is dropped and nothing renders as a hand-built text
// template.
type DispatchFrame =
  | {
      readonly event: DispatchEvent;
      readonly notify: unknown;
      readonly quiet: unknown;
      readonly delivery: unknown;
    }
  | {
      readonly raw: unknown;
      readonly rejectedDelivery?: DispatchDelivery;
      readonly malformedDelivery?: true;
    };

function isObject(value: unknown): value is Readonly<Record<string, unknown>> {
  return typeof value === "object" && value !== null;
}

/** `event` with the whole body of the item it carried only the first part of, or none of: the
 *  body the server answered, with the edited mark after it for an item edited since, or the
 *  deleted line for an item deleted since (`wholeCarriedBody`), and neither flag that said the
 *  body on the event was not the item's own (`bodyWasCut`). The event is left as it arrived when
 *  the caller read no body. */
function withWholeBody(event: DispatchEvent, body: string | undefined): DispatchEvent {
  if (body === undefined || !isObject(event.payload) || !bodyWasCut(event.payload)) return event;
  const { body_truncated: _cut, payload_omitted: _omitted, ...carried } = event.payload;
  return { ...event, payload: { ...carried, body } };
}

/** The frame one Dispatch envelope's payload carries. `wholeBody` is the body of the item the
 *  event named as cut, read from the server since (`renderInbound`'s `cutItem`); the event then
 *  reads as though it had carried that body whole. */
function parseDispatchFrame(rawPayload: string, wholeBody?: string): DispatchFrame {
  let value: unknown;
  try {
    value = parseFrameJSON(rawPayload);
  } catch {
    return { raw: rawPayload };
  }

  // A targeted frame requires `delivery`, so only a frame carrying the key can be one.
  if (isObject(value) && "delivery" in value) {
    const targeted = DispatchTargetedFrameSchema.safeParse(value);
    if (targeted.success) {
      // `notify` and `quiet` are fields of the event the frame wraps, which is where the
      // outbox marshals them, and the schema drops both as unknown keys.
      const wireFrame = value as {
        readonly event: DispatchEvent & { readonly notify?: unknown; readonly quiet?: unknown };
      };
      return {
        event: withWholeBody(targeted.data.event, wholeBody),
        notify: wireFrame.event.notify,
        quiet: wireFrame.event.quiet,
        delivery: targeted.data.delivery,
      };
    }
    const recoverable = RecoverableDispatchDeliveryFailureSchema.safeParse(value);
    if (recoverable.success) {
      const delivery = recoverable.data.delivery;
      const commentDelivery = isCommentTargetedDelivery(delivery);
      const resource = commentDelivery ? "comment" : "message";
      const id = commentDelivery ? delivery.comment_id : recoverable.data.event.payload.id;
      return {
        raw: value,
        rejectedDelivery: {
          resource,
          id,
          attempt: delivery.attempt,
          mode: delivery.mode,
          replyPath: dispatchReplyPath(resource, id),
          replyFields: commentDelivery ? { target: delivery.target } : {},
          issueKey: recoverable.data.event.issue_key,
          body: recoverable.data.event.payload.body ?? "",
        },
        malformedDelivery: true,
      };
    }
    return { raw: value, malformedDelivery: true };
  }

  const parsed = DispatchEventSchema.safeParse(value);
  if (!parsed.success) return { raw: value };
  const wireEvent = value as DispatchEvent & {
    readonly notify?: unknown;
    readonly quiet?: unknown;
  };
  return {
    event: withWholeBody(parsed.data, wholeBody),
    notify: wireEvent.notify,
    quiet: wireEvent.quiet,
    delivery: undefined,
  };
}

/**
 * The fewest bytes a card may lay a payload out in: the hub's largest frame (nats-server's
 * `max_payload`, 1 MiB). A card may hold more, up to twice the text it lays out (its frame and a
 * body read whole), since a body Dispatch's API gives whole comes from no frame. A layout
 * normally holds about as many bytes as the text it lays out, since TOON drops JSON's quotes and
 * braces and escapes no character JSON does not. Measured with Bun 1.4.4: the largest card laid
 * out in this package's tests is 7,007 bytes, of the 29 envelope fixtures in `@legion/contracts`
 * 2,632 bytes (from a 2,726-byte frame), a GitHub comment at GitHub's 65,536-character limit
 * 65,783 bytes, a 1 MiB frame of a table 478,316 bytes, and a Dispatch comment read whole near
 * Dispatch's 1 MiB request limit about 480 bytes more than its body. A layout past the bound is
 * indentation: an array chain 1,400 levels deep, a 3 KB frame, lays out in about 2 MB.
 */
export const INBOUND_CARD_MAX_BYTES = 1 << 20;

/** `value` as TOON, or undefined once its lines pass `maxBytes`. The encoder yields one line at a
 *  time, so a layout far larger than its value costs no more than the bound to refuse. */
function encodeWithin(value: unknown, maxBytes: number): string | undefined {
  const lines: string[] = [];
  // The newlines that join the lines: one fewer than the lines.
  let bytes = -1;
  for (const line of encodeLines(value)) {
    bytes += Buffer.byteLength(line) + 1;
    if (bytes > maxBytes) return undefined;
    lines.push(line);
  }
  return lines.join("\n");
}

export function inboundTimestamp(milliseconds: number | undefined): string {
  if (milliseconds === undefined) return "unknown";
  const date = new Date(Math.floor(milliseconds / 1_000) * 1_000);
  return Number.isNaN(date.getTime()) ? "unknown" : date.toISOString().replace(".000Z", "Z");
}

/** One inbound envelope's JSON as a card for the agent, with what the host does with it. A body
 *  is bounded block by block, so one event can carry its first part alone, or none of it, and
 *  the result names that item as `cutItem`: a host that reads it whole
 *  (`wholeCarriedBody`) renders again with the body in `wholeBody`, and the card then holds the
 *  body the comment or message route answers rather than one that stops mid-thought. */
export function renderInbound(
  raw: string,
  sessionID: string,
  subject?: string,
  wholeBody?: string
): RenderInboundResult {
  let value: unknown;
  try {
    value = parseFrameJSON(raw);
  } catch {
    return {
      skip: false,
      content: encode({
        envoy: { topic: subject ?? "unknown", unrecognised: "payload was not JSON" },
      }),
    };
  }

  const parsed = InboundEnvelopeSchema.safeParse(value);
  const parseIssues = parsed.success
    ? []
    : parsed.error.issues.map((issue) => issue.path.join(".") || "envelope");
  let envelope: InboundEnvelope;
  if (parsed.success) {
    envelope = parsed.data;
  } else {
    const tolerant = TolerantInboundEnvelopeSchema.safeParse(value);
    if (!tolerant.success) {
      return {
        skip: false,
        content: encode({ envoy: { unrecognised: parseIssues.join(", ") || "envelope" } }),
      };
    }
    const data = tolerant.data as Partial<InboundEnvelope>;
    envelope = { ...data, source: data.source ?? "unknown" };
  }
  let dispatchEvent: unknown;
  let dispatchActor: DispatchEvent["actor"] | undefined;
  let dispatchIssue: string | undefined;
  let dispatchReply: ReplyHint | undefined;
  let delivery: DispatchDelivery | undefined;
  let rejectedDelivery: DispatchDelivery | undefined;
  let malformedDelivery = false;
  let pictures: string[] = [];
  // The note a cut body earns on the rendered record (carriedBodyNote), and the item whose
  // whole body the host can read in its place; both undefined when the event carried the body
  // whole, and after a render that was given that body (wholeBody).
  let bodyNote: string | undefined;
  let cutItem: CarriedItem | undefined;
  let inReplyTo = envelope.in_reply_to;
  // The sessions the event's own payload names, which no spoof check flags: on an ask event the
  // ask's author, another follower's session when this session merely replied; on a reply in an
  // ask's thread the sessions that own the ask (`ask_sessions`), which every copy carries.
  let payloadSessions: readonly string[] = [];
  // Text that follows the card's TOON block: a person's edit's diff as a fenced `diff` block.
  let trailer = "";
  const at = inboundTimestamp(envelope.issued_at);
  // The session's inbox in either form, the session alone or under a project, names it the
  // recipient: every form of such a topic holds the old one (`topicForms`), no other session's.
  const toReader =
    envelope.topic !== undefined &&
    topicForms(envelope.topic).includes(legacyAgentSubject(sessionID));
  const dispatchRendered = envelope.source === "dispatch" && envelope.payload !== undefined;
  if (envelope.source === "dispatch") {
    const topic = subject ?? envelope.topic;
    if (envelope.payload === undefined) {
      dispatchIssue = "payload";
    } else {
      const frame = parseDispatchFrame(envelope.payload, wholeBody);
      if ("event" in frame) {
        // An owner topic carries every event of its issue, document or project, so a frame
        // carrying nothing to read is dropped here rather than shown or recorded. Dispatch
        // states that as `quiet` (`events.Quiet`): a delivery receipt, an anchor refresh,
        // settings or per-viewer bookkeeping, a targeted message its session already had, an ask
        // edit that only moved a version, or a version nobody named. `notify` is a different
        // question — whether to wake the issue's route and claimant — and it is false for every
        // event an agent wrote, so skipping on it dropped exactly the activity a session that
        // subscribed to `notifications.dispatch.issue.<KEY>.>` asked for
        // (skills/dispatch/SKILL.md). A frame with no `quiet` was published by a Dispatch from
        // before this field and is judged by the old rule: the stream still holds 72 hours of
        // them, and a replay must not start showing their receipts and autosaves.
        const quiet = frame.quiet === undefined ? frame.notify === false : frame.quiet === true;
        if (quiet && topic?.startsWith(DISPATCH_TOPIC_PREFIX) === true) {
          return { skip: true, content: "", envelope };
        }
        if (frame.event.actor.kind === "session" && frame.event.actor.id === sessionID) {
          return { skip: true, content: "", envelope };
        }
        // A meta-notice about Envoy plumbing, not a domain event: a short plain
        // sentence is higher signal here than the generic TOON envelope below.
        // This event also reaches the issue's own topic — every other subscriber,
        // not just the removed session — so it renders only for the session it
        // names; every other receiving session sees nothing (high signal).
        if (frame.event.type === "subscription.removed") {
          const removed = SubscriptionRemovedPayloadSchema.safeParse(frame.event.payload);
          if (!removed.success || removed.data.session_id !== sessionID) {
            return { skip: true, content: "", envelope };
          }
          const who = removed.data.by?.id ?? "someone";
          return {
            skip: false,
            content: `Unsubscribed from ${dispatchOwner(frame.event, topic)} by ${who}`,
            envelope,
          };
        }
        // Same shape for a human curating an ask's followers: only the session the
        // payload names hears it (the removed session on its own topic, an added one
        // wherever it listens); every other subscriber of the owner topic sees nothing.
        if (
          frame.event.type === "ask.follower_added" ||
          frame.event.type === "ask.follower_removed"
        ) {
          const follower = AskFollowerPayloadSchema.safeParse(frame.event.payload);
          if (!follower.success || follower.data.session_id !== sessionID) {
            return { skip: true, content: "", envelope };
          }
          const who = follower.data.by?.id ?? "someone";
          const owner = dispatchOwner(frame.event, topic);
          const ask = follower.data.ask_id ?? "?";
          return {
            skip: false,
            content:
              frame.event.type === "ask.follower_added"
                ? `Now following ask ${ask} on ${owner} (added by ${who}): its answer and replies reach you directly; ${commandLine("dispatch_follow", { ask, action: "unfollow" })} to stop.`
                : `No longer following ask ${ask} on ${owner} (removed by ${who}).`,
            envelope,
          };
        }
        const comment =
          frame.event.type === "comment.created"
            ? CommentPayloadSchema.safeParse(frame.event.payload)
            : undefined;
        const commentPayload = comment?.success === true ? comment.data : undefined;
        const ownsAsk = ownsRepliedAsk(commentPayload, sessionID);
        const compact = dispatchCompact(frame.event, commentPayload, ownsAsk);
        payloadSessions = commentPayload?.ask_sessions ?? [];
        if (frame.event.type.startsWith("ask.")) {
          const asked = AskAuthorPayloadSchema.safeParse(frame.event.payload);
          if (asked.success && asked.data.author.kind === "session") {
            payloadSessions = [asked.data.author.id];
          }
        }
        if (inReplyTo !== undefined) {
          inReplyTo = dispatchReplyRef(frame.event, topic, inReplyTo, commentPayload);
        }
        dispatchReply = dispatchCommentReplyWith(frame.event, topic, commentPayload);
        if (frame.event.type === "message.created") {
          const message = DispatchTargetedMessagePayloadSchema.safeParse(frame.event.payload);
          const requested = DispatchTargetedDeliverySchema.safeParse(frame.delivery);
          if (message.success && requested.success && !isCommentTargetedDelivery(requested.data)) {
            delivery = {
              resource: "message",
              id: message.data.id,
              attempt: requested.data.attempt,
              mode: requested.data.mode,
              replyPath: dispatchReplyPath("message", message.data.id),
              replyFields: {},
              issueKey: frame.event.issue_key,
              body: message.data.body,
              ...(message.data.body_truncated === true ? { bodyTruncated: true } : {}),
              ...(typeof message.data.broadcast_id === "string"
                ? { broadcastId: message.data.broadcast_id }
                : {}),
            };
            // A human's direct message to this session carries no issue key, and
            // `dispatch message` answers it with `--in-reply-to` alone; every other targeted
            // message names the issue the reply posts into.
            dispatchReply = {
              command: commandLine("dispatch_message", {
                ...(frame.event.issue_key === null ? {} : { issue: frame.event.issue_key }),
                in_reply_to: message.data.id,
                body: "...",
              }),
            };
          }
        } else if (frame.event.type === "comment.created") {
          const comment = DispatchTargetedCommentPayloadSchema.safeParse(frame.event.payload);
          const requested = DispatchTargetedDeliverySchema.safeParse(frame.delivery);
          if (comment.success && requested.success && isCommentTargetedDelivery(requested.data)) {
            delivery = {
              resource: "comment",
              id: requested.data.comment_id,
              attempt: requested.data.attempt,
              mode: requested.data.mode,
              replyPath: dispatchReplyPath("comment", requested.data.comment_id),
              replyFields: { target: requested.data.target },
              issueKey: frame.event.issue_key,
              body: comment.data.body,
              ...(comment.data.body_truncated === true ? { bodyTruncated: true } : {}),
            };
          }
        }
        // A reply in an ask's thread says what it is to this session, in the words the session's
        // answers list gives it, and carries the command that answers the ask only for a session
        // asked to answer: one that owns the ask, and one a mention named. Any other session the
        // reply reached (by the issue's route or claim, a credit on its document, a
        // subscription) is told why it has the reply, and given no command that answers the ask
        // as though it were its own.
        const mentioned = delivery !== undefined;
        const relation =
          ownsAsk === undefined || commentPayload === undefined
            ? undefined
            : sessionAnswerRelationPhrase(
                repliedAskRelation(frame.event, commentPayload, ownsAsk, mentioned)
              );
        if (ownsAsk === false && !mentioned) dispatchReply = undefined;
        // `owner` is the issue key, or `PROJECT / slug` for a document (the artifact id when
        // no document topic names it); nothing else in the block restates where the event is.
        // A compact ask record names its ask exactly once: through `re:` when the envelope is
        // correlated, else as `ask` here (openings and edits arrive uncorrelated).
        const compactRecord =
          compact === undefined
            ? undefined
            : inReplyTo !== undefined || compact.askId === undefined
              ? compact.record
              : {
                  ask: dispatchReplyRef(frame.event, topic, compact.askId, commentPayload),
                  ...compact.record,
                };
        const payload =
          compactRecord === undefined
            ? dispatchPayloadForReader(frame.event, sessionID, envelope.issued_at, toReader)
            : undefined;
        // A session's `origin` says where it runs (its host, machine, directory and tmux pane),
        // which a person finding the session reads and an agent does not: the card keeps the
        // session's title alone. `dispatchActor` keeps the actor whole for the host.
        let actor: unknown = frame.event.actor;
        if ("origin" in frame.event.actor) {
          const { origin, ...rest } = frame.event.actor;
          const title = isObject(origin) ? origin["session_title"] : undefined;
          actor =
            typeof title === "string" && title !== ""
              ? { ...rest, origin: { session_title: title } }
              : rest;
        }
        // An event on no issue and no document, as a direct message to this session is, has no
        // owner to name (`dispatchOwner` answers `unknown`), so the card names none.
        const owner = dispatchOwner(frame.event, topic);
        dispatchEvent = {
          ...(owner === "unknown" ? {} : { owner }),
          type: frame.event.type,
          actor,
          ...(relation === undefined ? {} : { relation }),
          // A payload left with no field says nothing, so the card prints no `payload:` for it.
          ...(compactRecord ??
            (isObject(payload) && Object.keys(payload).length === 0 ? {} : { payload })),
        };
        dispatchActor = frame.event.actor;
        bodyNote = carriedBodyNote(frame.event);
        if (bodyWasCut(frame.event.payload)) cutItem = carriedItem(frame.event);
        pictures = eventPictures(frame.event);
        if (frame.event.type === "artifact.human_edited") {
          const edit = ArtifactHumanEditedPayloadSchema.safeParse(frame.event.payload);
          if (edit.success) trailer = humanEditDiffBlock(edit.data.diff, edit.data.diff_truncated);
        }
      } else {
        dispatchEvent = frame.raw;
        rejectedDelivery = frame.rejectedDelivery;
        malformedDelivery = frame.malformedDelivery === true;
      }
    }
  }

  let parsedPayload: unknown;
  if (
    !dispatchRendered &&
    envelope.payload !== undefined &&
    (envelope.source === "github" || envelope.payload !== envelope.payload_summary)
  ) {
    try {
      parsedPayload = parseFrameJSON(envelope.payload);
    } catch {
      parsedPayload = envelope.payload;
    }
  }

  const payloadSummary = envelope.payload_summary ?? "unknown";
  const message =
    !dispatchRendered &&
    envelope.payload !== undefined &&
    envelope.payload !== envelope.payload_summary
      ? parsedPayload
      : undefined;
  const summaryIsHead =
    typeof message === "string" &&
    envelope.payload_summary !== undefined &&
    message.startsWith(
      envelope.payload_summary.endsWith("…")
        ? envelope.payload_summary.slice(0, -1)
        : envelope.payload_summary
    );
  // The summary is scanned before the payload, in index order, and the pattern spans no
  // newline, so this sees the same matches as scanning the two joined by "\n".
  let foreignSession: string | undefined;
  for (const part of [envelope.payload_summary ?? "", envelope.payload ?? ""]) {
    for (const match of part.matchAll(FOREIGN_SESSION_ID)) {
      if (
        match[0] !== envelope.source_session &&
        match[0] !== sessionID &&
        !payloadSessions.includes(match[0])
      ) {
        foreignSession = match[0];
        break;
      }
    }
    if (foreignSession !== undefined) break;
  }
  const role = envelope.sender?.roles?.[0];
  // A card that says the sender wants nothing back prints no ready-made reply. The two are one
  // statement to the reader, and a filled-in envoy_send under `expects_reply: false` is an
  // invitation to do the thing the line just said is not wanted. `optional` and `required` keep
  // the hint. This withholds a Dispatch frame's own reply command too: the one such frame carrying
  // `false` is a comment mention that joins no ask thread, and its card still names the comment,
  // so a reader who answers anyway has what it needs without being invited.
  const wantsNoReply = envelope.expects_reply === false;
  const reply = wantsNoReply ? undefined : (dispatchReply ?? replyWith(envelope));
  const sourceIssue =
    parseIssues.includes("source") || KNOWN_SOURCES[envelope.source] !== undefined
      ? undefined
      : `source=${envelope.source}`;
  const unrecognised =
    [
      ...parseIssues,
      ...(sourceIssue === undefined ? [] : [sourceIssue]),
      ...(dispatchIssue === undefined ? [] : [dispatchIssue]),
    ].join(", ") || undefined;
  const rendered = {
    ...(toReader ? { to: `you (${sessionID})` } : {}),
    from: senderLabel(envelope),
    at,
    id: envelope.event_id ?? "unknown",
    // The listener could not deliver this message when it was sent, so it kept it until this
    // session claimed the role. The time it was kept says how long it waited.
    ...(envelope.kept_at === undefined ? {} : { kept_at: inboundTimestamp(envelope.kept_at) }),
    ...(envelope.expires_at === undefined ? {} : { by: inboundTimestamp(envelope.expires_at) }),
    ...(envelope.urgency === undefined ? {} : { urgency: envelope.urgency }),
    ...(envelope.expects_reply === undefined ? {} : { expects_reply: envelope.expects_reply }),
    ...(inReplyTo === undefined ? {} : { re: inReplyTo }),
    ...(envelope.supersedes === undefined ? {} : { supersedes: envelope.supersedes }),
    ...(reply === undefined ? {} : { reply_with: reply }),
    ...(role === undefined || wantsNoReply
      ? {}
      : {
          reply_role: {
            tool: "envoy_publish",
            args: { topic: `notifications.role.${role}`, message: "..." },
          },
        }),
    ...(dispatchRendered
      ? { dispatch: dispatchEvent }
      : {
          ...(summaryIsHead ? {} : { summary: payloadSummary }),
          ...(message === undefined ? {} : { message }),
        }),
    ...(bodyNote === undefined ? {} : { body_note: bodyNote }),
    ...(foreignSession === undefined
      ? {}
      : {
          note: `body names session ${foreignSession}; the sender is ${envelope.source_session ?? "unknown"}`,
        }),
    ...(unrecognised === undefined ? {} : { unrecognised }),
  };

  // The card's TOON encoder recurses once per level of a value, so a payload nested some
  // thousands of levels deep overflows the stack (an object chain 7,000 deep does in Bun 1.4.4).
  // A throw out of here throws in every host's render and in the consumer's give-up alike, so
  // the frame would go back to the stream on every delivery and nobody would be told of it. And
  // each level indents every line below it, so a deep value the encoder does reach the bottom of
  // lays out in bytes that grow with the square of its depth: a 20 KB array chain made a 100 MB
  // card. Either way the payload is the text its sender sent, so the card shows it as that text,
  // as it shows a payload that is no JSON, and the host takes the frame. That text is bounded as
  // every message's is: by the frame, at most the hub's 1 MiB. A payload that already is text is
  // bounded so already, and a throw the payload is not behind is thrown as before.
  const payloadKey = dispatchRendered ? "dispatch" : "message";
  const payloadValue = dispatchRendered ? dispatchEvent : message;
  // A layout holds about as many bytes as the text it lays out, and a body Dispatch's API gave
  // whole (`wholeBody`) is text the frame did not carry, so a card may hold twice that text when
  // that is more than the hub's frame: a 1 MiB comment read whole still lays out whole. The body
  // counts as the card writes it (`escapeString`, TOON's own escaping), since TOON writes a
  // control character as six bytes: a body of them lays out in six times its own bytes.
  const cardBound = Math.max(
    INBOUND_CARD_MAX_BYTES,
    2 *
      (Buffer.byteLength(raw) +
        (wholeBody === undefined ? 0 : Buffer.byteLength(escapeString(wholeBody))))
  );
  let card: string | undefined;
  let payloadShownAsText: string | undefined;
  try {
    card =
      typeof payloadValue === "object" && payloadValue !== null
        ? encodeWithin({ envoy: rendered }, cardBound)
        : encode({ envoy: rendered });
  } catch (error) {
    if (envelope.payload === undefined || payloadValue === undefined) throw error;
    payloadShownAsText = messageFor(error);
  }
  if (card === undefined) {
    payloadShownAsText ??= `its layout is larger than the ${cardBound} bytes a card holds`;
    card = encode({ envoy: { ...rendered, [payloadKey]: envelope.payload } });
  }

  return {
    skip: false,
    content: trailer === "" ? card : `${card}\n\n${trailer}`,
    envelope,
    ...(dispatchActor === undefined ? {} : { dispatchActor }),
    ...(delivery === undefined ? {} : { delivery }),
    ...(rejectedDelivery === undefined ? {} : { rejectedDelivery }),
    ...(malformedDelivery ? { malformedDelivery: true as const } : {}),
    ...(pictures.length === 0 ? {} : { pictures }),
    ...(cutItem === undefined ? {} : { cutItem }),
    ...(payloadShownAsText === undefined ? {} : { payloadShownAsText }),
  };
}
