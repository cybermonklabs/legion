import { expect, mock, spyOn, test } from "bun:test";
import { DISPATCH_WRITING_HINT_QUIET } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import { http } from "msw";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { onFakeClock } from "../../../__tests__/fake-clock";
import { answerRequests } from "../../../__tests__/network";
import { stopTheLateWrite } from "../../../__tests__/stop-late-write";
import { ApiError, api } from "../../../api/client";
import { whoAmIQuery } from "../../../api/queries";
import { pageKindBullet } from "../../../theme/classes";
import { KEY_HOURS } from "../../writes/write-words";
import type { TreeNode } from "../issue-tree";
import { NodeRow } from "../NodeRow";
import {
  criterionItemOf,
  criterionTakenBy,
  type OutlineWriter,
  OutlineWriterProvider,
  useOutlineWriter,
} from "../outline-writer";
import { CRITERION_STOPPED_WORDS } from "../page-words";
import {
  criterionMarkdown,
  criterionOffered,
  criterionRequest,
  useCriterionLine,
} from "./criterion-write";
import type { RoomFacts } from "./room-target";

/**
 * A criterion written under a node of the outline: its markdown, its one request on the tree
 * artifact, the line's text and answer as the write goes, and the box's aim, which no other
 * target ends while the write is out. Each request is answered at `fetch` (`answerRequests`), so
 * the write goes the whole way the page's does: the keyed write, the client, and the refusal the
 * client makes of Dispatch's answer. Last, the colour a criterion's row draws its bullet in.
 */

const KEY = "CORE-1";
const TREE = "artifact-tree";
const PARENT = `dispatch://${KEY}/comment/comment-1`;
const TEXT = "A reader can open the criterion under the comment it was written in.";

/** What Dispatch answers an edit that landed. */
const LANDED = {
  applied: 1,
  changed: true,
  lost_ops: null,
  token: "sha256:landed",
  unchanged_ops: null,
  version: null,
  written: "",
};

/** One request the tree artifact's edits route was sent. */
interface Sent {
  readonly body: unknown;
  readonly method: string;
  readonly path: string;
}

/** Answers `POST /api/v1/artifacts/{id}/edits` with `answer` from now on, and keeps each request
 *  it answers. A later call answers in its place. */
function answerEdits(answer: () => Response | Promise<Response>): Sent[] {
  const sent: Sent[] = [];
  answerRequests(
    http.post("/api/v1/artifacts/:id/edits", async ({ request }) => {
      sent.push({
        body: await request.json(),
        method: request.method,
        path: new URL(request.url).pathname,
      });
      return answer();
    })
  );
  return sent;
}

/** Dispatch's refusal, as the client reads its status and code. */
function refusal(status: number, code: string): Response {
  return Response.json({ code, error: `refused with ${code}` }, { status });
}

/** A client of its own signed in as alice, whose writes carry her login. */
function signedInClient(): QueryClient {
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** The criterion line under `PARENT`, on `client`, a client of its own signed in as alice. It
 *  tells `onOut` of its write going out and settling, as the box hears of it. */
function renderLine(
  onLanded: () => void,
  onOut: (out: boolean) => void = () => {},
  client = signedInClient()
) {
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  return renderHook(
    () => useCriterionLine({ issueKey: KEY, onLanded, onOut, parent: PARENT, tree: TREE }),
    { wrapper }
  );
}

/** One attempt of a write, as the client's `send` is handed it: its request, and the key it
 *  goes under. */
interface Attempt {
  readonly key: string;
  readonly request: unknown;
}

/** Answers each attempt of a write at the client's `send` with `answer()`, and keeps each one.
 *  A write the reader stops waits out its send deadline on the fake clock, and an answer at
 *  `fetch` comes on the real event loop, so the cases that stop a write answer a level above it,
 *  where each attempt tells its request as `send` does before it goes out. */
function answerSends(answer: () => Promise<unknown>): {
  readonly attempts: Attempt[];
  readonly restore: () => void;
} {
  const attempts: Attempt[] = [];
  const send = spyOn(api, "send").mockImplementation(((
    ...[request, bounds]: Parameters<typeof api.send>
  ) => {
    bounds.sent?.(request);
    attempts.push({ key: bounds.key, request });
    return answer();
  }) as typeof api.send);
  return { attempts, restore: () => send.mockRestore() };
}

/** Dispatch out of reach: an answer a write is tried again under its key after. */
const UNAVAILABLE = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });

