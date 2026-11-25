import { expect, test } from "bun:test";
import { legionControllerNoticeSubject } from "@legion/contracts";
import { renderInbound } from "../delivery";
import { splitWordsBlocks } from "../words-block";

const reader = "01a01111-2222-7333-4444-555555555555";

function daemonMessage(topic: string, summary: string, payload: unknown, urgency = "med"): string {
  return JSON.stringify({
    event_id: "c2f0e1a4b7d94c3e8a1f5b6d9e0c7a28",
    source: "agent",
    source_event_id: "legion-outbox:41",
    topic,
    dedupe_key: "legion-outbox:41",
    issued_at: Date.parse("2026-11-14T21:05:01Z"),
    urgency,
    payload_summary: summary,
    payload: JSON.stringify(payload),
    trace_id: "trace-daemon",
  });
}

/** The project-paused notice as pause.PausedNotice builds it (texts.ProjectPausedRecord): the
 *  pause, who paused (`by.kind: operator`), when, and the reason, named in `words` with who paused
 *  and when. */
function paused(reason: string) {
  return {
    kind: "project-paused",
    issue: "ALPHA",
    pause: { id: 41, by: { kind: "operator" }, at: "2026-11-18T11:00:00Z", reason },
    words: { "pause.reason": { by: "operator", at: "2026-11-18T11:00:00Z" } },
  };
}

const topic = legionControllerNoticeSubject("alpha");

// R4: "A card ... for a daemon notice ... carries ... no field twice apart from the head summary";
// R1: the block of another author's words "names the author and the time where the text's own
// fields do not already name them". The project-paused card names who paused in its fields
// (`pause.by.kind: operator`), so its reason block names no author again; it already leaves the
// time out, which `pause.at` names.
test("a project-paused card names who paused once", () => {
  const content = renderInbound(
    daemonMessage(topic, "project-paused on ALPHA", paused("Load test of the cluster.")),
    reader,
    topic
  ).content;
  const { head, blocks } = splitWordsBlocks(content);
  expect(head).toContain("kind: operator");
  expect(blocks.length).toBe(1);
  expect(blocks[0]?.at).toBeUndefined();
  expect(blocks[0]?.by).toBeUndefined();
});
