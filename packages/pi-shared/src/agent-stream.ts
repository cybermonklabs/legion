import { createHash } from "node:crypto";
import {
  AGENT_STREAM_LIMITS,
  AGENT_STREAM_PROTOCOL,
  AGENT_STREAM_WATCH_TTL_MS,
  type AgentStreamCommand,
  type AgentStreamFrame,
  type AgentStreamPart,
  type AgentStreamReplay,
  type AgentStreamSettings,
  type AgentStreamSettingsReported,
  type AgentStreamSettingsUnreported,
  blockPlaceholder,
  capAgentStreamText,
  isTypableCommandName,
} from "@legion/contracts";
import { readInboundCard } from "@legion/envoy-client/inbound-card";
import type { ContextUsage } from "@legion/pi-shared/pi-types";

/**
 * Turns this Oh My Pi session's own messages into the frames a Dispatch viewer renders
 * (LEGION-232).
 *
 * Two rules shape it. **Nothing leaves the session unwatched:** a frame is published only inside
 * the window a viewer's `watch` or `replay` arms, so a session nobody is looking at puts no
 * conversation on the bus at all. **Nothing is stored anywhere else:** the only history is the
 * bounded ring below, in this process, beside the transcript the session already keeps on disk;
 * the relay and the dashboard hold frames no longer than a viewer's connection. A process that
 * takes up a session whose transcript already holds turns fills the ring from the part of that
 * transcript the session is still in (`restore`), so a session resumed after a restart replays
 * those turns, where it replayed none.
 *
 * The ring holds the message object the host hands over, not a built frame; a message read back
 * from the transcript is a copy (`restore`). A streamed tool call arrives as one `message_update`
 * per delta — tens of thousands for a large write — and building a frame for each would serialise
 * the whole growing argument object every time, which is quadratic in its size and was measured
 * at 1.2 s of main-thread CPU for a 100 KB call in a session nobody was even watching. Frames are
 * built where they are actually needed: at a publish the throttle allows, and at a replay.
 *
 * Ordering never depends on the host's delivery order, because there isn't one to depend on.
 * Oh My Pi 18.3.2 hands `message_update` to extensions through its own queue while
 * `message_start` and `message_end` are emitted directly, so a message's last few updates arrive
 * *after* it has already settled. A settled message is therefore final here: once a key holds
 * one, a later streaming record for it is dropped outright — no ring write, no sequence number —
 * so a finished turn can never be left looking like a running one.
 *
 * Beside the session's own turns it carries each Envoy card the session displays (an
 * `envoy-message` custom message: the brief a session launcher sends, another agent's
 * message, a Dispatch notification), as a user message saying who sent it and what they wrote, so
 * the live view shows the conversation the terminal shows (dispatch://INFRA-5271). A card that
 * delivers a Dispatch message to the session is the one left out: Dispatch stores that message
 * for the session, and the live view draws it from there (`DispatchMessageCardDetails`).
 */

/**
 * A message and its content parts as an Oh My Pi `message_*` handler receives them. Every field
 * a frame reads is `unknown`: the host's own types are not importable here, and a build that
 * renames one must be caught by the checks below rather than by a cast that assumes it.
 */
interface HostMessage {
  readonly role?: unknown;
  readonly content?: unknown;
  readonly timestamp?: unknown;
  readonly toolCallId?: unknown;
  readonly toolName?: unknown;
  readonly isError?: unknown;
  readonly model?: unknown;
  readonly provider?: unknown;
  readonly tag?: unknown;
  readonly customType?: unknown;
  readonly display?: unknown;
  readonly details?: unknown;
}

interface HostPart {
  readonly type?: unknown;
  readonly text?: unknown;
  readonly thinking?: unknown;
  readonly id?: unknown;
  readonly name?: unknown;
  readonly arguments?: unknown;
}

/**
 * One message, with the identity and role a frame needs, once the host's shape checks out. `host`
 * is the object `record` was handed for a live message, kept rather than its content: reading
 * `content` is what costs, and a streaming message's is replaced by the host on every delta, so
 * the reference is always the latest without this copying one. For a message read back from the
 * transcript it is `restore`'s copy (`detached`), which on purpose does not follow the host's
 * object: the host rewrites that object in place, and a prune would reach the ring through it.
 */
interface ReadableMessage {
  readonly role: string;
  readonly timestamp: number;
  readonly toolCallId: string | undefined;
  readonly toolName: string | undefined;
  readonly isError: boolean;
  /** `provider/model` combined (e.g. `anthropic/claude-opus-5`), when the host message named
   *  both as strings; undefined for a user message, a tool result, and an assistant message
   *  whose host object names neither (an adapter with no access to its own model identity). */
  readonly model: string | undefined;
  /** The tag the host kept on a typed input's user message (its Dispatch message id). */
  readonly tag: string | undefined;
  readonly host: HostMessage;
}

