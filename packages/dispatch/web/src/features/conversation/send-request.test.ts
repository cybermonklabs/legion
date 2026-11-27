import { beforeAll, expect, spyOn, test } from "bun:test";

import { ApiError, api } from "../../api/client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendInterruptedError, SendStoppedError } from "../writes/keyed-write";
import { requestFor, type SentRequest, sendAttempt } from "./send-request";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** A message to the Planner's session, as Send froze it, under key `send-1`. */
const refused: SentRequest = {
  anchor: undefined,
  ask: { multiple: false, options: [], urgency: "med" },
  draft: { body: "Status please", namesParty: false, replacement: "" },
  edit: undefined,
  kind: "comment",
  owner: { kind: "session", sessionId: "planner-session" },
  pickedDelivery: "aside",
  plainDelivery: "steer",
  replyTo: null,
  sendKey: "send-1",
};

/** The same send as the composer shows it now, with `draft` over its draft. */
function shown(draft: Partial<SentRequest["draft"]> = {}): Omit<SentRequest, "sendKey"> {
  const { sendKey: _key, pickedDelivery: _picked, ...rest } = refused;
  return { ...rest, draft: { ...refused.draft, ...draft } };
}

const verdict = new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused it" });

// A send that ended without Dispatch's verdict - the reader stopped it, or a reload interrupted it
// too long ago - may have landed, so its unedited Retry is that request itself, key included, and
// Dispatch answers it with what the first did.
test("an unedited Retry after Stop trying or an interrupted reload is the refused request, key and all", () => {
  for (const refusal of [new SendStoppedError(), new SendInterruptedError()]) {
    expect(requestFor(refused, refusal, shown())).toBe(refused);
  }
});

// After Stop trying, an edit that leaves the message on the wire what it was - here a
// suggestion's replacement, which a session message never carries - is still that one write,
// under its key; an edit of the words is another message, under a key of its own.
test("an edited Retry after Stop trying keeps the key only while the wire message is the same", () => {
  const stopped = new SendStoppedError();
  const sameWire = requestFor(refused, stopped, {
    ...shown({ replacement: "The replacement it would suggest" }),
    pickedDelivery: "aside",
  });
  expect(sameWire.sendKey).toBe("send-1");

  const otherWords = requestFor(refused, stopped, shown({ body: "Status please, now" }));
  expect(otherWords.sendKey).not.toBe("send-1");
  expect(otherWords.draft.body).toBe("Status please, now");
});

// Dispatch's refusal is its verdict on the key, which it would answer the same way for a day: a
// Retry after it is a new action under a new key. An unedited message to one session still goes
// as it was sent - its picked mode included - and an edited one as the composer shows it now.
test("a Retry after Dispatch's refusal goes under a new key, unedited or not", () => {
  const unedited = requestFor(refused, verdict, shown());
  expect(unedited.sendKey).not.toBe("send-1");
  expect({ ...unedited, sendKey: "send-1" }).toEqual(refused);

  const edited = requestFor(refused, verdict, shown({ body: "Status please, now" }));
  expect(edited.sendKey).not.toBe("send-1");
  expect(edited.pickedDelivery).toBeUndefined();
});

// A comment refused by Dispatch is sent again as the composer shows it now, in the modes its
// mentioned sessions advertise now, under a key of its own.
test("a comment's Retry after Dispatch's refusal is the request the composer shows now", () => {
  const comment: SentRequest = {
    ...refused,
    owner: { issueKey: "CORE-1", kind: "issue" },
    pickedDelivery: undefined,
  };
  const next = { ...shown(), owner: comment.owner, plainDelivery: "aside" as const };
  const retried = requestFor(comment, verdict, next);
  expect(retried.plainDelivery).toBe("aside");
  expect(retried.sendKey).not.toBe("send-1");
  expect(requestFor(undefined, undefined, next).sendKey).not.toBe("send-1");
});

// The issue page's box anchors a comment on a statement by the document and the statement's whole
// words alone: the server finds the words and anchors the comment to the block that holds them,
// so the request names no occurrence and no mark.
test("a comment on a statement is sent with its quote and no occurrence", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  try {
    const quote = "The box stays at the foot of the screen while the outline scrolls.";
    const sent: SentRequest = {
      ...refused,
      draft: { body: "Even on a phone?", namesParty: false, replacement: "" },
      owner: { issueKey: "CORE-1", kind: "issue" },
      pickedDelivery: undefined,
      statement: { artifact: "core-1-spec", quote, row: "core-1-spec#b-r1" },
    };
    const bounds = { key: sent.sendKey };
    await sendAttempt(sent)(bounds);

    expect(createComment.mock.calls).toStrictEqual([
      ["CORE-1", { anchor: { artifact: "core-1-spec", quote }, body: "Even on a phone?" }, bounds],
    ]);
  } finally {
    createComment.mockRestore();
  }
});

// A reply under an ask is a comment that names the ask by `ask_id`, the field Dispatch reads for
// it, and carries no `reply_to`, which Dispatch reads as a comment's id alone. A reply to a
// comment still names that comment by `reply_to`.
test("a reply under an ask names the ask by ask_id and no reply_to, and a reply to a comment names reply_to", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  try {
    const reply = (parentKind: "ask" | "comment"): SentRequest => ({
      ...refused,
      owner: { issueKey: "CORE-1", kind: "issue" },
      pickedDelivery: undefined,
      replyTo: {
        actor: { id: "alice", kind: "user" },
        excerpt: "",
        id: `${parentKind}-1`,
        parentKind,
      },
    });
    await sendAttempt(reply("ask"))({ key: "send-ask" });
    await sendAttempt(reply("comment"))({ key: "send-comment" });
    expect(createComment.mock.calls.map(([issueKey, input]) => [issueKey, input])).toStrictEqual([
      ["CORE-1", { ask_id: "ask-1", body: "Status please" }],
      ["CORE-1", { body: "Status please", reply_to: "comment-1" }],
    ]);
  } finally {
    createComment.mockRestore();
  }
});
