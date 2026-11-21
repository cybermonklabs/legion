import { messageFor } from "@legion/envoy-client/errors";
import {
  APPROVAL_MESSAGE_TYPE,
  type Conversion,
  type HostAgentRegistry,
  isApprovalMessage,
  isHandoffPrompt,
  isSideTurn,
  messageKey,
  nextConversion,
  sessionCompacting,
} from "@legion/pi-shared/approval-message";
import { customEntries } from "@legion/pi-shared/custom-entry";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { SessionContext } from "@legion/pi-shared/pi-types";

/**
 * The approval message: while a person's approval of a root's spec stands, a root architect's own
 * session carries one message in each model request, in the daemon's words
 * (`POST /legion/v1/claims/approval`). It is a custom message of pi-shared's
 * `APPROVAL_MESSAGE_TYPE`, text only, attribution `agent`, which Oh My Pi sends as a `system`
 * message inside the conversation. It is never a session message: a `context` handler places it
 * in each request, and the session keeps only a record of each copy placed.
 *
 * - **A new placement.** A request of the conversation's own loop whose route answer stands at a
 *   version the request holds no placed copy of gets the message as its last entry, stamped with
 *   the time of its placement. pi-envoy's people's-answers message goes before it, whichever
 *   handler runs first (pi-shared's `splitTrailingApproval`).
 * - **The record.** When the answer to that request is saved, the copy is recorded with
 *   `pi.appendEntry` as `APPROVAL_SENT_ENTRY`: its version, its text, its timestamp and the key of
 *   the assistant message that answered (`messageKey`).
 * - **Every later request** that holds that answer gets the copy right before it, rebuilt from the
 *   record with the same bytes, never from the route: after a changes request or a later approval
 *   too, and in a process that resumed the session. Moving or removing it would change the prefix
 *   that the later replies' signed thinking is bound to. A compaction that takes the answer out
 *   leaves no copy, and the next request places the message again.
 * - **No new placement** in a side turn, the handoff prompt or a compaction's own request; each of
 *   those rebuilds the copies whose answers it holds, so a remote compaction's two conversions
 *   count the same messages before its cut.
 *
 * The route is read at `session_start`, and again before the next request of the loop after an
 * entry that is not the model's or a tool's entered the session (a daemon task, an Envoy card, a
 * compaction summary). A read waits at most `APPROVAL_READ_DEADLINE_MS`; one that fails keeps the
 * last answer, and the next request reads again.
 */

/** The custom type of the session entry that records one placed copy (`ApprovalSent`). A reader
 *  of the session store sees from these entries when the architect got the message. A change to
 *  the data's shape takes a new custom type: a reader passes over an entry it cannot read. */
export const APPROVAL_SENT_ENTRY = "legion-approval-sent";

/** The longest a read of the approval route holds a request, as pi-envoy's answers read does. */
export const APPROVAL_READ_DEADLINE_MS = 800;

/** One placed copy, recorded once its answer was saved and never rewritten: the version the
 *  message names, its text byte for byte, its timestamp (the time it was first placed, which every
 *  rebuilt copy carries, so Oh My Pi keys it the same on every request), and the `messageKey` of
 *  the assistant message that answered the request it was placed in (`assistant@<timestamp>`). */
export interface ApprovalSent {
  readonly version: number;
  readonly text: string;
  readonly timestamp: number;
  readonly answer: string;
}

export interface ApprovalMessageLog {
  readonly info: (message: string, fields?: Record<string, unknown>) => void;
  readonly warn: (message: string, fields?: Record<string, unknown>) => void;
}

export interface ApprovalMessageDeps {
  /** The claim the session with that id registered, or undefined. The message is placed only for
   *  a root architect's claim (`role` architect on its tree's own issue). */
  readonly claim: (sessionID: string) => ClaimCapability | undefined;
  /** The pane's daemon client, built lazily by the entry that owns it. */
  readonly daemon: () => Pick<LegionDaemonClient, "approval">;
  /** `pi.appendEntry`. */
  readonly appendEntry: (customType: string, data: ApprovalSent) => void;
  /** The host's roster of live sessions (`host.AgentRegistry`), for the test of a remote
   *  compaction's own whole conversion (`sessionCompacting`). */
  readonly registry: HostAgentRegistry;
  readonly now: () => number;
  readonly log: ApprovalMessageLog;
}

