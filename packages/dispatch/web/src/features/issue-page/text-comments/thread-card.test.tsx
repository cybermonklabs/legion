import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { type RenderResult, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../../api/client";
import type { Anchor, Comment } from "../../../api/types";
import type { Thread, ThreadComment } from "../../margin/useMarginItems";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { AGENT, KEY } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { buildIssueTree, type IssueTreeReads, type TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { readComment } from "../outline-fixtures";
import { authorText } from "../page-structure";
import { AppProviders, stubReads, treeOf } from "../slots/stage-fixtures";
import { commentNode } from "../tree-read/tree-fixtures";
import { GATES } from "./gates";
import {
  answerSeededReads,
  columnOf,
  type DrawnPage,
  drawPage,
  newClient,
  ON_CHECK,
  ON_STATEMENT,
  REPLY_ON_STATEMENT,
  readAgain,
  SERVES,
  type SeededReads,
  seededIssue,
  seededNodes,
  type TextThreadSeed,
} from "./margin-fixtures";
import type { QuotedReplyBlock } from "./on-text";
import { TextThreadCard, useThreadCardHost } from "./TextThreadCard";
import { type TextThread, textThreadsOf } from "./text-threads";

/**
 * R4: each thread on text is drawn by today's margin card (`margin/ThreadCard.tsx`), fixed open,
 * with the controls it draws on `main`, the quoted words first and each entry whole, inside
 * `NoTooltips` with its stored text marked as the authors'. A thread rooted in a message, or in a
 * reply with a quote, draws no Resolve or Reopen, and a message thread's reply writes a message.
 *
 * A case about the column draws the page (`drawPage`) with the threads the reads hold. A case
 * about a thread no read of this base answers - a reply with a quote, a message on words, both
 * gate G4 (`GATES.REPLY_QUOTE`) - builds the thread as `textThreadsOf` builds it and draws its
 * card alone, as the column draws it; the case where the page must read such a reply waits for
 * G4.
 */

// The cards draw their bodies through the shared renderer, whose Markdown engine loads lazily, and
// the rows run in random order, so the file loads the engine before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

/** Another person of the issue, who is not its reader. */
const BOB = { id: "bob", kind: "user" } as const;

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** The page at Conversation with `reads` answered, once its column is drawn. */
async function drawnColumn(reads: SeededReads): Promise<{
  readonly client: QueryClient;
  readonly column: HTMLElement;
  readonly view: DrawnPage;
}> {
  stubs = answerSeededReads(reads);
  const client = newClient();
  const view = drawPage(client, "conversation");
  const column = await columnOf(view, COLUMN);
  if (column === null) {
    view.finish();
    throw new Error("the page draws no column of comments on text");
  }
  return { client, column, view };
}

/** The card the column draws for the thread rooted at `id`, once it is drawn. */
async function cardIn(column: HTMLElement, id: string): Promise<HTMLElement> {
  return await waitFor(() => {
    const card = column.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(id)}"]`);
    if (card === null) throw new Error(`the column draws no card for ${id}`);
    return card;
  });
}

/** `seed` written by `author`, whose name the card draws. */
function writtenBy(seed: TextThreadSeed, author: Comment["author"]): TextThreadSeed {
  return { ...seed, comment: { ...seed.comment, author } };
}

/** The issue of the cards drawn alone, read once, as the page holds it. */
const ISSUE = seededIssue();

/** One card as the column draws it, with the writes and the reader of the issue's cards. */
function CardAlone({ thread }: { readonly thread: TextThread }): ReactNode {
  return <TextThreadCard host={useThreadCardHost(ISSUE)} thread={thread} />;
}

/** The card of `thread` alone, in the box the column draws it in - inside `NoTooltips`, its
 *  stored text marked as the authors' - or outside `NoTooltips` where `tooltips` says so, under
 *  what the app provides above the page. */
function drawCard(thread: TextThread, { tooltips = false } = {}): RenderResult {
  stubs = stubReads();
  const card = (
    <div {...authorText}>
      <CardAlone thread={thread} />
    </div>
  );
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={newClient()}>
        {tooltips ? card : <NoTooltips>{card}</NoTooltips>}
      </AppProviders>
    </MemoryRouter>
  );
}

