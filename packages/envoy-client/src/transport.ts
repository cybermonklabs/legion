import {
  DELIVERY_DUPLICATE_WINDOW_HOURS,
  type Envelope,
  EnvelopeSchema,
  type ExpectsReply,
  ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS,
  LISTENER_CALL_ATTEMPTS,
  LISTENER_CALL_RETRY_DELAY_MS,
  LISTENER_CALL_TIMEOUT_MS,
  MINTED_DEDUPE_KEY_PATTERN,
} from "@legion/contracts";
import { holdUrlUserinfo } from "@legion/contracts/held-secrets";
import { errorClass, requestForText } from "@legion/contracts/log-text";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { z } from "zod";
import { credentialDeadline, type RequestCredential } from "./bearer";
import { messageFor, reachedNothing } from "./errors";
import { bodyForText, redirectOrigin, SentTokens, type Tokens } from "./excerpt";
import { machineID } from "./machine";
import { streamDeliversNothing } from "./stream-topics";
import type { TmuxPlace } from "./tmux-place";

/** What names the listener's URL in a text where the URL does not parse (`requestForText`). The
 *  configuration refuses such a URL before a client is made (`envoy-config.ts`). */
const ENVOY_URL_SETTING = "the configured Envoy URL";

/**
 * A session's state, which the listener computes from that session's last registration and from
 * nothing else (dispatch://INFRA-392): `live` within the live window (5 minutes), `unresponsive`
 * past it and inside the gone horizon (an hour), `gone` past that or with no record at all. An
 * unresponsive session is not dead — it keeps its role and issue claims, a message sent to it is
 * stored for it, a message to a role it holds is kept for that role for an hour, and nothing
 * starts a replacement for it.
 */
const SessionStateSchema = z.enum(["live", "unresponsive", "gone"]);

export type SessionState = z.infer<typeof SessionStateSchema>;

/**
 * What an agent must do about a session that stopped answering (dispatch://INFRA-392). Every
 * answer that names a session in that state says this, in one wording: the tool descriptions
 * (`tool-contract.ts`), the send confirmation, the publish confirmation of a message kept for an
 * unresponsive role holder, and the refusal that an unresponsive holder's role lookup, or a
 * publish with `keep: false` to its role, gets. Each is read at the moment an agent decides
 * whether its peer is alive. A publish to a topic counts its recipients and names none, so its
 * confirmation says nothing of the kind. The minutes are counted from the holder's last
 * registration (`last_seen`) and are `ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS` itself, the sum the
 * Dispatch server refuses another agent's take by, so a change of that constant changes this
 * sentence with it rather than leaving it saying a number nothing enforces.
 */
export const UNRESPONSIVE_RULE =
  "An unresponsive session is not dead: it keeps its role claims and its issue claims, and a " +
  "message sent to it is stored until it answers. Start no replacement for it and take no claim " +
  `from it until it is gone, or until ${ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS / 60_000} minutes ` +
  "have passed since its last_seen, the time of its last registration.";

// A listener predating the field names no state, and it lists only sessions it holds a
// registration for: `live` is what such an answer means, and reading an absent state as anything
// else would make every session on an older listener look dead.
const WireStateSchema = SessionStateSchema.default("live");

const InterestWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  dir: z.string(),
  topics: z.array(z.string()),
  updated_at: z.number().int().optional(),
  warnings: z.array(z.string()).optional(),
});

/**
 * How the listener delivers a session's stored subjects, answered to every registration it
 * accepts: `consumer` relays the session's durable consumer on `subject`. There is no other
 * mode — a registration no session consumer can be given is refused by one of the reasons
 * `ErrorWireSchema.reason` lists below, never answered.
 */
const DeliveryAnswerWireSchema = z.object({
  mode: z.literal("consumer"),
  subject: z.string().min(1),
  stream_id: z.string(),
  gap: z.boolean(),
  // The stream sequence the session's consumer starts at, which the session keeps as the floor
  // of its resume point (`consumer-delivery`'s `answered`). Optional: a listener of an earlier
  // release answers none, and such an answer must still parse.
  consumer_start: z.number().int().nonnegative().optional(),
});

// A list the listener may write as null (a session with no interest record, dispatch://INFRA-742),
// as an absent key (a listener that predates the field), or as a real array. The reader answers []
// for the first two and still refuses a wrong element type, so listSessions refuses such a row
// rather than reading it as empty.
const StringListWireSchema = z
  .array(z.string())
  .nullish()
  .transform((list) => list ?? []);

const SessionWireSchema = z.object({
  session_id: z.string(),
  machine_id: z.string(),
  // The session's real machine, as the client reported it (machineID()); machine_id stays the
  // listener it last registered through. The session's owner is the listener its stored
  // subscription names (the one store.Registry.Match fans its topics out on), else the one on
  // machine_id; the owner relays the session's consumer, and any other listener leaves inbox
  // messages to it silently (session.HandleAgentResult.OwnedElsewhere). No delivery reads host.
  // Optional here only for a listener predating the field — the live listener always fills it in,
  // falling back to machine_id itself (session.SessionEntry.HostOrMachine).
  host: z.string().optional(),
  dir: z.string(),
  port: z.number().int(),
  title: z.string(),
  topics: StringListWireSchema,
  roles: StringListWireSchema,
  capabilities: StringListWireSchema,
  self_subscribed: z.boolean(),
  last_seen: z.number().int().optional(),
  state: WireStateSchema,
  // The plugin the session registered through and its build; absent from a listener predating the
  // fields and for a session whose plugin predates them.
  plugin: z.string().optional(),
  plugin_version: z.string().optional(),
  // How the listener delivers to the session: "consumer" when its own session consumer holds its
  // stored messages, "" for a session on the core path. A listener that reports delivery writes
  // the key for every session, so an absent key is only a listener predating the field, never a
  // session on the core path; optional here so such a listener's row still parses.
  delivery: z.string().optional(),
});

const RoleWireSchema = z.object({
  role: z.string(),
  holder: z.string(),
  host: z.string().optional(),
  last_seen: z.number().int(),
  // The holder's own state. A role answer names it `holder_state`; `state` is the same session's
  // state as a session row reports it, so both are read and `holder_state` wins.
  state: WireStateSchema,
  holder_state: SessionStateSchema.optional(),
  // The process the claim names as the role's holder, answered beside the holder's session id
  // on the 200 and on the 404 that names a lapsed holder. Optional: a listener predating the
  // field names none, and so does a claim an earlier build wrote.
  client_instance_id: z.string().optional(),
  // The hand-back mark the claim carries (`internal/store`'s `RoleClaim.HandedBack`): the
  // holder gave this role back through a release that could not free the claim, so no
  // registration renews it and the reaper frees the role a live window and up to one reap
  // interval later. Answered on the 200 and on the 404 that names a lapsed holder. Optional:
  // the listener omits it where the claim carries no mark, and a listener predating the field
  // sends none, so an absent one means no mark.
  handed_back: z.boolean().optional(),
});

const RoleHeldWireSchema = z.object({
  error: z.string(),
  role: z.string(),
  holder: z.string(),
});

/**
 * A listener's error body. Beyond the message and its `code`, a refusal that names a session
 * names that session's state and its last registration, and a role's 404 names its holder too:
 * a send's `no live session <id>` carries `state` and `last_seen`
 * (`listenerwire.NoLiveSessionRefusal`), and a role lookup's or a role publish's
 * `holder lapsed` carries `holder`, `holder_state`, `last_seen` and `claim_released`
 * (`listenerwire.RoleRefusal`). Those two are the only refusals that name
 * a session. The listener's own text names neither the state nor the last registration, so
 * `EnvoyApiError` says them itself (`refusalStateText`): an agent told its peer did not answer
 * must be told whether that peer is unresponsive — still holding its role and its claims — or
 * gone.
 */
