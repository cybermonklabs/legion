import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { type Envelope, legacyDispatchIssueSubject } from "@legion/contracts";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { decode } from "@toon-format/toon";
import { bearerCredential } from "../bearer";
import {
  type DispatchDelivery,
  INBOUND_CARD_MAX_BYTES,
  postDeliveryReply,
  renderInbound,
  replyWith,
  senderLabel,
} from "../delivery";
import { objectChainPastTheEncoder } from "./deep-json";

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

// `quiet` left out is a frame published before Dispatch stated it, which a reader judges by
// `notify` instead.
function dispatchEvent(
  type: string,
  payload: object,
  eventActor = actor,
  { notify = true, quiet }: { notify?: boolean; quiet?: boolean } = {}
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
        created_at: "2026-09-09T00:00:00Z",
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

  test("offers a reply hint only for an agent envelope with a source session", () => {
    expect(replyWith(envelope({ source: "agent", source_session: "ses_agent" }))).toEqual({
      tool: "envoy_send",
      args: { session_id: "ses_agent", in_reply_to: "event-1", message: "..." },
    });
    expect(replyWith(envelope({ source: "agent" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human", source_session: "ses_human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "github", source_session: "ses_github" }))).toBeUndefined();
    expect(
      replyWith(envelope({ source: "dispatch", source_session: "ses_dispatch" }))
    ).toBeUndefined();
  });
});

