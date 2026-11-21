import { handledMessages } from "@legion/pi-shared/dispatch-user-turn";
import { encode } from "@toon-format/toon";
import { writtenCardIdentity } from "./missed-messages";

/**
 * What a resumed session is told about the turn its previous process ended in (R2).
 *
 * The transcript holds the cut turn, so no delivery is sent again: what is missing is the news
 * that the turn was cut. The daemon already gives a task's resumed worker that news
 * (`interruptedTask` in `packages/daemon/internal/supervise/delivery.go`), and this gives the same
 * news for the deliveries a turn was started or joined by. Both notes reach the same agents, so
 * the sentences here are the daemon's own, word for word, where they say the same thing
 * (`relaunchSentence` in `packages/daemon/internal/supervise/relaunch.go`). A turn a task started
 * and a delivery joined can get both notes; they say the same thing.
 *
 * How the transcript says a turn had no end. A session Legion drives marks each of its runs
 * (`TURN_MARK_ENTRY`, which `extensions/envoy.ts` writes): `ended: false` as the host starts the
 * run (`agent_start`), and `ended: true` as the host settles it with nothing to follow, which is
 * an `agent_end` without `willContinue` (Oh My Pi's `session/agent-session.ts`, `#settleAgentEnd`
 * and the routing before it): the model's stop, a person's interrupt, the daemon's abort, a spent
 * retry budget, an error the host does not retry. A run the host continues (a retry, a reminder,
 * a background job's result) is settled by the run that finishes it. The host writes each entry
 * into the session file as it records it and in the order it records them
 * (`#appendToSessionFile` in Oh My Pi's `session/session-manager.ts`), so a mark on disk follows
 * everything recorded before it.
 *
 * In a transcript that holds a mark, the last turn ended only where the newest mark is an end
 * mark and no reply, user message or delivery card follows it. Every other last turn is cut,
 * whatever its last reply carries. That is what tells a provider error the host is retrying from
 * one that ended the run, which the host's own entries cannot: it writes a failed attempt as it
 * writes any provider error, and marks it retried only once the retry closes
 * (`#markPendingRetryErrors` in Oh My Pi's `session/turn-recovery.ts`). A process killed in a
 * retry's backoff, in a credential wait before the retry is announced (a saved usage reset it
 * redeems, another account's short block), or between saving the attempt and anything after it,
 * left no end mark.
 *
 * The reading errs toward an extra note and never toward a missing one. A run the host drops
 * without telling extensions of its end (a session switch, a continuation it scheduled and never
 * ran), and an end mark a kill took before it reached the file, read as cut. That is the safe
 * side: the note only says the turn was cut and asks the agent to read what that turn did before
 * it acts again, which costs a read of its own transcript, while a missing note leaves it to start
 * again work the turn already did, or to wait for answers that will never come.
 *
 * A transcript that holds no mark at all was written before the plugin marked runs, or by a
 * process that never started a run, and is read as it was before marks. The branch's newest
 * assistant message decides, with what follows it:
 *
 *  - It ended its turn, and nothing that could start a turn follows it: the last turn ended, and
 *    nothing is told. A message ends its turn when it ends a model turn (`stopReason` other than
 *    `toolUse`, the rule `../src/card-wake` reads the same transcripts by) or carries an
 *    `errorMessage` (a person's interrupt, an abort the daemon sent, a provider error), and the
 *    resumed session reads that message itself.
 *  - It ended its turn, and a user message or a delivery card follows it: that started the next
 *    turn, and the process ended before the turn wrote a reply. That turn is told.
 *  - It called a tool (`toolUse`), or it carries no `stopReason` at all (a reply the process was
 *    killed in the middle of): the turn has no end, and it is told.
 *  - It is the message Oh My Pi writes at resume to close a turn whose process recorded its exit
 *    in the middle of it, as a SIGTERM's does (`createInterruptedTurnAbortMessage` in Oh My Pi's
 *    `session/exit-diagnostics.ts`: `aborted`, with `HOST_CLOSED_TURN_ERROR`). The host is saying
 *    the previous process ended in that turn, so it is told. Once a user message, a delivery or a
 *    later reply follows it, it is the end of the turn it closed, as any abort is.
 *  - There is no assistant message at all: a turn is told only where something could have started
 *    one, which is a user message or a delivery card. A transcript holding neither — a fresh
 *    session, or one that holds the plugin's own notices alone — is told nothing.
 *
 * Where the cut turn starts, so a delivery of an earlier turn is never named: after the newest
 * end mark; in a marked transcript no run of which has ended, after the newest reply written
 * before its first mark; and in an unmarked one, after the newest assistant message that ended a
 * turn before it.
 *
 * What it names. Every delivery card the host wrote into the cut turn (`writtenCardIdentity`: an
 * `envoy-message` the ledger knows a frame for, which the plugin's own notices are not), and every
 * person's Send that Envoy accepted as the person's own turn, which is no card: the host writes it
 * as the person's user message, or as the skill prompt a `/skill:` Send submits, carrying the
 * Dispatch message id as its `tag` (`pi.sendUserInput`), and the session recorded handling an
 * attempt of that message (`HANDLED_ATTEMPT_ENTRY`) before it went out. Such a Send is named by
 * that id, from `dispatch`, as its card would say. A Send's attempt the accept did not make a turn
 * is its card, named as a card. On a host without `pi.sendUserInput` the Send is a user message
 * with no tag, which nothing in the transcript links to its attempt, so it is not named there.
 */