/** What the line says of a write the reader stopped while Dispatch still holds its key: the
 *  page's own sentence, cut around the hours Dispatch keeps a key. */
const STOPPED_NOTE = `${CRITERION_STOPPED_WORDS.unconfirmed.before}${KEY_HOURS}${CRITERION_STOPPED_WORDS.unconfirmed.after}`;

/** A node of the outline, as the tree read draws it. */
function treeNode(partial: Partial<TreeNode> & Pick<TreeNode, "id" | "type">): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** The parent a criterion's opening line names, its character references decoded as both
 *  parsers decode them; `undefined` for a line that opens no criterion or carries anything but
 *  its parent, a block id among them. */
function parentOf(opener: string | undefined): string | undefined {
  const value = /^:::criterion\{parent="([^"]*)"\}$/u.exec(opener ?? "")?.[1];
  return value
    ?.replaceAll("&#x22;", '"')
    .replaceAll("&#xA;", "\n")
    .replaceAll("&#xD;", "\r")
    .replaceAll("&amp;", "&");
}

/** The typed text's lines as the block draws them: a paragraph draws no line's leading spaces,
 *  and the text's trailing whitespace is no part of it. */
function typedLines(typed: string): string[] {
  return typed
    .trimEnd()
    .split("\n")
    .map((line) => line.replace(/^[ \t]+/u, ""));
}

/** Each case: the text as typed, and the exact markdown its criterion is. */
const CASES: readonly { readonly typed: string; readonly markdown: string }[] = [
  {
    typed: `A reader opens dispatch://${KEY}/spec#b-r1\nand reads it whole.`,
    markdown: `:::criterion{parent="${PARENT}"}\nA reader opens dispatch://${KEY}/spec#b-r1\nand reads it whole.\n:::`,
  },
  { typed: "- a bullet", markdown: `:::criterion{parent="${PARENT}"}\n\\- a bullet\n:::` },
  { typed: "# a heading", markdown: `:::criterion{parent="${PARENT}"}\n\\# a heading\n:::` },
  { typed: "> a quote", markdown: `:::criterion{parent="${PARENT}"}\n\\> a quote\n:::` },
  { typed: "1. an item", markdown: `:::criterion{parent="${PARENT}"}\n1\\. an item\n:::` },
  { typed: "1) an item", markdown: `:::criterion{parent="${PARENT}"}\n1\\) an item\n:::` },
  {
    typed: "A line above\n===",
    markdown: `:::criterion{parent="${PARENT}"}\nA line above\n\\===\n:::`,
  },
  { typed: "    four spaces", markdown: `:::criterion{parent="${PARENT}"}\nfour spaces\n:::` },
  { typed: "::name", markdown: `:::criterion{parent="${PARENT}"}\n\\::name\n:::` },
  { typed: ":::", markdown: `:::criterion{parent="${PARENT}"}\n\\:::\n:::` },
  {
    typed: "One paragraph.\n\nAnother one.",
    markdown: `:::criterion{parent="${PARENT}"}\nOne paragraph.\n\nAnother one.\n:::`,
  },
  {
    typed: "* a star\n+ a plus\n  # an indented heading",
    markdown: `:::criterion{parent="${PARENT}"}\n\\* a star\n\\+ a plus\n\\# an indented heading\n:::`,
  },
  {
    typed: "```\ncode\n~~~",
    markdown: `:::criterion{parent="${PARENT}"}\n\\\`\`\`\ncode\n\\~~~\n:::`,
  },
  {
    typed: "| a | b |\na | b\n--|--",
    markdown: `:::criterion{parent="${PARENT}"}\n\\| a | b |\na | b\n\\--|--\n:::`,
  },
  {
    typed: "---\n***\n___",
    markdown: `:::criterion{parent="${PARENT}"}\n\\---\n\\***\n\\___\n:::`,
  },
  {
    typed: "[a]: https://example.com",
    markdown: `:::criterion{parent="${PARENT}"}\n\\[a]: https://example.com\n:::`,
  },
  {
    typed: "*Bold* words\n2024 was a year\n-1 is a number",
    markdown: `:::criterion{parent="${PARENT}"}\n*Bold* words\n2024 was a year\n-1 is a number\n:::`,
  },
  {
    typed: "Words, then spaces.  \n \n",
    markdown: `:::criterion{parent="${PARENT}"}\nWords, then spaces.\n:::`,
  },
  { typed: "<details>", markdown: `:::criterion{parent="${PARENT}"}\n\\<details>\n:::` },
];

