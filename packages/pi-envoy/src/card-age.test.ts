import { describe, expect, test } from "bun:test";
import { renderInbound } from "@legion/envoy-client/delivery";
import { envoyNoticeText, failureNoticeFields } from "@legion/envoy-client/delivery-ledger";
import { CardAges, duration, MATERIAL_WAIT_MS } from "./card-age";
import { REPLAY_DELIVERED_ENTRY } from "./missed-messages";

const SESSION = "ses_reader";
const SENT = Date.parse("2026-10-31T06:27:17Z");
const LATE = Date.parse("2026-10-31T11:13:02Z");

/** A card as the extension hands it: `renderInbound`'s text of a message sent at `sentAt`. */
function card(eventID: string, sentAt: number, handedAt: number): Record<string, unknown> {
  const rendered = renderInbound(
    JSON.stringify({
      event_id: eventID,
      source: "agent",
      topic: `notifications.agent.${SESSION}`,
      source_session: "ses_sender",
      issued_at: sentAt,
      payload_summary: "Its log reports 06:27:17Z from date -u.",
      urgency: "high",
    }),
    SESSION
  );
  return {
    role: "custom",
    customType: "envoy-message",
    content: rendered.content,
    display: true,
    details: { event_id: eventID },
    timestamp: handedAt,
  };
}

/**
 * The notice the extension shows in place of a frame the host refused, as `showReplayCard` hands
 * it: an inert one carries the frame's identity alone, an ordinary one the wake's card marker too.
 */
function failureNotice(eventID: string, options: { btw?: boolean }): Record<string, unknown> {
  return {
    role: "custom",
    customType: "envoy-message",
    content: envoyNoticeText(
      failureNoticeFields({
        eventId: eventID,
        sender: "ses_sender",
        summary: "unread card",
        publishedAt: new Date(SENT).toISOString(),
        error: "host refused card",
        readWith: "envoy_inbox",
        envelope: { event_id: eventID, source: "agent" },
      })
    ),
    display: true,
    details: {
      event_id: eventID,
      ...(options.btw === true ? {} : { envoyCard: "c0ffee00-0000-4000-8000-000000000000" }),
    },
    timestamp: SENT,
  };
}

/** The text of the card among `messages` that names `eventID`: its content, or its first block's. */
function textOf(messages: readonly unknown[] | undefined, eventID: string): string {
  for (const message of messages ?? []) {
    if (typeof message !== "object" || message === null) continue;
    if (!("details" in message) || !("content" in message)) continue;
    const { details, content } = message;
    if (typeof details !== "object" || details === null || !("event_id" in details)) continue;
    if (details.event_id !== eventID) continue;
    if (typeof content === "string") return content;
    const [first]: unknown[] = Array.isArray(content) ? content : [];
    if (typeof first === "object" && first !== null && "text" in first) return String(first.text);
  }
  return "";
}

/** The content blocks of `message`, when it has them. */
function blocksOf(message: unknown): readonly unknown[] {
  if (typeof message !== "object" || message === null || !("content" in message)) return [];
  return Array.isArray(message.content) ? message.content : [];
}

/**
 * The custom type a session file holds a card's recorded delivery under, written out as the file
 * holds it: a release that renamed it would no longer read the deliveries sessions already hold.
 */
const CARD_DELIVERED = "envoy-card-delivered";

/**
 * Session files as a test keeps them: each session's entries by its id, and the `record` a
 * `CardAges` is given, which appends a delivery to the session it names as `pi.appendEntry` does,
 * a `custom` entry holding its data.
 */
function sessionFiles() {
  const files = new Map<string, Record<string, unknown>[]>();
  const entries = (sessionID: string): Record<string, unknown>[] => {
    const file = files.get(sessionID) ?? [];
    files.set(sessionID, file);
    return file;
  };
  return {
    entries,
    record: (sessionID: string, data: unknown): void => {
      entries(sessionID).push({ type: "custom", customType: CARD_DELIVERED, data });
    },
    /** The data of every delivery recorded in session `sessionID`, in the order written. */
    recorded: (sessionID: string): unknown[] =>
      entries(sessionID).flatMap((entry) =>
        entry.customType === CARD_DELIVERED ? [entry.data] : []
      ),
  };
}

