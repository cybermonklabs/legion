import { isSessionAnswersText } from "@legion/envoy-client/session-answers";
import type * as host from "@oh-my-pi/pi-coding-agent";

/**
 * How both plugins' `context` handlers read one model request: which kind of request it is (a side
 * turn, the handoff prompt, a compaction's own), where it ends on the session's branch, and the
 * roles, timestamps, keys and texts they match messages on. pi-legion places the approval message
 * and pi-envoy its people's-answers message; neither plugin's shipped code imports the other's,
 * so what both read lives here, one body for both.
 */

/** A message's role, or undefined where it has none. */
export function roleOf(message: unknown): unknown {
  return typeof message === "object" && message !== null && "role" in message
    ? message.role
    : undefined;
}

/** A message's numeric timestamp, or undefined where it has none. */
export function timestampOf(message: unknown): number | undefined {
  return typeof message === "object" &&
    message !== null &&
    "timestamp" in message &&
    typeof message.timestamp === "number"
    ? message.timestamp
    : undefined;
}

/** The timestamp pi-envoy puts on each user message it adds to a request (`userTextMessage`). A
 *  message a person sends carries the time it was sent, so the stamp tells such an added message
 *  from a person's own. Every pi-envoy release so far stamps 0: with a different value, a reader
 *  of this stamp takes an older copy's message for a person's own. */
export const ADDED_MESSAGE_TIMESTAMP = 0;

/** The text of `message`'s first content block when `message` has `role` and that block has text:
 *  where a handler looks for a message of its own, and how it reads the host's own prompts. */
export function leadingText(message: unknown, role: string): string | undefined {
  if (
    typeof message !== "object" ||
    message === null ||
    !("role" in message) ||
    message.role !== role ||
    !("content" in message) ||
    !Array.isArray(message.content)
  ) {
    return undefined;
  }
  const [first]: unknown[] = message.content;
  if (
    typeof first === "object" &&
    first !== null &&
    "text" in first &&
    typeof first.text === "string"
  ) {
    return first.text;
  }
  return undefined;
}

/** Whether `message` is a user message whose first text block is pi-envoy's people's-answers list
 *  or the line saying the list could not be read (`isSessionAnswersText`). It does not read the
 *  stamp, so a person's own message that quotes the marker matches too. A caller that must tell
 *  the two apart reads `isAddedAnswersMessage`. */
export function isSessionAnswersMessage(message: unknown): boolean {
  const text = leadingText(message, "user");
  return text !== undefined && isSessionAnswersText(text);
}

/** The text of `message` when the host wrote it (`attribution: "agent"`) with `role`. */
function hostText(message: unknown, role: string): string | undefined {
  return typeof message === "object" &&
    message !== null &&
    "attribution" in message &&
    message.attribution === "agent"
    ? leadingText(message, role)
    : undefined;
}

/** A context message as matching reads it: its role and timestamp, and a tool result's call id,
 *  since the tool results of one round can share a timestamp. */
export function messageKey(message: unknown): string {
  if (typeof message !== "object" || message === null) return "";
  const role = "role" in message ? String(message.role) : "";
  const timestamp = "timestamp" in message ? String(message.timestamp) : "";
  const call = "toolCallId" in message ? `#${String(message.toolCallId)}` : "";
  return `${role}@${timestamp}${call}`;
}

/**
 * The context message a branch entry becomes, as far as matching reads it, or undefined for an
 * entry that becomes none. Oh My Pi builds a compaction's messages from the branch with
 * `getMessageFromEntry` (`packages/agent/src/compaction/messages.ts:302-320` at the pinned tag;
 * 18.12.2's bundle named it `MI`, and the two constructors `yu` and `hm`):
 *   if (entry.type === "message") return entry.message;
 *   if (entry.type === "custom_message") return createCustomMessage(entry.customType, entry.content,
 *     entry.display, entry.details, entry.timestamp, entry.attribution);
 *   if (entry.type === "branch_summary") return createBranchSummaryMessage(entry.summary,
 *     entry.fromId, entry.timestamp);
 * where `createCustomMessage` returns `{role: "custom", ..., timestamp: new Date(timestamp).getTime()}`
 * and `createBranchSummaryMessage` `{role: "branchSummary", ..., timestamp: new Date(timestamp).getTime()}`.
 * A `compaction` entry becomes no message there: the earlier summary heads the conversion, so no cut
 * ends on it.
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
export type BranchPosition =
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
 * conversion: a message a `context` handler added to the first would make it keep one message past
 * the cut. That conversion ends at a message the branch holds a later one after (`before`). A
 * main-loop request ends at a message not yet written, or at the branch's last. A failed or
 * aborted reply, and the results the host wrote for its tool calls, which a retry drops from the
 * context but not from the branch (`turn-recovery`), do not count as later. Both plugins read it:
 * pi-envoy adds no answers message to such a conversion, and pi-legion places no approval message.
 */