test("the write names the node's address as parent and the text as typed", () => {
  for (const { markdown, typed } of CASES) {
    const written = criterionMarkdown(PARENT, typed);
    expect(written).toBe(markdown);
    const lines = written.split("\n");
    expect(parentOf(lines[0])).toBe(PARENT);
    expect(lines.at(-1)).toBe(":::");
    // Each start the rule escapes carries one backslash: before its first character, or after
    // an ordered item's digits. Taken off, the block's lines are the typed text.
    expect(lines.slice(1, -1).map((line) => line.replace(/^(\d*)\\/u, "$1"))).toEqual(
      typedLines(typed)
    );
  }
  // An address is the parent whole, whatever it holds: a quote and an `&` as character
  // references, which both parsers decode, and a backslash as itself, which neither reads.
  const odd = `dispatch://${KEY}/comment/a"b\\c&d`;
  const written = criterionMarkdown(odd, "Words.");
  expect(written).toBe(
    `:::criterion{parent="dispatch://${KEY}/comment/a&#x22;b\\c&amp;d"}\nWords.\n:::`
  );
  expect(parentOf(written.split("\n")[0])).toBe(odd);
});

test("a refusal keeps the text", async () => {
  const textAtLanding: string[] = [];
  // What the line told, in order: its write going out, its write settling, and a landing.
  const told: string[] = [];
  const line = renderLine(
    () => {
      told.push("landed");
      textAtLanding.push(line.result.current.text);
    },
    (out) => told.push(out ? "out" : "settled")
  );
  try {
    answerEdits(() => refusal(400, "INVALID_JSON"));
    act(() => line.result.current.setText(TEXT));
    act(() => line.result.current.send());
    await waitFor(() => expect(line.result.current.answer).toBe("criterion_not_written"));
    expect(line.result.current.text).toBe(TEXT);
    expect(line.result.current.pending).toBe(false);
    // The box held its aim while the write was out, and the refusal lets it go.
    expect(told).toEqual(["out", "settled"]);

    answerEdits(() => refusal(404, "NOT_FOUND"));
    act(() => line.result.current.send());
    await waitFor(() => expect(line.result.current.answer).toBe("criterion_gone"));
    expect(line.result.current.text).toBe(TEXT);
    expect(told).toEqual(["out", "settled", "out", "settled"]);

    // The next change of the text lets the answer go.
    act(() => line.result.current.setText(`${TEXT} Still.`));
    expect(line.result.current.answer).toBeUndefined();

    // While the write is out the text stays; its landing tells the line and then lets it go.
    const answered = Promise.withResolvers<Response>();
    answerEdits(() => answered.promise);
    act(() => line.result.current.send());
    expect(line.result.current.pending).toBe(true);
    expect(line.result.current.answer).toBeUndefined();
    expect(line.result.current.text).toBe(`${TEXT} Still.`);
    expect(textAtLanding).toEqual([]);
    await waitFor(() => expect(told).toHaveLength(5));
    expect(told[4]).toBe("out");
    answered.resolve(Response.json(LANDED));
    await waitFor(() => expect(line.result.current.text).toBe(""));
    expect(textAtLanding).toEqual([`${TEXT} Still.`]);
    expect(line.result.current.answer).toBeUndefined();
    expect(line.result.current.pending).toBe(false);
    // The box lets go of its aim before the landing ends it.
    expect(told.slice(4)).toEqual(["out", "settled", "landed"]);
  } finally {
    line.unmount();
  }
  // A write Dispatch made and kept no answer for (`409 KEY_COMMITTED`) lands all the same.
  const onLanded = mock(() => {});
  const onOut = mock((_out: boolean) => {});
  const unanswered = renderLine(onLanded, onOut);
  try {
    answerEdits(() => refusal(409, "KEY_COMMITTED"));
    act(() => unanswered.result.current.setText(TEXT));
    act(() => unanswered.result.current.send());
    await waitFor(() => expect(unanswered.result.current.text).toBe(""));
    expect(onLanded).toHaveBeenCalledTimes(1);
    expect(onOut.mock.calls).toEqual([[true], [false]]);
    expect(unanswered.result.current.answer).toBeUndefined();
  } finally {
    unanswered.unmount();
  }
});

