import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { decode } from "@toon-format/toon";
import { renderInbound } from "../delivery";

// renderInbound reads every Dispatch payload through readDispatchPayload (packages/contracts), the
// reader the edge listener's verdict is held to: fixtures/dispatch-frame-verdicts.json records that
// reader's kind for every payload of the corpus gen-go writes it from, and
// TestDispatchPayloadIsEventAgreesWithTheHostsReading holds the Go reader to the same file. This
// holds the card to it: each payload, rendered on the session's own agent subject, is shown as
// unrecognised exactly when its kind is not `event`. A renderInbound that read a payload with a
// reader of its own fails on any payload where the two differ.

type Verdict = {
  readonly name: string;
  readonly payload: string;
  readonly kind: "event" | "recovered" | "raw";
};

const verdicts = JSON.parse(
  readFileSync(
    new URL("../../../contracts/fixtures/dispatch-frame-verdicts.json", import.meta.url),
    "utf8"
  )
) as readonly Verdict[];

// No event of the corpus is this session's, so no card is skipped as the session's own.
const session = "ses_reader";
const own = `notifications.agent.${session}`;

describe("renderInbound shows a Dispatch payload as readDispatchPayload reads it", () => {
  test("a payload of the verdict fixture is shown as unrecognised exactly when its kind is not event", () => {
    const disagreements: string[] = [];
    let events = 0;
    for (const { name, payload, kind } of verdicts) {
      const rendered = renderInbound(
        JSON.stringify({ source: "dispatch", event_id: "e-1", topic: own, payload }),
        session,
        own
      );
      // A card the renderer skips, or a notice it writes in place of a card, comes only of an
      // event it read; a notice decodes to a string, which holds no `envoy`.
      const card = rendered.skip
        ? undefined
        : (decode(rendered.content) as { envoy?: Record<string, unknown> } | null);
      const unrecognised = card?.envoy?.unrecognised === "payload";
      if (unrecognised === (kind === "event")) disagreements.push(`${name} (${kind})`);
      if (kind === "event") events += 1;
    }
    console.log(
      `dispatch-frame verdicts: ${verdicts.length} payloads, ${events} events and ${verdicts.length - events} others, ${disagreements.length} cards that disagree`
    );
    expect(verdicts.length).toBeGreaterThanOrEqual(500);
    expect(events).toBeGreaterThan(0);
    expect(verdicts.length - events).toBeGreaterThan(0);
    expect(disagreements).toEqual([]);
  });
});
