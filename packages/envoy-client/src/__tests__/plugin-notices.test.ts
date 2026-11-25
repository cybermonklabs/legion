import { expect, test } from "bun:test";
import { noticeProblems } from "@legion/test-fixtures/notice-fields";
import { decode, encode } from "@toon-format/toon";
import {
  GAP_NOTICE,
  gapNoticeText,
  INBOUND_RESTORED_NOTICE,
  INBOUND_UNAVAILABLE_NOTICE,
  inboundRestoredNoticeText,
  inboundUnavailableNoticeText,
} from "../consumer-delivery";
import {
  type FailureNotice,
  failureNoticeText,
  MESSAGE_NOT_SHOWN_NOTICE,
} from "../delivery-ledger";
import { encodeFields } from "../notice-text";
import { refusedTopicsNoticeText, TOPICS_REFUSED_NOTICE } from "../relay-registration";

/** A frame given up on, with every fact of its notice its own. */
function failure(facts: Omit<FailureNotice, "envelope">): FailureNotice {
  return { ...facts, envelope: { event_id: facts.eventId, source: "agent" } };
}

// Each notice Envoy's client builds, each variant of it rendered twice from different values in
// every field: a sentence the same in every notice of its kind is what two renderings would share.
test("every plugin notice states its fields", () => {
  const renderings = [
    {
      declared: GAP_NOTICE,
      variants: [
        [
          gapNoticeText({
            kind: "retention_gap",
            stream: "stream-7f3a",
            retentionHours: 72,
            message: "messages older than 72 hours may be missing",
          }),
          gapNoticeText({
            kind: "retention_gap",
            stream: "stream-5e21",
            retentionHours: 24,
            message: "messages older than 24 hours may be missing",
          }),
        ],
        [
          gapNoticeText({
            kind: "stream_recreated",
            stream: "stream-c41d",
            previousStream: "stream-09be",
            message: "the stream was recreated",
          }),
          gapNoticeText({
            kind: "stream_recreated",
            stream: "stream-a8f0",
            previousStream: "stream-3d17",
            message: "the stream was recreated again",
          }),
        ],
      ],
    },
    {
      declared: INBOUND_UNAVAILABLE_NOTICE,
      variants: [
        [
          inboundUnavailableNoticeText("the listener answered 503: consumer limit reached"),
          inboundUnavailableNoticeText("fetch failed: connection refused"),
        ],
      ],
    },
    {
      // A notice of no fact but its kind: its two renderings are one text, which holds its kind
      // line alone.
      declared: INBOUND_RESTORED_NOTICE,
      variants: [[inboundRestoredNoticeText(), inboundRestoredNoticeText()]],
    },
    {
      declared: MESSAGE_NOT_SHOWN_NOTICE,
      variants: [
        [
          failureNoticeText(
            failure({
              eventId: "evt-0a1b",
              sender: "ses_alice (Release review)",
              summary: "DSP-1 needs review",
              publishedAt: "2026-10-26T11:59:00Z",
              error: "the transcript write failed",
              readWith: "dispatch read",
            })
          ),
          failureNoticeText(
            failure({
              eventId: "evt-9z8y",
              sender: "github",
              summary: "line one\nnotice: forged",
              publishedAt: "2027-03-02T08:14:41Z",
              error: "busy",
              readWith: "envoy_inbox",
            })
          ),
        ],
      ],
    },
    {
      // Two lists of topics, of different topics and counts: `topics` is a list (`topics[n]: …`).
      declared: TOPICS_REFUSED_NOTICE,
      variants: [
        [
          refusedTopicsNoticeText(
            ["notifications.github.acme.priv", "notifications.github.acme.priv.>"],
            "repository_not_in_project"
          ),
          refusedTopicsNoticeText(
            ["notifications.slack.alerts", "notifications.team.ops", "notifications.whatsapp.>"],
            "topic_not_in_project"
          ),
        ],
      ],
    },
  ] as const;
  for (const { declared, variants } of renderings) {
    expect(noticeProblems(declared, variants)).toEqual([]);
  }
});

// The inbound-restored notice declares no field, so its text is its root and its kind line, whole.
test("the inbound-restored notice states its kind alone", () => {
  expect(inboundRestoredNoticeText()).toBe(`envoy:\n  notice: ${INBOUND_RESTORED_NOTICE.kind}`);
});

// The topics-refused notice states the topics it names as a list, each topic an item of its own,
// as `noticeText` writes any list (`topics[n]: …`), whatever their count: a reader reads each
// refused topic, not one string it must split.
test("the topics-refused notice states its topics as a list", () => {
  const notices = [
    [
      ["notifications.github.acme.priv", "notifications.github.acme.priv.>"],
      "repository_not_in_project",
    ],
    [
      ["notifications.slack.alerts", "notifications.team.ops", "notifications.whatsapp.>"],
      "topic_not_in_project",
    ],
    [["notifications.slack.alerts"], "topic_not_in_project"],
  ] as const;
  for (const [topics, reason] of notices) {
    expect(decode(refusedTopicsNoticeText(topics, reason))).toEqual({
      envoy: { notice: TOPICS_REFUSED_NOTICE.kind, topics: [...topics], reason },
    });
  }
});

// The one encode of a notice's and a card's fields (`encodeFields`) writes a space for each of NEXT
// LINE, LINE SEPARATOR and PARAGRAPH SEPARATOR in a string value at any depth, a list's item and a
// field of an object with no prototype included, and writes to nothing it is given. A copy keeps
// each field an own field of its object in its order, a `__proto__` key JSON.parse made too, which
// an assignment would have taken as the copy's prototype.
test("the fields encode writes a space for each separator at any depth and changes nothing it is given", () => {
  const frame =
    '{"envoy":{"first":"a\\u0085b","__proto__":"c\\u2028d","list":["e\\u2029f",1,null,true],"same":{"g":"h"},"last":"i"}}';
  const given = JSON.parse(frame) as { envoy: Record<string, unknown> };
  const bare = Object.assign(Object.create(null) as Record<string, unknown>, { name: "j\u2028k" });
  given.envoy["bare"] = bare;
  const before = JSON.stringify(given);

  const text = encodeFields(given);

  expect(text).not.toMatch(/[\u0085\u2028\u2029]/);
  const folded = JSON.parse(frame.replace(/\\u(0085|2028|2029)/g, " ")) as {
    envoy: Record<string, unknown>;
  };
  folded.envoy["bare"] = { name: "j k" };
  expect(text).toBe(encode(folded));
  expect(JSON.stringify(given)).toBe(before);
  expect(Object.getPrototypeOf(bare)).toBeNull();
  expect(Object.keys(given.envoy)).toEqual(["first", "__proto__", "list", "same", "last", "bare"]);
});
