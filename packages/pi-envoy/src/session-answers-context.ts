import {
  isSessionAnswersText,
  SESSION_ANSWERS_MARKER,
  type SessionAnswersRead,
  sessionAnswersChanged,
} from "@legion/envoy-client/session-answers";
import { leadingText, userTextMessage } from "./context-message";

/** The first line of Oh My Pi's side-turn notice (`side-channel-no-tools.md`): the `developer`
 *  message `AgentSession.runEphemeralTurn` puts after the conversation, before the side prompt. */
const SIDE_TURN_NOTICE = "Ephemeral side-channel turn; reuses current conversation context.";

/** How Oh My Pi's handoff prompt (`handoff-document.md`) begins. */
const HANDOFF_PROMPT = "<critical>\nWrite a handoff document for another instance of yourself.";

/** The requests a copy of this extension already decided the list for: the messages array the
 *  host hands each `context` handler of one request in turn, which a handler that returns nothing
 *  passes on as it is. It lives on `globalThis` under a registered symbol, as the read state of
 *  `@legion/envoy-client/session-answers` does, because Oh My Pi loads each copy of this extension
 *  as a module of its own. A change to its shape takes a new key (`.v2`). */
const DECIDED_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-requests.v1");
const processGlobal = globalThis as typeof globalThis & { [DECIDED_KEY]?: WeakSet<object> };
const decided = processGlobal[DECIDED_KEY] ?? new WeakSet<object>();
processGlobal[DECIDED_KEY] = decided;

/** The text of `message` when the host wrote it (`attribution: "agent"`) with `role`. */
function hostText(message: unknown, role: string): string | undefined {
  return typeof message === "object" &&
    message !== null &&
    "attribution" in message &&
    message.attribution === "agent"
    ? leadingText(message, role)
    : undefined;
}

/**
 * Whether the request's last message is the list of people's answers as an older copy of this
 * extension appends it, after the last message on every request: a user message whose first text
 * block starts with the list's marker or the failure line's. Only that one position is checked,
 * as `withDispatchFirst` checks only its own: a tool result, delivered message or reply quoting
 * the marker earlier in the conversation is conversation, which must not switch the list off. A
 * copy of this version records the array it returns in `decided` instead, wherever its list sits.
 */
function endsWithSessionAnswers(messages: readonly unknown[]): boolean {
  const text = leadingText(messages.at(-1), "user");
  return text !== undefined && isSessionAnswersText(text);
}

/** A context message as matching reads it: its role and timestamp, and a tool result's call id,
 *  since the tool results of one round can share a timestamp. */
function messageKey(message: unknown): string {
  if (typeof message !== "object" || message === null) return "";
  const role = "role" in message ? String(message.role) : "";
  const timestamp = "timestamp" in message ? String(message.timestamp) : "";
  const call = "toolCallId" in message ? `#${String(message.toolCallId)}` : "";
  return `${role}@${timestamp}${call}`;
}

/**
 * Whether the request is a side turn: Oh My Pi's `runEphemeralTurn` (a Dispatch BTW, the stop-time
 * self-check, `/btw`, the idle recap) converts `[...conversation, notice, ...history, prompt]`,
 * where the notice is a host `developer` message, the history `/btw`'s earlier questions and
 * answers, and the prompt a host user message.
 */
function isSideTurn(messages: readonly unknown[]): boolean {
  if (hostText(messages.at(-1), "user") === undefined) return false;
  for (let index = messages.length - 2; index >= 0; index -= 1) {
    const message = messages[index];
    const role =
      typeof message === "object" && message !== null && "role" in message
        ? message.role
        : undefined;
    if (role !== "user" && role !== "assistant") {
      return hostText(message, "developer")?.includes(SIDE_TURN_NOTICE) === true;
    }
  }
  return false;
}

