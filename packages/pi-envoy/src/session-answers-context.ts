import {
  isSessionAnswersText,
  SESSION_ANSWERS_MARKER,
  type SessionAnswersRead,
  sessionAnswersChanged,
} from "@legion/envoy-client/session-answers";
import {
  type Conversion,
  isHandoffPrompt,
  isSideTurn,
  leadingText,
  messageKey,
  nextConversion,
  splitTrailingApproval,
} from "@legion/pi-shared/approval-message";
import { customEntries } from "@legion/pi-shared/custom-entry";
import { userTextMessage } from "./context-message";

/** The requests a copy of this extension already decided the list for: the messages array the
 *  host hands each `context` handler of one request in turn, which a handler that returns nothing
 *  passes on as it is. It lives on `globalThis` under a registered symbol, as the read state of
 *  `@legion/envoy-client/session-answers` does, because Oh My Pi loads each copy of this extension
 *  as a module of its own. A change to its shape takes a new key (`.v2`). */
const DECIDED_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-requests.v1");
const processGlobal = globalThis as typeof globalThis & { [DECIDED_KEY]?: WeakSet<object> };
const decided = processGlobal[DECIDED_KEY] ?? new WeakSet<object>();
processGlobal[DECIDED_KEY] = decided;

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

/** Each session's last decided conversion (`nextConversion`), by the session's `sessionManager`.
 *  It lives on `globalThis` as `decided` does, since only the copy that decides a request records
 *  it. */
const CONVERSIONS_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-conversions.v1");
const conversionsGlobal = globalThis as typeof globalThis & {
  [CONVERSIONS_KEY]?: WeakMap<object, Conversion>;
};
const conversions = conversionsGlobal[CONVERSIONS_KEY] ?? new WeakMap<object, Conversion>();
conversionsGlobal[CONVERSIONS_KEY] = conversions;

/** The decision of the newest request of each session to start a read, by the session's
 *  `sessionManager`: a promise that settles, and never rejects, once that request has decided or
 *  failed, which is never before every request of the session that started a read before it has.
 *  Each such request waits on the one before it (`decidedInTurn`), so the requests of one session
 *  decide one after another in the order they started, whatever order their reads settle in. On
 *  `globalThis` as `decided` is, since any copy of this extension may decide a request of the
 *  session. A change to its shape takes a new key (`.v2`). */
const DECIDING_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-deciding.v1");
const decidingGlobal = globalThis as typeof globalThis & {
  [DECIDING_KEY]?: WeakMap<object, Promise<void>>;
};
const deciding = decidingGlobal[DECIDING_KEY] ?? new WeakMap<object, Promise<void>>();
decidingGlobal[DECIDING_KEY] = deciding;

/** What one request's read hands on (`carriedAnswers`): its text, the list or the failure line,
 *  undefined when the read gave none; when the read gave them, what the people wrote in every item
 *  Dispatch sent inside the read's window (`SessionAnswersRead.words`) and where that window starts
 *  (`SessionAnswersRead.since`); when the read failed, why, from one of three sources: this copy's
 *  read gives `SessionAnswersRead.reason` with the cached list (`source: "cache"`) and with the
 *  failure line (`source: "failure"`), a read that threw gives its error's message with the
 *  failure line (the extension's `sessionAnswersText`), and a list an older copy rendered from its
 *  cache gives the sentence its read line names (`placedLegacy`), as that copy's renderer wrote it,
 *  ending with a full stop, `?` or `!`; and whether Dispatch answered the read (`source: "fresh"`,
 *  or an older copy's list with no read line), which ends the session's failure episode
 *  (`episodes`). */
export interface CarriedAnswers {
  readonly text: string | undefined;
  readonly words?: readonly string[] | undefined;
  readonly since?: string | undefined;
  readonly reason?: string | undefined;
  readonly answered?: boolean | undefined;
}

/** A read that gave text: what `showsNew` compares the newest answers message sent with. */
type ShownAnswers = CarriedAnswers & { readonly text: string };