/** One delivery written into the cut turn, as the note names it. */
export interface CutTurnDelivery {
  /** The frame's event id, which `envoy_inbox` and the listener name it by; for a person's Send
   *  that became the session's own turn, its Dispatch message id. */
  readonly id?: string;
  /** Who sent it, as the card's own `from:` line reads. */
  readonly from?: string;
  /** The card's summary line, where it carries one. */
  readonly summary?: string;
}

/** A last turn with no end, and the deliveries written into it, in transcript order. */
export interface CutTurn {
  readonly deliveries: readonly CutTurnDelivery[];
}

/** The `notice:` of the card this module writes; no delivery of the listener carries it. */
export const CUT_TURN_NOTICE = "previous turn interrupted";

/** The custom transcript entry (`pi.appendEntry`) a session Legion drives marks its runs with: at
 *  the host's `agent_start`, and at an `agent_end` without `willContinue`. */
export const TURN_MARK_ENTRY = "envoy-turn-mark";

/** What a `TURN_MARK_ENTRY` mark holds. */
export interface TurnMark {
  /** False as a run starts, true as the host settles a run with nothing to follow. */
  readonly ended: boolean;
}

/** The `errorMessage` of the assistant message Oh My Pi closes a turn with at resume because its
 *  previous process exited in it (`INTERRUPTED_TURN_ERROR`, Oh My Pi's
 *  `session/exit-diagnostics.ts`). */
const HOST_CLOSED_TURN_ERROR = "Previous OMP process exited before completing the turn.";

/** A non-empty string field of `value`, if it has one. */
function stringField(value: object, field: string): string | undefined {
  const found: unknown = field in value ? Reflect.get(value, field) : undefined;
  return typeof found === "string" && found !== "" ? found : undefined;
}

/** The entry's `message`, for an entry that holds one (`{type: "message", message}`). */
function hostMessage(entry: unknown): object | undefined {
  if (typeof entry !== "object" || entry === null) return undefined;
  if (stringField(entry, "type") !== "message") return undefined;
  const message: unknown = "message" in entry ? entry.message : undefined;
  return typeof message === "object" && message !== null ? message : undefined;
}

/** What a `TURN_MARK_ENTRY` mark says, or undefined for every other entry. */
function turnMarkEnded(entry: unknown): boolean | undefined {
  if (typeof entry !== "object" || entry === null) return undefined;
  if (stringField(entry, "type") !== "custom") return undefined;
  if (stringField(entry, "customType") !== TURN_MARK_ENTRY) return undefined;
  const data: unknown = "data" in entry ? entry.data : undefined;
  if (typeof data !== "object" || data === null || !("ended" in data)) return undefined;
  return typeof data.ended === "boolean" ? data.ended : undefined;
}