export interface ApprovalMessage {
  /** `session_start`: reads the route for a root architect's own session. */
  readonly sessionStart: (context: SessionContext) => Promise<void>;
  /** A `context` handler's work on `messages`, the array the host or the handler before handed
   *  on, edited in place: every copy rebuilt, and a new placement where one is due. True when it
   *  changed the array. */
  readonly context: (messages: unknown[], context: SessionContext) => Promise<boolean>;
  /** `message_end`: records a new placement once its answer is saved, and marks the route's
   *  answer stale when an entry that is not the model's or a tool's entered the session. */
  readonly messageEnd: (message: unknown, context: SessionContext) => void;
  /** `session_compact`: a compaction summary entered the session. */
  readonly sessionCompact: (context: SessionContext) => void;
  /** `before_provider_request`: the wire body of the one request whose new placement Oh My Pi's
   *  date reminder follows, with the reminder moved where the next request has it; undefined for
   *  every other body. */
  readonly beforeProviderRequest: (payload: unknown, context: SessionContext) => object | undefined;
}

/** The approval the route last answered: its document, the version approved and the message. */
interface Standing {
  readonly document: string;
  readonly version: number;
  readonly text: string;
}

/** A new placement whose answer is not saved yet. Every request of the loop places this same
 *  copy, the same bytes and timestamp, until an answer to one of them is saved, or the standing
 *  version changes. */
interface Pending {
  readonly session: string;
  readonly version: number;
  readonly text: string;
  readonly timestamp: number;
}

/** The description of the host's per-call mark (`kPerCallContextMessage`,
 *  `ai/src/utils/block-symbols.ts:170` at the pinned tag): a `Symbol()`, not a `Symbol.for`, so a
 *  plugin reads it by its description. Oh My Pi copies it onto each wire message it converts from
 *  a message built for one request alone (a `context` handler's, the date reminder's trailing
 *  copy). */
const PER_CALL = "agent.message.perCallContext";

/** Oh My Pi's date reminder as its control message carries it on the wire
 *  (`prompts/system/date-cwd-reminder.md`, rendered and trimmed by `renderDateCwdReminder`, with
 *  the local date as `YYYY-MM-DD`, at the pinned tag). */
const DATE_REMINDER =
  /^<system-reminder>\nToday: \d{4}-\d{2}-\d{2}; current working directory: '.*'\. Do not repeat this information in your reply\.\n<\/system-reminder>$/;

function approvalMessage(text: string, timestamp: number): Record<string, unknown> {
  return {
    role: "custom",
    customType: APPROVAL_MESSAGE_TYPE,
    content: [{ type: "text", text }],
    display: false,
    attribution: "agent",
    timestamp,
  };
}

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

/**
 * Whether an assistant message reaches the next request: Oh My Pi drops a turn that a `length`,
 * `error` or `aborted` stop cut off with no text and no tool call (`isSurvivingAssistant`,
 * `ai/src/providers/transform-messages.ts:518-528` at the pinned tag), and an interrupted one
 * with no content. Such a turn answered nothing a later request holds, so it gets no record.
 */
function survives(message: unknown): boolean {
  if (typeof message !== "object" || message === null) return false;
  const stop = "stopReason" in message ? message.stopReason : undefined;
  const truncated = stop === "length" || stop === "error" || stop === "aborted";
  const content: unknown[] =
    "content" in message && Array.isArray(message.content) ? message.content : [];
  if (content.length === 0) return !truncated;
  return content.some((block: unknown) => {
    const type =
      typeof block === "object" && block !== null && "type" in block ? block.type : undefined;
    return type === "toolCall" || type === "text" || !truncated;
  });
}

function isApprovalSent(data: unknown): data is ApprovalSent {
  return (
    typeof data === "object" &&
    data !== null &&
    "version" in data &&
    Number.isInteger(data.version) &&
    "text" in data &&
    typeof data.text === "string" &&
    data.text !== "" &&
    "timestamp" in data &&
    typeof data.timestamp === "number" &&
    "answer" in data &&
    typeof data.answer === "string"
  );
}

/** Whether the host marked `message` per-call (`PER_CALL`): built for one request alone. */
export function perCall(message: unknown): boolean {
  if (typeof message !== "object" || message === null) return false;
  const marks = message as Record<symbol, unknown>;
  return Object.getOwnPropertySymbols(message).some(
    (symbol) => symbol.description === PER_CALL && marks[symbol] === true
  );
}

