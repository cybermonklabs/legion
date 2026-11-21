import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  type Envelope,
  isLegionRole,
  legacyDispatchIssueSubject,
  legionControllerNoticeSubject,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { decode, encode } from "@toon-format/toon";
import { bearerCredential } from "../bearer";
import { type DispatchDelivery, postDeliveryReply, renderInbound, senderLabel } from "../delivery";
import { type ReadBlock, splitWordsBlocks } from "../words-block";

const reader = "01a01111-2222-7333-4444-555555555555";
const actor = { kind: "session", id: "session-1" };

/** The Dispatch bearer a delivery reply is posted under. */
const REPLY_TOKEN = fixtureSecret("dsp");

const targetedDispatchPayload = readFileSync(
  new URL("../../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
  "utf8"
);

const targetedMessageID = "11111111-1111-4111-8111-111111111111";
const targetedCommentID = "22222222-2222-4222-8222-222222222222";
const targetedCommentDeliveryID = "33333333-3333-4333-8333-333333333333";

function envelope(overrides: Partial<Envelope> = {}): Envelope {
  return {
    event_id: "event-1",
    source: "agent",
    source_event_id: "agent.ses_sender.event-1",
    topic: "notifications.agent.ses_target",
    dedupe_key: "agent.ses_target.event-1",
    issued_at: 1,
    payload_summary: "message",
    trace_id: "trace-1",
    ...overrides,
  };
}

/** A card's fields, as TOON's `decode` reads them once its blocks are split off, and its blocks. */
function cardOf<Envoy = Record<string, unknown>>(
  content: string
): { readonly envoy: Envoy; readonly blocks: readonly ReadBlock[] } {
  const { head, blocks } = splitWordsBlocks(content);
  // Every card these tests render opens with `envoy:`, and each test names the fields it reads.
  const decoded = decode(head) as { readonly envoy: Envoy };
  return { envoy: decoded.envoy, blocks };
}

// `quiet` left out is a frame published before Dispatch stated it, which a reader judges by
// `notify` instead. `createdAt` is when the event happened: the event's `created_at`.
function dispatchEvent(
  type: string,
  payload: object,
  eventActor = actor,
  {
    notify = true,
    quiet,
    createdAt = "2026-09-09T00:00:00Z",
  }: { notify?: boolean; quiet?: boolean; createdAt?: string } = {}
): string {
  return JSON.stringify(
    envelope({
      event_id: "dispatch-1",
      source: "dispatch",
      source_event_id: "1",
      topic: legacyDispatchIssueSubject("DSP-1", ">"),
      payload_summary: "Dispatch update",
      payload: JSON.stringify({
        id: 1,
        issue_key: "DSP-1",
        seq: 7,
        type,
        actor: eventActor,
        notify,
        ...(quiet === undefined ? {} : { quiet }),
        created_at: createdAt,
        payload,
      }),
    })
  );
}

const issue = { title: "Native Dispatch", status: "in_progress", route: "role:legion-controller" };
const apiQuestion = "The service needs a public API. Which approach should we use?";
const transportQuestion = "The service needs one transport. Which approach should we use?";
const publishQuestion = "The document is ready for readers. How should we publish it?";
const draftQuestion = "The document needs a publication decision. How should we proceed?";
const releaseQuestion = "The release is ready. How should we proceed?";

const openAsk = {
  id: "ask-1",
  issue_key: "DSP-1",
  author: actor,
  question: apiQuestion,
  options: [{ label: "JSON API" }, { label: "MCP API" }],
  multiple: false,
  urgency: "med",
  opened_event_id: 7,
  anchor: {
    artifact_id: "artifact-1",
    block_id: "block-1",
    mark_id: "mark-1",
    quote: apiQuestion,
    orphaned: false,
  },
  state: "open",
  answer: null,
  created_at: "2026-09-09T00:00:00Z",
};
const answeredAsk = {
  ...openAsk,
  state: "answered",
  answer: {
    user: { kind: "user", id: "sami" },
    selected: ["JSON"],
    text: "Use JSON HTTP.",
    at: "2026-09-09T00:01:00Z",
  },
};
const comment = {
  id: "comment-1",
  issue_key: "DSP-1",
  author: actor,
  body: "Please update this.",
  anchor: { artifact_id: "artifact-1", mark_id: "m-1", quote: "old line", orphaned: false },
  reply_to: "comment-0",
  resolved: false,
  suggestion: null,
  created_at: "2026-09-09T00:00:00Z",
  artifact_name: "spec.md",
};

const targetedComment = {
  ...comment,
  id: targetedCommentID,
  artifact_id: "artifact-1",
  deliveries: [],
  mentions: [],
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  ask_id: null,
  project_key: "CORE",
  artifact_slug: "spec",
  suppress_route: false,
  suppressed_authors: [],
};

function targetedCommentFrame(
  payload: Record<string, unknown> = targetedComment,
  delivery: Partial<{
    readonly attempt: number;
    readonly mode: "aside" | "btw" | "steer";
    readonly comment_id: string;
    readonly target: string;
  }> = {}
): string {
  return JSON.stringify({
    event: {
      id: 8,
      issue_key: "CORE-1",
      seq: 8,
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      notify: true,
      created_at: "2026-09-12T00:00:00Z",
      payload,
    },
    delivery: {
      attempt: 1,
      mode: "aside",
      comment_id: targetedCommentDeliveryID,
      target: "session:ses_target",
      ...delivery,
    },
  });
}

describe("inbound delivery policy", () => {
  test("labels agent, human, and other envelopes by session before source", () => {
    expect(senderLabel(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      "ses_agent"
    );
    expect(senderLabel(envelope({ source: "human", source_session: "ses_human" }))).toBe(
      "ses_human"
    );
    expect(senderLabel(envelope({ source: "github" }))).toBe("github");
    expect(senderLabel(envelope({ source: "agent" }))).toBe("agent");
    expect(senderLabel(envelope({ source: "human" }))).toBe("human");
  });
});

describe("renderInbound dispatch events", () => {
  test("renders ask.answered as the answer alone: no ask row, no restated owner", () => {
    const rendered = renderInbound(dispatchEvent("ask.answered", answeredAsk), reader);
    const decoded = cardOf(rendered.content);

    // The asker wrote the question and `re:` names the ask, so the frame carries what moved
    // (who answered, which question, the answer) and never the ask row again. The options the
    // answer chose are a field; the words the person typed are a block, by them, at the answer.
    expect(decoded.envoy).toEqual({
      from: "dispatch",
      at: "1970-01-01T00:00:00Z",
      id: "dispatch-1",
      dispatch: {
        owner: "DSP-1",
        type: "ask.answered",
        actor: { kind: "session", id: "session-1" },
        ask: "dispatch://DSP-1/ask/ask-1",
        question: apiQuestion,
        selected: ["JSON"],
      },
    });
    expect(decoded.blocks).toEqual([
      {
        tag: "answer",
        name: "answer",
        text: "Use JSON HTTP.",
        by: "user:sami",
        at: "2026-09-09T00:01:00Z",
      },
    ]);
    for (const restated of ["payload:", "issue_key:", "opened_event_id", "options", "anchor"]) {
      expect(rendered.content).not.toContain(restated);
    }
  });

  test("renders a changed answer with the answer it replaced", () => {
    const rendered = renderInbound(
      dispatchEvent("ask.answered", {
        ...answeredAsk,
        answer: { ...answeredAsk.answer, selected: ["MCP"], text: null },
        previous_answer: { ...answeredAsk.answer, user: "sami" },
      }),
      reader
    );
    const decoded = cardOf(rendered.content);

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.answered",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: apiQuestion,
      selected: ["MCP"],
      previous_selected: ["JSON"],
    });
    expect(decoded.blocks).toEqual([
      {
        tag: "previous_answer",
        name: "previous_answer",
        text: "Use JSON HTTP.",
        by: "user:sami",
        at: "2026-09-09T00:01:00Z",
      },
    ]);
  });

  test("preserves an anchor document in delivered ask events", () => {
    const anchorArtifact = { name: "Spec", primary: true, project: "DSP", slug: "spec" };
    const rendered = renderInbound(
      dispatchEvent("ask.opened", { ...openAsk, anchor_artifact: anchorArtifact }),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.opened",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: apiQuestion,
      options: ["JSON API", "MCP API"],
      quote: apiQuestion,
      document: "DSP/spec",
    });
  });

  test("still heads a retained ask.answered envelope that carries the removed action kind", () => {
    // JetStream keeps 72 h of ask.* envelopes written before the action kind was folded into
    // questions; the inbound decoder must not drop their question/answer lines over a stale kind.
    const retained = {
      ...answeredAsk,
      kind: "action",
      options: [{ label: "Done" }, { label: "Can't" }],
      answer: { ...answeredAsk.answer, selected: ["Can't"], text: "No access." },
    };
    const rendered = renderInbound(dispatchEvent("ask.answered", retained), reader);
    const decoded = cardOf<{ dispatch: Record<string, unknown> }>(rendered.content);

    expect(decoded.envoy.dispatch).toMatchObject({
      type: "ask.answered",
      question: apiQuestion,
      selected: ["Can't"],
    });
    expect(decoded.blocks.map((block) => [block.name, block.text])).toEqual([
      ["answer", "No access."],
    ]);
  });

  test("shows the ask's ref as 're:' when an answered ask is correlated to its ask id", () => {
    const correlated = JSON.parse(dispatchEvent("ask.answered", answeredAsk)) as Record<
      string,
      unknown
    >;
    const rendered = renderInbound(JSON.stringify({ ...correlated, in_reply_to: "ask-1" }), reader);
    const decoded = cardOf(rendered.content);
    expect(decoded.envoy.re).toBe("dispatch://DSP-1/ask/ask-1");
    expect(rendered.content).not.toContain(`re: ${apiQuestion}`);
    // Correlated: the ask is named once, by `re:`, never again inside the record.
    expect(decoded.envoy.dispatch).not.toHaveProperty("ask");
  });

  test("an uncorrelated ask event still names its ask, so two asks never render alike", () => {
    // The producer correlates answers and replies (in_reply_to) but not openings or edits;
    // without the ref the receiving session could not tell one new ask from another.
    const first = decode(renderInbound(dispatchEvent("ask.opened", openAsk), reader).content) as {
      envoy: { re?: string; dispatch: Record<string, unknown> };
    };
    const second = decode(
      renderInbound(dispatchEvent("ask.opened", { ...openAsk, id: "ask-2" }), reader).content
    ) as { envoy: { dispatch: Record<string, unknown> } };

    expect(first.envoy.re).toBeUndefined();
    expect(first.envoy.dispatch.ask).toBe("dispatch://DSP-1/ask/ask-1");
    expect(second.envoy.dispatch.ask).toBe("dispatch://DSP-1/ask/ask-2");
    expect(first.envoy.dispatch).not.toEqual(second.envoy.dispatch);
  });

  test("does not offer a reply hint for an ask.opened event", () => {
    const decoded = decode(renderInbound(dispatchEvent("ask.opened", openAsk), reader).content) as {
      envoy: Record<string, unknown>;
    };

    expect(decoded.envoy.reply_with).toBeUndefined();
  });

  test("renders a targeted Dispatch message with its reply path", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: targetedDispatchPayload,
        })
      ),
      reader
    );
    const decoded = cardOf<Record<string, unknown> & { dispatch: { payload: object } }>(
      rendered.content
    );

    // The message's words are a block of their own, at the event's time, since the message
    // payload carries none; the actor who wrote them is the card's `actor`.
    expect(decoded.envoy.dispatch).toMatchObject({
      owner: "CORE-1",
      type: "message.created",
      actor: { kind: "user", id: "alice" },
      payload: { id: targetedMessageID, target: "session:ses_target" },
    });
    expect(decoded.envoy.dispatch.payload).not.toHaveProperty("body");
    expect(decoded.envoy.dispatch.payload).not.toHaveProperty("author");
    expect(decoded.blocks).toEqual([
      { tag: "body", name: "body", text: "Can this ship?", at: "0001-01-01T00:00:00Z" },
    ]);
    expect(decoded.envoy.dispatch).not.toHaveProperty("issue_key");
    expect(decoded.envoy.reply_with).toEqual({
      command: `dispatch message --issue CORE-1 --body ... --in-reply-to ${targetedMessageID}`,
    });
    expect(rendered.delivery).toEqual({
      resource: "message",
      id: targetedMessageID,
      attempt: 1,
      mode: "btw",
      replyPath: `/api/v1/messages/${targetedMessageID}/reply`,
      replyFields: {},
      issueKey: "CORE-1",
      body: "Can this ship?",
    });
  });

  // A host reads the type, beside the delivery, to tell the card of an event from the message or
  // mention Dispatch delivers to the session and from an ask's card (pi-envoy's `lowCard`).
  test("names the type of the Dispatch event it renders, and none for a frame that is no event", () => {
    const commented = renderInbound(dispatchEvent("comment.created", comment), reader);
    const answered = renderInbound(dispatchEvent("ask.answered", answeredAsk), reader);
    const targeted = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: targetedDispatchPayload })),
      reader
    );
    const unparsed = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: "not json" })),
      reader
    );
    const agent = renderInbound(JSON.stringify(envelope()), reader);

    expect([commented, answered, targeted].map((rendered) => rendered.dispatchType)).toEqual([
      "comment.created",
      "ask.answered",
      "message.created",
    ]);
    expect(commented.delivery).toBeUndefined();
    expect(targeted.delivery).toBeDefined();
    expect(unparsed.dispatchType).toBeUndefined();
    expect(agent.dispatchType).toBeUndefined();
  });

  test("decodes a targeted comment with its comment reply address and hint", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: targetedCommentFrame(),
        })
      ),
      reader
    );
    const decoded = cardOf(rendered.content);

    expect(rendered.malformedDelivery).toBeUndefined();
    expect(rendered.delivery).toEqual({
      resource: "comment",
      id: targetedCommentDeliveryID,
      attempt: 1,
      mode: "aside",
      replyPath: `/api/v1/comments/${targetedCommentDeliveryID}/reply`,
      replyFields: { target: "session:ses_target" },
      issueKey: "CORE-1",
      body: "Please update this.",
    });
    expect(decoded.envoy.reply_with).toEqual({
      command: `dispatch comment --issue CORE-1 --body ... --reply-to ${targetedCommentID}`,
    });
  });

  test("reports malformed targeted comments through their delivery-derived reply address", async () => {
    const replies: Array<{ readonly path: string; readonly body: unknown }> = [];
    const server = Bun.serve({
      port: 0,
      fetch: async (request) => {
        replies.push({ path: new URL(request.url).pathname, body: await request.json() });
        return Response.json({});
      },
    });
    const { created_at: _createdAt, ...malformedPayload } = targetedComment;

    try {
      const rendered = renderInbound(
        JSON.stringify(
          envelope({
            source: "dispatch",
            payload: targetedCommentFrame({
              ...malformedPayload,
              id: "comment-payload-1",
            }),
          })
        ),
        reader
      );
      const rejected = rendered.rejectedDelivery;
      if (rejected === undefined) throw new Error("missing recoverable rejected delivery");

      await postDeliveryReply(
        { url: `http://127.0.0.1:${server.port}`, credential: bearerCredential(REPLY_TOKEN) },
        reader,
        rejected,
        { error: "Invalid Dispatch targeted delivery frame" }
      );

      expect(replies).toEqual([
        {
          path: `/api/v1/comments/${targetedCommentDeliveryID}/reply`,
          body: {
            actor: { kind: "session", id: reader },
            attempt: 1,
            target: "session:ses_target",
            error: "Invalid Dispatch targeted delivery frame",
          },
        },
      ]);
    } finally {
      server.stop(true);
    }
  });

  test("rejects resource-crossing identifiers before targeted frames can issue replies", async () => {
    const paths: string[] = [];
    const server = Bun.serve({
      port: 0,
      fetch: (request) => {
        paths.push(new URL(request.url).pathname);
        return Response.json({});
      },
    });
    const message = JSON.parse(targetedDispatchPayload) as {
      event: { payload: { id: string } };
    };
    message.event.payload.id = "../comments/comment-1";
    const frames = [
      targetedCommentFrame(targetedComment, { comment_id: "../messages/message-1" }),
      JSON.stringify(message),
    ];

    try {
      for (const payload of frames) {
        const rendered = renderInbound(
          JSON.stringify(envelope({ source: "dispatch", payload })),
          reader
        );
        if (rendered.delivery !== undefined) {
          await postDeliveryReply(
            {
              url: `http://127.0.0.1:${server.port}`,
              credential: bearerCredential(REPLY_TOKEN),
            },
            reader,
            rendered.delivery,
            { error: "Invalid Dispatch targeted delivery frame" }
          );
        }
      }

      expect(paths).toEqual([]);
    } finally {
      server.stop(true);
    }
  });

  test("constructs encoded reply paths from the declared delivery resource", async () => {
    const paths: string[] = [];
    const server = Bun.serve({
      port: 0,
      fetch: (request) => {
        paths.push(new URL(request.url).pathname);
        return Response.json({});
      },
    });
    const deliveries: DispatchDelivery[] = [
      {
        resource: "message",
        id: "../comments/comment-1",
        attempt: 1,
        mode: "aside",
        replyPath: "/api/v1/comments/comment-1/reply",
        replyFields: {},
        issueKey: "CORE-1",
        body: "message",
      },
      {
        resource: "comment",
        id: "../messages/message-1",
        attempt: 1,
        mode: "aside",
        replyPath: "/api/v1/messages/message-1/reply",
        replyFields: { target: "session:ses_target" },
        issueKey: "CORE-1",
        body: "comment",
      },
    ];

    try {
      for (const delivery of deliveries) {
        await postDeliveryReply(
          {
            url: `http://127.0.0.1:${server.port}`,
            credential: bearerCredential(REPLY_TOKEN),
          },
          reader,
          delivery,
          { error: "Invalid Dispatch targeted delivery frame" }
        );
      }

      expect(paths).toEqual([
        "/api/v1/messages/..%2Fcomments%2Fcomment-1/reply",
        "/api/v1/comments/..%2Fmessages%2Fmessage-1/reply",
      ]);
    } finally {
      server.stop(true);
    }
  });

  // The reply settles the delivery attempt, so it is a write like any other: one key for the
  // reply, asked again under it when Dispatch is briefly down, and Dispatch's refusal carried as
  // its own error.
  test("posts a delivery reply as one keyed write, asked again under its key", async () => {
    const keys: Array<string | null> = [];
    let answers = 0;
    const server = Bun.serve({
      port: 0,
      fetch: (request) => {
        const key = request.headers.get("Idempotency-Key");
        keys.push(key);
        answers += 1;
        // The stand-in sends the key back as Dispatch's keyed layer does. The client asks again
        // under the same key whatever the answer carries, and reads the echo only to settle a
        // fresh 4xx refusal that echoes exactly its key.
        const headers: Record<string, string> = key === null ? {} : { "Idempotency-Key": key };
        if (answers === 1)
          return Response.json({ code: "UNAVAILABLE", error: "down" }, { status: 503, headers });
        if (answers === 2) return Response.json({ reply: { id: "reply-1" } }, { headers });
        return Response.json(
          { code: "ATTEMPT_ANSWERED", error: "already answered" },
          { status: 409, headers }
        );
      },
    });
    const delivery: DispatchDelivery = {
      resource: "message",
      id: "message-1",
      attempt: 2,
      mode: "btw",
      replyPath: "/api/v1/messages/message-1/reply",
      replyFields: {},
      issueKey: null,
      body: "status?",
    };
    const config = {
      url: `http://127.0.0.1:${server.port}`,
      credential: bearerCredential(REPLY_TOKEN),
    };

    try {
      await postDeliveryReply(config, reader, delivery, { body: "green" });
      expect(keys).toHaveLength(2);
      expect(keys[0]).toBeString();
      expect(keys[1]).toBe(keys[0] as string);

      await expect(postDeliveryReply(config, reader, delivery, { body: "again" })).rejects.toThrow(
        expect.objectContaining({ code: "ATTEMPT_ANSWERED", status: 409 })
      );
      expect(keys).toHaveLength(3);
      expect(keys[2]).not.toBe(keys[0] as string);
    } finally {
      server.stop(true);
    }
  });

  test("renders an issue-less targeted BTW with a reply address and an issue-less reply hint", () => {
    const frame = JSON.parse(targetedDispatchPayload) as {
      event: { issue_key: string | null; payload: { issue_key: string | null; target: string } };
    };
    frame.event.issue_key = null;
    frame.event.payload.issue_key = null;
    frame.event.payload.target = `session:${reader}`;
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          topic: `notifications.agent.${reader}`,
          payload: JSON.stringify(frame),
        })
      ),
      reader,
      `notifications.agent.${reader}`
    );
    const decoded = cardOf(rendered.content);

    expect(rendered.delivery).toMatchObject({
      resource: "message",
      id: targetedMessageID,
      attempt: 1,
      mode: "btw",
      replyPath: `/api/v1/messages/${targetedMessageID}/reply`,
      replyFields: {},
      issueKey: null,
    });
    // The conversation has no issue, so the hint names only the message the answer threads
    // under; `dispatch message` takes that shape and replies through the delivery route.
    expect(decoded.envoy.reply_with).toEqual({
      command: `dispatch message --body ... --in-reply-to ${targetedMessageID}`,
    });
  });

  test("marks only the matching shared-prefix session as an issue-less message recipient", () => {
    const otherSession = "01a02222-3333-7444-8555-666666666666";
    const frame = JSON.parse(targetedDispatchPayload) as {
      event: { issue_key: string | null; payload: { issue_key: string | null; target: string } };
    };
    frame.event.issue_key = null;
    frame.event.payload.issue_key = null;
    frame.event.payload.target = `session:${reader}`;
    const raw = JSON.stringify(
      envelope({
        source: "dispatch",
        topic: `notifications.agent.${reader}`,
        payload: JSON.stringify(frame),
      })
    );

    const intended = cardOf<{ to?: string; dispatch: { payload: { target: string } } }>(
      renderInbound(raw, reader, `notifications.agent.${reader}`).content
    );
    const other = cardOf<{ to?: string; dispatch: { payload: { target: string } } }>(
      renderInbound(raw, otherSession, `notifications.agent.${reader}`).content
    );

    expect(intended.envoy.to).toBe("you");
    expect(intended.envoy.dispatch.payload.target).toBe(
      "you (session:01a01111-2222-7333-4444-555555555555)"
    );
    expect(other.envoy.to).toBeUndefined();
    expect(other.envoy.dispatch.payload.target).toBe(
      "session:01a01111-2222-7333-4444-555555555555"
    );
  });

  // A session's inbox has two forms while every reader takes both (dispatch://INFRA-3862): the
  // session alone, and the session under a project (`_` for none). A message on either form is
  // this session's own, so its card names it the recipient; another session's inbox is not, in
  // either form.
  test("names the session the recipient of a message on its inbox in either form", () => {
    const otherSession = "01a02222-3333-7444-8555-666666666666";
    for (const topic of [
      `notifications.agent.${reader}`,
      `notifications.agent._.${reader}`,
      `notifications.agent.PRIV.${reader}`,
    ]) {
      const raw = JSON.stringify(envelope({ source: "agent", topic, payload: "hello" }));
      const intended = cardOf<{ to?: string }>(renderInbound(raw, reader, topic).content);
      const other = cardOf<{ to?: string }>(renderInbound(raw, otherSession, topic).content);

      expect(intended.envoy.to, topic).toBe("you");
      expect(other.envoy.to, topic).toBeUndefined();
    }
  });

  test("keeps rendering a targeted Dispatch message when its payload grows", () => {
    const extendedPayload = JSON.parse(targetedDispatchPayload) as {
      event: { payload: Record<string, unknown> };
    };
    extendedPayload.event.payload.future_field = "added by a newer Dispatch";
    const unchanged = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: targetedDispatchPayload })),
      reader
    );
    const rendered = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: JSON.stringify(extendedPayload) })),
      reader
    );

    expect(rendered.malformedDelivery).toBeUndefined();
    expect(rendered.content).toBe(unchanged.content);
    expect(rendered.delivery).toEqual(unchanged.delivery);
  });

  test("renders ask.answered's free-text answer even with no selected option", () => {
    const textOnlyAnswered = {
      ...openAsk,
      state: "answered",
      answer: {
        user: { kind: "user", id: "sami" },
        selected: [],
        text: "Neither; let's do a third thing.",
        at: "2026-09-09T00:01:00Z",
      },
    };
    const rendered = renderInbound(dispatchEvent("ask.answered", textOnlyAnswered), reader);
    const decoded = cardOf(rendered.content);

    expect(decoded.envoy).toEqual({
      from: "dispatch",
      at: "1970-01-01T00:00:00Z",
      id: "dispatch-1",
      dispatch: {
        owner: "DSP-1",
        type: "ask.answered",
        actor: { kind: "session", id: "session-1" },
        ask: "dispatch://DSP-1/ask/ask-1",
        question: apiQuestion,
      },
    });
    expect(decoded.blocks.map((block) => [block.name, block.text])).toEqual([
      ["answer", "Neither; let's do a third thing."],
    ]);
  });

  test("does not flag the asker's session as foreign when the answer reaches another follower", () => {
    const asker = "01a0aaaa-bbbb-7ccc-dddd-0123456789ab";
    const answeredByAnotherFollower = {
      ...answeredAsk,
      author: { kind: "session", id: asker },
    };
    const rendered = renderInbound(
      dispatchEvent("ask.answered", answeredByAnotherFollower, { kind: "user", id: "sami" }),
      reader
    );

    expect(rendered.skip).toBe(false);
    expect(rendered.content).not.toContain("note:");
  });

  test("renders ask.resolved as a low-key Dispatch update", () => {
    const resolvedAsk = {
      ...openAsk,
      resolution: {
        actor: { kind: "session", id: "session-1" },
        at: "2026-09-10T00:01:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
      state: "resolved",
    };

    const decoded = cardOf<{ dispatch: Record<string, unknown> }>(
      renderInbound(dispatchEvent("ask.resolved", resolvedAsk), reader).content
    );

    // The resolver is the event's actor, so the reason's block names only the time.
    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.resolved",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: apiQuestion,
      resolved: "retracted",
    });
    expect(decoded.blocks).toEqual([
      {
        tag: "reason",
        name: "reason",
        text: "A newer question supersedes this one.",
        at: "2026-09-10T00:01:00Z",
      },
    ]);
  });

  // `kind` is an open string now, so a kind this build has never seen parses rather than costing
  // the reader the card, and the words Dispatch wrote stand beside the person's own note on
  // every reader of the frame: this one, as a field of their own outside the note's block, and a
  // build older than this change, which declares no `dispatch_words` of its own and prints the
  // payload as it came.
  test("a resolution kind this build does not know still reads the words Dispatch wrote", () => {
    const note = "Whoever builds it decides.";
    const dispatchWords =
      "The person chose not to answer this question. Decide the next step without an answer. " +
      "Do not ask this question again unless the facts change. Write in your work what you assumed.";
    const declined = {
      ...openAsk,
      state: "resolved",
      resolution: {
        actor: { kind: "user", id: "alice@example.com" },
        at: "2026-09-10T00:01:00Z",
        kind: "declined",
        reason: note,
        dispatch_words: dispatchWords,
      },
    };
    const card = (payload: object) =>
      cardOf<{ dispatch: Record<string, unknown> }>(
        renderInbound(dispatchEvent("ask.resolved", payload), reader).content
      );

    // The reader that cannot parse the frame: `AskEventPayloadSchema` declares `edited_by` as
    // `z.never().optional()`, so a payload carrying that key is refused whatever its resolution.
    // Its card carries the raw payload, which is how a build older than this change reads both
    // parts of a decline.
    expect(card({ ...declined, edited_by: actor }).envoy.dispatch).toMatchObject({
      payload: { resolution: { kind: "declined", reason: note, dispatch_words: dispatchWords } },
    });

    const read = card(declined);
    expect(read.envoy.dispatch).toMatchObject({
      resolved: "declined",
      dispatch_words: dispatchWords,
    });
    expect(read.blocks).toEqual([
      {
        tag: "reason",
        name: "reason",
        text: note,
        by: "user:alice@example.com",
        at: "2026-09-10T00:01:00Z",
      },
    ]);

    // A kind no build has ever seen reads as it came, and a resolution the person wrote no
    // reason for draws no block.
    const unknownKind = card({
      ...declined,
      resolution: { ...declined.resolution, kind: "handed_to_the_council", reason: "" },
    });
    expect(unknownKind.envoy.dispatch).toMatchObject({
      resolved: "handed_to_the_council",
      dispatch_words: dispatchWords,
    });
    expect(unknownKind.blocks).toEqual([]);
  });

  test("renders ask.edited with its current question and edit history", () => {
    const decoded = decode(
      renderInbound(
        dispatchEvent("ask.edited", {
          ...openAsk,
          question: transportQuestion,
          options: [{ label: "REST transport" }, { label: "gRPC transport" }],
          multiple: true,
          urgency: "high",
          edited_at: "2026-09-11T03:26:00Z",
          previous: {
            question: apiQuestion,
            options: [{ label: "JSON API" }, { label: "MCP API" }],
            multiple: false,
            urgency: "med",
          },
          edited_by: actor,
        }),
        reader
      ).content
    ) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.edited",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: transportQuestion,
      options: ["REST transport", "gRPC transport"],
      quote: apiQuestion,
      previous: apiQuestion,
    });
  });
  test("renders an artifact-owned ask edit by project document", () => {
    const capturedArtifactAskEdit = JSON.stringify(
      envelope({
        event_id: "dispatch-72",
        source: "dispatch",
        source_event_id: "72",
        topic: "notifications.dispatch.document.CORE.design-notes.ask.edited",
        payload_summary: "CORE/design-notes ask edited",
        payload: JSON.stringify({
          id: 72,
          issue_key: null,
          artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          project: "CORE",
          seq: 4,
          type: "ask.edited",
          actor,
          notify: true,
          created_at: "2026-09-11T04:05:29Z",
          payload: {
            ...openAsk,
            issue_key: null,
            artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
            question: publishQuestion,
            edited_at: "2026-09-11T04:05:29Z",
            previous: {
              question: draftQuestion,
              options: [{ label: "Publish for readers" }],
              multiple: false,
              urgency: "med",
            },
            edited_by: actor,
          },
        }),
      })
    );

    const decoded = decode(renderInbound(capturedArtifactAskEdit, reader).content) as {
      envoy: { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.dispatch).toMatchObject({
      owner: "CORE / design-notes",
      type: "ask.edited",
      ask: "dispatch://CORE/artifact/design-notes/ask/ask-1",
      question: publishQuestion,
      previous: draftQuestion,
    });
  });

  test("skips a non-notify document frame", () => {
    const raw = JSON.stringify(
      envelope({
        source: "dispatch",
        topic: "notifications.dispatch.document.CORE.runbook-md.artifact.version",
        payload: JSON.stringify({
          id: 72,
          issue_key: null,
          artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          project: "CORE",
          seq: 4,
          type: "artifact.version",
          actor,
          notify: false,
          created_at: "2026-09-11T04:05:29Z",
          payload: {},
        }),
      })
    );

    expect(renderInbound(raw, reader)).toMatchObject({ skip: true, content: "" });
  });

  test("renders a person's spec edit as its versions and editor, then the diff in a fenced block", () => {
    const edit = {
      artifact_id: "artifact-1",
      name: "spec.md",
      from_version: 4,
      to_version: 7,
      editors: ["person@example.com"],
      diff: "--- previous\n+++ current\n@@ -1 +1 @@\n-The export runs at 02:00 UTC.\n+The export runs at 04:00 UTC.\n",
      diff_truncated: false,
    };
    const person = { kind: "user", id: "person@example.com" };
    const whole = renderInbound(dispatchEvent("artifact.human_edited", edit, person), reader);
    const [card, block] = whole.content.split("\n\n```diff\n");
    expect(decode(card ?? "")).toMatchObject({
      envoy: {
        dispatch: {
          owner: "DSP-1",
          type: "artifact.human_edited",
          edit: "spec.md v4 to v7, edited by person@example.com",
        },
      },
    });
    expect(card).not.toContain("02:00");
    expect(block).toBe(`${edit.diff}\`\`\``);

    const cut = renderInbound(
      dispatchEvent("artifact.human_edited", { ...edit, diff_truncated: true }, person),
      reader
    );
    expect(cut.content.endsWith(`${edit.diff}\`\`\`\n(diff cut at 4,096 bytes)`)).toBe(true);
  });

  test("renders subscription.removed as a short plain-text notice, not the generic TOON envelope", () => {
    const rendered = renderInbound(
      dispatchEvent("subscription.removed", {
        session_id: reader,
        by: { kind: "user", id: "alice" },
        topics: ["notifications.dispatch.issue.DSP-1.>"],
      }),
      reader
    );

    expect(rendered).toMatchObject({ skip: false, content: "Unsubscribed from DSP-1 by alice" });
  });

  test("falls back to 'someone' when subscription.removed carries no actor", () => {
    const rendered = renderInbound(
      dispatchEvent("subscription.removed", { session_id: reader, topics: [] }),
      reader
    );

    expect(rendered.content).toBe("Unsubscribed from DSP-1 by someone");
  });

  test("renders nothing for a subscription.removed naming a different session — this event also reaches the issue's own topic, so every other subscriber sees nothing", () => {
    const rendered = renderInbound(
      dispatchEvent("subscription.removed", {
        session_id: "ses_someone_else",
        by: { kind: "user", id: "alice" },
        topics: ["notifications.dispatch.issue.DSP-1.>"],
      }),
      reader
    );

    expect(rendered).toMatchObject({ skip: true, content: "" });
  });

  test("renders ask.follower_added and ask.follower_removed as short notices for the named session only", () => {
    const human = { kind: "user", id: "alice" };
    expect(
      renderInbound(
        dispatchEvent(
          "ask.follower_added",
          { ask_id: "ask-1", session_id: reader, by: human },
          human
        ),
        reader
      )
    ).toMatchObject({
      skip: false,
      content:
        "Now following ask ask-1 on DSP-1 (added by alice): its answer and replies reach you directly; dispatch follow --ask ask-1 --action unfollow to stop.",
    });
    expect(
      renderInbound(
        dispatchEvent(
          "ask.follower_removed",
          { ask_id: "ask-1", session_id: reader, by: human },
          human
        ),
        reader
      )
    ).toMatchObject({
      skip: false,
      content: "No longer following ask ask-1 on DSP-1 (removed by alice).",
    });
    expect(
      renderInbound(
        dispatchEvent(
          "ask.follower_removed",
          { ask_id: "ask-1", session_id: "ses_someone_else", by: human },
          human
        ),
        reader
      )
    ).toMatchObject({ skip: true, content: "" });
  });

  test("renders a comment.created reply to an ask as 're: <ask ref>' with the question head under dispatch", () => {
    const askID = "ask-1";
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-2",
        source: "dispatch",
        source_event_id: "2",
        topic: "notifications.agent.session-asker",
        payload_summary: "DSP-1 comment created",
        in_reply_to: askID,
        payload: JSON.stringify({
          id: 2,
          issue_key: "DSP-1",
          seq: 8,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: { ...comment, ask_id: askID, ask_question: apiQuestion },
        }),
      })
    );

    const decoded = cardOf<Record<string, unknown> & { dispatch: Record<string, unknown> }>(
      renderInbound(raw, reader).content
    );

    expect(decoded.envoy.re).toBe("dispatch://DSP-1/ask/ask-1");
    expect(decoded.envoy.dispatch.question).toBe(apiQuestion);
    expect(decoded.envoy.reply_with).toEqual({
      command: "dispatch comment --issue DSP-1 --body ... --reply-to-ask ask-1",
    });
  });

  test("gives ordinary comment events a reply_to hint", () => {
    const rendered = renderInbound(
      dispatchEvent("comment.created", { ...comment, ask_id: null }),
      reader
    );
    const decoded = cardOf(rendered.content);

    expect(rendered.delivery).toBeUndefined();

    expect(decoded.envoy.reply_with).toEqual({
      command: "dispatch comment --issue DSP-1 --body ... --reply-to comment-1",
    });
  });

  // The hint a card drops under `expects_reply: false` is a Dispatch frame's own reply command,
  // and the mention below is the one Dispatch frame its producer gives that value: a comment
  // joining no ask thread and answering no comment (`commentExpectsReply`,
  // packages/envoy/internal/dispatch/api/comment_delivery.go).
  describe("the reply command on a mention whose sender states the reply it wants", () => {
    const mention = { ...targetedComment, ask_id: null, reply_to: null };
    const card = (expects_reply: unknown) =>
      cardOf<Record<string, unknown> & { dispatch: { payload: Record<string, unknown> } }>(
        renderInbound(
          JSON.stringify({
            ...envelope({
              event_id: "dispatch-mention",
              source: "dispatch",
              payload: targetedCommentFrame(mention),
            }),
            expects_reply,
          }),
          reader
        ).content
      );

    test("prints no reply command when the mention wants no reply, and still names the comment", () => {
      const decoded = card(false);

      expect(decoded.envoy.reply_with).toBeUndefined();
      // The reader is not stranded: the line that says nothing is wanted is on the card, and so
      // is the id of the comment a reader who answers anyway addresses.
      expect(decoded.envoy.expects_reply).toBe(false);
      expect(decoded.envoy.dispatch.payload.id).toBe(targetedCommentID);
    });

    test("keeps the reply command when the same mention invites a reply", () => {
      const decoded = card("optional");

      expect(decoded.envoy.expects_reply).toBe("optional");
      expect(decoded.envoy.reply_with).toEqual({
        command: `dispatch comment --issue CORE-1 --body ... --reply-to ${targetedCommentID}`,
      });
    });
  });

  test("gives document-owned ask comments a project document reply hint and ask ref", () => {
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-document-comment",
        source: "dispatch",
        topic: "notifications.agent.session-asker",
        in_reply_to: "ask-1",
        payload: JSON.stringify({
          id: 3,
          issue_key: null,
          artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          project: "CORE",
          seq: 9,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: {
            ...comment,
            ask_id: "ask-1",
            project_key: "CORE",
            artifact_slug: "design-notes",
          },
        }),
      })
    );

    const decoded = cardOf(renderInbound(raw, reader).content);

    expect(decoded.envoy.re).toBe("dispatch://CORE/artifact/design-notes/ask/ask-1");
    expect(decoded.envoy.reply_with).toEqual({
      command:
        "dispatch comment --project CORE --artifact design-notes --body ... --reply-to-ask ask-1",
    });
  });

  test("a human reply on a still-open ask reaches the agent with the ask's state", () => {
    const askID = "ask-1";
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-2b",
        topic: "notifications.dispatch.issue.DSP-1.comment.created",
        source: "dispatch",
        in_reply_to: askID,
        payload: JSON.stringify({
          id: 2,
          issue_key: "DSP-1",
          seq: 8,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: {
            ...comment,
            author: { kind: "user", id: "alice" },
            ask_id: askID,
            ask_question: apiQuestion,
            ask_state: "open",
          },
        }),
      })
    );

    const decoded = cardOf<{ dispatch: Record<string, unknown> }>(
      renderInbound(raw, reader).content
    );

    // The person who replied is the event's actor, so the reply's block names only its time.
    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      question: apiQuestion,
      state: "open",
    });
    expect(decoded.blocks).toEqual([
      { tag: "reply", name: "reply", text: "Please update this.", at: "2026-09-09T00:00:00Z" },
    ]);
  });

  test("an agent's progress note on an ask renders as the reply with whose turn it is", () => {
    const decoded = cardOf<{ dispatch: Record<string, unknown> }>(
      renderInbound(
        dispatchEvent("comment.created", {
          ...comment,
          ask_id: "ask-1",
          ask_question: releaseQuestion,
          ask_state: "open",
          ask_waiting_on: "agent",
          turn: "agent",
        }),
        reader
      ).content
    );

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "comment.created",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: releaseQuestion,
      state: "open",
      waiting_on: "agent",
    });
    expect(decoded.blocks.map((block) => [block.name, block.text])).toEqual([
      ["reply", "Please update this."],
    ]);
  });

  // Whose ask a reply in an ask's thread is on, as the session reading it decides from the
  // sessions the payload names as the ask's own (`ask_sessions`). ask-reply-recipients.test.ts
  // renders the copies a real outbox published; these are the cases those copies do not hold.
  describe("a reply in an ask's thread, by the session that reads it", () => {
    const asker = "01a02222-3333-7444-8555-666666666666";
    const askReply = {
      id: targetedCommentID,
      issue_key: "CORE-1",
      artifact_id: null,
      author: { kind: "user", id: "alice" },
      body: "Which pool size do you need?",
      anchor: null,
      reply_to: null,
      ask_id: "ask-1",
      ask_question: apiQuestion,
      ask_state: "open",
      ask_waiting_on: "agent",
      ask_sessions: [asker],
      turn: "agent",
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      created_at: "2026-09-12T00:00:00Z",
      mentions: [],
      deliveries: [],
      artifact_name: "",
    };
    const replyToAsk = {
      command: "dispatch comment --issue CORE-1 --body ... --reply-to-ask ask-1",
    };
    type Card = Record<string, unknown> & { dispatch: Record<string, unknown> };
    const card = (raw: string, session: string) =>
      cardOf<Card>(renderInbound(raw, session).content);
    /** The words of the card's blocks, by the name of the field that holds them. */
    const wordsOf = (read: { readonly blocks: readonly ReadBlock[] }) =>
      read.blocks.map(({ name, text }) => [name, text]);
    /** The reply's event as the outbox publishes it on a session's own topic, correlated to the
     *  ask; `event` overrides the event's own fields. */
    const copy = (payload: Record<string, unknown>, event: Record<string, unknown> = {}) =>
      JSON.stringify(
        envelope({
          event_id: "dispatch-ask-reply",
          source: "dispatch",
          source_event_id: "8",
          topic: "notifications.agent.session-reader",
          payload_summary: "CORE-1 comment created",
          in_reply_to: "ask-1",
          payload: JSON.stringify({
            id: 8,
            issue_key: "CORE-1",
            seq: 8,
            type: "comment.created",
            actor: { kind: "user", id: "alice" },
            notify: true,
            created_at: "2026-09-12T00:00:00Z",
            ...event,
            payload,
          }),
        })
      );
    /** The same reply as the delivery a mention of the reader sent: a targeted frame. */
    const mention = (payload: Record<string, unknown>) =>
      JSON.stringify(
        envelope({
          event_id: "dispatch-ask-mention",
          source: "dispatch",
          payload: targetedCommentFrame(payload),
        })
      );

    test("a mention of a session that does not own the ask says so and keeps the mention's reply command, with no turn", () => {
      const decoded = card(mention(askReply), reader);

      expect(decoded.envoy.dispatch.relation).toBe("mentioned you");
      expect(decoded.envoy.reply_with).toEqual(replyToAsk);
      expect(decoded.envoy.dispatch.waiting_on).toBeUndefined();
      expect(decoded.envoy.dispatch.state).toBe("open");
    });

    test("a mention of a session that owns the ask is a reply on its own ask, with its turn", () => {
      const decoded = card(mention(askReply), asker);

      expect(decoded.envoy.dispatch.relation).toBe("replied on your ask");
      expect(decoded.envoy.reply_with).toEqual(replyToAsk);
      expect(decoded.envoy.dispatch.waiting_on).toBe("agent");
    });

    test("a reply on a project document's ask that reached another session is a comment on a document, with no reply command", () => {
      const document = "a4cf7999-cab2-4326-939d-cb1e76733cc3";
      const decoded = card(
        copy(
          {
            ...askReply,
            issue_key: null,
            artifact_id: document,
            project_key: "CORE",
            artifact_slug: "design-notes",
          },
          { issue_key: null, artifact_id: document, project: "CORE" }
        ),
        reader
      );

      expect(decoded.envoy.dispatch.relation).toBe("commented on a document");
      expect(decoded.envoy.reply_with).toBeUndefined();
      expect(decoded.envoy.dispatch.waiting_on).toBeUndefined();
      // Which ask the reply is on, and what it says, stay on the card.
      expect(decoded.envoy.re).toBe("dispatch://CORE/artifact/design-notes/ask/ask-1");
      expect(decoded.envoy.dispatch.question).toBe(apiQuestion);
      expect(wordsOf(decoded)).toEqual([["reply", "Which pool size do you need?"]]);
      expect(decoded.envoy.dispatch.state).toBe("open");
    });

    // The outbox's stand-in for an event the bus refused keeps ask_sessions whole, whatever its
    // size, so the session reading it decides from it as it does from the whole event, and a host
    // that reads the body whole and renders again changes nothing of that.
    test("a stand-in that carries ask_sessions is the owner's own ask and no other session's, before and after its body is read whole", () => {
      const { body: _body, ...kept } = askReply;
      const raw = copy({ ...kept, payload_omitted: true });

      for (const wholeBody of [undefined, askReply.body]) {
        const owner = cardOf<Card>(renderInbound(raw, asker, undefined, wholeBody).content);
        expect(owner.envoy.dispatch.relation).toBe("replied on your ask");
        expect(owner.envoy.reply_with).toEqual(replyToAsk);
        expect(owner.envoy.dispatch.waiting_on).toBe("agent");

        const other = cardOf<Card>(renderInbound(raw, reader, undefined, wholeBody).content);
        expect(other.envoy.dispatch.relation).toBe("commented on an issue");
        expect(other.envoy.reply_with).toBeUndefined();
        expect(other.envoy.dispatch.waiting_on).toBeUndefined();
      }
    });

    // A reply on a project document's ask names that document only in project_key and
    // artifact_slug, since the owner's own topic names none, and the command that answers the ask
    // posts into it. The stand-in keeps both whole however long the document's slug, so the
    // owner's card still carries that command.
    test("a stand-in of a reply on a project document's ask gives its owner, on its own topic, the command that answers the ask, whatever the slug's length", () => {
      const document = "a4cf7999-cab2-4326-939d-cb1e76733cc3";
      const slug = Array.from({ length: 11 }, () => "design-notes-for-the-queue").join("-");
      expect(JSON.stringify(slug).length).toBeGreaterThan(256);
      const { body: _body, ...kept } = askReply;
      const raw = copy(
        {
          ...kept,
          issue_key: null,
          artifact_id: document,
          project_key: "CORE",
          artifact_slug: slug,
          payload_omitted: true,
        },
        { issue_key: null, artifact_id: document, project: "CORE" }
      );

      const decoded = card(raw, asker);

      expect(decoded.envoy.dispatch.relation).toBe("replied on your ask");
      expect(decoded.envoy.reply_with).toEqual({
        command: `dispatch comment --project CORE --artifact ${slug} --body ... --reply-to-ask ask-1`,
      });
      expect(decoded.envoy.dispatch.waiting_on).toBe("agent");
    });

    // A stand-in with no list is one a Dispatch older than ask_sessions sent: the card names no
    // relation, and keeps the command that answers the ask and whose turn it is, as before the
    // field.
    test("a stand-in that carries no ask_sessions shows the card it showed before, with no relation", () => {
      const { body: _body, ask_sessions: _sessions, ...older } = askReply;
      const decoded = card(copy({ ...older, payload_omitted: true }), reader);

      expect(decoded.envoy.dispatch).not.toHaveProperty("relation");
      expect(decoded.envoy.reply_with).toEqual(replyToAsk);
      expect(decoded.envoy.re).toBe("dispatch://CORE-1/ask/ask-1");
      expect(decoded.envoy.dispatch).toMatchObject({
        question: apiQuestion,
        state: "open",
        waiting_on: "agent",
      });
      // The stand-in carries no words of the reply, and the card says where to read them.
      expect(wordsOf(decoded)).toEqual([]);
      expect(decoded.envoy.body_note).toContain(`GET /api/v1/comments/${targetedCommentID}`);
    });

    // A Dispatch older than `ask_sessions` wrote this reply, or a resend replayed one stored
    // before it: the card names no relation, and keeps the command that answers the ask and
    // whose turn it is, as before the field.
    test("a reply that carries no ask_sessions shows the card it showed before, with no relation", () => {
      const { ask_sessions: _sessions, ...older } = askReply;
      const decoded = card(copy(older), reader);

      expect(decoded.envoy.dispatch).not.toHaveProperty("relation");
      expect(decoded.envoy.reply_with).toEqual(replyToAsk);
      expect(decoded.envoy.re).toBe("dispatch://CORE-1/ask/ask-1");
      expect(decoded.envoy.dispatch).toMatchObject({
        question: apiQuestion,
        state: "open",
        waiting_on: "agent",
      });
      expect(wordsOf(decoded)).toEqual([["reply", "Which pool size do you need?"]]);
    });

    test("the ask's own sessions the payload names are no foreign session to note", () => {
      const follower = "01a03333-4444-7555-8666-777777777777";
      const owned = { ...askReply, ask_sessions: [asker, follower] };

      expect(card(copy(owned), reader).envoy.note).toBeUndefined();
      // Another session the body names is still noted.
      const foreign = "01a04444-5555-7666-8777-888888888888";
      expect(card(copy({ ...owned, body: `Ask ${foreign} first.` }), reader).envoy.note).toContain(
        foreign
      );
    });
  });

  test("renders a message.created reply as 're: <message ref>', not the parent's text", () => {
    const rootID = "message-1";
    const raw = JSON.stringify(
      envelope({
        event_id: "dispatch-3",
        source: "dispatch",
        source_event_id: "3",
        topic: "notifications.agent.session-writer",
        payload_summary: "DSP-1 message created",
        in_reply_to: rootID,
        payload: JSON.stringify({
          id: 3,
          issue_key: "DSP-1",
          seq: 9,
          type: "message.created",
          actor: { kind: "user", id: "alice" },
          notify: true,
          created_at: "2026-09-09T00:00:00Z",
          payload: {
            id: "message-2",
            issue_key: "DSP-1",
            author: { kind: "user", id: "alice" },
            body: "Sounds good.",
            in_reply_to: rootID,
            reply_body: "Ship the build tonight.",
            created_at: "2026-09-09T00:00:00Z",
          },
        }),
      })
    );

    const decoded = cardOf<
      Record<string, unknown> & { dispatch: { payload: Record<string, unknown> } }
    >(renderInbound(raw, reader).content);

    expect(decoded.envoy.re).toBe("dispatch://DSP-1/message/message-1");
    expect(decoded.envoy.dispatch.payload.reply_body).toBe("Ship the build tonight.");
  });

  // A comment's, a suggestion's or a message's body is its author's words, so it leaves the
  // payload for a block after the card's fields, and so does the author where the event's actor
  // names them. The text a suggestion proposes is its author's words too, in a block of its own.
  test("types each Dispatch event's nested payload by its wire-contract schema", () => {
    const cases: Array<{ type: string; payload: object; expectedPayload: object; body?: string }> =
      [
        { type: "issue.updated", payload: issue, expectedPayload: issue },
        {
          type: "artifact.created",
          payload: { artifact: { id: "artifact-1", slug: "spec-md", name: "spec.md" } },
          expectedPayload: { artifact: { id: "artifact-1", slug: "spec-md", name: "spec.md" } },
        },
        {
          type: "artifact.version",
          payload: {
            artifact_id: "artifact-1",
            name: "spec.md",
            version: {
              number: 3,
              named: true,
              summary: "Clarify transport",
              authors: [actor],
              created_at: "2026-09-09T00:00:00Z",
            },
            diff: "@@ -1 +1 @@\n-MCP\n+JSON",
          },
          expectedPayload: {
            artifact_id: "artifact-1",
            name: "spec.md",
            version: { number: 3, summary: "Clarify transport" },
            diff: "@@ -1 +1 @@\n-MCP\n+JSON",
          },
        },
        {
          type: "comment.resolved",
          payload: { ...comment, resolved: true },
          expectedPayload: {
            id: "comment-1",
            artifact_name: "spec.md",
            reply_to: "comment-0",
            anchor: { quote: "old line" },
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          body: "Please update this.",
        },
        {
          type: "comment.reopened",
          payload: { ...comment, resolved: false },
          expectedPayload: {
            id: "comment-1",
            artifact_name: "spec.md",
            reply_to: "comment-0",
            anchor: { quote: "old line" },
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          body: "Please update this.",
        },
        {
          type: "comment.edited",
          payload: { ...comment },
          expectedPayload: {
            id: "comment-1",
            artifact_name: "spec.md",
            reply_to: "comment-0",
            anchor: { quote: "old line" },
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          body: "Please update this.",
        },
        {
          type: "comment.anchor_refreshed",
          payload: { ...comment, anchor: { ...comment.anchor, orphaned: true } },
          expectedPayload: {
            id: "comment-1",
            artifact_name: "spec.md",
            reply_to: "comment-0",
            anchor: { artifact_id: "artifact-1", mark_id: "m-1", orphaned: true },
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          body: "Please update this.",
        },
        {
          type: "message.created",
          payload: {
            id: "message-1",
            issue_key: "DSP-1",
            author: actor,
            body: "The build is green.",
            created_at: "2026-09-09T00:00:00Z",
          },
          expectedPayload: { id: "message-1" },
          body: "The build is green.",
        },
        {
          type: "message.created",
          payload: {
            id: "message-2",
            issue_key: "DSP-1",
            author: actor,
            body: "Sounds good.",
            in_reply_to: "message-1",
            reply_body: "The build is green.",
            created_at: "2026-09-09T00:00:00Z",
          },
          expectedPayload: {
            id: "message-2",
            in_reply_to: "message-1",
            reply_body: "The build is green.",
          },
          body: "Sounds good.",
        },
        {
          type: "child.status",
          payload: { child_key: "DSP-2", from: "todo", to: "in_progress" },
          expectedPayload: { child_key: "DSP-2", from: "todo", to: "in_progress" },
        },
      ];

    for (const { type, payload, expectedPayload, body } of cases) {
      const decoded = cardOf<{ dispatch: { payload: object } }>(
        renderInbound(dispatchEvent(type, payload), reader).content
      );
      expect(decoded.envoy.dispatch).toMatchObject({
        owner: "DSP-1",
        type,
        actor: { kind: "session", id: "session-1" },
        payload: expectedPayload,
      });
      expect(decoded.envoy.dispatch.payload).not.toHaveProperty("body");
      expect(decoded.envoy.dispatch.payload).not.toHaveProperty("author");
      expect(decoded.blocks.map((block) => [block.name, block.text])).toEqual(
        body === undefined ? [] : [["body", body]]
      );
    }

    const accepted = cardOf<{ dispatch: { payload: object } }>(
      renderInbound(
        dispatchEvent("suggestion.accepted", {
          ...comment,
          suggestion: { replace_with: "new line", accepted: true },
        }),
        reader
      ).content
    );
    expect(accepted.envoy.dispatch).toMatchObject({
      owner: "DSP-1",
      type: "suggestion.accepted",
      actor: { kind: "session", id: "session-1" },
      payload: {
        id: "comment-1",
        artifact_name: "spec.md",
        reply_to: "comment-0",
        anchor: { quote: "old line" },
        created_at: "2026-09-09T00:00:00Z",
      },
    });
    for (const moved of ["body", "author", "suggestion"]) {
      expect(accepted.envoy.dispatch.payload).not.toHaveProperty(moved);
    }
    expect(accepted.blocks.map(({ name, text, field }) => [name, text, field])).toEqual([
      ["body", "Please update this.", undefined],
      ["replace_with", "new line", "suggestion.replace_with"],
    ]);
  });

  test("surfaces the comment id before the body, and the message id, in rendered dispatch payloads", () => {
    const commentContent = renderInbound(
      dispatchEvent("comment.created", { ...comment }),
      reader
    ).content;
    expect(commentContent).toContain("comment-1");
    expect(commentContent.indexOf("comment-1")).toBeLessThan(commentContent.indexOf("\n<body>\n"));

    const messageContent = renderInbound(
      dispatchEvent("message.created", {
        id: "message-1",
        issue_key: "DSP-1",
        author: actor,
        body: "The build is green.",
        created_at: "2026-09-09T00:00:00Z",
      }),
      reader
    ).content;
    expect(messageContent).toContain("message-1");
  });

  test("renders an unrecognized-shape dispatch payload as its raw structured object", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: JSON.stringify({
            secret: "now visible as data, not prose", // fixture-secret: a field of an unrecognized Dispatch payload, rendered as data; not a credential, never held
            unexpected: { nested: true },
          }),
        })
      ),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: { dispatch: unknown } };

    expect(decoded.envoy.dispatch).toEqual({
      secret: "now visible as data, not prose", // fixture-secret: the same payload field, read back; never held
      unexpected: { nested: true },
    });
  });

  test("falls back to the generic summary path when a dispatch envelope has no payload", () => {
    const rendered = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload_summary: "DSP-1 ask answered" })),
      reader
    );
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy.summary).toBe("DSP-1 ask answered");
    expect(decoded.envoy.dispatch).toBeUndefined();
    expect(decoded.envoy.unrecognised).toBe("payload");
  });

  test("keeps punctuation and new lines of a dispatch message body as written, in its block", () => {
    const body = 'First line: has "quotes", commas,\nand a second line.';
    const decoded = cardOf(
      renderInbound(dispatchEvent("message.created", { body }), reader).content
    );

    expect(decoded.blocks.map((block) => [block.name, block.text])).toEqual([["body", body]]);
  });

  test("renders a non-notifying Dispatch event on an agent subject", () => {
    const rendered = renderInbound(
      dispatchEvent("message.created", { body: "Agent subject remains visible" }, actor, {
        notify: false,
      }),
      reader,
      `notifications.agent.${reader}`
    );

    expect(rendered.skip).toBe(false);
    const decoded = cardOf(rendered.content);
    expect(decoded.envoy).toMatchObject({ dispatch: { type: "message.created" } });
    expect(decoded.blocks.map((block) => block.text)).toEqual(["Agent subject remains visible"]);
  });

  test("renders a non-notifying Dispatch event on a role subject", () => {
    const rendered = renderInbound(
      dispatchEvent("message.created", { body: "Role subject remains visible" }, actor, {
        notify: false,
      }),
      reader,
      "notifications.role.legion-controller"
    );

    expect(rendered.skip).toBe(false);
    const decoded = cardOf(rendered.content);
    expect(decoded.envoy).toMatchObject({ dispatch: { type: "message.created" } });
    expect(decoded.blocks.map((block) => block.text)).toEqual(["Role subject remains visible"]);
  });

  test("drops an event that does not request an agent wake", () => {
    expect(
      renderInbound(
        dispatchEvent("message.created", { body: "Persist without steering" }, actor, {
          notify: false,
        }),
        reader
      )
    ).toMatchObject({ skip: true, content: "" });
  });

  // The one thing `envoy_subscribe notifications.dispatch.issue.<KEY>.>` is for
  // (skills/dispatch/SKILL.md): a session following an issue hears what the agents on it do.
  // Every such event carries notify false, because notify asks whether to wake the issue's route
  // and claimant, so a reader that skipped on notify saw none of them.
  test("shows an agent's comment on the issue topic it subscribed to", () => {
    const rendered = renderInbound(
      dispatchEvent("comment.created", { id: "comment-9", body: "Slice 3 is pushed." }, actor, {
        notify: false,
        quiet: false,
      }),
      reader,
      legacyDispatchIssueSubject("DSP-1", "comment.created")
    );

    expect(rendered.skip).toBe(false);
    const decoded = cardOf(rendered.content);
    expect(decoded.envoy).toMatchObject({
      dispatch: { type: "comment.created", payload: { id: "comment-9" } },
    });
    expect(decoded.blocks.map((block) => block.text)).toEqual(["Slice 3 is pushed."]);
  });

  test("drops a quiet event on an issue topic however it was authored", () => {
    expect(
      renderInbound(
        dispatchEvent("comment.delivery", { id: "comment-9", state: "delivered" }, actor, {
          notify: false,
          quiet: true,
        }),
        reader,
        legacyDispatchIssueSubject("DSP-1", "comment.delivery")
      )
    ).toMatchObject({ skip: true, content: "" });
    expect(
      renderInbound(
        dispatchEvent(
          "artifact.version",
          { version: { number: 4, named: false } },
          { kind: "user", id: "alice" },
          { notify: false, quiet: true }
        ),
        reader,
        legacyDispatchIssueSubject("DSP-1", "artifact.version")
      )
    ).toMatchObject({ skip: true, content: "" });
  });

  // The stream holds 72 hours of frames published before Dispatch stated `quiet`, and a replay
  // of those must not start showing their receipts and autosaves.
  test("judges a frame published before `quiet` by its notify flag", () => {
    expect(
      renderInbound(
        dispatchEvent("comment.created", { id: "comment-9", body: "Older frame" }, actor, {
          notify: false,
        }),
        reader,
        legacyDispatchIssueSubject("DSP-1", "comment.created")
      )
    ).toMatchObject({ skip: true, content: "" });
    expect(
      renderInbound(
        dispatchEvent(
          "comment.created",
          { id: "comment-9", body: "Older frame a person wrote" },
          { kind: "user", id: "alice" },
          { notify: true }
        ),
        reader,
        legacyDispatchIssueSubject("DSP-1", "comment.created")
      ).skip
    ).toBe(false);
  });

  test("drops an event authored by the reader session", () => {
    expect(
      renderInbound(
        dispatchEvent("message.created", { body: "Own update" }, { kind: "session", id: reader }),
        reader
      )
    ).toMatchObject({ skip: true, content: "" });
  });
});

