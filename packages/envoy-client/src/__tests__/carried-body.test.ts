import { describe, expect, test } from "bun:test";
import type { DispatchEvent } from "@legion/contracts";
import { decode } from "@toon-format/toon";

import { bearerCredential } from "../bearer";
import {
  type CarriedItem,
  type DispatchDelivery,
  renderInbound,
  wholeCarriedBody,
} from "../delivery";
import { DispatchClient } from "../dispatch-http";
import { eventTextsWhole } from "../dispatch-picture-tools";
import { pictureAddresses } from "../dispatch-pictures";
import { splitWordsBlocks } from "../words-block";

const SERVER = "https://dispatch.example";
const COMMENT_ID = "22222222-2222-4222-8222-222222222222";
const MESSAGE_ID = "33333333-3333-4333-8333-333333333333";
const PICTURE = "dispatch://DSP-1/artifact/shot-png@v1";
/** The comment as it is stored: the picture its author embedded sits past where the event's
 *  share of the body ends. */
const STORED_BODY = `The first paragraph.\n\n![shot.png](${PICTURE})`;
const CARRIED_BODY = "The first paragraph.";

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** A Dispatch that answers the one comment read, as `stored` says the comment now stands, and
 *  the read of issue message `MESSAGE_ID`, as `storedMessage` says that message stands,
 *  recording each path; `fail` answers every read 500 instead, which is how a host meets a
 *  Dispatch that is down. */
function dispatch(
  fail = false,
  stored: object = { body: STORED_BODY },
  storedMessage: object = { body: STORED_BODY }
) {
  const requests: string[] = [];
  const fetchImpl = async (input: RequestInfo | URL): Promise<Response> => {
    const url = new URL(input instanceof Request ? input.url : input.toString());
    requests.push(url.pathname);
    if (fail) return json({ code: "INTERNAL", error: "down" }, 500);
    if (url.pathname === `/api/v1/comments/${COMMENT_ID}`) {
      return json({ comment: { ...comment, ...stored }, replies: [] });
    }
    if (url.pathname === `/api/v1/issues/DSP-1/messages/${MESSAGE_ID}`) {
      return json({ message: { ...message, ...storedMessage }, replies: [] });
    }
    return json({ code: "NOT_FOUND", error: "not found" }, 404);
  };
  return {
    requests,
    client: new DispatchClient(SERVER, bearerCredential("token"), fetchImpl as typeof fetch),
  };
}

const comment = {
  id: COMMENT_ID,
  issue_key: "DSP-1",
  artifact_id: null,
  author: { kind: "user" as const, id: "alice" },
  body: CARRIED_BODY,
  anchor: null,
  reply_to: null,
  ask_id: null,
  turn: null,
  resolved: false,
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  deleted_at: null,
  deleted_by: null,
  suggestion: null,
  reply_block: null,
  answers: null,
  mentions: [],
  deliveries: [],
  created_at: "2026-09-09T00:00:00Z",
};

const message = {
  id: MESSAGE_ID,
  issue_key: "DSP-1",
  author: { kind: "user" as const, id: "alice" },
  body: CARRIED_BODY,
  in_reply_to: null,
  broadcast_id: null,
  reply_block: null,
  deliveries: [],
  edited_at: null,
  deleted_at: null,
  deleted_by: null,
  created_at: "2026-09-09T00:00:00Z",
};

const cutEvent: DispatchEvent = {
  id: 7,
  seq: 7,
  issue_key: "DSP-1",
  type: "comment.created",
  actor: { kind: "user", id: "alice" },
  notify: true,
  created_at: "2026-09-09T00:00:00Z",
  payload: { ...comment, body_truncated: true, artifact_name: "" },
};

const wholeEvent: DispatchEvent = {
  ...cutEvent,
  id: 8,
  seq: 8,
  payload: { ...comment, body: STORED_BODY, artifact_name: "" },
};

/** One inbound envelope as Dispatch's outbox publishes it, carrying `event`; `event` is the
 *  wire event, which a stand-in holds fewer fields of than the schema's own type. */
function envelopeOf(event: object): string {
  return JSON.stringify({
    event_id: "dispatch-7",
    source: "dispatch",
    source_event_id: "7",
    topic: "notifications.dispatch.issue.DSP-1.comment.created",
    dedupe_key: "dispatch-7",
    issued_at: 1_788_756_072_000,
    payload_summary: "DSP-1 comment created",
    payload: JSON.stringify({ ...event, quiet: false }),
    trace_id: "dispatch-7",
  });
}