/**
 * The context message a branch entry becomes, as far as matching reads it, or undefined for an
 * entry that becomes none. The pinned host (18.12.2) builds a compaction's messages from the
 * branch with `MI`:
 *   if (e.type === "message") return e.message;
 *   if (e.type === "custom_message") return yu(e.customType, e.content, e.display, e.details, e.timestamp, e.attribution);
 *   if (e.type === "branch_summary") return hm(e.summary, e.fromId, e.timestamp);
 * where `yu` returns `{role: "custom", ..., timestamp: new Date(s).getTime()}` and `hm` returns
 * `{role: "branchSummary", ..., timestamp: new Date(o).getTime()}`. A `compaction` entry becomes
 * no message there: the earlier summary heads the conversion, so no cut ends on it.
 */
function entryMessage(entry: unknown):
  | {
      readonly role?: unknown;
      readonly timestamp?: unknown;
      readonly stopReason?: unknown;
      readonly content?: unknown;
      readonly toolCallId?: unknown;
    }
  | undefined {
  if (typeof entry !== "object" || entry === null || !("type" in entry)) return undefined;
  if (entry.type === "message") {
    return "message" in entry && typeof entry.message === "object" && entry.message !== null
      ? entry.message
      : undefined;
  }
  const role =
    entry.type === "custom_message"
      ? "custom"
      : entry.type === "branch_summary"
        ? "branchSummary"
        : undefined;
  if (role === undefined) return undefined;
  const timestamp =
    "timestamp" in entry && typeof entry.timestamp === "string"
      ? new Date(entry.timestamp).getTime()
      : undefined;
  return { role, timestamp };
}

/** Where a request ends on the session's branch: at a message with a later one after it, at the
 *  branch's last message (`tip`, the id of the branch's last entry that becomes a message), or at
 *  a message the branch does not hold. */
type BranchPosition =
  | { readonly at: "before" }
  | { readonly at: "last"; readonly tip: string }
  | { readonly at: "absent" };

/** The tool call ids of `message` when it is an assistant reply that failed or was aborted. */
function failedCalls(message: {
  readonly role?: unknown;
  readonly stopReason?: unknown;
  readonly content?: unknown;
}): string[] {
  if (
    message.role !== "assistant" ||
    (message.stopReason !== "error" && message.stopReason !== "aborted")
  ) {
    return [];
  }
  return (Array.isArray(message.content) ? message.content : []).flatMap((block: unknown) =>
    typeof block === "object" &&
    block !== null &&
    "type" in block &&
    block.type === "toolCall" &&
    "id" in block
      ? [String(block.id)]
      : []
  );
}

/**
 * Where the request's last message sits on the session's branch, matched by role and timestamp.
 * Oh My Pi's Anthropic remote compaction (`buildLiveProviderContext`) converts the conversation up
 * to its cut alone, counts the result, and keeps that many messages of the whole conversation's
 * conversion: a list on the first would make it keep one message past the cut. A main-loop request
 * ends at a message not yet written, or at the branch's last. A failed or aborted reply, and the
 * results the host wrote for its tool calls, which a retry drops from the context but not from the
 * branch (`turn-recovery`), do not count as later.
 */
function branchPosition(messages: readonly unknown[], entries: readonly unknown[]): BranchPosition {
  const last = messages.at(-1);
  if (typeof last !== "object" || last === null || !("timestamp" in last) || !("role" in last)) {
    return { at: "absent" };
  }
  const dropped = new Set(
    entries.flatMap((entry) => {
      const message = entryMessage(entry);
      return message === undefined ? [] : failedCalls(message);
    })
  );
  let later = false;
  let tip: string | undefined;
  for (let index = entries.length - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    const message = entryMessage(entry);
    if (message === undefined) continue;
    tip ??= typeof entry === "object" && entry !== null && "id" in entry ? String(entry.id) : "";
    if (message.role === last.role && message.timestamp === last.timestamp) {
      return later ? { at: "before" } : { at: "last", tip };
    }
    const failed =
      (message.role === "assistant" &&
        (message.stopReason === "error" || message.stopReason === "aborted")) ||
      (message.role === "toolResult" && dropped.has(String(message.toolCallId)));
    if (!failed) later = true;
  }
  return { at: "absent" };
}

/** A session's last decided conversion: a remote compaction's cut (`"cut"`), or a request with its
 *  key (undefined unless it ended at the branch's last message) and whether it came right after a
 *  cut. */