/** Whether `message`, an assistant message of an unmarked transcript, ends the turn it is in: the
 *  model stopped there, or a person, the daemon or a provider error ended the run
 *  (`errorMessage`). A turn goes on with a tool's result (`toolUse`), and a message with no
 *  `stopReason` at all is a reply the process was killed in the middle of. */
function endsTurn(message: object): boolean {
  if (stringField(message, "errorMessage") !== undefined) return true;
  const stopReason = stringField(message, "stopReason");
  return stopReason !== undefined && stopReason !== "toolUse";
}

/** The card's text, or its text blocks joined where it carries pictures. */
function cardText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const texts: string[] = [];
  for (const block of content) {
    if (typeof block !== "object" || block === null) continue;
    if (stringField(block, "type") !== "text") continue;
    const text = stringField(block, "text");
    if (text !== undefined) texts.push(text);
  }
  return texts.join("\n");
}

/** The top-level scalar lines of the `envoy:` block a card is rendered as (`renderInbound`), by
 *  their key. A TOON value is bare, or quoted as JSON; a line that holds neither is passed over. */
function cardFields(text: string): Map<string, string> {
  const fields = new Map<string, string>();
  const lines = text.split("\n");
  if (lines[0] !== "envoy:") return fields;
  for (const line of lines.slice(1)) {
    const match = /^ {2}([a-z_]+): (.+)$/.exec(line);
    const key = match?.[1];
    const raw = match?.[2];
    if (key === undefined || raw === undefined) continue;
    let value: unknown = raw;
    if (raw.startsWith('"')) {
      try {
        value = JSON.parse(raw);
      } catch {
        continue;
      }
    }
    if (typeof value === "string" && value !== "") fields.set(key, value);
  }
  return fields;
}

/** The delivery an entry of the cut turn names, or nothing for every other entry: a card of a
 *  frame the ledger knows (`writtenCardIdentity`), which the plugin's own notices are not. */
function deliveryOf(entry: unknown): CutTurnDelivery | undefined {
  if (typeof entry !== "object" || entry === null) return undefined;
  if (stringField(entry, "type") !== "custom_message") return undefined;
  const identity = writtenCardIdentity(entry);
  if (identity === undefined) return undefined;
  const fields = cardFields(cardText("content" in entry ? entry.content : undefined));
  const id = identity.event_id ?? fields.get("id") ?? identity.dedupe_key;
  const from = fields.get("from");
  const summary = fields.get("summary");
  return {
    ...(id === undefined ? {} : { id }),
    ...(from === undefined ? {} : { from }),
    ...(summary === undefined ? {} : { summary }),
  };
}

/** The person's Send an entry of the cut turn is, or nothing for every other entry: a user
 *  message, or a skill prompt (`custom_message`), whose `tag` is the id of a Dispatch message the
 *  session recorded handling (`accepted`). */
function personsSendOf(entry: unknown, accepted: ReadonlySet<string>): CutTurnDelivery | undefined {
  if (typeof entry !== "object" || entry === null) return undefined;
  const message = hostMessage(entry);
  let tag: string | undefined;
  if (message !== undefined) {
    if (stringField(message, "role") === "user") tag = stringField(message, "tag");
  } else if (stringField(entry, "type") === "custom_message") {
    tag = stringField(entry, "tag");
  }
  return tag !== undefined && accepted.has(tag) ? { id: tag, from: "dispatch" } : undefined;
}

/** Whether the entry could have started a turn: a user message, or a delivery card. */
function couldStartTurn(entry: unknown): boolean {
  const message = hostMessage(entry);
  if (message !== undefined) return stringField(message, "role") === "user";
  return deliveryOf(entry) !== undefined;
}

/** Whether the entry is an assistant message the host wrote. */
function isReply(entry: unknown): boolean {
  const message = hostMessage(entry);
  return message !== undefined && stringField(message, "role") === "assistant";
}

/**
 * Where the cut turn of a marked transcript starts, or undefined where its last run ended:
 * `firstMark` is the index of its first mark, `newestEnd` that of its newest end mark (-1 for
 * none), and `endedLast` whether its newest mark is an end mark.
 */
