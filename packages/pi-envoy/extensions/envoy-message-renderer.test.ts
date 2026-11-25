import { describe, expect, test } from "bun:test";
import { renderInbound } from "@legion/envoy-client/delivery";
import type { MessageRenderer, MessageRendererTheme } from "@legion/pi-shared/pi-types";
import { registerEnvoyMessageRenderer } from "./envoy-message-renderer";

const SESSION = "01a01111-2222-7333-4444-555555555555";
const PEER = "01a0cccc-dddd-7eee-ffff-0123456789ab";

/** A theme that adds no colour, so a row reads the card's characters alone. */
const theme: MessageRendererTheme = {
  fg: (_color, text) => text,
  bold: (text) => text,
  boxRound: {
    topLeft: "+",
    topRight: "+",
    bottomLeft: "+",
    bottomRight: "+",
    horizontal: "-",
    vertical: "|",
  },
};

function renderer(): MessageRenderer {
  let registered: MessageRenderer | undefined;
  registerEnvoyMessageRenderer({
    registerMessageRenderer: (customType, render) => {
      if (customType === "envoy-message") registered = render;
    },
  });
  if (registered === undefined) throw new Error("envoy-message renderer was not registered");
  return registered;
}

/**
 * The rows of the card's text as a terminal 200 columns wide draws it, collapsed or expanded,
 * each without the box's border and indent: from the card's first line, `envoy:`, to its last
 * row before the box's bottom padding.
 */
function textRows(content: string, expanded: boolean): string[] {
  const component = renderer()({ customType: "envoy-message", content }, { expanded }, theme);
  if (component === undefined) throw new Error("renderer returned no component");
  const rows = component
    .render(200)
    .map((line) => Bun.stripANSI(line).replace(/^\|/, "").replace(/\|$/, "").trim());
  const from = rows.indexOf("envoy:");
  let to = rows.length;
  while (to > from && (rows[to - 1] === "" || rows[to - 1]?.startsWith("+") === true)) to -= 1;
  return rows.slice(from, to);
}

/** `content`'s lines as `textRows` reads them. */
function trimmed(lines: readonly string[]): string[] {
  return lines.map((line) => line.trim());
}

function card(fields: Record<string, unknown>): string {
  return renderInbound(
    JSON.stringify({
      event_id: "event-1",
      source: "agent",
      source_event_id: "agent.event-1",
      topic: `notifications.agent.${SESSION}`,
      dedupe_key: "agent.event-1",
      issued_at: Date.parse("2026-11-22T13:30:50Z"),
      payload_summary: "message",
      trace_id: "trace-1",
      ...fields,
    }),
    SESSION
  ).content;
}

/** A person's message to the session through Dispatch's message route, as its card shows it:
 *  measured on 2026-11-22 in production, the card ended in `…` and its body showed only after
 *  Ctrl+O (dispatch://INFRA-5271, R2). */
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
          id: "9d4bfad6-0000-4000-8000-000000000000",
          in_reply_to: null,
          issue_key: null,
          target: `session:${SESSION}`,
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode: "steer" },
    }),
    payload_summary: body,
  });
}

/** The fields of an agent's message that a listener kept for the session, with a reply asked for
 *  and a message it replies to and one it replaces, so the card's fields run to the fold. */
const KEPT = {
  kept_at: Date.parse("2026-11-22T13:00:00Z"),
  expires_at: Date.parse("2026-11-22T14:30:50Z"),
  urgency: "low",
  expects_reply: "required",
  in_reply_to: "agent.event-0",
  supersedes: "agent.event-00",
};

/** An agent's message whose sender holds a role, kept as `KEPT` says, so the card's fields run
 *  past the fold and the block of its words opens below it. */
function roleHolderMessage(payload: string): string {
  return card({
    source_session: PEER,
    sender: { session_id: PEER, title: "Reviewer", roles: ["legion-reviewer"] },
    payload,
    ...KEPT,
  });
}