describe("renderInbound dispatch events", () => {
  test("renders ask.answered as the answer alone: no ask row, no restated owner", () => {
    const rendered = renderInbound(dispatchEvent("ask.answered", answeredAsk), reader);
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    // The asker wrote the question and `re:` names the ask, so the frame carries what moved
    // (who answered, which question, the answer) and never the ask row again.
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
        answer: "JSON - Use JSON HTTP.",
      },
    });
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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.answered",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: apiQuestion,
      answer: "MCP",
      previous_answer: "JSON - Use JSON HTTP.",
    });
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
    const decoded = decode(rendered.content) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toMatchObject({
      type: "ask.answered",
      question: apiQuestion,
      answer: "Can't - No access.",
    });
  });

  test("shows the ask's ref as 're:' when an answered ask is correlated to its ask id", () => {
    const correlated = JSON.parse(dispatchEvent("ask.answered", answeredAsk)) as Record<
      string,
      unknown
    >;
    const rendered = renderInbound(JSON.stringify({ ...correlated, in_reply_to: "ask-1" }), reader);
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };
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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(decoded.envoy.dispatch).toMatchObject({
      owner: "CORE-1",
      type: "message.created",
      actor: { kind: "user", id: "alice" },
      payload: { body: "Can this ship?" },
    });
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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

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

    const intended = decode(
      renderInbound(raw, reader, `notifications.agent.${reader}`).content
    ) as { envoy: { to?: string; dispatch: { payload: { target: string } } } };
    const other = decode(
      renderInbound(raw, otherSession, `notifications.agent.${reader}`).content
    ) as { envoy: { to?: string; dispatch: { payload: { target: string } } } };

    expect(intended.envoy.to).toBe("you (01a01111-2222-7333-4444-555555555555)");
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
      const intended = decode(renderInbound(raw, reader, topic).content) as {
        envoy: { to?: string };
      };
      const other = decode(renderInbound(raw, otherSession, topic).content) as {
        envoy: { to?: string };
      };

      expect(intended.envoy.to, topic).toBe(`you (${reader})`);
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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

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
        answer: "Neither; let's do a third thing.",
      },
    });
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

    const decoded = decode(
      renderInbound(dispatchEvent("ask.resolved", resolvedAsk), reader).content
    ) as {
      envoy: { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "ask.resolved",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: apiQuestion,
      resolved: "retracted: A newer question supersedes this one.",
    });
  });

  // `kind` is an open string now, so a kind this build has never seen parses rather than costing
  // the reader the card, and the words Dispatch wrote stand beside the person's own note on
  // every reader of the frame: this one, and a build older than this change, which declares no
  // `dispatch_words` of its own and prints the payload as it came.
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
      (
        decode(renderInbound(dispatchEvent("ask.resolved", payload), reader).content) as {
          envoy: { dispatch: Record<string, unknown> };
        }
      ).envoy.dispatch;

    // The reader that cannot parse the frame: `AskEventPayloadSchema` declares `edited_by` as
    // `z.never().optional()`, so a payload carrying that key is refused whatever its resolution.
    // Its card carries the raw payload, which is how a build older than this change reads both
    // parts of a decline.
    expect(card({ ...declined, edited_by: actor })).toMatchObject({
      payload: { resolution: { kind: "declined", reason: note, dispatch_words: dispatchWords } },
    });

    expect(card(declined).resolved).toBe(`declined: ${note} · Dispatch's words: ${dispatchWords}`);

    // A kind no build has ever seen reads as it came, and a resolution the person wrote no
    // reason for leaves no separator behind.
    expect(
      card({
        ...declined,
        resolution: { ...declined.resolution, kind: "handed_to_the_council", reason: "" },
      }).resolved
    ).toBe(`handed_to_the_council · Dispatch's words: ${dispatchWords}`);
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

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: Record<string, unknown> & { dispatch: Record<string, unknown> };
    };

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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

    expect(rendered.delivery).toBeUndefined();

    expect(decoded.envoy.reply_with).toEqual({
      command: "dispatch comment --issue DSP-1 --body ... --reply-to comment-1",
    });
  });

  // The hint a card drops under `expects_reply: false` is a Dispatch frame's own reply command
  // as much as an agent's `envoy_send`, and the mention below is the one Dispatch frame its
  // producer gives that value: a comment joining no ask thread and answering no comment
  // (`commentExpectsReply`, packages/envoy/internal/dispatch/api/comment_delivery.go). Every
  // other case for the suppression is an agent envelope, so a fix that dropped `replyWith`
  // alone and let the command through would pass them all; this pair is what fails it.
  describe("the reply command on a mention whose sender states the reply it wants", () => {
    const mention = { ...targetedComment, ask_id: null, reply_to: null };
    const card = (expects_reply: unknown) =>
      decode(
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
      ) as { envoy: Record<string, unknown> & { dispatch: { payload: Record<string, unknown> } } };

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

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: Record<string, unknown>;
    };

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
            ask_id: askID,
            ask_question: apiQuestion,
            ask_state: "open",
          },
        }),
      })
    );

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: { dispatch: Record<string, unknown> };
    };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "comment.created",
      actor: { kind: "user", id: "alice" },
      question: apiQuestion,
      reply: "Please update this.",
      state: "open",
    });
  });

  test("an agent's progress note on an ask renders as the reply with whose turn it is", () => {
    const decoded = decode(
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
    ) as { envoy: { dispatch: Record<string, unknown> } };

    expect(decoded.envoy.dispatch).toEqual({
      owner: "DSP-1",
      type: "comment.created",
      actor: { kind: "session", id: "session-1" },
      ask: "dispatch://DSP-1/ask/ask-1",
      question: releaseQuestion,
      reply: "Please update this.",
      state: "open",
      waiting_on: "agent",
    });
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
    type Card = { envoy: Record<string, unknown> & { dispatch: Record<string, unknown> } };
    const card = (raw: string, session: string) =>
      decode(renderInbound(raw, session).content) as Card;
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
      expect(decoded.envoy.dispatch.reply).toBe("Which pool size do you need?");
      expect(decoded.envoy.dispatch.state).toBe("open");
    });

    // The outbox's stand-in for an event the bus refused keeps ask_sessions whole, whatever its
    // size, so the session reading it decides from it as it does from the whole event, and a host
    // that reads the body whole and renders again changes nothing of that.
    test("a stand-in that carries ask_sessions is the owner's own ask and no other session's, before and after its body is read whole", () => {
      const { body: _body, ...kept } = askReply;
      const raw = copy({ ...kept, payload_omitted: true });

      for (const wholeBody of [undefined, askReply.body]) {
        const owner = decode(renderInbound(raw, asker, undefined, wholeBody).content) as Card;
        expect(owner.envoy.dispatch.relation).toBe("replied on your ask");
        expect(owner.envoy.reply_with).toEqual(replyToAsk);
        expect(owner.envoy.dispatch.waiting_on).toBe("agent");

        const other = decode(renderInbound(raw, reader, undefined, wholeBody).content) as Card;
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

    // A stand-in with no list is one a Dispatch older than ask_sessions sent: the card is the one
    // this renderer gave a stand-in before it read the field, byte for byte.
    test("a stand-in that carries no ask_sessions shows the card it showed before, with no relation", () => {
      const { body: _body, ask_sessions: _sessions, ...older } = askReply;

      expect(renderInbound(copy({ ...older, payload_omitted: true }), reader).content).toBe(
        [
          "envoy:",
          "  from: dispatch",
          '  at: "1970-01-01T00:00:00Z"',
          "  id: dispatch-ask-reply",
          '  re: "dispatch://CORE-1/ask/ask-1"',
          "  reply_with:",
          "    command: dispatch comment --issue CORE-1 --body ... --reply-to-ask ask-1",
          "  dispatch:",
          "    owner: CORE-1",
          "    type: comment.created",
          "    actor:",
          "      kind: user",
          "      id: alice",
          "    question: The service needs a public API. Which approach should we use?",
          '    reply: ""',
          "    state: open",
          "    waiting_on: agent",
          `  body_note: "this event carries no body: it was too large for the bus; read it whole with GET /api/v1/comments/${targetedCommentID}"`,
        ].join("\n")
      );
    });

    // A Dispatch older than `ask_sessions` wrote this reply, or a resend replayed one stored
    // before it: the card is the one this renderer gave before it read the field, byte for byte.
    test("a reply that carries no ask_sessions shows the card it showed before, with no relation", () => {
      const { ask_sessions: _sessions, ...older } = askReply;

      expect(renderInbound(copy(older), reader).content).toBe(
        [
          "envoy:",
          "  from: dispatch",
          '  at: "1970-01-01T00:00:00Z"',
          "  id: dispatch-ask-reply",
          '  re: "dispatch://CORE-1/ask/ask-1"',
          "  reply_with:",
          "    command: dispatch comment --issue CORE-1 --body ... --reply-to-ask ask-1",
          "  dispatch:",
          "    owner: CORE-1",
          "    type: comment.created",
          "    actor:",
          "      kind: user",
          "      id: alice",
          "    question: The service needs a public API. Which approach should we use?",
          "    reply: Which pool size do you need?",
          "    state: open",
          "    waiting_on: agent",
        ].join("\n")
      );
    });

    test("the ask's own sessions the payload names are no foreign session to note", () => {
      const follower = "01a03333-4444-7555-8666-777777777777";
      const owned = { ...askReply, ask_sessions: [asker, follower] };

      expect(card(copy(owned), reader).envoy.note).toBeUndefined();
      // Another session the body names is still noted.
      const foreign = "01a04444-5555-7666-8777-888888888888";
      expect(card(copy({ ...owned, body: `Ask ${foreign} first.` }), reader).envoy.note).toBe(
        `body names session ${foreign}; the sender is unknown`
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

    const decoded = decode(renderInbound(raw, reader).content) as {
      envoy: Record<string, unknown> & { dispatch: { payload: Record<string, unknown> } };
    };

    expect(decoded.envoy.re).toBe("dispatch://DSP-1/message/message-1");
    expect(decoded.envoy.dispatch.payload.reply_body).toBe("Ship the build tonight.");
  });

  test("types each Dispatch event's nested payload by its wire-contract schema", () => {
    const cases: Array<{ type: string; payload: object; expectedPayload: unknown }> = [
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
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "comment.reopened",
        payload: { ...comment, resolved: false },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "comment.edited",
        payload: { ...comment },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "comment.anchor_refreshed",
        payload: { ...comment, anchor: { ...comment.anchor, orphaned: true } },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { artifact_id: "artifact-1", mark_id: "m-1", orphaned: true },
          suggestion: null,
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
      },
      {
        type: "suggestion.accepted",
        payload: { ...comment, suggestion: { replace_with: "new line", accepted: true } },
        expectedPayload: {
          id: "comment-1",
          artifact_name: "spec.md",
          body: "Please update this.",
          reply_to: "comment-0",
          anchor: { quote: "old line" },
          suggestion: { replace_with: "new line" },
          author: actor,
          created_at: "2026-09-09T00:00:00Z",
        },
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
        expectedPayload: { id: "message-1", author: actor, body: "The build is green." },
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
          author: actor,
          body: "Sounds good.",
          in_reply_to: "message-1",
          reply_body: "The build is green.",
        },
      },
      {
        type: "child.status",
        payload: { child_key: "DSP-2", from: "todo", to: "in_progress" },
        expectedPayload: { child_key: "DSP-2", from: "todo", to: "in_progress" },
      },
    ];

    for (const { type, payload, expectedPayload } of cases) {
      const decoded = decode(renderInbound(dispatchEvent(type, payload), reader).content) as {
        envoy: { dispatch: Record<string, unknown> };
      };
      expect(decoded.envoy.dispatch).toMatchObject({
        owner: "DSP-1",
        type,
        actor: { kind: "session", id: "session-1" },
        payload: expectedPayload,
      });
    }
  });

  test("surfaces the comment id before the body, and the message id, in rendered dispatch payloads", () => {
    const commentContent = renderInbound(
      dispatchEvent("comment.created", { ...comment }),
      reader
    ).content;
    expect(commentContent).toContain("comment-1");
    expect(commentContent.indexOf("comment-1")).toBeLessThan(commentContent.indexOf("body:"));

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

  test("shows none of a dispatch payload it reads no event of, and says the payload was unrecognised", () => {
    // The edge listener passes a Dispatch frame only by the event readDispatchPayload reads, the
    // reader this plugin calls, so text around an event no reader judged is no card's to show:
    // another project's event nested beside the token's own project is the case it closes.
    const nested = {
      event: {
        issue_key: "PUB-1",
        type: "comment.created",
        actor,
        payload: { id: "c-1", body: "PUB's comment" },
      },
      project: "PRIV",
      issue_key: "PRIV-1",
    };
    for (const payload of [
      JSON.stringify({
        secret: "never shown", // fixture-secret: a field of an unrecognized Dispatch payload, never rendered; not a credential, never held
        unexpected: { nested: true },
      }),
      JSON.stringify(nested),
      "PUB's comment, no JSON",
    ]) {
      const rendered = renderInbound(
        JSON.stringify(envelope({ source: "dispatch", payload })),
        reader
      );
      const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };

      expect(decoded.envoy.dispatch).toBeUndefined();
      expect(decoded.envoy.unrecognised).toBe("payload");
      expect(rendered.content).not.toContain("never shown");
      expect(rendered.content).not.toContain("PUB");
    }
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

  test("preserves punctuation and newlines in a dispatch message body through TOON", () => {
    const body = 'First line: has "quotes", commas,\nand a second line.';
    const decoded = decode(
      renderInbound(dispatchEvent("message.created", { body }), reader).content
    ) as { envoy: { dispatch: { payload: { body: string } } } };

    expect(decoded.envoy.dispatch.payload.body).toBe(body);
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
    expect(decode(rendered.content)).toMatchObject({
      envoy: {
        dispatch: { type: "message.created", payload: { body: "Agent subject remains visible" } },
      },
    });
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
    expect(decode(rendered.content)).toMatchObject({
      envoy: {
        dispatch: { type: "message.created", payload: { body: "Role subject remains visible" } },
      },
    });
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
    expect(decode(rendered.content)).toMatchObject({
      envoy: {
        dispatch: { type: "comment.created", payload: { body: "Slice 3 is pushed." } },
      },
    });
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

  // The card's TOON encoder recurses once per level of a value, so a payload nested some
  // thousands of levels deep overflowed the stack and threw out of `renderInbound`, in every host
  // and on every delivery, or laid out in a card far larger than the payload. Such a payload is
  // the text its sender sent, and the card shows it so.
  describe("a payload the card cannot lay out", () => {
    const nested = `${'{"a":'.repeat(9_999)}1${"}".repeat(9_999)}`;
    // An object chain 100,000 deep, past the encoder's recursion and JSON.stringify's: one depth
    // for every row here, and the Bun measurement it rests on, in `objectChainPastTheEncoder`.
    const deeper = objectChainPastTheEncoder;
    // The card stops laying a value out once its lines pass the bound, so an object chain 9,999
    // deep is refused by the bound before the encoder's recursion reaches its bottom.
    const BOUND = "its layout is larger than the 1048576 bytes a card holds";
    // The bound a card's reason names (`payloadShownAsText`), or NaN where the reason names none,
    // which no byte count is within.
    const boundNamed = (reason: string | undefined) =>
      Number(/^its layout is larger than the (\d+) bytes a card holds$/.exec(reason ?? "")?.[1]);

    test("a message whose JSON payload is nested 9,999 deep is shown as the text its sender sent", () => {
      const rendered = renderInbound(
        JSON.stringify(
          envelope({ event_id: "evt-deep", payload_summary: "deep", payload: nested })
        ),
        reader
      );

      // The card is TOON this renderer wrote, read back as the other rows read theirs.
      const card = decode(rendered.content) as { envoy: Record<string, unknown> };
      expect(rendered.payloadShownAsText).toBe(BOUND);
      expect(card.envoy).toMatchObject({ id: "evt-deep", summary: "deep", message: nested });
    });

    // A Dispatch card the layout cannot hold shows its own `dispatch` block as JSON text - the
    // owner (`PRIV-1` from the event's `issue_key`), type, actor and payload - never the frame's
    // payload.
    test("a Dispatch event whose payload is nested 9,999 deep is shown as its text", () => {
      const deepEvent = `{"issue_key":"PRIV-1","type":"comment.created","actor":{"kind":"user","id":"alice"},"payload":${nested}}`;
      const rendered = renderInbound(
        JSON.stringify(
          envelope({ event_id: "dispatch-deep", source: "dispatch", payload: deepEvent })
        ),
        reader
      );

      const card = decode(rendered.content) as { envoy: Record<string, unknown> };
      expect(rendered.payloadShownAsText).toBe(BOUND);
      expect(card.envoy).toMatchObject({
        id: "dispatch-deep",
        dispatch: `{"owner":"PRIV-1","type":"comment.created","actor":{"kind":"user","id":"alice"},"payload":${nested}}`,
      });
    });

    // readDispatchPayload reads an event by the keys its schema names and drops the rest, so a key
    // beside the event's own reaches no card, the one shown as text among them.
    test("a Dispatch event the card cannot lay out shows no key its reader dropped", () => {
      const sibling = `{"issue_key":"PRIV-1","type":"comment.created","actor":{"kind":"user","id":"alice"},"event":{"project":"PUB","body":"PUB's comment"},"payload":{"body":"PRIV's comment","deep":${nested}}}`;
      const rendered = renderInbound(
        JSON.stringify(
          envelope({ event_id: "dispatch-sibling", source: "dispatch", payload: sibling })
        ),
        reader
      );

      expect(rendered.payloadShownAsText).toBe(BOUND);
      expect(rendered.content).toContain("PRIV's comment");
      expect(rendered.content).not.toContain("PUB");
    });

    // A cut comment whose payload the card cannot lay out, rendered again with its body read
    // whole, as a host renders it after reading that body (`cutItem`).
    const end = "the last words of the whole body.";
    const renderDeepCutComment = (eventId: string, wholeBody: string) =>
      renderInbound(
        JSON.stringify(
          envelope({
            event_id: eventId,
            source: "dispatch",
            payload: targetedCommentFrame({
              ...targetedComment,
              body: "The first paragraph.",
              body_truncated: true,
              deep: JSON.parse(nested),
            }),
          })
        ),
        reader,
        undefined,
        wholeBody
      );

    // A body read whole is never cut to the part the frame carried, in a card shown as text too.
    test("a cut Dispatch comment the card cannot lay out shows its body read whole", () => {
      const rendered = renderDeepCutComment("dispatch-deep-cut", `${"word ".repeat(2_000)}${end}`);

      expect(rendered.payloadShownAsText).toBe(BOUND);
      expect(rendered.content).toContain(end);
      expect(rendered.content).not.toContain("The first paragraph.");
    });

    // The text of an event holds a body read whole escaped twice, once by JSON and once by the
    // card: a quotation mark is four bytes there, as many as the bound gives it (twice TOON's two),
    // so a body of them brings such a card near its bound, and it stays within it.
    test("a cut Dispatch comment shown as text with a 1,048,300-byte body read whole stays within the bound", () => {
      const rendered = renderDeepCutComment(
        "dispatch-deep-cut-whole",
        `${'"'.repeat(1_048_300 - end.length)}${end}`
      );

      const bound = boundNamed(rendered.payloadShownAsText);
      const cardBytes = Buffer.byteLength(rendered.content);
      console.log(
        `a card shown as text with a body read whole: ${cardBytes} bytes, bound ${bound}`
      );
      expect(bound).toBeGreaterThan(INBOUND_CARD_MAX_BYTES);
      expect(cardBytes).toBeGreaterThan(4 * (1_048_300 - end.length));
      expect(cardBytes).toBeLessThanOrEqual(bound);
      expect(rendered.content).toContain(end);
    });

    // A Dispatch event JSON.stringify cannot write (a payload 100,000 deep) is shown by its owner,
    // its type and its actor's `kind` and `id` (and `act`, which this actor has none of): the
    // actor's other keys pass the schema, and an array chain 9,999 deep among them lays out in
    // about 100 MB, so the card writes none of them.
    test("a Dispatch event with an actor 9,999 deep beside a payload 100,000 deep is shown within the bound", () => {
      const arrays = `${"[".repeat(9_999)}${"]".repeat(9_999)}`;
      const raw = JSON.stringify(
        envelope({
          event_id: "dispatch-deep-actor",
          source: "dispatch",
          payload: `{"issue_key":"PRIV-1","type":"comment.created","actor":{"kind":"user","id":"alice","x":${arrays}},"payload":{"deep":${deeper}}}`,
        })
      );
      const rendered = renderInbound(raw, reader);

      const bound = Math.max(INBOUND_CARD_MAX_BYTES, 2 * Buffer.byteLength(raw));
      const cardBytes = Buffer.byteLength(rendered.content);
      console.log(
        `a card of an actor 9,999 deep beside a payload 100,000 deep: ${cardBytes} bytes, bound ${bound}`
      );
      expect(cardBytes).toBeLessThanOrEqual(bound);
      const card = decode(rendered.content) as { envoy: { dispatch: Record<string, unknown> } };
      expect(card.envoy.dispatch).toEqual({
        owner: "PRIV-1",
        type: "comment.created",
        actor: { kind: "user", id: "alice" },
        payload: `not shown: ${rendered.payloadShownAsText}`,
      });
    });

    // An actor's `act` is the agent or the personal token that acted for its person
    // (`ActorActSchema`), and a reader takes a user with no `act` for the person acting alone
    // (`actorActText`). A session actor's `service` is the subject of the verified service token
    // that made the write, which only named the session, and a reader takes a session with no
    // `service` for the session writing as itself (`actorText`). So a card that cannot write its
    // event as text keeps the act and the service beside the actor's `kind` and `id`, and says one
    // it cannot read is not shown; a session writing as itself is the control.
    test.each([
      [
        "a person's act an agent made shows that agent",
        {
          kind: "user",
          id: "alice",
          act: { session: "ses_x", session_proven: true, scopes: ["person"] },
        },
        {
          kind: "user",
          id: "alice",
          act: { session: "ses_x", session_proven: true, scopes: ["person"] },
        },
      ],
      [
        "an act its reader cannot read says it is not shown",
        { kind: "user", id: "alice", act: { session: "ses_x", origin: "broker" } },
        { kind: "user", id: "alice", act: "not shown" },
      ],
      [
        "a session's write a service token made shows that service",
        { kind: "session", id: "ses_y", service: "system:serviceaccount:legion:legion-worker" },
        { kind: "session", id: "ses_y", service: "system:serviceaccount:legion:legion-worker" },
      ],
      [
        "a service its reader cannot read says it is not shown",
        { kind: "session", id: "ses_y", service: { namespace: "legion" } },
        { kind: "session", id: "ses_y", service: "not shown" },
      ],
      [
        "an agent's own act shows the agent",
        { kind: "session", id: "ses_y" },
        { kind: "session", id: "ses_y" },
      ],
    ])("a Dispatch event the card cannot write as text: %s", (_name, eventActor, shownActor) => {
      const rendered = renderInbound(
        JSON.stringify(
          envelope({
            event_id: "dispatch-deep-act",
            source: "dispatch",
            payload: `{"issue_key":"PRIV-1","type":"comment.created","actor":${JSON.stringify(eventActor)},"payload":{"deep":${deeper}}}`,
          })
        ),
        reader
      );

      const card = decode(rendered.content) as { envoy: { dispatch: Record<string, unknown> } };
      expect(card.envoy.dispatch).toEqual({
        owner: "PRIV-1",
        type: "comment.created",
        actor: shownActor,
        payload: `not shown: ${rendered.payloadShownAsText}`,
      });
    });

    // The card's other fields repeat the event's issue key, and its reply command quotes the key
    // for a shell (each `'` written `'\''`, whose backslash the card escapes again), so a key of
    // quotes lays out in several times the frame's bytes, header and all. Such a card drops those
    // fields and keeps the owner (here the issue key), the type, the actor and the reason, within
    // the bound `payloadShownAsText` names.
    test("a Dispatch card whose header the bound cannot hold keeps its owner, type, actor and reason within the bound", () => {
      const key = "'".repeat(1_000_000);
      const raw = JSON.stringify(
        envelope({
          event_id: "dispatch-quoted-key",
          source: "dispatch",
          payload: JSON.stringify({
            issue_key: key,
            type: "comment.created",
            actor: { kind: "user", id: "alice" },
            payload: { id: "comment-9", body: "Slice 3 is pushed." },
          }),
        })
      );
      const rendered = renderInbound(raw, reader);

      const bound = boundNamed(rendered.payloadShownAsText);
      const cardBytes = Buffer.byteLength(rendered.content);
      console.log(
        `a card of a ${Buffer.byteLength(raw)}-byte frame whose issue key is quotes: ${cardBytes} bytes, bound ${bound}`
      );
      expect(cardBytes).toBeLessThanOrEqual(bound);
      const card = decode(rendered.content) as { envoy: Record<string, unknown> };
      expect(card.envoy).toEqual({
        dispatch: {
          owner: key,
          type: "comment.created",
          actor: { kind: "user", id: "alice" },
          payload: `not shown: ${rendered.payloadShownAsText}`,
        },
      });
    });

    // The owner of a document's event is read from a topic - here the one the host names, which
    // the frame does not bound - so a card may not hold even the owner; it then keeps the type, the
    // actor and the reason: the type and the actor are bytes of the frame, the reason is short, and
    // the bound is at least twice the frame.
    test("a Dispatch card that cannot hold even its owner keeps its type, actor and reason within the bound", () => {
      const slug = "s".repeat(2 * INBOUND_CARD_MAX_BYTES);
      const rendered = renderInbound(
        JSON.stringify(
          envelope({
            event_id: "dispatch-long-slug",
            source: "dispatch",
            payload: JSON.stringify({
              issue_key: null,
              type: "comment.created",
              actor: { kind: "user", id: "alice" },
              payload: { id: "comment-9", body: "Slice 3 is pushed." },
            }),
          })
        ),
        reader,
        `notifications.dispatch.document.PUB.${slug}.comment.created`
      );

      expect(rendered.payloadShownAsText).toBe(BOUND);
      expect(Buffer.byteLength(rendered.content)).toBeLessThanOrEqual(INBOUND_CARD_MAX_BYTES);
      expect(decode(rendered.content)).toEqual({
        envoy: {
          dispatch: {
            type: "comment.created",
            actor: { kind: "user", id: "alice" },
            payload: `not shown: ${BOUND}`,
          },
        },
      });
    });

    // A Dispatch payload readDispatchPayload reads no event of is shown as unrecognised and
    // none of its text, however deep: the edge passes a Dispatch frame only by the event it reads.
    test("a Dispatch payload nested 9,999 deep that is no event shows none of its text", () => {
      const rendered = renderInbound(
        JSON.stringify(
          envelope({ event_id: "dispatch-deep-raw", source: "dispatch", payload: nested })
        ),
        reader
      );

      const card = decode(rendered.content) as { envoy: Record<string, unknown> };
      expect(rendered.payloadShownAsText).toBeUndefined();
      expect(card.envoy).toMatchObject({ id: "dispatch-deep-raw", unrecognised: "payload" });
      expect(card.envoy.dispatch).toBeUndefined();
    });

    // The encoder reads the whole value before it lays out a line, with a recursion of its own:
    // an object chain 100,000 deep overflows the stack there, and the card shows it as its text.
    test("a message whose JSON payload is an object chain 100,000 deep, past the encoder's recursion, is shown as its text", () => {
      const rendered = renderInbound(
        JSON.stringify(envelope({ event_id: "evt-deeper", payload: deeper })),
        reader
      );

      const card = decode(rendered.content) as { envoy: Record<string, unknown> };
      expect(rendered.payloadShownAsText).toBe("Maximum call stack size exceeded.");
      expect(card.envoy).toMatchObject({ id: "evt-deeper", message: deeper });
    });

    // Each level indents every line below it, so an array chain the encoder reaches the bottom
    // of lays out in bytes that grow with the square of its depth: 9,999 levels, a 20 KB frame,
    // made a card of 100,060,107 bytes.
    test("a message whose JSON payload is an array chain 9,999 deep is shown as its text, in a card under the bound", () => {
      const arrays = `${"[".repeat(9_999)}${"]".repeat(9_999)}`;
      const rendered = renderInbound(
        JSON.stringify(envelope({ event_id: "evt-wide", payload: arrays })),
        reader
      );

      expect(Buffer.byteLength(rendered.content)).toBeLessThanOrEqual(INBOUND_CARD_MAX_BYTES);
      expect(rendered.payloadShownAsText).toBe(
        "its layout is larger than the 1048576 bytes a card holds"
      );
      const card = decode(rendered.content) as { envoy: Record<string, unknown> };
      expect(card.envoy).toMatchObject({ id: "evt-wide", message: arrays });
    });

    // The control: a layout under the bound is the card as before, an array chain 1,000 deep
    // (1,008,114 bytes) as much as any.
    test("a message whose JSON payload is an array chain 1,000 deep lays out under the bound as before", () => {
      const arrays = `${"[".repeat(1_000)}${"]".repeat(1_000)}`;
      const rendered = renderInbound(JSON.stringify(envelope({ payload: arrays })), reader);

      expect(rendered.payloadShownAsText).toBeUndefined();
      expect(rendered.content).toContain("\n  message[1]:\n    - [1]:\n      - [1]:\n");
      expect(rendered.content).not.toContain('message: "[');
    });

    // A payload that is text already is bounded by its frame, and the bound never touches it: a
    // card over it from a long summary beside a long text says nothing of a layout.
    test("a text payload is shown as text however large its card", () => {
      const words = "A line of a long review, with words and a full stop.\n";
      const rendered = renderInbound(
        JSON.stringify(
          envelope({
            payload_summary: words.repeat(11_000),
            payload: `Body.\n${words.repeat(11_000)}`,
          })
        ),
        reader
      );

      expect(Buffer.byteLength(rendered.content)).toBeGreaterThan(INBOUND_CARD_MAX_BYTES);
      expect(rendered.payloadShownAsText).toBeUndefined();
    });
  });

  test("renders direct agent metadata with a one-line summary and no payload", () => {
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

    expect(rendered.content).toBe(
      [
        "envoy:",
        "  to: you (01a01111-2222-7333-4444-555555555555)",
        `  from: ${sender} (Reviewer)`,
        '  at: "2026-09-07T04:41:12Z"',
        "  id: agent-message-2",
        '  by: "2026-09-07T05:00:00Z"',
        "  urgency: high",
        "  expects_reply: required",
        "  re: agent-message-1",
        "  supersedes: agent-message-0",
        "  reply_with:",
        "    tool: envoy_send",
        "    args:",
        `      session_id: ${sender}`,
        "      in_reply_to: agent-message-2",
        "      message: ...",
        "  reply_role:",
        "    tool: envoy_publish",
        "    args:",
        "      topic: notifications.role.legion-reviewer",
        "      message: ...",
        "  summary: First paragraph.",
      ].join("\n")
    );
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

  // A card that says nothing is wanted must not carry a ready-made way to send something: a
  // filled-in `envoy_send` under `expects_reply: false` invites the reader to do what the line
  // above it says the sender does not want, and a sender holding a role was handed a filled-in
  // `envoy_publish` as well. The suppression is tied to that one value: `optional` and
  // `required` keep both hints.
  describe("hints on a card whose sender states the reply it wants", () => {
    const sender = "01a0cccc-dddd-7eee-ffff-0123456789ab";
    const card = (expects_reply: unknown): string =>
      renderInbound(
        JSON.stringify({
          event_id: "agent-no-reply-role",
          source: "agent",
          source_session: sender,
          source_event_id: "agent.no-reply-role",
          topic: `notifications.agent.${reader}`,
          dedupe_key: "agent-no-reply-role",
          issued_at: Date.parse("2026-09-07T04:41:12Z"),
          payload_summary: "Status only.",
          trace_id: "trace-agent-no-reply-role",
          sender: { session_id: sender, title: "Reviewer", roles: ["legion-reviewer"] },
          expects_reply,
        }),
        reader
      ).content;

    const head = [
      "envoy:",
      `  to: you (${reader})`,
      `  from: ${sender} (Reviewer)`,
      '  at: "2026-09-07T04:41:12Z"',
      "  id: agent-no-reply-role",
    ];
    const hints = [
      "  reply_with:",
      "    tool: envoy_send",
      "    args:",
      `      session_id: ${sender}`,
      "      in_reply_to: agent-no-reply-role",
      "      message: ...",
      "  reply_role:",
      "    tool: envoy_publish",
      "    args:",
      "      topic: notifications.role.legion-reviewer",
      "      message: ...",
    ];
    const summary = "  summary: Status only.";
    const noHints = [...head, "  expects_reply: false", summary].join("\n");

    test("prints neither hint when the sender wants no reply", () => {
      expect(card(false)).toBe(noHints);
    });

    test("keeps both hints when a reply is optional", () => {
      expect(card("optional")).toBe(
        [...head, "  expects_reply: optional", ...hints, summary].join("\n")
      );
    });

    test("keeps both hints when a reply is required", () => {
      expect(card("required")).toBe(
        [...head, "  expects_reply: required", ...hints, summary].join("\n")
      );
    });

    // The retired spelling reads as `false`, so the normalisation and the suppression must
    // agree: a frame an older build left on the stream prints that one line and drops the
    // hints with it.
    test("prints neither hint for the retired spelling of no reply", () => {
      expect(card("none")).toBe(noHints);
    });
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

    expect(rendered.content).toBe(
      [
        "envoy:",
        `  from: ${sender}`,
        '  at: "2026-09-07T04:41:12Z"',
        "  id: agent-message-kept",
        '  kept_at: "2026-09-07T04:41:13Z"',
        "  reply_with:",
        "    tool: envoy_send",
        "    args:",
        `      session_id: ${sender}`,
        "      in_reply_to: agent-message-kept",
        "      message: ...",
        "  summary: The implementer is stalled.",
      ].join("\n")
    );
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

    expect(rendered.content).not.toMatch(/\n {2}summary:/);
    expect(rendered.content).toContain(`  message: "${body.replace(/\n/g, "\\n")}"`);
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

    expect(rendered.content).toContain(
      `  note: body names session ${foreign}; the sender is ${sender}`
    );
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

    expect(rendered.content).toContain("  summary: unknown");
    expect(rendered.content).toContain("  message: unknown");
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

    expect(rendered.content).toContain(
      `note: body names session ${foreign}; the sender is ${sender}`
    );
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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };
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
    const decoded = decode(rendered.content) as { envoy: Record<string, unknown> };
    expect(decoded.envoy.body_note).toBeUndefined();
  });

  // A host renders a cut comment again with the body Dispatch's API gave whole. That body comes
  // from no frame: one near Dispatch's 1 MiB request limit brings the card past 1 MiB, and the
  // card must still show it, not the part the frame carried.
  test("a comment rendered again with a 1,048,300-byte body read whole shows that body", () => {
    const end = "the last words of the whole body.";
    const wholeBody = `${"word ".repeat(209_700).slice(0, 1_048_300 - end.length)}${end}`;
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
      reader,
      undefined,
      wholeBody
    );

    expect(Buffer.byteLength(wholeBody)).toBe(1_048_300);
    expect({
      cardBytes: Buffer.byteLength(rendered.content),
      holdsTheWholeBody: rendered.content.includes(end),
      payloadShownAsText: rendered.payloadShownAsText,
    }).toEqual({
      cardBytes: expect.any(Number),
      holdsTheWholeBody: true,
      payloadShownAsText: undefined,
    });
    expect(Buffer.byteLength(rendered.content)).toBeGreaterThan(INBOUND_CARD_MAX_BYTES);
  });

  // TOON writes a control character as six bytes (`\u0001`), so a body read whole that is mostly
  // control characters lays out in about six times its bytes. The bound counts the body as the
  // card writes it, so such a body is still shown whole, not the part the frame carried.
  test("a 1,048,300-byte body read whole that is mostly control characters shows that body", () => {
    const end = "the last words of the whole body.";
    const wholeBody = `${"\u0001".repeat(1_048_300 - end.length)}${end}`;
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
      reader,
      undefined,
      wholeBody
    );

    expect(Buffer.byteLength(wholeBody)).toBe(1_048_300);
    expect({
      cardBytes: Buffer.byteLength(rendered.content),
      holdsTheWholeBody: rendered.content.includes(end),
      payloadShownAsText: rendered.payloadShownAsText,
    }).toEqual({
      cardBytes: expect.any(Number),
      holdsTheWholeBody: true,
      payloadShownAsText: undefined,
    });
    expect(Buffer.byteLength(rendered.content)).toBeGreaterThan(6 * (1_048_300 - end.length));
  });
});