/** The entry the host writes for `message`, a card, as it takes the card into the session at `at`. */
function cardEntry(message: Record<string, unknown>, at: number): Record<string, unknown> {
  return {
    type: "custom_message",
    customType: message.customType,
    timestamp: new Date(at).toISOString(),
    content: message.content,
    display: true,
    details: message.details,
  };
}

/** The replay ledger's record of the card naming `eventID`, stamped `at` by the ledger's clock. */
function ledgerRecord(eventID: string, at: number): Record<string, unknown> {
  return { type: "custom", customType: REPLAY_DELIVERED_ENTRY, data: { event_id: eventID, at } };
}

describe("a card in a model request", () => {
  // The case that made this: a session hours behind on its inbox read a card stamped 06:27:17Z
  // at 11:13Z, took the stamp for the present time, and told other sessions that the machines'
  // clocks ran 4 h 45 min slow.
  test("written 4 h 45 min after it was sent, says when it was delivered and how long it waited", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late, SESSION, () => []);

    const text = textOf(
      ages.annotate([late], () => [], SESSION),
      "evt-late"
    );

    expect(text).toContain('\n  at: "2026-10-31T06:27:17Z"\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(text).toContain(
      "\n  waited: \"4 h 45 min between at and delivered; delivered is this machine's clock and at the sending server's, and at is when it was sent, not the present time\"\n"
    );
    // With no clock difference measured, the card claims none.
    expect(text).not.toContain("clocks:");
  });

  test("written at once says when it was delivered, and nothing about a wait", () => {
    const ages = new CardAges({ now: () => SENT + 2_000 });
    const fresh = card("evt-fresh", SENT, SENT + 1_000);
    ages.written(fresh, SESSION, () => []);

    const text = textOf(
      ages.annotate([fresh], () => [], SESSION),
      "evt-fresh"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T06:27:19Z"\n');
    expect(text).not.toContain("waited:");
  });

  test("says nothing of a wait under the threshold, and states one at it", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const under = card("evt-under", SENT, SENT);
    const at = card("evt-at", SENT, SENT);
    clock = SENT + MATERIAL_WAIT_MS - 1_000;
    ages.written(under, SESSION, () => []);
    clock = SENT + MATERIAL_WAIT_MS;
    ages.written(at, SESSION, () => []);

    const annotated = ages.annotate([under, at], () => [], SESSION);

    expect(textOf(annotated, "evt-under")).not.toContain("waited:");
    expect(textOf(annotated, "evt-at")).toContain('waited: "15 min between at and delivered;');
  });

  // A request's bytes repeat on the next request, so the provider's cache reads them again.
  test("reads the same in every request, however much later the request is made", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late, SESSION, () => []);
    const first = ages.annotate([late], () => [], SESSION);
    clock = LATE + 3 * 3_600_000;
    const second = ages.annotate([late], () => [], SESSION);

    expect(JSON.stringify(second)).toBe(JSON.stringify(first));
  });

  test("whose message_end came after its first request takes that request's time, and keeps it", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const handed = card("evt-handed", SENT, SENT + 1_000);
    clock = LATE;
    const first = textOf(
      ages.annotate([handed], () => [], SESSION),
      "evt-handed"
    );
    clock = LATE + 60_000;
    ages.written(handed, SESSION, () => []);

    expect(first).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(
      textOf(
        ages.annotate([handed], () => [], SESSION),
        "evt-handed"
      )
    ).toBe(first);
  });

  test("written before this process started takes the time of its ledger record", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE + 86_400_000 });
    const branch = [
      {
        type: "custom",
        customType: REPLAY_DELIVERED_ENTRY,
        data: { event_id: "evt-old", at: LATE },
      },
    ];

    const text = textOf(
      ages.annotate([old], () => branch, SESSION),
      "evt-old"
    );

    expect(text).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(text).toContain('waited: "4 h 45 min');
  });

  // The process ended after the host wrote the card and before its ledger record was appended: the
  // card's entry carries the time it was handed, so the message entries written before it bound
  // the write.
  test("written before this process started, its ledger record lost, takes the newest message time up to its own entry", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE + 86_400_000 });
    const branch = [
      { type: "message", timestamp: "2026-10-31T11:12:58.000Z", message: { role: "assistant" } },
      {
        type: "custom_message",
        customType: "envoy-message",
        timestamp: new Date(SENT).toISOString(),
        details: { event_id: "evt-old" },
        content: old.content,
      },
    ];

    const text = textOf(
      ages.annotate([old], () => branch, SESSION),
      "evt-old"
    );

    expect(text).toContain('delivered: "2026-10-31T11:12:58Z"');
    expect(text).toContain('waited: "4 h 45 min');
  });

  // Oh My Pi appends a `custom` entry such as `date-cwd-reminder` as it prepares a request, after
  // the `context` hooks showed the card, and writes the card's own entry later through its
  // message_end queue, stamped with the time it was handed the card: the `custom` entry stands
  // before the card's entry with a later stamp.
  test("written before this process started, its ledger record lost, takes no time from a custom entry the host appended before the card's entry", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE + 86_400_000 });
    const branch = [
      {
        type: "custom",
        customType: "date-cwd-reminder",
        timestamp: "2026-10-31T11:13:03.256Z",
        data: {},
      },
      {
        type: "custom_message",
        customType: "envoy-message",
        timestamp: "2026-10-31T11:13:02.910Z",
        details: { event_id: "evt-old" },
        content: old.content,
      },
      {
        type: "custom",
        customType: "system-prompt-digest",
        timestamp: "2026-10-31T11:13:03.357Z",
        data: {},
      },
    ];

    const text = textOf(
      ages.annotate([old], () => branch, SESSION),
      "evt-old"
    );

    expect(text).toContain('delivered: "2026-10-31T11:13:02Z"');
  });

  test("handed before this process started and never written into the session is left as it is", () => {
    const old = card("evt-old", SENT, SENT);
    const ages = new CardAges({ now: () => LATE });

    expect(ages.annotate([old], () => [], SESSION)).toBeUndefined();
  });

  // Two copies of the extension can run in one process; the first copy's lines stand.
  test("that another copy already annotated is left as it is", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const late = card("evt-late", SENT, SENT);
    clock = LATE;
    ages.written(late, SESSION, () => []);
    const annotated = ages.annotate([late], () => [], SESSION);
    const other = new CardAges({ now: () => LATE + 60_000 });
    other.written(late, SESSION, () => []);

    expect(other.annotate(annotated ?? [], () => [], SESSION)).toBeUndefined();
  });

  test("with pictures is annotated in its text block, and its pictures kept", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const base = card("evt-pictures", SENT, SENT);
    const image = { type: "image", data: "AAAA", mimeType: "image/png" };
    const withPictures = { ...base, content: [{ type: "text", text: base.content }, image] };
    clock = LATE;
    ages.written(withPictures, SESSION, () => []);

    const annotated = ages.annotate([withPictures], () => [], SESSION);
    const blocks = blocksOf(annotated?.[0]);

    expect(textOf(annotated, "evt-pictures")).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(blocks[1]).toEqual(image);
    expect(withPictures.content[0]).toEqual({ type: "text", text: base.content });
  });

  test("leaves every other message alone, and changes nothing when no card is there", () => {
    const ages = new CardAges({ now: () => LATE });
    const user = {
      role: "user",
      content: [{ type: "text", text: "at: 06:27:17Z" }],
      timestamp: LATE,
    };
    const notice = {
      role: "custom",
      customType: "envoy-message",
      content: "envoy:\n  notice: session id changed",
      display: true,
      details: { local: true },
      timestamp: LATE,
    };

    expect(ages.annotate([user, notice], () => [], SESSION)).toBeUndefined();
  });

  // One process serves one session at a time and moves between them. A session that followed the
  // same issue is shown the event again, and reads it when its own host writes it.
  test("replayed in a session switched to later takes that session's own delivery time", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const first = card("evt-shared", SENT, SENT);
    ages.written(first, SESSION, () => []);
    const early = textOf(
      ages.annotate([first], () => [], SESSION),
      "evt-shared"
    );

    // The host resumes a session that followed the same topic, hours later, and writes the card
    // there: its ledger record in that session's entries dates the write.
    clock = LATE;
    const second = card("evt-shared", SENT, SENT);
    ages.written(second, "ses_switched", () => []);
    const branch = [
      {
        type: "custom",
        customType: REPLAY_DELIVERED_ENTRY,
        data: { event_id: "evt-shared", at: LATE },
      },
    ];
    const late = textOf(
      ages.annotate([second], () => branch, "ses_switched"),
      "evt-shared"
    );

    expect(early).toContain('delivered: "2026-10-31T06:27:17Z"');
    expect(early).not.toContain("waited:");
    expect(late).toContain('delivered: "2026-10-31T11:13:02Z"');
    expect(late).toContain('waited: "4 h 45 min');
    // Back on the first session, its own request reads exactly as it did before.
    expect(
      textOf(
        ages.annotate([first], () => branch, SESSION),
        "evt-shared"
      )
    ).toBe(early);
  });

  // A session the host refused a card is shown a notice naming it instead, worded by the replay
  // engine: it dates the send `published_at:`, and reaches the session just as late.
  test("that is a replay failure notice says when the notice was delivered, ordinary or inert", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const ordinary = failureNotice("evt-notice", {});
    const inert = failureNotice("evt-notice-inert", { btw: true });
    clock = LATE;
    ages.written(ordinary, SESSION, () => []);
    ages.written(inert, SESSION, () => []);

    const annotated = ages.annotate([ordinary, inert], () => [], SESSION);

    for (const eventID of ["evt-notice", "evt-notice-inert"]) {
      const text = textOf(annotated, eventID);
      expect(text).toContain(
        '\n  published_at: "2026-10-31T06:27:17.000Z"\n  delivered: "2026-10-31T11:13:02Z"\n'
      );
      expect(text).toContain(
        "\n  waited: \"4 h 45 min between published_at and delivered; delivered is this machine's clock and published_at the sending server's, and published_at is when it was sent, not the present time\"\n"
      );
    }
  });
});

