import {
  DELIVERY_ATTEMPT_STATES,
  DELIVERY_CAPABILITIES,
  DELIVERY_DUPLICATE_WINDOW_HOURS,
  DELIVERY_DUPLICATE_WINDOW_MS,
  type DeliveryAttemptState,
  type MessageDeliveryMode,
  POSSIBLY_DELIVERED_CAUSES,
} from "@legion/contracts";

import type { AgentPart } from "../refs/AgentName";
import type { PostDeletion } from "../refs/post-marks";

/**
 * Each delivery mode as a person reads it, the one mapping every surface names a mode with: the
 * conversation composer's picker, the Agents list's card headlines and attempt lines, the
 * mention composer's warning, the broadcast's exclusions and the issue event feed all say what the
 * composer said. Send is Enter at the session's terminal, the wire's `steer`.
 */
export const MODE_LABELS: Record<MessageDeliveryMode, string> = {
  aside: "Aside",
  btw: "BTW",
  steer: "Send",
};

/** A delivery mode as Dispatch stored it on an attempt or a broadcast, as a person reads it: its
 *  mode's label, or the string as it was written when this dashboard knows no such mode. */
export function capabilityLabel(capability: string): string {
  const mode = deliveryMode(capability);
  return mode === undefined ? capability : MODE_LABELS[mode];
}

/** `value` as a delivery mode, or `undefined` where this dashboard knows no such mode: a mode
 *  reaches the browser as the string the route table declares, and `DELIVERY_CAPABILITIES` is the
 *  closed list that string is one of. Every surface that needs the mode itself narrows here. */
export function deliveryMode(value: string): MessageDeliveryMode | undefined {
  return DELIVERY_CAPABILITIES.find((known) => known === value);
}

/**
 * The delivery modes among the capabilities a session lists, in list order and each once. A list
 * can also hold capabilities that are no mode this Dispatch knows - a newer plugin's, say - so an
 * empty list is not the only list that takes no message: whether a session takes messages is
 * whether this is empty, never whether its list is. Every surface that decides it reads it here.
 */
export function deliveryModesOf(capabilities: readonly string[]): MessageDeliveryMode[] {
  const modes: MessageDeliveryMode[] = [];
  for (const capability of capabilities) {
    const mode = deliveryMode(capability);
    if (mode !== undefined && !modes.includes(mode)) modes.push(mode);
  }
  return modes;
}

/**
 * Why sessions whose capabilities hold no delivery mode (`deliveryModesOf`) take no message: the
 * sentences that follow a warning naming them. An empty list is not the only list that takes no
 * message, and the two have different causes, so each cause is said once, of the sessions it is
 * true of: a session lists nothing while its plugin cannot reach its conversation, and a list whose
 * every capability is unknown here says nothing of that. The second names its sessions only beside
 * sessions the first is about; on its own, `it` or `they` already reads as the sessions warned of.
 * The live view, the Inbox and the mention composer all say it from here, so they cannot differ.
 */
export function noDeliveryReasons(
  sessions: readonly { readonly capabilities: readonly string[]; readonly title: string }[]
): string {
  const listing = sessions.filter((session) => session.capabilities.length > 0);
  const reasons: string[] = [];
  if (listing.length < sessions.length) {
    reasons.push("A session advertises none while its plugin cannot reach its conversation.");
  }
  if (listing.length > 0) {
    const subject =
      listing.length < sessions.length
        ? listing.map((session) => session.title).join(" and ")
        : listing.length > 1
          ? "they"
          : "it";
    reasons.push(
      `None of the capabilities ${subject} ${listing.length > 1 ? "list" : "lists"} is a delivery mode this Dispatch knows.`
    );
  }
  return reasons.join(" ");
}

/** What the live view and the Inbox say of a listed session whose capabilities hold no delivery
 *  mode (`deliveryModesOf`): a message sent to it records a failed attempt, and why it takes none
 *  (`noDeliveryReasons`). Undefined for a list that holds a mode, so no surface can say it of a
 *  session that takes messages. */
export function noDeliveryLine(capabilities: readonly string[]): string | undefined {
  if (deliveryModesOf(capabilities).length > 0) return undefined;
  return `This session advertises no delivery mode, so a message sent here records a failed attempt. ${noDeliveryReasons([{ capabilities, title: "This session" }])}`;
}

/** `value` as an attempt's state. Dispatch writes one of the four `DELIVERY_ATTEMPT_STATES` the
 *  route table carries as a plain string, and each is kept as it is: a queued attempt is its own
 *  state, which no surface may draw as sent (INFRA-626). One this build does not know reads as
 *  `sent`, which is what every surface already showed for it - each tests for the states it
 *  knows, and draws anything else as sent. */
