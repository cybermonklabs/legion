import {
  ArtifactCreatedEventPayloadSchema as ArtifactCreatedPayloadSchema,
  ArtifactHumanEditedEventPayloadSchema as ArtifactHumanEditedPayloadSchema,
  ArtifactVersionEventPayloadSchema as ArtifactVersionPayloadSchema,
  AskEditedEventPayloadSchema as AskEditedPayloadSchema,
  AskFollowerEventPayloadSchema as AskFollowerPayloadSchema,
  AskEventPayloadSchema as AskPayloadSchema,
  ChildStatusEventPayloadSchema as ChildStatusPayloadSchema,
  CommentEventPayloadSchema as CommentPayloadSchema,
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
  type SessionAnswerRelation,
  SubscriptionRemovedEventPayloadSchema as SubscriptionRemovedPayloadSchema,
  topicForms,
} from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";
import { textHead } from "./ask-answer";
import { commandLine } from "./dispatch-command";
import type { ActiveDispatchConfig } from "./dispatch-config";
import { DispatchClient } from "./dispatch-http";
import { dispatchChildRef, dispatchDocumentRef, dispatchIssueRef } from "./dispatch-owner";
import { pictureAddresses } from "./dispatch-pictures";
import { humanEditDiffBlock, humanEditHead } from "./human-edit";
import { sessionAnswerRelationPhrase } from "./session-answer-relations";
import { type NamedWords, wordsBlocks } from "./words-block";

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
  const client = new DispatchClient(config.url, config.credential);
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

export type DeliveryEnvelope = Pick<InboundEnvelope, "source" | "source_session" | "sender">;