// A request's bytes up to its newest message repeat on the next request, and the provider's prompt
// cache reads them again only while they do, so an old card must read the same in the first request
// of a process that resumed the session as in the requests before it.
describe("a card after a resume", () => {
  // INFRA-4227 S12, in real Oh My Pi: the host wrote a card and was restarted 0.4 s later, before
  // the card's ledger record reached the session file. The process that resumed the session
  // replayed the card, the same event, before its first request, and the old card's `delivered:`
  // line moved two seconds on, so the provider wrote 76,870 of 116,991 prompt tokens again.
  test("after a resume that replays an old card before the first request, the old card keeps its delivered line, and a new card carries its own time", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    let clock = LATE;
    const first = new CardAges({ now: () => clock, record: files.record });
    const old = card("evt-old", SENT, LATE);
    file.push(cardEntry(old, LATE));
    first.written(old, SESSION, () => file);
    const before = textOf(
      first.annotate([old], () => file, SESSION),
      "evt-old"
    );

    // The process ends before the card's ledger record is written. The one that resumes the
    // session minutes later replays the card, and a new card arrives: the host writes both before
    // the first request, and the ledger records the replay by its own clock.
    clock = LATE + 5 * 60_000;
    const resumed = new CardAges({ now: () => clock, record: files.record });
    const replayed = card("evt-old", SENT, clock);
    file.push(cardEntry(replayed, clock));
    resumed.written(replayed, SESSION, () => file);
    file.push(ledgerRecord("evt-old", clock + 1_000));
    const fresh = card("evt-new", clock - 1_000, clock);
    file.push(cardEntry(fresh, clock));
    resumed.written(fresh, SESSION, () => file);
    const after = resumed.annotate([old, replayed, fresh], () => file, SESSION);

    expect(before).toContain('\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(textOf(after, "evt-old")).toBe(before);
    expect(textOf(after?.slice(1), "evt-old")).toBe(before);
    expect(textOf(after, "evt-new")).toContain('\n  delivered: "2026-10-31T11:18:02Z"\n');
    // The replay recorded nothing: the session holds the one delivery it first gave the card.
    expect(files.recorded(SESSION)).toEqual([
      { event_id: "evt-old", at: LATE },
      { event_id: "evt-new", at: LATE + 5 * 60_000 },
    ]);
  });

  // The ledger stamps its record by its own clock, after the host's write. A process that resumed
  // the session took that later time for every card it had not seen written.
  test("after a resume, an old card whose ledger record is stamped after its write keeps its delivered line, and a new card that arrives before the first request carries its own time", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    let clock = LATE;
    const first = new CardAges({ now: () => clock, record: files.record });
    const old = card("evt-old", SENT, LATE);
    file.push(cardEntry(old, LATE));
    first.written(old, SESSION, () => file);
    file.push(ledgerRecord("evt-old", LATE + 2_000));
    const before = textOf(
      first.annotate([old], () => file, SESSION),
      "evt-old"
    );

    clock = LATE + 5 * 60_000;
    const resumed = new CardAges({ now: () => clock, record: files.record });
    const fresh = card("evt-new", clock - 1_000, clock);
    file.push(cardEntry(fresh, clock));
    resumed.written(fresh, SESSION, () => file);
    const after = resumed.annotate([old, fresh], () => file, SESSION);

    expect(before).toContain('\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(textOf(after, "evt-old")).toBe(before);
    expect(textOf(after, "evt-new")).toContain('\n  delivered: "2026-10-31T11:18:02Z"\n');
  });

  // A session an earlier release wrote records no delivery for its cards, nor does one whose
  // record was not written. A process that resumed such a session and wrote an old card again
  // before its first request took the time of that write for the old card, and its `delivered:`
  // line moved on by the hours between the two processes.
  test("after a resume of a session that records no delivery, an old card the resumed process writes again before its first request keeps its delivered line, and a new card carries its own time", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    let clock = LATE;
    // An earlier release, which records nothing. Oh My Pi writes a card's entry into the session
    // file a few milliseconds after the extension's `message_end` handler returns.
    const first = new CardAges({ now: () => clock });
    const old = card("evt-old", SENT, LATE);
    first.written(old, SESSION, () => file);
    file.push(cardEntry(old, LATE + 400));
    const before = textOf(
      first.annotate([old], () => file, SESSION),
      "evt-old"
    );

    // The process that resumes the session three hours later writes the card again, its ledger
    // record never written, and then a new card, both before its first request.
    clock = LATE + 3 * 3_600_000;
    const resumed = new CardAges({ now: () => clock, record: files.record });
    const replayed = card("evt-old", SENT, clock);
    resumed.written(replayed, SESSION, () => file);
    file.push(cardEntry(replayed, clock + 400));
    clock += 2_000;
    const fresh = card("evt-new", clock - 1_000, clock);
    resumed.written(fresh, SESSION, () => file);
    file.push(cardEntry(fresh, clock + 400));
    const after = resumed.annotate([old, replayed, fresh], () => file, SESSION);

    expect(before).toContain('\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(textOf(after, "evt-old")).toBe(before);
    expect(textOf(after?.slice(1), "evt-old")).toBe(before);
    expect(textOf(after, "evt-new")).toContain('\n  delivered: "2026-10-31T14:13:04Z"\n');
    // The old card's delivery is the time of its entry, which the resumed process fixed and
    // recorded, and the new card's the time the resumed process wrote it.
    expect(files.recorded(SESSION)).toEqual([
      { event_id: "evt-old", at: LATE + 400 },
      { event_id: "evt-new", at: LATE + 3 * 3_600_000 + 2_000 },
    ]);
  });
});

