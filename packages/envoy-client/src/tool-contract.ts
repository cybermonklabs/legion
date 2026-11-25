import {
  DELIVERY_DUPLICATE_WINDOW_HOURS,
  EnvelopeSchema,
  EXPECTS_REPLY_VALUES,
  type ExpectsReply,
  type SchemaApi,
  type SchemaNode,
  SLACK_POST_TEXT_MAX,
  type ToolArgumentsShape,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import { STORED_TOPIC_KINDS } from "./stream-topics";
import { formatZodIssues, ToolInputError } from "./tool-input-errors";
import {
  type ConnectionState,
  type KeyTakenResult,
  type MessageMetadataInput,
  type PublishResult,
  type SessionInfo,
  type SessionState,
  UNRESPONSIVE_RULE,
} from "./transport";

// The rule itself lives beside the states it is about (`transport.ts`), so the tool
// descriptions, the send and publish confirmations and a refusal's message all say it in one
// wording.

const SESSION_STATE_GUIDE =
  "A session is live, unresponsive or gone, by how long ago it last registered: live within 5 " +
  "minutes, unresponsive after that and within an hour, gone past the hour or with no record at " +
  `all. ${UNRESPONSIVE_RULE} A session named with no state is live. A send answered with state ` +
  "stopped means that session is not running: the message is stored for it and reaches it if " +
  `it runs again within ${DELIVERY_DUPLICATE_WINDOW_HOURS} hours, and nothing of its claims changes.`;

const DELIVERY_CONTRACT =
  "Delivery is at-least-once, possibly out of order across topics; use id for dedupe and at for freshness.";

const PUBLISH_COUNTS =
  "The answer counts who gets it: recipients is every session other than you that the message " +
  `goes to, one that is stopped and gets it when it runs again within ${DELIVERY_DUPLICATE_WINDOW_HOURS} hours included, and ` +
  "recipients_connected is how many of those are running now. A listener that counts neither " +
  "sends neither, and a role topic names its holder instead.";

/** What a subscription and a publish reach, in envoy_subscribe's and envoy_publish's texts. */
export const STORED_DELIVERY =
  `Every notification topic is stored (${STORED_TOPIC_KINDS.join(", ")}; a role message reaches ` +
  "its holder as a stored copy on the holder's agent topic), and reaches each subscribed session " +
  "once from the durable consumer its listener relays to it, also one that was away or restarted. " +
  "A 200 means the stream stored the message for each subscribed session's consumer, not that any " +
  "recipient has read it.";

// The listener's 503 under `MESSAGE_NOT_SENT_PREFIX` (`@legion/contracts`): the bus refused the
// message before anything went out, because the deployed stream does not capture its topic for
// now (a hub move's quiesce). It is temporary, and the transport throws the listener's own text.
export const NOT_SENT_GUIDE =
  "An error that says the message was not sent means nothing was stored, so the same message may " +
  "be sent again later.";

const TOPIC_GUIDE =
  "Topic guide (all are under notifications.): agent.<session_id> (subscribe: own inbox); role.<role> " +
  "(publish-to; holders claim via envoy_role_set). > matches one or more trailing tokens and does not " +
  "match the base subject. Envoy registers the concrete base when you subscribe to <subject>.>, so " +
  "the recommended default remains github.<owner>.<repo>.pr.<n>.>. <owner> and <repo> are one token " +
  "each, with every dot in the name written _ (sjawhar/.github is github.sjawhar._github; " +
  "acme/site.io is github.acme.site_io), and a topic spelled with the dot receives nothing. " +
  "Default PR subscription: " +
  "github.<owner>.<repo>.pr.<n>.> (it receives the quiet PR family): pr.<n> (lifecycle: " +
  "opened/synchronize/closed; closed carries merged, merge_commit_sha, merged_by, head_sha), " +
  "pr.<n>.comment, pr.<n>.review, pr.<n>.mention, pr.<n>.checks (one settlement event per commit whose checks settle, the head or not, naming its sha: " +
  "passed/failed/cancelled/skipped check runs with failing names and URLs, check_runs, each check's latest run as " +
  "{name, id, conclusion} with conclusion GitHub's own word for that run where one is recorded (for example " +
  "success, skipped, neutral or timed_out; a skipped check ran nothing on that commit), and statuses, each commit status as " +
  "{context, state, created_at} with state success, failure or error and created_at the time GitHub created it, " +
  "left out when none is recorded; for each context keep the entry with the later created_at, and keep a context " +
  'that a settlement leaves out as it was; re-fires with superseded_settlement: "true" ' +
  "when new runs appear, or a commit status is new or changes state, for the same commit). Other GitHub: issue.<n>, issue.<n>.comment, " +
  "issue.<n>.mention, mention, push.branch.<name>, push.tag.<name>, workflow.<file>.<action> (only " +
  "runs without an associated PR); slack.<team>.<channel>.message|mention and " +
  "slack.<team>.<channel>.thread.<ts>.message|mention; ghostwispr.<session>.<kind>; " +
  "whatsapp.<phone>.<jid>.<kind>; envoy.exceptions.<original-topic>.";

export type { SchemaApi, ToolArgumentsShape } from "@legion/contracts";

const URGENCY_VALUES = EnvelopeSchema.shape.urgency.unwrap().options as unknown as readonly [
  "low",
  "med",
  "high",
  "blocking",
];

/** Message metadata arguments shared by envoy_send and envoy_publish, built on the host's Zod. */
export function messageMetadataShape<Element extends SchemaNode<Element>>(
  schema: SchemaApi<Element>
): ToolArgumentsShape {
  return {
    in_reply_to: schema.string().optional(),
    supersedes: schema.string().optional(),
    urgency: schema
      .enum(URGENCY_VALUES)
      .describe(
        "low | med | high | blocking: how soon the recipient should act, shown on its card. It does not deliver the message sooner or ahead of others."
      )
      .optional(),
    expects_reply: schema
      .literal(EXPECTS_REPLY_VALUES)
      .describe("false | optional | required")
      .optional(),
    expires_at: schema.number({ int: true }).optional(),
  };
}

export const MessageMetadataSchema = z.object(
  messageMetadataShape(zodSchemaApi(z)) as z.ZodRawShape
);

export type MessageMetadataArguments = {
  readonly in_reply_to?: string;
  readonly supersedes?: string;
  readonly urgency?: (typeof URGENCY_VALUES)[number];
  readonly expects_reply?: ExpectsReply;
  readonly expires_at?: number;
};

/** Converts validated tool-wire metadata into the Envoy client's camel-case input. */
export function toMessageMetadata(args: MessageMetadataArguments): MessageMetadataInput {
  return {
    ...(args.in_reply_to === undefined ? {} : { inReplyTo: args.in_reply_to }),
    ...(args.supersedes === undefined ? {} : { supersedes: args.supersedes }),
    ...(args.urgency === undefined ? {} : { urgency: args.urgency }),
    ...(args.expects_reply === undefined ? {} : { expectsReply: args.expects_reply }),
    ...(args.expires_at === undefined ? {} : { expiresAt: args.expires_at }),
  };
}

function messageArguments<Element extends SchemaNode<Element>>(
  schema: SchemaApi<Element>
): ToolArgumentsShape {
  return { message: schema.string(), ...messageMetadataShape(schema) };
}

export const EnvoyToolOperation = {
  subscribe: "subscribe",
  unsubscribe: "unsubscribe",
  listInterests: "listInterests",
  inbox: "inbox",
  send: "send",
  publish: "publish",
  setRole: "setRole",
  getRole: "getRole",
  whoami: "whoami",
  listSessions: "listSessions",
  slackPost: "slackPost",
} as const;

export type EnvoyToolOperation = (typeof EnvoyToolOperation)[keyof typeof EnvoyToolOperation];

/**
 * What each operation's arguments parse to. The shapes above validate on the host's Zod and
 * come back untyped, so this is the typed view a host reads after `parse`; keep it in step
 * with the builders.
 */
export interface ToolArgumentsByOperation {
  readonly subscribe: { readonly topics: readonly string[] };
  readonly unsubscribe: { readonly topics?: readonly string[] };
  readonly listInterests: Record<string, never>;
  readonly inbox: Record<string, never>;
  readonly send: MessageMetadataArguments & {
    readonly session_id: string;
    readonly message: string;
    readonly idempotency_key?: string;
  };
  readonly publish: MessageMetadataArguments & { readonly topic: string; readonly message: string };
  readonly setRole: { readonly role: string };
  readonly getRole: { readonly role: string };
  readonly whoami: Record<string, never>;
  readonly listSessions: {
    readonly machine?: string;
    readonly dir?: string;
    readonly title?: string;
  };
  readonly slackPost: {
    readonly channel: string;
    readonly thread_ts: string;
    readonly text: string;
  };
}

export type ToolSpec = {
  readonly name: string;
  readonly description: string;
  readonly arguments: <Element extends SchemaNode<Element>>(
    schema: SchemaApi<Element>
  ) => ToolArgumentsShape;
  readonly operation: EnvoyToolOperation;
};

export const envoyToolSpecs = [
  {
    name: "envoy_subscribe",
    description: `Subscribe this session to Envoy notification topics. A notifications. topic no stream subject captures is refused by name: nothing would store it, so you would receive nothing on it; subscribe to a notifications.team.<name> topic instead. ${STORED_DELIVERY} ${TOPIC_GUIDE}`,
    arguments: (schema) => ({
      topics: schema.array(schema.string()).describe("NATS-style topic patterns to subscribe to."),
    }),
    operation: EnvoyToolOperation.subscribe,
  },
  {
    name: "envoy_unsubscribe",
    description:
      "Unsubscribe this session from Envoy topics, or remove all current subscriptions if topics are omitted. A subscription another plugin holds for this session's role stays, and the result lists it as kept.",
    arguments: (schema) => ({ topics: schema.array(schema.string()).optional() }),
    operation: EnvoyToolOperation.unsubscribe,
  },
  {
    name: "envoy_list",
    description:
      "List the current Envoy topic subscriptions for this session so you can confirm the exact topic shapes that are active.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.listInterests,
  },
  {
    name: "envoy_inbox",
    description: "List this Pi session's 50 most recent rendered Envoy deliveries, newest first.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.inbox,
  },
  {
    name: "envoy_send",
    description: `Send an Envoy agent-to-agent message directly to another session by session ID. The confirmation names the recipient's state. A message to an unresponsive session is stored and reaches it when it answers again, so an unresponsive recipient is not a failed send and no reason to send to someone else instead. ${SESSION_STATE_GUIDE} ${DELIVERY_CONTRACT} A send that is not confirmed delivered errs naming its idempotency_key; send the same message again with that idempotency_key within ${DELIVERY_DUPLICATE_WINDOW_HOURS} hours, and Envoy drops it as a repeat if the first copy arrived. ${NOT_SENT_GUIDE}`,
    arguments: (schema) => ({
      session_id: schema
        .string()
        .describe("Target session ID; find it with envoy_sessions or envoy_whoami."),
      ...messageArguments(schema),
      idempotency_key: schema
        .string()
        .describe(
          "Only to send again a message whose envoy_send was not confirmed delivered: the idempotency_key that error named. Omit it for a new message."
        )
        .optional(),
    }),
    operation: EnvoyToolOperation.send,
  },
  {
    name: "envoy_publish",
    description: `Publish an Envoy message to a notifications. topic. ${STORED_DELIVERY} A role or built-in topic is delivered as its guide says. A role message has three possible answers. First, a live session holds the role, and the stream stores the forward for that holder's consumer. Its 200 says the stream stored it. It does not say that the holder read it. It names the holder and the state the listener resolved the holder in. Second, no live session holds the role: nobody claims it, or its holder is unresponsive or gone. The listener then keeps the message for one hour. This is a success. The result says that the listener kept the message, and its kept_until is the time that hour ends. For a holder that is not live, the result also names that holder and its state. The next session to hold the role live before kept_until gets the message. That can be the same holder when it answers again. If no session takes the message by kept_until, Envoy drops it. If the publish named your session as its source, Envoy then sends your session a message that names the dropped message, unless it dropped it more than an hour late. Third, the listener keeps nothing and errs, which tells you to reach the agent some other way: the holder refuses the message, or the forward is not stored within four seconds. A team topic (such as notifications.team.deploy-squad.all) is stored like every other, so a session subscribed to it that was away still receives it when it returns, and one that subscribes after you publish does not. A notifications. topic no stream subject captures is refused by name: nothing would store it. ${NOT_SENT_GUIDE} ${PUBLISH_COUNTS} ${SESSION_STATE_GUIDE} ${DELIVERY_CONTRACT}`,
    arguments: (schema) => ({ topic: schema.string(), ...messageArguments(schema) }),
    operation: EnvoyToolOperation.publish,
  },
  {
    name: "envoy_role_set",
    description:
      "Set the current session as the holder of a named role. Messages published to notifications.role.<role> route to this session. Moving to a different role is refused while another plugin holds subscriptions that last only as long as this session keeps its current role.",
    arguments: (schema) => ({ role: schema.string() }),
    operation: EnvoyToolOperation.setRole,
  },
  {
    name: "envoy_role_get",
    description: `Get the holder of a named Envoy role, with its holder_state and the holder's last_seen. A holder that is only unresponsive still holds the role: nothing a lookup does releases its claim, and only a gone holder leaves the role unheld. ${SESSION_STATE_GUIDE}`,
    arguments: (schema) => ({ role: schema.string() }),
    operation: EnvoyToolOperation.getRole,
  },
  {
    name: "envoy_whoami",
    description:
      "Returns this session's Envoy identity: session ID, machine ID, port, and directory. session_id is the address a reply reaches. Where a host runs a task subagent inside its parent's process, such a subagent registers no Envoy session of its own, so its session_id is the parent session that spawned it and the result says so.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.whoami,
  },
  {
    name: "envoy_sessions",
    description:
      "List the Envoy sessions that are live or unresponsive; a gone session is listed by neither. " +
      "Filter by optional machine, directory, or title. Each row carries its state and its " +
      `last_seen, the epoch milliseconds of its last registration. ${SESSION_STATE_GUIDE}`,
    arguments: (schema) => ({
      machine: schema.string().optional(),
      dir: schema.string().optional(),
      title: schema.string().optional(),
    }),
    operation: EnvoyToolOperation.listSessions,
  },
  {
    name: "slack_post",
    description:
      "Reply in a Slack thread as the Legion Slack app, with the bot token the secrets broker grants this session (LEGION_SLACK_BOT_TOKEN); the session never holds it. channel is the Slack channel id (a mention's channel_id); thread_ts is the thread's root ts: the mention's thread_ts, or its own ts when it has none. Answers the new message's ts. A refusal carries Slack's own error code (not_in_channel: the app is not in that channel) or the broker's state (waiting for approval, denied). Sent once and never retried: after 'Slack did not answer' the reply may already be in the thread, so do not post it again.",
    arguments: (schema) => ({
      channel: schema.string({ min: 1 }).describe("Slack channel id, such as C0123ABCD."),
      thread_ts: schema
        .string({ min: 1 })
        .describe("The thread's root ts, which keeps the reply in the mention's thread."),
      text: schema
        .string({ min: 1, max: SLACK_POST_TEXT_MAX })
        .describe(`The reply, in Slack mrkdwn, at most ${SLACK_POST_TEXT_MAX} characters.`),
    }),
    operation: EnvoyToolOperation.slackPost,
  },
] as const satisfies readonly ToolSpec[];

/**
 * When a stopped session was last seen, from `last_seen` (unix milliseconds), as a UTC instant to
 * the second: `2026-11-02T09:15:00Z`. An instant rather than "4 minutes ago", so the line means
 * the same whenever it is read again, in a transcript a day later included, and a test pins it
 * without a clock.
 */
function lastSeenText(lastSeen: number): string {
  return `${new Date(lastSeen).toISOString().slice(0, 19)}Z`;
}

/**
 * What a send says when the listener already held it under this target and idempotency key: the
 * resend stored nothing, so one copy is there and no second one.
 */
const STORED_ONCE =
  "an earlier attempt under the same idempotency_key was already stored, so this one stored no " +
  "second copy";

/**
 * The envoy_send confirmation line both hosts show the model.
 *
 * A stopped target (`state`, what the listener's connection check found) is queued: Envoy stored
 * the message on that session's topic and it arrives if the session runs again within the
 * stream's {@link DELIVERY_DUPLICATE_WINDOW_HOURS} hours. The queued line says `queued`, never
 * `sent`, because nothing read the message and a model told "sent" waits for an answer that is
 * not coming.
 *
 * A connected target is sent to, and the line names its registry state and, for one that stopped
 * answering, since when (`lastSeen`, the listener's record of its last registration): a send to
 * an unresponsive session is stored for it, not a delivery that failed, and how long it has been
 * silent is what an agent judges the wait by.
 *
 * A `duplicate` answer is a resend of a message the listener already stored under the same
 * idempotency key. It stored nothing this time. A current listener answers the stored copy, whose
 * event id is the first send's, but an older one answers the event id it minted for the resend,
 * which names no stored copy, and the answer does not say which listener wrote it, so the line
 * leaves the id out; the target's state still chooses the verb, because a stopped target's one
 * stored copy is waiting rather than delivered.
 *
 * `confirmed` is false only for a listener too old to echo the recipient, which is also too old
 * to answer a state, so the unconfirmed note belongs to the connected line alone.
 */
export function sendConfirmationText(result: {
  readonly envelope: { readonly event_id: string };
  readonly recipient: string;
  readonly recipientState: SessionState;
  readonly state: ConnectionState;
  readonly confirmed: boolean;
  readonly lastSeen?: number;
  readonly duplicate: boolean;
}): string {
  if (result.state === "stopped") {
    const seen =
      result.lastSeen === undefined ? "" : ` (last seen ${lastSeenText(result.lastSeen)})`;
    const waits = `the session is stopped${seen}; if it runs again within ${DELIVERY_DUPLICATE_WINDOW_HOURS} hours, it gets the message`;
    if (result.duplicate) {
      return `queued once for ${result.recipient}: ${STORED_ONCE}; ${waits}`;
    }
    return `queued ${result.envelope.event_id} for ${result.recipient}: ${waits}`;
  }
  if (result.duplicate) {
    return `sent to ${result.recipient} once: ${STORED_ONCE}`;
  }
  const since =
    result.lastSeen === undefined ? "" : ` since ${new Date(result.lastSeen).toISOString()}`;
  const state =
    result.recipientState === "unresponsive"
      ? `unresponsive${since}, so it is stored until that session answers; ${UNRESPONSIVE_RULE}`
      : result.recipientState;
  return `sent ${result.envelope.event_id} to ${result.recipient} (${state})${result.confirmed ? "" : " (recipient unconfirmed by listener)"}`;
}

/**
 * How the line for a taken key says to deliver the message after all, for each tool that can get
 * one: `envoy_send` mints a new idempotency_key when the call names none (its idempotency_key is
 * only for a send that was not confirmed), and `envoy_publish` takes none and mints one for every
 * publish.
 */
const KEY_TAKEN_AGAIN = {
  envoy_send: "send it again without an idempotency_key; Envoy gives it a new one",
  envoy_publish: "publish it again; Envoy gives every publish a new idempotency_key",
} as const;

/**
 * The line both hosts show the model for a send or a publish whose idempotency key another
 * sender's message already holds ({@link KeyTakenResult}): nothing was delivered for this caller,
 * and the line names that message's event id so the model does not read the key as its own, and
 * says how `tool` delivers the message after all.
 */
export function keyTakenText(result: KeyTakenResult, tool: keyof typeof KEY_TAKEN_AGAIN): string {
  return `not delivered: another sender's message (${result.keyTaken.eventId}) already holds this idempotency_key, so Envoy stored nothing for you; ${KEY_TAKEN_AGAIN[tool]}`;
}

/** The fields of a key-taken result both hosts hand the model beside {@link keyTakenText}, under
 *  the listener's own snake_case names. */
export function keyTakenDetails(result: KeyTakenResult) {
  return {
    event_id: result.keyTaken.eventId,
    topic: result.keyTaken.topic,
    delivered: false,
    key_taken: true,
  } as const;
}

/**
 * The envoy_publish confirmation line both hosts show the model.
 *
 * A publish to a topic the stream captures is stored once and read by each subscribed session's
 * own consumer, so its answer counts the sessions other than the sender the topic is for
 * (`recipients`) and how many of those run now (`recipientsConnected`), and the line says both. A
 * count of zero is the answer that matters most — nobody subscribes, so the message reaches
 * nobody — and the line names the topic. A listener that cannot count (one whose NATS user cannot
 * read the known-session bucket, or whose copy of the known-session or interest bucket cannot be
 * served: its first read not complete, or past its stale bound) sends no count, which reads as the
 * plain line every release wrote before the counts existed, and one whose copy of the session
 * registry cannot be served sends `recipients` alone, which the line gives without the running
 * count.
 *
 * A role publish carries no count: it names its holder and the state the listener resolved the
 * holder in. A kept answer says until when Envoy keeps the message for the role, and why no live
 * session holds the role: nobody claims it, or its holder is not live, which the line names with
 * its state and, for an unresponsive one, with the rule that such a session is not dead.
 * `senderNamed` is whether the publish named the sending session as its source: only then does the
 * listener send that session a message when no session takes the copy in its hour, unless it drops
 * the copy more than an hour late, so only then does the line promise one. It is required, so a
 * host that leaves it out fails to compile rather than promise a message that never comes. A
 * listener of an earlier release names no time and sends no such message, so its kept answer
 * reads as kept with no time.
 */
export function publishConfirmationText(
  result: {
    readonly envelope: { readonly event_id: string; readonly topic: string };
    readonly holder?: string;
    readonly holderState?: SessionState;
    readonly kept?: boolean;
    readonly keptReason?: PublishResult["keptReason"];
    readonly keptUntil?: number;
    readonly recipients?: number;
    readonly recipientsConnected?: number;
  },
  options: { readonly senderNamed: boolean }
): string {
  const published = `published ${result.envelope.event_id}`;
  if (result.recipients === 0) {
    return `${published}; no other session subscribes to ${result.envelope.topic}, so no other session gets it`;
  }
  if (result.recipients !== undefined) {
    // One recipient takes the singular: "to 1 other session (1 running now)". A model reading
    // "1 other sessions" reads a bug in the sender before it reads the count.
    const sessions =
      result.recipients === 1 ? "1 other session" : `${result.recipients} other sessions`;
    const running =
      result.recipientsConnected === undefined
        ? ""
        : ` (${result.recipientsConnected} running now)`;
    return `${published} to ${sessions}${running}`;
  }
  const sentences = [`${published}.`];
  // A kept answer is a success of its own: no live session holds the role, so the listener holds
  // the message on the role's kept lane and no holder received it. A holder that is not live is
  // named as one, never as the session that took the message.
  if (result.kept === true) {
    const lapsed = result.keptReason === "holder_lapsed";
    const holder = result.holder === undefined ? "" : ` ${result.holder}`;
    const why = lapsed
      ? `its holder${holder} is ${result.holderState ?? "not live"}, so no live session holds the role now`
      : "no live session holds the role now";
    const until =
      result.keptUntil === undefined ? "" : ` until ${new Date(result.keptUntil).toISOString()}`;
    sentences.push(`Envoy kept it for the role${until}: ${why}.`);
    sentences.push(
      result.keptUntil === undefined
        ? "Envoy did not say until when it keeps it."
        : "The next session to hold the role before then gets it."
    );
    if (result.keptUntil !== undefined && options.senderNamed) {
      sentences.push(
        "If none does, Envoy drops it and sends you a message that names it, unless it drops it more than an hour late."
      );
    }
    // Last, so that "before then" above can only mean kept_until, never a time the rule names.
    if (lapsed && result.holderState === "unresponsive") sentences.push(UNRESPONSIVE_RULE);
  } else if (result.holder !== undefined) {
    const state = result.holderState === undefined ? "" : ` (${result.holderState})`;
    sentences.push(`Its role holder is ${result.holder}${state}.`);
  }
  return sentences.join(" ");
}

/** The fields of a publish answer both hosts hand the model beside its confirmation line
 *  (`publishConfirmationText`), under the listener's own snake_case names, each one only when the
 *  answer carries it. Each host adds its own fields around them. */
export function publishResultDetails(
  result: Pick<
    PublishResult,
    | "holder"
    | "holderState"
    | "lastSeen"
    | "recipients"
    | "recipientsConnected"
    | "kept"
    | "keptReason"
    | "keptUntil"
  >
) {
  return {
    ...(result.holder === undefined ? {} : { holder: result.holder }),
    ...(result.holderState === undefined ? {} : { holder_state: result.holderState }),
    ...(result.lastSeen === undefined ? {} : { last_seen: result.lastSeen }),
    ...(result.recipients === undefined ? {} : { recipients: result.recipients }),
    ...(result.recipientsConnected === undefined
      ? {}
      : { recipients_connected: result.recipientsConnected }),
    ...(result.kept === undefined ? {} : { kept: result.kept }),
    ...(result.keptReason === undefined ? {} : { kept_reason: result.keptReason }),
    ...(result.keptUntil === undefined ? {} : { kept_until: result.keptUntil }),
  };
}

/**
 * The sessions an `envoy_sessions` call's `machine` argument names, which both hosts filter for
 * themselves: the listener's `/v1/sessions` takes no machine. A session answers for two machines:
 * `host`, where its process runs, and `machine_id`, the listener it registered through. A filter
 * naming either one is that machine's filter, so a hub's id (`envoy-hub`, `gke`) lists the
 * sessions registered through it. No filter — the argument absent or empty — lists them all.
 */
export function sessionsOnMachine(
  sessions: readonly SessionInfo[],
  machine: string | undefined
): readonly SessionInfo[] {
  if (machine === undefined || machine === "") return sessions;
  return sessions.filter((session) => session.host === machine || session.machine_id === machine);
}

// The specs are frozen module constants, so each operation's schema is built once and shared;
// parsing never mutates it.
const envoyToolSchemas = new Map<EnvoyToolOperation, z.ZodType>();

/**
 * The idempotency key envoy_send takes: the transport's `crypto.randomUUID()`. The listener stores
 * a send once per target and key only for a key of this shape (`MINTED_DEDUPE_KEY_PATTERN`), so a
 * resend under any other key would be a second delivery.
 */
const SEND_IDEMPOTENCY_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Validates an Envoy tool call on real zod in one pass and returns its typed arguments,
 * or throws a `ToolInputError` naming every problem. Hosts register the tools with
 * lenient argument validation, so this is the single place a bad call is refused.
 */
export function parseEnvoyToolArguments<Operation extends EnvoyToolOperation>(
  operation: Operation,
  parameters: Record<string, unknown>
): ToolArgumentsByOperation[Operation] {
  const spec = envoyToolSpecs.find((candidate) => candidate.operation === operation);
  if (spec === undefined) throw new Error(`missing Envoy tool specification for ${operation}`);
  let schema = envoyToolSchemas.get(operation);
  if (schema === undefined) {
    schema = z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape).strict();
    envoyToolSchemas.set(operation, schema);
  }
  const parsed = schema.safeParse(parameters, { reportInput: true });
  if (!parsed.success) {
    throw new ToolInputError(spec.name, formatZodIssues(parsed.error.issues, schema, spec.name));
  }
  const { data } = parsed;
  if (
    typeof data === "object" &&
    data !== null &&
    "idempotency_key" in data &&
    typeof data.idempotency_key === "string" &&
    !SEND_IDEMPOTENCY_KEY.test(data.idempotency_key)
  ) {
    throw new ToolInputError(spec.name, [
      `idempotency_key must be the key an unconfirmed send named, a lower-case UUID; got ${JSON.stringify(data.idempotency_key)}`,
    ]);
  }
  return data as ToolArgumentsByOperation[Operation];
}
