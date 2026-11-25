import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { legacyAgentSubject, legacyDispatchIssueSubject } from "@legion/contracts";
import { decode } from "@toon-format/toon";
import { renderInbound } from "../delivery";
import { subscriptionRemovedTopics } from "../dispatch-subscribe";

// The rules `parseFrameJSON` holds every read of an inbound frame to, each row through a reader
// that reads a frame with it. Before them, a zod object that passes unknown keys through copied a
// `__proto__` key by assignment, which set the copy's prototype, so a field the frame did not
// carry was read through that key's object; and the card's TOON encoder threw on text holding half
// a surrogate pair alone, so the host answered the frame `nak` on every delivery.

const reader = "01a01111-2222-7333-4444-555555555555";
const otherSession = "01a02222-3333-7444-8555-666666666666";
/** Another project's issue topic: the one the reported frame named only inside its `__proto__`. */
const otherTopic = "notifications.dispatch.issue.PUB.PUB-1.comment.created";

const targetedDispatchPayload = readFileSync(
  new URL("../../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
  "utf8"
);

/**
 * `fields` with a key spelled `__proto__` holding `proto` beside them, as JSON.parse makes it from
 * a frame's text. An object literal's `__proto__` sets the literal's prototype and makes no key,
 * so the key is defined here as an own property, which JSON.stringify writes.
 */
function withProtoKey(fields: object, proto: unknown): object {
  return Object.defineProperty({ ...fields }, "__proto__", {
    value: proto,
    enumerable: true,
    configurable: true,
    writable: true,
  });
}

/** The envelope `renderInbound` hands on for `raw`, which every row here expects it to hand on. */
function renderedEnvelope(raw: string): Readonly<Record<string, unknown>> {
  const { envelope } = renderInbound(raw, reader);
  if (envelope === undefined) throw new Error(`renderInbound handed on no envelope for ${raw}`);
  return envelope;
}

/** A Dispatch envelope carrying `event` as its payload's JSON text. */
function dispatchFrame(
  event: object,
  topic: string = legacyDispatchIssueSubject("DSP-1", ">")
): string {
  const payload = JSON.stringify(event);
  expect(payload).toContain('"__proto__"');
  return JSON.stringify({ event_id: "dispatch-1", source: "dispatch", topic, payload });
}

/** The targeted message of the shared fixture, its payload's `body` only inside a `__proto__`
 *  key, and without the payload fields `without` names. */
function targetedMessageBodyInProto(without: readonly string[] = []): string {
  const frame = JSON.parse(targetedDispatchPayload) as {
    event: { payload: Record<string, unknown> } & Record<string, unknown>;
  };
  const { body, ...rest } = frame.event.payload;
  for (const field of without) delete rest[field];
  expect(body).toBe("Can this ship?");
  return dispatchFrame(
    { ...frame, event: { ...frame.event, payload: withProtoKey(rest, { body: "Ship it now." }) } },
    legacyAgentSubject("ses_target")
  );
}

describe("an inbound frame's fields are read from its own keys only", () => {
  test("the frames below carry the key as text, as a sender writes it", () => {
    expect(JSON.stringify(withProtoKey({ source: "agent" }, { topic: otherTopic }))).toBe(
      `{"source":"agent","__proto__":{"topic":"${otherTopic}"}}`
    );
  });

  test("renderInbound (InboundEnvelopeSchema): the reported frame's envelope names no topic", () => {
    const envelope = renderedEnvelope(
      `{"source":"dispatch","__proto__":{"topic":"${otherTopic}"}}`
    );

    expect(envelope.topic).toBeUndefined();
    expect(Object.getPrototypeOf(envelope)).toBe(Object.prototype);
  });

  test("renderInbound: the key spelled with a JSON escape names no topic either", () => {
    const envelope = renderedEnvelope(
      `{"source":"dispatch","\\u005f_proto__":{"topic":"${otherTopic}"}}`
    );

    expect(envelope.topic).toBeUndefined();
  });

  test("renderInbound: no field of the envelope is read through the key", () => {
    const fields = [
      ["event_id", "event-2"],
      ["source_event_id", "agent.ses_sender.event-2"],
      ["source_session", otherSession],
      ["topic", otherTopic],
      ["dedupe_key", "agent.ses_target.event-2"],
      ["issued_at", 1],
      ["expires_at", 2],
      ["kept_at", 3],
      ["payload_summary", "summary"],
      ["payload", "body"],
      ["sender", { session_id: otherSession }],
      ["in_reply_to", "event-0"],
      ["supersedes", "event-1"],
      ["urgency", "high"],
      ["expects_reply", "required"],
    ] as const;
    const read = fields
      .filter(
        ([field, value]) =>
          field in
          renderedEnvelope(JSON.stringify(withProtoKey({ source: "agent" }, { [field]: value })))
      )
      .map(([field]) => field);

    expect(read).toEqual([]);
  });

  test("renderInbound: the card names the session the recipient only for a topic the frame carries", () => {
    const raw = JSON.stringify(
      withProtoKey(
        { event_id: "event-1", source: "agent", payload: "hello" },
        { topic: legacyAgentSubject(reader) }
      )
    );
    const card = decode(renderInbound(raw, reader).content) as { envoy: { to?: string } };

    expect(card.envoy.to).toBeUndefined();
  });

  test("renderInbound: a frame's own topic is the one read, whatever its `__proto__` names", () => {
    const envelope = renderedEnvelope(
      `{"source":"dispatch","topic":"notifications.dispatch.issue.PRIV.PRIV-1.comment.created","__proto__":{"topic":"${otherTopic}"}}`
    );

    expect(envelope.topic).toBe("notifications.dispatch.issue.PRIV.PRIV-1.comment.created");
  });

  test("renderInbound: keys spelled `constructor` and `prototype` reach no field", () => {
    const envelope = renderedEnvelope(
      `{"source":"dispatch","constructor":{"topic":"${otherTopic}"},"prototype":{"topic":"${otherTopic}"}}`
    );

    expect(envelope.topic).toBeUndefined();
    expect(Object.getPrototypeOf(envelope)).toBe(Object.prototype);
  });

  test("renderInbound's tolerant read (TolerantInboundEnvelopeSchema) names no topic", () => {
    const envelope = renderedEnvelope(
      `{"source":"dispatch","issued_at":"not a time","__proto__":{"topic":"${otherTopic}"}}`
    );

    expect(envelope.topic).toBeUndefined();
  });

  test("renderInbound (DispatchEventSchema's actor): an actor names this session only by its own id", () => {
    const rendered = renderInbound(
      dispatchFrame({
        issue_key: "DSP-1",
        type: "issue.updated",
        actor: withProtoKey({ kind: "session" }, { id: reader }),
        notify: true,
        payload: { title: "Native Dispatch", status: "in_progress" },
      }),
      reader,
      legacyDispatchIssueSubject("DSP-1", "issue.updated")
    );

    expect(rendered.skip).toBe(false);
  });

  test("renderInbound (DispatchEventSchema's payload): a subscription.removed is this session's only by the session its payload names", () => {
    const rendered = renderInbound(
      dispatchFrame({
        issue_key: "DSP-1",
        type: "subscription.removed",
        actor: { kind: "user", id: "alice" },
        notify: true,
        payload: withProtoKey({ by: { kind: "user", id: "alice" } }, { session_id: reader }),
      }),
      reader,
      legacyDispatchIssueSubject("DSP-1", "subscription.removed")
    );

    expect(rendered).toMatchObject({ skip: true, content: "" });
  });

  test("renderInbound (DispatchTargetedFrameSchema): a targeted message whose body only its `__proto__` holds is no delivery", () => {
    const rendered = renderInbound(targetedMessageBodyInProto(), reader);

    expect(rendered.delivery).toBeUndefined();
    expect(rendered.malformedDelivery).toBe(true);
  });

  test("renderInbound (RecoverableDispatchDeliveryFailureSchema): a malformed targeted message's body is its own", () => {
    const rendered = renderInbound(targetedMessageBodyInProto(["created_at"]), reader);

    expect(rendered.rejectedDelivery?.body).toBe("");
  });

  test("subscriptionRemovedTopics (DispatchEventSchema's payload): the session and topics are the ones the payload names", () => {
    const raw = dispatchFrame({
      issue_key: "DSP-1",
      type: "subscription.removed",
      actor: { kind: "user", id: "alice" },
      payload: withProtoKey(
        { by: { kind: "user", id: "alice" } },
        { session_id: "ses_target", topics: ["notifications.dispatch.issue.DSP-1.>"] }
      ),
    });

    expect(subscriptionRemovedTopics(raw, "ses_target")).toBeUndefined();
  });
});

describe("an inbound frame's text is read as well-formed Unicode", () => {
  test("renderInbound: the reported frame renders, half a surrogate pair shown as U+FFFD", () => {
    const card = decode(renderInbound(String.raw`{"payload":"\ud800"}`, reader).content) as {
      envoy: { message?: unknown };
    };

    expect(card.envoy.message).toBe("\uFFFD");
  });

  test("renderInbound: a message's JSON renders, such a half in a key or a value shown as U+FFFD", () => {
    // The later of two keys that read alike wins, as JSON.parse keeps the later of two keys
    // spelled alike.
    const raw = JSON.stringify({
      event_id: "event-1",
      source: "agent",
      payload: String.raw`{"text":"a\ud800b","\udc00":1,"\ufffd":2}`,
    });
    const card = decode(renderInbound(raw, reader).content) as { envoy: { message?: unknown } };

    expect(card.envoy.message).toEqual({ text: "a\uFFFDb", "\uFFFD": 2 });
  });

  test("renderInbound (a Dispatch event's payload): a comment whose body holds such a half renders", () => {
    const raw = JSON.stringify({
      event_id: "dispatch-1",
      source: "dispatch",
      payload: String.raw`{"issue_key":"DSP-1","type":"comment.created","actor":{"kind":"user","id":"alice"},"notify":true,"payload":{"id":"comment-1","body":"a\ud800b"}}`,
    });
    const rendered = renderInbound(
      raw,
      reader,
      legacyDispatchIssueSubject("DSP-1", "comment.created")
    );

    expect(rendered.content).toContain("a\uFFFDb");
  });

  test("renderInbound: a whole surrogate pair is the character it spells", () => {
    const card = decode(
      renderInbound(String.raw`{"source":"agent","payload":"\ud83d\ude00"}`, reader).content
    ) as { envoy: { message?: unknown } };

    expect(card.envoy.message).toBe("\u{1F600}");
  });
});

describe("an inbound frame is read at every depth JSON.parse reads", () => {
  // JSON.parse reads a value nested past 9,999 levels; a reviver, which Bun calls recursively,
  // gave up near 4,500 levels, and the frame then rendered as `payload was not JSON`.
  test("renderInbound: a frame holding a value nested 9,999 deep renders as it does without it", () => {
    const fields = `"event_id":"event-1","source":"agent","payload":"hello"`;
    const deep = `${"[".repeat(9_999)}${"]".repeat(9_999)}`;

    expect(renderInbound(`{${fields},"deep":${deep}}`, reader).content).toBe(
      renderInbound(`{${fields}}`, reader).content
    );
  });
});
