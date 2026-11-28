import { inboundTimestamp } from "@legion/envoy-client/delivery";
import { customEntries } from "@legion/pi-shared/custom-entry";
import { encode } from "@toon-format/toon";
import { REPLAY_DELIVERED_ENTRY, writtenCardIdentity } from "./missed-messages";

/**
 * How long a card may take from its send to this session before its card states the wait in
 * words, and how far this machine's clock may run from the sending server's before the card says
 * so. A card read that late carries an `at:` stamp that far behind the present, and a reader that
 * takes the stamp for the present time draws wrong conclusions from it, about the clocks of the
 * machines that sent it among others; a clock that far off makes a wait out of nothing, or hides
 * one, unless the card says how the clocks stand.
 */
export const MATERIAL_WAIT_MS = 15 * 60_000;

/** The most cards whose delivery time `CardAges` keeps; past it the oldest is forgotten first. */
export const CARD_AGES_LIMIT = 20_000;

/**
 * The custom type of the session entry (`pi.appendEntry`) that records the delivery a session first
 * gave a card (`CardDelivered`), which every later request of the session reads back, in this
 * process once `CardAges` no longer holds it and in a process that resumed the session, so the card
 * reads the same in each. Oh My Pi builds no context message from a `custom` entry. A change to the
 * data's shape takes a new custom type: a reader passes over an entry it cannot read.
 */
export const CARD_DELIVERED_ENTRY = "envoy-card-delivered";

/**
 * The delivery a session first gave a card, as the session records it (`CARD_DELIVERED_ENTRY`'s
 * data): `event_id`, the event the card shows; `at`, the time it reached the session by this
 * machine's clock, in epoch milliseconds; and `clock_offset`, once one was measured, how many
 * milliseconds that clock ran ahead of the Envoy listener's (negative: behind).
 */
export interface CardDelivered {
  readonly event_id: string;
  readonly at: number;
  readonly clock_offset?: number;
}

/** What `CardAges` asks of the extension around it. */
export interface CardAgesDeps {
  readonly now: () => number;
  /**
   * How many milliseconds this machine's clock runs ahead of the Envoy listener's (negative:
   * behind), from the listener's latest reply (`ServerClock`, `./server-clock`); undefined before
   * any reply carried its time, or with nothing to measure.
   */
  readonly clockOffset?: () => number | undefined;
  /**
   * Writes `entry` into the file of session `sessionID` (`CARD_DELIVERED_ENTRY`), called while
   * `written` or `annotate` runs: when the session first fixes a card's delivery, and again with
   * the same time when a delivery fixed with no clock difference first takes one. Never called for
   * a delivery read from the session's own entries as it was recorded. Absent, nothing is recorded,
   * and a process that resumes the session takes each card's time from its ledger record or its
   * place in the session.
   */
  readonly record?: (sessionID: string, entry: CardDelivered) => void;
}

/** When a card reached a session, by this machine's clock, and how that clock stood then. */
interface Delivery {
  readonly at: number;
  /**
   * `CardAgesDeps.clockOffset` when the time was fixed, or as the session recorded it; undefined
   * until one was measured.
   */
  readonly clockOffset: number | undefined;
}

/** A card in a request's messages: its event id, and the text that holds its `envoy:` block. */
interface CardText {
  readonly eventID: string;
  readonly text: string;
  /** The message with that text replaced: a copy, since the host keeps the original. */
  readonly withText: (text: string) => unknown;
}