type Conversion = "cut" | { readonly key: string | undefined; readonly afterCut: boolean };

/** Each session's last decided conversion, by the session's `sessionManager`. It lives on
 *  `globalThis` as `decided` does, since only the copy that decides a request records it. */
const CONVERSIONS_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-conversions.v1");
const conversionsGlobal = globalThis as typeof globalThis & {
  [CONVERSIONS_KEY]?: WeakMap<object, Conversion>;
};
const conversions = conversionsGlobal[CONVERSIONS_KEY] ?? new WeakMap<object, Conversion>();
conversionsGlobal[CONVERSIONS_KEY] = conversions;

/** What one request's read hands on (`carriedAnswers`): its text, the list or the failure line,
 *  undefined when the read gave none; when the read gave them, what the people wrote in every item
 *  Dispatch sent inside the read's window (`SessionAnswersRead.words`) and where that window starts
 *  (`SessionAnswersRead.since`); when the read failed and gave the cached list instead, why it
 *  failed, from one of two sources: this copy's read gives `SessionAnswersRead.reason`, and a list
 *  an older copy rendered from its cache gives the sentence its read line names (`placedLegacy`),
 *  as that copy's renderer wrote it, ending with a full stop, `?` or `!`; and whether Dispatch
 *  answered the read (`source: "fresh"`). */
export interface CarriedAnswers {
  readonly text: string | undefined;
  readonly words?: readonly string[] | undefined;
  readonly since?: string | undefined;
  readonly reason?: string | undefined;
  readonly answered?: boolean | undefined;
}

/** A read that gave text: what a session's list is placed with. */
type ShownAnswers = CarriedAnswers & { readonly text: string };

/**
 * What `SessionAnswersRequest.read` hands on for `read`: its text, its `words` and `since`
 * whenever it gave them (a read that answered, or that gave the cached list), with its text or
 * without, since a read whose window holds no item still tells which items of a placed list left
 * it (`keptWithoutText`), the `reason` of a read that gave the cached list (`source: "cache"`), and
 * whether Dispatch answered the read (`source: "fresh"`). Undefined when it gave neither text nor
 * words: Dispatch is off, or has no answers route and no list read from it before has an item
 * inside the 72 hours (none was read, or every item of the one read has aged out). The
 * extension's `sessionAnswersText` and the placement cases of `session-answers-read.test.ts` both
 * call it, so those cases check the extension's own mapping.
 */
export function carriedAnswers(read: SessionAnswersRead): CarriedAnswers | undefined {
  if (read.text === undefined && read.words === undefined) return undefined;
  const reason = read.source === "cache" ? read.reason : undefined;
  const answered = read.source === "fresh";
  return { text: read.text, words: read.words, since: read.since, reason, answered };
}

/** Where a session's list sits: right after the message (`messageKey`) that was the last of the
 *  request that first showed what it shows, and the bytes it was placed with (the list, or the
 *  failure line), which every later request that keeps the place sends again. */
interface Placement {
  readonly after: string;
  readonly carried: ShownAnswers;
}

/** Each session's placement, by the session's `sessionManager` and then its session id (Oh My Pi
 *  keeps one `sessionManager` when it switches sessions), on `globalThis` as `conversions` is. A
 *  change to its shape takes a new key (`.v2`). */
const PLACEMENTS_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-placements.v1");
const placementsGlobal = globalThis as typeof globalThis & {
  [PLACEMENTS_KEY]?: WeakMap<object, Map<string, Placement>>;
};
const placements =
  placementsGlobal[PLACEMENTS_KEY] ?? new WeakMap<object, Map<string, Placement>>();
placementsGlobal[PLACEMENTS_KEY] = placements;

/** The sessions, by `sessionManager` and then session id as `placements` keys them, whose kept
 *  bytes already hid a failed read that `SessionAnswersRequest.unshownFailure` was told of, and
 *  that no read of theirs has answered since. A session leaves it when one of its reads answers,
 *  or an older copy's list with no read line arrives (`placedLegacy`), so the next failure hidden
 *  that way is told again. On `globalThis` as `placements` is. */