function markedCutTurnStart(
  branch: readonly unknown[],
  firstMark: number,
  newestEnd: number,
  endedLast: boolean
): number | undefined {
  const afterEnd = branch.slice(newestEnd + 1);
  if (endedLast && !afterEnd.some((entry) => isReply(entry) || couldStartTurn(entry))) {
    return undefined;
  }
  if (newestEnd >= 0) return newestEnd + 1;
  // No marked run has ended, so the cut one is the first marked run, which follows whatever the
  // process before marks last replied.
  for (let index = firstMark - 1; index >= 0; index -= 1) {
    if (isReply(branch[index])) return index + 1;
  }
  return 0;
}

/** Where the cut turn of an unmarked transcript starts, or undefined where its last turn ended
 *  and where it holds no turn at all. */
function unmarkedCutTurnStart(branch: readonly unknown[]): number | undefined {
  let newestSeen = false;
  // A user message or a delivery after the newest assistant message: a turn it started.
  let started = false;
  for (let index = branch.length - 1; index >= 0; index -= 1) {
    const entry = branch[index];
    const message = hostMessage(entry);
    if (message === undefined || stringField(message, "role") !== "assistant") {
      if (!newestSeen && couldStartTurn(entry)) started = true;
      continue;
    }
    // The host's close of a cut turn, newest and with nothing after it, belongs to that turn; a
    // turn after it makes it the end of the turn it closed, as any abort is.
    const hostClosedCutTurn =
      !newestSeen &&
      !started &&
      stringField(message, "stopReason") === "aborted" &&
      stringField(message, "errorMessage") === HOST_CLOSED_TURN_ERROR;
    const ends = !hostClosedCutTurn && endsTurn(message);
    if (!newestSeen) {
      newestSeen = true;
      if (ends && !started) return undefined;
    }
    // The turn before the cut one ended here, so the cut turn starts after it.
    if (ends) return index + 1;
  }
  return newestSeen || started ? 0 : undefined;
}

/**
 * The last turn of `branch` where that turn has no end, with the deliveries written into it;
 * undefined where the branch's last turn ended, and for a branch that holds no turn at all.
 */
export function cutTurn(branch: readonly unknown[]): CutTurn | undefined {
  let firstMark = -1;
  let newestEnd = -1;
  let endedLast = false;
  for (const [index, entry] of branch.entries()) {
    const ended = turnMarkEnded(entry);
    if (ended === undefined) continue;
    if (firstMark === -1) firstMark = index;
    if (ended) newestEnd = index;
    endedLast = ended;
  }
  const startsAt =
    firstMark === -1
      ? unmarkedCutTurnStart(branch)
      : markedCutTurnStart(branch, firstMark, newestEnd, endedLast);
  if (startsAt === undefined) return undefined;
  const accepted = handledMessages(branch);
  const deliveries: CutTurnDelivery[] = [];
  for (const entry of branch.slice(startsAt)) {
    const delivery = deliveryOf(entry) ?? personsSendOf(entry, accepted);
    if (delivery !== undefined) deliveries.push(delivery);
  }
  return { deliveries };
}

/**
 * The card a resumed session is shown for `cut`. The sentences are the daemon's own
 * (`relaunchSentence`), since both notes are read by the same agents, and the deliveries are
 * named above them, so "those messages" names what the reader has just read.
 */
export function cutTurnNoticeText(cut: CutTurn): string {
  const said = [
    "Your previous turn was interrupted.",
    "Your previous process ended in that turn.",
    "Your tool calls in flight were never answered.",
    "The subagents and background jobs of that turn are gone.",
    "No result of that work will come to you.",
    "Your transcript does not say so.",
    ...(cut.deliveries.length === 0
      ? []
      : ["Those messages started or joined that turn.", "No message is sent to you again."]),
    "Before you start any of that work again, check what it already did.",
  ];
  return encode({
    envoy: {
      notice: CUT_TURN_NOTICE,
      ...(cut.deliveries.length === 0 ? {} : { deliveries: cut.deliveries }),
      detail: said.join(" "),
    },
  });
}