const ErrorWireSchema = z.object({
  error: z.string(),
  code: z.string().optional(),
  // Why the listener refused. This list is the one every other comment in these packages points
  // at, and it is the listener's own (`cmd/listener/session_delivery.go`'s delivery reason
  // constants and the refusals that carry them, and `cmd/listener/sender_limit.go`'s
  // `senderRateLimitedAnswer`). A registration it can give the session no consumer: 503
  // `consumer_error` (a consumer call that failed, or a listener holding no session-consumer
  // connection), 503 `no_grant` (the listener's NATS user may not manage session consumers), 503
  // `consumer_limit` (the stream holds as many consumers as its `max_consumers` allows, so the
  // server creates no more), 409 `not_requested` (the registration asked for no consumer) and 409
  // `invalid_session_id` (the id names no consumer). A send or a publish: 429
  // `sender_rate_limited` (its agent session sent faster than the listener lets one session send,
  // so this message was not stored; `retry_after_seconds` says when it can be sent again).
  reason: z.string().optional(),
  expected: z.array(z.string()).optional(),
  state: SessionStateSchema.optional(),
  holder: z.string().optional(),
  holder_state: SessionStateSchema.optional(),
  last_seen: z.number().int().optional(),
  /** Always false on a lapsed role's 404: a lookup releases no claim, whatever the state. */
  claim_released: z.boolean().optional(),
  /** A `sender_rate_limited` refusal's wait: the whole seconds until the sender may send again. */
  retry_after_seconds: z.number().int().positive().optional(),
});

/**
 * What a send or a publish answers: the envelope, and the fields the listener adds beside it.
 * `.extend` drops anything not declared here, so a field the listener sends and this schema does
 * not know never reaches a caller.
 *
 * `recipient`, `recipient_state`, `state`, `last_seen` and `duplicate` are a send's
 * (`listenerwire.SendResponse`). `recipient_state` is the target's registry state and
 * stays the strict three-value enum every deployed plugin parses. `state` is what the listener's
 * connection check found: `connected`, or `stopped` when no plugin of the target holds its relay
 * subject, so the message waits on the session's topic. It is read as a string rather than a
 * closed pair, as this file reads a delivery answer's `reason`, so a word a later listener adds
 * still parses: only `stopped` is stopped, and anything else is the connected reading every
 * release gave before the state existed. `duplicate` says the listener already stored a send
 * under this target and idempotency key, so this one stored nothing new; absent reads as false,
 * as an older listener's answer does.
 *
 * `holder` and `holder_state` are a role publish's, under the two names every role answer uses
 * (`listenerwire.PublishResponse`). A publish to a topic the stream captures stores one copy that
 * each subscribed session's own consumer delivers, so there is no per-recipient send to report; its
 * answer counts instead: `recipients` is every session other than the sender the topic is for,
 * and `recipients_connected` how many of those are running now. Both are absent from a role
 * topic's answer and from a listener whose copy of the known-session or interest bucket cannot be
 * served (its first read not complete, or past its stale bound; the known-session bucket also when
 * the listener cannot read it at all), and `recipients_connected` alone from one whose copy of the
 * session registry cannot be served; a count of zero is sent. Each field is omitted when there is
 * none, and absent from a listener predating it.
 */
const EnvelopeResponseSchema = EnvelopeSchema.extend({
  recipient: z.string().optional(),
  holder: z.string().optional(),
  holder_state: SessionStateSchema.optional(),
  recipient_state: SessionStateSchema.optional(),
  state: z.string().optional(),
  // The last registration (epoch ms) of a send's recipient, which the listener sends with an
  // unresponsive or a stopped recipient's state so the sender can say how long it has been
  // silent, or of a role publish's holder, which a 202 kept for a holder that is not live names
  // beside its state; omitted when the listener knows none, and absent from a listener predating
  // the field.
  last_seen: z.number().int().optional(),
  duplicate: z.boolean().optional(),
  recipients: z.number().int().nonnegative().optional(),
  recipients_connected: z.number().int().nonnegative().optional(),
});

/**
 * The publish route's answer. A role topic has three of them, told apart by status: 200, a live
 * session holds the role and the stream stored the forward for its consumer, with `holder`; 202,
 * no live session holds the role, so the listener kept the message on the role's kept lane for
 * one hour and hands it to the next session that holds the role live within that hour, with
 * `kept`, `kept_reason` and `kept_until`; 404, nothing was kept and the sender's own fallback
 * must run, which `request` throws as an `EnvoyApiError`. A 202 is answered for a role nobody
 * claims (`kept_reason: "unclaimed"`) and for a claim whose holder is unresponsive or gone
 * (`"holder_lapsed"`), which also names that `holder`, its `holder_state` and its `last_seen`
 * (read by the schema this one extends). A 404 comes when the listener keeps nothing: the
 * publish sent `keep: false`, the role's token is one no claim can name, or the keep failed. A
 * listener of an earlier release also answers it for a holder that is not live.
 * 202 is a success answer, so it passes `response.ok` and reaches this schema — load-bearing:
 * without it a kept message would read as a refusal.
 */
const PublishResponseSchema = EnvelopeResponseSchema.extend({
  kept: z.boolean().optional(),
  kept_reason: z.enum(["unclaimed", "holder_lapsed"]).optional(),
  /** The end of the kept copy's hour, in epoch milliseconds. Absent from a listener of an earlier
   *  release, which keeps the copy for a time it does not name. */
  kept_until: z.number().int().optional(),
});

export type EnvoyFetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

export type EnvoyClientConfig = {
  readonly baseUrl: string;
  readonly fetch: EnvoyFetch;
  readonly timeoutMs?: number;
  /** Whether a request a network error or a 5xx failed is sent once more, after 250 ms. Default
   * true; false for a best-effort read that must not hold its caller past one timeout. */
  readonly retry?: boolean;
  /**
   * What every call presents (`EnabledEnvoySettings.credential`): the listener's bearer, as it
   * always was; or an access token the secrets broker traded this machine's identity for, or the
   * identity of a person signed in on it, presented under the `DPoP` scheme with a proof of the
   * token's key on each request; or none at all, which sends no `Authorization` header, for a
   * loopback listener that asks for none.
   */
  readonly credential?: RequestCredential | undefined;
};

export type Interest = z.infer<typeof InterestWireSchema>;

export type DeliveryAnswer = z.infer<typeof DeliveryAnswerWireSchema>;

/**
 * The registration's answer: the interest, and the consumer the listener gave the session. Every
 * registration this client makes carries one or is refused (`parseSubscribeAnswer`): a session
 * receives only what its own consumer holds. A serving-port registration, which the listener
 * answers without one when it could give none and pushes to the port instead, is made by no
 * caller of this client any more — the OpenCode plugin that made them is gone (sjawhar/legion#2607).
 */
export type SubscribeResult = Interest & { readonly delivery: DeliveryAnswer };

/** Where a session's stream sequence was up to: what a new consumer may be seeded from. */
export interface ResumePoint {
  /** The stream's `envoy_stream_id`. */
  readonly stream_id: string;
  readonly seq: number;
}

/** The registration's `delivery` field: this session asks for consumer delivery. */
export interface DeliveryRequest {
  readonly consumer: true;
  /** A random string per plugin NATS connection; a new one restarts the relay at its ack floor. */
  readonly epoch: string;
  readonly resume?: ResumePoint;
}

/** A delivery answer this build does not take, read far enough to name it in the failure. */
const DeliveryRefusalSchema = z.object({
  mode: z.string(),
  reason: z.string().optional(),
});

/** What an answer whose `delivery` field this build cannot read at all is reported as. */
const DELIVERY_UNREADABLE = "answered a delivery field this build cannot read";

/**
 * Reads a registration answer. A session receives only what its own consumer holds, so an answer
 * that names no consumer delivery — none at all (a listener predating session consumers), the
 * `core` mode this build no longer accepts, or one this build cannot read — fails the
 * registration here, loudly and with the listener's own words in the message, rather than
 * leaving the session registered and deaf.
 */