const UNSHOWN_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-unshown.v1");
const unshownGlobal = globalThis as typeof globalThis & {
  [UNSHOWN_KEY]?: WeakMap<object, Set<string>>;
};
const unshown = unshownGlobal[UNSHOWN_KEY] ?? new WeakMap<object, Set<string>>();
unshownGlobal[UNSHOWN_KEY] = unshown;

/** How the line naming the read that produced a list begins (`As read at <time>; this read failed:
 *  <reason>`), which `renderSessionAnswers` writes on the list's second line, under the marker,
 *  when it answers from the cache, and only then. */
const READ_LINE_PREFIX = "As read at ";

/** `text` without the line naming the read that produced it (`READ_LINE_PREFIX`). Two texts whose
 *  rest is the same show the same items. */
function withoutReadLine(text: string): string {
  const lines = text.split("\n");
  return lines[1]?.startsWith(READ_LINE_PREFIX) === true
    ? [lines[0], ...lines.slice(2)].join("\n")
    : text;
}

/** Why the read that produced `text` failed, as the line naming that read (`READ_LINE_PREFIX`)
 *  says it: the sentence after `this read failed: `, as the renderer wrote it. Undefined when
 *  `text` has no such line: a list rendered from a read Dispatch answered, or the failure line. */
function failedRead(text: string): string | undefined {
  const line = text.split("\n")[1];
  if (line?.startsWith(READ_LINE_PREFIX) !== true) return undefined;
  const failed = "; this read failed: ";
  const at = line.indexOf(failed);
  return at < 0 ? undefined : line.slice(at + failed.length);
}

/** Whether `carried` is a list of what people wrote, rather than the failure line, which says the
 *  list could not be read and shows nothing a person wrote. */
function isList(carried: ShownAnswers): boolean {
  return carried.text.startsWith(SESSION_ANSWERS_MARKER);
}

/**
 * Whether `next` shows something `shown` did not: an item, an answer or comment a person changed,
 * an item a person retracted, or that the list could not be read. The failure line, which a read
 * gives when it has no list to give (a Dispatch config error, a changed Dispatch URL, a cache
 * whose every item aged out, a read that threw), is a change where a list is placed, so it goes
 * after the last message and the model reads that this read failed rather than kept bytes that
 * look like a read that answered. A read that fails but gives the cached list is a list: with the
 * same items it keeps the placed bytes, and the request does not say that this read failed. A
 * list where the failure line is placed is a change too. Two failure lines show the same thing
 * and differ only in their reason, so a failure line after a placed one keeps that one's place
 * and bytes, as a list does with its read note.
 *
 * Between two lists, the two reads' words (`sessionAnswersWords`, one string per item Dispatch
 * sent inside the read's window, shown or left out by the list's character cap) answer it
 * (`sessionAnswersChanged`): an item new to `next`, or one whose words changed, is a change, so a
 * new item or an edit the cap leaves out still moves the list, and the model reads the new text
 * with its left-out line rather than the person's old words as current. An item `next` no longer
 * holds is a change while its time is inside `next`'s window, since a person retracted it or
 * Dispatch removed it, and no change once the window has left it behind.
 *
 * A list an older copy of this extension placed has no words, and then only its rendered text is
 * left to compare. The comparison there is deliberately crude: the texts are the same only when
 * they differ in nothing but the line naming the read, and every other difference counts as
 * something new, so the list is re-rendered after the last message. Reading that text more
 * closely — by paragraph, or cut into items, each without the line counting what the character
 * cap left out — was tried twice and defeated twice by an item whose own words read like the
 * renderer's: a person's answer quoting another item's first lines, or ending in a sentence of
 * the cap line's shape, was taken for something already shown, and what they wrote reached no
 * request at all. A list re-rendered because only this read's note changed costs a prompt-cache
 * miss, on a path that lasts only until every copy of this extension carries this change; a
 * swallowed answer costs a person's words. So this path pays the miss. `keptWithoutText` holds
 * the same path's rule for a read with no text, which takes such a list out whenever it gives
 * words; the older-copy path is in both places, and goes from both.
 */
