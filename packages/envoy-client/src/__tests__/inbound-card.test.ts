import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { renderInbound } from "../delivery";
import { storedText } from "../delivery-ledger";
import { readInboundCard } from "../inbound-card";

const reader = "01a01111-2222-7333-4444-555555555555";
const sender = "01a0cccc-dddd-7eee-ffff-0123456789ab";

/** The brief a session launcher has its claimer publish to the new agent's role: the
 *  listener's publish, which names no session, and asks for no reply. */
const BRIEF = [
  "alice started you with the session launcher; this is their request:",
  "",
  "Work on the Dispatch issue ACME-1: https://dispatch.example/issues/ACME-1.",
].join("\n");

function card(fields: Record<string, unknown>): string {
  return renderInbound(
    JSON.stringify({
      event_id: "event-1",
      source: "agent",
      source_event_id: "agent.event-1",
      topic: `notifications.agent.${reader}`,
      dedupe_key: "agent.event-1",
      issued_at: Date.parse("2026-11-22T13:38:52Z"),
      payload_summary: "message",
      trace_id: "trace-1",
      ...fields,
    }),
    reader
  ).content;
}

/** A person's direct message to the reader from Dispatch, as its delivery frame carries it. */
function dispatchMessage(body: string): string {
  const actor = { id: "alice", kind: "user" };
  return card({
    source: "dispatch",
    dedupe_key: "send-1",
    payload: JSON.stringify({
      event: {
        actor,
        issue_key: null,
        payload: {
          author: actor,
          body,
          created_at: "2026-11-22T13:30:50Z",
          deliveries: [],
          id: "44444444-4444-4444-8444-444444444444",
          in_reply_to: null,
          issue_key: null,
          target: `session:${reader}`,
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode: "steer" },
    }),
    payload_summary: body,
  });
}

/** The card's line `line`, so a row reads the words where the card holds them. */
function lineOf(text: string, line: number | undefined): string | undefined {
  return line === undefined ? undefined : text.split("\n")[line];
}

describe("readInboundCard", () => {
  test("reads an Envoy message's sender and its whole text, and the line that holds it", () => {
    const text = card({
      source: "envoy",
      source_event_id: "envoy.brief",
      topic: "notifications.role.alice-acme-1",
      payload: BRIEF,
      payload_summary: `${BRIEF.slice(0, 40)}…`,
      expects_reply: false,
    });

    const read = readInboundCard(text);

    expect(read).toMatchObject({ dispatch: false, from: "envoy", words: { text: BRIEF } });
    expect(lineOf(text, read?.words?.line)).toStartWith("  message: ");
  });

  // `renderInbound` writes a session's payload that says no more than its summary once, in the
  // block of its message.
  test("reads a short message from its summary", () => {
    const text = card({ source_session: sender, payload_summary: "Status only." });

    const read = readInboundCard(text);

    expect(read).toMatchObject({ from: sender, words: { text: "Status only." } });
    expect(lineOf(text, read?.words?.line)).toBe("Status only.");
  });

  // A sender that holds a role is named with its title and its role, and its words follow the
  // card's fields in a block of their own.
  test("finds the words below the fields of a sender that holds a role", () => {
    const text = card({
      source_session: sender,
      sender: { session_id: sender, title: "Reviewer", roles: ["legion-reviewer"] },
      payload: "The review is done: two findings.",
    });

    const read = readInboundCard(text);

    expect(read?.from).toBe(`${sender} (Reviewer)`);
    expect(read?.words?.text).toBe("The review is done: two findings.");
    expect(lineOf(text, (read?.words?.line ?? 0) - 1)).toBe("<message>");
    expect(lineOf(text, read?.words?.line)).toBe("The review is done: two findings.");
  });

  test("reads no words from a structured message", () => {
    const read = readInboundCard(
      card({ source_session: sender, payload: JSON.stringify({ step: 3, done: false }) })
    );

    expect(read).toEqual({
      bodyNote: undefined,
      dispatch: false,
      from: sender,
      note: undefined,
      words: undefined,
    });
  });

  // An empty `message` is no words: a host that drew it would draw a message with nothing in it.
  test("reads no words from an empty message", () => {
    const text = card({ source_session: sender, payload: "", payload_summary: "Nothing here." });
    expect(text).toContain("<message>\n\n</message>");

    expect(readInboundCard(text)?.words).toBeUndefined();
  });

  // `renderInbound` warns, beside the words, that a body names a session other than its sender.
  test("reads the note a card adds about its words", () => {
    const named = "01a0aaaa-bbbb-7ccc-dddd-eeeeeeeeeeee";
    const read = readInboundCard(
      card({ source_session: sender, payload: `Ask ${named} about it.` })
    );

    expect(read?.bodyNote).toBeUndefined();
    expect(read).toMatchObject({
      note: `body names session ${named}`,
      words: { text: `Ask ${named} about it.` },
    });
  });

  test("reads a Dispatch message's body, past the card's header", () => {
    const text = dispatchMessage("Reply with the single word fromweb and nothing else.");

    const read = readInboundCard(text);

    expect(read).toMatchObject({
      dispatch: true,
      from: "dispatch",
      words: { text: "Reply with the single word fromweb and nothing else." },
    });
    expect(read?.words?.line).toBeGreaterThan(12);
    expect(lineOf(text, read?.words?.line)).toBe(
      "Reply with the single word fromweb and nothing else."
    );
  });

  // The card holds a message's words in a block of their own, line by line, so the words take each
  // of the block's lines, and the host that shows them shows each line.
  test("reads each line of a message that runs over several lines of its block", () => {
    const body = "First, the plan.\n\nThen the checks:\n- one\n- two";
    const text = dispatchMessage(body);

    const read = readInboundCard(text);

    expect(read?.words).toMatchObject({ text: body, lines: 5 });
    const line = read?.words?.line ?? 0;
    expect(
      text
        .split("\n")
        .slice(line, line + 5)
        .join("\n")
    ).toBe(body);
    expect(lineOf(text, line + 5)).toBe("</body>");
  });

  test("reads a field's words as one line of the card", () => {
    const read = readInboundCard(
      card({
        source: "envoy",
        source_event_id: "envoy.brief",
        topic: "notifications.role.alice-acme-1",
        payload: BRIEF,
        payload_summary: `${BRIEF.slice(0, 40)}…`,
      })
    );

    expect(read?.words).toMatchObject({ text: BRIEF, lines: 1 });
  });

  test("reads the body of the contract's targeted delivery fixture", () => {
    const payload = readFileSync(
      new URL("../../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
      "utf8"
    );
    const read = readInboundCard(card({ source: "dispatch", payload }));

    expect(read?.words?.text).toBe("Can this ship?");
  });

  test("reads no words from a Dispatch event that is no message", () => {
    const read = readInboundCard(
      card({
        source: "dispatch",
        payload: JSON.stringify({
          id: 1,
          issue_key: "ACME-1",
          seq: 7,
          type: "issue.updated",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-11-22T00:00:00Z",
          payload: { title: "Ship it", status: "in_progress" },
        }),
      })
    );

    expect(read).toEqual({
      bodyNote: undefined,
      dispatch: true,
      from: "dispatch",
      note: undefined,
      words: undefined,
    });
  });

  // What follows the TOON block is no TOON: the line a card published while the session was away
  // ends with, and a picture's placeholder as Oh My Pi's card shows it.
  test("reads a card that ends in lines of its own", () => {
    const text = `${storedText(card({ source_session: sender, payload_summary: "Hi." }), "2026-11-22T13:00:00Z")}\n[picture: image/png]`;

    expect(readInboundCard(text)?.words?.text).toBe("Hi.");
  });

  test("reads nothing from text that is no card", () => {
    expect(readInboundCard("Unsubscribed from ACME-1 by alice")).toBeUndefined();
    expect(readInboundCard(`notice:\n  ${BRIEF.split("\n")[0]}`)).toBeUndefined();
  });
});