/**
 * The time each Envoy card reached this session, and each card's text in a model request with it.
 *
 * A card's `envoy:` block carries `at:`, the time its sender sent it, or, on a replay failure
 * notice, `published_at:`. The host writes the card when it takes it into the session, which can
 * be much later: on a session whose inbox ran behind, hours later. So every request the session
 * makes shows each card with a second time, `delivered:`, the time the host wrote it, and, when
 * the two are `MATERIAL_WAIT_MS` or more apart, `waited:`, the gap in words. The card the host
 * persists is unchanged: these lines exist only in the requests (`annotate`, from the extension's
 * `context` handler).
 *
 * The two times come from two clocks, and only two: `at:` is set by the server that accepted the
 * message (the Envoy listener the sender sent through, or Dispatch's server for a Dispatch event),
 * never by the agent that sent it, and `delivered:` is this machine's. Each card fixes how far this
 * machine's clock ran from the listener's (`CardAgesDeps.clockOffset`) when its delivery time is
 * first used. When that is `MATERIAL_WAIT_MS` or more either way, the card says which way and by
 * how much (`clocks:`), and `waited:` is the gap less that difference, so a clock that runs ahead
 * makes no wait and one that runs behind hides none. A `delivered:` still that many minutes or more
 * before `at:`, less any difference measured, says which clock that leaves ahead: this machine's
 * behind the sending server's by at least that, or, with a difference measured, the sending
 * server's ahead of the listener's (Dispatch's, or a listener on the sender's own machine, whose
 * clocks are not measured). A smaller difference is not shown and stays in the stated wait.
 *
 * The delivery a session first gives a card is recorded in the session (`CARD_DELIVERED_ENTRY`,
 * through `CardAgesDeps.record`) and read back from it, so a card's text is the same in every
 * request of the session, in this process and in one that resumed it. The card's write
 * (`written`) and a request that shows a card this process holds no delivery for (`annotate`)
 * read it in one order (`#fromSession`): the delivery the session records for the card's event;
 * else the time its replay ledger record was written (`REPLAY_DELIVERED_ENTRY`, its `at`), when
 * the session's entries hold one; else, when they hold an entry of the card's event (written, its
 * record lost to a process that ended in between), the newest time of a `message` or
 * `custom_message` entry up to the first one; else, at the card's write, the time of that write,
 * and at a request, the time of that request when the host handed the card to this process
 * (`timestamp`) after this instance started. A card the host writes again, as a process that
 * resumed the session replays a card whose ledger record the process before it never wrote, so
 * takes the delivery of the older entry of its event, never the time of its own write, and both
 * copies read the same; a card written for the first time takes its own entry's estimate when the
 * host wrote that entry before the write's handler ran: a time no later than the write, and
 * seconds before it when the card waited for the process to start. Any of those times but the
 * session's own record is fixed when first used and recorded then. A card known by none of them
 * keeps its text as it is.
 *
 * Each of those deliveries is remembered and recorded under the session it belongs to as well as
 * the event, because one process serves a session at a time and moves between sessions: a session
 * that followed the same topic is shown the same event again, and reads it when its own host writes
 * it, hours after the session before it did. A switched session therefore takes its own record,
 * its own write, its own ledger record or its own branch position, never the time another session
 * was shown the event; a session returned to takes what it was already told.
 *
 * A request's bytes up to the newest message repeat on the next request, so the provider's prompt
 * cache can read them again: each card's delivery is fixed the first time the session uses it and
 * reused after, from memory for the `CARD_AGES_LIMIT` newest session-and-event pairs, and from the
 * session's record, which has no limit, for any other and after a resume. The clock difference is
 * fixed with the time, except that a card whose time was fixed before any listener reply carried
 * the listener's time takes the first one measured after, which changes its text once; the session
 * records it again then, with the same time, so a process that resumes the session reads the same
 * difference. A card the session holds no readable record for (written by an older release, or one
 * whose record `CardAgesDeps.record` did not write) takes, after a resume, its ledger record's time
 * or its branch position, also when the resumed process writes it again before its first request.
 * That can differ from the time the process before showed by seconds: when the ledger stamped the
 * card late, and when the card has no ledger record and the host stamped its entry with the time
 * it was handed the card, seconds before the earlier release's `message_end` when the card arrived
 * while that process was still starting.
 */
export class CardAges {
  readonly #deps: CardAgesDeps;
  readonly #startedAt: number;
  readonly #delivered = new Map<string, Delivery>();

  constructor(deps: CardAgesDeps) {
    this.#deps = deps;
    this.#startedAt = deps.now();
  }