describe("renderInbound non-dispatch envelopes", () => {
  test("keeps GitHub messages on the existing TOON rendering path", () => {
    expect(
      renderInbound(
        JSON.stringify(
          envelope({
            event_id: "github-comment-1",
            source: "github",
            source_event_id: "github.issue_comment.42",
            topic: "notifications.github.example-org.example-repo.issue.42.comment",
            dedupe_key: "github-comment-1",
            issued_at: Date.parse("2026-09-07T04:41:12Z"),
            payload_summary: "A comment was created on issue 42.",
            payload: JSON.stringify({ author: "Reviewer", body: "Ship it.", number: 42 }),
            trace_id: "trace-github-comment-1",
          })
        ),
        reader
      ).content
    ).toBe(
      [
        "envoy:",
        "  from: github",
        '  at: "2026-09-07T04:41:12Z"',
        "  id: github-comment-1",
        "  summary: A comment was created on issue 42.",
        "  message:",
        "    author: Reviewer",
        "    body: Ship it.",
        "    number: 42",
      ].join("\n")
    );
  });

  test("renders a direct message of another session as its fields, then its words in a block", () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-2",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-2",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-2",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        expires_at: Date.parse("2026-09-07T05:00:00Z"),
        payload_summary: "First paragraph.",
        trace_id: "trace-agent-message-2",
        sender: { session_id: sender, title: "Reviewer", roles: ["legion-reviewer"] },
        in_reply_to: "agent-message-1",
        supersedes: "agent-message-0",
        urgency: "high",
        expects_reply: "required",
      }),
      reader
    );
    const card = cardOf(rendered.content);

    expect(card.envoy).toEqual({
      to: "you",
      from: `${sender} (Reviewer)`,
      role: "legion-reviewer",
      at: "2026-09-07T04:41:12Z",
      id: "agent-message-2",
      by: "2026-09-07T05:00:00Z",
      urgency: "high",
      expects_reply: "required",
      re: "agent-message-1",
      supersedes: "agent-message-0",
    });
    expect(card.blocks).toEqual([{ tag: "message", name: "message", text: "First paragraph." }]);
  });

  // A card prints one vocabulary. The envelope carries the JSON literal `false` for "no reply
  // wanted", while a replayed frame and a session on an older plugin build still carry the
  // string "none" for the same thing. Both read as the one line, and neither leaves the field
  // unread and listed as unrecognised.
  test("renders either spelling of no reply as one expects_reply: false line", () => {
    const card = (expects_reply: unknown): string =>
      renderInbound(
        JSON.stringify({ ...envelope({ event_id: "agent-no-reply" }), expects_reply }),
        reader
      ).content;

    expect(card(false)).toBe(
      [
        "envoy:",
        "  from: agent",
        '  at: "1970-01-01T00:00:00Z"',
        "  id: agent-no-reply",
        "  expects_reply: false",
        "  summary: message",
      ].join("\n")
    );
    expect(card("none")).toBe(card(false));
  });

  test("renders when a message forwarded from a role's kept lane was kept", () => {
    // The listener could not deliver this when it was sent, so it kept it until this session
    // claimed the role. The card says when, so the holder can judge how long it waited.
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-kept",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-kept",
        topic: "notifications.role.legion-architect",
        dedupe_key: "agent-message-kept",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        kept_at: Date.parse("2026-09-07T04:41:13Z"),
        payload_summary: "The implementer is stalled.",
        trace_id: "trace-agent-message-kept",
      }),
      reader
    );
    const card = cardOf(rendered.content);

    expect(card.envoy).toEqual({
      from: sender,
      at: "2026-09-07T04:41:12Z",
      id: "agent-message-kept",
      kept_at: "2026-09-07T04:41:13Z",
    });
    expect(card.blocks.map((block) => block.text)).toEqual(["The implementer is stalled."]);
  });

  test("omits a summary duplicated by a longer agent message", () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const head = "A".repeat(159);
    const body = `${head}B long first line continues past the truncation cap.\n\nSecond paragraph.`;
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-3",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-3",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-3",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: `${head}…`,
        payload: body,
        trace_id: "trace-agent-message-3",
      }),
      reader
    );

    const card = cardOf(rendered.content);
    expect(card.envoy).not.toHaveProperty("summary");
    expect(card.blocks).toEqual([{ tag: "message", name: "message", text: body }]);
  });

  test("keeps a GitHub headline with a distinct structured payload", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "github-comment-2",
        source: "github",
        source_event_id: "github.issue_comment.43",
        topic: "notifications.github.example-org.example-repo.issue.43.comment",
        dedupe_key: "github-comment-2",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A comment was created on issue 43.",
        payload: JSON.stringify({ author: "Reviewer", body: "A comment was created", number: 43 }),
        trace_id: "trace-github-comment-2",
      }),
      reader
    );

    expect(rendered.content).toContain("  summary: A comment was created on issue 43.");
    expect(rendered.content).toContain("    body: A comment was created");
  });

  test("notes a foreign session named only in a payload", () => {
    const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
    const foreign = "01a0cccc-dddd-7eee-ffff-0123456789ab";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "agent-message-foreign-session",
        source: "agent",
        source_session: sender,
        source_event_id: "agent.message-foreign-session",
        topic: `notifications.agent.${reader}`,
        dedupe_key: "agent-message-foreign-session",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "The reviewer shared a detailed update.",
        payload: `Ask ${foreign} to confirm the release.`,
        trace_id: "trace-agent-message-foreign-session",
      }),
      reader
    );

    // The note names the session the words name; `from` alone names the sender.
    const card = cardOf(rendered.content);
    expect(card.envoy.note).toBe(`body names session ${foreign}`);
    expect(rendered.content.split(sender)).toHaveLength(2);
  });

  test("renders recognized fields without raw unknown data", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "unknown-source-1",
        source: "newkind",
        topic: "notifications.example",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A future source delivered this summary.",
        unknown_field: "never show this raw value",
      }),
      reader
    );

    expect(rendered.content).toContain("  unrecognised: source=newkind");
    expect(rendered.content).not.toContain("never show this raw value");
    expect(rendered.content).not.toContain("unknown_field");
  });

  test("keeps a literal unknown payload when the summary is absent", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "payload-unknown-1",
        source: "agent",
        topic: "notifications.agent.ses_target",
        payload: "unknown",
      }),
      reader
    );

    const card = cardOf(rendered.content);
    expect(card.envoy.summary).toBe("unknown");
    expect(card.blocks.map((block) => [block.name, block.text])).toEqual([["message", "unknown"]]);
  });

  test("keeps recognized fields when a JSON envelope omits its source", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "missing-source-1",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: "A sender was not provided.",
      }),
      reader
    );

    expect(rendered.content).toBe(
      [
        "envoy:",
        "  from: unknown",
        '  at: "2026-09-07T04:41:12Z"',
        "  id: missing-source-1",
        "  summary: A sender was not provided.",
        "  unrecognised: source",
      ].join("\n")
    );
  });

  test("keeps valid fields when a known field has the wrong type", () => {
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "invalid-timestamp-1",
        source: "agent",
        issued_at: "not-a-timestamp",
        payload_summary: "The timestamp was malformed.",
      }),
      reader
    );

    expect(rendered.content).toBe(
      [
        "envoy:",
        "  from: agent",
        "  at: unknown",
        "  id: invalid-timestamp-1",
        "  summary: The timestamp was malformed.",
        "  unrecognised: issued_at",
      ].join("\n")
    );
  });

  test("keeps valid sender fields when sender.roles is malformed", () => {
    const sender = "01a00000-0000-7000-0000-000000000001";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "invalid-sender-1",
        source: "agent",
        source_session: sender,
        topic: "notifications.agent.ses_target",
        payload_summary: "The sender role was malformed.",
        sender: { title: "Reviewer", roles: "reviewer" },
      }),
      reader
    );

    expect(rendered.content).toContain(`  from: ${sender} (Reviewer)`);
    expect(rendered.content).toContain("  unrecognised: sender.roles");
  });

  test("hides a non-JSON frame and identifies its delivery subject", () => {
    const rendered = renderInbound("not json: secret raw bytes", reader, "notifications.example");

    expect(rendered).toEqual({
      skip: false,
      content: "envoy:\n  topic: notifications.example\n  unrecognised: payload was not JSON",
    });
    expect(rendered.content).not.toContain("secret raw bytes");
  });

  test("notes a foreign session named in the body", () => {
    const foreign = "01a0ffff-aaaa-7bbb-cccc-0123456789ab";
    const sender = "01a00000-0000-7000-0000-000000000001";
    const rendered = renderInbound(
      JSON.stringify({
        event_id: "foreign-session-1",
        source: "agent",
        source_session: sender,
        topic: "notifications.agent.elsewhere",
        issued_at: Date.parse("2026-09-07T04:41:12Z"),
        payload_summary: `Please contact ${foreign} before proceeding.`,
      }),
      reader
    );

    expect(cardOf(rendered.content).envoy.note).toBe(`body names session ${foreign}`);
  });
});