export function deliveryOutcomeState(value: string): DeliveryOutcome["state"] {
  return DELIVERY_ATTEMPT_STATES.find((known) => known === value) ?? "sent";
}

/**
 * The mode a message with no `/aside ` or `/btw ` command goes in when it answers no thread, from
 * the capabilities of each live session it addresses: Send when every one of them advertises it,
 * else Aside when every one of them advertises that - so a plain message to a session that takes
 * only Aside is an Aside rather than a Send recorded as failed. A session whose list holds no
 * delivery mode (`deliveryModesOf`) - an empty list, or one of capabilities this Dispatch knows no
 * mode for - takes none, so it does not decide the mode for the rest. BTW is never picked for the
 * reader: it answers in a side turn that leaves the conversation as it was, which a plain message
 * does not ask for. Otherwise, or with nobody live to ask, it is a Send, which the composer then
 * warns about.
 */
export function plainDelivery(capabilities: readonly (readonly string[])[]): MessageDeliveryMode {
  const taking = capabilities.filter((list) => deliveryModesOf(list).length > 0);
  const everyone = (mode: MessageDeliveryMode) => taking.every((list) => list.includes(mode));
  return taking.length > 0 && !everyone("steer") && everyone("aside") ? "aside" : "steer";
}

/**
 * The mode a broadcast goes in until its sender picks one, from the capabilities of each live
 * session selected: BTW, the broadcast's own default, when every one of them advertises it, else
 * Aside when every one advertises that - so a selection that holds a session taking only Aside
 * reaches it rather than leaving it out. A session whose list holds no delivery mode
 * (`deliveryModesOf`), an empty list included, takes none and does not decide it. Otherwise, or
 * with nobody live, it is BTW, and the composer names who that leaves out.
 */
export function broadcastDelivery(
  capabilities: readonly (readonly string[])[]
): MessageDeliveryMode {
  const taking = capabilities.filter((list) => deliveryModesOf(list).length > 0);
  const everyone = (mode: MessageDeliveryMode) => taking.every((list) => list.includes(mode));
  return taking.length > 0 && !everyone("btw") && everyone("aside") ? "aside" : "btw";
}

/**
 * The one rule every delivery surface applies, so the dashboard cannot promise on one card what
 * it contradicts on another. Four surfaces render a delivery attempt — the targeted-message
 * card, the comment thread's "Mention deliveries" list, the issue event feed, and the broadcast
 * page's row for a send nobody is carrying — and each of them asks these functions rather than
 * spelling the condition or the sentence again. What the promise rests on is stated once, on
 * `DELIVERY_DUPLICATE_WINDOW_MS` in `@legion/contracts`.
 */

/** The shape every delivery attempt shares, whatever its row type spells the fields. */
export interface DeliveryOutcome {
  readonly state: DeliveryAttemptState;
  /** ISO-8601, as both `MessageDelivery.created_at` and `CommentDelivery.created_at` carry it. */
  readonly createdAt: string;
  readonly duplicate?: boolean;
  /** The session the attempt went to answered it with an error instead of a reply (a BTW whose
   *  side turn failed, a frame the host refused), as `rowAnsweredWithError` reads it off the
   *  attempt. Absent means it did not. */
  readonly answeredWithError?: boolean;
}

/**
 * Whether an attempt row records its session's error answer. Dispatch writes an envelope id only
 * for a send the listener accepted (`sendResolvedDelivery`), and every failure Dispatch records
 * itself leaves it null, so a failed row that carries one was failed by the session's own error
 * reply after Dispatch recorded the send. A session that refuses the frame before Dispatch
 * records the send leaves a row no different from a failed send, which a row cannot tell apart.
 */
export function rowAnsweredWithError(row: {
  readonly state: string;
  readonly envelope_id: string | null;
}): boolean {
  return row.state === "failed" && row.envelope_id !== null;
}

/**
 * The one case the promise misses inside the window: a host holds the keys it was handed in
 * memory, so one that restarted after the first send (`claude --resume`, a resumed pod) no
 * longer recognises its repeat.
 */
const unlessRestarted = "unless the session restarted since it was sent";

/**
 * Whether a send made at `createdAt` can still be recognised as a repeat, so re-sending it in its
 * own mode cannot deliver it twice.
 *
 * A NEGATIVE age is not rejected. `createdAt` is stamped by Postgres and `now` is the browser's
 * clock; nothing reconciles them, so a client running behind the server sees a just-failed
 * attempt as dated in the future. That attempt is the youngest there is and certainly inside the
 * window — and because this decides whether the same-mode Retry renders at all, rejecting it
 * would hide the safe action and leave only the mode-change buttons, which are a genuine second
 * delivery. `Number.isFinite` still rejects an unparseable stamp.
 */