export type RenderInboundResult = {
  readonly skip: boolean;
  readonly content: string;
  readonly envelope?: InboundEnvelope;
  /** Actor of a Dispatch event that passed the wire schema. */
  readonly dispatchActor?: DispatchEvent["actor"];
  /** Type of a Dispatch event that passed the wire schema (`frame.event.type`): with `delivery`,
   *  what a host reads to tell an event's card from the message or mention Dispatch delivers to
   *  the session, and an ask's card (`ask.*`) from the rest. */
  readonly dispatchType?: string;
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
 *  which belongs to no issue, is read through the session it was delivered to. */
export async function wholeCarriedBody(
  client: DispatchClient,
  item: CarriedItem,
  session?: string
): Promise<string | undefined> {
  try {
    if (item.resource === "comment") {
      const read = await client.getComment(item.id);
      return [read.comment, ...(read.replies ?? [])].find((comment) => comment.id === item.id)
        ?.body;
    }
    const read =
      item.issueKey === null
        ? session === undefined
          ? undefined
          : await client.getMessageThread(item.id, session)
        : await client.getMessage(item.issueKey, item.id);
    return read === undefined
      ? undefined
      : [read.message, ...(read.replies ?? [])].find((message) => message.id === item.id)?.body;
  } catch {
    return undefined;
  }
}

export function senderLabel(envelope: DeliveryEnvelope): string {
  const sender = sendingSession(envelope) ?? envelope.source ?? "unknown";
  return envelope.sender?.title === undefined ? sender : `${sender} (${envelope.sender.title})`;
}

/** The session that sent `envelope`: the `source_session` its body names, else the session the
 *  listener's sender stamp names (`sender.session_id`), which is the session a broker token
 *  proved where the body named none. Undefined where neither names one: only the Legion daemon's
 *  own publish names no session. */
function sendingSession(envelope: DeliveryEnvelope): string | undefined {
  const session = envelope.source_session || envelope.sender?.session_id;
  return session === undefined || session === "" ? undefined : session;
}

/** Whether the Legion daemon sent `envelope`: its source is `agent`, and it names no session,
 *  neither as `source_session` nor in the listener's sender stamp (`sendingSession`). A message of
 *  a session names that session in one of the two. */
export function daemonSent(envelope: DeliveryEnvelope): boolean {
  return envelope.source === "agent" && sendingSession(envelope) === undefined;
}

/** The `dispatch` command line that answers a Dispatch event, which its card prints. */
type ReplyCommand = { readonly command: string };

/** An author or an actor that a Dispatch payload names: an object with a kind and an id, or the
 *  login of a person, as an answer names who answered. */
function actorOf(value: unknown): { readonly kind: string; readonly id: string } | undefined {
  if (typeof value === "string") return value === "" ? undefined : { kind: "user", id: value };
  if (!isObject(value)) return undefined;
  const { kind, id } = value;
  return typeof kind === "string" && typeof id === "string" ? { kind, id } : undefined;
}

/** How a block names the author of its words: `<kind>:<id>`, the form the daemon's texts name an
 *  actor in. Undefined where the event's actor, which the card's fields name, is that author, and
 *  where the payload names no author. */
function authorBy(author: unknown, actor: DispatchEvent["actor"]): string | undefined {
  const who = actorOf(author);
  if (who === undefined || (who.kind === actor.kind && who.id === actor.id)) return undefined;
  return `${who.kind}:${who.id}`;
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
  "suggestion.accepted": CommentPayloadSchema,
  "suggestion.rejected": CommentPayloadSchema,
  "message.created": MessagePayloadSchema,
  "message.delivery": MessageDeliveryPayloadSchema,
  "message.answered": MessagePayloadSchema,
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
): ReplyCommand | undefined {
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
// producer correlates answers and replies, not openings or edits). `words` are the words of a
// person's act that the event carries (a reply, an answer, a resolution's reason), which the card
// draws in blocks after its fields.
type CompactDispatchRecord = Readonly<Record<string, string | readonly string[]>>;
interface CompactDispatch {
  readonly record: CompactDispatchRecord;
  readonly askId: string | undefined;
  readonly words: readonly NamedWords[];
}

/** The block of the words `text` of the field `name`, which `author` wrote at `at`; none where the
 *  event carried no words. The block names the author only where the event's actor is not that
 *  author (`authorBy`), and gives the words' `field` path where the field is inside an object of
 *  the payload. */
function said(
  name: string,
  text: string | null | undefined,
  author: unknown,
  at: string | undefined,
  actor: DispatchEvent["actor"],
  field?: string
): NamedWords[] {
  if (text === null || text === undefined || text === "") return [];
  return [{ name, words: { text, by: authorBy(author, actor), at, field } }];
}

function dispatchCompact(
  event: DispatchEvent,
  comment: CommentPayload | undefined,
  createdAt: string | undefined,
  ownsAsk: boolean | undefined
): CompactDispatch | undefined {
  if (event.type === "artifact.human_edited") {
    // A person's edit names its document, versions and editors in the card; its diff follows the
    // card as a fenced block (humanEditDiffBlock), where a reader sees it as the diff it is.
    const edit = ArtifactHumanEditedPayloadSchema.safeParse(event.payload);
    if (!edit.success) return undefined;
    return { askId: undefined, record: { edit: humanEditHead(edit.data) }, words: [] };
  }
  if (event.type === "comment.created") {
    if (comment?.ask_id === undefined || comment.ask_id === null) return undefined;
    const question = dispatchAskQuestion(comment);
    // `state` tells a clarification on an open ask from discussion after the answer;
    // `waiting_on` says whose move it is now, which is a move for the ask's own sessions alone
    // (`ownsRepliedAsk`): a session the reply reached for another reason is shown no turn. The
    // record names no time, so the reply's block gives the comment's own.
    return {
      askId: comment.ask_id,
      record: {
        ...(question === undefined ? {} : { question }),
        ...(comment.ask_state === undefined ? {} : { state: comment.ask_state }),
        ...(comment.ask_waiting_on === undefined || ownsAsk === false
          ? {}
          : { waiting_on: comment.ask_waiting_on }),
      },
      words: said(
        "reply",
        comment.body,
        comment.author,
        comment.created_at ?? createdAt,
        event.actor
      ),
    };
  }
  if (!event.type.startsWith("ask.")) return undefined;
  const ask = (event.type === "ask.edited" ? AskEditedPayloadSchema : AskPayloadSchema).safeParse(
    event.payload
  );
  if (!ask.success) return undefined;
  let record: CompactDispatchRecord = { question: textHead(ask.data.question ?? "") };
  let words: readonly NamedWords[] = [];
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
      // The options an answer chose are labels of the ask's own, so they stay fields; the words
      // the person typed go in a block. The schema keeps neither who gave the answer nor when, so
      // the block reads both from the payload as the event carried it.
      const answer = ask.data.answer;
      const given: Readonly<Record<string, unknown>> =
        isObject(event.payload) && isObject(event.payload["answer"]) ? event.payload["answer"] : {};
      const givenAt = given["at"];
      const previous = ask.data.previous_answer;
      const selected = (answer?.selected ?? []).filter((label) => label !== "");
      const previousSelected = (previous?.selected ?? []).filter((label) => label !== "");
      record = {
        ...record,
        ...(selected.length === 0 ? {} : { selected }),
        ...(previousSelected.length === 0 ? {} : { previous_selected: previousSelected }),
      };
      words = [
        ...said(
          "answer",
          answer?.text,
          given["user"],
          typeof givenAt === "string" && givenAt !== "" ? givenAt : createdAt,
          event.actor
        ),
        ...said("previous_answer", previous?.text, previous?.user, previous?.at, event.actor),
      ];
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
      // The kind, and the words Dispatch wrote for the asking agent beside a decline, are
      // Dispatch's, so they stay fields, where nobody reads Dispatch's instruction as the
      // person's note; the reason is the words of whoever closed the ask, in a block.
      const resolution = ask.data.resolution;
      if (resolution === null || resolution === undefined) break;
      const dispatchWords = resolution.dispatch_words;
      record = {
        ...record,
        resolved: resolution.kind ?? "resolved",
        ...(dispatchWords === undefined || dispatchWords === ""
          ? {}
          : { dispatch_words: dispatchWords }),
      };
      words = said(
        "reason",
        resolution.reason,
        resolution.actor,
        resolution.at ?? createdAt,
        event.actor
      );
      break;
    }
    default:
      break;
  }
  return { askId: ask.data.id, record, words };
}