describe("renderInbound pictures", () => {
  const shot = "dispatch://DSP-1/artifact/shot-png@v1";
  const chart = "dispatch://DSP-1/artifact/chart-png@v2";
  const asked = "dispatch://DSP-1/artifact/asked-png@v1";
  const shown = (...addresses: string[]) =>
    addresses.map((address, index) => `![picture ${index + 1}](${address})`).join("\n");

  // An answer or a resolution delivers what moved, so the question's own picture, delivered when
  // the ask opened, does not come again with it.
  test.each([
    ["comment.created", { ...comment, body: `Before and after:\n\n${shown(shot, chart)}` }],
    ["comment.answered", { ...comment, body: `In reply:\n\n${shown(shot, chart)}` }],
    ["ask.opened", { ...openAsk, question: `${apiQuestion}\n\n${shown(shot, chart)}` }],
    [
      "ask.edited",
      {
        ...openAsk,
        question: `${transportQuestion}\n\n${shown(shot, chart)}`,
        multiple: false,
        urgency: "med",
        edited_at: "2026-09-11T03:26:00Z",
        previous: {
          question: apiQuestion,
          options: openAsk.options,
          multiple: false,
          urgency: "med",
        },
        edited_by: actor,
      },
    ],
    [
      "ask.answered",
      {
        ...answeredAsk,
        question: `${apiQuestion}\n\n${shown(asked)}`,
        answer: { ...answeredAsk.answer, text: `Like this:\n\n${shown(shot, chart)}` },
      },
    ],
    [
      "ask.resolved",
      {
        ...openAsk,
        question: `${apiQuestion}\n\n${shown(asked)}`,
        state: "resolved",
        resolution: {
          actor,
          at: "2026-09-10T00:01:00Z",
          kind: "resolved",
          reason: shown(shot, chart),
        },
      },
    ],
  ] as const)("%s carries the pictures its new text embeds, in writing order", (type, payload) => {
    expect(renderInbound(dispatchEvent(type, payload), reader).pictures).toEqual([shot, chart]);
  });

  // A broadcast's frame carries its message with the recipient's pending attempt already listed,
  // where a direct message's lists none yet; both are the same message to the agent.
  test("a targeted message carries its pictures whether or not its attempt is listed", () => {
    const messageID = "44444444-4444-4444-8444-444444444444";
    const attempt = {
      message_id: messageID,
      attempt: 1,
      delivery: "btw",
      session_id: reader,
      envelope_id: null,
      duplicate: false,
      state: "pending",
      error: null,
      reply_id: null,
      created_at: "2026-10-04T10:00:00Z",
      requested_by: { kind: "user", id: "alice" },
      accepted_at: null,
      accepted_as: null,
    };
    for (const deliveries of [[], [attempt]]) {
      const rendered = renderInbound(
        JSON.stringify(
          envelope({
            source: "dispatch",
            topic: `notifications.agent.${reader}`,
            payload: JSON.stringify({
              event: {
                id: 9,
                issue_key: null,
                seq: 9,
                type: "message.created",
                actor: { kind: "user", id: "alice" },
                notify: false,
                created_at: "2026-10-04T10:00:00Z",
                payload: {
                  id: messageID,
                  issue_key: null,
                  author: { kind: "user", id: "alice" },
                  body: `Look:\n\n${shown(shot)}`,
                  target: `session:${reader}`,
                  in_reply_to: null,
                  broadcast_id:
                    deliveries.length === 0 ? null : "55555555-5555-4555-8555-555555555555",
                  created_at: "2026-10-04T10:00:00Z",
                  deliveries,
                },
              },
              delivery: { attempt: 1, mode: "btw" },
            }),
          })
        ),
        reader,
        `notifications.agent.${reader}`
      );
      expect(rendered.delivery?.resource).toBe("message");
      expect(rendered.pictures).toEqual([shot]);
    }
  });

  test("a text that only names a picture, or shows one from another website, carries none", () => {
    const rendered = renderInbound(
      dispatchEvent("comment.created", {
        ...comment,
        body: `See ${shot} and ![logo](https://example.com/logo.png).`,
      }),
      reader
    );
    expect(rendered.pictures).toBeUndefined();
  });
});