describe("a collapsed envoy-message card", () => {
  test("shows the text of a message sent through Dispatch's message route", () => {
    const body = "Reply with the single word fromweb and nothing else.";
    const lines = dispatchMessage(body).split("\n");
    const at = lines.indexOf(body);
    // The header of such a card is longer than the fold, which is how the body was folded away.
    expect(at).toBeGreaterThan(12);
    expect(lines[at - 1]).toBe("<body>");

    expect(textRows(lines.join("\n"), false)).toEqual([
      ...trimmed(lines.slice(0, 12)),
      "…",
      body,
      ...(at < lines.length - 1 ? ["…"] : []),
    ]);
  });

  test("shows the text of an agent's message whose fields run past the fold", () => {
    const lines = roleHolderMessage("The review is done: two findings.").split("\n");
    const at = lines.indexOf("The review is done: two findings.");
    expect(at).toBeGreaterThan(12);
    expect(lines[at - 1]).toBe("<message>");

    expect(textRows(lines.join("\n"), false)).toEqual([
      ...trimmed(lines.slice(0, 12)),
      "…",
      "The review is done: two findings.",
      ...(at < lines.length - 1 ? ["…"] : []),
    ]);
  });

  // An agent's message whose summary is its head, from a sender that holds no role, opens its
  // block on line 11 and writes the message on line 12, right after the twelve lines above the
  // fold: no line is left out before it, so no `…` stands there, and one stands for its closing
  // tag.
  test("shows the words with no fold before them when they are the line right after the fold", () => {
    const words = "Both findings are fixed.";
    const lines = card({
      source_session: PEER,
      payload: words,
      payload_summary: words,
      ...KEPT,
    }).split("\n");
    expect(lines.indexOf(words)).toBe(12);
    expect(lines).toHaveLength(14);

    expect(textRows(lines.join("\n"), false)).toEqual([...trimmed(lines.slice(0, 13)), "…"]);
  });

  test("folds a long card that carries no words of a sender at the fold alone", () => {
    const lines = roleHolderMessage(JSON.stringify({ step: 3, done: false })).split("\n");
    expect(lines.length).toBeGreaterThan(12);

    expect(textRows(lines.join("\n"), false)).toEqual([...trimmed(lines.slice(0, 12)), "…"]);
  });

  // A block holds the words line by line, where a field held them all on its one line, so the
  // collapsed card shows each line of them that is past the fold.
  test("shows every line of a message's words that is past the fold", () => {
    const body = "First, the plan.\n\nThen the checks:\n- one\n- two";
    const lines = dispatchMessage(body).split("\n");
    const at = lines.indexOf("First, the plan.");
    expect(at).toBeGreaterThan(12);
    expect(lines[at + 5]).toBe("</body>");

    expect(textRows(lines.join("\n"), false)).toEqual([
      ...trimmed(lines.slice(0, 12)),
      "…",
      ...body.split("\n"),
      "…",
    ]);
  });

  test("shows the rest of a message's words that start above the fold and run past it", () => {
    const words = ["one", "two", "three", "four", "five", "six", "seven"];
    const lines = card({
      source_session: PEER,
      sender: { session_id: PEER, title: "Reviewer", roles: ["legion-reviewer"] },
      payload: words.join("\n"),
    }).split("\n");
    const at = lines.indexOf("one");
    expect(at).toBeLessThan(12);
    expect(at + words.length).toBeGreaterThan(12);

    expect(textRows(lines.join("\n"), false)).toEqual([
      ...trimmed(lines.slice(0, at + words.length)),
      "…",
    ]);
  });

  test("shows a short card whole, and an expanded card whole", () => {
    const short = card({ source_session: PEER, payload_summary: "Status only." });
    expect(short.split("\n").length).toBeLessThanOrEqual(12);
    expect(textRows(short, false)).toEqual(trimmed(short.split("\n")));

    const long = dispatchMessage("Ship it?");
    expect(textRows(long, true)).toEqual(trimmed(long.split("\n")));
  });
});