function showsNew(shown: ShownAnswers, next: ShownAnswers): boolean {
  if (isList(shown) !== isList(next)) return true;
  if (!isList(next)) return false;
  if (shown.words === undefined || next.words === undefined) {
    return withoutReadLine(shown.text) !== withoutReadLine(next.text);
  }
  return sessionAnswersChanged(shown.words, next.words, next.since);
}

/** `messages` with `placed` put back where it sits, with the bytes it was placed with, or
 *  undefined when its place left the conversation. */
function keepPlaced(messages: readonly unknown[], placed: Placement): unknown[] | undefined {
  const index = messages.findLastIndex((message) => messageKey(message) === placed.after);
  if (index < 0) return undefined;
  return [
    ...messages.slice(0, index + 1),
    userTextMessage(placed.carried.text),
    ...messages.slice(index + 1),
  ];
}

/**
 * `messages` for a request whose read gave no text (`carried`): its window holds no item, or
 * Dispatch has no answers route and nothing is cached, or Dispatch is off. The session's placed
 * list stays where it sits: taking it out would change the request from that place on exactly as
 * changing its bytes would, and the next read that carries the cached list would put it back. It
 * leaves, with its place, when the read's words show an item of it gone while its time is still
 * inside the read's window (`sessionAnswersChanged`), as when a person retracts the only answer
 * the window held: kept, the list would go on showing withdrawn words as current. A list whose
 * items only aged out of the window stays. A list an older copy of this extension placed has no
 * words that tell its aged-out items from retracted ones, so a read with no text that gives words
 * takes it out, as `showsNew`, which holds the same path's rule for a read with text, re-renders
 * such a list on any difference but its read line; the older-copy path is in both places, and
 * goes from both. A read with no words (no answers route, Dispatch off) compares nothing and
 * keeps the list. A placed failure line is not kept: a read that gave no text did not fail, so the
 * line no longer says what is true, and it leaves with its place, so the next failure line goes
 * after the last message. Undefined, so the request carries no list, when nothing is placed, when a
 * failure line was, when the list leaves, when its place left the conversation, and while a
 * compaction may be converting the conversation (`compacting`), where a list before the cut would
 * push a message out.
 */
function keptWithoutText(
  messages: readonly unknown[],
  carried: CarriedAnswers | undefined,
  request: SessionAnswersRequest,
  compacting: boolean
): unknown[] | undefined {
  const sessions = placements.get(request.session);
  const placed = sessions?.get(request.id);
  if (sessions === undefined || placed === undefined) return undefined;
  const shown = placed.carried.words;
  const next = carried?.words;
  const withdrawn =
    next !== undefined &&
    (shown === undefined || sessionAnswersChanged(shown, next, carried?.since));
  if (!isList(placed.carried) || withdrawn) {
    sessions.delete(request.id);
    return undefined;
  }
  return compacting ? undefined : keepPlaced(messages, placed);
}

/**
 * `messages` with the list (or the failure line) inserted where it belongs. One that shows
 * something the session's placed list did not (`showsNew`) goes after the last message, where the
 * model reads it as what arrived last, and stays right after that message on every later request
 * while what is read shows nothing new: the model reads it as part of the conversation, and its
 * bytes sit at the same place in each request, so the cached prefix of each request runs through
 * it. A list that keeps its place keeps the bytes it was placed with, not the text this request's
 * read gave: the text holds what the read itself did (`As read at …; this read failed: …`), which
 * changes from one request to the next on a loaded machine, and a changed byte inside the
 * conversation costs the whole request behind it a prompt-cache miss. So a kept list can name a
 * read older than this request's, and a failed read that gives the cached list says nothing of
 * its failure there, unless the kept bytes are that read's own text, which say it: otherwise the
 * request tells its `unshownFailure` why that read failed, which the extension logs, on the first
 * such request and then not until a read of the session has answered (`unshown`). A kept list can
 * also hold an item that has since aged out of the window. The next item a person writes, edits or
 * retracts, or a read that gives the failure line, takes it out of that place and puts the new
 * text after the last message; a retracted item that was the last one inside the window takes it
 * out of the request (`keptWithoutText`). A session with no list placed yet, or whose list's
 * message left the conversation (a compaction summarized it), gets it after the last message
 * again. While a compaction may be converting the conversation (`compacting`) the list goes after
 * the last message and places nothing: a remote compaction keeps only as many messages of its
 * whole conversion as its cut's conversion holds, so a list before the cut would push one out.
 */