// The delivery a session first gives a card goes into the session file (`CARD_DELIVERED_ENTRY`),
// which every later request reads back once the process no longer holds it.
describe("a card's delivery the session records", () => {
  const AHEAD = 4 * 3_600_000 + 45 * 60_000;

  test("is used over the card's ledger record, and is not recorded again", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    const old = card("evt-old", SENT, SENT);
    file.push(
      { type: "custom", customType: CARD_DELIVERED, data: { event_id: "evt-old", at: LATE } },
      ledgerRecord("evt-old", LATE + 60_000)
    );
    const ages = new CardAges({ now: () => LATE + 86_400_000, record: files.record });

    const text = textOf(
      ages.annotate([old], () => file, SESSION),
      "evt-old"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(files.recorded(SESSION)).toEqual([{ event_id: "evt-old", at: LATE }]);
  });

  test("that does not read as a delivery is passed over, and the ledger record's time is fixed and recorded", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    const old = card("evt-old", SENT, SENT);
    const unreadable: unknown[] = [
      { event_id: "", at: LATE - 60_000 },
      { event_id: 7, at: LATE - 60_000 },
      { at: LATE - 60_000 },
      { event_id: "evt-old", at: "2026-10-31T11:12:02Z" },
      { event_id: "evt-old", at: Number.NaN },
      { event_id: "evt-old", at: Number.POSITIVE_INFINITY },
      { event_id: "evt-old", at: LATE - 60_000, clock_offset: "4 h 45 min" },
      { event_id: "evt-old", at: LATE - 60_000, clock_offset: Number.NaN },
      { event_id: "evt-old", at: LATE - 60_000, clock_offset: null },
      null,
      "evt-old",
    ];
    for (const data of unreadable) file.push({ type: "custom", customType: CARD_DELIVERED, data });
    file.push(ledgerRecord("evt-old", LATE));
    const ages = new CardAges({ now: () => LATE + 86_400_000, record: files.record });

    const text = textOf(
      ages.annotate([old], () => file, SESSION),
      "evt-old"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T11:13:02Z"\n');
    expect(files.recorded(SESSION).slice(unreadable.length)).toEqual([
      { event_id: "evt-old", at: LATE },
    ]);
  });

  test("fixes and records the time the card's place in the session gives, or the time of the request it was handed for", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    const old = card("evt-old", SENT, SENT);
    file.push(
      { type: "message", timestamp: "2026-10-31T11:12:58.000Z", message: { role: "assistant" } },
      cardEntry(old, SENT)
    );
    let clock = LATE;
    const ages = new CardAges({ now: () => clock, record: files.record });
    const handed = card("evt-handed", SENT, LATE + 1_000);
    clock = LATE + 2_000;

    ages.annotate([old, handed], () => file, SESSION);

    expect(files.recorded(SESSION)).toEqual([
      { event_id: "evt-old", at: Date.parse("2026-10-31T11:12:58Z") },
      { event_id: "evt-handed", at: LATE + 2_000 },
    ]);
  });

  test("fixed with no clock difference is recorded again, with the same time, when one is first measured, and a later process reads the same text", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    let clock = SENT;
    let offset: number | undefined;
    const ages = new CardAges({
      now: () => clock,
      clockOffset: () => offset,
      record: files.record,
    });
    const prompt = card("evt-measured-later", SENT, SENT);
    clock = SENT + 1_000 + AHEAD;
    file.push(cardEntry(prompt, clock));
    ages.written(prompt, SESSION, () => file);
    const unmeasured = textOf(
      ages.annotate([prompt], () => file, SESSION),
      "evt-measured-later"
    );
    offset = AHEAD;
    const measured = textOf(
      ages.annotate([prompt], () => file, SESSION),
      "evt-measured-later"
    );
    offset = 0;
    ages.annotate([prompt], () => file, SESSION);
    // A process that takes the session over an hour later, measuring no difference at all.
    const later = new CardAges({
      now: () => clock + 3_600_000,
      clockOffset: () => 0,
      record: files.record,
    });
    const again = textOf(
      later.annotate([prompt], () => file, SESSION),
      "evt-measured-later"
    );

    expect(unmeasured).toContain('waited: "4 h 45 min between at and delivered; ');
    expect(measured).toContain("clocks: \"this machine's clock runs 4 h 45 min ahead of");
    expect(measured).not.toContain("waited:");
    expect(again).toBe(measured);
    expect(files.recorded(SESSION)).toEqual([
      { event_id: "evt-measured-later", at: SENT + 1_000 + AHEAD },
      { event_id: "evt-measured-later", at: SENT + 1_000 + AHEAD, clock_offset: AHEAD },
    ]);
  });

  test("read with no clock difference takes the first one measured, and records it with the same time", () => {
    const files = sessionFiles();
    const file = files.entries(SESSION);
    const prompt = card("evt-measured-later", SENT, SENT);
    const at = SENT + 1_000 + AHEAD;
    file.push({
      type: "custom",
      customType: CARD_DELIVERED,
      data: { event_id: "evt-measured-later", at },
    });
    const ages = new CardAges({
      now: () => at + 3_600_000,
      clockOffset: () => AHEAD,
      record: files.record,
    });

    const text = textOf(
      ages.annotate([prompt], () => file, SESSION),
      "evt-measured-later"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T11:12:18Z"\n');
    expect(text).toContain("clocks: \"this machine's clock runs 4 h 45 min ahead of");
    expect(files.recorded(SESSION)).toEqual([
      { event_id: "evt-measured-later", at },
      { event_id: "evt-measured-later", at, clock_offset: AHEAD },
    ]);
  });

  // One process serves one session at a time and moves between them, and each session's file
  // holds only the delivery that session was given.
  test("is recorded under each session for the delivery that session was given", () => {
    const files = sessionFiles();
    let clock = SENT;
    const ages = new CardAges({ now: () => clock, record: files.record });
    const first = card("evt-shared", SENT, SENT);
    ages.written(first, SESSION, () => files.entries(SESSION));
    // The host moves to a session that followed the same topic and writes the event there later.
    clock = LATE;
    const second = card("evt-shared", SENT, LATE);
    ages.written(second, "ses_switched", () => files.entries("ses_switched"));
    // A third session had the card written by a process before this one: its ledger record dates it.
    files.entries("ses_third").push(ledgerRecord("evt-shared", LATE + 60_000));
    ages.annotate([first], () => files.entries("ses_third"), "ses_third");

    expect(files.recorded(SESSION)).toEqual([{ event_id: "evt-shared", at: SENT }]);
    expect(files.recorded("ses_switched")).toEqual([{ event_id: "evt-shared", at: LATE }]);
    expect(files.recorded("ses_third")).toEqual([{ event_id: "evt-shared", at: LATE + 60_000 }]);
  });
});