export function parseSubscribeAnswer(body: unknown): SubscribeResult {
  const interest = InterestWireSchema.parse(body);
  const raw =
    typeof body === "object" && body !== null && "delivery" in body ? body.delivery : undefined;
  if (raw === undefined || raw === null) {
    throw new Error(
      `the listener answered no delivery for ${interest.session_id}: it predates session consumers, and a session receives only what its own consumer holds`
    );
  }
  const delivery = DeliveryAnswerWireSchema.safeParse(raw);
  if (!delivery.success) {
    const named = DeliveryRefusalSchema.safeParse(raw);
    const said = named.success
      ? `answered delivery mode ${named.data.mode}${named.data.reason === undefined ? "" : ` (${named.data.reason})`}`
      : DELIVERY_UNREADABLE;
    throw new Error(
      `the listener gave ${interest.session_id} no consumer: it ${said}, and a session receives only what its own consumer holds`
    );
  }
  return { ...interest, delivery: delivery.data };
}

/**
 * The subjects a session holds open and the listener's registry can disagree after a reconnect
 * or a human removal; this reports both, and which side knows each topic. Registry topics come
 * first in insertion order, then the live-only ones.
 */
export function mergeInterestSources(
  registryTopics: readonly string[],
  liveTopics: readonly string[]
): Map<string, "registry" | "live" | "both"> {
  const interests = new Map<string, "registry" | "live" | "both">();
  for (const topic of registryTopics) {
    interests.set(topic, liveTopics.includes(topic) ? "both" : "registry");
  }
  for (const topic of liveTopics) {
    if (!interests.has(topic)) interests.set(topic, "live");
  }
  return interests;
}

export type SessionInfo = z.infer<typeof SessionWireSchema>;

/**
 * A role lookup's answer. Its `client_instance_id` is the process the holder's claim names,
 * which is how a caller sees that the role is held under its own session id by another process:
 * `setRole`'s 409 names the holding session alone, and that session id can be the refused
 * caller's own, so a refused soft claim reads this lookup to tell the two apart.
 */
export type RoleInfo = z.infer<typeof RoleWireSchema>;

/**
 * The state a role answer reports for its holder: `holder_state` where the listener names it,
 * else the row's own state. A holder that is only unresponsive still holds the role — nothing on
 * a lookup releases its claim — so this never reads as "no holder".
 */
export function holderStateOf(role: RoleInfo): SessionState {
  return role.holder_state ?? role.state;
}

/** A process id as the two rules below weigh it: an empty one names no process, exactly as an
 *  absent one does. The listener omits `client_instance_id` where the claim names no process,
 *  and a build before it answered the empty string. */
function namedProcess(id: string | undefined): string | undefined {
  return id === "" ? undefined : id;
}

/**
 * Whether a role lookup's answer is this caller's own hold: it names `sessionID` as the holder
 * *and* `clientInstanceID` as the process whose claim stands, and it carries no hand-back mark.
 * A claim standing under this very session id that another process made is not this caller's to
 * renew — reading it as this caller's is how a lane kept heartbeating while a continuation's
 * claim held the role and then lapsed (dispatch://INFRA-783) — while a claim naming no process
 * at all is read as this caller's, since an older listener and a claim an earlier build wrote
 * name none. `undefined` is the lookup's 404: no holder, so no hold of this caller's.
 *
 * A marked claim (`handed_back`) is no hold of anyone's, whoever the holder is: the mark is the
 * listener's word that this role was handed back, so no registration renews the claim and the
 * reaper frees the role a live window and up to one reap interval later. A process that still
 * wants the role must claim again rather than read the lookup as confirmation. That covers the
 * one case the clients must read the mark for: a client built before the claim named its
 * process sends no client instance on a release, so its hand-back marks the live holder's
 * claim instead of freeing it. The holder sees at its very next heartbeat that it does not
 * hold the role, claims again, and is granted — a claim from the process the claim names
 * always is — and the fresh claim clears the mark. A holder that really did hand the role back
 * does not re-take it, because it no longer asks for a role it has ended.
 *
 * One rule for every client, because every client keeps it: {@link mayActOnRoleRecord} is the
 * other.
 */
export function roleHeldBy(
  info: RoleInfo | undefined,
  sessionID: string,
  clientInstanceID: string | undefined
): boolean {
  if (info === undefined || info.holder !== sessionID || info.handed_back === true) return false;
  const holding = namedProcess(info.client_instance_id);
  return holding === undefined || holding === namedProcess(clientInstanceID);
}

/**
 * Whether this process may act on a persisted role record — a transcript's role claim entry, a
 * channel server's role file — that `writer` names the writer of, given whether this process
 * holds a role already (`holdsRole`).
 *
 * A record this process wrote is acted on as it always was, and so is one written before the
 * record named its writer: that build was one process per transcript and per state directory. A
 * record another process wrote is acted on only while this process holds no role. Two processes
 * read one record whenever a second client resumes a live session's id — which is how a copy's
 * release ended the live process's role for hours (dispatch://INFRA-783) — and the holder is
 * not the one to decide that from a record it did not write: with no role to lose it asks, and
 * the listener decides the claim on the two processes rather than this side guessing.
 */
export function mayActOnRoleRecord(
  writer: string | undefined,
  clientInstanceID: string | undefined,
  holdsRole: boolean
): boolean {
  const wrote = namedProcess(writer);
  if (wrote === undefined || wrote === namedProcess(clientInstanceID)) return true;
  return !holdsRole;
}

export type SubscribeInput = {
  readonly sessionID: string;
  readonly directory: string;
  readonly topics: readonly string[];
  /** Always 0: the listener refuses a registration whose port is above 0, naming the field. */
  readonly port: number;
  readonly title: string;
  readonly capabilities?: readonly string[];
  readonly driving: boolean;
  readonly selfSubscribed?: boolean;
  /**
   * Asks for consumer delivery (`DeliveryRequest`). Required: a session receives only what its
   * own consumer holds, so a registration that asked for none is refused `409 not_requested`.
   */
  readonly delivery: DeliveryRequest;
  /**
   * The client instance that drives this session: one Oh My Pi Envoy extension instance, one
   * Claude Code channel-server process, minted once when that instance starts. The listener
   * stores it on the session's registry entry, a soft role claim naming a predecessor compares
   * it, and a deregistration carrying it deletes only an entry that is this instance's.
   */
  readonly clientInstanceID?: string;
  /** The plugin this session registers through (its npm package name) and that plugin's build,
   * sent on every registration so the listener's session list shows which build each session
   * runs. */
  readonly plugin: { readonly name: string; readonly version: string };
  /** Where this session's pane sits in tmux (`readTmuxPlace`), carried on every registration so
   * the layout follows a pane the human moved. Absent for a session outside tmux, which is a
   * session in no pane. The listener builds its entry fresh from each registration, so an absent
   * place clears the place it holds: a caller whose tmux call did not answer, and that means to
   * keep the place it last reported, must send that place again rather than leave this off
   * (`createTmuxPlaceCache` keeps the last reading for exactly that). */
  readonly place?: TmuxPlace;
};

/**
 * A new {@link SubscribeInput.clientInstanceID}: 128 random bits as 32 lower-case hex characters.
 *
 * A client instance is one host process driving sessions at the listener — one Oh My Pi Envoy
 * extension instance, one Claude Code channel-server process — and mints this once, when it
 * starts, for every registration and deregistration it makes.
 */
export function newClientInstanceID(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
    byte.toString(16).padStart(2, "0")
  ).join("");
}