// A body is bounded block by block, so one event carries its first part alone and says so with
// `body_truncated`. A host reads that off the delivery (`bodyTruncated`), and the rendered card
// carries the same note the Go deliverer writes, naming the route that answers the whole item.
describe("a body an event carried only in part", () => {
  test("a targeted comment delivery carries bodyTruncated and the note", () => {
    const rendered = renderInbound(
      JSON.stringify(
        envelope({
          source: "dispatch",
          payload: targetedCommentFrame({
            ...targetedComment,
            body: "The first paragraph.",
            body_truncated: true,
          }),
        })
      ),
      reader
    );

    expect(rendered.delivery?.body).toBe("The first paragraph.");
    expect(rendered.delivery?.bodyTruncated).toBe(true);
    const decoded = cardOf(rendered.content);
    expect(decoded.envoy.body_note).toBe(
      `the body is its first part only; read it whole with GET /api/v1/comments/${targetedCommentID}`
    );
  });

  test("a delivery whose body rode whole carries neither", () => {
    const rendered = renderInbound(
      JSON.stringify(envelope({ source: "dispatch", payload: targetedCommentFrame() })),
      reader
    );

    expect(rendered.delivery?.bodyTruncated).toBeUndefined();
    const decoded = cardOf(rendered.content);
    expect(decoded.envoy.body_note).toBeUndefined();
  });
});