/** One message or tool result the session may still replay. */
interface RingEntry {
  readonly key: string;
  readonly at: number;
  readonly seq: number;
  /** False once the host has settled this message; a settled entry is never replaced. */
  readonly streaming: boolean;
  readonly message: ReadableMessage;
  /** The Dispatch message a user message delivered, when the caller says it is one: a person's
   *  direct message the session took as its own user turn. */
  readonly dispatchMessageId: string | undefined;
}

export interface AgentStreamPublisherDeps {
  /** A core NATS publish. Never JetStream: see the subject note in `@legion/contracts`. */
  readonly publish: (subject: string, payload: string) => void;
  readonly now: () => number;
}

/** The custom type of the card the Envoy plugin shows the session each Envoy message in. */
const ENVOY_CARD = "envoy-message";

/**
 * What an `envoy-message` card's `details` carry when the card delivers a Dispatch message to the
 * session (pi-envoy's `handCard` writes it), or answers one (the agent's copy of a person's
 * `!command` report, whose frame the stream already carries tagged with that message): that
 * message's id. Dispatch stores every message it delivers to a session for that session, and the
 * live view draws it from that store (`GET /api/v1/agents/{session_id}/messages`), or from the
 * report's own frame, so such a card is not streamed: the page would show the message twice.
 */
export interface DispatchMessageCardDetails {
  readonly dispatchMessageId: string;
}

/** What a `settings` frame says without its envelope: what the session's host reports of its
 *  model and thinking level, or that it cannot report them. */
export type AgentStreamSettingsState =
  | Omit<AgentStreamSettingsReported, "v" | "kind" | "seq">
  | Omit<AgentStreamSettingsUnreported, "v" | "kind" | "seq">;

/** What a `context` frame says without its envelope: the two figures the host's own
 *  `getContextUsage()` answered with, once `setContext`'s rule has let them through. */
export type AgentStreamContextState = Omit<
  Extract<AgentStreamFrame, { kind: "context" }>,
  "v" | "kind" | "seq"
>;

const encoder = new TextEncoder();

/** What a replay's `frames` array costs beyond its frames: its two brackets. Each frame after the
 *  first adds a comma. */
const REPLAY_ARRAY_BYTES = 2;

/** The UTF-8 bytes `value` serialises to, which is what NATS measures. */
function jsonBytes(value: unknown): number {
  return encoder.encode(JSON.stringify(value)).length;
}

/**
 * Sequence numbers each millisecond of a publisher's clock is worth. A viewer keeps a frame only
 * when its `seq` beats the one it already applied, and a page held open while its session is
 * relaunched (`--resume`) keeps everything the earlier process sent, so a later process must start
 * above any number an earlier one reached: seeding from `now() * SEQ_PER_MS` does that unless the
 * earlier one averaged more than a thousand records a millisecond. `Date.now() * 1000` stays below
 * `Number.MAX_SAFE_INTEGER` until the year 2255.
 */
const SEQ_PER_MS = 1_000;

/** `provider/id` combined (e.g. `anthropic/claude-opus-5`), or undefined when either is missing or
 *  empty — an empty string still satisfies `typeof x === "string"`, and combining one anyway would
 *  publish a malformed `/claude-opus-5` or `anthropic/`. Never capped: a settings frame names the
 *  model a person's pick sends back, so it must be the exact id. */
export function modelReference(provider: unknown, id: unknown): string | undefined {
  if (typeof id !== "string" || id === "") return undefined;
  if (typeof provider !== "string" || provider === "") return undefined;
  return `${provider}/${id}`;
}

/** The model an assistant message names, `provider/model` combined and capped. */
function readModel(message: HostMessage): string | undefined {
  const model = modelReference(message.provider, message.model);
  return model === undefined
    ? undefined
    : capAgentStreamText(model, AGENT_STREAM_LIMITS.modelChars);
}

function readMessage(raw: unknown): ReadableMessage | null {
  if (typeof raw !== "object" || raw === null) return null;
  const message = raw as HostMessage;
  // A message with no host timestamp has no identity that survives its own stream, and the
  // viewer orders the conversation by it. The host stamps every message it keeps.
  if (typeof message.role !== "string" || typeof message.timestamp !== "number") return null;
  return {
    host: message,
    isError: message.isError === true,
    model: readModel(message),
    role: message.role,
    timestamp: message.timestamp,
    toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
    toolName: typeof message.toolName === "string" ? message.toolName : undefined,
    tag: typeof message.tag === "string" ? message.tag : undefined,
  };
}

/** A string field of an object the host handed over, if it is one. */
function stringField(value: unknown, field: string): string | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const found: unknown = Reflect.get(value, field);
  return typeof found === "string" && found !== "" ? found : undefined;
}

/** Whether `host` is an Envoy card this stream carries: every `envoy-message` the session
 *  displays but one that delivers a Dispatch message, which the live view draws from Dispatch's
 *  store (`DispatchMessageCardDetails`). */