export type UnsubscribeInput = {
  readonly sessionID: string;
  readonly topics: readonly string[];
  /**
   * The client instance handing these interests back: the same id its registration carries
   * ({@link SubscribeInput.clientInstanceID}). The listener frees only the role claims that
   * name this process, and never forces one from another, so a second process that resumed
   * this session id keeps the role it holds. A release that names no process frees no claim,
   * and marks the claim of each role lane topic it names handed back, whether that claim names
   * a process or none, which stops that claim's renewal and frees the role a live window and
   * up to one reap interval later. A role lane
   * topic whose claim the release did not free stays on the session's record, because that
   * topic is the interest half of a claim that still stands and the record's role topics are
   * what a deregistration of the session expands into the claims it frees; the answer names
   * the topics that actually left it. Omitted from the wire when absent, so an older listener
   * sees an unchanged body.
   */
  readonly clientInstanceID?: string;
};

/**
 * The concrete subjects a registration carries for each topic: a `<base>.>` wildcard, and its
 * base as a subject of its own when the stream stores a publish to that base. A base no stream
 * subject captures is left out: nothing can be published on it, and the listener would leave it
 * out of the row it stores and warn about it at every registration
 * (`streamDeliversNothing`).
 */
export function expandSubscriptionTopics(topics: readonly string[]): readonly string[] {
  const expanded = new Set<string>();
  for (const topic of topics) {
    const base = topic.endsWith(".>") ? topic.slice(0, -2) : undefined;
    if (base === undefined || base.includes("*") || base.includes(">")) {
      expanded.add(topic);
      continue;
    }
    const segments = base.split(".");
    if (segments.length < 2 || segments.some((segment) => segment.length === 0)) {
      throw new TypeError(
        `cannot expand Envoy wildcard topic "${topic}": its concrete base must have at least two non-empty segments`
      );
    }
    if (!streamDeliversNothing(base)) expanded.add(base);
    expanded.add(topic);
  }
  return [...expanded];
}

export type MessageSource = "agent" | "human";

export type AgentSourceInput = {
  readonly source?: "agent";
  readonly sourceSessionID: string;
};

export type HumanSourceInput = {
  readonly source: "human";
  readonly sourceSessionID?: string;
};

export type MessageMetadataInput = {
  readonly inReplyTo?: string;
  readonly supersedes?: string;
  readonly urgency?: "low" | "med" | "high" | "blocking";
  readonly expectsReply?: ExpectsReply;
  readonly expiresAt?: number;
};

export type SendInput = (AgentSourceInput | HumanSourceInput) &
  MessageMetadataInput & {
    readonly targetSessionID: string;
    readonly message: string;
    /**
     * Sent on every attempt; one is minted when absent. Pass the `idempotencyKey` of an
     * `EnvoySendUnconfirmedError` to send that message again: the listener stores it at most once.
     */
    readonly idempotencyKey?: string;
  };

export type PublishInput = (AgentSourceInput | HumanSourceInput) &
  MessageMetadataInput & {
    readonly topic: string;
    readonly message: string;
    readonly payload?: string;
    readonly idempotencyKey?: string;
    /**
     * Only a role topic reads this. Unset, or true, keeps a message that no live session took,
     * for a role nobody claims or a holder that is unresponsive or gone, on the role's kept lane
     * for one hour, for the next session that holds the role live; the listener answers 202
     * (`kept`, `keptUntil`). False keeps nothing, so the listener answers 404 and the sender's
     * own fallback runs. The field is omitted from the wire unless set, so an older listener
     * sees an unchanged request.
     */
    readonly keep?: boolean;
  };

export type SetRoleInput = {
  readonly sessionID: string;
  readonly role: string;
  /**
   * Claim only if the role is unheld, its holder is no longer live, or its
   * holder is `previousSessionID` and that predecessor is this client
   * instance's (see `previousSessionID`). Any other live holder is left in
   * place and reported in the result instead of being displaced. Default
   * (false) is last-claim-wins.
   */
  readonly soft?: boolean;
  /**
   * The session id this claimant continues (a fork or branch mints a new id
   * in the same process). A soft claim takes the role from that predecessor
   * only when the predecessor has no live registry entry, when its live entry
   * carries this client instance's own `clientInstanceID`, or when either
   * side carries no instance id; a predecessor still live under another
   * client instance keeps the role. Sent only with `soft`.
   */
  readonly previousSessionID?: string;
  /**
   * The process making this claim: the same id its registration carries
   * ({@link SubscribeInput.clientInstanceID}). The listener records it on the
   * claim as the role's holder, and where the role is already held under this
   * claimant's own session id it decides on that standing claim alone: the
   * claim is granted where it names the very process holding it, granted once
   * nobody has renewed that claim inside the live window, and otherwise
   * refused 409 naming the holder. Omitted from the wire when absent, so an
   * older listener sees an unchanged body.
   */
  readonly clientInstanceID?: string;
};

export type SetRoleResult =
  | { readonly claimed: true; readonly interest: Interest }
  | { readonly claimed: false; readonly holder: string };

export type ListSessionsInput = {
  readonly directory?: string;
  readonly title?: string;
  /** `unresponsive` asks the listener for the sessions past their live window as well; without it
   *  the list holds the live ones alone. A gone session is never listed either way. */
  readonly include?: "unresponsive";
};

/**
 * What the listener's connection check found for a send's target: `connected`, a plugin of that
 * session holds its relay subject now; `stopped`, none does, so the message waits on the
 * session's topic. Beside the registry state (`SessionState`) rather than a fourth value of it:
 * a session stays live or unresponsive by the age of its last registration whatever this says,
 * and keeps its claims.
 */
export type ConnectionState = "connected" | "stopped";

export type SendResult = {
  readonly envelope: Envelope;
  readonly recipient: string;
  readonly confirmed: boolean;
  /** The recipient's state as the listener resolved it; `live` from a listener that names none.
   *  A message to an unresponsive recipient is stored and reaches it when it answers again. */
  readonly recipientState: SessionState;
  /**
   * What the target was when the listener stored the message. `stopped` means the message waits
   * on the session's topic for the stream's 72 hours and reaches it if it runs again in that
   * time. A listener that answers no state reads as `connected`, which is what every release
   * meant before the state existed.
   */
  readonly state: ConnectionState;
  /** The recipient's last registration in epoch milliseconds, which the listener sends beside an
   *  unresponsive or a stopped recipient's state so the sender can say since when it has been
   *  silent. Absent when the listener knows none, and from a listener predating the field. */
  readonly lastSeen?: number;
  /** The listener already held a send under this target and idempotency key: delivered once. */
  readonly duplicate: boolean;
};

export type PublishResult = {
  readonly envelope: Envelope;
  /** A role publish's resolved holder, and the state the listener's record puts it in: on a 200
   *  the live holder the message was stored for, on a 202 kept for `holder_lapsed` the holder
   *  that is not live. */
  readonly holder?: string;
  readonly holderState?: SessionState;
  /** That holder's last registration in epoch milliseconds, which the listener sends beside a
   *  holder that is not live on a 202 kept for it. Absent when the listener knows none, and from
   *  a listener predating the field. */
  readonly lastSeen?: number;
  /**
   * How many sessions other than the sender get this message, counted just before it was
   * published; a stopped session Envoy still knows is one of them. Absent for a role topic, whose
   * answer names its holder instead, and for a listener whose NATS user cannot read the
   * known-session bucket; zero is a count, not an absence.
   */
  readonly recipients?: number;
  /** How many of {@link PublishResult.recipients} are running now; absent whenever that is. */
  readonly recipientsConnected?: number;
  /**
   * True on the listener's 202: no live session holds the role, so the listener kept the message
   * on the role's kept lane and hands it to the next session that holds the role live before
   * `keptUntil`. The message is not lost, so this is a success answer, not a refusal. Absent
   * unless the listener answered it.
   */
  readonly kept?: boolean;
  /** Why the message was kept: `unclaimed` for a role nobody claims, `holder_lapsed` for a claim
   * whose holder is unresponsive or gone. Read from the answer's own schema, so a reason a later
   * listener adds is added in one place. */
  readonly keptReason?: z.infer<typeof PublishResponseSchema>["kept_reason"];
  /** The end of the kept copy's hour, in epoch milliseconds. If no session takes the copy by
   * then, the listener drops it and sends the publish's source session a message that names it,
   * unless no walk had claimed the copy and its hour ended more than an hour before the walk that
   * drops it. Absent from a listener of an earlier release, which names no time and sends no
   * such message. */
  readonly keptUntil?: number;
};