/** The block of a post that a reply quotes `quote` of, as INFRA-5703 answers it (G4). */
function quotedBlock(post: Comment, quote: string, removed = false): QuotedReplyBlock {
  return {
    block_id: "b1",
    node: `comment/${post.id}#b1`,
    occurrence: 0,
    quote,
    removed,
    text: post.body,
  };
}

/** A thread on text rooted at `root`, whose words are the reply's quote of a post's block, as
 *  `textThreadsOf` builds one (`replyRoot`, and the card's anchor from the block): a message's
 *  where `kind` says so. */
function quotingThread(
  root: ThreadComment & { readonly reply_block: QuotedReplyBlock },
  kind: "comment" | "message",
  thread: Partial<Thread> = {}
): TextThread {
  const block = root.reply_block;
  const anchor: Anchor = {
    artifact_id: "",
    block_id: block.block_id,
    mark_id: "",
    orphaned: block.removed,
    quote: block.quote ?? "",
    version: 0,
  };
  return {
    firstLine: block.removed,
    kind,
    lenses: new Map(),
    occurrence: block.occurrence ?? 0,
    orphaned: block.removed,
    quote: block.quote ?? "",
    replyRoot: true,
    thread: {
      anchor,
      key: root.id,
      ...(kind === "message" ? { kind } : {}),
      lastReplyAt: undefined,
      replies: [],
      resolved: root.resolved,
      root: { comment: { ...root, anchor }, kind: "comment" },
      ...thread,
    },
  };
}

/** The post a quoting reply answers, under the issue. */
const POST = readComment("post", 6);

/** Alice's reply to `POST` that quotes words of its block: the root of a thread on text. */
function quotingReply(removed = false): Comment & { readonly reply_block: QuotedReplyBlock } {
  return {
    ...readComment("quoting", 7, POST.id),
    reply_block: quotedBlock(POST, "words", removed),
  };
}

/** Alice's message that answers `POST` and quotes words of its block, as the card draws a
 *  message (no anchor, resolution or suggestion). */
function quotingMessage(): Comment & { readonly reply_block: QuotedReplyBlock } {
  return { ...quotingReply(), id: "message-on-post", reply_to: POST.id };
}

/** The names of the controls `card` draws a reader can reach - each button and each field - with
 *  the record a copy control names written as `<reference>`. */
function controlsOf(card: HTMLElement): string[] {
  return [
    ...within(card)
      .getAllByRole("button")
      .map((button) => `button ${button.getAttribute("aria-label") ?? button.textContent}`),
    ...within(card)
      .queryAllByRole("textbox")
      .map((field) => `field ${field.getAttribute("aria-label") ?? ""}`),
  ]
    .map((name) => name.replace(/dispatch:\/\/\S+/u, "<reference>"))
    .sort();
}

test("the card never draws its collapsed preview", async () => {
  const user = userEvent.setup();
  const { client, column, view } = await drawnColumn({ threads: [ON_STATEMENT] });
  try {
    const card = await cardIn(column, ON_STATEMENT.comment.id);
    const whole = () => ({
      // The collapsed card is the card's `aria-expanded="false"` with its root's preview inside
      // the button that opens it.
      collapsed:
        card.getAttribute("aria-expanded") !== "true" ||
        [...card.querySelectorAll("button")].some(
          (button) => button.textContent?.includes(ON_STATEMENT.comment.body) === true
        ),
      field: within(card).queryByRole("textbox", { name: "Reply" }) !== null,
      reply: within(card)
        .queryByRole("list", { name: "Replies" })
        ?.textContent?.includes(REPLY_ON_STATEMENT.body),
    });
    await waitFor(() =>
      expect(whole(), "the card is not drawn open").toEqual({
        collapsed: false,
        field: true,
        reply: true,
      })
    );
    expect(
      within(card).queryByRole("button", { name: "Cancel reply" }),
      "the card draws a control that would collapse it"
    ).toBeNull();
    // Escape in the reply's field ends no reply of a card that stays open, and a read again
    // draws it open as before.
    await user.click(within(card).getByRole("textbox", { name: "Reply" }));
    await user.keyboard("{Escape}");
    expect(whole(), "Escape in the reply's field collapsed the card").toEqual({
      collapsed: false,
      field: true,
      reply: true,
    });
    await readAgain(client);
    expect(whole(), "a read again drew the card collapsed").toEqual({
      collapsed: false,
      field: true,
      reply: true,
    });
  } finally {
    view.finish();
    client.clear();
  }
});