function placeList(
  messages: readonly unknown[],
  carried: ShownAnswers,
  request: SessionAnswersRequest,
  compacting: boolean
): unknown[] {
  if (compacting) return [...messages, userTextMessage(carried.text)];
  const sessions = placements.get(request.session) ?? new Map<string, Placement>();
  placements.set(request.session, sessions);
  const placed = sessions.get(request.id);
  if (placed !== undefined && !showsNew(placed.carried, carried)) {
    const kept = keepPlaced(messages, placed);
    if (kept !== undefined) {
      // Placed bytes that are this read's own text name its failure, and the model reads that.
      if (carried.reason !== undefined && carried.text !== placed.carried.text) {
        const told = unshown.get(request.session) ?? new Set<string>();
        unshown.set(request.session, told);
        if (!told.has(request.id)) {
          told.add(request.id);
          request.unshownFailure(carried.reason);
        }
      }
      return kept;
    }
  }
  sessions.set(request.id, { after: messageKey(messages.at(-1)), carried });
  return [...messages, userTextMessage(carried.text)];
}

export interface SessionAnswersRequest {
  /** The session's branch entries (`sessionManager.getBranch`). */
  readonly branch: () => readonly unknown[];
  /** The session the request is for (its `sessionManager`): the key of its last conversion. */
  readonly session: object;
  /** The session's id (`sessionManager.getSessionId`): with `session`, the key of its list's
   *  place. */
  readonly id: string;
  /** Whether Oh My Pi is compacting the session now: the live `AgentSession`'s `isCompacting`, or
   *  its `compactionSpeculation` reading `"running"`. */
  readonly compacting: () => boolean;
  /** This request's read (`carriedAnswers`): the list or the failure line, a read with no text
   *  that still gives its words, or undefined when it gave neither. A read with no text keeps a
   *  list already placed unless its words show an item of that list gone inside the window, and
   *  takes a placed failure line out (`keptWithoutText`). */
  readonly read: () => Promise<CarriedAnswers | undefined>;
  /** Told why this request's read failed (`CarriedAnswers.reason`) when that read gave the cached
   *  list, or an older copy's list names a failed read (`placedLegacy`), and the request keeps the
   *  placed bytes, which do not name this read's failure; not told when those bytes are this
   *  read's own text, which names it. Told on the first such request of the session, and not again
   *  until one of its reads has answered (`CarriedAnswers.answered`, or an older copy's list with
   *  no read line), so a lasting failure, such as a Dispatch with no answers route, is told once.
   *  Nothing else that a read gives is told here: a newer list, a retraction and the failure line
   *  each reach the model. The extension logs it. */
  readonly unshownFailure: (reason: string) => void;
}