/**
 * What `SessionAnswersRequest.read` hands on for `read`: its text, its `words` and `since`
 * whenever it gave them (a read that answered, or that gave the cached list), with its text or
 * without, since a read whose window holds no item still tells which items of the newest list
 * sent left it (`SESSION_ANSWERS_NO_ITEM`), the `reason` of a read that gave the cached list
 * (`source: "cache"`) or the failure line (`source: "failure"`), which the log names, and whether
 * Dispatch answered the read (`source: "fresh"`). Undefined when it gave neither text nor words:
 * Dispatch is off, or has no answers route and no list read from it before has an item inside the
 * 72 hours (none was read, or every item of the one read has aged out). The extension's
 * `sessionAnswersText` calls it, and so does `session-answers-read.test.ts`, whose cases check on
 * reads through the real `readSessionAnswers` the rules by which `withSessionAnswers` keeps every
 * list sent, so those cases check the extension's own mapping.
 */
export function carriedAnswers(read: SessionAnswersRead): CarriedAnswers | undefined {
  if (read.text === undefined && read.words === undefined) return undefined;
  const reason = read.source === "cache" || read.source === "failure" ? read.reason : undefined;
  const answered = read.source === "fresh";
  return { text: read.text, words: read.words, since: read.since, reason, answered };
}

/**
 * The custom type of the session entry (`pi.appendEntry`) that records one answers message a
 * main-loop request sent (`AnswersSent`): a list, the "no item" list, or the failure line. The copy
 * of this extension that decides a request writes one when it appends a message after the
 * request's last message (`SessionAnswersRequest.record`), and every later main-loop request of the
 * session, in this process or in one that resumed the session, rebuilds each message the branch
 * records at its old place with its old bytes (`withSessionAnswers`). Oh My Pi builds no context
 * message from a `custom` entry, so the model reads each one only as a request carries it. A
 * change to the data's shape takes a new custom type: a reader passes over an entry it cannot
 * read, and the message it recorded then leaves the request.
 */
export const ANSWERS_SENT_ENTRY = "envoy-answers-sent";

/** One answers message a request sent, as the session records it (`ANSWERS_SENT_ENTRY`'s data):
 *  `after`, the `messageKey` of the request's last message before the answers message was added,
 *  never a trailing approval message (`withSessionAnswers`), which every later request that still
 *  holds that message carries it right after; `blocks`, the exact text of each of its text blocks
 *  in order, of which there is at least one, which it is rebuilt from with no constant read again,
 *  so a later release that rewords the sentence still rebuilds the old bytes; and `words`, the
 *  read's words when the read gave them (`CarriedAnswers.words`). Its kind is its first block's: a
 *  list, the "no item" list included, starts with `SESSION_ANSWERS_MARKER`, and the failure line
 *  with its own marker. */
export interface AnswersSent {
  readonly after: string;
  readonly blocks: readonly [string, ...string[]];
  readonly words?: readonly string[];
}

/** The sentence every list a request appends carries as its second text block, the "no item"
 *  list's too. A newer list never takes an earlier one's place, since that would change bytes a
 *  request already sent: it goes after the last message, and this sentence tells the model that
 *  it replaces every earlier list and failure line, any a compaction summary repeats included. The
 *  list stays the first block, so its text stays the renderer's capped output and an older copy's
 *  check of the first block still finds the marker. The failure line carries no sentence. */
export const SESSION_ANSWERS_REPLACES =
  "This list replaces every earlier list of people's answers in this conversation, and every earlier line saying the list could not be read: read only the newest one as current.";

/** The first block of the list a request appends in place of taking a list out: when a read gives
 *  no text and its words show an item of the newest list sent withdrawn while its time is inside
 *  the read's window (`sessionAnswersChanged`), or the newest list sent is an older copy's, which
 *  has no words. Its entry records the read's words (`[]`), so a later read with no text compares
 *  equal and appends nothing. */
export const SESSION_ANSWERS_NO_ITEM = `${SESSION_ANSWERS_MARKER}\n\nNo item: every item an earlier list showed was withdrawn or is now older than 72 hours.`;