// The card's reply composer takes no focus for itself: a thread that loads, and a read that draws
// its card again, leave the focus where the reader had it, and so scroll nothing.
test("the card's reply composer takes no focus as its card mounts or is drawn again", async () => {
  const reads: SeededReads = { threads: [] };
  stubs = answerSeededReads(reads);
  const client = newClient();
  const view = drawPage(client, "conversation");
  const taken: string[] = [];
  const noteFocus = (event: FocusEvent) => {
    if (event.target instanceof Element && event.target.closest("aside") !== null) {
      taken.push(event.target.tagName);
    }
  };
  document.addEventListener("focusin", noteFocus);
  try {
    await waitFor(() => expect(view.container.querySelector("[data-node-id]")).not.toBeNull());
    const before = document.activeElement;
    reads.threads = [ON_STATEMENT];
    await readAgain(client);
    const column = await columnOf(view, COLUMN);
    expect(column, "no column is drawn once the thread loads").not.toBeNull();
    await cardIn(column as HTMLElement, ON_STATEMENT.comment.id);
    await readAgain(client);
    expect(taken, "a card's composer took the focus").toEqual([]);
    expect(document.activeElement, "the focus moved as the thread loaded").toBe(before);
  } finally {
    document.removeEventListener("focusin", noteFocus);
    view.finish();
    client.clear();
  }
});

test("the card draws main's controls", async () => {
  const suggestion: TextThreadSeed = {
    ...ON_CHECK,
    comment: { ...ON_CHECK.comment, suggestion: { accepted: null, replace_with: "draws all" } },
  };
  const { client, column, view } = await drawnColumn({ threads: [ON_STATEMENT, suggestion] });
  try {
    const card = await cardIn(column, ON_STATEMENT.comment.id);
    await waitFor(() =>
      expect(
        within(card).queryByRole("textbox", { name: "Reply" }),
        "no reply field"
      ).not.toBeNull()
    );
    // The reader's own comment: the reply control, Resolve, Edit and the copy reference.
    expect(
      {
        copy: within(card).queryAllByRole("button", { name: /^Copy reference/ }).length > 0,
        edit: within(card).queryAllByRole("button", { name: "Edit" }).length > 0,
        resolve: within(card).queryByRole("button", { name: "Resolve" }) !== null,
        send: within(card).queryByRole("button", { name: "Send" }) !== null,
      },
      "the card of the reader's comment lacks a control of main's"
    ).toEqual({ copy: true, edit: true, resolve: true, send: true });
    // A suggestion: Accept and Reject.
    const suggested = await cardIn(column, ON_CHECK.comment.id);
    expect(
      {
        accept: within(suggested).queryByRole("button", { name: "Accept suggestion" }) !== null,
        reject: within(suggested).queryByRole("button", { name: "Reject suggestion" }) !== null,
      },
      "the suggestion's card lacks Accept or Reject"
    ).toEqual({ accept: true, reject: true });
  } finally {
    view.finish();
    client.clear();
  }
});

