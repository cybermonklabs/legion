import { expect, test } from "bun:test";
import { gapNoticeText, inboundUnavailableNoticeText } from "../consumer-delivery";
import { failureNoticeText } from "../delivery-ledger";

// R2 of dispatch://INFRA-3686: "Each daemon text, and each plugin notice that the Design names, is
// built from a record of its kind ... No field holds a sentence that is the same in every text of
// its kind." The Design names Envoy's gap, inbound-unavailable and message-not-shown notices,
// pi-envoy's session-id notice and the wake for cards that came with no turn. Each is rendered
// twice from different values in every field; the lines both renderings hold, other than the root
// and the kind (`notice: <kind>`), are a field holding the same words in every notice of the kind.
test("no field of Envoy's own notices holds the same sentence in every notice of its kind", () => {
  const pairs: Record<string, readonly [string, string]> = {
    gap: [
      gapNoticeText({
        kind: "retention_gap",
        stream: "stream-7f3a",
        retentionHours: 72,
        message: "a",
      }),
      gapNoticeText({
        kind: "retention_gap",
        stream: "stream-5e21",
        retentionHours: 24,
        message: "b",
      }),
    ],
    "inbound unavailable": [
      inboundUnavailableNoticeText("the listener answered 503: consumer limit reached"),
      inboundUnavailableNoticeText("fetch failed: connection refused"),
    ],
    "message not shown": [
      failureNoticeText({
        eventId: "evt-0a1b",
        sender: "ses_alice",
        summary: "DSP-1 needs review",
        publishedAt: "2026-10-26T11:59:00Z",
        error: "the transcript write failed",
        readWith: "dispatch read",
        envelope: { event_id: "evt-0a1b", source: "agent" },
      } as never),
      failureNoticeText({
        eventId: "evt-9z8y",
        sender: "github",
        summary: "other",
        publishedAt: "2027-03-02T08:14:41Z",
        error: "busy",
        readWith: "envoy_inbox",
        envelope: { event_id: "evt-9z8y", source: "agent" },
      } as never),
    ],
  };
  const shared: string[] = [];
  for (const [kind, [a, b]] of Object.entries(pairs)) {
    const second = new Set(b.split("\n"));
    for (const line of a.split("\n")) {
      if (line === "envoy:" || line.trimStart().startsWith("notice: ")) continue;
      if (second.has(line)) shared.push(`${kind}: ${line.trim()}`);
    }
  }
  expect(shared).toEqual([]);
});
