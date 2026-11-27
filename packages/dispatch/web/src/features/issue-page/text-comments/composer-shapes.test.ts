import { beforeAll, describe, expect, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { settle } from "../../../__tests__/fake-clock";
import { keptTab } from "../../../__tests__/kept-sends-harness";
import { api } from "../../../api/client";
import type { ComposerAnchor, ReplyTarget } from "../../conversation/composer-model";
import { draftSource } from "../../conversation/draft-pills";
import { heldOf, request } from "../../conversation/held-sends-fixture";
import { type SentRequest, sendAttempt } from "../../conversation/send-request";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { GATES, type GateName } from "./gates";
import {
  type ComposerAddress,
  composerAddressOf,
  type Place,
  type QuotedWords,
  type TextWriteTarget,
  writeFor,
} from "./text-comment-write";

/**
 * R5: what the composer of selected words sends, through the composer's own send
 * (`send-request.ts`, which `MentionComposer` sends through) from the anchor or the reply target
 * the node's write hands it (`composerAddressOf`), and what a kept draft of it sends after a
 * reload (`held-sends.ts`). Each case answers `api.createComment` or `api.createMessage`, as
 * `send-request.test.ts` does, and reads the body the send put on the wire; today's composers,
 * which set none of R5's fields, send the bytes they sent before.
 */

// A comment's parties are read from its words through the shared Markdown engine, which loads
// lazily, and the rows run in random order, so the file loads it before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const ISSUE = "CORE-1";
const BODY = "Why this word?";

/** Every gate on, so each kind's write stands, whatever this base holds. */
const EVERY_GATE = Object.fromEntries(Object.keys(GATES).map((name) => [name, true])) as Record<
  GateName,
  boolean
>;

/** The words a selection quotes: the second match of "draws" in the block `b2`. */
const WORDS: QuotedWords = { block: "b2", occurrence: 1, quote: "draws" };

/** The address the composer is handed for words of a node of `kind` standing in `place`. */
function addressOf(kind: string, place: Place, at: TextWriteTarget): ComposerAddress {
  const write = writeFor({ issue: ISSUE, kind, place, visibility: "project" }, EVERY_GATE);
  if (write === null) throw new Error(`no write stands on a ${kind} in ${place}`);
  const address = composerAddressOf(write, at, WORDS);
  if (address === null) throw new Error(`the ${kind}'s write names nothing`);
  return address;
}

/** A block of the spec, a statement, by its tree id and its block id. */
const STATEMENT: TextWriteTarget = {
  block_id: "r1",
  document: { artifact_id: "artifact-spec" },
  id: "spec-md#r1",
};

/** An item of its own: its id and its author. */
function item(id: string): TextWriteTarget {
  return { author: [{ id: "alice", kind: "user" }], id };
}

/** The composer's send of `BODY` as `MentionComposer` freezes it at Send: the held sends' request
 *  (`held-sends-fixture.ts`), on the issue's route, with its anchor and its reply target. */
function composed(
  { anchor, replyTo }: { anchor?: ComposerAnchor; replyTo?: ReplyTarget | null },
  sendKey = "send-1"
): SentRequest {
  return {
    ...request,
    anchor,
    draft: { body: BODY, replacement: "" },
    replyTo: replyTo ?? null,
    sendKey,
  };
}

/** One request a send put on the wire: its route, its issue and its body. */
interface Sent {
  readonly body: unknown;
  readonly issue: string;
  readonly route: "comments" | "messages";
}

/** The requests `request`'s send puts on the wire, one attempt of it. */
async function sentBy(request: SentRequest): Promise<Sent[]> {
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const createMessage = spyOn(api, "createMessage").mockResolvedValue(undefined as never);
  try {
    await sendAttempt(request)({ key: request.sendKey });
    return [
      ...createComment.mock.calls.map(([issue, body]) => ({
        body,
        issue,
        route: "comments" as const,
      })),
      ...createMessage.mock.calls.map(([issue, body]) => ({
        body,
        issue,
        route: "messages" as const,
      })),
    ];
  } finally {
    createComment.mockRestore();
    createMessage.mockRestore();
  }
}

test("a block anchor sends artifact, block_id, quote and occurrence", async () => {
  const request = composed(addressOf("requirement", "document", STATEMENT));
  expect(await sentBy(request), "the comment on a statement's words").toStrictEqual([
    {
      body: {
        anchor: { artifact: "artifact-spec", block_id: "r1", occurrence: 1, quote: "draws" },
        body: BODY,
      },
      issue: ISSUE,
      route: "comments",
    },
  ]);
});

// In an editor the anchor is the mark its selection bar wrote, since the server refuses
// `block_id`, `quote` or `occurrence` beside `mark_id` (decision 10).
test("an editor anchor sends artifact and mark_id alone", async () => {
  const request = composed({
    anchor: { artifact: "artifact-spec", mark_id: "m1", quote: "draws" },
  });
  expect(await sentBy(request), "the comment on an editor's mark").toStrictEqual([
    {
      body: { anchor: { artifact: "artifact-spec", mark_id: "m1" }, body: BODY },
      issue: ISSUE,
      route: "comments",
    },
  ]);
});

test("a node anchor sends node, quote and occurrence", async () => {
  const request = composed(addressOf("lens", "item", item("lens-1")));
  expect(await sentBy(request), "the comment on a lens's words").toStrictEqual([
    {
      body: { anchor: { node: "lens-1", occurrence: 1, quote: "draws" }, body: BODY },
      issue: ISSUE,
      route: "comments",
    },
  ]);
});

// A comment on a node's words names no document (`artifact: ""`), so a reference in it is read
// in the issue's spec, as one in a comment on the issue is, and never in a document of that name.
test("a node anchor reads its references in the issue's spec", () => {
  const { anchor, replyTo } = addressOf("lens", "item", item("lens-1"));
  expect(anchor?.node, "the lens's words are anchored to the lens").toBe("lens-1");
  expect(draftSource({ issueKey: ISSUE, kind: "issue" }, replyTo, anchor)).toStrictEqual({
    needsSpecOf: ISSUE,
  });
});

test("a reply to a comment's block sends reply_to, reply_to_block, quote and occurrence", async () => {
  const request = composed(addressOf("comment", "item", item("comment-1")));
  expect(await sentBy(request), "the reply to a comment's words").toStrictEqual([
    {
      body: {
        body: BODY,
        occurrence: 1,
        quote: "draws",
        reply_to: "comment-1",
        reply_to_block: "b2",
      },
      issue: ISSUE,
      route: "comments",
    },
  ]);
});

// A part of an ask is answered by a comment that names the ask, with the block it quotes.
test("a part of an ask sends ask_id", async () => {
  const request = composed(addressOf("ask", "item", item("ask-1")));
  expect(await sentBy(request), "the reply to an ask's words").toStrictEqual([
    {
      body: { ask_id: "ask-1", body: BODY, occurrence: 1, quote: "draws", reply_to_block: "b2" },
      issue: ISSUE,
      route: "comments",
    },
  ]);
});

test("a message reply sends in_reply_to, in_reply_to_block, quote and occurrence", async () => {
  const request = composed(addressOf("message", "item", item("message-1")));
  expect(await sentBy(request), "the reply to a message's words").toStrictEqual([
    {
      body: {
        body: BODY,
        in_reply_to: "message-1",
        in_reply_to_block: "b2",
        occurrence: 1,
        quote: "draws",
      },
      issue: ISSUE,
      route: "messages",
    },
  ]);
});

// A send out when the page reloads is kept in the tab and made again by the next page under its
// key (`held-sends.ts`): it must name the same words, so it sends the request it first sent.
test("a kept draft keeps its anchor over a reload", async () => {
  const sends: readonly (readonly [string, SentRequest])[] = [
    ["a block anchor", composed(addressOf("requirement", "document", STATEMENT), "send-block")],
    ["a node anchor", composed(addressOf("lens", "item", item("lens-1")), "send-node")],
    ["a comment's block", composed(addressOf("comment", "item", item("comment-1")), "send-reply")],
    ["an ask's block", composed(addressOf("ask", "item", item("ask-1")), "send-ask")],
    ["a message's block", composed(addressOf("message", "item", item("message-1")), "send-msg")],
  ];
  const name: MutationKey = ["text-comment", ISSUE, "the words"];
  for (const [what, request] of sends) {
    const tab = keptTab();
    const out = Promise.withResolvers<never>().promise;
    const createComment = spyOn(api, "createComment").mockReturnValue(out);
    const createMessage = spyOn(api, "createMessage").mockReturnValue(out);
    const wire = () => [...createComment.mock.calls, ...createMessage.mock.calls];
    try {
      heldOf(tab).held.send(name, request);
      await waitFor(() => expect(wire(), `${what}: the send never went out`).toHaveLength(1));
      const { held: after, writes } = heldOf(tab);
      writes.resume("alice");
      await settle();
      expect(after.get(name)?.request, `${what}: the reload kept another request`).toEqual(request);
      const [first, again] = wire();
      expect(again, `${what}: the reload sent nothing again`).toBeDefined();
      expect(again?.[1], `${what}: the reload sent another body`).toStrictEqual(first?.[1]);
      expect(again?.[2]?.key, `${what}: the reload sent under another key`).toBe(request.sendKey);
    } finally {
      createComment.mockRestore();
      createMessage.mockRestore();
      tab.close();
    }
  }
});

// One case per shape a composer of today sends - an editor's mark, a closed issue's quote, a
// comment on the issue, a reply to a comment, a reply under an ask, a reply to a message - each
// held to the bytes its request carried before R5's fields: none of them sets one.
describe("today's anchors send as before", () => {
  const actor = { id: "alice", kind: "user" } as const;
  const today: readonly (readonly [string, SentRequest, string])[] = [
    [
      "an editor's mark",
      composed({ anchor: { artifact: "artifact-spec", mark_id: "m1", quote: "draws" } }),
      `{"body":"${BODY}","anchor":{"artifact":"artifact-spec","mark_id":"m1"}}`,
    ],
    [
      "a closed issue's quote",
      composed({ anchor: { artifact: "artifact-spec", occurrence: 0, quote: "draws" } }),
      `{"body":"${BODY}","anchor":{"artifact":"artifact-spec","occurrence":0,"quote":"draws"}}`,
    ],
    ["a comment on the issue", composed({}), `{"body":"${BODY}"}`],
    [
      "a reply to a comment",
      composed({ replyTo: { actor, excerpt: "", id: "comment-1", parentKind: "comment" } }),
      `{"body":"${BODY}","reply_to":"comment-1"}`,
    ],
    [
      "a reply under an ask",
      composed({ replyTo: { actor, excerpt: "", id: "ask-1", parentKind: "ask" } }),
      `{"body":"${BODY}","ask_id":"ask-1"}`,
    ],
    [
      "a reply to a message",
      composed({ replyTo: { actor, excerpt: "", id: "message-1", parentKind: "message" } }),
      `{"body":"${BODY}","in_reply_to":"message-1"}`,
    ],
  ];
  for (const [what, request, bytes] of today) {
    test(`${what} sends the bytes it sent before`, async () => {
      const sent = await sentBy(request);
      expect(sent.length, `${what}: one request`).toBe(1);
      expect(JSON.stringify(sent[0]?.body), `${what}: the body on the wire`).toBe(bytes);
    });
  }
});
