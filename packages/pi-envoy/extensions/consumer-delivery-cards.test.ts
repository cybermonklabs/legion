// The extension's consumer-delivery rows on the card a frame becomes: a late BTW, the notice of a
// frame the session was never shown, a BTW whose side turn runs on, a card Escape cleared, a
// payload the card shows as its text, the time a card reached the session, and a body an event
// carried only in part. The relay's own rows are in `consumer-delivery.test.ts`, which says why
// the three files are apart. Each has the `describe` "consumer delivery", and they boot one
// session (`../test/consumer-delivery-session`) on one harness (`../test/extension-harness`).
import { describe, expect, test } from "bun:test";
import { legacyDispatchIssueSubject } from "@legion/contracts";
import { logger } from "@oh-my-pi/pi-utils";
import { REPLAY_DELIVERED_ENTRY } from "../src/missed-messages";
import { AGENT, bootConsumer, peer, SESSION } from "../test/consumer-delivery-session";
import {
  ledgerSeam,
  sessionContext,
  settle,
  targetedDispatchEnvelope,
  until,
  useExtensionHarness,
} from "../test/extension-harness";
import { response } from "../test/listener-stand-in";

useExtensionHarness();

describe("consumer delivery", () => {
  test("a late BTW is shown as a card that starts no turn, and runs no side turn", async () => {
    let sideTurns = 0;
    const session = await bootConsumer("consumer-late-btw", {
      runEphemeralTurn: async () => {
        sideTurns += 1;
        return { replyText: "too late" };
      },
    });

    const inbox = session.relay(9, targetedDispatchEnvelope("btw", "late-btw"), { delivered: 2 });
    await until(() => session.replies(inbox).includes("done"), "the late BTW's done");

    const cards = session.cards("dispatch-late-btw");
    expect(cards).toHaveLength(1);
    expect(cards[0]?.content).toContain("runs no side turn");
    expect(cards[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });
    // The inert path stamps the hand-over state too, so a later reading of the stored transcripts
    // says which state held the waiting card.
    expect(cards[0]?.details).toMatchObject({ handOver: "inert" });
    expect(session.events.indexOf("written dispatch-late-btw")).toBeLessThan(
      session.events.indexOf(`${inbox} done`)
    );
    expect(sideTurns).toBe(0);
    expect(session.dispatchReplies).toHaveLength(0);
  });

  // The one window before a late BTW's card: the read of the body whole. The card is the
  // session the frame names, and `sendCard` would hand it to the conversation the host moved to
  // inside that read, under the name of the session the person wrote to. Nothing is handed: the
  // claim goes back, so the frame is naked and the listener sends it again to the session it
  // names and to no other.
  test("shows no late BTW card when the host left inside the read of its body, and naks the frame", async () => {
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    const move: { fire?: () => Promise<void> } = {};
    let moved = false;
    try {
      const session = await bootConsumer("consumer-late-btw-moved", {
        dispatch: async (url) => {
          if (!url.pathname.includes("/messages/")) return undefined;
          if (!moved) {
            moved = true;
            await move.fire?.();
          }
          return response({
            message: { body: "Can this ship?", id: "11111111-1111-4111-8111-111111111111" },
            replies: [],
          });
        },
      });
      // The conversation the host moves to is idle and holds no message, so an inert card is
      // handed to it at once rather than waiting for a settled moment: this row reads whether
      // the card went there, not whether the host was ready for it.
      move.fire = async () => {
        await session.fixture.handlers.get("session_switch")?.(
          { reason: "new" },
          {
            ...sessionContext("ses_after_btw"),
            hasPendingMessages: () => false,
            isIdle: () => true,
          }
        );
      };

      const inbox = session.relay(
        9,
        targetedDispatchEnvelope("btw", "late-btw-moved", { bodyTruncated: true }),
        { delivered: 2 }
      );
      await until(() => session.replies(inbox).includes("nak"), "the late BTW's nak");

      expect(moved).toBe(true);
      expect(session.cards("dispatch-late-btw-moved")).toHaveLength(0);
      expect(warnings).toContain(
        "envoy: a card was not shown here; the session it names is closed"
      );
      expect(session.replies(inbox)).toEqual(["received", "nak"]);
    } finally {
      stopSink();
    }
  });

  // The notice naming a frame the session was never shown is that session's: the ledger hands
  // the host the session the frame was relayed to (`DeliveryLedgerHost.showFailureNotice`), and
  // the host shows it there and nowhere else. The host moves while the notice is in flight -
  // the one window where the session the notice names and the conversation the host has open
  // part company, the ledger having bound the give-up to that session before it called - and the
  // notice is still held to the session it names: it is handed to nobody, the conversation the
  // host moved to is shown nothing of the frame, and the frame goes back to the stream for
  // whoever takes ses_delivery next (dispatch://INFRA-771).
  test("holds a give-up's notice to the session its frame was relayed to, showing the conversation the host moved to nothing of it", async () => {
    const moved = "ses_moved_notice";
    const warnings: logger.LogEvent[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry);
    });
    try {
      const session = await bootConsumer("consumer-notice-addressed", {
        // Every card of this row's frames is refused, so each of their deliveries fails; the
        // notice naming a frame is no card of it, and is taken. The host's own messages - the
        // card wake's among them - are left alone, since refusing those tests nothing here and
        // throws where nothing is waiting to catch it.
        refuseCard: (message) =>
          String(message.details?.event_id ?? "").startsWith("evt-notice-") &&
          !message.content.includes("message not shown"),
      });
      // One frame's three deliveries: the third is given up on, and what comes back is what the
      // session answered the listener on that delivery's inbox.
      const givenUp = async (seq: number, label: string): Promise<string[]> => {
        let inbox = "";
        for (const delivered of [1, 2, 3]) {
          inbox = session.relay(seq, peer(label), { delivered });
          await until(
            () => session.replies(inbox).length > 1,
            `the answer to delivery ${delivered} of ${label}`
          );
        }
        return session.replies(inbox);
      };

      // The host stays on the session the frame was relayed to: the notice is carded there, and
      // the frame is settled on it.
      expect(await givenUp(9, "notice-shown")).toEqual(["received", "done"]);
      const shown = session.cards("evt-notice-shown");
      expect(shown).toHaveLength(1);
      expect(shown[0]?.content).toContain("notice: message not shown");

      // The host moves to another conversation while the next frame's notice is in flight.
      ledgerSeam.beforeFailureNotice = async () => {
        ledgerSeam.beforeFailureNotice = undefined;
        await session.fixture.handlers.get("session_switch")?.(
          { reason: "new" },
          { ...sessionContext(moved), hasPendingMessages: () => false, isIdle: () => true }
        );
      };
      expect(await givenUp(10, "notice-moved")).toEqual(["received", "nak"]);

      // The conversation the host moved to was shown nothing of that frame: no card of it, and
      // no notice naming it.
      expect(session.cards("evt-notice-moved")).toHaveLength(0);
      expect(
        session.fixture.deliveries.filter((delivery) =>
          JSON.stringify(delivery).includes("evt-notice-moved")
        )
      ).toEqual([]);
      // Which is because the notice was addressed to the session the frame was relayed to, with
      // another conversation open here: the one card not shown is that notice.
      expect(
        warnings
          .filter(
            (entry) =>
              entry.message === "envoy: a card was not shown here; the session it names is closed"
          )
          .map((entry) => entry.context)
      ).toEqual([{ addressed: SESSION, eventID: "evt-notice-moved", open: moved }]);
      // The first frame's notice is still there, so the row above reads a session that is shown
      // notices, not one that is shown nothing at all.
      expect(session.cards("evt-notice-shown")).toHaveLength(1);
    } finally {
      ledgerSeam.beforeFailureNotice = undefined;
      stopSink();
    }
  });

  test("a BTW whose side turn is still running delays neither the next frame's received nor its card", async () => {
    const answering = Promise.withResolvers<{ replyText: string }>();
    let sideTurns = 0;
    const session = await bootConsumer("consumer-btw-off-worker", {
      runEphemeralTurn: () => {
        sideTurns += 1;
        return answering.promise;
      },
    });

    const btw = session.relay(1, targetedDispatchEnvelope("btw", "btw-running"));
    await until(() => sideTurns === 1, "the side turn");
    const next = session.relay(2, peer("after-btw"));
    await until(() => session.replies(next).includes("done"), "the next frame's done");

    expect(session.replies(next)).toEqual(["received", "done"]);
    expect(session.cards("evt-after-btw")).toHaveLength(1);
    expect(session.replies(btw)).toEqual(["received"]);

    answering.resolve({ replyText: "the answer" });
    await until(() => session.replies(btw).includes("done"), "the BTW's done");
    expect(session.dispatchReplies).toHaveLength(1);
    expect(session.cards("dispatch-btw-running")).toHaveLength(0);
  });

  test("a card Escape cleared from the queue between received and done is shown once afterwards, and done only after that write", async () => {
    const session = await bootConsumer("consumer-queue-cleared", { turn: "streaming" });

    const inbox = session.relay(5, peer("cleared"));
    await until(() => session.cards("evt-cleared").length > 0, "the card");
    expect(session.replies(inbox)).toEqual(["received"]);

    await session.interrupt();
    expect(session.replies(inbox)).toEqual(["received"]);
    expect(
      session.entries().filter((entry) => JSON.stringify(entry).includes("evt-cleared"))
    ).toEqual([]);

    await session.tick();
    await until(() => session.replies(inbox).includes("done"), "the frame's done");

    expect(session.events.filter((event) => event === "written evt-cleared")).toHaveLength(1);
    expect(session.events.indexOf("written evt-cleared")).toBeLessThan(
      session.events.indexOf(`${inbox} done`)
    );
    expect(session.replies(inbox)).toEqual(["received", "done"]);
    await session.tick();
    expect(session.events.filter((event) => event === "written evt-cleared")).toHaveLength(1);
  });

  // A peer's message whose JSON payload is nested past the depth the card's TOON encoder reaches
  // made the shared renderer throw in `deliver`, so every copy of the frame failed and went back
  // to the stream. The card shows such a payload as the text its sender sent, the frame is taken,
  // and the extension says so once, naming the message.
  test("a message whose payload is nested 9,999 deep is shown as its text, taken, and logged once with its id", async () => {
    const warnings: { readonly message: string; readonly context?: unknown }[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push({ message: entry.message, context: entry.context });
    });
    const session = await bootConsumer("consumer-deep-payload");
    try {
      const nested = `${'{"a":'.repeat(9_999)}1${"}".repeat(9_999)}`;
      const inbox = session.relay(
        1,
        JSON.stringify({ event_id: "evt-deep", source: "agent", topic: AGENT, payload: nested })
      );
      // The answer that settles the frame: `done` once it is taken, `nak` once it failed.
      await until(
        () => session.replies(inbox).includes("done") || session.replies(inbox).includes("nak"),
        "the frame's settling answer",
        30_000
      );

      expect(session.replies(inbox)).toEqual(["received", "done"]);
      // TOON quotes the text as JSON quotes a string, so the card holds the sender's text whole.
      expect(session.cards("evt-deep").map((card) => card.content)).toEqual([
        expect.stringContaining(`\n  message: ${JSON.stringify(nested)}`),
      ]);
      expect(warnings.filter(({ message }) => message.includes("shown as its text"))).toEqual([
        {
          message: "envoy: a message's payload is shown as its text; the card could not lay it out",
          context: {
            event_id: "evt-deep",
            error: "its layout is larger than the 1048576 bytes a card holds",
          },
        },
      ]);
    } finally {
      stopSink();
      await session.stop();
    }
  });

  // A session behind on its inbox read a card stamped five hours earlier, took the stamp for the
  // present time, and told other sessions that the machines' clocks were slow. Each model request
  // now shows when the card reached the session beside when it was sent (`../src/card-age`).
  describe("the time a card reached the session", () => {
    test("a card the host writes hours after its send shows every model request when it was delivered and how long it waited", async () => {
      const session = await bootConsumer("card-age-late", { turn: "streaming" });
      const sentAt = Date.now() - (5 * 3_600_000 + 30_000);
      const frame = JSON.stringify({ ...JSON.parse(peer("card-age-late")), issued_at: sentAt });

      // The turn streams, so the host holds the card until the turn takes it.
      session.relay(1, frame);
      await until(() => session.cards("evt-card-age-late").length > 0, "the relayed card");
      await session.take();
      const [card] = session.cards("evt-card-age-late");
      if (card === undefined) throw new Error("the card was not handed");
      const written = {
        role: "custom",
        customType: card.customType,
        content: card.content,
        display: true,
        details: card.details,
        timestamp: sentAt,
      };
      const context = sessionContext(SESSION);
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      const first = JSON.stringify(await contextHandler({ messages: [written] }, context));
      const second = JSON.stringify(await contextHandler({ messages: [written] }, context));

      expect(first).toMatch(/\\n {2}delivered: \\"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\\"\\n/);
      expect(first).toContain(
        "waited: \\\"5 h 0 min between at and delivered; delivered is this machine's clock and at the sending server's, and at is when it was sent, not the present time\\\""
      );
      // The same bytes in every request, so the provider's prompt cache reads them again.
      expect(second).toBe(first);
      // The card the host keeps is the one it was handed.
      expect(card.content).not.toContain("delivered:");
    });

    // The mirror of the case above: a machine whose clock runs 4 h 45 min ahead of the listener's
    // read a card that waited a second exactly as if it had waited 4 h 45 min. The listener's
    // replies carry its clock, so the card says how the clocks stand and states no wait.
    test("a card from a listener whose clock runs hours behind this machine's says so, and states no wait", async () => {
      const behind = 4 * 3_600_000 + 45 * 60_000;
      const session = await bootConsumer("card-age-clocks", {
        turn: "streaming",
        listenerClock: () => Date.now() - behind,
      });
      // The listener stamps the send by its own clock, a second before the card reaches us.
      const sentAt = Date.now() - behind - 1_000;
      const frame = JSON.stringify({ ...JSON.parse(peer("card-age-clocks")), issued_at: sentAt });

      session.relay(1, frame);
      await until(() => session.cards("evt-card-age-clocks").length > 0, "the relayed card");
      await session.take();
      const [card] = session.cards("evt-card-age-clocks");
      if (card === undefined) throw new Error("the card was not handed");
      const written = {
        role: "custom",
        customType: card.customType,
        content: card.content,
        display: true,
        details: card.details,
        timestamp: Date.now(),
      };
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      const request = JSON.stringify(
        await contextHandler({ messages: [written] }, sessionContext(SESSION))
      );

      expect(request).not.toContain("waited:");
      expect(request).toContain(
        "clocks: \\\"this machine's clock runs 4 h 45 min ahead of the Envoy listener's, read from the listener's replies; delivered is this machine's clock and at the sending server's\\\""
      );
    });

    // One process serves one session at a time, and a session the host moves to is shown the
    // same event by its own consumer. The request that session makes must state when the event
    // reached it, from its own ledger record, and never when the session before it was shown it.
    test("an event in the session the host moved to reads that session's own delivery time", async () => {
      const session = await bootConsumer("card-age-moved", { turn: "streaming" });
      session.relay(1, peer("card-age-moved"));
      await until(() => session.cards("evt-card-age-moved").length > 0, "the relayed card");
      await session.take();

      // The host resumes a session it left, whose file holds no record of this event: what that
      // session was shown is its own file's, and this process's write for the session before it
      // is not in it.
      const mark = session.fixture.entries.length;
      const movedFile = () => session.fixture.entries.slice(mark);
      const resumed = sessionContext("ses_moved");
      const movedContext = {
        ...resumed,
        sessionManager: { ...resumed.sessionManager, getBranch: movedFile, getEntries: movedFile },
      };
      await session.fixture.handlers.get("session_switch")?.({ reason: "resume" }, movedContext);
      await settle();

      const [card] = session.cards("evt-card-age-moved");
      if (card === undefined) throw new Error("the card was not handed");
      // Handed before this process started, as a card held across a restart is: the request time
      // is no delivery time for it, so with nothing in this session's file it stays as it is.
      const written = {
        role: "custom",
        customType: card.customType,
        content: card.content,
        display: true,
        details: card.details,
        timestamp: 1,
      };
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      const movedWithoutRecord =
        JSON.stringify(await contextHandler({ messages: [written] }, movedContext)) ?? "";

      // The write this session's host made, a minute and a half before this request.
      const movedAt = Math.floor((Date.now() - 90_000) / 1_000) * 1_000;
      const movedIso = `${new Date(movedAt).toISOString().slice(0, 19)}Z`;
      const recorded = {
        ...movedContext,
        sessionManager: {
          ...movedContext.sessionManager,
          getBranch: () => [
            ...movedFile(),
            {
              type: "custom",
              customType: REPLAY_DELIVERED_ENTRY,
              data: { event_id: "evt-card-age-moved", at: movedAt },
            },
          ],
        },
      };
      const movedWithRecord = JSON.stringify(
        await contextHandler({ messages: [written] }, recorded)
      );
      const live = JSON.stringify(
        await contextHandler({ messages: [written] }, sessionContext(SESSION))
      );
      const liveAgain = JSON.stringify(
        await contextHandler({ messages: [written] }, sessionContext(SESSION))
      );

      // Never the time the session shown it first was shown it: with nothing in this session's
      // file, its request states no delivery time at all.
      expect(movedWithoutRecord).not.toContain("delivered:");
      expect(movedWithRecord).toContain(`delivered: \\"${movedIso}\\"`);
      // The session shown it first reads its own write, which its `message_end` fixed.
      expect(live).toMatch(/\\n {2}delivered: \\"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\\"\\n/);
      expect(live).not.toContain(movedIso);
      // Returning to it reads the same bytes, so the provider's prompt cache reads them again.
      expect(liveAgain).toBe(live);
    });

    // The delivery a session first gives a card goes into its file once, at the card's
    // `message_end` (`envoy-card-delivered`, a literal so the case loads against a source without
    // the export), and the requests after it record nothing (`../src/card-age`).
    test("a card's write records its delivery in the session once", async () => {
      const session = await bootConsumer("card-age-recorded", { turn: "streaming" });
      session.relay(1, peer("card-age-recorded"));
      await until(() => session.cards("evt-card-age-recorded").length > 0, "the relayed card");
      await session.take();
      const [card] = session.cards("evt-card-age-recorded");
      if (card === undefined) throw new Error("the card was not handed");
      const { customType, content, details } = card;
      const written = { role: "custom", customType, content, display: true, details, timestamp: 1 };
      const contextHandler = session.fixture.handlers.get("context");
      if (contextHandler === undefined) throw new Error("no context handler was registered");
      await contextHandler({ messages: [written] }, sessionContext(SESSION));
      await contextHandler({ messages: [written] }, sessionContext(SESSION));

      const recorded = session.fixture.entries.filter(
        (entry) => entry.customType === "envoy-card-delivered"
      );
      expect(recorded.map((entry) => entry.data)).toEqual([
        { event_id: "evt-card-age-recorded", at: expect.any(Number) },
      ]);
    });
  });

  // A comment, message or broadcast body is bounded block by block, so one event carries its
  // first part alone and says so with `body_truncated`; an event the bus refused whole reaches
  // the session as a stand-in with no body at all (`payload_omitted`). A host that prompted its
  // agent with either would hand it less than the comment says, so the session reads the whole
  // body through its own Dispatch before it prompts; with no Dispatch, or a read that fails, it
  // keeps what the event carried and the note that says so.
  describe("a body an event carried only in part", () => {
    const CUT_COMMENT = "44444444-4444-4444-8444-444444444444";
    const CARRIED = "The first paragraph.";
    const WHOLE = `${CARRIED}\n\nThe second paragraph, past where the event stopped.`;
    /** The topic the session follows, and the subject the comment event is published on: the
     *  frame reaches the session as its own consumer's relay of that subject. */
    const CUT_TOPIC = legacyDispatchIssueSubject("INFRA-370", ">");
    const CUT_SUBJECT = legacyDispatchIssueSubject("INFRA-370", "comment.created");

    /** The payload the event carries: the comment row with its body cut (`body_truncated`), or,
     *  for `omitted`, the stand-in a bus refusal leaves behind - the row's short top-level fields
     *  and `payload_omitted`, with no body at all. */
    function commentPayload(omitted: boolean): Record<string, unknown> {
      const named = {
        id: CUT_COMMENT,
        issue_key: "INFRA-370",
        author: { kind: "user", id: "alice" },
        created_at: "2026-11-02T00:00:00Z",
      };
      return omitted
        ? { ...named, payload_omitted: true }
        : {
            ...named,
            artifact_id: null,
            body: CARRIED,
            body_truncated: true,
            anchor: null,
            reply_to: null,
            ask_id: null,
            turn: null,
            resolved: false,
            resolved_by: null,
            resolved_at: null,
            edited_at: null,
            suggestion: null,
            mentions: [],
            deliveries: [],
            artifact_name: "",
          };
    }

    function cutCommentEnvelope(topic: string, omitted: boolean): string {
      return JSON.stringify({
        event_id: "dispatch-cut-1",
        source: "dispatch",
        source_event_id: "1",
        topic,
        dedupe_key: "dispatch-cut-1",
        issued_at: 1,
        payload_summary: "INFRA-370 comment created",
        payload: JSON.stringify({
          id: 1,
          seq: 1,
          issue_key: "INFRA-370",
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          quiet: false,
          created_at: "2026-11-02T00:00:00Z",
          payload: commentPayload(omitted),
        }),
        trace_id: "dispatch-cut-1",
      });
    }

    /** Boots a session following INFRA-370's topic, relays the comment event - cut, or the
     *  stand-in when `omitted` - on the session's own consumer, the one path a frame reaches it
     *  by, and answers the comment read with `read`. Returns the card its agent was prompted
     *  with. */
    async function cardForCutComment(
      name: string,
      read: (path: string) => Response | undefined,
      omitted = false
    ): Promise<string> {
      const session = await bootConsumer(name);
      await session.follow(CUT_TOPIC);
      await settle();
      // The listener's own answers go on being answered; the comment read is the test's.
      const listener = globalThis.fetch;
      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) =>
        read(new URL(input.toString()).pathname) ?? (await listener(input, init))) as typeof fetch;

      const inbox = session.relay(1, cutCommentEnvelope(CUT_SUBJECT, omitted), {
        subject: CUT_SUBJECT,
      });
      await until(() => session.replies(inbox).includes("done"), "the cut comment's done");
      const card = session.cards("dispatch-cut-1")[0]?.content;
      if (card === undefined) throw new Error("no card was delivered for the cut comment");
      return card;
    }

    test("is prompted with the body the comment route answers", async () => {
      const reads: string[] = [];
      const card = await cardForCutComment("cut-comment-read", (path) => {
        if (path !== `/api/v1/comments/${CUT_COMMENT}`) return undefined;
        reads.push(path);
        return response({
          comment: { id: CUT_COMMENT, issue_key: "INFRA-370", body: WHOLE },
          replies: [],
        });
      });

      expect(reads).toEqual([`/api/v1/comments/${CUT_COMMENT}`]);
      // Byte for byte: the body the route answered, written as the card writes a text holding
      // blank lines (one quoted scalar), and no note, because nothing is missing.
      expect(card).toContain(`body: ${JSON.stringify(WHOLE)}`);
      expect(card).not.toContain("first part only");
    });

    test("keeps the part it carried, and the note, when that read fails", async () => {
      const card = await cardForCutComment("cut-comment-read-fails", (path) =>
        path === `/api/v1/comments/${CUT_COMMENT}`
          ? new Response(JSON.stringify({ code: "INTERNAL", error: "down" }), { status: 500 })
          : undefined
      );

      expect(card).toContain(CARRIED);
      expect(card).not.toContain("The second paragraph");
      expect(card).toContain(
        `the body is its first part only; read it whole with GET /api/v1/comments/${CUT_COMMENT}`
      );
    });

    test("is prompted with that body for a stand-in the bus left with no body at all", async () => {
      const reads: string[] = [];
      const card = await cardForCutComment(
        "omitted-comment-read",
        (path) => {
          if (path !== `/api/v1/comments/${CUT_COMMENT}`) return undefined;
          reads.push(path);
          return response({
            comment: { id: CUT_COMMENT, issue_key: "INFRA-370", body: WHOLE },
            replies: [],
          });
        },
        true
      );

      expect(reads).toEqual([`/api/v1/comments/${CUT_COMMENT}`]);
      expect(card).toContain(`body: ${JSON.stringify(WHOLE)}`);
      expect(card).not.toContain("too large for the bus");
    });

    test("gives the note for a stand-in whose read fails, having no body to keep", async () => {
      const card = await cardForCutComment(
        "omitted-comment-read-fails",
        (path) =>
          path === `/api/v1/comments/${CUT_COMMENT}`
            ? new Response(JSON.stringify({ code: "INTERNAL", error: "down" }), { status: 500 })
            : undefined,
        true
      );

      expect(card).not.toContain("The second paragraph");
      expect(card).toContain(
        `this event carries no body: it was too large for the bus; read it whole with GET /api/v1/comments/${CUT_COMMENT}`
      );
    });

    // The comment changed after its event was written. The card re-reads it, so it prints the
    // comment as it stands: the deleted line every read prints in place of the body a delete
    // cleared, or the words an edit wrote with the edited mark beside them. The part the event
    // carried is in neither, nor the note that the body was cut.
    test("is prompted with the deleted line for a comment deleted since", async () => {
      const card = await cardForCutComment("cut-comment-deleted", (path) =>
        path === `/api/v1/comments/${CUT_COMMENT}`
          ? response({
              comment: {
                id: CUT_COMMENT,
                issue_key: "INFRA-370",
                body: "",
                deleted_at: "2026-11-03T00:00:00Z",
                deleted_by: { kind: "user", id: "alice" },
              },
              replies: [],
            })
          : undefined
      );

      expect(card).toContain(
        `body: ${JSON.stringify("Deleted by user alice at 2026-11-03T00:00:00Z")}`
      );
      expect(card).not.toContain(CARRIED);
      expect(card).not.toContain("first part only");
    });

    test("is prompted with the words an edit wrote and the edited mark for a comment edited since", async () => {
      const card = await cardForCutComment("cut-comment-edited", (path) =>
        path === `/api/v1/comments/${CUT_COMMENT}`
          ? response({
              comment: {
                id: CUT_COMMENT,
                issue_key: "INFRA-370",
                body: "The paragraph as edited.\n\nAnd a second one.",
                edited_at: "2026-11-03T00:00:00Z",
              },
              replies: [],
            })
          : undefined
      );

      expect(card).toContain(
        `body: ${JSON.stringify("The paragraph as edited.\n\nAnd a second one. · edited 2026-11-03T00:00:00Z")}`
      );
      expect(card).not.toContain(CARRIED);
      expect(card).not.toContain("first part only");
    });
  });
});