/** The sessions, by `sessionManager` and then session id (Oh My Pi keeps one `sessionManager` when
 *  it switches sessions), inside a failure episode: from the first read that failed
 *  (`CarriedAnswers.reason`) to the next read Dispatch answers (`CarriedAnswers.answered`, or an
 *  older copy's list with no read line). `SessionAnswersRequest.readFailed` is told on the first
 *  read of an episode, whether the request shows the model a message for it or not, and not again
 *  inside it, so a lasting failure, such as a Dispatch with no answers route or one that misses
 *  the read's deadline on every request, is one log line. On `globalThis` as `conversions` is. A
 *  change to its shape takes a new key (`.v2`). */
const EPISODES_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers-episodes.v1");
const episodesGlobal = globalThis as typeof globalThis & {
  [EPISODES_KEY]?: WeakMap<object, Set<string>>;
};
const episodes = episodesGlobal[EPISODES_KEY] ?? new WeakMap<object, Set<string>>();
episodesGlobal[EPISODES_KEY] = episodes;

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
function readLineFailure(text: string): string | undefined {
  const line = text.split("\n")[1];
  if (line?.startsWith(READ_LINE_PREFIX) !== true) return undefined;
  const failed = "; this read failed: ";
  const at = line.indexOf(failed);
  return at < 0 ? undefined : line.slice(at + failed.length);
}

/** Whether `text` is a list of what people wrote, rather than the failure line, which says the
 *  list could not be read and shows nothing a person wrote. */
function isList(text: string): boolean {
  return text.startsWith(SESSION_ANSWERS_MARKER);
}

/**
 * Whether `next` shows something `sent` did not: an item, an answer or comment a person changed,
 * an item a person retracted, or that the list could not be read. `sent` is the newest answers
 * message a request sent, as its entry records it (`AnswersSent`), read by its first block and
 * its words. The failure line, which a read gives when it has no list to give (a Dispatch config
 * error, a changed Dispatch URL, a cache whose every item aged out, a read that threw), is a
 * change after a list sent, so it is appended after the last message and the model reads that
 * this read failed rather than bytes sent before that look like a read that answered. A read that
 * fails but gives the cached list is a list: with the same items it appends nothing, the request
 * keeps the bytes sent, and only the log says that this read failed
 * (`SessionAnswersRequest.readFailed`). A list after a failure line sent is a change too. Two
 * failure lines show the same thing and differ only in their reason, so a failure line after one
 * sent appends nothing, and the one sent stays the newest message with its own reason, as a list
 * does with its read note.
 *
 * Between two lists, the two reads' words (`sessionAnswersWords`, one string per item Dispatch
 * sent inside the read's window, shown or left out by the list's character cap) answer it
 * (`sessionAnswersChanged`): an item new to `next`, or one whose words changed, is a change, so a
 * new item or an edit the cap leaves out still appends a list, and the model reads the new text
 * with its left-out line rather than the person's old words as current. An item `next` no longer
 * holds is a change while its time is inside `next`'s window, since a person retracted it or
 * Dispatch removed it, and no change once the window has left it behind.
 *
 * A list an older copy of this extension added has no words, and neither has its entry, which
 * records it as this copy's (`placedLegacy`), and then only its rendered text is left to compare.
 * The comparison there is deliberately crude: the texts are the same only when they differ in
 * nothing but the line naming the read, and every other difference counts as something new, so a
 * new list is appended after the last message. Reading that text more closely — by paragraph, or
 * cut into items, each without the line counting what the character cap left out — was tried
 * twice and defeated twice by an item whose own words read like the renderer's: a person's answer
 * quoting another item's first lines, or ending in a sentence of the cap line's shape, was taken
 * for something already shown, and what they wrote reached no request at all. A list appended
 * because only this read's note changed costs a list's worth of new bytes, on a path that lasts
 * only until every copy of this extension carries this change; a swallowed answer costs a
 * person's words. So this path pays the bytes. `addedBlocks` holds the same path's rule for a
 * read with no text, which appends the "no item" list after such a list whenever the read gives
 * words; the older-copy path is in both places, and goes from both.
 */