// A write the reader stopped may have reached Dispatch, and an insert at the end of the tree
// matches nothing it already holds, so a second write under a new key would write the criterion
// twice. The line says the write is unconfirmed, in the page's own sentence, rather than that
// it was not written, and its Retry makes that very request again under the same key, which
// Dispatch answers with what the stopped write did there.
test("a stopped write says it is unconfirmed, and its Retry sends the same request under its key", async () => {
  let down = true;
  const sends = answerSends(() => (down ? Promise.reject(UNAVAILABLE) : Promise.resolve(LANDED)));
  const client = signedInClient();
  const onLanded = mock(() => {});
  const told: boolean[] = [];
  const line = renderLine(onLanded, (out) => told.push(out), client);
  try {
    act(() => line.result.current.setText(TEXT));
    await onFakeClock(async () => {
      act(() => line.result.current.send());
      await stopTheLateWrite(client);
    });
    expect(line.result.current.answer).toBeUndefined();
    expect(line.result.current.stopped).toBe(STOPPED_NOTE);
    expect(line.result.current.text).toBe(TEXT);
    expect(line.result.current.pending).toBe(false);
    // The stop settled the write, so the box lets go of its aim.
    expect(told).toEqual([true, false]);
    const request = criterionRequest(TREE, criterionMarkdown(PARENT, TEXT));
    const [first] = sends.attempts;
    expect(first).toEqual({ key: expect.any(String), request });
    const stopped = sends.attempts.length;

    down = false;
    act(() => line.result.current.retry());
    await waitFor(() => expect(onLanded).toHaveBeenCalledTimes(1));
    expect(sends.attempts.slice(stopped)).toEqual([{ key: first?.key, request }]);
    expect(line.result.current.text).toBe("");
    expect(line.result.current.stopped).toBeUndefined();
    expect(told).toEqual([true, false, true, false]);
  } finally {
    line.unmount();
    sends.restore();
  }
});

// Enter and Send while a stopped write waits for its Retry make no write under a new key: the
// send makes the stopped write again, as Retry does. The text is that write's own words, so it
// takes no change meanwhile, and the request sent is the one the key covers.
test("a send while a write is stopped makes that write again under its key, and the text takes no change", async () => {
  let down = true;
  const sends = answerSends(() => (down ? Promise.reject(UNAVAILABLE) : Promise.resolve(LANDED)));
  const client = signedInClient();
  const onLanded = mock(() => {});
  const line = renderLine(onLanded, undefined, client);
  try {
    act(() => line.result.current.setText(TEXT));
    await onFakeClock(async () => {
      act(() => line.result.current.send());
      await stopTheLateWrite(client);
    });
    const [first] = sends.attempts;
    const stopped = sends.attempts.length;

    act(() => line.result.current.setText(`${TEXT} Edited.`));
    expect(line.result.current.text).toBe(TEXT);
    expect(line.result.current.stopped).toBe(STOPPED_NOTE);

    down = false;
    act(() => line.result.current.send());
    await waitFor(() => expect(onLanded).toHaveBeenCalledTimes(1));
    expect(sends.attempts.slice(stopped)).toEqual([
      { key: first?.key, request: criterionRequest(TREE, criterionMarkdown(PARENT, TEXT)) },
    ]);
  } finally {
    line.unmount();
    sends.restore();
  }
});

/** The box's writer of `KEY`, held as each render hands it over, with its reads answered empty and
 *  no tree read of the issue held. */
function drawWriter() {
  const client = signedInClient();
  client.setQueryData(["asks", KEY, "thread"], []);
  client.setQueryData(["issue-messages", KEY], []);
  client.setQueryData(["agents"], []);
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(OutlineWriterProvider, { children, issueKey: KEY })
    );
  const box = renderHook(() => useOutlineWriter(), { wrapper });
  const writer = (): OutlineWriter => {
    const held = box.result.current;
    if (held === undefined) throw new Error("no writer is drawn");
    return held;
  };
  return { box, writer };
}

/** A person's comment a Reply aims the line at, which the writer reads alone for its mentions
 *  (`GET /api/v1/comments/{id}`) before it sets the line (`outline-writer.tsx`' `replyTo`). */
const REPLY = {
  author: { id: "bob", kind: "user" },
  id: "comment-2",
  kind: "comment",
  text: TEXT,
} as const;