/**
 * What a refusal naming a session's state adds to its message: which of the three states the
 * listener put that session in, when it last registered, and, where that session stopped
 * answering, the rule that such a session is not dead. The listener's own text says none of it
 * ("no live session ses_x", "role reviewer holder ses_x lapsed; claim retained (last seen
 * 1762...)"), and an agent reading only the message cannot tell a peer that stopped answering
 * from one that ended — which is the mistake this change exists to stop (dispatch://INFRA-392,
 * R7). A body that names no state adds nothing, so an older listener's message is what it
 * always was.
 */
function refusalStateText(parsed: z.infer<typeof ErrorWireSchema>): string {
  const state = parsed.holder_state ?? parsed.state;
  if (state === undefined) return "";
  const who = parsed.holder === undefined ? "That session" : `Holder ${parsed.holder}`;
  // A listener that can name no registration at all sends none (the field is omitted, or 0):
  // the state alone is said, since a made-up time would read as knowledge nobody has.
  const seen =
    parsed.last_seen === undefined || parsed.last_seen <= 0
      ? ""
      : `, last seen ${new Date(parsed.last_seen).toISOString()}`;
  const sentences = [`${who} is ${state}${seen}.`];
  if (state === "unresponsive") sentences.push(UNRESPONSIVE_RULE);
  return ` ${sentences.join(" ")}`;
}

export class EnvoyApiError extends Error {
  readonly name = "EnvoyApiError";
  readonly expected: readonly string[] | undefined;
  /** The listener's machine-readable `code`, when its error body carries one. */
  readonly code: string | undefined;
  /**
   * Why the listener refused, when its error body carries one: one of the names
   * `ErrorWireSchema.reason` lists above, such as the reason a registration no session consumer
   * can be given is refused with, or `sender_rate_limited`.
   */
  readonly reason: string | undefined;
  /** The session the refusal is about: a role's holder, where its body names one. */
  readonly holder: string | undefined;
  /** That session's state as the listener's record put it, when the body names one: a refused
   *  send's `state`, a lapsed role's `holder_state`. */
  readonly sessionState: SessionState | undefined;
  /** That session's last registration in epoch milliseconds, when the body names one. */
  readonly lastSeen: number | undefined;
  /** Whether the refusal released the holder's role claim. The listener answers it on every
   *  lapsed role and it is always false: a lookup or a publish takes nothing from a holder,
   *  whatever its state. */
  readonly claimReleased: boolean | undefined;
  /**
   * The whole seconds until the sender may send again, when the body names them: a
   * `sender_rate_limited` refusal's `retry_after_seconds`. The client never sends a refused
   * message again by itself (`request` retries nothing below 500), so a caller that wants the
   * message sent waits this long and sends it again.
   */
  readonly retryAfterSeconds: number | undefined;

  /**
   * `details.responseBody` is the answer's body as it came, which the readers of a refusal's
   * fields parse (a role's holder, `phase-reminder.ts`'s reason). The message names it only where
   * it is the listener's error JSON; any other body was written by something in the listener's
   * place, which can echo the request back, so the message names it through `bodyForText`: one
   * line with `shown.tokens` (the credential's secret, and the tokens the client presented lately)
   * and any `Authorization`, `Bearer` or `DPoP` value redacted, and the origin a redirect pointed
   * at (`shown.redirectTo`). Neither is kept.
   */
  constructor(
    readonly details: {
      readonly method: string;
      /** The request's URL as text names it (`requestForText`): as built on a plain base, and the
       *  base's fixed phrase otherwise. */
      readonly url: string;
      readonly status: number;
      readonly responseBody: string;
    },
    shown: { readonly tokens?: Tokens; readonly redirectTo?: string | undefined } = {}
  ) {
    let parsed: z.infer<typeof ErrorWireSchema> | undefined;
    try {
      const result = ErrorWireSchema.safeParse(JSON.parse(details.responseBody));
      if (result.success) parsed = result.data;
    } catch {
      // A body that is not JSON is no error of the listener's: the message names it below.
    }
    const redirected =
      shown.redirectTo === undefined
        ? ""
        : ` (a redirect to ${shown.redirectTo}, which this client does not follow)`;
    // The reason joins the message: a refused registration must fail with the listener's own
    // machine-readable why in what the session is shown, not only in the client's field.
    super(
      parsed === undefined
        ? `${details.method} ${details.url} failed with ${details.status}${redirected}: ${bodyForText(details.responseBody, shown.tokens ?? "", "the listener's error JSON")}`
        : `${parsed.error}${parsed.reason === undefined ? "" : ` (reason: ${parsed.reason})`}${parsed.expected === undefined ? "" : ` (expected: ${parsed.expected.join(", ")})`}${refusalStateText(parsed)}`
    );
    this.expected = parsed?.expected;
    this.code = parsed?.code;
    this.reason = parsed?.reason;
    this.holder = parsed?.holder;
    this.sessionState = parsed?.holder_state ?? parsed?.state;
    this.lastSeen = parsed?.last_seen;
    this.claimReleased = parsed?.claim_released;
    this.retryAfterSeconds = parsed?.retry_after_seconds;
  }
}

/** The listener's own test of whether a send's dedupe key lets the stream and hosts drop a repeat. */
const mintedDedupeKey = new RegExp(MINTED_DEDUPE_KEY_PATTERN);

/**
 * A send that ended without the listener's word that it was stored: an attempt got no answer at
 * all (the request timed out, its connection failed, or a 2xx answer's body did not arrive or did
 * not parse) or got a 5xx, so the message may or may not have reached its target, even when a
 * later attempt was refused. Every attempt carried `idempotencyKey`. The listener sends under the
 * dedupe key `agent.<target>.<idempotencyKey>`, and when that key is one a repeat is recognised by
 * (`MINTED_DEDUPE_KEY_PATTERN`: a lower-case UUID and a target with no dot), the stream stores
 * one copy and the recipient's plugin drops a repeat inside `DELIVERY_DUPLICATE_WINDOW_MS`, which
 * states the rare cases where a recipient that restarted still sees it twice (`resendSafe`). A
 * send refused with a 4xx on every attempt it made stays an `EnvoyApiError`. `last` (also the
 * `cause`) is what the last attempt got.
 */
export class EnvoySendUnconfirmedError extends Error {
  override readonly name = "EnvoySendUnconfirmedError";
  /** Whether Envoy recognises a resend under `idempotencyKey` inside the window as a repeat. */
  readonly resendSafe: boolean;

  constructor(
    readonly targetSessionID: string,
    readonly idempotencyKey: string,
    readonly attempts: number,
    readonly last: unknown
  ) {
    const got =
      last instanceof EnvoyApiError
        ? `the listener answered ${last.details.status} (${last.message})`
        : `the listener did not answer (${last instanceof Error ? last.message : String(last)})`;
    const tried = attempts === 1 ? "on its only attempt" : `on the last of ${attempts} attempts`;
    const resendSafe = mintedDedupeKey.test(`agent.${targetSessionID.trim()}.${idempotencyKey}`);
    const resend = resendSafe
      ? `Sending it again with idempotency_key ${idempotencyKey} within ` +
        `${DELIVERY_DUPLICATE_WINDOW_HOURS} hours is the safe resend: Envoy drops it as ` +
        "a repeat if the first copy arrived (a recipient that restarted in between can rarely " +
        "see it twice)."
      : `Envoy cannot recognise a resend under idempotency_key ${idempotencyKey} as a repeat ` +
        "(the key is not a lower-case UUID, or the session id holds a dot): ask the recipient " +
        "whether it arrived before sending it again.";
    super(
      `The send to ${targetSessionID} is not confirmed delivered: ${got} ${tried}. It may or may ` +
        `not have arrived. ${resend}`,
      { cause: last }
    );
    this.resendSafe = resendSafe;
  }
}