test("the quote comes first, then each entry whole", async () => {
  // A root of three paragraphs, which a collapsed card would cut to two lines, by bob, and alice's
  // reply under it.
  const body = [
    "The first paragraph of the root, which says what the words miss.",
    "A second paragraph, which the collapsed card would cut.",
    "A third paragraph, the last words of the root.",
  ].join("\n\n");
  const root = writtenBy({ ...ON_STATEMENT, comment: { ...ON_STATEMENT.comment, body } }, BOB);
  const { client, column, view } = await drawnColumn({
    posts: [REPLY_ON_STATEMENT],
    threads: [root],
  });
  try {
    const card = await cardIn(column, ON_STATEMENT.comment.id);
    await waitFor(() =>
      expect(card.textContent, "the reply is not drawn").toContain(REPLY_ON_STATEMENT.body)
    );
    const text = card.textContent ?? "";
    const quote = card.querySelector("blockquote");
    expect(quote?.textContent, "the card does not draw the quoted words").toBe(ON_STATEMENT.quote);
    const places = [
      text.indexOf(quote?.textContent ?? "\u0000"),
      text.indexOf("The first paragraph"),
      text.indexOf("A third paragraph, the last words of the root."),
      text.indexOf(REPLY_ON_STATEMENT.body),
    ];
    expect(
      places.every((at) => at >= 0),
      "an entry's words are missing"
    ).toBe(true);
    expect(places, "the quote, the root and its reply are out of order").toEqual(
      [...places].sort((left, right) => left - right)
    );
    expect(card.querySelector('[class*="line-clamp"]'), "an entry is cut").toBeNull();
    const reply = within(card).getByRole("list", { name: "Replies" });
    const rootPart = card.cloneNode(true) as HTMLElement;
    rootPart.querySelector('[aria-label="Replies"]')?.remove();
    expect(
      {
        replyTime: reply.querySelector("time") !== null,
        rootAuthor: rootPart.textContent?.includes("bob"),
        rootTime: rootPart.querySelector("time") !== null,
      },
      "an entry is drawn without its author or its time"
    ).toEqual({ replyTime: true, rootAuthor: true, rootTime: true });
  } finally {
    view.finish();
    client.clear();
  }
});

test("a resolved thread is drawn with Reopen", async () => {
  const resolved: TextThreadSeed = {
    ...ON_STATEMENT,
    comment: {
      ...ON_STATEMENT.comment,
      resolved: true,
      resolved_at: "2026-11-05T01:09:00Z",
      resolved_by: BOB,
    },
  };
  const { client, column, view } = await drawnColumn({ threads: [resolved] });
  try {
    const card = await cardIn(column, ON_STATEMENT.comment.id);
    await waitFor(() =>
      expect(
        within(card).queryByRole("button", { name: "Reopen" }),
        "the resolved thread draws no Reopen"
      ).not.toBeNull()
    );
    expect(within(card).queryByRole("button", { name: "Resolve" }), "it draws Resolve").toBeNull();
    expect(card.textContent, "it does not say who resolved it").toContain("Resolved by");
  } finally {
    view.finish();
    client.clear();
  }
});

test("a removed reply block draws the orphan notice", () => {
  const view = drawCard(quotingThread(quotingReply(true), "comment"));
  try {
    const card = view.getByTestId("margin-comment-quoting");
    expect(card.textContent, "the card draws no notice that the words left the text").toContain(
      "Text changed."
    );
    // The same thread whose block still holds its words draws none.
    view.rerender(
      <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
        <AppProviders client={newClient()}>
          <NoTooltips>
            <CardAlone thread={quotingThread(quotingReply(), "comment")} />
          </NoTooltips>
        </AppProviders>
      </MemoryRouter>
    );
    expect(
      view.getByTestId("margin-comment-quoting").textContent,
      "a reply whose words stand draws the notice"
    ).not.toContain("Text changed.");
  } finally {
    view.unmount();
  }
});