function showsNew(sent: AnswersSent, next: ShownAnswers): boolean {
  const [shown] = sent.blocks;
  if (isList(shown) !== isList(next.text)) return true;
  if (!isList(next.text)) return false;
  if (sent.words === undefined || next.words === undefined) {
    return withoutReadLine(shown) !== withoutReadLine(next.text);
  }
  return sessionAnswersChanged(sent.words, next.words, next.since);
}

/** Whether `value` is an array of strings. */
function isTexts(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

/** Whether `data` reads as an answers message sent (`AnswersSent`): `after` a string, `blocks`
 *  strings of which there is at least one and the first is a list or the failure line
 *  (`isSessionAnswersText`), and `words`, when set, strings. */
function isAnswersSent(data: unknown): data is AnswersSent {
  if (typeof data !== "object" || data === null) return false;
  if (!("after" in data) || typeof data.after !== "string") return false;
  if (!("blocks" in data) || !isTexts(data.blocks)) return false;
  const first = data.blocks[0];
  if (first === undefined || !isSessionAnswersText(first)) return false;
  return !("words" in data) || data.words === undefined || isTexts(data.words);
}

/**
 * `messages` with every answers message the branch `entries` record (`ANSWERS_SENT_ENTRY`)
 * rebuilt from its blocks right after the last message whose `messageKey` is its `after`, several
 * after one message (a retried request's) in branch order, and those entries in branch order
 * (`present`); undefined when the request holds none of them. An entry whose message the request
 * no longer holds, because a compaction summarized it or the branch moved, is left out of both. An
 * entry whose data does not read as one (`isAnswersSent`) is passed over, with no error and no log:
 * that costs a byte change only after a change of the entry's shape, which takes a new custom type.
 */
function rebuilt(
  messages: readonly unknown[],
  entries: readonly unknown[]
): { readonly messages: unknown[]; readonly present: readonly AnswersSent[] } | undefined {
  const recorded = customEntries(entries, ANSWERS_SENT_ENTRY).filter(isAnswersSent);
  if (recorded.length === 0) return undefined;
  const lastAt = new Map<string, number>();
  for (const [index, message] of messages.entries()) lastAt.set(messageKey(message), index);
  const after = new Map<number, AnswersSent[]>();
  const present: AnswersSent[] = [];
  for (const sent of recorded) {
    const index = lastAt.get(sent.after);
    if (index === undefined) continue;
    present.push(sent);
    const there = after.get(index);
    if (there === undefined) after.set(index, [sent]);
    else there.push(sent);
  }
  if (present.length === 0) return undefined;
  const withSent: unknown[] = [];
  for (const [index, message] of messages.entries()) {
    withSent.push(message);
    for (const sent of after.get(index) ?? []) withSent.push(userTextMessage(...sent.blocks));
  }
  return { messages: withSent, present };
}

/**
 * The failure episode rule for `carried`, the read a request is decided with (this copy's own, or
 * an older copy's list): a read Dispatch answered ends the session's episode (`episodes`); the
 * first failed read of an episode tells `request.readFailed` why it failed, and whether the
 * request appends a message for it (`shown`); a later failed read of the same episode tells
 * nothing. The model reads a failure only in a message a request appends: the failure line, or
 * the cached list when it shows something new, whose read line names it. A failed read that gives
 * the cached list with the same items, or a failure line after one already sent, keeps the
 * messages sent, which do not name this read's failure, and then only the log says it.
 */
function tellFailure(
  carried: CarriedAnswers | undefined,
  request: SessionAnswersRequest,
  shown: boolean
): void {
  if (carried?.answered === true) {
    episodes.get(request.session)?.delete(request.id);
    return;
  }
  const reason = carried?.reason;
  if (reason === undefined) return;
  const failing = episodes.get(request.session) ?? new Set<string>();
  episodes.set(request.session, failing);
  if (failing.has(request.id)) return;
  failing.add(request.id);
  request.readFailed(reason, shown);
}

/**
 * `messages` for a request made while a compaction may be converting the conversation: the read's
 * text after the last message, as a request that holds no answers message sent appends it
 * (`addedBlocks`), with none of the messages sent rebuilt and nothing recorded; undefined when the
 * read gave no text. A remote compaction keeps only as many messages of its whole conversion as
 * its cut's conversion holds, so a message before the cut would push one out. A main-loop request
 * beside a running speculative compaction therefore carries none of the messages sent where they
 * went, and its bytes differ from the request before it from the first of them on: the known
 * limit of this path. The next request made with no compaction running rebuilds every message
 * sent whose anchor it holds, and compares its read with the newest of them, so a change this path
 * showed is appended again.
 */
function whileCompacting(
  messages: readonly unknown[],
  carried: CarriedAnswers | undefined,
  request: SessionAnswersRequest
): unknown[] | undefined {
  const added = addedBlocks([], carried);
  tellFailure(carried, request, added !== undefined);
  return added === undefined ? undefined : [...messages, userTextMessage(...added)];
}

/** The blocks of the answers message `carried` appends after `present`, the answers messages sent
 *  that the request holds, in branch order, under `withAnswersSent`'s rule; undefined when it
 *  appends none. */
function addedBlocks(
  present: readonly AnswersSent[],
  carried: CarriedAnswers | undefined
): AnswersSent["blocks"] | undefined {
  const text = carried?.text;
  if (carried !== undefined && text !== undefined) {
    const next = { ...carried, text };
    const newest = present.at(-1);
    if (newest !== undefined && !showsNew(newest, next)) return undefined;
    return isList(text) ? [text, SESSION_ANSWERS_REPLACES] : [text];
  }
  const words = carried?.words;
  if (words === undefined) return undefined;
  const list = present.findLast((sent) => isList(sent.blocks[0]));
  return list !== undefined &&
    (list.words === undefined || sessionAnswersChanged(list.words, words, carried?.since))
    ? [SESSION_ANSWERS_NO_ITEM, SESSION_ANSWERS_REPLACES]
    : undefined;
}

/**
 * `messages` for a main-loop request with `carried`, the read it is decided with, under the one
 * rule this copy's own reads and an older copy's list (`placedLegacy`) both follow. Every answers
 * message the branch `entries` record whose anchor the request holds is rebuilt where it went,
 * with its bytes (`rebuilt`), and one is appended after the last message only when the read shows
 * a change against them (`addedBlocks`):
 *
 * - a read with text appends it when the request holds none of them, or when it shows something
 *   the newest of them did not (`showsNew`): a list with `SESSION_ANSWERS_REPLACES` as its second
 *   block, the failure line alone;
 * - a read with no text that gives words appends the "no item" list (`SESSION_ANSWERS_NO_ITEM`)
 *   when the newest list among them shows an item those words no longer hold while its time is
 *   inside the read's window (`sessionAnswersChanged`), or is an older copy's, which has no words.
 *   An item that only aged out appends nothing, and so does a read when nothing was listed before,
 *   and a failure line sent after the newest list stays the newest message: a recovery to an empty
 *   window has nothing new to say;
 * - a read with neither text nor words (Dispatch off, or no answers route and nothing cached)
 *   appends nothing.
 *
 * The message appended is recorded (`request.record`) with the key of the request's last message
 * and the read's words, so the next request rebuilds it right after that message and compares its
 * own read with it; the request carries it whether or not the write happened. The read's failure
 * episode is told whether a message is appended (`tellFailure`). Undefined when the request
 * carries no answers message at all.
 */
function withAnswersSent(
  messages: readonly unknown[],
  entries: readonly unknown[],
  carried: CarriedAnswers | undefined,
  request: SessionAnswersRequest
): unknown[] | undefined {
  const sent = rebuilt(messages, entries);
  const added = addedBlocks(sent?.present ?? [], carried);
  tellFailure(carried, request, added !== undefined);
  if (added === undefined) return sent?.messages;
  const words = carried?.words;
  request.record({
    after: messageKey(messages.at(-1)),
    blocks: added,
    ...(words === undefined ? {} : { words }),
  });
  return [...(sent?.messages ?? messages), userTextMessage(...added)];
}

/** `messages` decided with `carried`: through the compaction path (`whileCompacting`) when
 *  `compacting`, else through the rule over the answers messages the branch `entries` record
 *  (`withAnswersSent`), the only path that calls `entries`. `withSessionAnswers` hands it the
 *  branch read again after the read, once every earlier request of the session has decided
 *  (`decidedInTurn`); `placedLegacy`, which awaits nothing, the branch as it is. A result is added
 *  to `decided`, so no other copy of this extension decides the request again. */
function decide(
  messages: readonly unknown[],
  entries: () => readonly unknown[],
  carried: CarriedAnswers | undefined,
  request: SessionAnswersRequest,
  compacting: boolean
): unknown[] | undefined {
  const result = compacting
    ? whileCompacting(messages, carried, request)
    : withAnswersSent(messages, entries(), carried, request);
  if (result !== undefined) decided.add(result);
  return result;
}

export interface SessionAnswersRequest {
  /** The session's branch entries (`sessionManager.getBranch`), or undefined once the host is on
   *  another session than the one the request was built from (`id`), whose branch holds none of
   *  this request's answers messages. `withSessionAnswers` reads it before the read, for where the
   *  request ends (`branchPosition`), and again after it, once every earlier request of the
   *  session has decided, for every answers message an earlier request of the session sent
   *  (`ANSWERS_SENT_ENTRY`); when the second read gives undefined, the request decides from the
   *  first. */
  readonly branch: () => readonly unknown[] | undefined;
  /** The session the request is for (its `sessionManager`): the key of its last conversion, and
   *  with `id` the key of its failure episode. */
  readonly session: object;
  /** The session's id (`sessionManager.getSessionId`) as the request began: with `session`, the
   *  key of its failure episode. */
  readonly id: string;
  /** Whether Oh My Pi is compacting the session now: the live `AgentSession`'s `isCompacting`, or
   *  its `compactionSpeculation` reading `"running"`. */
  readonly compacting: () => boolean;
  /** This request's read (`carriedAnswers`): the list or the failure line, a read with no text
   *  that still gives its words, or undefined when it gave neither. A read with text is appended
   *  when it shows something the newest answers message sent did not; a read with no text appends
   *  the "no item" list when its words show an item of the newest list sent withdrawn inside the
   *  window; any other read appends nothing (`withAnswersSent`). */
  readonly read: () => Promise<CarriedAnswers | undefined>;
  /** Records in the session the answers message this request appends after its last message
   *  (`ANSWERS_SENT_ENTRY`), so every later request of the session, in this process or in one
   *  that resumed the session, rebuilds it where it went with the bytes it carried. Called at
   *  most once per request, only by the copy that decided the request: after its read and after
   *  every earlier request of the session that read has decided, or at once for a list an older
   *  copy added (`placedLegacy`), and never while a compaction may be converting the
   *  conversation. The request carries the message whether or not the write happened: the
   *  extension writes nothing when the host moved to another session while the read waited, since
   *  the entry belongs to the session the request was built from, and logs that, or a write that
   *  failed. */
  readonly record: (entry: AnswersSent) => void;
  /** Told why a read failed (`CarriedAnswers.reason`) on the first failed read of the session's
   *  failure episode (`episodes`), and whether this request shows the model a message for it
   *  (`shown`: the failure line, or the cached list when it shows something new, appended after
   *  the last message) or keeps the answers messages sent, which do not name this read's failure.
   *  Not told again until a read of the session has answered (`CarriedAnswers.answered`, or an
   *  older copy's list with no read line), so a lasting failure, such as a Dispatch with no
   *  answers route, is told once, shown or not. The extension logs it. */
  readonly readFailed: (reason: string, shown: boolean) => void;
}

/**
 * The messages one main-loop provider request carries, with what people wrote to the session as
 * separate user messages that no later request moves, edits or takes out. Every answers message a
 * request appends is recorded in the session (`ANSWERS_SENT_ENTRY`, through `request.record`),
 * except while a compaction may be converting the conversation (below), and each later request
 * rebuilds every one whose anchor it still holds right after that message, from the blocks
 * recorded (`withAnswersSent`). A read that shows a change appends one more message after the last
 * message and records it: a list, when the request holds no message sent or the list shows
 * something the newest of them did not, with the sentence `SESSION_ANSWERS_REPLACES` as its second
 * block, so the model reads only the newest as current; the failure line, when the request holds
 * no message sent or the newest is a list, so a run of failed reads appends it once; the "no item"
 * list (`SESSION_ANSWERS_NO_ITEM`), when a read with no text shows an item of the newest list sent
 * withdrawn inside the window. Nothing else is appended: a failed read that gives the cached list
 * with the same items keeps the bytes sent, and a recovery to an empty window keeps the failure
 * line sent as the newest message. So a request whose read shows nothing new carries the same
 * bytes as the request before it, a request whose read shows a change carries them up to its new
 * message, and the first request of a process that resumed the session rebuilds the same messages
 * from the session file. An answers message whose anchor a compaction summarized, or that the
 * branch moved away from, leaves the request and the comparison, so the next read with text goes
 * after the last message. The first failed read of a failure episode is told to
 * `request.readFailed`, which the extension logs, and not again until a read answers.
 *
 * The branch is read before the read, for where the request ends (`branchPosition`), and again
 * after it, for the answers messages sent, once every earlier request of the session has decided
 * (`deciding`): the request starts its read at once, and decides when both its read and the
 * decision of the request before it have settled (`decidedInTurn`). Oh My Pi does not wait for
 * the `context` handler of a run that was stopped: it races the handler against the run and drops
 * what the handler returns, and the handler goes on to its read and its record. The next run's
 * request can start while that read is still open; in this order it decides after the stopped
 * one, rebuilds the message that one appended where it went, and appends only what that message
 * did not show, so a stopped request whose handler goes on and the request after it never both
 * append the same change, whichever read settles first. A read waits on Dispatch at most its
 * deadline (800 ms, `readSessionAnswers`), and the reads of the requests before this one started
 * before its own, so this request waits on them about as long as on its own read. The second read
 * of the branch is of the session the request was built from: once the host is on another,
 * `request.branch` gives undefined, and the request decides from the branch as it began, and the
 * extension's `record` writes nothing.
 *
 * While a compaction may be converting the conversation (`request.compacting`, or the request
 * right after a remote compaction's cut) the request carries none of the messages sent, and the
 * read's text goes after the last message and is not recorded (`whileCompacting`): such a
 * request's bytes differ from the request before it from the first message sent on, this path's
 * known limit. Undefined when the request carries no answers message: the read appends nothing and
 * the request holds no message sent, or one of these, for which `read` is not called and which
 * takes no place in that order:
 *
 * - another copy of this extension already decided this request: it added its answers messages
 *   (and passed on the array it returned), or it read and found none to carry, or the request is
 *   one of those below;
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
 *   with a tail, whose whole conversion it matches, and carries the read's list.
 *
 * No answers message is ever at the head, and none is ever merged into a message of the
 * conversation, whose bytes and cache anchors are the conversation's. Oh My Pi marks a message a
 * `context` handler adds as per-call, and its Anthropic placement puts no cache anchor on a
 * per-call message, so every byte before the message a change appends, the messages sent before
 * it included, is the cached prefix of the request before it.
 *
 * A request that ends with a run of approval messages (`splitTrailingApproval`: the message
 * pi-legion places while a person's approval of the spec stands) is decided as the request up to
 * the message before that run (`withAnswers`), and the run goes back after what that gives, so the
 * approval message stays the request's last message whichever plugin's `context` handler runs
 * first. The message before the run is where the request ends on the branch (`branchPosition`),
 * the last message every check above reads, and the anchor an answers message appended is
 * recorded after: an approval message is no session message, and a later request holds it only
 * where pi-legion rebuilds it, so no answers message anchors on one.
 */
export async function withSessionAnswers(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): Promise<unknown[] | undefined> {
  if (decided.has(messages)) return undefined;
  decided.add(messages);
  const { head, tail } = splitTrailingApproval(messages);
  const answered = await withAnswers(head, request);
  if (answered === undefined || tail.length === 0) return answered;
  const withApproval = [...answered, ...tail];
  decided.add(withApproval);
  return withApproval;
}

/**
 * `withSessionAnswers`'s rule for `messages`, a request without its trailing approval messages.
 * It awaits nothing, so a request that reads takes its place among the session's requests
 * (`deciding`) as it begins, in the order the requests began.
 */
function withAnswers(
  messages: readonly unknown[],
  request: SessionAnswersRequest
): Promise<unknown[] | undefined> | unknown[] | undefined {
  if (endsWithSessionAnswers(messages)) return placedLegacy(messages, request);
  if (isSideTurn(messages) || isHandoffPrompt(messages)) return undefined;
  const entries = request.branch() ?? [];
  const { conversion, kind } = nextConversion(conversions.get(request.session), messages, entries);
  conversions.set(request.session, conversion);
  if (kind === "cut" || (kind === "repeat" && request.compacting())) return undefined;
  const earlier = deciding.get(request.session);
  const decision = decidedInTurn(messages, entries, earlier, kind === "afterCut", request);
  deciding.set(
    request.session,
    decision.then(
      () => undefined,
      () => undefined
    )
  );
  return decision;
}

/**
 * `messages` for a request that read, decided in its turn among the session's requests: it starts
 * `request.read` at once, and decides once both that read and `earlier`, the decision of the
 * session's request before it (`deciding`), have settled, whichever settles first, with the branch
 * read again then; with `entries`, the branch as the request began, when the second read gives
 * undefined because the host is on another session. A read that throws still waits on `earlier`
 * before this decision fails with its error, so the request after it never decides ahead of the
 * one before it. `afterCut` says the request came right after a remote compaction's cut
 * (`nextConversion`): it goes through the compaction path.
 */
async function decidedInTurn(
  messages: readonly unknown[],
  entries: readonly unknown[],
  earlier: Promise<void> | undefined,
  afterCut: boolean,
  request: SessionAnswersRequest
): Promise<unknown[] | undefined> {
  const [read] = await Promise.allSettled([request.read(), earlier]);
  if (read.status === "rejected") throw read.reason;
  const compacting = afterCut || request.compacting();
  return decide(messages, () => request.branch() ?? entries, read.value, request, compacting);
}

/**
 * The request an older copy of this extension, which puts the list after the last message of
 * every request, already gave its list: that list stands in for this copy's read, with no read of
 * this copy's own, and the request without it goes through the same rule as this copy's reads
 * (`withAnswersSent`), or the compaction path (`whileCompacting`). So the messages sent are
 * rebuilt where they went, and the older copy's list is appended, and recorded as this copy's
 * message with the sentence, only when it shows something the newest message sent did not. The
 * older copy decided which requests carry one. A list it rendered from its cache names the failed
 * read on its second line (`As read at …; this read failed: …`); that failure is the read's
 * `reason` (`readLineFailure`), so `readFailed` is told it under the same episode rule as this
 * copy's own reads. Every renderer writes a list without that line only for a read Dispatch
 * answered, so such a list ends the session's failure episode, as this copy's own answered read
 * does, and the next failure an older copy's list names is told again. An older copy's failure
 * line names no reason here and ends no episode.
 *
 * It decides at once, with no await: nothing can come between its read of the branch and its
 * record, so it takes no place in the order in which the session's requests that read decide
 * (`deciding`), and reads the branch as it is (a branch that gives undefined, the host on
 * another session, reads as empty).
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
  const reason = readLineFailure(text);
  const carried: CarriedAnswers = {
    text,
    reason,
    answered: reason === undefined && isList(text),
  };
  return decide(messages.slice(0, -1), () => request.branch() ?? [], carried, request, compacting);
}