export type EnvoyClient = {
  readonly subscribe: (input: SubscribeInput) => Promise<SubscribeResult>;
  readonly unsubscribe: (input: UnsubscribeInput) => Promise<void>;
  readonly getInterest: (sessionID: string) => Promise<Interest>;
  readonly send: (input: SendInput) => Promise<SendResult>;
  readonly publish: (input: PublishInput) => Promise<PublishResult>;
  /**
   * Deletes the session's registry entry. With `clientInstanceID` the listener deletes the entry
   * only when it carries the same instance id or none, so a departing client instance never
   * deletes the registration a successor instance made under the same session id; without one
   * the entry is deleted as before. The listener's answer says whether the entry stayed, and
   * this resolves `void` because no caller acts on it: a deregistering client has nothing to
   * retry either way. `timeoutMs` is the time its caller has left, a shutdown's budget: one
   * attempt, its credential waiting on that time less the request's reserve
   * (`credentialDeadline`), so a credential still waiting on a trade leaves the DELETE time to go
   * without it.
   */
  readonly unregisterSession: (input: {
    readonly sessionID: string;
    readonly clientInstanceID?: string;
    readonly timeoutMs?: number;
  }) => Promise<void>;
  /**
   * Claims `role` for the session. A soft claim (`soft`) naming a predecessor
   * (`previousSessionID`) takes the role from it only when that predecessor has no live registry
   * entry, when its live entry carries this client instance's own instance id, or when either
   * side carries no instance id; any other live holder keeps the role and is reported as
   * `{ claimed: false, holder }`.
   */
  readonly setRole: (input: SetRoleInput) => Promise<SetRoleResult>;
  readonly getRole: (role: string) => Promise<RoleInfo>;
  readonly listSessions: (input?: ListSessionsInput) => Promise<readonly SessionInfo[]>;
};

type RequestOptions = {
  readonly timeoutMs?: number;
  readonly retry?: boolean;
  /** Called for each attempt that got no answer or a 5xx: one the listener may have acted on. */
  readonly onUnanswered?: () => void;
  /**
   * Called when the call fails on an attempt that reached no route of the listener: its
   * credential could not be minted and the listener's auth gate answered 401 to the request
   * sent without it, or the attempt's deadline ended while the credential waited, so nothing
   * was sent. Such an attempt is retried as an unanswered one is, but is never one the listener
   * may have acted on.
   */
  readonly onNotSent?: () => void;
  /** Called as each attempt starts, so a caller can say how many it made. */
  readonly onAttempt?: () => void;
  /**
   * The session this call acts for, where its body names one the listener binds to the
   * credential: a subscribe, an unsubscribe, a deregistration, a role claim, and a
   * send's or a publish's `source_session`. The listener answers 403 to a body naming a session
   * its broker token does not (`sessionNotBound`, packages/envoy/cmd/listener/brokerauth.go), so
   * the credential is asked for that session's own headers rather than the caller's current
   * ones; a call this process makes for the session it has just left — the hand-back of a role
   * that session still holds — is the one that needs it. A read of another session's record
   * names none: those are not narrowed, and asking for a credential of a session this process
   * never acted as would trade for a name the broker refuses.
   */
  readonly actingSession?: string;
};

/** One attempt's outcome: the listener's status and body; no answer, and why; or nothing that
 *  reached a route of the listener, and why (`RequestOptions.onNotSent`). */
type AttemptOutcome =
  | { readonly response: Response; readonly body: string }
  | { readonly unanswered: unknown }
  | { readonly notSent: unknown };

/**
 * The one place the client's call timing resolves: a configuration names a per-attempt timeout
 * and whether a failed call is retried, and whatever it leaves unset comes from the declarations
 * in `@legion/contracts`, which weigh a whole call's cost against the live window a refresh must
 * renew a session's registration inside. A per-request override (`RequestOptions.timeoutMs` and
 * `RequestOptions.retry`) still overrides what this returns, for the one call it is given on.
 */