/** What the read of `REPLY`'s comment answers: its author, and no mention. */
const REPLIED = { comment: { author: REPLY.author, mentions: [] }, replies: [] } as never;

// A criterion whose write is out holds the box. A row's Reply, `r` and `a`, another row's Write a
// criterion, an edit, Write on the issue and `clear` are each refused, as they are while a send
// of the line's own is out, so the line keeps its text and draws its refusal; once the write
// settles each takes the line again, a Reply once its comment's read answers.
test("a criterion out holds the box's other targets", async () => {
  const getComment = spyOn(api, "getComment").mockResolvedValue(REPLIED);
  const { box, writer } = drawWriter();
  const comment = treeNode({ id: "comment-1", reference: PARENT, type: "comment" });
  const other = treeNode({
    id: "message-1",
    reference: `dispatch://${KEY}/message/message-1`,
    type: "message",
  });
  const ask = treeNode({
    id: "ask-1",
    reference: `dispatch://${KEY}/ask/ask-1`,
    state: "open",
    type: "ask",
  });
  try {
    act(() => writer().criterionUnder(comment));
    expect(writer().criterion?.id).toBe("comment-1");

    act(() => writer().holdCriterion(true));
    act(() => writer().replyTo(REPLY));
    act(() => writer().answerNode(ask));
    act(() => writer().criterionUnder(other));
    act(() => writer().edit("comment-3", "Words."));
    act(() => writer().writeOnIssue());
    act(() => writer().clear());
    expect(writer().criterion?.id).toBe("comment-1");
    expect(writer().reply).toBeNull();
    expect(writer().answer).toBeUndefined();
    expect(writer().editing).toBeUndefined();

    act(() => writer().holdCriterion(false));
    act(() => writer().clear());
    expect(writer().criterion).toBeUndefined();
    act(() => writer().replyTo(REPLY));
    await waitFor(() => expect(writer().reply?.id).toBe("comment-2"));
  } finally {
    box.unmount();
    getComment.mockRestore();
  }
});

// A Reply to a person's comment sets the line once that comment's own read answers. Write a
// criterion pressed while the read is out aims the line, as any later press does, so the read's
// answer sets nothing and the criterion line stays.
test("a Reply whose comment is still being read sets nothing once Write a criterion aims the line", async () => {
  const read = Promise.withResolvers<never>();
  const getComment = spyOn(api, "getComment").mockReturnValue(read.promise);
  const { box, writer } = drawWriter();
  try {
    act(() => writer().replyTo(REPLY));
    act(() =>
      writer().criterionUnder(treeNode({ id: "comment-1", reference: PARENT, type: "comment" }))
    );
    await act(async () => read.resolve(REPLIED));
    expect(writer().criterion?.id, "the Reply's read ended the criterion line").toBe("comment-1");
    expect(writer().reply).toBeNull();
  } finally {
    box.unmount();
    getComment.mockRestore();
  }
});

// The criterion line's write going out while a Reply's comment is still being read holds the box
// as it holds every press made while the write is out: the read's answer sets nothing, so the line
// keeps its text and its refusal.
test("a Reply whose comment is still being read sets nothing once the criterion line's write is out", async () => {
  const read = Promise.withResolvers<never>();
  const getComment = spyOn(api, "getComment").mockReturnValue(read.promise);
  const { box, writer } = drawWriter();
  try {
    act(() =>
      writer().criterionUnder(treeNode({ id: "comment-1", reference: PARENT, type: "comment" }))
    );
    act(() => writer().replyTo(REPLY));
    act(() => writer().holdCriterion(true));
    await act(async () => read.resolve(REPLIED));
    expect(writer().criterion?.id, "the Reply's read ended the criterion line").toBe("comment-1");
    expect(writer().reply).toBeNull();
    act(() => writer().holdCriterion(false));
  } finally {
    box.unmount();
    getComment.mockRestore();
  }
});

// The criterion line's hint says where its words go, in words of its own, written out here rather
// than read from the list the line reads them from, and never the document editor's "A comment on
// the document", since the page draws no document there. Under a message a reply tells nobody it
// names, so the words say a comment on the issue is what tells. Until the page reads the issue's
// tree, no row offers Write a criterion and no control reaches the line, so its row is uncovered
// with a reason and draws no hint yet.
test("the criterion line's hint says it writes under what it answers", () => {
  expect(DISPATCH_WRITING_HINT_QUIET.criterion).toBe(
    "Type @ for an agent, a role or a person, # for an issue, [[ for anything else. A picked agent or role gets a link, and a picked person shows as a name without one. Nobody you pick is told. The criterion goes under the comment, question or message it answers. A comment on the issue tells a picked agent or role."
  );
});

