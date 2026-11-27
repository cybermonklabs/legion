import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { readDispatchPayload } from "@legion/contracts";
import { renderInbound } from "../delivery";
import { parseFrameJSON } from "../frame-json";

// The plugin's side of the edge's Dispatch property: the generated Dispatch frames the edge
// listener passes on a session's own agent subject for a token of PRIV, which
// TestEdgeFrameVerdictOnGeneratedDispatchFrames
// (packages/envoy/cmd/listener/edge_dispatch_property_test.go, the one generator) records in
// packages/contracts/fixtures/dispatch-frame-property.json with its own verdicts, must not reach
// the session as a card that shows another project's event.
//
// The card is the frame rendered by renderInbound on the session's own agent subject; it shows
// another project's event when it holds "PUB" or "pub-1". This plugin shows nothing of a Dispatch
// payload readDispatchPayload reads no event of; the plugin's earlier raw display showed that
// payload whole, so the oracle adds it back for such a frame, read as the plugin reads it
// (parseFrameJSON), and holds the edge's rule alone, without the plugin's change behind it.
//
// The oracle can fail: over the first frames the edge withholds, which the fixture records too, it
// finds cards that show PUB, and a fixed frame - the token's project beside another project's
// nested event - shows PUB in its card.

type PropertyFixture = { readonly passed: readonly string[]; readonly withheld: readonly string[] };

const fixture = JSON.parse(
  readFileSync(
    new URL("../../../contracts/fixtures/dispatch-frame-property.json", import.meta.url),
    "utf8"
  )
) as PropertyFixture;

const session = "ses_v";
const own = `notifications.agent.${session}`;

/** The payload value the plugin's earlier raw display showed whole for a frame it read no event
 *  of, or undefined for a frame whose payload it read as an event. */
function rawShownPayload(frame: string): unknown {
  const envelope = JSON.parse(frame) as { readonly payload?: unknown };
  if (typeof envelope.payload !== "string") return undefined;
  let value: unknown;
  try {
    value = parseFrameJSON(envelope.payload);
  } catch {
    return envelope.payload;
  }
  return readDispatchPayload(value).kind === "event" ? undefined : value;
}

/** Whether that earlier plugin's card of frame shows another project's event. */
function cardShowsPub(frame: string): boolean {
  let content: string;
  try {
    content = (renderInbound(frame, session, own) as { content: string }).content;
  } catch {
    return false;
  }
  const raw = rawShownPayload(frame);
  if (raw !== undefined) content += `\n${JSON.stringify(raw)}`;
  return /PUB/.test(content) || /pub-1/.test(content);
}

describe("the Dispatch frames the edge passes", () => {
  test("control: over the frames the edge withholds, the oracle finds cards that show PUB", () => {
    const showing = fixture.withheld.filter(cardShowsPub);
    console.log(
      `dispatch-frame property: of ${fixture.withheld.length} frames the edge withholds, ${showing.length} cards show PUB`
    );
    expect(showing.length).toBeGreaterThan(0);
  });

  test("control: the token's project beside another project's nested event shows PUB in the card", () => {
    // The edge's side withholds this frame (TestEdgeFrameVerdictPassesADispatchFrameOnlyOfAListedProject,
    // "the token's project beside another project's nested event, which the plugin reads as no event").
    const pubEvent = JSON.stringify({
      issue_key: "PUB-1",
      project: "PUB",
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      payload: { id: "c-1", body: "PUB's comment" },
    });
    const frame = JSON.stringify({
      source: "dispatch",
      event_id: "e-1",
      topic: own,
      payload: `{"event":${pubEvent},"project":"PRIV","issue_key":"PRIV-1"}`,
    });
    expect(cardShowsPub(frame)).toBe(true);
  });

  test("show another project's event in no card", () => {
    const showing = fixture.passed.filter(cardShowsPub);
    console.log(
      `dispatch-frame property: the edge passes ${fixture.passed.length} frames, ${showing.length} of whose cards show PUB`
    );
    expect(fixture.passed.length).toBeGreaterThan(0);
    expect(showing).toEqual([]);
  }, 120_000);
});