test("a message thread draws no Resolve and its reply sends a message", async () => {
  const user = userEvent.setup();
  const root = quotingMessage();
  const view = drawCard(quotingThread(root, "message"));
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const createComment = spyOn(api, "createComment").mockResolvedValue({} as never);
  stubs.push(createMessage, createComment);
  try {
    const card = view.getByTestId(`margin-comment-${root.id}`);
    const field = await within(card).findByRole("textbox", { name: "Reply" });
    expect(
      {
        reopen: within(card).queryByRole("button", { name: "Reopen" }) !== null,
        resolve: within(card).queryByRole("button", { name: "Resolve" }) !== null,
      },
      "the message thread draws Resolve or Reopen"
    ).toEqual({ reopen: false, resolve: false });
    await user.click(field);
    await user.paste("An answer to the message.");
    await user.click(within(card).getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage, "the reply sent no message").toHaveBeenCalledTimes(1)
    );
    expect(createMessage.mock.calls[0]?.[0], "the message went to another issue").toBe(KEY);
    expect(
      createMessage.mock.calls[0]?.[1],
      "the message does not answer the thread's root"
    ).toMatchObject({ in_reply_to: root.id });
    expect(createComment, "the reply sent a comment").not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("a reply-rooted thread draws no Resolve or Reopen", async () => {
  const root = quotingReply();
  for (const resolved of [false, true]) {
    const view = drawCard(quotingThread({ ...root, resolved }, "comment", { resolved }));
    try {
      const card = view.getByTestId(`margin-comment-${root.id}`);
      await within(card).findByRole("textbox", { name: "Reply" });
      expect(
        {
          reopen: within(card).queryByRole("button", { name: "Reopen" }) !== null,
          resolve: within(card).queryByRole("button", { name: "Resolve" }) !== null,
        },
        `the ${resolved ? "resolved" : "open"} reply-rooted thread draws Resolve or Reopen`
      ).toEqual({ reopen: false, resolve: false });
      expect(
        within(card).queryByRole("button", { name: "Edit" }),
        "the reader's own reply lost its Edit"
      ).not.toBeNull();
    } finally {
      view.unmount();
      for (const stub of stubs) stub.mockRestore();
      stubs = [];
    }
  }
});

// R4 (G4): the page reads a reply's quote from its `reply_block` (INFRA-5703), which no server of
// this base answers.
test.skipIf(!GATES.REPLY_QUOTE)(
  "the quote comes from reply_block where the thread is a reply",
  () => {
    const reply = quotingReply();
    const tree = treeOf([...seededNodes([]), commentNode(POST), commentNode(reply)], SERVES);
    const reads: IssueTreeReads = { comments: [POST, reply], issue: ISSUE, tree };
    const drawn = new Set<string>();
    const walk = (node: TreeNode): void => {
      for (const child of node.children) {
        drawn.add(child.id);
        walk(child);
      }
    };
    walk(buildIssueTree(reads));
    const thread = textThreadsOf(tree, reads, drawn).get(POST.id)?.threads[0];
    expect(thread?.thread.key, "the reply roots no thread by the post it quotes").toBe(reply.id);
    const view = drawCard(thread as TextThread);
    try {
      expect(
        view.getByTestId(`margin-comment-${reply.id}`).querySelector("blockquote")?.textContent,
        "the card's quote is not the reply_block's"
      ).toBe(reply.reply_block.quote);
    } finally {
      view.unmount();
    }
  }
);

test("an agent's thread draws as a person's", async () => {
  const person = writtenBy(ON_STATEMENT, BOB);
  const agent = writtenBy(ON_CHECK, AGENT);
  const { client, column, view } = await drawnColumn({ threads: [person, agent] });
  try {
    const personCard = await cardIn(column, person.comment.id);
    const agentCard = await cardIn(column, agent.comment.id);
    await waitFor(() =>
      expect(
        within(agentCard).queryByRole("textbox", { name: "Reply" }),
        "the agent's card draws no reply field"
      ).not.toBeNull()
    );
    expect(controlsOf(agentCard), "the agent's card draws other controls than a person's").toEqual(
      controlsOf(personCard)
    );
    expect(agentCard.textContent, "the agent's card does not name the agent").toContain(
      "Architect"
    );
    expect(personCard.textContent, "the person's card does not name its author").toContain("bob");
  } finally {
    view.finish();
    client.clear();
  }
});

test("the column draws its cards inside NoTooltips with authors' text marked", async () => {
  const { client, column, view } = await drawnColumn({ threads: [ON_STATEMENT] });
  try {
    const card = await cardIn(column, ON_STATEMENT.comment.id);
    await waitFor(() =>
      expect(card.querySelector("time"), "the card draws no time").not.toBeNull()
    );
    expect(
      card.closest("[data-author-text]"),
      "the card's stored text is not marked as the authors'"
    ).not.toBeNull();
    expect(
      [...column.querySelectorAll("[title]")].map((element) => element.getAttribute("title")),
      "a control in the column draws a tooltip"
    ).toEqual([]);
  } finally {
    view.finish();
    client.clear();
  }
  // The same card drawn outside `NoTooltips` carries the tooltips the column's leaves out.
  const alone = drawCard(quotingThread(quotingReply(), "comment"), { tooltips: true });
  try {
    expect(
      alone.container.querySelectorAll("[title]").length,
      "a card outside NoTooltips draws no tooltip, so the column's case proves nothing"
    ).toBeGreaterThan(0);
  } finally {
    alone.unmount();
  }
});