function insideDuplicateWindow(createdAt: string, now: number): boolean {
  const age = now - Date.parse(createdAt);
  return Number.isFinite(age) && age < DELIVERY_DUPLICATE_WINDOW_MS;
}

/**
 * Whether re-sending this attempt in its OWN mode can still be promised not to deliver twice.
 *
 * A same-mode retry repeats the dedupe key of the send before it, and a repeat is recognised only
 * inside `DELIVERY_DUPLICATE_WINDOW_MS` (`insideDuplicateWindow`). Past the window nothing
 * remembers the key, and the same retry is a second delivery - the very defect this promise
 * exists to rule out.
 *
 * An attempt already recorded `duplicate` is excluded for a different reason: it reached the
 * listener and changed nothing, so there is no failure left to retry.
 *
 * So is an attempt the session answered with an error, because its Retry could not be promised
 * to arrive. The notification stream stored the first frame under that key, so it stores the
 * Retry no more than any other repeat, and a session the listener pushes to from the stream is
 * never handed it while the attempt reads as a duplicate. The dashboard cannot tell that session
 * from one whose host takes the Retry, so it points at a mode change, a new key, for both
 * (`answeredWithErrorGuidance`).
 *
 * A queued attempt needs no clause of its own: it is not `failed`. The listener took it and
 * stored it for a session that is not running, so there is nothing to retry, and a Retry would
 * store a second copy for the same session to read when it comes back.
 */
export function isSafeRetry(attempt: DeliveryOutcome, now: number = Date.now()): boolean {
  return (
    attempt.state === "failed" &&
    attempt.duplicate !== true &&
    attempt.answeredWithError !== true &&
    insideDuplicateWindow(attempt.createdAt, now)
  );
}

/**
 * Whether a surface may offer to send a post again - the same-mode Retry of one of its attempts,
 * or a send in another mode - given the post's deletion (`postDeletion`): never once its author
 * deleted it. Dispatch refuses every new delivery of a deleted post, `409 POST_DELETED`, and the
 * delete ended each of its sends still pending with `DELETED_POST_DELIVERY_ERROR`. So an attempt
 * of a deleted post offers no Retry and no mode change, and earns none of the guidance that
 * names them: the one a delete ended reads the delete's words and nothing else. Every surface
 * that offers either asks this first, and asks `isSafeRetry` and `failureGuidance` only of an
 * attempt whose post it holds for.
 */
export function resendable(deleted: PostDeletion | null | undefined): boolean {
  return deleted === null || deleted === undefined;
}

/** What a duplicate attempt reads as: an earlier attempt landed, so this one added nothing. */
export const duplicateText = `Delivered by an earlier attempt, so not delivered again ${unlessRestarted}`;

/** What an attempt the post's delete ended reads as (`DELETED_POST_DELIVERY_ERROR`): the delete
 *  ended the send, so it did not fail. The targeted message card and the Conversation tab's
 *  mention list both say it in these words. */
export const endedByDeleteText = "Ended by the post's delete";

/** What a queued attempt's session is and when it gets the message, said after the session's
 *  name: the one wording of a queued attempt stored for a session, which `queuedParts` and the
 *  Activity tab's mention and message lines (`activityDescription`) read. The hours are the
 *  stream's retention, the same window in which a repeat is recognised
 *  (`DELIVERY_DUPLICATE_WINDOW_MS`). An attempt kept for a role reads `keptForRoleWait` instead. */
export const queuedWait = `is stopped; it gets the message if it runs again within ${DELIVERY_DUPLICATE_WINDOW_HOURS} hours`;

/** What a queued attempt reads as on the Agents page's card (`TargetedMessageCard`), the
 *  Conversation tab's delivery line (`ConversationTab`) and the search bar's send
 *  (`SendToAgentOffer`): the listener took the message for a session it knows and that is not
 *  running, and stores it until that session runs again. `session` is the recipient as the
 *  surface draws it, so a card names it through `AgentName` and a plain line names it as text. */
export function queuedParts(session: AgentPart): AgentPart[] {
  return ["Queued: ", session, ` ${queuedWait}`];
}

/** Who gets a message kept for a role, and until when, said after the role: the one wording of a
 *  queued attempt kept for a role (`keptForRole`), which `keptForRoleParts` and the Activity tab's
 *  mention and message lines read. The hour is how long the listener keeps a message for a role
 *  no live session holds, from the moment it stored it (`roleKeepWindow` in
 *  packages/envoy/cmd/listener/role_keep.go, answered as the publish's `kept_until`). No contract
 *  carries that window, so it is stated here. */