test("the act needs view_of null and a tree", () => {
  const facts = (tree: string | null, viewOf: string | null, converted = true): RoomFacts => ({
    converted,
    issueKey: KEY,
    tree,
    viewOf,
  });
  expect(criterionOffered(facts(TREE, null))).toBe(true);
  expect(criterionOffered(facts(TREE, null, false))).toBe(true);
  expect(criterionOffered(facts(TREE, "CORE-2"))).toBe(false);
  expect(criterionOffered(facts(null, null))).toBe(false);
  expect(criterionOffered(undefined)).toBe(false);
});

test("a conversation node is a criterion's parent and no other is", () => {
  const node = (
    type: TreeNode["type"],
    reference: string | undefined,
    id = `row-${type}`
  ): Pick<TreeNode, "id" | "reference" | "text" | "type"> => ({
    id,
    ...(reference === undefined ? {} : { reference }),
    text: TEXT,
    type,
  });
  // None of the node's words go with it: the target line names the node by its author and kind.
  expect(criterionItemOf(node("comment", PARENT))).toEqual({
    address: PARENT,
    id: "row-comment",
    kind: "comment",
  });
  const message = `dispatch://${KEY}/message/message-1`;
  expect(criterionItemOf(node("message", message))?.kind).toBe("message");
  // An ask indexed from a block of a document keeps its row's id, the block's node id, which a
  // landing finds it by, and names the ask's own address as the parent.
  const ask = `dispatch://${KEY}/ask/ask-1`;
  expect(criterionItemOf(node("ask", ask, "spec#b-q1"))).toEqual({
    address: ask,
    id: "spec#b-q1",
    kind: "ask",
  });
  // Its author goes with it, for the target line to name the node by.
  const author = { id: "bob", kind: "user" } as const;
  expect(criterionItemOf({ ...node("comment", PARENT), author })).toEqual({
    address: PARENT,
    author,
    id: "row-comment",
    kind: "comment",
  });
  // A node of another issue is one too: whether the act is offered there is the writer's.
  expect(criterionItemOf(node("comment", "dispatch://CORE-2/comment/comment-2"))?.address).toBe(
    "dispatch://CORE-2/comment/comment-2"
  );
  // No address, an address that names no item, and a node of any other kind are none.
  expect(criterionItemOf(node("comment", undefined))).toBeUndefined();
  expect(criterionItemOf(node("comment", `dispatch://${KEY}/spec#b-r1`))).toBeUndefined();
  expect(criterionItemOf(node("comment", `dispatch://${KEY}`))).toBeUndefined();
  expect(criterionItemOf(node("requirement", PARENT))).toBeUndefined();
  expect(criterionItemOf(node("issue", `dispatch://${KEY}`, KEY))).toBeUndefined();
});

test("a conversation node of another issue offers no Write a criterion", () => {
  const node = (reference: string): Pick<TreeNode, "id" | "reference" | "text" | "type"> => ({
    id: "row-comment",
    reference,
    text: TEXT,
    type: "comment",
  });
  // The issue's tree read offers the act: a tree, and a view of no other issue.
  const offering: RoomFacts = { converted: true, issueKey: KEY, tree: TREE, viewOf: null };
  const own = node(PARENT);
  // A comment of another issue, such as a borrowed check's parent: it is a criterion's parent all
  // the same, and is written under in its own issue, never here.
  const borrowed = node("dispatch://CORE-2/comment/comment-2");
  expect(criterionItemOf(borrowed)).toBeDefined();
  expect(criterionTakenBy(own, KEY, offering)).toBe(true);
  expect(criterionTakenBy(borrowed, KEY, offering)).toBe(false);
  // With no tree read held, no node offers it.
  expect(criterionTakenBy(own, KEY, undefined)).toBe(false);
  expect(criterionTakenBy(borrowed, KEY, undefined)).toBe(false);
});