/** The stand-in Dispatch's outbox publishes when the bus refuses an event whole: the payload's
 *  top-level fields short enough to send, and `payload_omitted`, with no body at all. */
const omittedEvent = {
  ...cutEvent,
  id: 9,
  seq: 9,
  payload: {
    id: COMMENT_ID,
    issue_key: "DSP-1",
    author: comment.author,
    created_at: comment.created_at,
    payload_omitted: true,
  },
};

// A picture an author embedded past where the event's share of the body ends is named nowhere in
// the event, so the body is read whole from the server before its pictures are listed.
describe("pictures of a body an event carried only in part", () => {
  test("the cut body is read whole, so the picture past the cut is found", async () => {
    const { client, requests } = dispatch();

    const texts = await eventTextsWhole(client, [cutEvent]);

    expect(texts).toEqual([{ text: STORED_BODY, at: cutEvent.created_at }]);
    expect(pictureAddresses(texts[0]?.text ?? "")).toEqual([PICTURE]);
    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
  });

  test("a body that rode whole is not read again", async () => {
    const { client, requests } = dispatch();

    const texts = await eventTextsWhole(client, [wholeEvent]);

    expect(texts).toEqual([{ text: STORED_BODY, at: wholeEvent.created_at }]);
    expect(requests).toEqual([]);
  });

  test("a read that fails leaves the event the part it carried", async () => {
    const { client } = dispatch(true);

    expect(await eventTextsWhole(client, [cutEvent])).toEqual([
      { text: CARRIED_BODY, at: cutEvent.created_at },
    ]);
  });
});

// A host that prompts its agent with the frame reads the whole body into the card first:
// `renderInbound` names the item whose body the event carried in part, or not at all, the host
// reads that item, and a second render is given the body it answered. The body is its author's
// words, so the card holds it in a block of its own after its fields.
function bodyBlocks(content: string): string[] {
  return splitWordsBlocks(content)
    .blocks.filter((block) => block.name === "body")
    .map((block) => block.text);
}

/** The payload fields of a Dispatch card's event, as TOON's `decode` reads the card's fields once
 *  its blocks are split off. */
function cardPayload(content: string): Record<string, unknown> {
  const card = decode(splitWordsBlocks(content).head) as {
    readonly envoy: { readonly dispatch: { readonly payload: Record<string, unknown> } };
  };
  return card.envoy.dispatch.payload;
}