export function resolveCallTiming(config: Pick<EnvoyClientConfig, "timeoutMs" | "retry">): {
  timeoutMs: number;
  attempts: number;
  retryDelayMs: number;
} {
  return {
    timeoutMs: config.timeoutMs ?? LISTENER_CALL_TIMEOUT_MS,
    attempts: config.retry === false ? 1 : LISTENER_CALL_ATTEMPTS,
    retryDelayMs: LISTENER_CALL_RETRY_DELAY_MS,
  };
}

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  // Its user info held: Bun sends it as `Authorization: Basic` where no credential's header
  // replaces it, and a listener at the wrong URL can echo that (`holdUrlUserinfo`).
  const baseUrl = holdUrlUserinfo(withoutTrailingSlashes(config.baseUrl));
  const { timeoutMs, attempts, retryDelayMs } = resolveCallTiming(config);

  const credential = config.credential;
  // The tokens this client presented lately, which every excerpt of an answer redacts beside the
  // credential's secret now: a renewal can come between a request and its answer (`SentTokens`).
  const sent = new SentTokens();
  const tokens = (): readonly string[] => sent.with(credential?.secret() ?? "");

  /** `url` as a text names it: on a clean base, as built, and otherwise the base's phrase. */
  const shown = (url: string): string => requestForText(baseUrl, url, ENVOY_URL_SETTING);

  /** `raw`, the body of a 2xx answer to `method` `path`, as JSON. One that is not JSON throws a
   *  `SyntaxError` naming the body through `bodyForText`, never the parser's own message, which
   *  quotes the word it stopped at: whatever answered in the listener's place chose that word. */
  const answerJson = (raw: string, method: string, path: string): unknown => {
    try {
      return JSON.parse(raw);
    } catch {
      const named = bodyForText(raw, tokens(), "JSON");
      throw new SyntaxError(`${method} ${shown(`${baseUrl}${path}`)} answered with ${named}`);
    }
  };

  /** A call that got no answer, as our own error: the runtime's own error repeats the host it
   *  dialed, which an unclean base names (`https://user:p@ssword` dials `ssword`), so it is named
   *  by its class and kept as no cause. */
  const noAnswer = (method: string | undefined, url: string, error: unknown): Error =>
    new Error(`${method ?? "GET"} ${shown(url)} got no answer: ${errorClass(error)}`);

  const attemptOnce = async (
    url: string,
    init: RequestInit,
    deadlineMs: number,
    actingSession: string | undefined
  ): Promise<AttemptOutcome> => {
    let response: Response;
    // What a credential that could not mint said, where one could not. A listener that asks for
    // no credential answers the same with or without one, so a broker that is down or refused
    // this machine's trade must not take those calls down: the request goes without the headers
    // and stands or falls on the listener's own answer. A listener that did want one answers 401,
    // and that is reported as the failed trade it really is, below.
    let unminted: unknown;
    // One deadline for the whole attempt, `deadlineMs` (the listener's 5 s), and the credential
    // waits on all of it but the request's reserve (`credentialDeadline`: 4 s of 5, the request
    // 1 s). A broker credential that hands over its live token when its wait ends leaves the
    // request that second to be sent and answered; one still waiting then (a pod's missing
    // registration, a broker that does not answer) stops, and the request goes without it in that
    // second, as a credential that could not mint at all does.
    const deadline = AbortSignal.timeout(deadlineMs);
    const waited = credentialDeadline(deadline, deadlineMs);
    try {
      const headers = new Headers(init.headers);
      // Awaited only when the credential answers with a promise. A bearer answers at once, and
      // an awaited microtask here would let whatever the caller does next — the plugin's NATS
      // drain at shutdown — start before this request leaves.
      let presented: Readonly<Record<string, string>> | undefined;
      try {
        const ready = credential?.headers(init.method ?? "GET", url, actingSession, waited);
        presented = ready instanceof Promise ? await ready : ready;
      } catch (error) {
        unminted = error;
      }
      // Only the attempt's own deadline leaves nothing to send: the attempt sent nothing, and
      // fails in the credential's own words (a pending registration names its session).
      if (unminted !== undefined && deadline.aborted) return { notSent: unminted };
      sent.note(presented);
      for (const [name, value] of Object.entries(presented ?? {})) headers.set(name, value);
      // No call follows a redirect: the listener answers none, so a 3xx is something in its
      // place, and followed it would turn a POST into a GET (301, 302, 303) or send it again to
      // another origin (307, 308). The 3xx is the call's answer, which reached no listener.
      response = await config.fetch(url, {
        ...init,
        headers,
        signal: deadline,
        redirect: "manual",
      });
    } catch (error) {
      // A name that did not resolve, or a connection nothing took, sent nothing to any listener:
      // retried as an unanswered attempt is, but never one the listener may have acted on.
      const failed = noAnswer(init.method, url, error);
      return reachedNothing(error) ? { notSent: failed } : { unanswered: failed };
    }
    if (unminted !== undefined && response.status === 401) {
      // The listener wanted the credential this machine could not mint: a listener that requires
      // one answers a request without it 401 and nothing else, at its auth gate before any route
      // runs (`apiauth.go`). That is no answer about the call itself, and nothing the call asked
      // was done, so it retries as an unanswered attempt does — the broker may have come back —
      // but is never one the listener may have acted on, and the error names both halves. A 403
      // is the listener's own refusal of what the call asked (a send's capability, a denied
      // publish), and stands as its answer.
      return {
        notSent: new Error(
          `${init.method ?? "GET"} ${shown(url)} answered ${response.status}, and this machine's Envoy credential could not be minted: ${messageFor(unminted)}`,
          { cause: unminted }
        ),
      };
    }
    // The body is read under the same deadline. A refusal whose body does not arrive is still the
    // listener's refusal; any other answer whose body does not arrive is no answer.
    try {
      return { response, body: await response.text() };
    } catch (error) {
      if (response.status >= 400 && response.status < 500) {
        return { response, body: `(its body did not arrive: ${errorClass(error)})` };
      }
      return { unanswered: noAnswer(init.method, url, error) };
    }
  };

  const request = async (
    path: string,
    init: RequestInit,
    options: RequestOptions = {}
  ): Promise<string> => {
    const url = `${baseUrl}${path}`;
    const tries = options.retry === false ? 1 : attempts;
    for (let attempt = 0; attempt < tries; attempt += 1) {
      options.onAttempt?.();
      const outcome = await attemptOnce(
        url,
        init,
        options.timeoutMs ?? timeoutMs,
        options.actingSession
      );
      if ("unanswered" in outcome) {
        options.onUnanswered?.();
        if (attempt + 1 < tries) {
          await waitForRetry(retryDelayMs);
          continue;
        }
        throw outcome.unanswered;
      }
      if ("notSent" in outcome) {
        if (attempt + 1 < tries) {
          await waitForRetry(retryDelayMs);
          continue;
        }
        options.onNotSent?.();
        throw outcome.notSent;
      }
      const { response, body } = outcome;
      if (response.ok) return body;

      // A body that is not the listener's error JSON was written by something in its place, which
      // can echo the request back: the error names it in one redacted line with the tokens this
      // client presents, and a redirect, which is never followed (`attemptOnce`), by the origin it
      // pointed at (`EnvoyApiError`).
      const redacted = tokens();
      const error = new EnvoyApiError(
        {
          method: init.method ?? "GET",
          url: shown(url),
          status: response.status,
          responseBody: body,
        },
        { tokens: redacted, redirectTo: redirectOrigin(response, url, redacted) }
      );
      // Only a 5xx is sent again. A 4xx is the listener's answer to this message, and so is a 429
      // `sender_rate_limited`: the agent reads its sentence and decides when to send it again.
      if (response.status >= 500) {
        options.onUnanswered?.();
        if (attempt + 1 < tries) {
          await waitForRetry(retryDelayMs);
          continue;
        }
      }
      throw error;
    }
    throw new Error("Envoy request exhausted retries");
  };

  const post = (path: string, body: object, options?: RequestOptions) =>
    request(
      path,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      },
      options
    );

  // What POST /v1/interests/subscribe carries.
  const subscribeBody = (input: SubscribeInput) => ({
    session_id: input.sessionID,
    dir: input.directory,
    topics: expandSubscriptionTopics(input.topics),
    port: input.port,
    title: input.title,
    ...(input.capabilities === undefined ? {} : { capabilities: input.capabilities }),
    driving: input.driving,
    ...(input.selfSubscribed === undefined ? {} : { self_subscribed: input.selfSubscribed }),
    delivery: input.delivery,
    // The client instance driving this session (`clientInstanceID`), omitted when the
    // caller has none so an older listener sees an unchanged body. The listener stores it
    // on the registry entry, where a soft role claim and a deregistration compare it.
    ...(input.clientInstanceID === undefined ? {} : { client_instance_id: input.clientInstanceID }),
    // Where this session process actually runs (machineID(): ENVOY_MACHINE_ID, falling
    // back to the kernel hostname), sent on every subscribe and heartbeat (registerSession
    // re-subscribes on each tick). The listener stores it separately from the registering
    // listener's own machine_id and reads it for no delivery: it is what a human-facing
    // list shows as the session's machine. The owner is the listener the session's stored
    // subscription names, else the one it registered through; only the owner relays the
    // session's consumer, and any other listener leaves inbox messages to it silently.
    host: machineID(),
    // The pane this session sits in (`readTmuxPlace`), beside the machine holding it. Omitted
    // when the caller reports none, which clears the place the listener holds for this session
    // rather than keeping it: the listener's entry is built fresh from this body, so a session
    // that has left tmux stops being in a pane.
    ...(input.place === undefined ? {} : { tmux_place: input.place }),
    plugin: input.plugin.name,
    plugin_version: input.plugin.version,
  });

  return {
    subscribe: async (input) =>
      parseSubscribeAnswer(
        answerJson(
          await post(
            "/v1/interests/subscribe",
            subscribeBody(input),
            // Every call below whose body names the session it acts for presents that session's
            // own credential, since the listener binds a broker token's session to it.
            { actingSession: input.sessionID }
          ),
          "POST",
          "/v1/interests/subscribe"
        )
      ),
    unsubscribe: async (input) => {
      await post(
        "/v1/interests/unsubscribe",
        {
          session_id: input.sessionID,
          topics: expandSubscriptionTopics(input.topics),
          // The process handing the interests back, omitted when the caller names none so an
          // older listener sees an unchanged body. The listener frees the role claims naming
          // this process and no others, and forces none.
          ...(input.clientInstanceID === undefined
            ? {}
            : { client_instance_id: input.clientInstanceID }),
        },
        { actingSession: input.sessionID }
      );
    },
    getInterest: async (sessionID) =>
      InterestWireSchema.parse(
        answerJson(
          await request(`/v1/interests/${sessionID}`, {}),
          "GET",
          `/v1/interests/${sessionID}`
        )
      ),
    send: async (input) => {
      const idempotencyKey = input.idempotencyKey ?? crypto.randomUUID();
      // An attempt the listener may have acted on keeps the send unconfirmed, whatever a later
      // attempt got: a first attempt can store the message and lose its answer, and the retry
      // then find the target gone. A last attempt that reached no route of the listener (its
      // auth gate's 401 to a request sent without the credential that could not be minted)
      // stored nothing, so where no earlier attempt may have, the send was refused.
      let unanswered = false;
      let notSent = false;
      // The attempts this send made, which the unconfirmed error names: fewer than the client's
      // where an answer ends the send early, such as a 2xx whose body does not parse.
      let made = 0;
      let response: z.infer<typeof EnvelopeResponseSchema>;
      try {
        const answer = await post(
          "/v1/messages/send",
          {
            source: input.source ?? "agent",
            ...sourceSession(input.sourceSessionID),
            target_session: input.targetSessionID,
            message: input.message,
            idempotency_key: idempotencyKey,
            ...messageMetadata(input),
          },
          {
            onUnanswered: () => {
              unanswered = true;
            },
            onAttempt: () => {
              made += 1;
            },
            onNotSent: () => {
              notSent = true;
            },
            ...(input.sourceSessionID === undefined || input.sourceSessionID === ""
              ? {}
              : { actingSession: input.sourceSessionID }),
          }
        );
        // A 2xx whose body does not parse is the listener's word that cannot be read.
        response = EnvelopeResponseSchema.parse(answerJson(answer, "POST", "/v1/messages/send"));
      } catch (error) {
        const refused = notSent || (error instanceof EnvoyApiError && error.details.status < 500);
        if (refused && !unanswered) throw error;
        throw new EnvoySendUnconfirmedError(input.targetSessionID, idempotencyKey, made, error);
      }
      return {
        envelope: response,
        recipient: response.recipient ?? input.targetSessionID,
        confirmed: response.recipient !== undefined,
        recipientState: response.recipient_state ?? "live",
        // Only `stopped` is stopped: an answer with no state is an older listener's, and a word
        // this build does not know says nothing about the target that is worth acting on.
        state: response.state === "stopped" ? "stopped" : "connected",
        ...lastSeenOf(response.last_seen),
        duplicate: response.duplicate === true,
      };
    },
    publish: async (input) => {
      const idempotencyKey = input.idempotencyKey ?? crypto.randomUUID();
      const response = PublishResponseSchema.parse(
        answerJson(
          await post(
            "/v1/messages/publish",
            {
              source: input.source ?? "agent",
              ...sourceSession(input.sourceSessionID),
              topic: input.topic,
              message: input.message,
              ...(input.payload === undefined ? {} : { payload: input.payload }),
              idempotency_key: idempotencyKey,
              ...messageMetadata(input),
              // Omitted unless the caller set it, so an older listener sees an unchanged request.
              ...(input.keep === undefined ? {} : { keep: input.keep }),
            },
            input.sourceSessionID === undefined || input.sourceSessionID === ""
              ? {}
              : { actingSession: input.sourceSessionID }
          ),
          "POST",
          "/v1/messages/publish"
        )
      );
      return {
        envelope: response,
        ...(response.holder === undefined ? {} : { holder: response.holder }),
        ...(response.holder_state === undefined ? {} : { holderState: response.holder_state }),
        ...lastSeenOf(response.last_seen),
        ...(response.recipients === undefined ? {} : { recipients: response.recipients }),
        ...(response.recipients_connected === undefined
          ? {}
          : { recipientsConnected: response.recipients_connected }),
        ...(response.kept === undefined ? {} : { kept: response.kept }),
        ...(response.kept_reason === undefined ? {} : { keptReason: response.kept_reason }),
        ...(response.kept_until === undefined ? {} : { keptUntil: response.kept_until }),
      };
    },
    unregisterSession: async (input) => {
      // The query parameter is omitted without an instance id, so that request stays
      // byte-for-byte what an older client sent and the listener deletes unconditionally.
      const query =
        input.clientInstanceID === undefined
          ? ""
          : `?client_instance_id=${encodeURIComponent(input.clientInstanceID)}`;
      await request(
        `/v1/sessions/${encodeURIComponent(input.sessionID)}${query}`,
        { method: "DELETE" },
        {
          actingSession: input.sessionID,
          ...(input.timeoutMs === undefined
            ? {}
            : { timeoutMs: Math.max(1, input.timeoutMs), retry: false }),
        }
      );
    },
    setRole: async (input) => {
      // `soft`, `previous_session_id` and `client_instance_id` are omitted from
      // the wire unless set, so an older listener sees an unchanged hard-claim
      // request. The claim's own `client_instance_id` is the process it is made
      // for: the listener records it on the claim, and weighs a claim under the
      // holder's own session id against the standing claim's. A soft claim
      // against a live predecessor is still decided by the instance id each
      // side's registry entry carries (`SubscribeInput.clientInstanceID`).
      const soft = input.soft === true;
      const previous = soft ? (input.previousSessionID ?? "") : "";
      const body: {
        session_id: string;
        role: string;
        soft?: true;
        previous_session_id?: string;
        client_instance_id?: string;
      } = {
        session_id: input.sessionID,
        role: input.role,
        ...(soft ? { soft: true } : {}),
        ...(previous === "" ? {} : { previous_session_id: previous }),
        ...(input.clientInstanceID === undefined
          ? {}
          : { client_instance_id: input.clientInstanceID }),
      };
      let raw: string;
      try {
        raw = await post("/v1/roles/set", body, { actingSession: input.sessionID });
      } catch (error) {
        // A soft claim refused by a live holder is an outcome, not a failure:
        // the listener answers 409 with the holder's id.
        if (error instanceof EnvoyApiError && error.details.status === 409) {
          let answer: unknown;
          try {
            answer = JSON.parse(error.details.responseBody);
          } catch {
            // A 409 whose body did not arrive, or is not JSON, names no holder: it stays the refusal.
            throw error;
          }
          const held = RoleHeldWireSchema.safeParse(answer);
          if (held.success) return { claimed: false, holder: held.data.holder };
        }
        throw error;
      }
      return {
        claimed: true,
        interest: InterestWireSchema.parse(answerJson(raw, "POST", "/v1/roles/set")),
      };
    },
    getRole: async (role) =>
      RoleWireSchema.parse(
        answerJson(
          await request(`/v1/roles/${encodeURIComponent(role)}`, { method: "GET" }),
          "GET",
          `/v1/roles/${encodeURIComponent(role)}`
        )
      ),
    listSessions: async (input = {}) => {
      const search = new URLSearchParams();
      if (input.directory !== undefined) search.set("dir", input.directory);
      if (input.title !== undefined) search.set("title", input.title);
      if (input.include !== undefined) search.set("include", input.include);
      const query = search.size === 0 ? "" : `?${search.toString()}`;
      return SessionWireSchema.array().parse(
        answerJson(await request(`/v1/sessions${query}`, {}), "GET", `/v1/sessions${query}`)
      );
    },
  };
}