  /**
   * The host wrote `message` in session `sessionID`. When this process holds no delivery for that
   * session and the card's event, the write fixes one in the order every caller reads
   * (`#fromSession`), whose last source here is the time of this write. So a card the host writes
   * again, as a process that resumed the session replays a card whose ledger record the process
   * before it never wrote, takes the delivery of the older entry of its event, never the time of
   * its own write. A card written for the first time takes its own entry's estimate when the host
   * wrote that entry before this handler ran (a time no later than the write, and seconds before it
   * when the card waited for the process to start), else the time of the write. `branch` is that
   * session's entries, read only when this process holds no delivery for the pair.
   */
  written(message: unknown, sessionID: string, branch: () => readonly unknown[]): void {
    const eventID = writtenCardIdentity(message)?.event_id;
    if (eventID === undefined) return;
    const key = deliveryKey(sessionID, eventID);
    if (this.#delivered.has(key)) return;
    this.#fromSession(key, sessionID, eventID, sessionReads(branch), () => this.#deps.now());
  }

  /**
   * `messages`, one model request's in session `sessionID`, with each card's delivery time and,
   * when material, its wait; undefined when no card changed. A card this process holds no
   * delivery for takes one in the order every caller reads (`#fromSession`), whose last source
   * here is the time of this request when the host handed the card to this process after this
   * instance started. `branch` is that session's entries, read at most once per request and only
   * when a card's delivery in it is not known yet.
   */
  annotate(
    messages: readonly unknown[],
    branch: () => readonly unknown[],
    sessionID: string
  ): unknown[] | undefined {
    const reads = sessionReads(branch);
    let changed = false;
    const annotated = messages.map((message) => {
      const card = cardText(message);
      if (card === undefined) return message;
      const key = deliveryKey(sessionID, card.eventID);
      let delivered =
        this.#delivered.get(key) ??
        this.#fromSession(key, sessionID, card.eventID, reads, () => this.#handedHere(message));
      if (delivered === undefined) return message;
      if (delivered.clockOffset === undefined) {
        const clockOffset = this.#deps.clockOffset?.();
        if (clockOffset !== undefined) {
          delivered = this.#remember(key, { at: delivered.at, clockOffset });
          this.#record(sessionID, card.eventID, delivered);
        }
      }
      const text = withDelivery(card.text, delivered.at, delivered.clockOffset);
      if (text === undefined) return message;
      changed = true;
      return card.withText(text);
    });
    return changed ? annotated : undefined;
  }

  /**
   * The delivery session `sessionID` gives the card naming `eventID` (held under `key`) when this
   * process holds none, in the one order `written` and `annotate` both read: the delivery the
   * session records for the event (`recordedDeliveries`), taken as it is and not recorded again;
   * else the time the session's entries give the event (`branchTimes`: its ledger record, else
   * the newest message time up to its first entry); else `otherwise()`, the caller's last source.
   * A time read from the entries or from `otherwise` is fixed and recorded (`#fix`). Undefined
   * when no source gives one.
   */
  #fromSession(
    key: string,
    sessionID: string,
    eventID: string,
    reads: SessionReads,
    otherwise: () => number | undefined
  ): Delivery | undefined {
    const recorded = reads.recorded().get(eventID);
    if (recorded !== undefined) return this.#remember(key, recorded);
    const at = reads.times().get(eventID) ?? otherwise();
    return at === undefined ? undefined : this.#fix(key, sessionID, eventID, at);
  }

  /** Now, for a card the host was handed after this instance started; undefined otherwise. */
  #handedHere(message: unknown): number | undefined {
    const handed =
      typeof message === "object" && message !== null && "timestamp" in message
        ? message.timestamp
        : undefined;
    return typeof handed === "number" && handed >= this.#startedAt ? this.#deps.now() : undefined;
  }

  /**
   * Fixes `at`, with the clock difference measured now, as the delivery session `sessionID` gives
   * the card naming `eventID` (held under `key`), and records it in the session.
   */
  #fix(key: string, sessionID: string, eventID: string, at: number): Delivery {
    const delivery = this.#remember(key, { at, clockOffset: this.#deps.clockOffset?.() });
    this.#record(sessionID, eventID, delivery);
    return delivery;
  }

  #record(sessionID: string, eventID: string, delivery: Delivery): void {
    this.#deps.record?.(sessionID, {
      event_id: eventID,
      at: delivery.at,
      ...(delivery.clockOffset === undefined ? {} : { clock_offset: delivery.clockOffset }),
    });
  }

  #remember(key: string, delivery: Delivery): Delivery {
    this.#delivered.set(key, delivery);
    if (this.#delivered.size > CARD_AGES_LIMIT) {
      const oldest = this.#delivered.keys().next();
      if (oldest.done !== true) this.#delivered.delete(oldest.value);
    }
    return delivery;
  }
}

/**
 * What a remembered delivery time is held under: the session that was shown the card and the event
 * it showed. A session id holds no newline, so no pair can read as another.
 */
function deliveryKey(sessionID: string, eventID: string): string {
  return `${sessionID}\n${eventID}`;
}

/**
 * One call's reads of a session's entries (`CardAges.#fromSession`'s sources): the deliveries the
 * session records and the times its entries give, each computed at most once and only when asked,
 * from one read of the branch, so a request that shows many cards scans the branch once.
 */
interface SessionReads {
  readonly recorded: () => Map<string, Delivery>;
  readonly times: () => Map<string, number>;
}