export function branchPosition(
  messages: readonly unknown[],
  entries: readonly unknown[]
): BranchPosition {
  const last = messages.at(-1);
  if (typeof last !== "object" || last === null || !("timestamp" in last) || !("role" in last)) {
    return { at: "absent" };
  }
  /** The tool call ids of the branch's failed or aborted replies, read the first time the walk
   *  meets a tool result: most requests end at the branch's last message and never need them. */
  let dropped: Set<string> | undefined;
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
    if (message.role === "toolResult" && dropped === undefined) {
      dropped = new Set(
        entries.flatMap((candidate) => {
          const written = entryMessage(candidate);
          return written === undefined ? [] : failedCalls(written);
        })
      );
    }
    const failed =
      (message.role === "assistant" &&
        (message.stopReason === "error" || message.stopReason === "aborted")) ||
      (message.role === "toolResult" && dropped?.has(String(message.toolCallId)) === true);
    if (!failed) later = true;
  }
  return { at: "absent" };
}

/** `messages` cut before its trailing run of messages that `matches`: `head`, every message up to
 *  the last one that does not match, and `tail`, the messages after it, in order. A request that
 *  does not end with a match is all `head`, the same array. */
export function splitTrailing(
  messages: readonly unknown[],
  matches: (message: unknown) => boolean
): { readonly head: readonly unknown[]; readonly tail: readonly unknown[] } {
  let end = messages.length;
  while (end > 0 && matches(messages[end - 1])) end -= 1;
  return end === messages.length
    ? { head: messages, tail: [] }
    : { head: messages.slice(0, end), tail: messages.slice(end) };
}

/** Whether `message` is a people's-answers message pi-envoy added to the request: an answers
 *  message (`isSessionAnswersMessage`) stamped `ADDED_MESSAGE_TIMESTAMP`, as pi-envoy stamps each
 *  user message it adds (`userTextMessage`). A person's own message that quotes the marker carries
 *  the time it was sent. */
export function isAddedAnswersMessage(message: unknown): boolean {
  return timestampOf(message) === ADDED_MESSAGE_TIMESTAMP && isSessionAnswersMessage(message);
}

/** A session's last conversion, as each plugin keeps it in a store of its own: a remote
 *  compaction's cut (`"cut"`), or a request with its key (undefined unless it ended at the branch's
 *  last message) and whether it came right after a cut. */
export type Conversion = "cut" | { readonly key: string | undefined; readonly afterCut: boolean };

/**
 * What one request is among its session's conversions (`nextConversion`): a conversion of the
 * conversation up to a remote compaction's cut (`cut`), the request right after one (`afterCut`),
 * a repeat of the conversion before it over the same branch (`repeat`), or none of these (`new`).
 */
export type ConversionKind = "cut" | "afterCut" | "repeat" | "new";

/**
 * The session's conversion after a request of `messages` on the branch `entries`, given the
 * session's last one (`previous`), and what the request is among them. Oh My Pi's Anthropic remote
 * compaction converts the conversation up to its cut, which ends at a message the branch holds a
 * later one after (`branchPosition`), and then the whole conversation. One that keeps no tail
 * converts the same messages twice, both ending at the branch's last message, so the second is a
 * `repeat` of the first: the same key, which is the branch's last message and each message's role
 * and timestamp (`messageKey`). A repeat is a compaction's own only while the session compacts
 * (`sessionCompacting`), which each caller asks. The request right after a cut is `afterCut`,
 * never a repeat, and each plugin reads it its own way. Both plugins read this: pi-envoy adds no
 * answers message to a compaction's own conversion, and pi-legion places no approval message
 * there. Each keeps its own store of conversions.
 *
 * The position and the key are read without the request's trailing run of pi-envoy's answers
 * messages (`isAddedAnswersMessage`), messages built for that request alone that the branch never
 * holds, so both plugins read one request alike whichever plugin's `context` handler runs first.
 * pi-envoy, run first while the session compacts, adds one to a compaction's first conversion and
 * none to its repeat; read with it, pi-legion would find the first ending at no message of the
 * branch, and the repeat new. pi-envoy reads its own requests before it adds one, so for it the run
 * is always empty; it cuts the approval messages pi-legion places itself (`splitTrailingApproval`).
 */