// Dispatch takes a criterion only under a comment or a message that still stands, so one its
// author deleted offers no Write a criterion, as it offers no Reply (`replyItemOf`), and the
// writer takes no aim at it: `criterionUnder` sets nothing where `criterionItemOf` answers none.
test("a comment or message its author deleted offers no Write a criterion", () => {
  const offering: RoomFacts = { converted: true, issueKey: KEY, tree: TREE, viewOf: null };
  const deletedAt = "2026-11-05T12:00:00Z";
  const by = { id: "bob", kind: "user" } as const;
  for (const standing of [
    treeNode({ author: by, id: "comment-1", reference: PARENT, text: TEXT, type: "comment" }),
    treeNode({
      author: by,
      id: "message-1",
      reference: `dispatch://${KEY}/message/message-1`,
      text: TEXT,
      type: "message",
    }),
  ]) {
    // The row of a deleted post as the tree read draws it: the deleted line for its words.
    const deleted: TreeNode = {
      ...standing,
      deletedAt,
      deletedBy: by,
      text: `Deleted by bob · ${deletedAt}`,
    };
    expect(criterionTakenBy(standing, KEY, offering), standing.id).toBe(true);
    expect(criterionItemOf(standing), standing.id).toBeDefined();
    expect(criterionTakenBy(deleted, KEY, offering), deleted.id).toBe(false);
    expect(criterionItemOf(deleted), deleted.id).toBeUndefined();
  }
});

test("a send carries the tree's edits route and one insert at the end", async () => {
  const markdown = criterionMarkdown(PARENT, TEXT);
  const route = `/api/v1/artifacts/${TREE}/edits`;
  const body = { ops: [{ after: "end", markdown, op: "insert" }] };
  expect(criterionRequest(TREE, markdown)).toEqual({ body, method: "POST", path: route });

  const answered = Promise.withResolvers<Response>();
  const sent = answerEdits(() => answered.promise);
  const onLanded = mock(() => {});
  const line = renderLine(onLanded);
  try {
    act(() => line.result.current.setText(TEXT));
    act(() => line.result.current.send());
    // A second send while the first is out makes no second write.
    act(() => line.result.current.send());
    await waitFor(() => expect(sent).toHaveLength(1));
    answered.resolve(Response.json(LANDED));
    await waitFor(() => expect(onLanded).toHaveBeenCalledTimes(1));
    expect(sent).toEqual([{ body, method: "POST", path: route }]);
  } finally {
    line.unmount();
  }
});

test("a blank text sends nothing", () => {
  const sent = answerEdits(() => Response.json(LANDED));
  const onLanded = mock(() => {});
  const line = renderLine(onLanded);
  try {
    // The line starts blank, and its Send's refusal reads that.
    expect(line.result.current.blank).toBe(true);
    // A send that makes its write is pending at once - the mutation's first step, before any
    // request goes - so a line still not pending after its send made none.
    for (const blank of ["", " \n\t \n"]) {
      act(() => line.result.current.setText(blank));
      expect(line.result.current.blank).toBe(true);
      act(() => line.result.current.send());
      expect(line.result.current.pending).toBe(false);
      expect(line.result.current.text).toBe(blank);
    }
    expect(sent).toEqual([]);
    expect(onLanded).not.toHaveBeenCalled();
    // Words with blank lines around them are no blank text: Send and Enter write them.
    act(() => line.result.current.setText(` \n${TEXT}\n `));
    expect(line.result.current.blank).toBe(false);
  } finally {
    line.unmount();
  }
});

// R1: a criterion's row draws its bullet in the statements' colour, wherever it stands. A
// criterion carries no `statement` mark, which only a requirement of the issue's spec does
// (`isStatement`), so a colour read from that mark drew it in a block's colour.
test("a criterion's row draws its bullet in the statements' colour", () => {
  const view = render(
    createElement(
      QueryClientProvider,
      { client: signedInClient() },
      createElement(
        MemoryRouter,
        null,
        createElement(NodeRow, {
          author: { kind: "person", name: "alice", reader: false },
          context: false,
          focused: false,
          isClosed: false,
          level: 2,
          node: treeNode({ id: "tree-md#b-c1", text: TEXT, type: "criterion" }),
          onFocus: () => {},
          onToggle: () => {},
          ownAnswer: false,
          rowKey: "tree-md#b-c1",
          tabStop: false,
        })
      )
    )
  );
  try {
    const dot = view.container.querySelector("[data-node-bullet] i");
    expect(dot?.classList.contains(pageKindBullet.statement)).toBe(true);
  } finally {
    view.unmount();
  }
});