function sessionReads(branch: () => readonly unknown[]): SessionReads {
  let entries: readonly unknown[] | undefined;
  let recorded: Map<string, Delivery> | undefined;
  let times: Map<string, number> | undefined;
  return {
    recorded: () => {
      entries ??= branch();
      recorded ??= recordedDeliveries(entries);
      return recorded;
    },
    times: () => {
      entries ??= branch();
      times ??= branchTimes(entries);
      return times;
    },
  };
}

/**
 * The delivery each card's event has in the session's entries (`CARD_DELIVERED_ENTRY`), by event
 * id: the newest record of it, which carries the clock difference once one was measured. Data that
 * does not read as a recorded delivery (`isCardDelivered`) is passed over.
 */
function recordedDeliveries(entries: readonly unknown[]): Map<string, Delivery> {
  const recorded = new Map<string, Delivery>();
  for (const data of customEntries(entries, CARD_DELIVERED_ENTRY)) {
    if (!isCardDelivered(data)) continue;
    recorded.set(data.event_id, { at: data.at, clockOffset: data.clock_offset });
  }
  return recorded;
}

/**
 * Whether `data` reads as a recorded delivery (`CardDelivered`): `event_id` a string that is not
 * empty, `at` a finite number, and `clock_offset`, when set, a finite number.
 */
function isCardDelivered(data: unknown): data is CardDelivered {
  if (typeof data !== "object" || data === null) return false;
  if (!("event_id" in data) || typeof data.event_id !== "string" || data.event_id === "") {
    return false;
  }
  if (!("at" in data) || typeof data.at !== "number" || !Number.isFinite(data.at)) return false;
  return (
    !("clock_offset" in data) ||
    data.clock_offset === undefined ||
    (typeof data.clock_offset === "number" && Number.isFinite(data.clock_offset))
  );
}

/**
 * The time each card was written, from the session's entries, for a card the session records no
 * delivery for (`recordedDeliveries`): its ledger record's (`ReplayLedgerEntry.at`), else, for a
 * card whose entry is there with no record (the process ended between the host's write and the
 * record's), the newest time of a `message` or `custom_message` entry up to and including the
 * card's own. Oh My Pi writes a card's entry through its message_end persistence queue, stamped
 * with the time it was handed the card, while it appends a `custom` entry such as
 * `date-cwd-reminder` synchronously as it prepares a request, after the `context` hooks have shown
 * the card, so such an entry can stand before the card's entry with a later stamp, and no entry of
 * another type moves the time. `message` and `custom_message` entries go through the same queue,
 * in message_end order, so the newest of them up to the card's own is no later than the time the
 * session first showed the card.
 */
function branchTimes(entries: readonly unknown[]): Map<string, number> {
  const ledger = new Map<string, number>();
  const positions = new Map<string, number>();
  let newest = Number.NEGATIVE_INFINITY;
  for (const entry of entries) {
    if (typeof entry !== "object" || entry === null || !("type" in entry)) continue;
    if (entry.type === "message" || entry.type === "custom_message") {
      const stamp = Date.parse(
        "timestamp" in entry && typeof entry.timestamp === "string" ? entry.timestamp : ""
      );
      if (!Number.isNaN(stamp)) newest = Math.max(newest, stamp);
    }
    if (entry.type === "custom_message") {
      const eventID = writtenCardIdentity(entry)?.event_id;
      if (eventID !== undefined && !positions.has(eventID) && Number.isFinite(newest)) {
        positions.set(eventID, newest);
      }
      continue;
    }
    if (entry.type !== "custom") continue;
    if (!("customType" in entry) || entry.customType !== REPLAY_DELIVERED_ENTRY) continue;
    const data: unknown = "data" in entry ? entry.data : undefined;
    if (typeof data !== "object" || data === null) continue;
    const eventID = "event_id" in data ? data.event_id : undefined;
    const at = "at" in data ? data.at : undefined;
    if (typeof eventID !== "string" || typeof at !== "number" || ledger.has(eventID)) continue;
    ledger.set(eventID, at);
  }
  for (const [eventID, at] of positions) if (!ledger.has(eventID)) ledger.set(eventID, at);
  return ledger;
}