export function nextConversion(
  previous: Conversion | undefined,
  messages: readonly unknown[],
  entries: readonly unknown[]
): { readonly conversion: Conversion; readonly kind: ConversionKind } {
  const conversation = splitTrailing(messages, isAddedAnswersMessage).head;
  const position = branchPosition(conversation, entries);
  if (position.at === "before") return { conversion: "cut", kind: "cut" };
  const key =
    position.at === "last" ? [position.tip, ...conversation.map(messageKey)].join("\n") : undefined;
  const conversion: Conversion = { key, afterCut: previous === "cut" };
  if (previous === "cut") return { conversion, kind: "afterCut" };
  const repeat =
    key !== undefined && typeof previous === "object" && previous.key === key && !previous.afterCut;
  return { conversion, kind: repeat ? "repeat" : "new" };
}

/** The first line of Oh My Pi's side-turn notice (`side-channel-no-tools.md`): the `developer`
 *  message `AgentSession.runEphemeralTurn` puts after the conversation, before the side prompt. */
const SIDE_TURN_NOTICE = "Ephemeral side-channel turn; reuses current conversation context.";

/**
 * Whether the request is a side turn: Oh My Pi's `runEphemeralTurn` (a Dispatch BTW, the stop-time
 * self-check, `/btw`, the idle recap) converts `[...conversation, notice, ...history, prompt]`,
 * where the notice is a host `developer` message, the history `/btw`'s earlier questions and
 * answers, and the prompt a host user message.
 */
export function isSideTurn(messages: readonly unknown[]): boolean {
  if (hostText(messages.at(-1), "user") === undefined) return false;
  for (let index = messages.length - 2; index >= 0; index -= 1) {
    const message = messages[index];
    const role = roleOf(message);
    if (role !== "user" && role !== "assistant") {
      return hostText(message, "developer")?.includes(SIDE_TURN_NOTICE) === true;
    }
  }
  return false;
}

/** How Oh My Pi's handoff prompt (`handoff-document.md`) begins. */
const HANDOFF_PROMPT = "<critical>\nWrite a handoff document for another instance of yourself.";

/** Whether the request is Oh My Pi's handoff prompt: its last message is a host user message that
 *  begins with `HANDOFF_PROMPT`. The same words from the person are their own prompt. */
export function isHandoffPrompt(messages: readonly unknown[]): boolean {
  return hostText(messages.at(-1), "user")?.startsWith(HANDOFF_PROMPT) === true;
}

/** The host's roster of live sessions as a plugin reads it: `host.AgentRegistry`, which a host
 *  build without that export leaves undefined. The caller passes it in, so this module loads where
 *  the host package does not resolve. */
export type HostAgentRegistry = { global(): { list(): readonly host.AgentRef[] } } | undefined;

/** The host's live `AgentSession`, as far as these plugins read it. */
export type LiveAgentSession = NonNullable<host.AgentRef["session"]>;

/**
 * The live `AgentSession` under `session`, its live id, found in the host's roster (`registry`) at
 * every call, since the host keeps one `AgentSession` across `/new`, `/resume` and a fork and
 * changes its id. Undefined on a host without the registry export, or listing no live session
 * under that id.
 */
export function liveAgentSession(
  registry: HostAgentRegistry,
  session: string
): LiveAgentSession | undefined {
  return (
    registry
      ?.global()
      .list()
      .find((ref) => ref.session?.sessionManager.getSessionId() === session)?.session ?? undefined
  );
}

/**
 * Whether Oh My Pi is compacting `session` now, so that the request a `context` handler sees may be
 * a compaction's own: the live `AgentSession`'s `isCompacting` (an automatic or manual compaction,
 * or a handoff), or its `compactionSpeculation` reading `"running"` (a speculative one), getters
 * the declared host surface leaves out. A speculative compaction runs beside the main loop, so a
 * main-loop request made while it runs reads as compacting too. A session the roster does not
 * list reads as not compacting.
 */
export function sessionCompacting(registry: HostAgentRegistry, session: string): boolean {
  const live = liveAgentSession(registry, session);
  return (
    live !== undefined &&
    (("isCompacting" in live && live.isCompacting === true) ||
      ("compactionSpeculation" in live && live.compactionSpeculation === "running"))
  );
}