// The card of R1 and R4 (dispatch://INFRA-3686/spec): a card from another session and a card of
// the Legion daemon carry no reply command and nothing twice, and every card puts the words of
// another author in a block of their own after its fields. The check is `c-card-compact`.
describe("what a card carries", () => {
  const sender = "01a0bbbb-cccc-7ddd-eeee-0123456789ab";
  const architectRole = "legion-infra-infra-3686-architect";

  /** An envelope a session sent, as the listener writes it: a message of one line rides as its
   *  summary alone, a longer one as its summary and the whole message (`messageEnvelope`). */
  function sessionMessage(
    words: string,
    overrides: Record<string, unknown> = {}
  ): Record<string, unknown> {
    const summary = words.split("\n")[0] ?? "";
    return {
      event_id: "d77bf664a3c94f0e8d1b2a7c5e6f9d01",
      source: "agent",
      source_session: sender,
      source_event_id: `agent.${sender}.d77bf664a3c94f0e8d1b2a7c5e6f9d01`,
      topic: `notifications.agent.${reader}`,
      dedupe_key: `agent.${reader}.d77bf664a3c94f0e8d1b2a7c5e6f9d01`,
      issued_at: Date.parse("2026-11-14T21:05:01Z"),
      payload_summary: summary,
      ...(words === summary ? {} : { payload: words }),
      trace_id: "trace-session-message",
      ...overrides,
    };
  }

  /** A notice or a role message as the daemon publishes it: its payload as JSON under the summary
   *  `<kind> on <KEY>`, with no session, at `urgency`. */
  function daemonMessage(
    topic: string,
    summary: string,
    payload: unknown,
    urgency = "med"
  ): string {
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

  /** How often `part` occurs in `text`. */
  const times = (text: string, part: string) => text.split(part).length - 1;

  test("a card from another session carries no reply command", () => {
    const title = "Legion architect · INFRA-3231";
    const readerRole = "legion-infra-infra-3686-implementer";
    const words = "Push your branch, then move the issue back to planning.";
    const sendersRoles = [
      [],
      ["legion-infra-infra-3231-architect"],
      ["legion-infra-infra-3231-architect", "legion-controller"],
    ];
    for (const roles of sendersRoles) {
      for (const expectsReply of [undefined, false, "optional", "required"] as const) {
        for (const topic of [`notifications.agent.${reader}`, `notifications.role.${readerRole}`]) {
          // A session a broker token proved sends with no source_session in its body: the
          // listener's sender stamp alone names it, and only the daemon's own publish has neither.
          for (const by of ["source_session", "sender stamp"] as const) {
            const where = `${topic}, ${roles.length} roles, expects_reply ${String(expectsReply)}, by ${by}`;
            const raw = sessionMessage(words, {
              topic,
              ...(by === "source_session" ? {} : { source_session: undefined }),
              // A sender's list can name a role twice; the card names it once.
              sender: { session_id: sender, title, roles: [...roles, ...roles] },
              ...(expectsReply === undefined ? {} : { expects_reply: expectsReply }),
            });
            const content = renderInbound(JSON.stringify(raw), reader, topic).content;
            const card = cardOf<Record<string, unknown>>(content);

            expect(content, where).not.toContain("reply_with");
            expect(content, where).not.toContain("reply_role");
            expect(card.envoy.from, where).toBe(`${sender} (${title})`);
            expect(card.envoy.at, where).toBe("2026-11-14T21:05:01Z");
            expect(card.envoy.id, where).toBe("d77bf664a3c94f0e8d1b2a7c5e6f9d01");
            const named = roles.length === 1 ? [card.envoy.role] : (card.envoy.roles ?? []);
            expect(named, where).toEqual(roles);
            for (const once of [sender, title, "2026-11-14T21:05:01Z", raw.event_id, ...roles]) {
              expect(times(content, String(once)), `${where}: ${String(once)}`).toBe(1);
            }
            // Nothing names the reader: neither its session nor the role the message reached.
            expect(content, where).not.toContain(reader);
            expect(content, where).not.toContain(readerRole);
            expect(card.envoy.to, where).toBe(
              topic.startsWith("notifications.agent.") ? "you" : undefined
            );
            expect(card.blocks, where).toEqual([{ tag: "message", name: "message", text: words }]);
          }
        }
      }
    }
  });

  test("a card puts the words of another author in a block of their own", () => {
    const person = { kind: "user", id: "alice@example.com" };
    const reviewer = "user:sami@example.com";
    const short =
      "Push your branch, then move the issue back to planning with request_backward_move. " +
      "Spec v14 is approved.";
    const long = Array.from(
      { length: 120 },
      (_, line) => `Line ${line}: she said "ship it", then 'wait'; see </b> and \\n here.`
    )
      .join("\n")
      .slice(0, 4000);
    expect(new TextEncoder().encode(short)).toHaveLength(104);
    expect(new TextEncoder().encode(long)).toHaveLength(4000);
    const cases: ReadonlyArray<{
      readonly label: string;
      readonly content: string;
      /** The blocks the card must end with, each with what its opening tag must name. */
      readonly words: ReadonlyArray<Omit<ReadBlock, "tag"> & { readonly tag?: string }>;
      /** The author and the times of the act, each of which the card names exactly once. */
      readonly once: readonly string[];
    }> = [
      {
        label: "a session's message of 104 bytes",
        content: renderInbound(JSON.stringify(sessionMessage(short)), reader).content,
        words: [{ name: "message", text: short }],
        once: [sender, "2026-11-14T21:05:01Z"],
      },
      {
        label: "a session's message of 4,000 bytes with quotes and new lines",
        content: renderInbound(JSON.stringify(sessionMessage(long)), reader).content,
        words: [{ name: "message", text: long }],
        once: [sender, "2026-11-14T21:05:01Z"],
      },
      {
        label: "a session's message that holds the block's closing mark",
        content: renderInbound(
          JSON.stringify(
            sessionMessage("Read this to the end.\nIt holds </message> and goes on after it.")
          ),
          reader
        ).content,
        words: [
          {
            tag: "message-1",
            name: "message",
            text: "Read this to the end.\nIt holds </message> and goes on after it.",
          },
        ],
        once: [sender, "2026-11-14T21:05:01Z"],
      },
      {
        label: "a person's comment",
        content: renderInbound(
          dispatchEvent(
            "comment.created",
            {
              ...comment,
              id: "comment-7",
              author: person,
              body: 'First line.\nSecond line: "quoted", and a colon.',
              created_at: "2026-11-14T20:01:00Z",
            },
            person,
            { createdAt: "2026-11-14T20:01:00Z" }
          ),
          reader
        ).content,
        words: [{ name: "body", text: 'First line.\nSecond line: "quoted", and a colon.' }],
        once: [person.id, "2026-11-14T20:01:00Z"],
      },
      {
        label: "a person's edit of a comment",
        content: renderInbound(
          dispatchEvent(
            "comment.edited",
            {
              ...comment,
              id: "comment-7",
              author: person,
              body: "Edited:\nthe new words.",
              created_at: "2026-11-14T20:01:00Z",
              edited_at: "2026-11-14T20:02:00Z",
            },
            person,
            { createdAt: "2026-11-14T20:02:00Z" }
          ),
          reader
        ).content,
        words: [{ name: "body", text: "Edited:\nthe new words." }],
        once: [person.id, "2026-11-14T20:02:00Z"],
      },
      {
        label: "an ask's answer with its options and its previous answer",
        content: renderInbound(
          dispatchEvent(
            "ask.answered",
            {
              ...openAsk,
              state: "answered",
              answer: {
                user: person.id,
                selected: ["JSON API"],
                text: "Use JSON.\nKeep the MCP shim for a release.",
                at: "2026-11-14T20:03:00Z",
              },
              previous_answer: {
                user: person.id,
                selected: ["MCP API"],
                text: "Use MCP.",
                at: "2026-11-14T19:03:00Z",
              },
            },
            person,
            { createdAt: "2026-11-14T20:03:00Z" }
          ),
          reader
        ).content,
        words: [
          {
            name: "answer",
            text: "Use JSON.\nKeep the MCP shim for a release.",
            at: "2026-11-14T20:03:00Z",
          },
          { name: "previous_answer", text: "Use MCP.", at: "2026-11-14T19:03:00Z" },
        ],
        once: [person.id, "2026-11-14T20:03:00Z", "2026-11-14T19:03:00Z"],
      },
      {
        label: "a reply in an ask's thread",
        content: renderInbound(
          dispatchEvent(
            "comment.created",
            {
              ...comment,
              id: "comment-8",
              author: person,
              body: "Which version?\nThe spec says v6.",
              created_at: "2026-11-14T20:04:00Z",
              ask_id: "ask-1",
              ask_question: apiQuestion,
              ask_state: "open",
            },
            person,
            { createdAt: "2026-11-14T20:04:00Z" }
          ),
          reader
        ).content,
        words: [
          { name: "reply", text: "Which version?\nThe spec says v6.", at: "2026-11-14T20:04:00Z" },
        ],
        once: [person.id, "2026-11-14T20:04:00Z"],
      },
      {
        label: "an ask's resolution with its reason",
        content: renderInbound(
          dispatchEvent(
            "ask.resolved",
            {
              ...openAsk,
              state: "resolved",
              resolution: {
                actor: person,
                at: "2026-11-14T20:05:00Z",
                kind: "declined",
                reason: "Not this week.\nAsk again after the release.",
                dispatch_words: "The person chose not to answer this question.",
              },
            },
            person,
            { createdAt: "2026-11-14T20:05:00Z" }
          ),
          reader
        ).content,
        words: [
          {
            name: "reason",
            text: "Not this week.\nAsk again after the release.",
            at: "2026-11-14T20:05:00Z",
          },
        ],
        once: [person.id, "2026-11-14T20:05:00Z"],
      },
      {
        label: "a message",
        content: renderInbound(
          dispatchEvent(
            "message.created",
            { id: "message-7", author: person, body: "Can this ship?\nThe tests are green." },
            person,
            { createdAt: "2026-11-14T20:06:00Z" }
          ),
          reader
        ).content,
        words: [
          {
            name: "body",
            text: "Can this ship?\nThe tests are green.",
            at: "2026-11-14T20:06:00Z",
          },
        ],
        once: [person.id, "2026-11-14T20:06:00Z"],
      },
      {
        label: "an answer to a message",
        content: renderInbound(
          dispatchEvent(
            "message.answered",
            {
              id: "message-8",
              author: person,
              body: "Yes, ship it.\nI read the logs.",
              in_reply_to: "message-7",
              reply_body: "Can this ship?",
            },
            person,
            { createdAt: "2026-11-14T20:07:00Z" }
          ),
          reader
        ).content,
        words: [
          { name: "body", text: "Yes, ship it.\nI read the logs.", at: "2026-11-14T20:07:00Z" },
        ],
        once: [person.id, "2026-11-14T20:07:00Z"],
      },
      {
        label: "a phase-finished with a worker's summary of two lines",
        content: renderInbound(
          daemonMessage(`notifications.role.${architectRole}`, "phase-finished on INFRA-3686", {
            kind: "phase-finished",
            role: "implementer",
            phase: "implementing",
            issue: "INFRA-3686",
            to: "testing",
            summary: "Built the card renderer.\nThe tests pass.",
            handoff_phase: "implement",
            handoff_version: 3,
            words: { summary: { by: "implementer", at: "2026-11-14T21:00:00Z" } },
          }),
          reader
        ).content,
        words: [
          {
            name: "summary",
            text: "Built the card renderer.\nThe tests pass.",
            at: "2026-11-14T21:00:00Z",
          },
        ],
        once: ["implementer", "2026-11-14T21:00:00Z"],
      },
      {
        label: "a phase-finished after a backward move whose reason holds the closing mark",
        content: renderInbound(
          daemonMessage(`notifications.role.${architectRole}`, "phase-finished on INFRA-3686", {
            kind: "phase-finished",
            role: "tester",
            phase: "testing",
            issue: "INFRA-3686",
            to: "implementing",
            reason: "The card drops its id.\nSee </reason> in the fixture.",
            words: { reason: { by: "tester", at: "2026-11-14T21:01:00Z" } },
          }),
          reader
        ).content,
        words: [
          {
            tag: "reason-1",
            name: "reason",
            text: "The card drops its id.\nSee </reason> in the fixture.",
            at: "2026-11-14T21:01:00Z",
          },
        ],
        once: ["tester", "2026-11-14T21:01:00Z"],
      },
      {
        label: "a design-changes-requested with a person's reason",
        content: renderInbound(
          daemonMessage(
            `notifications.role.${architectRole}`,
            "design-changes-requested on INFRA-3686",
            {
              kind: "design-changes-requested",
              issue: "INFRA-3686",
              version: 6,
              reason: "Name the reader once.\nKeep the head summary.",
              actor: reviewer,
              ref: "dispatch://INFRA-3686/spec",
              at: "2026-11-14T21:02:00Z",
              words: { reason: { by: reviewer, at: "2026-11-14T21:02:00Z" } },
            }
          ),
          reader
        ).content,
        words: [{ name: "reason", text: "Name the reader once.\nKeep the head summary." }],
        once: [reviewer, "2026-11-14T21:02:00Z"],
      },
      {
        label: "a parked-decision with a person's answer",
        content: renderInbound(
          daemonMessage("notifications.legion.infra.controller", "parked-decision on INFRA-3686", {
            kind: "parked-decision",
            issue: "INFRA-3686",
            actor: reviewer,
            ref: "dispatch://INFRA-3686/ask/2e360d60-db20-4cbe-b300-0ddedfa9f328",
            decision: "answered",
            root: "INFRA-3686",
            at: "2026-11-14T21:03:00Z",
            answer: "Ship it.\nThen tell the controller.",
            options: ["Ship"],
            words: { answer: { by: reviewer, at: "2026-11-14T21:03:00Z" } },
          }),
          reader
        ).content,
        words: [{ name: "answer", text: "Ship it.\nThen tell the controller." }],
        once: [reviewer, "2026-11-14T21:03:00Z"],
      },
    ];

    for (const { label, content, words, once } of cases) {
      // The reader throws where anything but blocks follows the first block, so no field comes
      // after the words to be read as the author's.
      const { head, blocks } = splitWordsBlocks(content);
      expect(decode(head), label).toMatchObject({ envoy: {} });
      expect(
        blocks.map(
          ({ tag, name, text, by, at }): Partial<ReadBlock> => ({ tag, name, text, by, at })
        ),
        label
      ).toEqual(words.map((said) => ({ ...said, tag: said.tag ?? said.name })));
      expect(content.endsWith(`</${blocks.at(-1)?.tag}>`), label).toBe(true);
      for (const { text } of words) {
        // The words are whole in their block, and in no field.
        expect(times(content, text), label).toBe(1);
        expect(head, label).not.toContain(text.split("\n")[0]);
      }
      for (const named of once) expect(times(content, named), `${label}: ${named}`).toBe(1);
      expect(head, label).not.toMatch(/^ +words:/m);
    }

    // Dispatch's own words beside a decline stay a field, outside the person's block.
    const declined = cases.find(({ label }) => label === "an ask's resolution with its reason");
    const resolution = cardOf<{ dispatch: Record<string, unknown> }>(declined?.content ?? "");
    expect(resolution.envoy.dispatch).toMatchObject({
      resolved: "declined",
      dispatch_words: "The person chose not to answer this question.",
    });
    // The options an answer chose stay fields beside the blocks of its words.
    const answered = cases.find(({ label }) => label.startsWith("an ask's answer"));
    expect(
      cardOf<{ dispatch: Record<string, unknown> }>(answered?.content ?? "").envoy.dispatch
    ).toMatchObject({ selected: ["JSON API"], previous_selected: ["MCP API"] });

    // A stand-in the bus took with its long fields dropped carries no answer, and its `words`
    // entry says more follows: the block has no words and that mark, and the fields name the
    // author and the time.
    const standIn = cardOf<Record<string, unknown>>(
      renderInbound(
        daemonMessage("notifications.legion.infra.controller", "parked-decision on INFRA-3686", {
          kind: "parked-decision",
          issue: "INFRA-3686",
          actor: reviewer,
          ref: "dispatch://INFRA-3686/ask/2e360d60-db20-4cbe-b300-0ddedfa9f328",
          decision: "answered",
          root: "INFRA-3686",
          at: "2026-11-14T21:03:00Z",
          words: { answer: { by: reviewer, at: "2026-11-14T21:03:00Z", more: true } },
        }),
        reader
      ).content
    );
    expect(standIn.envoy.message).not.toHaveProperty("answer");
    expect(standIn.blocks).toEqual([{ tag: "answer", name: "answer", text: "", more: true }]);
  });

  test("a daemon card shows its fields as lines and keeps its summary", () => {
    // A checks-red to the architect, as a daemon before this change wrote it: its role names the
    // reader, whose topic ends in that role.
    const checksRed = {
      kind: "checks-red",
      role: "architect",
      phase: "testing",
      issue: "INFRA-3686",
      conflicts_with: "main",
      head: "5c2b01d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3",
      pull_request: "example-org/example-repo#4211",
    };
    // A notice that names no issue, as a daemon before this change writes it: the summary names
    // the issue alone.
    const unkeyed = {
      kind: "phase-finished",
      role: "implementer",
      phase: "implementing",
      summary: "Done: the card renders.",
    };
    // A CI settlement, a role message published as an object.
    const settled = {
      kind: "ci-settled",
      issue: "INFRA-3686",
      pull_request: "example-org/example-repo#4211",
      head: "5c2b01d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3",
      phase: "implementing",
      not_passed: [
        { check: "daemon-go", conclusion: "failure" },
        { check: "ci-ok", conclusion: "failure" },
      ],
      passed: 12,
      list_passed: "gh pr checks 4211 --repo example-org/example-repo",
    };
    const cards = [
      {
        topic: `notifications.role.${architectRole}`,
        summary: "checks-red on INFRA-3686",
        payload: checksRed,
        shown: { ...checksRed, role: undefined },
        readerRole: "architect",
      },
      {
        topic: `notifications.role.${architectRole}`,
        summary: "phase-finished on INFRA-3686",
        payload: unkeyed,
        shown: unkeyed,
        readerRole: "architect",
      },
      {
        topic: "notifications.role.legion-infra-infra-3686-implementer",
        summary: "ci-settled on INFRA-3686",
        payload: settled,
        shown: settled,
        readerRole: "implementer",
      },
    ];

    for (const { topic, summary, payload, shown, readerRole } of cards) {
      const content = renderInbound(daemonMessage(topic, summary, payload), reader, topic).content;
      const card = cardOf<Record<string, unknown>>(content);

      expect(card.envoy.from, summary).toBe("agent");
      expect(card.envoy.summary, summary).toBe(`${payload.kind} on INFRA-3686`);
      expect(card.envoy.message, summary).toEqual(JSON.parse(JSON.stringify(shown)));
      // Each field is a line of the encoder's form under `message:`, and no value is one quoted
      // string with its new lines escaped.
      const lines = encode({ message: JSON.parse(JSON.stringify(shown)) })
        .split("\n")
        .map((line) => `  ${line}`)
        .join("\n");
      expect(content, summary).toContain(lines);
      expect(content, summary).not.toContain("\\n");
      expect(card.blocks, summary).toEqual([]);
      // No field names the reader's own role.
      expect(content, summary).not.toMatch(new RegExp(`^ +role: ${readerRole}$`, "m"));
    }
    const settledCard = renderInbound(
      daemonMessage(
        "notifications.role.legion-infra-infra-3686-implementer",
        "ci-settled on INFRA-3686",
        settled
      ),
      reader
    ).content;
    expect(settledCard).toContain(
      "    not_passed[2]{check,conclusion}:\n      daemon-go,failure\n"
    );

    // A role message published as text, the hook prompt, is drawn after the fields as its own
    // lines, in a block of its own.
    const hook =
      "Legion hook backward on INFRA-3686: work went back from testing to implementing.\n" +
      "Read the tester's handoff first.\ncount: 2";
    const prompted = cardOf<Record<string, unknown>>(
      renderInbound(
        daemonMessage(
          "notifications.role.legion-infra-infra-3686-implementer",
          "hook-prompt on INFRA-3686",
          hook
        ),
        reader
      ).content
    );
    expect(prompted.envoy.summary).toBe("hook-prompt on INFRA-3686");
    expect(prompted.envoy).not.toHaveProperty("message");
    expect(prompted.blocks).toEqual([{ tag: "message", name: "message", text: hook }]);
  });

  test("a Dispatch card keeps its reply command", () => {
    const card = cardOf<Record<string, unknown>>(
      renderInbound(dispatchEvent("comment.created", { ...comment, ask_id: null }), reader).content
    );

    expect(card.envoy.reply_with).toEqual({
      command: "dispatch comment --issue DSP-1 --body ... --reply-to comment-1",
    });
  });

  /** One sample of the daemon's golden file of every text (`goldenSample`,
   *  packages/daemon/internal/daemon/texts_golden_test.go). */
  interface GoldenSample {
    readonly kind: string;
    readonly variant: string;
    readonly sample: number;
    readonly reader: string;
    readonly form: "task" | "notice" | "controller-notice" | "role-message";
    readonly urgency: string;
    readonly issue: string;
    /** A task: its text, and the spans of R1's blocks and of the sentences R8 keeps in it. */
    readonly text?: string;
    readonly spans?: readonly GoldenSpan[];
    /** A notice or a role message: its summary, and the payload the daemon publishes: an object,
     *  or the hook prompt's text. */
    readonly summary?: string;
    readonly payload?: string | GoldenPayload;
  }

  /** A payload the daemon publishes as an object; its `words` names each field that holds another
   *  author's words by its path in the payload. */
  interface GoldenPayload {
    readonly words?: Readonly<Record<string, GoldenWords>>;
    readonly [field: string]: unknown;
  }

  /** A part of a task's text, from `start` to `end` in UTF-16 units: a block, or a kept sentence. */
  interface GoldenSpan {
    readonly part: "block" | "kept";
    readonly name?: string;
    readonly start: number;
    readonly end: number;
  }

  /** The author and the time of the words of one field, as a payload's `words` object names them. */
  interface GoldenWords {
    readonly by: string;
    readonly at: string;
    readonly rest?: number;
    readonly more?: boolean;
  }

  /** The objects that hold the field at `keys` in `value`, from `value` to the field's own object. */
  function holdersOf(
    value: unknown,
    keys: readonly (string | number)[]
  ): Readonly<Record<string, unknown>>[] {
    const holders: Readonly<Record<string, unknown>>[] = [];
    let at = value;
    for (const key of keys) {
      if (typeof at !== "object" || at === null) break;
      // A JSON value here: an object, or a list that a number indexes.
      const fields = at as Readonly<Record<string | number, unknown>>;
      if (!Array.isArray(at)) holders.push(fields);
      at = fields[key];
    }
    return holders;
  }

  /** `value` less each field whose payload path is in `paths`. */
  function without(value: unknown, paths: ReadonlySet<string>, path = ""): unknown {
    if (Array.isArray(value)) {
      return value.map((item, index) => without(item, paths, `${path}[${index}]`));
    }
    if (typeof value !== "object" || value === null) return value;
    return Object.fromEntries(
      Object.entries(value).flatMap(([key, item]): [string, unknown][] => {
        const at = path === "" ? key : `${path}.${key}`;
        return paths.has(at) ? [] : [[key, without(item, paths, at)]];
      })
    );
  }

  /** A key as TOON's encoder writes it: a bare name, or a quoted one. */
  const KEY = String.raw`(?:[A-Za-z_][\w.]*|"(?:[^"\\]|\\.)*")`;
  /** `key: value`, or `key:` that opens an object on the lines below it. */
  const FIELD_LINE = new RegExp(`^${KEY}:(?: .+)?$`);
  /** A list's header: `key[n]:`, its items on the line or below it, or `key[n]{fields}:`, its rows
   *  below it. */
  const LIST_HEADER = new RegExp(`^${KEY}\\[(\\d+)\\](\\{[^}]+\\})?:(?: .+)?$`);
  /** A `\n` escape in a quoted value: a backslash and `n` after no backslash or after escaped ones. */
  const ESCAPED_NEW_LINE = /(?<!\\)(?:\\\\)*\\n/;

  /** Each of `lines` that is of no form TOON's encoder writes: a key: value line, a list's header,
   *  or a list's row or item. */
  function notEncoderLines(lines: readonly string[]): string[] {
    const odd: string[] = [];
    // The indent of the last table's header, and how many of its rows are still to come.
    let tableIndent = 0;
    let rows = 0;
    for (const line of lines) {
      const indent = line.length - line.trimStart().length;
      const text = line.slice(indent);
      if (rows > 0 && indent > tableIndent && text !== "") {
        rows -= 1;
        continue;
      }
      const item = text.startsWith("- ") ? text.slice(2) : undefined;
      const header = LIST_HEADER.exec(item ?? text);
      if (header?.[2] !== undefined) {
        tableIndent = indent;
        rows = Number(header[1]);
      }
      if (header === null && item === undefined && !FIELD_LINE.test(text)) odd.push(line);
    }
    return odd;
  }

  test("a daemon card renders the golden payloads", () => {
    // Every task, notice and role message of the daemon, in each variant of its builders, twice,
    // with hostile values in titles, words and check names (TestEveryKindOnTheListStatesItsFields
    // writes the file).
    const golden = JSON.parse(
      readFileSync(
        new URL("../../../daemon/internal/daemon/testdata/texts.golden.json", import.meta.url),
        "utf8"
      )
    ) as { readonly samples: readonly GoldenSample[] };
    const cards = new Set<string>();
    let keptDecisions = 0;

    for (const sample of golden.samples) {
      const label = `${sample.kind}/${sample.variant}/${sample.sample}`;
      if (sample.form === "task") {
        // A task is TOON, R1's blocks and the sentences R8 keeps aside: each of those is whole
        // lines, and what is left decodes and encodes again to the same text.
        const text = sample.text ?? "";
        const outside: string[] = [];
        let from = 0;
        for (const span of [...(sample.spans ?? [])].sort((a, b) => a.start - b.start)) {
          const marked = text.slice(span.start, span.end);
          const opensLine = span.start === 0 || text[span.start - 1] === "\n";
          const endsLine = span.end === text.length || text[span.end] === "\n";
          expect(opensLine && endsLine, `${label}: ${marked}`).toBe(true);
          if (span.part === "block") {
            expect(marked, label).toStartWith(`<${span.name}`);
            expect(marked, label).toEndWith(`</${span.name}>`);
          }
          outside.push(text.slice(from, span.start));
          from = span.end;
        }
        outside.push(text.slice(from));
        const fields = outside
          .map((part) => part.replace(/^\n|\n$/g, ""))
          .filter((part) => part !== "")
          .join("\n");
        expect(encode(decode(fields)), label).toBe(fields);
        continue;
      }

      // The daemon publishes to its reader's topic: a role's lane, or the controller's.
      cards.add(sample.kind);
      const project = sample.issue.replace(/-\d+$/, "").toLowerCase();
      const topic = isLegionRole(sample.reader)
        ? roleTopic(roleToken(project, sample.issue, sample.reader))
        : legionControllerNoticeSubject(project);
      const content = renderInbound(
        daemonMessage(topic, sample.summary ?? "", sample.payload, sample.urgency),
        reader,
        topic
      ).content;
      // The reader throws where anything but blocks follows the first block.
      const { head, blocks } = splitWordsBlocks(content);
      const { envoy } = decode(head) as { readonly envoy: Record<string, unknown> };
      expect(envoy.from, label).toBe("agent");
      expect(envoy.urgency, label).toBe(sample.urgency);
      expect(envoy.summary, label).toBe(`${sample.kind} on ${sample.issue}`);
      expect(head, label).not.toMatch(ESCAPED_NEW_LINE);

      if (typeof sample.payload === "string") {
        // The hook prompt, published as its text: the `Legion hook` first line, a blank line, the
        // hook's words as written, a blank line, and its facts as lines of the encoder. The card
        // draws the text after its fields, whole and unescaped, its parts in that order.
        const text = sample.payload;
        const first = text.slice(0, text.indexOf("\n\n"));
        const facts = text.slice(text.lastIndexOf("\n\n") + 2);
        const words = text.slice(first.length + 2, text.length - facts.length - 2);
        expect(first, label).toStartWith(`Legion hook `);
        expect(first, label).toContain(` on ${sample.issue}: work went back from `);
        expect(first, label).not.toContain("\n");
        expect(words, label).not.toBe("");
        expect(decode(facts), label).toMatchObject({ count: expect.any(Number) });
        expect(envoy, label).not.toHaveProperty("message");
        expect(
          blocks.map(({ name, text }) => ({ name, text })),
          label
        ).toEqual([{ name: "message", text }]);
        let from = head.length;
        for (const [part, said] of [
          ["first line", first],
          ["words", words],
          ["facts", facts],
        ] as const) {
          const index = content.indexOf(said, from);
          expect(index, `${label}: ${part}`).toBeGreaterThanOrEqual(from);
          from = index + said.length;
        }
        continue;
      }

      // A payload published as an object: its fields as lines under `message:`, less `words` and
      // less each field whose words `words` names, which is drawn in a block after the fields.
      if (sample.payload === undefined) throw new Error(`${label}: the sample has no payload`);
      const named = sample.payload.words ?? {};
      const paths = Object.keys(named);
      const shown = without(sample.payload, new Set(["words", ...paths]));
      expect(envoy.message, label).toEqual(shown);
      const lines = head.split("\n");
      const opens = lines.indexOf("  message:");
      const closes = lines.findIndex((line, index) => index > opens && !line.startsWith("    "));
      const fields = lines.slice(opens + 1, closes < 0 ? lines.length : closes);
      expect(notEncoderLines(fields), label).toEqual([]);
      expect(["  message:", ...fields].join("\n"), label).toBe(
        encode({ message: shown })
          .split("\n")
          .map((line) => `  ${line}`)
          .join("\n")
      );

      expect(blocks.map(({ name, field }) => field ?? name).sort(), label).toEqual(paths.sort());
      for (const [path, entry] of Object.entries(named)) {
        const where = `${label}: ${path}`;
        // The keys of the path, such as `catch_up.kept.decisions[1].answer`, an index a number.
        const keys = Array.from(path.matchAll(/([^.[\]]+)|\[(\d+)\]/g), ([, key, index]) =>
          key === undefined ? Number(index) : key
        );
        const name = String(keys.at(-1));
        const block = blocks.find((said) => (said.field ?? said.name) === path);
        expect(block?.name, where).toBe(name);
        expect(block?.field, where).toBe(path === name ? undefined : path);
        expect<unknown>(block?.text, where).toBe(holdersOf(sample.payload, keys).at(-1)?.[name]);
        expect(block?.rest, where).toBe(entry.rest);
        // The author and the time of these words, from their own entry in `words`: on the block,
        // or as a field of an object that holds the words, such as the kept decision's own row,
        // or by the object that holds the words' field below the payload's own fields, where it
        // has a `by` or an `at` of its own, as the pause names who paused and when. That more
        // follows, from the entry too: on the block, unless that object says `more` itself, as a
        // kept decision's row does.
        const holders = holdersOf(envoy.message, keys);
        const own = holders.length > 1 ? holders.at(-1) : undefined;
        const ownBy = own?.["by"] !== undefined;
        const ownAt = own?.["at"] !== undefined;
        const ownMore = own?.["more"] === true;
        expect(block?.more, where).toBe(entry.more === true && !ownMore ? true : undefined);
        const held = holders
          .flatMap((holder) => Object.values(holder))
          .filter((value): value is string => typeof value === "string");
        if (ownBy) expect(block?.by, where).toBeUndefined();
        else expect(block?.by ?? held.find((value) => value === entry.by), where).toBe(entry.by);
        const time = Date.parse(entry.at);
        expect(time, where).not.toBeNaN();
        if (ownAt) expect(block?.at, where).toBeUndefined();
        else {
          const at =
            block?.at ??
            held.find((value) => /^\d{4}-\d{2}-\d{2}T/.test(value) && Date.parse(value) === time);
          expect(at === undefined ? undefined : Date.parse(at), where).toBe(time);
        }
      }
      if (sample.kind === "catch-up") {
        const kept = paths.filter((path) =>
          /^catch_up\.kept\.decisions\[\d+\]\.answer$/.test(path)
        );
        keptDecisions = Math.max(keptDecisions, kept.length);
      }
    }

    expect([...cards]).toEqual(
      expect.arrayContaining(["catch-up", "tick", "ci-settled", "update-ask", "hook-prompt"])
    );
    expect(keptDecisions).toBe(2);
  });
});