function dispatchPayload(event: DispatchEvent): unknown {
  const schema = DISPATCH_PAYLOAD_SCHEMAS[event.type];
  if (schema === undefined) return event.payload;
  const parsed = schema.safeParse(event.payload);
  return parsed.success ? parsed.data : event.payload;
}

function hasTarget(value: unknown): value is { readonly target: unknown } {
  return isObject(value) && "target" in value;
}

function dispatchPayloadForReader(event: DispatchEvent, sessionID: string): unknown {
  const payload = dispatchPayload(event);
  if (!hasTarget(payload)) return payload;
  const target = payload.target;
  return target === `session:${sessionID}` ? { ...payload, target: `you (${target})` } : payload;
}

/**
 * The payload a card prints of a comment, a suggestion or a message, and the blocks of its author's
 * words: its body, and the text a suggestion proposes (`suggestion.replace_with`). Those words
 * leave the payload, and so does a suggestion that holds nothing else. The author leaves it too,
 * and each block names the author where the event's actor is someone else (`authorBy`). A block
 * gives the time of the event where the payload gives none: a message payload carries none.
 */
function payloadWords(
  event: DispatchEvent,
  payload: unknown,
  createdAt: string | undefined
): { readonly payload: unknown; readonly words: readonly NamedWords[] } {
  const carriesBody = ["comment.", "suggestion.", "message."].some((prefix) =>
    event.type.startsWith(prefix)
  );
  if (!carriesBody || !isObject(payload) || Array.isArray(payload)) return { payload, words: [] };
  const { author, body, suggestion } = payload;
  const suggested: Readonly<Record<string, unknown>> =
    isObject(suggestion) && !Array.isArray(suggestion) ? suggestion : {};
  const { replace_with: proposed } = suggested;
  const drawsBody = typeof body === "string" && body !== "";
  const drawsProposed = typeof proposed === "string" && proposed !== "";
  if (!drawsBody && !drawsProposed) return { payload, words: [] };
  const named = actorOf(author) !== undefined;
  const rest = Object.fromEntries(
    Object.entries(payload).flatMap(([key, value]): [string, unknown][] => {
      if ((key === "body" && drawsBody) || (key === "author" && named)) return [];
      if (key !== "suggestion" || !drawsProposed) return [[key, value]];
      const left = Object.entries(suggested).filter(([field]) => field !== "replace_with");
      return left.length === 0 ? [] : [[key, Object.fromEntries(left)]];
    })
  );
  const at = typeof rest["created_at"] === "string" ? undefined : createdAt;
  return {
    payload: rest,
    words: [
      ...(drawsBody ? said("body", body, author, at, event.actor) : []),
      ...(drawsProposed
        ? said("replace_with", proposed, author, at, event.actor, "suggestion.replace_with")
        : []),
    ],
  };
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
      /** When the event happened (its `created_at`), which the event schema does not keep. */
      readonly createdAt: string | undefined;
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
 *  body the server answered, and neither flag that said the body on the event was not the
 *  item's own (`bodyWasCut`). The event is left as it arrived when the caller read no body. */
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
    value = JSON.parse(rawPayload);
  } catch {
    return { raw: rawPayload };
  }

  // A targeted frame requires `delivery`, so only a frame carrying the key can be one.
  if (isObject(value) && "delivery" in value) {
    const targeted = DispatchTargetedFrameSchema.safeParse(value);
    if (targeted.success) {
      // `notify`, `quiet` and `created_at` are fields of the event the frame wraps, which is where
      // the outbox marshals them, and the schema drops all three as unknown keys.
      const wireFrame = value as { readonly event: WireEvent };
      return {
        event: withWholeBody(targeted.data.event, wholeBody),
        notify: wireFrame.event.notify,
        quiet: wireFrame.event.quiet,
        delivery: targeted.data.delivery,
        createdAt:
          typeof wireFrame.event.created_at === "string" ? wireFrame.event.created_at : undefined,
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
  const wireEvent = value as WireEvent;
  return {
    event: withWholeBody(parsed.data, wholeBody),
    notify: wireEvent.notify,
    quiet: wireEvent.quiet,
    delivery: undefined,
    createdAt: typeof wireEvent.created_at === "string" ? wireEvent.created_at : undefined,
  };
}

/** An event as the outbox marshals it, with the fields the event schema drops. */
type WireEvent = DispatchEvent & {
  readonly notify?: unknown;
  readonly quiet?: unknown;
  readonly created_at?: unknown;
};

export function inboundTimestamp(milliseconds: number | undefined): string {
  if (milliseconds === undefined) return "unknown";
  const date = new Date(Math.floor(milliseconds / 1_000) * 1_000);
  return Number.isNaN(date.getTime()) ? "unknown" : date.toISOString().replace(".000Z", "Z");
}

/** A value that starts as an RFC 3339 time does. */
const TIME = /^\d{4}-\d{2}-\d{2}T/;

/** A segment of a words path (`WORDS_PATH`): a field's name that holds no `.`, `[`, `]`, white
 *  space, control character, `<`, `>`, `"`, `'` or `&`. */
const PATH_SEGMENT = /^[^.[\]\s\p{Cc}<>"'&]+$/u;

/** A key of a daemon payload's `words` object: the path of a field in the payload, its segments
 *  (`PATH_SEGMENT`) joined by `.`, each followed by the index of a list's item in brackets for
 *  each list the path enters there, as `summary` or `catch_up.kept.decisions[1].answer`. */
const WORDS_PATH =
  /^[^.[\]\s\p{Cc}<>"'&]+(?:\[(?:0|[1-9][0-9]*)\])*(?:\.[^.[\]\s\p{Cc}<>"'&]+(?:\[(?:0|[1-9][0-9]*)\])*)*$/u;

/** One key of a words path (`WORDS_PATH`): a field's name, or the index of a list's item in
 *  brackets. */
const PATH_KEY = /([^.[\]]+)|\[(\d+)\]/g;

/**
 * What a card shows of a notice or a role message of the Legion daemon published as an object:
 * its fields in the payload's key order, and a block for each field that holds another author's
 * words. The payload's `words` object names those fields by their path in the payload
 * (`WORDS_PATH`: `summary`, `catch_up.kept.decisions[1].answer`) with their author and time
 * (`texts.WordsPaths`, packages/daemon/internal/texts/words.go). Each such field, a string,
 * leaves the fields for a block of its own, in the payload's order, and `words` itself is not
 * shown. The block's name is the field's name with each character a tag's name cannot hold
 * written as `_` (`blockName`), so a block takes a tag of its own, and its `field` attribute gives
 * the path where that name is not the path. A path whose field the payload leaves out, as a
 * stand-in the bus took with its long fields dropped leaves out an answer, draws a block with no
 * words after those, with its entry's mark that more follows. A block names the author and the
 * time only where no field of the objects that hold the words names them, and where the object
 * that holds the words' field, when that is no field of the payload itself, holds no `by` (for
 * the author) or `at` (for the time) of its own: the pause of a project-paused notice names who
 * paused and when (`pause.by`, `pause.at`), and so its reason's block names neither. By the same
 * rule a block says that more follows only where that object says no `more` of its own: a
 * catch-up's kept decision says it in its row (texts.DecisionMark), so its answer's block does
 * not. The row is where every decision says it, also one whose words the event left out, which
 * no block draws. What the holders name is read once for each holder, however many words it
 * holds. A `role` that names the reader (the card's topic ends in `-<role>`) is not shown: the
 * reader knows its own role. A payload with no `words` object, as a daemon before that object
 * writes, shows every field, and so does a payload whose `words` object has a key that is no
 * path, or whose path names a list's item or a field that holds no string: the daemon writes no
 * such key, and its words have no block to go in.
 */
function daemonFields(
  payload: Readonly<Record<string, unknown>>,
  topics: readonly (string | undefined)[]
): { readonly fields: unknown; readonly words: readonly NamedWords[] } {
  const named = payload["words"];
  const paths = isObject(named) && !Array.isArray(named) ? named : undefined;
  // `Object.fromEntries` defines each key as an own field, `__proto__` too: JSON.parse makes it an
  // own key, and an assignment would take it as the object's prototype.
  const visible = Object.fromEntries(
    Object.entries(payload).filter(
      ([key, value]) =>
        !(
          key === "role" &&
          typeof value === "string" &&
          topics.some((topic) => topic?.endsWith(`-${value}`) === true)
        )
    )
  );
  const every = { fields: visible, words: [] };
  if (paths === undefined) return every;
  const keyed: (readonly [string, readonly (string | number)[]])[] = [];
  for (const path of Object.keys(paths)) {
    if (!WORDS_PATH.test(path)) return every;
    keyed.push([
      path,
      Array.from(path.matchAll(PATH_KEY), ([, key, index]) => key ?? Number(index)),
    ]);
  }
  const root = Object.fromEntries(Object.entries(visible).filter(([key]) => key !== "words"));
  const taken: TakenWords[] = [];
  const fields = withoutWords(root, "", paths, [], taken);
  const found = new Set(taken.map(({ path }) => path));
  for (const [path, keys] of keyed) {
    if (found.has(path)) continue;
    const name = keys.at(-1);
    if (typeof name !== "string" || fieldAt(root, keys).found !== undefined) return every;
    taken.push({ path, name, text: "", holders: fieldAt(fields, keys).holders });
  }
  const names = holderNames();
  const words = taken.map(({ path, name, text, holders }): NamedWords => {
    const entry = paths[path];
    const meta: Readonly<Record<string, unknown>> = isObject(entry) ? entry : {};
    const { by, at, rest, more } = meta;
    const time = typeof at === "string" && TIME.test(at) ? Date.parse(at) : Number.NaN;
    const own = holders.length > 1 ? holders[holders.length - 1] : undefined;
    const byShown =
      (own !== undefined && namesOwn(own, "by")) ||
      holders.some((holder) => names(holder).values.has(by));
    const timeShown =
      (own !== undefined && namesOwn(own, "at")) ||
      holders.some((holder) => {
        const { values, times } = names(holder);
        return values.has(at) || (!Number.isNaN(time) && times.has(time));
      });
    const moreShown = own !== undefined && Object.hasOwn(own, "more") && own["more"] === true;
    const block = blockName(name);
    return {
      name: block,
      words: {
        text,
        by: typeof by === "string" && !byShown ? by : undefined,
        at: typeof at === "string" && !timeShown ? at : undefined,
        field: path === block ? undefined : path,
        rest: typeof rest === "number" && Number.isInteger(rest) ? rest : undefined,
        more: more === true && !moreShown,
      },
    };
  });
  return { fields, words };
}

/** What the fields of one object that holds words name: their values, and the instant of each
 *  value that starts as an RFC 3339 time does (`TIME`). */
interface HolderNames {
  readonly values: ReadonlySet<unknown>;
  readonly times: ReadonlySet<number>;
}

/** What each object that holds words names (`HolderNames`), read the first time it is asked of
 *  and kept, so the fields of an object are read once however many words it holds. */
function holderNames(): (holder: Readonly<Record<string, unknown>>) => HolderNames {
  const read = new Map<Readonly<Record<string, unknown>>, HolderNames>();
  return (holder) => {
    let names = read.get(holder);
    if (names === undefined) {
      const values = new Set(Object.values(holder));
      const times = new Set<number>();
      for (const value of values) {
        if (typeof value !== "string" || !TIME.test(value)) continue;
        const time = Date.parse(value);
        if (!Number.isNaN(time)) times.add(time);
      }
      names = { values, times };
      read.set(holder, names);
    }
    return names;
  };
}

/** Whether `holder` has a field `key` that names something: a value that is neither null nor
 *  empty. */
function namesOwn(holder: Readonly<Record<string, unknown>>, key: string): boolean {
  const value = Object.hasOwn(holder, key) ? holder[key] : undefined;
  return value !== undefined && value !== null && value !== "";
}

/** The name of the block of the field `name`, a segment of a words path (`PATH_SEGMENT`): `name`
 *  with each character that is no ASCII letter, digit or `_` written as `_`, and a `_` before a
 *  leading digit. That is a name a tag can have (`wordsBlocks`), and it holds no `-`, which joins
 *  a tag's suffix to its name, so no block's name is another block's tag with a suffix. */
function blockName(name: string): string {
  const written = name.replace(/[^A-Za-z0-9_]/gu, "_");
  return /^[0-9]/.test(written) ? `_${written}` : written;
}

/** The objects along `keys` in `value`, from `value` to the object of the last key, and the value
 *  of the last key: undefined where `value` leaves it out. */
function fieldAt(
  value: unknown,
  keys: readonly (string | number)[]
): { readonly holders: Readonly<Record<string, unknown>>[]; readonly found: unknown } {
  const holders: Readonly<Record<string, unknown>>[] = [];
  let at = value;
  for (const key of keys) {
    if (!isObject(at)) return { holders, found: undefined };
    if (!Array.isArray(at)) holders.push(at);
    at = Object.hasOwn(at, key) ? at[key] : undefined;
  }
  return { holders, found: at };
}

/** A field that holds another author's words, taken out of a daemon payload's fields: its path,
 *  its name (the path's last segment), and its words. */
interface TakenWords {
  readonly path: string;
  readonly name: string;
  readonly text: string;
  /** The objects that hold the words, from the payload's own fields to the words' object. */
  readonly holders: readonly Readonly<Record<string, unknown>>[];
}

/** `value`, whose path in the payload is `path`, less each string field that `paths` names,
 *  each of which goes to `taken`. `path` is undefined where no words path reaches `value`, as
 *  below a field whose name is no segment of one (`PATH_SEGMENT`). `holders` are the objects
 *  that hold `value`. */
function withoutWords(
  value: unknown,
  path: string | undefined,
  paths: Readonly<Record<string, unknown>>,
  holders: readonly Readonly<Record<string, unknown>>[],
  taken: TakenWords[]
): unknown {
  if (Array.isArray(value)) {
    return value.map((item, index) =>
      withoutWords(
        item,
        path === undefined ? undefined : `${path}[${index}]`,
        paths,
        holders,
        taken
      )
    );
  }
  if (!isObject(value)) return value;
  const kept: Record<string, unknown> = {};
  const chain = [...holders, kept];
  for (const [key, item] of Object.entries(value)) {
    let at: string | undefined;
    if (path !== undefined && PATH_SEGMENT.test(key)) at = path === "" ? key : `${path}.${key}`;
    if (typeof item === "string" && at !== undefined && Object.hasOwn(paths, at)) {
      taken.push({ path: at, name: key, text: item, holders: chain });
    } else {
      // Defined, not assigned: an assignment of `__proto__` would set the prototype of `kept`.
      Object.defineProperty(kept, key, {
        value: withoutWords(item, at, paths, chain, taken),
        enumerable: true,
        writable: true,
        configurable: true,
      });
    }
  }
  return kept;
}

/** A line that Go's `strings.TrimSpace` leaves empty: Unicode White_Space alone, which U+0085
 *  holds and JavaScript's `trim` does not (and U+FEFF, which `trim` removes, is not). */
const BLANK_LINE = /^\p{White_Space}*$/u;

/** Whether `summary` opens `text`: the first line of `text` that is not blank, less the white
 *  space before it, begins with the summary less its closing ellipsis. That line is the one
 *  Envoy's listener makes a message's summary of (`OneLineSummary`,
 *  packages/envoy/internal/contracts/normalize.go), which reads a line as blank as Go's
 *  `strings.TrimSpace` does (`BLANK_LINE`). */
function summaryOpens(text: string, summary: string | undefined): boolean {
  if (summary === undefined) return false;
  const head = summary.endsWith("…") ? summary.slice(0, -1) : summary;
  // A payload can be long, so the scan stops at its first line that is not blank.
  let start = 0;
  let end = text.indexOf("\n");
  while (end !== -1 && BLANK_LINE.test(text.slice(start, end))) {
    start = end + 1;
    end = text.indexOf("\n", start);
  }
  const line = end === -1 ? text.slice(start) : text.slice(start, end);
  const first = BLANK_LINE.test(line) ? "" : line;
  return first.replace(/^[ \t\r]+/, "").startsWith(head);
}

/**
 * One inbound envelope's JSON as a card for the agent, with what the host does with it.
 *
 * The card is TOON under `envoy:`, each field once, and then a block (`wordsBlocks`) for each part
 * of it that another author wrote: a session's message, JSON text too, the body, reply, answer or
 * reason of a Dispatch event and the text a suggestion proposes, and each field a daemon notice
 * names in its `words` object. A person's edit of a document has its diff after the card as a
 * fenced block instead. A card from another session names the sender's session and title, each
 * role the sender holds, the time and the message id, and carries no reply command: the
 * descriptions of `envoy_send` and `envoy_publish` say how to reply from those fields. A Dispatch
 * card keeps its reply command.
 *
 * A body is bounded block by block, so one event can carry its first part alone, or none of it, and
 * the result names that item as `cutItem`: a host that reads it whole (`wholeCarriedBody`) renders
 * again with the body in `wholeBody`, and the card then holds the body the comment or message route
 * answers rather than one that stops mid-thought.
 */
export function renderInbound(
  raw: string,
  sessionID: string,
  subject?: string,
  wholeBody?: string
): RenderInboundResult {
  let value: unknown;
  try {
    value = JSON.parse(raw);
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
  let dispatchType: string | undefined;
  let dispatchIssue: string | undefined;
  let dispatchReply: ReplyCommand | undefined;
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
  // The words another author wrote that follow the card's fields, each in a block of its own.
  const blocks: NamedWords[] = [];
  // Text that follows the card's TOON block: a person's edit's diff as a fenced `diff` block.
  let trailer = "";
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
            dispatchType: frame.event.type,
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
            dispatchType: frame.event.type,
          };
        }
        const comment =
          frame.event.type === "comment.created"
            ? CommentPayloadSchema.safeParse(frame.event.payload)
            : undefined;
        const commentPayload = comment?.success === true ? comment.data : undefined;
        const ownsAsk = ownsRepliedAsk(commentPayload, sessionID);
        const compact = dispatchCompact(frame.event, commentPayload, frame.createdAt, ownsAsk);
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
        const shown =
          compact === undefined
            ? payloadWords(
                frame.event,
                dispatchPayloadForReader(frame.event, sessionID),
                frame.createdAt
              )
            : { payload: undefined, words: compact.words };
        dispatchEvent = {
          owner: dispatchOwner(frame.event, topic),
          type: frame.event.type,
          actor: frame.event.actor,
          ...(relation === undefined ? {} : { relation }),
          ...(compactRecord ?? { payload: shown.payload }),
        };
        blocks.push(...shown.words);
        dispatchActor = frame.event.actor;
        dispatchType = frame.event.type;
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

  // What a card shows of a payload that is not a Dispatch event's. A message of another session
  // (`sendingSession`: its `source_session`, or the session the listener's sender stamp names) is
  // its sender's words, so it goes in a block whole, JSON text too: no key of it reads as a field
  // of the card. A notice or a role message of the Legion daemon (`agent`, with no session) shows
  // its fields as lines (`daemonFields`), and one it publishes as text (the hook prompt) goes in a
  // block. Every other source shows its payload as one `message` field.
  const sender = sendingSession(envelope);
  const sessionSent = envelope.source !== "dispatch" && sender !== undefined;
  const fromDaemon = daemonSent(envelope);
  let message: unknown;
  let messageText: string | undefined;
  if (sessionSent) {
    // The listener carries a message of one line as its summary alone.
    messageText =
      envelope.payload === undefined || envelope.payload === envelope.payload_summary
        ? envelope.payload_summary
        : envelope.payload;
  } else if (
    !dispatchRendered &&
    envelope.payload !== undefined &&
    envelope.payload !== envelope.payload_summary
  ) {
    let parsedPayload: unknown;
    try {
      parsedPayload = JSON.parse(envelope.payload);
    } catch {
      parsedPayload = envelope.payload;
    }
    if (fromDaemon && isObject(parsedPayload) && !Array.isArray(parsedPayload)) {
      const shown = daemonFields(parsedPayload, [envelope.topic, subject]);
      message = shown.fields;
      blocks.push(...shown.words);
    } else if (fromDaemon && !isObject(parsedPayload)) {
      messageText = typeof parsedPayload === "string" ? parsedPayload : envelope.payload;
    } else {
      message = parsedPayload;
    }
  }
  if (messageText !== undefined) blocks.push({ name: "message", words: { text: messageText } });
  const payloadSummary = envelope.payload_summary ?? "unknown";
  const headText = messageText ?? (typeof message === "string" ? message : undefined);
  const summaryIsHead = headText !== undefined && summaryOpens(headText, envelope.payload_summary);
  // Each session that the summary or the payload names, other than the sender, the reader and the
  // sessions the event's own payload names (`payloadSessions`), in the order they come: the
  // summary's first. A daemon card names none: the daemon names a session in a field or in a
  // worker's words on purpose, never as a misrouted message does.
  const namedSessions: string[] = [];
  for (const part of fromDaemon ? [] : [envelope.payload_summary ?? "", envelope.payload ?? ""]) {
    for (const [id] of part.matchAll(FOREIGN_SESSION_ID)) {
      if (id !== sender && id !== sessionID && !payloadSessions.includes(id)) {
        namedSessions.push(id);
      }
    }
  }
  const roles = [...new Set(envelope.sender?.roles ?? [])].filter((role) => role !== "");
  // A Dispatch card that says the sender wants nothing back prints no reply command. The two are
  // one statement to the reader, and a filled-in command under `expects_reply: false` is an
  // invitation to do the thing the line just said is not wanted. `optional` and `required` keep
  // it. The one Dispatch frame carrying `false` is a comment mention that joins no ask thread, and
  // its card still names the comment, so a reader who answers anyway has what it needs without
  // being invited.
  const reply = envelope.expects_reply === false ? undefined : dispatchReply;
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
  const shownFields = {
    // The session's inbox in either form, the session alone or under a project, names it the
    // recipient: every form of such a topic holds the old one (`topicForms`), no other session's.
    ...(envelope.topic !== undefined &&
    topicForms(envelope.topic).includes(legacyAgentSubject(sessionID))
      ? { to: "you" }
      : {}),
    from: senderLabel(envelope),
    ...(roles.length === 0 ? {} : roles.length === 1 ? { role: roles[0] } : { roles }),
    at: inboundTimestamp(envelope.issued_at),
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
    ...(dispatchRendered
      ? { dispatch: dispatchEvent }
      : {
          ...(summaryIsHead ? {} : { summary: payloadSummary }),
          ...(message === undefined ? {} : { message }),
        }),
    ...(bodyNote === undefined ? {} : { body_note: bodyNote }),
  };
  const issues = unrecognised === undefined ? {} : { unrecognised };
  let fields = encode({ envoy: { ...shownFields, ...issues } });
  // The note names a session that the card's words name and its fields do not, as a message sent
  // to the wrong session names the one it was for. A session that a field names already, such as
  // a Dispatch event's actor, is not named twice.
  const foreignSession = namedSessions.find((id) => !fields.includes(id));
  if (foreignSession !== undefined) {
    const note = `body names session ${foreignSession}`;
    fields = encode({ envoy: { ...shownFields, note, ...issues } });
  }
  // Each block's tag is chosen against the fields as well as the words of every block: the fields
  // hold other authors' words too (an ask's question, an issue's title in a catch-up).
  const card = blocks.length === 0 ? fields : `${fields}\n${wordsBlocks(fields, blocks)}`;
  return {
    skip: false,
    content: trailer === "" ? card : `${card}\n\n${trailer}`,
    envelope,
    ...(dispatchActor === undefined ? {} : { dispatchActor }),
    ...(dispatchType === undefined ? {} : { dispatchType }),
    ...(delivery === undefined ? {} : { delivery }),
    ...(rejectedDelivery === undefined ? {} : { rejectedDelivery }),
    ...(malformedDelivery ? { malformedDelivery: true as const } : {}),
    ...(pictures.length === 0 ? {} : { pictures }),
    ...(cutItem === undefined ? {} : { cutItem }),
  };
}