describe("renderInbound and a body an event did not carry whole", () => {
  const SESSION = "ses_reader";
  const ITEM: CarriedItem = { resource: "comment", id: COMMENT_ID, issueKey: "DSP-1" };

  test("names the cut item, and the body read whole lands on the card with no note", async () => {
    const { client, requests } = dispatch();

    const cut = renderInbound(envelopeOf(cutEvent), SESSION);
    expect(cut.cutItem).toEqual(ITEM);
    expect(cut.content).toContain("the body is its first part only");

    const body = await wholeCarriedBody(client, ITEM, SESSION);
    const whole = renderInbound(envelopeOf(cutEvent), SESSION, undefined, body);

    expect(body).toEqual({ body: STORED_BODY });
    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
    expect(bodyBlocks(whole.content)).toEqual([STORED_BODY]);
    expect(whole.content).not.toContain("first part only");
    expect(whole.cutItem).toBeUndefined();
  });

  test("names the item of a stand-in whose body the bus dropped, and takes its body too", async () => {
    const { client, requests } = dispatch();

    const omitted = renderInbound(envelopeOf(omittedEvent), SESSION);
    expect(omitted.cutItem).toEqual(ITEM);
    expect(omitted.content).toContain(
      `this event carries no body: it was too large for the bus; read it whole with GET /api/v1/comments/${COMMENT_ID}`
    );

    const body = await wholeCarriedBody(client, ITEM, SESSION);
    const whole = renderInbound(envelopeOf(omittedEvent), SESSION, undefined, body);

    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
    expect(bodyBlocks(whole.content)).toEqual([STORED_BODY]);
    expect(whole.content).not.toContain("too large for the bus");
    expect(whole.cutItem).toBeUndefined();
  });

  test("keeps the part it carried, and the note, when the read fails or the body rode whole", async () => {
    const failing = dispatch(true).client;

    expect(await wholeCarriedBody(failing, ITEM, SESSION)).toBeUndefined();
    const kept = renderInbound(envelopeOf(cutEvent), SESSION, undefined, undefined);
    expect(bodyBlocks(kept.content)).toEqual([CARRIED_BODY]);
    expect(kept.content).toContain(
      `the body is its first part only; read it whole with GET /api/v1/comments/${COMMENT_ID}`
    );

    const rode = renderInbound(envelopeOf(wholeEvent), SESSION);
    expect(rode.cutItem).toBeUndefined();
    expect(rode.content).not.toContain("first part only");
  });

  // An item deleted since the event has no body: the card names the delete in fields of its own
  // (`deleted_at`, `deleted_by`), where the body the delete cleared would stand, and draws no
  // block for words that are gone.
  test("an item deleted since the event reads as the delete's fields and no body", async () => {
    const deletion = {
      deleted_at: "2026-09-10T00:00:00Z",
      deleted_by: { kind: "user", id: "alice" },
    };
    const deleted = dispatch(false, { body: "", ...deletion }, { body: "", ...deletion }).client;
    // The event's carried part embeds the picture, so the card lists it until the body read whole
    // replaces that part: the deleted post has no body the card may draw from.
    const cutWithPicture = {
      ...cutEvent,
      payload: { ...comment, body: STORED_BODY, body_truncated: true, artifact_name: "" },
    };
    expect(renderInbound(envelopeOf(cutWithPicture), SESSION).pictures).toEqual([PICTURE]);

    // The delete cleared the body, which the card would otherwise print empty.
    const body = await wholeCarriedBody(deleted, ITEM, SESSION);
    expect(body).toEqual({ body: "", ...deletion });
    const whole = renderInbound(envelopeOf(cutWithPicture), SESSION, undefined, body);
    const payload = cardPayload(whole.content);
    expect(payload).toMatchObject(deletion);
    expect(payload["body"]).toBeUndefined();
    expect(bodyBlocks(whole.content)).toEqual([]);
    expect(whole.content).not.toContain(CARRIED_BODY);
    expect(whole.content).not.toContain("first part only");
    expect(whole.pictures).toBeUndefined();
    expect(
      await wholeCarriedBody(
        deleted,
        { resource: "message", id: MESSAGE_ID, issueKey: "DSP-1" },
        SESSION
      )
    ).toEqual({ body: "", ...deletion });
  });

  // A targeted frame names the delivery it asks of the session: the message or the comment, the
  // attempt and its mode, where the answer goes, and the broadcast a message is one copy of. The
  // render given the body whole names that same delivery, its `body` the one the read answered.
  // A post deleted since has no body left in the payload, which the frame's schema requires, so
  // the delivery is read from the frame as that schema checked it, never from the payload the read
  // rewrote: a host still holds the frame to the session it names and knows its mode.
  describe("the delivery a targeted frame names, read again", () => {
    const MENTION_ID = "44444444-4444-4444-8444-444444444444";
    const BROADCAST_ID = "55555555-5555-4555-8555-555555555555";
    const target = `session:${SESSION}`;
    const deletion = {
      deleted_at: "2026-09-10T00:00:00Z",
      deleted_by: { kind: "user", id: "alice" },
    };
    const messageItem: CarriedItem = { resource: "message", id: MESSAGE_ID, issueKey: "DSP-1" };
    const messageFrame = (mode: DispatchDelivery["mode"], payload: object = {}) => ({
      event: {
        ...cutEvent,
        type: "message.created",
        payload: { ...message, body_truncated: true, target, ...payload },
      },
      delivery: { attempt: 3, mode },
    });
    /** The delivery a frame names, but for its body, which is what the read changes. */
    type Named = Omit<DispatchDelivery, "body" | "bodyTruncated">;
    const messageDelivery = (mode: DispatchDelivery["mode"]): Named => ({
      resource: "message",
      id: MESSAGE_ID,
      attempt: 3,
      mode,
      replyPath: `/api/v1/messages/${MESSAGE_ID}/reply`,
      replyFields: {},
      issueKey: "DSP-1",
    });
    const rows: readonly {
      readonly name: string;
      readonly frame: object;
      readonly item: CarriedItem;
      readonly delivery: Named;
    }[] = [
      ...(["steer", "aside", "btw"] as const).map((mode) => ({
        name: `a message sent as ${mode}`,
        frame: messageFrame(mode),
        item: messageItem,
        delivery: messageDelivery(mode),
      })),
      {
        name: "one recipient's copy of a broadcast",
        frame: messageFrame("aside", { broadcast_id: BROADCAST_ID }),
        item: messageItem,
        delivery: { ...messageDelivery("aside"), broadcastId: BROADCAST_ID },
      },
      {
        name: "a comment mention",
        frame: {
          event: cutEvent,
          delivery: { attempt: 2, mode: "steer", comment_id: MENTION_ID, target },
        },
        item: ITEM,
        delivery: {
          resource: "comment",
          id: MENTION_ID,
          attempt: 2,
          mode: "steer",
          replyPath: `/api/v1/comments/${MENTION_ID}/reply`,
          replyFields: { target },
          issueKey: "DSP-1",
        },
      },
    ];
    /** The frame addressed to the session's own subject, as the outbox publishes a delivery. */
    const addressed = (frame: object) =>
      JSON.stringify({
        event_id: "dispatch-targeted-7",
        source: "dispatch",
        source_event_id: "7",
        topic: `notifications.agent.${SESSION}`,
        dedupe_key: "dispatch-targeted-7",
        issued_at: 1_788_756_072_000,
        payload_summary: "DSP-1 delivery",
        payload: JSON.stringify(frame),
        trace_id: "dispatch-targeted-7",
      });

    for (const row of rows) {
      test(`${row.name} names the delivery its frame names whether the read finds the post whole or deleted`, async () => {
        const raw = addressed(row.frame);
        const cut = renderInbound(raw, SESSION);
        expect(cut.cutItem).toEqual(row.item);
        expect(cut.delivery).toEqual({ ...row.delivery, body: CARRIED_BODY, bodyTruncated: true });

        const stored = await wholeCarriedBody(dispatch().client, row.item, SESSION);
        const whole = renderInbound(raw, SESSION, undefined, stored);
        expect(whole.delivery).toEqual({ ...row.delivery, body: STORED_BODY });

        const deleted = dispatch(false, { body: "", ...deletion }, { body: "", ...deletion });
        const gone = await wholeCarriedBody(deleted.client, row.item, SESSION);
        expect(gone).toEqual({ body: "", ...deletion });
        const card = renderInbound(raw, SESSION, undefined, gone);
        expect(card.delivery).toEqual({ ...row.delivery, body: "" });
        expect(card.cutItem).toBeUndefined();
        // The card names the delete and holds nothing of the post, the part the event carried
        // included.
        expect(cardPayload(card.content)).toMatchObject(deletion);
        expect(bodyBlocks(card.content)).toEqual([]);
        expect(card.content).not.toContain(CARRIED_BODY);
      });
    }
  });

  // A card that re-reads the post holds the words the post holds now, which an edit since the
  // event replaced, so it says when they were written: `edited_at`, a field of the card's own,
  // with the words alone in their block.
  test("a card that re-reads an item edited since carries the edit's time beside its words", async () => {
    const edit = { body: STORED_BODY, edited_at: "2026-09-10T00:00:00Z" };
    const { client, requests } = dispatch(false, edit, edit);

    const body = await wholeCarriedBody(client, ITEM, SESSION);
    expect(body).toEqual(edit);
    expect(requests).toEqual([`/api/v1/comments/${COMMENT_ID}`]);
    const whole = renderInbound(envelopeOf(cutEvent), SESSION, undefined, body);
    expect(cardPayload(whole.content)["edited_at"]).toBe("2026-09-10T00:00:00Z");
    expect(bodyBlocks(whole.content)).toEqual([STORED_BODY]);
    expect(whole.content).not.toContain("first part only");
    expect(
      await wholeCarriedBody(
        client,
        { resource: "message", id: MESSAGE_ID, issueKey: "DSP-1" },
        SESSION
      )
    ).toEqual(edit);
  });

  // A card that prints only what its delivery frame carried is a delivery Dispatch already sent,
  // and keeps what it carried, with no mark, though Dispatch now holds the post edited. Each frame
  // here carries an edited_at of its own, so a card that printed a mark from its frame would print
  // one.
  test("a card that prints only what its frame carried carries no edited mark", () => {
    const edited_at = "2026-09-10T00:00:00Z";
    const rode = renderInbound(
      envelopeOf({
        ...wholeEvent,
        payload: { ...comment, body: STORED_BODY, edited_at, artifact_name: "" },
      }),
      SESSION
    );
    expect(bodyBlocks(rode.content)).toEqual([STORED_BODY]);
    expect(rode.content).not.toContain("· edited");

    const kept = renderInbound(
      envelopeOf({
        ...cutEvent,
        payload: { ...comment, edited_at, body_truncated: true, artifact_name: "" },
      }),
      SESSION,
      undefined,
      undefined
    );
    expect(bodyBlocks(kept.content)).toEqual([CARRIED_BODY]);
    expect(kept.content).not.toContain("· edited");
  });
});
