// The card (`renderInbound`) under dispatch://INFRA-3686's R4: a card for a daemon notice carries "no field twice apart from the head summary".
// A catch-up names each kept decision as a row (texts.DecisionMark) with `more`, whether more
// follows than the act's event carried, and names its words in the words object with the same
// `more` (workflow's keptFacts sets both from one Carry). ae79500d has a block name no `by` or
// `at` that the object holding its field names, and the catch-up leaves `rest` out of the row for
// the words object to give, but `more` is drawn both ways: the row's `more` column reads `true`
// and the answer's block opens with more="true". A person's reply over 32,000 UTF-16 units in the
// thread of an ask a parked tree keeps reaches the daemon cut (Dispatch's EventBody,
// body_truncated), so its kept decision carries words and more, and the next catch-up of the
// tree's architect draws this. The payload is the one texts.CatchUpRecord marshals at d5c0f25b.
import { expect, test } from "bun:test";
import { decode } from "@toon-format/toon";
import { renderInbound } from "../delivery";
import { splitWordsBlocks } from "../words-block";

const reader = "01a01111-2222-7333-4444-555555555555";
const architectTopic = "notifications.role.legion-alpha-alpha-4000-architect";

test("a kept decision whose words were cut says once that more follows", () => {
  const payload = {
    kind: "catch-up",
    issue: "ALPHA-4000",
    catch_up: {
      generation: 8,
      launch: 6,
      gates: [],
      issues: [],
      kept: {
        decisions: [
          {
            ref: "dispatch://ALPHA-4000/ask/a1",
            at: "2026-11-18T10:55:00Z",
            issue: "ALPHA-4050",
            decision: "ask-reply",
            actor: "user:afkslx",
            answer: "Ship the region nearest the queue.",
            answer_at: "2026-11-18T10:54:00Z",
            more: true,
          },
        ],
      },
    },
    words: {
      "catch_up.kept.decisions[0].answer": {
        by: "user:afkslx",
        at: "2026-11-18T10:54:00Z",
        more: true,
      },
    },
  };
  const { content } = renderInbound(
    JSON.stringify({
      event_id: "c2f0e1a4b7d94c3e8a1f5b6d9e0c7a28",
      source: "agent",
      source_event_id: "legion-outbox:41",
      topic: architectTopic,
      dedupe_key: "legion-outbox:41",
      issued_at: Date.parse("2026-11-18T11:00:00Z"),
      urgency: "med",
      payload_summary: "catch-up on ALPHA-4000",
      payload: JSON.stringify(payload),
      trace_id: "trace-daemon",
    }),
    reader,
    architectTopic
  );
  const { head, blocks } = splitWordsBlocks(content);
  const { envoy } = decode(head) as {
    envoy: { message: { catch_up: { kept: { decisions: Record<string, unknown>[] } } } };
  };
  const row = envoy.message.catch_up.kept.decisions[0] ?? {};
  const block = blocks.find(({ field }) => field === "catch_up.kept.decisions[0].answer");

  expect(block?.text).toBe("Ship the region nearest the queue.");
  expect([row.more, block?.more].filter((more) => more === true)).toEqual([true]);
});