/**
 * The messages one main-loop provider request carries, with what people wrote to the session as a
 * separate user message where `placeList` puts it: `request.read`'s text after the last message
 * when it shows something new, else the bytes the session's list was placed with, where it already
 * sat; a read that gives no text keeps a placed list there too, unless its words show an item of
 * that list gone inside the window (`keptWithoutText`). Undefined when the request carries no
 * list: the read gave no text and no list is kept, or one of these, for which `read` is not
 * called:
 *
 * - another copy of this extension already decided this request: it placed the list (and passed
 *   on the array it returned), or it read and found no text, or the request is one of those below;
 * - a side turn, whose last message must stay its prompt and which must not wait on a read;
 * - the handoff prompt, whose last message must stay that prompt;
 * - a conversion of the conversation up to a remote compaction's cut: it ends at a message the
 *   branch holds a later one after (`branchPosition`);
 * - a remote compaction's whole conversion when it keeps no tail: then it converts the same
 *   messages as its cut, both ending at the branch's last message, so the second of the two is a
 *   repeat of the conversion before it, over the same branch, while the session compacts. With no
 *   list on that one, the compaction keeps no list whatever the first carried:
 *     const ve = (await this.convertMessagesToLlm(Se, ce, pe)).length;
 *     const Re = await this.agent.buildSideRequestContext(await this.convertMessagesToLlm([...Se, ...le], ce, pe));
 *     for (let Ue = 0;Le < Re.messages.length && Ue < ve; Le++) ...
 *     return { ...Re, messages: Re.messages.slice(0, Le) };
 *   A repeat right after a cut is the main loop's next request beside a speculative compaction
 *   with a tail, whose whole conversion it matches, and keeps its list.
 *
 * It is never at the head, so a change in the list costs only the bytes after it: Oh My Pi marks a
 * message a `context` handler adds as per-call, and its Anthropic placement puts no cache anchor on
 * a per-call message, so every byte before the list is the cached prefix of the request before it,
 * and a list that keeps its place and its bytes is inside that prefix too. It is never merged into
 * a message of the conversation, whose bytes and cache anchors are the conversation's.
 */
export async function withSessionAnswers(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): Promise<unknown[] | undefined> {
  if (decided.has(messages)) return undefined;
  decided.add(messages);
  if (endsWithSessionAnswers(messages)) return placedLegacy(messages, request);
  if (
    isSideTurn(messages) ||
    hostText(messages.at(-1), "user")?.startsWith(HANDOFF_PROMPT) === true
  ) {
    return undefined;
  }
  const position = branchPosition(messages, request.branch());
  const previous = conversions.get(request.session);
  if (position.at === "before") {
    conversions.set(request.session, "cut");
    return undefined;
  }
  // What the request converted, for telling a repeat of it apart: the branch's last message and
  // each message's role and timestamp.
  const key =
    position.at === "last" ? [position.tip, ...messages.map(messageKey)].join("\n") : undefined;
  conversions.set(request.session, { key, afterCut: previous === "cut" });
  if (
    key !== undefined &&
    typeof previous === "object" &&
    previous.key === key &&
    !previous.afterCut &&
    request.compacting()
  ) {
    return undefined;
  }
  const carried = await request.read();
  if (carried?.answered === true) unshown.get(request.session)?.delete(request.id);
  const compacting = previous === "cut" || request.compacting();
  const text = carried?.text;
  const placed =
    carried === undefined || text === undefined
      ? keptWithoutText(messages, carried, request, compacting)
      : placeList(messages, { ...carried, text }, request, compacting);
  if (placed !== undefined) decided.add(placed);
  return placed;
}

/**
 * The request an older copy of this extension, which puts the list after the last message of
 * every request, already gave its list: that list moved where `placeList` puts it, with no read of
 * this copy's own. The older copy decided which requests carry one. A list it rendered from its
 * cache names the failed read on its second line (`As read at …; this read failed: …`); that
 * failure goes to `placeList` as the read's `reason` (`failedRead`), so where the placed bytes stay
 * and the line leaves the request, `unshownFailure` is told it under the same rule as this copy's
 * own reads. Every renderer writes a list without that line only for a read Dispatch answered, so
 * such a list takes the session out of `unshown`, as this copy's own answered read does, and the
 * next failure an older copy's list names is told again.
 */
function placedLegacy(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): unknown[] | undefined {
  const last = messages.at(-1);
  const text = leadingText(last, "user");
  // The list a copy adds is stamped 0 (`userTextMessage`); a person's own message quoting the
  // marker is theirs, never moved.
  const added =
    typeof last === "object" && last !== null && "timestamp" in last && last.timestamp === 0;
  if (text === undefined || !added) return undefined;
  const recorded = conversions.get(request.session);
  const compacting = (typeof recorded === "object" && recorded.afterCut) || request.compacting();
  const reason = failedRead(text);
  if (reason === undefined && text.startsWith(SESSION_ANSWERS_MARKER)) {
    unshown.get(request.session)?.delete(request.id);
  }
  const placed = placeList(messages.slice(0, -1), { text, reason }, request, compacting);
  decided.add(placed);
  return placed;
}