/** A wire message's text: its string content, or its text blocks joined as Oh My Pi joins them
 *  for a text-only model (`anthropic.ts:777-779`). */
function wireText(message: unknown): string | undefined {
  if (typeof message !== "object" || message === null || !("content" in message)) return undefined;
  const { content } = message;
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return undefined;
  const texts: string[] = [];
  for (const block of content) {
    if (typeof block !== "object" || block === null || !("text" in block)) return undefined;
    if (typeof block.text !== "string") return undefined;
    texts.push(block.text);
  }
  return texts.join("\n");
}

function withRole(message: unknown, role: string): unknown {
  return typeof message === "object" && message !== null ? { ...message, role } : message;
}

export function createApprovalMessage(deps: ApprovalMessageDeps): ApprovalMessage {
  /** The route's last answer: null where no approval stands or no read has answered. */
  let standing: Standing | null = null;
  /** When the last read started, and whether the route answered it. */
  let lastRead: { readonly at: number; readonly answered: boolean } | undefined;
  /** An entry that is not the model's or a tool's entered the session since the last read. */
  let stale = false;
  let reading: Promise<void> | undefined;
  /** Inside a run of failed reads, which logs its first failure and its end alone. */
  let failing = false;
  let pending: Pending | undefined;
  const conversions = new Map<string, Conversion>();
  /** The sessions and models a request carried the message on as a user message, each logged. */
  const userRole = new Set<string>();

  /** The claim of a root architect's own session, never a `task` subagent's event. */
  const rootClaim = (context: SessionContext): ClaimCapability | undefined => {
    if (context.agent?.kind === "sub") return undefined;
    const claim = deps.claim(context.sessionManager.getSessionId());
    return claim?.role === "architect" && claim.issue === claim.tree ? claim : undefined;
  };

  /** One read of the route: its answer, or the last answer kept when it fails. */
  const readOnce = async (claim: ClaimCapability): Promise<void> => {
    const at = deps.now();
    stale = false;
    try {
      const answer = await deps
        .daemon()
        .approval(
          { claimToken: claim.claimToken, sessionId: claim.sessionID, secret: claim.secret },
          { signal: AbortSignal.timeout(APPROVAL_READ_DEADLINE_MS) }
        );
      const approval = answer.approval;
      standing =
        approval === null
          ? null
          : { document: approval.document, version: approval.version, text: approval.message };
      lastRead = { at, answered: true };
      if (failing) {
        failing = false;
        deps.log.info("legion: the approval route answers again");
      }
    } catch (error) {
      lastRead = { at, answered: false };
      if (!failing) {
        failing = true;
        deps.log.warn(
          `legion: the approval route could not be read (${messageFor(error)}); each request is decided on the route's last answer, and the next one reads again`
        );
      }
    }
  };

  /** The read in flight, shared by every request that waits on it, or a new one. */
  const read = (claim: ClaimCapability): Promise<void> => {
    reading ??= readOnce(claim).finally(() => {
      reading = undefined;
    });
    return reading;
  };

  /** Whether the request reads the route first: no read has answered yet, or the last failed, or
   *  an entry that is not the model's or a tool's entered the session since the last read began
   *  (`message_end` and `session_compact` say so; a message the request holds stamped after it
   *  says so too, should its `message_end` reach this plugin late). */
  const readDue = (messages: readonly unknown[]): boolean => {
    const last = lastRead;
    if (last === undefined || !last.answered || stale) return true;
    return messages.some((message) => {
      const role = roleOf(message);
      return (
        role !== "assistant" &&
        role !== "toolResult" &&
        !isApprovalMessage(message) &&
        (timestampOf(message) ?? 0) > last.at
      );
    });
  };

  /**
   * Whether the request is outside the conversation's own loop: a side turn, Oh My Pi's handoff
   * prompt, or a compaction's own. A remote compaction converts the conversation up to its cut,
   * then the whole conversation right after it, and one that keeps no tail converts the same
   * messages twice while the session compacts (`nextConversion`). A main-loop request while a
   * speculative compaction runs is none of these, so it places as any other does. A request with
   * no turn in it (only a compaction summary) is outside the loop too.
   */
  const outsideLoop = (
    messages: readonly unknown[],
    branch: readonly unknown[],
    session: string
  ): boolean => {
    if (
      messages.every(
        (message) => roleOf(message) === "compactionSummary" || isApprovalMessage(message)
      )
    ) {
      return true;
    }
    if (isSideTurn(messages) || isHandoffPrompt(messages)) return true;
    const { conversion, kind } = nextConversion(conversions.get(session), messages, branch);
    conversions.set(session, conversion);
    return (
      kind === "cut" ||
      kind === "afterCut" ||
      (kind === "repeat" && sessionCompacting(deps.registry, session))
    );
  };

  /** Records `waiting` with `answer`, the saved assistant message that answered it. */
  const record = (waiting: Pending, answer: unknown): ApprovalSent => {
    pending = undefined;
    const sent: ApprovalSent = {
      version: waiting.version,
      text: waiting.text,
      timestamp: waiting.timestamp,
      answer: messageKey(answer),
    };
    try {
      deps.appendEntry(APPROVAL_SENT_ENTRY, sent);
    } catch (error) {
      deps.log.warn(
        `legion: the approval message placed in this session could not be recorded (${messageFor(error)}); the next request places it again`
      );
    }
    return sent;
  };

  /** The new placement of `session` whose answer `messages` already hold, recorded: the request
   *  after it can reach this plugin before that answer's `message_end` does. */
  const settle = (messages: readonly unknown[], session: string): ApprovalSent | undefined => {
    const waiting = pending;
    if (waiting === undefined || waiting.session !== session) return undefined;
    const answer = messages.find(
      (message) =>
        roleOf(message) === "assistant" &&
        (timestampOf(message) ?? -1) >= waiting.timestamp &&
        survives(message)
    );
    return answer === undefined ? undefined : record(waiting, answer);
  };

  /** Puts each copy `present` records right before its answer, in the order recorded. `keys` are
   *  the `messageKey`s of `messages`, kept in step with each copy put in. */
  const rebuild = (messages: unknown[], keys: string[], present: readonly ApprovalSent[]): void => {
    for (const sent of present) {
      const at = keys.indexOf(sent.answer);
      if (at < 0) continue;
      const copy = approvalMessage(sent.text, sent.timestamp);
      messages.splice(at, 0, copy);
      keys.splice(at, 0, messageKey(copy));
    }
  };

  /** A new placement at the end of `messages` when the standing version has no copy there. */
  const place = (
    messages: unknown[],
    session: string,
    present: readonly ApprovalSent[]
  ): boolean => {
    const now = standing;
    if (now === null || present.some((sent) => sent.version === now.version)) {
      if (pending?.session === session) pending = undefined;
      return false;
    }
    if (
      pending === undefined ||
      pending.session !== session ||
      pending.version !== now.version ||
      pending.text !== now.text
    ) {
      // Newer than every message of the request, so Oh My Pi, which tells messages apart by role
      // and timestamp, and puts a compaction's file list before the first message newer than the
      // compaction, keeps it last.
      const newest = Math.max(0, ...messages.map((message) => timestampOf(message) ?? 0));
      pending = {
        session,
        version: now.version,
        text: now.text,
        timestamp: Math.max(deps.now(), newest + 1),
      };
      deps.log.info(
        `legion: the approval message of ${now.document} at version ${now.version} is placed last in this session's request`
      );
    }
    messages.push(approvalMessage(pending.text, pending.timestamp));
    return true;
  };

  /** One log line for each session and model a request carries the message on as a user message:
   *  a model whose `compat.supportsMidConversationSystem` is not set. */
  const tellUserRole = (context: SessionContext, session: string): void => {
    const model = context.model;
    if (model === undefined || model.compat?.supportsMidConversationSystem === true) return;
    const name = `${model.provider}/${model.id}`;
    const seen = `${session} ${name}`;
    if (userRole.has(seen)) return;
    userRole.add(seen);
    deps.log.warn(
      `legion: session ${session} runs on ${name}, which takes no system message inside the conversation, so Oh My Pi sends the approval message as a user message`
    );
  };

  return {
    sessionStart: async (context) => {
      const claim = rootClaim(context);
      if (claim !== undefined) await read(claim);
    },

    context: async (messages, context) => {
      const claim = rootClaim(context);
      if (claim === undefined) return false;
      const session = context.sessionManager.getSessionId();
      const before = context.sessionManager.getBranch?.() ?? [];
      const outside = outsideLoop(messages, before, session);
      const reads = !outside && readDue(messages);
      if (reads) await read(claim);
      // The host can move to another session while the read waits: the request is the session's
      // it was built from, decided from that session's branch as it began, with nothing new.
      // Without a read nothing came between, so the branch read first is the branch.
      const moved = reads && context.sessionManager.getSessionId() !== session;
      const branch = reads && !moved ? (context.sessionManager.getBranch?.() ?? []) : before;
      const settled = moved ? undefined : settle(messages, session);
      const records = [
        ...customEntries(branch, APPROVAL_SENT_ENTRY).filter(isApprovalSent),
        ...(settled === undefined ? [] : [settled]),
      ];
      const present: ApprovalSent[] = [];
      if (records.length > 0) {
        const keys = messages.map(messageKey);
        const seen = new Set<string>();
        for (const sent of records) {
          const copy = `${sent.timestamp}\n${sent.answer}`;
          if (!keys.includes(sent.answer) || seen.has(copy)) continue;
          seen.add(copy);
          present.push(sent);
        }
        rebuild(messages, keys, present);
      }
      const placed = !outside && !moved && place(messages, session, present);
      if (present.length > 0 || placed) tellUserRole(context, session);
      return present.length > 0 || placed;
    },

    messageEnd: (message, context) => {
      if (rootClaim(context) === undefined) return;
      const role = roleOf(message);
      if (role !== "assistant") {
        if (role !== "toolResult" && !isApprovalMessage(message)) stale = true;
        return;
      }
      const waiting = pending;
      if (waiting === undefined || waiting.session !== context.sessionManager.getSessionId())
        return;
      if ((timestampOf(message) ?? -1) < waiting.timestamp || !survives(message)) return;
      record(waiting, message);
    },

    sessionCompact: (context) => {
      if (rootClaim(context) !== undefined) stale = true;
    },

    /**
     * Oh My Pi adds its date reminder after every `context` handler, when the date or the working
     * directory changed since the conversation's last request and the newest entry is not a user
     * turn; while only per-call messages follow its anchor, the last message that is not per-call,
     * it goes after them, as a per-call message (`session/date-cwd-reminder.ts:639-677`). On the
     * request with a new placement its conversion then makes the reminder `system` and the
     * placement `user`: the backward walk upgrades the last developer message first
     * (`anthropic.ts:6000-6005`, `:6255-6268`). On the next request the reminder sits right after
     * its anchor. So on the one body that ends with the new placement and a run of reminders, the
     * run goes right after the last message that is not per-call, before every per-call message
     * after it (pi-envoy's answers message among them), as user messages, and the placement stays
     * last, `system` where the model takes one and a user message comes before it. That body's
     * messages are then the next request's before the answer. The `cache_control` marks Oh My Pi
     * set inside `buildParams` stay as they are, and none sits on a per-call message. The same
     * body gives the same result each time the provider rebuilds it.
     */
    beforeProviderRequest: (payload, context) => {
      const waiting = pending;
      if (waiting === undefined || rootClaim(context) === undefined) return undefined;
      if (waiting.session !== context.sessionManager.getSessionId()) return undefined;
      if (
        typeof payload !== "object" ||
        payload === null ||
        !("messages" in payload) ||
        !Array.isArray(payload.messages)
      ) {
        return undefined;
      }
      const messages: readonly unknown[] = payload.messages;
      let reminders = messages.length;
      while (
        reminders > 0 &&
        perCall(messages[reminders - 1]) &&
        DATE_REMINDER.test(wireText(messages[reminders - 1]) ?? "")
      ) {
        reminders -= 1;
      }
      const placement = reminders - 1;
      if (reminders === messages.length || placement < 0) return undefined;
      const placed = messages[placement];
      if (!perCall(placed) || wireText(placed) !== waiting.text) return undefined;
      let anchor = placement - 1;
      while (anchor >= 0 && perCall(messages[anchor])) anchor -= 1;
      if (anchor < 0) return undefined;
      const moved = [
        ...messages.slice(0, anchor + 1),
        ...messages.slice(reminders).map((reminder) => withRole(reminder, "user")),
        ...messages.slice(anchor + 1, placement),
      ];
      const system =
        context.model?.compat?.supportsMidConversationSystem === true &&
        roleOf(moved.at(-1)) === "user";
      moved.push(withRole(placed, system ? "system" : "user"));
      return { ...payload, messages: moved };
    },
  };
}