function waitForRetry(delayMs: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, delayMs);
  return promise;
}

function messageMetadata(input: MessageMetadataInput) {
  return {
    ...(input.inReplyTo === undefined ? {} : { in_reply_to: input.inReplyTo }),
    ...(input.supersedes === undefined ? {} : { supersedes: input.supersedes }),
    ...(input.urgency === undefined ? {} : { urgency: input.urgency }),
    ...(input.expectsReply === undefined ? {} : { expects_reply: input.expectsReply }),
    ...(input.expiresAt === undefined ? {} : { expires_at: input.expiresAt }),
  };
}

/**
 * A blank source session is no source session. The listener's own JSON drops an empty
 * `source_session` on the way back out (`omitempty`), so sending one costs the recipient both
 * the sender label and the reply hint while looking, to the sender, like an attributed message.
 */
function sourceSession(sessionID: string | undefined) {
  return sessionID === undefined || sessionID === "" ? {} : { source_session: sessionID };
}

/**
 * A listener's `last_seen` as a result's `lastSeen`: omitted rather than zero when the listener
 * names none, so a reader never reads 1970 as a last registration.
 */
function lastSeenOf(lastSeen: number | undefined) {
  return lastSeen === undefined || lastSeen <= 0 ? {} : { lastSeen };
}