function streamedCard(host: HostMessage): boolean {
  if (host.customType !== ENVOY_CARD || host.display !== true) return false;
  const delivered: keyof DispatchMessageCardDetails = "dispatchMessageId";
  return stringField(host.details, delivered) === undefined;
}

/** The ring slot a message occupies, or null for a message this build does not carry. A tagged
 *  user message's slot names its tag too: two Sends can be stamped in the same millisecond. So
 *  does a card's: the wake's id for it, else the event it shows, else a digest of what it says.
 *  A card that names neither (one of the plugin's own notices) has nothing else that holds across
 *  its records: the pinned Oh My Pi hands `message_end` a copy of the message `message_start`
 *  was handed (`cloneMessageNotification`, `extensibility/extensions/lifecycle-mirror.ts`), and a
 *  process that takes up the session reads it from the transcript (`restore`). Two such notices
 *  stamped in the same millisecond say different things. */
function keyFor(message: ReadableMessage): string | null {
  if (message.role === "toolResult") {
    return message.toolCallId === undefined ? null : `t:${message.toolCallId}`;
  }
  if (message.role === "assistant") return `m:a${message.timestamp}`;
  if (message.role === "custom") {
    if (!streamedCard(message.host)) return null;
    const { details } = message.host;
    const card =
      stringField(details, "envoyCard") ??
      stringField(details, "event_id") ??
      createHash("sha256").update(contentText(message.host.content)).digest("hex").slice(0, 16);
    return `m:c${message.timestamp}:${card}`;
  }
  if (message.role !== "user") return null;
  return message.tag === undefined
    ? `m:u${message.timestamp}`
    : `m:u${message.timestamp}:${message.tag}`;
}

/** Flattens a message's content parts to plain text, which is how a tool result reads. */
function contentText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const pieces: string[] = [];
  for (const raw of content) {
    if (typeof raw !== "object" || raw === null) continue;
    const part = raw as HostPart;
    if (typeof part.text === "string") pieces.push(part.text);
    else if (typeof part.type === "string") pieces.push(blockPlaceholder(part.type));
  }
  return pieces.join("\n");
}

/**
 * A copy of a message the host still holds, made as the pinned Oh My Pi copies one for
 * `message_end` (`cloneMessageNotification`, `extensibility/extensions/lifecycle-mirror.ts`):
 * each enumerable field read once and structured-cloned, else copied through JSON, else kept as
 * its text, so nothing of the host's object is shared. The host rewrites its own objects in place
 * (a prune blanks a tool result's content, `blankToolResult` in
 * `packages/agent/src/compaction/pruning.ts`), and a ring entry must keep what its message said
 * when the ring took it.
 */
function detached(raw: unknown): unknown {
  if (typeof raw !== "object" || raw === null) return raw;
  const copy: Record<string, unknown> = {};
  for (const field of Object.keys(raw)) {
    const value: unknown = Reflect.get(raw, field);
    try {
      copy[field] = structuredClone(value);
      continue;
    } catch {
      // A value structuredClone refuses, such as a function in an extension's metadata.
    }
    try {
      const json = JSON.stringify(value);
      if (json !== undefined) {
        copy[field] = JSON.parse(json);
        continue;
      }
    } catch {
      // A cyclic value, which JSON refuses too.
    }
    copy[field] = String(value);
  }
  return copy;
}

/** Caps every string a tool call's arguments carry as they are serialised, so the cost of
 *  rendering a call that streams a large file in is bounded by the cap rather than by the file. */
function boundedStrings(_key: string, value: unknown): unknown {
  return typeof value === "string"
    ? capAgentStreamText(value, AGENT_STREAM_LIMITS.toolChars)
    : value;
}

/** A tool call's arguments as the viewer shows them. They stream in, so the value is often a
 *  partial object and must survive being unserialisable. */
function renderArguments(value: unknown): string {
  if (value === undefined || value === null) return "";
  try {
    return JSON.stringify(value, boundedStrings, 2) ?? "";
  } catch {
    return "[unserialisable arguments]";
  }
}

function conversationParts(content: unknown): AgentStreamPart[] {
  const { partChars, toolChars } = AGENT_STREAM_LIMITS;
  if (typeof content === "string") {
    return content === "" ? [] : [{ text: capAgentStreamText(content, partChars), type: "text" }];
  }
  if (!Array.isArray(content)) return [];
  const parts: AgentStreamPart[] = [];
  for (const raw of content) {
    if (typeof raw !== "object" || raw === null) continue;
    const part = raw as HostPart;
    if (part.type === "text" && typeof part.text === "string") {
      parts.push({ text: capAgentStreamText(part.text, partChars), type: "text" });
    } else if (part.type === "thinking" && typeof part.thinking === "string") {
      parts.push({ text: capAgentStreamText(part.thinking, partChars), type: "reasoning" });
    } else if (part.type === "toolCall" && typeof part.id === "string") {
      parts.push({
        argsText: capAgentStreamText(renderArguments(part.arguments), toolChars),
        toolCallId: part.id,
        toolName: typeof part.name === "string" ? part.name : "tool",
        type: "tool-call",
      });
    } else if (typeof part.type === "string") {
      // An image, or any part kind this build does not know: named, never dropped silently,
      // and never carrying its bytes. The spelling is the shared one, which the Agents page's
      // guard on what a streamed row may say reads too (`PICTURE_PLACEHOLDER`, INFRA-1026).
      parts.push({ text: blockPlaceholder(part.type), type: "text" });
    }
  }
  return parts;
}