export const keptForRoleWait =
  "the role's next session gets it if one takes the role within one hour";

/** Whether a queued attempt was kept for its role rather than stored for a session. Dispatch
 *  records a queued attempt that names no session for one target only: a role no live session
 *  holds and the Legion agent registry holds no entry for, whose project's daemon answers, so the
 *  listener keeps the message on the role's kept lane for the role's first session
 *  (sjawhar/legion#2230). A queued attempt stored for a session always names that session, a
 *  role's holder included. `target` is the delivery's address, and `sessionId` the session its
 *  attempt recorded, empty or null for none. */
export function keptForRole(
  target: string | null | undefined,
  sessionId: string | null | undefined
): boolean {
  return (sessionId ?? "") === "" && target?.startsWith("role:") === true;
}

/** What a queued attempt kept for a role reads as on the targeted-message card
 *  (`TargetedMessageCard`) and the Conversation tab's mention line (`ConversationTab`): the
 *  listener holds the message for the role, and the role's next session gets it inside the keep
 *  window. `role` is the role's address as the surface draws it. */
export function keptForRoleParts(role: AgentPart): AgentPart[] {
  return ["Kept for ", role, `: ${keptForRoleWait}`];
}

/**
 * The guidance a failed attempt earns while a same-mode retry is still safe.
 *
 * "Retry won't deliver it twice" holds for any in-window failure but a restarted session's: if
 * the send landed, its repeat is recognised and dropped, and if it never landed there is nothing
 * to duplicate. The second clause does not. "Sending in a different mode delivers it again" is
 * only true of a send that may already have reached the recipient — a receipt timeout, or a send
 * whose store the listener could not confirm (`POSSIBLY_DELIVERED_CAUSES`) — so it is keyed on
 * those causes. It is also withheld on the mention surface, which has no mode-change button to
 * name.
 */
export function safeRetryGuidance(surface: "card" | "mention", cause?: string | null): string {
  const safe = `Retry won't deliver it twice ${unlessRestarted}`;
  if (surface === "mention" || cause == null || !POSSIBLY_DELIVERED_CAUSES.includes(cause)) {
    return `${safe}.`;
  }
  return `${safe}; sending in a different mode delivers it again.`;
}

/**
 * The guidance an attempt the session answered with an error earns in place of a Retry: send it
 * again under a new key. On a card that is one of the mode-change actions beside it; the mention
 * surface has none, so a new comment is its new key.
 */
export function answeredWithErrorGuidance(surface: "card" | "mention"): string {
  const answered = "The session answered with an error";
  return surface === "card"
    ? `${answered}; send it in another mode instead.`
    : `${answered}; mention it in a new comment to send it again.`;
}

/**
 * The guidance a failed attempt's retry row adds to its cause, decided here once for every surface
 * that shows one: the promise to the same-mode Retry while `isSafeRetry` holds, the pointer at a
 * new key for an attempt its session answered with an error (which gets no Retry), and nothing
 * otherwise. Each sentence names a control in that row, so a surface asks only while it shows it.
 */
export function failureGuidance(
  attempt: DeliveryOutcome & { readonly error?: string | null },
  surface: "card" | "mention",
  now: number = Date.now()
): string | undefined {
  if (isSafeRetry(attempt, now)) return safeRetryGuidance(surface, attempt.error);
  if (attempt.answeredWithError === true) return answeredWithErrorGuidance(surface);
  return undefined;
}

/**
 * What the broadcast page says under the Retry of a send nobody is carrying: a pending attempt
 * whose claim lapsed, made at `createdAt`, or a recipient no attempt was recorded for at all
 * (`createdAt` undefined), which has nothing to repeat. The Retry re-sends in `mode`, the
 * attempt's own, so the promise holds only while its repeat is still recognised.
 */
export function strandedRetryGuidance(
  mode: MessageDeliveryMode,
  createdAt: string | undefined,
  now: number = Date.now()
): string {
  const retry = `Nobody is carrying this send. Retry uses ${MODE_LABELS[mode]} again`;
  if (createdAt === undefined || insideDuplicateWindow(createdAt, now)) {
    return `${retry}, which cannot deliver it twice ${unlessRestarted}.`;
  }
  return `${retry}, and may deliver it twice: the send is older than the window in which a repeat is recognised.`;
}

/**
 * Join a recorded cause to the guidance that follows it. Only the receipt-timeout cause ends in
 * a sentence, so joining with a bare space would run every other cause into the guidance
 * ("Failed: no live session Retry won't deliver it twice.").
 */
export function withGuidance(cause: string, guidance: string): string {
  return /[.!?]$/.test(cause) ? `${cause} ${guidance}` : `${cause}. ${guidance}`;
}