// A card's `at:` is the sending server's clock and its `delivered:` this machine's. On 2026-10-31 a
// real 4 h 45 min wait was read as slow clocks; the mirror, a clock 4 h 45 min ahead, printed the
// same `waited:` line byte for byte for a card that waited a second.
describe("a card whose two clocks disagree", () => {
  const AHEAD = 4 * 3_600_000 + 45 * 60_000;

  test("on a machine whose clock runs ahead says so, and makes no wait of it", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock, clockOffset: () => AHEAD });
    const prompt = card("evt-ahead", SENT, SENT);
    // Written one second after its send, by a clock 4 h 45 min ahead.
    clock = SENT + 1_000 + AHEAD;
    ages.written(prompt, SESSION, () => []);

    const text = textOf(
      ages.annotate([prompt], () => [], SESSION),
      "evt-ahead"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T11:12:18Z"\n');
    expect(text).not.toContain("waited:");
    expect(text).toContain(
      "\n  clocks: \"this machine's clock runs 4 h 45 min ahead of the Envoy listener's, read from the listener's replies; delivered is this machine's clock and at the sending server's\"\n"
    );
  });

  test("on a machine whose clock runs behind says so, and states the wait it would hide", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock, clockOffset: () => -AHEAD });
    const stale = card("evt-behind", SENT, SENT);
    // Written two hours after its send, by a clock 4 h 45 min behind: before its `at:`.
    clock = SENT + 2 * 3_600_000 - AHEAD;
    ages.written(stale, SESSION, () => []);

    const text = textOf(
      ages.annotate([stale], () => [], SESSION),
      "evt-behind"
    );

    expect(text).toContain('\n  delivered: "2026-10-31T03:42:17Z"\n');
    expect(text).toContain(
      "\n  waited: \"2 h 0 min between at and delivered, less the difference in clocks; delivered is this machine's clock and at the sending server's, and at is when it was sent, not the present time\"\n"
    );
    expect(text).toContain(
      "\n  clocks: \"this machine's clock runs 4 h 45 min behind the Envoy listener's, read from the listener's replies; delivered is this machine's clock and at the sending server's\"\n"
    );
  });

  test("delivered well before its send with no difference measured says the clocks disagree by at least that", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock });
    const early = card("evt-before", SENT, SENT);
    clock = SENT - 20 * 60_000;
    ages.written(early, SESSION, () => []);

    const text = textOf(
      ages.annotate([early], () => [], SESSION),
      "evt-before"
    );

    expect(text).not.toContain("waited:");
    expect(text).toContain(
      "\n  clocks: \"delivered is 20 min before at, so this machine's clock runs at least 20 min behind the sending server's; delivered is this machine's clock and at the sending server's\"\n"
    );
  });

  // Only the listener this session talks to is measured. A sending server whose clock runs ahead
  // of it (Dispatch's, or a listener on the sender's machine) leaves the card dated after it was
  // delivered even less the difference measured, and the card says which clock that leaves ahead.
  test("delivered well before its send, less the difference measured, says the sending server's clock runs ahead", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock, clockOffset: () => 20 * 60_000 });
    const early = card("evt-third-clock", SENT, SENT);
    clock = SENT - 3 * 3_600_000;
    ages.written(early, SESSION, () => []);

    const text = textOf(
      ages.annotate([early], () => [], SESSION),
      "evt-third-clock"
    );

    expect(text).not.toContain("waited:");
    expect(text).toContain(
      "\n  clocks: \"this machine's clock runs 20 min ahead of the Envoy listener's, read from the listener's replies; delivered is still 3 h 20 min before at less that difference, so the sending server's clock runs at least 3 h 20 min ahead of the Envoy listener's; delivered is this machine's clock and at the sending server's\"\n"
    );
  });

  // The stated limit: a difference under the threshold is neither shown nor taken off the wait.
  test("by less than the threshold adds to the wait it states, and says nothing of the clocks", () => {
    let clock = SENT;
    const ages = new CardAges({ now: () => clock, clockOffset: () => 10 * 60_000 });
    const late = card("evt-small-skew", SENT, SENT);
    clock = SENT + 2 * 3_600_000 + 10 * 60_000;
    ages.written(late, SESSION, () => []);

    const text = textOf(
      ages.annotate([late], () => [], SESSION),
      "evt-small-skew"
    );

    expect(text).toContain('waited: "2 h 10 min between at and delivered; ');
    expect(text).not.toContain("clocks:");
  });

  test("keeps the difference its time was fixed with, and takes the first one measured when there was none", () => {
    let clock = SENT;
    let offset: number | undefined;
    const ages = new CardAges({ now: () => clock, clockOffset: () => offset });
    const prompt = card("evt-measured-later", SENT, SENT);
    clock = SENT + 1_000 + AHEAD;
    ages.written(prompt, SESSION, () => []);
    const unmeasured = textOf(
      ages.annotate([prompt], () => [], SESSION),
      "evt-measured-later"
    );
    offset = AHEAD;
    const measured = textOf(
      ages.annotate([prompt], () => [], SESSION),
      "evt-measured-later"
    );
    // A later measurement leaves the card as it read, so the prompt cache reads it again.
    offset = 0;
    const again = textOf(
      ages.annotate([prompt], () => [], SESSION),
      "evt-measured-later"
    );

    expect(unmeasured).toContain('waited: "4 h 45 min between at and delivered; ');
    expect(measured).not.toContain("waited:");
    expect(measured).toContain("clocks: \"this machine's clock runs 4 h 45 min ahead of");
    expect(again).toBe(measured);
  });
});

test("a wait reads in hours and minutes", () => {
  expect([duration(52 * 60_000 + 59_000), duration(4 * 3_600_000 + 45 * 60_000)]).toEqual([
    "52 min",
    "4 h 45 min",
  ]);
});