/** The card `message` is, when it is an `envoy-message` card naming an event with an `envoy:` block. */
function cardText(message: unknown): CardText | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (!("role" in message) || message.role !== "custom") return undefined;
  const eventID = writtenCardIdentity(message)?.event_id;
  if (eventID === undefined) return undefined;
  const content: unknown = "content" in message ? message.content : undefined;
  if (typeof content === "string") {
    if (!content.startsWith("envoy:\n")) return undefined;
    return { eventID, text: content, withText: (text) => ({ ...message, content: text }) };
  }
  if (!Array.isArray(content)) return undefined;
  for (const [block, part] of content.entries()) {
    if (typeof part !== "object" || part === null) continue;
    const text = "text" in part ? part.text : undefined;
    if (typeof text !== "string" || !text.startsWith("envoy:\n")) continue;
    return {
      eventID,
      text,
      withText: (replaced) => ({
        ...message,
        content: content.map((other, index) =>
          index === block ? { ...part, text: replaced } : other
        ),
      }),
    };
  }
  return undefined;
}

/**
 * The line an `envoy:` block dates a card's send by, two spaces in, as the renderers encode it:
 * `at:` on the card a frame is rendered as (`renderInbound`), `published_at:` on the notice shown
 * in place of a frame the session could not be shown (`failureNoticeFields`).
 */
const SENT_LINE = /^ {2}(at|published_at): (.+)$/;

/** The `delivered:` line this module adds, which another copy of the extension may have added. */
const DELIVERED_LINE = /^ {2}delivered: /;

/**
 * A card's text with `delivered:`, and `waited:` and `clocks:` when material, after the line that
 * dates its send; undefined for a text with no such line, or one that already says when it was
 * delivered. `clockOffset` is how far this machine's clock ran ahead of the Envoy listener's
 * (negative: behind) when the card's time was first used, when known.
 */
export function withDelivery(
  text: string,
  delivered: number,
  clockOffset?: number
): string | undefined {
  const lines = text.split("\n");
  const sentIndex = lines.findIndex((line) => SENT_LINE.test(line));
  if (sentIndex < 0 || lines.some((line) => DELIVERED_LINE.test(line))) return undefined;
  const matched = SENT_LINE.exec(lines[sentIndex] ?? "");
  const field = matched?.[1] ?? "at";
  const sent = Date.parse(toonString(matched?.[2] ?? ""));
  const fields: Record<string, string> = { delivered: inboundTimestamp(delivered) };
  if (!Number.isNaN(sent)) {
    // `delivered - sent` crosses two clocks: this machine's and the sending server's.
    const gap = delivered - sent;
    const skew =
      clockOffset !== undefined && Math.abs(clockOffset) >= MATERIAL_WAIT_MS ? clockOffset : 0;
    const waitedMs = gap - skew;
    const whose = `delivered is this machine's clock and ${field} the sending server's`;
    if (waitedMs >= MATERIAL_WAIT_MS) {
      const less = skew === 0 ? "" : ", less the difference in clocks";
      fields.waited = `${duration(waitedMs)} between ${field} and delivered${less}; ${whose}, and ${field} is when it was sent, not the present time`;
    }
    const said: string[] = [];
    if (skew !== 0) {
      const way = skew > 0 ? "ahead of" : "behind";
      said.push(
        `this machine's clock runs ${duration(Math.abs(skew))} ${way} the Envoy listener's, read from the listener's replies`
      );
    }
    // A card delivered before it was sent, by this machine's clock less any difference measured.
    if (waitedMs <= -MATERIAL_WAIT_MS) {
      const early = duration(-waitedMs);
      said.push(
        skew === 0
          ? `delivered is ${early} before ${field}, so this machine's clock runs at least ${early} behind the sending server's`
          : `delivered is still ${early} before ${field} less that difference, so the sending server's clock runs at least ${early} ahead of the Envoy listener's`
      );
    }
    if (said.length > 0) fields.clocks = `${said.join("; ")}; ${whose}`;
  }
  const added = encode(fields)
    .split("\n")
    .map((line) => `  ${line}`);
  return [...lines.slice(0, sentIndex + 1), ...added, ...lines.slice(sentIndex + 1)].join("\n");
}

/** A TOON value as a string: a quoted one is JSON. */
function toonString(raw: string): string {
  if (!raw.startsWith('"')) return raw;
  try {
    const value: unknown = JSON.parse(raw);
    return typeof value === "string" ? value : raw;
  } catch {
    return raw;
  }
}

/** A wait in whole minutes: `52 min`, or `4 h 45 min`. */
export function duration(ms: number): string {
  const minutes = Math.floor(ms / 60_000);
  const hours = Math.floor(minutes / 60);
  return hours === 0 ? `${minutes} min` : `${hours} h ${minutes % 60} min`;
}