/** `text` as a fenced block, its fence longer than any run of backticks inside it, so the viewer
 *  shows it verbatim, as the terminal's card does, and no fence inside it ends the block. */
function fenced(text: string): string {
  let longest = 2;
  for (const run of text.matchAll(/`+/g)) longest = Math.max(longest, run[0].length);
  const fence = "`".repeat(longest + 1);
  return `${fence}\n${text}\n${fence}`;
}

/**
 * What the live view shows of a card's text (`readInboundCard`): who sent it, then the words they
 * wrote, which the view renders as it renders any message a person or an agent wrote, then each
 * note the card adds about them (`note`, `body_note`), as the terminal's card shows them. A card
 * with no such words - a Dispatch event, a structured message, an empty one, one of the plugin's
 * own notices - is shown whole and verbatim instead, as the terminal shows it: the view renders
 * Markdown, which reads the card's indented TOON as code blocks and joins its lines.
 */
function cardText(text: string): string {
  const card = readInboundCard(text);
  const head = card?.from === undefined ? "Envoy message:" : `Envoy message from ${card.from}:`;
  const words = card === undefined || card.dispatch ? undefined : card.words?.text;
  if (words === undefined) return `${head}\n\n${fenced(text)}`;
  const notes = [
    ...(card?.note === undefined ? [] : [`note: ${card.note}`]),
    ...(card?.bodyNote === undefined ? [] : [`body_note: ${card.bodyNote}`]),
  ];
  return [head, words, ...notes].join("\n\n");
}

/** A card's parts: its text (`cardText`), then a placeholder for each picture it carries. */
function cardParts(content: unknown): AgentStreamPart[] {
  const { partChars } = AGENT_STREAM_LIMITS;
  const blocks: unknown[] =
    typeof content === "string"
      ? [{ text: content, type: "text" }]
      : Array.isArray(content)
        ? content
        : [];
  const parts: AgentStreamPart[] = [];
  let read = false;
  for (const raw of blocks) {
    if (typeof raw !== "object" || raw === null) continue;
    const part = raw as HostPart;
    if (part.type === "text" && typeof part.text === "string") {
      // The card is its first text block: the extension sends a card's text, then its pictures.
      const text = read ? part.text : cardText(part.text);
      read = true;
      parts.push({ text: capAgentStreamText(text, partChars), type: "text" });
    } else if (typeof part.type === "string") {
      parts.push({ text: blockPlaceholder(part.type), type: "text" });
    }
  }
  return parts;
}

/** The frame a ring entry becomes, which is where a frame reads a message's content. Two other
 *  places read it: `keyFor`, for the text of a card that names no id, and `restore`, which copies
 *  each message its bound kept (`detached`). */
function frameFor(entry: RingEntry): AgentStreamFrame {
  const { dispatchMessageId, message } = entry;
  if (message.role === "toolResult" && message.toolCallId !== undefined) {
    return {
      kind: "tool-result",
      result: {
        at: message.timestamp,
        isError: message.isError,
        output: capAgentStreamText(
          contentText(message.host.content),
          AGENT_STREAM_LIMITS.toolChars
        ),
        toolCallId: message.toolCallId,
        toolName: message.toolName ?? "tool",
      },
      seq: entry.seq,
      v: AGENT_STREAM_PROTOCOL,
    };
  }
  if (message.role === "custom") {
    // A card is no person's turn, so it names no Dispatch message, which the live view would
    // read as one (`dispatchTurns`).
    return {
      kind: "message",
      message: {
        at: message.timestamp,
        id: entry.key.slice(2),
        parts: cardParts(message.host.content),
        role: "user",
        streaming: false,
      },
      seq: entry.seq,
      v: AGENT_STREAM_PROTOCOL,
    };
  }
  return {
    kind: "message",
    message: {
      at: message.timestamp,
      id: entry.key.slice(2),
      parts: conversationParts(message.host.content),
      role: message.role === "assistant" ? "assistant" : "user",
      streaming: entry.streaming,
      ...(dispatchMessageId === undefined ? {} : { dispatchMessageId }),
      // Only an assistant turn ever carries one: the host's own UserMessage has no model or
      // provider field to read, but a forged frame on the bus could still claim one, and the
      // contract promises "absent on a user message".
      ...(message.role === "assistant" && message.model !== undefined
        ? { model: message.model }
        : {}),
    },
    seq: entry.seq,
    v: AGENT_STREAM_PROTOCOL,
  };
}

export class AgentStreamPublisher {
  readonly #deps: AgentStreamPublisherDeps;
  /** Newest state per message or tool result, in the order each was first seen. */
  readonly #history = new Map<string, RingEntry>();
  /** Keys the host has settled. A settled message is final; see the class note. */
  readonly #settled = new Set<string>();
  /** When each message was last put on the wire, so a token storm costs one frame per
   *  `snapshotIntervalMs` rather than one per delta. */
  readonly #publishedAt = new Map<string, number>();
  /** The last sequence number handed out. Seeded from the clock, so it is monotonic across the
   *  session's processes as well as within one: see `SEQ_PER_MS`. `reset` never touches it. */
  #seq: number;
  #watchedUntil = 0;
  /** The slash commands the session takes from Dispatch, capped and each name once, before the
   *  byte fit; undefined for a session whose host cannot run typed input, which publishes none. */
  #listedCommands: readonly AgentStreamCommand[] | undefined;
  /** The host-order prefix of `#listedCommands` that fits the replay, as last published. */
  #commands: readonly AgentStreamCommand[] | undefined;
  #commandsSeq = 0;
  /** What the last fit was computed from, so a frame that changes neither the list nor the room
   *  left for it does not walk every command again (`#fitCommands`). A running turn calls
   *  `setContext` on every interval, and the answer there is the same list every time. */
  #fitListed: readonly AgentStreamCommand[] | undefined;
  #fitLimit = 0;
  #fitEmptyBytes = 0;
  /** The session's model and thinking level, as last published; undefined until first read. */
  #settings: AgentStreamSettingsState | undefined;
  #settingsSeq = 0;
  /** `#settings` as JSON, which a new state is compared against. */
  #settingsJson: string | undefined;
  /** How much of its context window the session fills, as last kept: the reading `setContext`
   *  last let through, `"cleared"` once the host has answered no window, and undefined until the
   *  host first answers either (and again after `reset`). */
  #context: AgentStreamContextState | "cleared" | undefined;
  #contextSeq = 0;
  /** The reading `#context` holds as JSON, which a new reading is compared against; undefined
   *  while it holds none. */
  #contextJson: string | undefined;

  constructor(deps: AgentStreamPublisherDeps) {
    this.#deps = deps;
    this.#seq = Math.trunc(deps.now()) * SEQ_PER_MS;
  }

  /** A viewer asked for this session: publish for the next `AGENT_STREAM_WATCH_TTL_MS`. */
  noteViewer(): void {
    this.#watchedUntil = this.#deps.now() + AGENT_STREAM_WATCH_TTL_MS;
  }

  /**
   * Publishes the settings, the context usage and the command list as they stand, while a viewer
   * is attached. The caller sends them to a viewer that has just armed a publisher nobody was
   * watching: what this process read before then was kept without being published, and a viewer
   * does not always read it from a replay. A page held open while its session is relaunched with
   * `--resume` gets no replay from the new process — its relay already had an answer and only
   * re-arms it — so without this its header keeps the earlier process's values and its ring stays
   * blank until the session's next turn. A viewer that also replays is sent the same frames twice
   * under the same `seq`, and keeps one.
   */
  publishState(subject: string): void {
    if (!this.watched) return;
    if (this.#settings !== undefined) {
      this.#deps.publish(subject, JSON.stringify(this.#settingsFrame(this.#settings)));
    }
    if (this.#context !== undefined) {
      this.#deps.publish(subject, JSON.stringify(this.#contextFrame(this.#context)));
    }
    if (this.#commands !== undefined) {
      this.#deps.publish(subject, JSON.stringify(this.#commandsFrame(this.#commands)));
    }
  }

  get watched(): boolean {
    return this.#deps.now() < this.#watchedUntil;
  }

  /**
   * Forget everything but the sequence counter: the session under this publisher has been
   * replaced. `/new`, `/resume`, a fork and a tree navigation all move the session id while the
   * extension instance lives on, and a viewer opening the new id must not be served the previous
   * conversation. The counter carries on, so a session resumed back into this process still
   * numbers its frames above what a page held open on it already applied.
   */
  reset(): void {
    this.#history.clear();
    this.#settled.clear();
    this.#publishedAt.clear();
    this.#watchedUntil = 0;
    this.#listedCommands = undefined;
    this.#commands = undefined;
    this.#settings = undefined;
    this.#settingsJson = undefined;
    this.#context = undefined;
    this.#contextJson = undefined;
  }

  /**
   * The session's slash commands: kept for every replay, whose last frame they are, and published
   * at once when they differ from the list last kept while a viewer is attached. Each name is
   * capped and kept once, a name a person could not type dropped (`isTypableCommandName`). The
   * list keeps the host-order prefix whose frame fits what the settings frame leaves of the
   * replay's byte budget (`#fitCommands`): the caps count characters, and a character JSON escapes
   * (`\u0001`) costs six bytes.
   */
  setCommands(subject: string, commands: readonly AgentStreamCommand[]): void {
    const seen = new Set<string>();
    const listed: AgentStreamCommand[] = [];
    for (const command of commands) {
      if (listed.length >= AGENT_STREAM_LIMITS.commands) break;
      const name = command.name;
      if (name.length > AGENT_STREAM_LIMITS.commandNameChars) continue;
      if (!isTypableCommandName(name) || seen.has(name)) continue;
      seen.add(name);
      listed.push({
        name,
        source: command.source,
        ...(command.description !== undefined && {
          description: capAgentStreamText(
            command.description,
            AGENT_STREAM_LIMITS.commandDescriptionChars
          ),
        }),
        ...(command.terminalOnly === true && { terminalOnly: true as const }),
      });
    }
    this.#listedCommands = listed;
    this.#fitCommands(subject);
  }

  /**
   * The session's model and thinking level: kept for every replay, whose frame before the commands
   * it is, and published at once when they differ from what was last kept while a viewer is
   * attached. Never cut: a replay pays for this frame first, whole, and the commands then keep the
   * prefix that fits in what is left, so a change here can change the commands frame too.
   */
  setSettings(subject: string, settings: AgentStreamSettingsState): void {
    const kept: AgentStreamSettingsState = settings.reported
      ? {
          ...(settings.model !== undefined && { model: settings.model }),
          models: [...settings.models],
          reported: true,
          thinking:
            settings.thinking === null
              ? null
              : { level: settings.thinking.level, levels: [...settings.thinking.levels] },
        }
      : { reported: false };
    const json = JSON.stringify(kept);
    if (json === this.#settingsJson) return;
    this.#settings = kept;
    this.#settingsJson = json;
    this.#seq += 1;
    this.#settingsSeq = this.#seq;
    if (this.watched) this.#deps.publish(subject, JSON.stringify(this.#settingsFrame(kept)));
    this.#fitCommands(subject);
  }

  /**
   * How much of its context window the session fills now, as the host's own `getContextUsage()`
   * answered: kept for every replay, whose frame between the settings and the commands it is, and
   * published at once when it differs from what was last kept while a viewer is attached.
   *
   * A reading is kept and published only when the host answers a window above zero and a usable
   * figure: either a `percent` the host computed or tokens of its own. An answer of no usage, or
   * of a window of zero or below, clears instead: the publisher sends `context-cleared` and keeps
   * it in every replay in the reading's place, so a viewer takes its ring away. It does so even
   * before any reading: a page held open while the session is relaunched with `--resume` holds
   * the earlier process's reading, and only a clearing frame with a newer `seq` takes that ring
   * away. The `{tokens: 0, contextWindow, percent: null}` answer reports a window and no figure,
   * so it leaves the slot as it is, neither replacing nor clearing it.
   *
   * The pinned host (Oh My Pi 18.18.6-sami.20261126-110039, `.omp-pin`) answers undefined only
   * from a runner not yet initialised, never after a good reading, so clearing on it cannot make
   * the ring flicker. A window of zero comes from a model with no window, or from no model, and
   * lasts until the model changes. The `percent: null` guard stays for an older pinned build,
   * 18.16.0-sami.20261105-032528, whose status line's startup wiring answered that shape: sent,
   * it would draw a ring at nought per cent over a conversation well underway.
   */
  setContext(subject: string, usage: ContextUsage | undefined): void {
    if (usage === undefined || !(usage.contextWindow > 0)) {
      if (this.#context === "cleared") return;
      this.#context = "cleared";
      // Forgotten, so a reading equal to the one before the clear is news again.
      this.#contextJson = undefined;
    } else {
      if (typeof usage.percent !== "number" && !(usage.tokens > 0)) return;
      const kept: AgentStreamContextState = {
        contextWindow: usage.contextWindow,
        tokens: usage.tokens,
      };
      const json = JSON.stringify(kept);
      if (json === this.#contextJson) return;
      this.#context = kept;
      this.#contextJson = json;
    }
    this.#seq += 1;
    this.#contextSeq = this.#seq;
    if (this.watched) {
      this.#deps.publish(subject, JSON.stringify(this.#contextFrame(this.#context)));
    }
    this.#fitCommands(subject);
  }

  /** Keeps the host-order prefix of the listed commands whose frame fits the replay's byte budget
   *  after the settings frame and the context slot's frame (a reading or the clearing frame), and
   *  publishes it when it differs from the list last kept. */
  #fitCommands(subject: string): void {
    const listed = this.#listedCommands;
    if (listed === undefined) return;
    // Measured as `replay` pays for it: the whole settings and context frames and the comma after
    // each, then this frame under the highest sequence number it can carry, then each command and
    // the comma before it.
    const settings =
      this.#settings === undefined ? 0 : jsonBytes(this.#settingsFrame(this.#settings)) + 1;
    const context =
      this.#context === undefined ? 0 : jsonBytes(this.#contextFrame(this.#context)) + 1;
    const limit = AGENT_STREAM_LIMITS.historyBytes - REPLAY_ARRAY_BYTES - settings - context;
    const emptyBytes = jsonBytes(this.#commandsFrame([], this.#seq + 1));
    // The prefix that fits is a function of these three alone, so when none of them moved the
    // answer is the list already kept and there is nothing to walk, serialise or publish.
    const unchanged =
      listed === this.#fitListed && limit === this.#fitLimit && emptyBytes === this.#fitEmptyBytes;
    if (unchanged) return;
    this.#fitListed = listed;
    this.#fitLimit = limit;
    this.#fitEmptyBytes = emptyBytes;
    let bytes = emptyBytes;
    const kept: AgentStreamCommand[] = [];
    for (const command of listed) {
      const cost = jsonBytes(command) + (kept.length === 0 ? 0 : 1);
      if (bytes + cost > limit) break;
      bytes += cost;
      kept.push(command);
    }
    if (JSON.stringify(kept) === JSON.stringify(this.#commands)) return;
    this.#commands = kept;
    this.#seq += 1;
    this.#commandsSeq = this.#seq;
    if (this.watched) this.#deps.publish(subject, JSON.stringify(this.#commandsFrame(kept)));
  }

  #commandsFrame(
    commands: readonly AgentStreamCommand[],
    seq = this.#commandsSeq
  ): AgentStreamFrame {
    return { commands, kind: "commands", seq, v: AGENT_STREAM_PROTOCOL };
  }

  #settingsFrame(settings: AgentStreamSettingsState): AgentStreamSettings {
    return { ...settings, kind: "settings", seq: this.#settingsSeq, v: AGENT_STREAM_PROTOCOL };
  }

  /** The frame the context slot rides in: the reading it keeps, or the clearing frame in its
   *  place. */
  #contextFrame(context: AgentStreamContextState | "cleared"): AgentStreamFrame {
    if (context === "cleared") {
      return { kind: "context-cleared", seq: this.#contextSeq, v: AGENT_STREAM_PROTOCOL };
    }
    return { ...context, kind: "context", seq: this.#contextSeq, v: AGENT_STREAM_PROTOCOL };
  }

  /**
   * Records one of the session's messages and, while a viewer is attached, publishes it.
   * `streaming` is false for a settled message (`message_end`, and every user, tool-result or card
   * message) and true for an assistant message still being produced. `dispatchMessageId` is the
   * Dispatch message a user message delivered, which the caller alone can know; a later record of
   * the same message keeps it. Returns whether the message was recorded: false for one this build
   * does not carry, and for a late streaming update of a message the host has already settled.
   */
  record(subject: string, raw: unknown, streaming: boolean, dispatchMessageId?: string): boolean {
    const entry = this.#keep(raw, streaming, dispatchMessageId);
    if (entry === undefined) return false;
    if (!this.watched) return true;
    const now = this.#deps.now();
    const published = this.#publishedAt.get(entry.key);
    const tooSoon =
      published !== undefined && now - published < AGENT_STREAM_LIMITS.snapshotIntervalMs;
    if (entry.streaming && tooSoon) return true;
    this.#publishedAt.set(entry.key, now);
    this.#deps.publish(subject, JSON.stringify(frameFor(entry)));
    return true;
  }

  /**
   * Fills the ring with turns this process did not run: the messages the caller reads back from
   * the session's transcript when the process takes the session up (a pod's restart and `omp
   * --continue`, a `--resume`, a `/resume`), oldest first, each as the host handed it to
   * `message_end` when it ran. The caller decides which part of the transcript that is; only what
   * it passes is read. Each is kept as `record` keeps a settled message, into the ring's own
   * bounds, so a replay after a restart holds those turns as the process that ran them recorded
   * them. A message whose slot the ring already holds stands: a restore after a turn of this
   * process adds the turns before it and replaces none of them.
   *
   * Which messages survive is decided as the ring itself decides it: the slots are walked in the
   * order the messages were recorded, a message of a slot the ring holds keeps that slot's place,
   * and past the bound the slot first taken goes. The slots the ring already held are this
   * process's own, each recorded after the ring was made, so they are the session's newest: they
   * are taken out while the older ones are filled in and put back after them, and the bound then
   * drops the oldest filled slot rather than a turn this process ran. Choosing them reads only the
   * slot each message takes: no body, except the text of a card that names no id of its own,
   * which is what its slot is (`keyFor`). Each message the bound kept is then copied (`detached`),
   * as the host copies a message for `message_end`: the host rewrites its own object in place (a
   * prune blanks a tool result's content), and the ring keeps what the message said when the
   * session was taken up, as it keeps what a live message said at its end. So the fill reads the
   * body of each message its bound kept, and of the others only the text of a card that names no
   * id. Nothing is published, since a viewer reads these from its replay. `dispatchMessageIdOf`
   * names the Dispatch message a user message delivered, which only the caller can know
   * (`record`'s `dispatchMessageId`).
   */
  restore(
    messages: readonly unknown[],
    dispatchMessageIdOf: (message: unknown) => string | undefined = () => undefined
  ): void {
    // The newest message of each slot the ring would still hold, by the slot's place in it.
    const slots = new Map<string, number>();
    for (const [index, raw] of messages.entries()) {
      const message = readMessage(raw);
      const key = message === null ? null : keyFor(message);
      if (key === null) continue;
      slots.set(key, index);
      if (slots.size <= AGENT_STREAM_LIMITS.historyMessages) continue;
      const oldest = slots.keys().next();
      if (oldest.done !== true) slots.delete(oldest.value);
    }
    const held = new Map(this.#history);
    this.#history.clear();
    for (const [key, index] of slots) {
      if (held.has(key)) continue;
      const raw = messages[index];
      this.#keep(detached(raw), false, dispatchMessageIdOf(raw));
    }
    for (const [key, entry] of held) this.#history.set(key, entry);
    this.#evict();
  }

  /** Puts one message into the ring, dropping the oldest past the bound, and returns its entry;
   *  undefined for a message this build does not carry, and for a late streaming update of a
   *  message the host has already settled (see the class note). */
  #keep(
    raw: unknown,
    streaming: boolean,
    dispatchMessageId: string | undefined
  ): RingEntry | undefined {
    const message = readMessage(raw);
    if (message === null) return undefined;
    const key = keyFor(message);
    if (key === null) return undefined;
    const stillStreaming = message.role === "assistant" && streaming;
    // The host's updates outlive its own `message_end`, so a settled message is final.
    if (stillStreaming && this.#settled.has(key)) return undefined;
    this.#seq += 1;
    const entry: RingEntry = {
      at: message.timestamp,
      dispatchMessageId: dispatchMessageId ?? this.#history.get(key)?.dispatchMessageId,
      key,
      message,
      seq: this.#seq,
      streaming: stillStreaming,
    };
    this.#history.set(key, entry);
    if (!stillStreaming) this.#settled.add(key);
    this.#evict();
    return entry;
  }

  /** Drops the slot first taken, with what is held about it, until the ring is within its
   *  bound. */
  #evict(): void {
    while (this.#history.size > AGENT_STREAM_LIMITS.historyMessages) {
      const oldest = this.#history.keys().next();
      if (oldest.done === true) break;
      this.#history.delete(oldest.value);
      this.#settled.delete(oldest.value);
      this.#publishedAt.delete(oldest.value);
    }
  }

  /**
   * What this session can still replay, oldest first, inside the byte budget.
   *
   * Answered only while a viewer has already armed the session. The ring itself fills whether
   * or not anyone is watching — it never leaves this process, and it is what lets the first
   * viewer to open a session see the turn it is already in — but handing it out is publishing,
   * so it obeys the same rule as a frame: a session nobody has opened answers nothing. The
   * relay arms the session before it asks (`Watch` then `Replay` on one connection, in that
   * order), so a genuine viewer is never refused.
   *
   * The budget is UTF-8 bytes, which is what NATS measures. Counting UTF-16 code units would let
   * a history of box-drawing tool output or CJK text build a reply three times its measured size,
   * over the server's 1 MiB `max_payload`, and the publish would throw. It bounds what `frames`
   * serialises to: every frame, the commas between them and the array's brackets.
   */
  replay(sessionID: string): AgentStreamReplay {
    if (!this.watched) return { frames: [], session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
    const ordered = [...this.#history.values()].sort((left, right) =>
      left.at === right.at ? left.seq - right.seq : left.at - right.at
    );
    // The settings, the context slot (a reading or the clearing frame) and then the command list
    // ride last, in that order, and are paid for first, so a full history never crowds them out;
    // `#fitCommands` keeps the three inside the budget.
    const tail: AgentStreamFrame[] = [];
    if (this.#settings !== undefined) tail.push(this.#settingsFrame(this.#settings));
    if (this.#context !== undefined) tail.push(this.#contextFrame(this.#context));
    if (this.#commands !== undefined) tail.push(this.#commandsFrame(this.#commands));
    let used =
      REPLAY_ARRAY_BYTES +
      tail.reduce((bytes, frame, index) => bytes + jsonBytes(frame) + (index === 0 ? 0 : 1), 0);
    const kept: AgentStreamFrame[] = [];
    for (let index = ordered.length - 1; index >= 0; index -= 1) {
      const entry = ordered[index];
      if (entry === undefined) continue;
      const frame = frameFor(entry);
      const separated = kept.length > 0 || tail.length > 0;
      const cost = jsonBytes(frame) + (separated ? 1 : 0);
      if (used + cost > AGENT_STREAM_LIMITS.historyBytes) break;
      used += cost;
      kept.unshift(frame);
    }
    kept.push(...tail);
    return { frames: kept, session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
  }
}
