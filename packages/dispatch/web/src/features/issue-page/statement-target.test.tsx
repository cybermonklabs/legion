import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { ApiError, api } from "../../api/client";
import type { Ask } from "../../api/types";
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { BOB, KEY, node, SPEC_SOURCE, STATEMENT_WORDS } from "./composer-fixture";
import { IssueTree } from "./IssueTree";
import type { TreeNode } from "./issue-tree";
import {
  type OutlineWriter,
  OutlineWriterProvider,
  replyItemOf,
  type StatementItem,
  type StatementTarget,
  useOutlineWriter,
} from "./outline-writer";
import { publishedTree } from "./projection-fixture";

/**
 * A statement as a target of the page's writer: `r` on a focused
 * statement aims the writer at it, with the anchor the box's comment carries, and no other
 * target stands beside it. The tree is drawn under the writer as `slots/body.tsx` draws it, so
 * the key is the tree's own `r` (`useTreeKeymap.ts`) and not a call made for it.
 */

/** The question `id` as the asks read sends it: the record `changeAnswer` reads the reader's own
 *  answer from. */
function askRecord(id: string, answer: Ask["answer"]): Ask {
  return {
    anchor: null,
    answer,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Yes" }, { label: "No" }],
    question: `Question ${id}?`,
    state: answer === null ? "open" : "answered",
    urgency: "med",
  };
}

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** The tree as the body slot draws it, handing back the writer above it. */
function PageBody({ held, root }: { held: { writer?: OutlineWriter }; root: TreeNode }): ReactNode {
  held.writer = useOutlineWriter();
  return (
    <IssueTree
      artifacts={new Map()}
      context={new Set()}
      emptyLine="Nothing here."
      filtering={false}
      focusReveal={false}
      holdsEscape={false}
      isClosed={false}
      onEditing={() => {}}
      published={publishedTree(root)}
      reveal={undefined}
      root={root}
      rootRow
    />
  );
}

/** The tree under the page's writer, every read of the writer's answered before it mounts, so
 *  nothing asks the network. Drawn with an earlier page's `client`, it is that page mounted again
 *  in the same tab, its held sends with it. */
function renderPage(
  root: TreeNode,
  asks: readonly Ask[],
  client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  })
) {
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  client.setQueryData(["asks", KEY], asks);
  for (const read of ["comments", "issue-messages"]) client.setQueryData([read, KEY], []);
  client.setQueryData(["agents"], []);
  const held: { writer?: OutlineWriter } = {};
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <OutlineWriterProvider issueKey={KEY} openAsks={[]}>
              <IssueScope />
              <PageBody held={held} root={root} />
            </OutlineWriterProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const writer = (): OutlineWriter => {
    if (held.writer === undefined) throw new Error("the page drew no writer");
    return held.writer;
  };
  return { client, view, writer };
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R11 and decision 74: a statement is a target of the writer. Its item names the document by
// the slug its node id holds and the block, with the words its row draws; the writer holds the
// anchor's quote, the statement's whole words as the server matches them, and no reply, answer
// or edit beside it. Every other aim, and setting the line back to the issue, ends it; and while
// a send of the line's own is out, nothing moves it, as nothing moves a reply.
test("r on a statement aims the writer at it, and every other aim ends it", async () => {
  const user = userEvent.setup();
  const tree = displayTree(STATEMENT_WORDS);
  if (tree === undefined) throw new Error("the engine read no statement");
  const statement = node({
    id: "spec#b-r1",
    reference: `dispatch://${KEY}/spec#b-r1`,
    source: SPEC_SOURCE,
    statement: true,
    text: STATEMENT_WORDS,
    tree,
    type: "requirement",
  });
  // A statement the coverage read sent no reading of: its stored words are all there is.
  const unread = node({
    id: "spec#b-r2",
    reference: `dispatch://${KEY}/spec#b-r2`,
    statement: true,
    text: "The box takes the focus.",
    type: "requirement",
  });
  const comment = node({
    author: { id: "bob", kind: "user" },
    id: "comment-1",
    reference: `dispatch://${KEY}/comment/comment-1`,
    text: "Earlier comment",
    type: "comment",
  });
  const asked = (id: string, state: "answered" | "open") =>
    node({
      author: { id: "planner", kind: "session" },
      id,
      options: [{ label: "Yes" }, { label: "No" }],
      question: state === "open",
      reference: `dispatch://${KEY}/ask/${id}`,
      state,
      text: `Question ${id}?`,
      type: "ask",
    });
  const open = asked("ask-1", "open");
  const answered = asked("ask-2", "answered");

  const item: StatementItem = {
    artifact: "spec",
    id: "spec#b-r1",
    kind: "statement",
    text: STATEMENT_WORDS,
    tree,
  };
  expect(replyItemOf(statement)).toEqual(item);
  expect(replyItemOf(unread)).toEqual({
    artifact: "spec",
    id: "spec#b-r2",
    kind: "statement",
    text: "The box takes the focus.",
  });
  // A statement whose row carries an author hands it on with the item, for the box's target line
  // to name; one whose row carries none hands none.
  expect(replyItemOf({ ...unread, author: BOB })).toEqual({
    artifact: "spec",
    author: BOB,
    id: "spec#b-r2",
    kind: "statement",
    text: "The box takes the focus.",
  });
  expect(replyItemOf(statement)).not.toHaveProperty("author");
  // The statement's words with no mark of markdown, its two paragraphs one space apart, and its
  // row, which a comment on it lands on. Its row carries no author, so the writer holds none.
  const aimed: StatementTarget = {
    artifact: "spec",
    quote: "The page draws one box. It sits at the foot.",
    row: "spec#b-r1",
  };

  const page = renderPage(
    node({ children: [statement, unread, comment, open, answered], id: KEY, type: "issue" }),
    [
      askRecord("ask-1", null),
      askRecord("ask-2", {
        at: "2026-11-06T00:00:00Z",
        selected: ["Yes"],
        text: "",
        user: "alice",
      }),
    ]
  );
  const { writer } = page;
  try {
    const press = async (nodeId: string, key: string) => {
      const row = page.view.container.querySelector<HTMLElement>(
        `[data-node-row][data-node-id="${nodeId}"]`
      );
      if (row === null) throw new Error(`the tree drew no row for ${nodeId}`);
      await act(async () => row.focus());
      await user.keyboard(key);
    };
    /** `r` on the statement: the writer holds it and nothing beside it. */
    const aimAtStatement = async () => {
      await press("spec#b-r1", "r");
      await waitFor(() => expect(writer().statement).toEqual(aimed));
      expect(writer().reply).toBeNull();
      expect(writer().answer).toBeUndefined();
      expect(writer().editing).toBeUndefined();
    };

    await aimAtStatement();
    await press("spec#b-r2", "r");
    await waitFor(() => expect(writer().statement?.quote).toBe("The box takes the focus."));

    // Each other aim ends the statement, and `r` on the statement then ends that aim.
    const aims: readonly {
      readonly aim: () => Promise<void>;
      readonly name: string;
      readonly set: () => unknown;
    }[] = [
      { aim: () => press("comment-1", "r"), name: "a reply", set: () => writer().reply?.id },
      { aim: () => press("ask-1", "a"), name: "an answer", set: () => writer().answer?.askId },
      {
        aim: () => act(async () => writer().pick(open, "No")),
        name: "an option",
        set: () => writer().answer?.selected,
      },
      {
        aim: () => act(async () => writer().changeAnswer(answered)),
        name: "a change of answer",
        set: () => writer().answer?.replacing?.at,
      },
      {
        aim: () => act(async () => writer().edit("comment-1", "My own words")),
        name: "an edit",
        set: () => writer().editing?.id,
      },
      { aim: () => act(async () => writer().clear()), name: "clear", set: () => undefined },
      {
        aim: () => act(async () => writer().writeOnIssue()),
        name: "the issue's own Reply",
        set: () => undefined,
      },
    ];
    const ended: Record<string, unknown> = {};
    for (const { aim, name, set } of aims) {
      await aimAtStatement();
      await aim();
      await waitFor(() => expect(writer().statement, name).toBeUndefined());
      ended[name] = set();
    }
    expect(ended).toEqual({
      "a change of answer": "2026-11-06T00:00:00Z",
      "a reply": "comment-1",
      "an answer": "ask-1",
      "an edit": "comment-1",
      "an option": ["No"],
      clear: undefined,
      "the issue's own Reply": undefined,
    });
    await aimAtStatement();

    // While the line's own send is out, a Reply elsewhere and `clear` keep the statement.
    const request: SentRequest = {
      anchor: undefined,
      ask: { multiple: false, options: [], urgency: "med" },
      draft: { body: "On the statement", replacement: "" },
      edit: undefined,
      kind: "comment",
      owner: { issueKey: KEY, kind: "issue" },
      plainDelivery: "steer",
      replyTo: null,
      sendKey: "send-1",
      statement: { artifact: aimed.artifact, quote: aimed.quote, row: aimed.row },
    };
    // The send stays out: Dispatch never answers it.
    const createComment = spyOn(api, "createComment").mockImplementation(
      () => new Promise<never>(() => {})
    );
    try {
      act(() => {
        heldSends(page.client).send(["issue-composer", KEY], request);
      });
      await press("comment-1", "r");
      act(() => writer().clear());
      act(() => writer().edit("comment-1", "My own words"));
      expect(writer().statement).toEqual(aimed);
      expect(writer().reply).toBeNull();
      expect(writer().editing).toBeUndefined();
    } finally {
      createComment.mockRestore();
    }
  } finally {
    page.view.unmount();
  }
});

// The reviewer's finding 4593511280: while a comment on a statement Dispatch refused is held,
// every other aim - a Reply on another row or on another statement, an Answer, an option, a
// change of answer, an Edit - leaves the writer on that statement, so the refused words are
// neither replaced by another target's seed nor sent under another target, as a refused reply
// holds its reply (`redirectedReplyTarget`). A writer that mounts again over the refused send
// takes its statement up from it - the document, the words and the row the send keeps, and no
// author - and a Reply on that same statement takes its row's whole target again, the author its
// row names with it.
test("a refused comment on a statement holds the writer on it, and a writer mounted again takes it up", async () => {
  const tree = displayTree(STATEMENT_WORDS);
  if (tree === undefined) throw new Error("the engine read no statement");
  const statement = node({
    author: BOB,
    id: "spec#b-r1",
    reference: `dispatch://${KEY}/spec#b-r1`,
    source: SPEC_SOURCE,
    statement: true,
    text: STATEMENT_WORDS,
    tree,
    type: "requirement",
  });
  const other = node({
    id: "spec#b-r2",
    reference: `dispatch://${KEY}/spec#b-r2`,
    statement: true,
    text: "The box takes the focus.",
    type: "requirement",
  });
  const comment = node({
    author: { id: "bob", kind: "user" },
    id: "comment-1",
    reference: `dispatch://${KEY}/comment/comment-1`,
    text: "Earlier comment",
    type: "comment",
  });
  const asked = (id: string, state: "answered" | "open") =>
    node({
      author: { id: "planner", kind: "session" },
      id,
      options: [{ label: "Yes" }, { label: "No" }],
      question: state === "open",
      reference: `dispatch://${KEY}/ask/${id}`,
      state,
      text: `Question ${id}?`,
      type: "ask",
    });
  const open = asked("ask-1", "open");
  const answered = asked("ask-2", "answered");
  const root = node({
    children: [statement, other, comment, open, answered],
    id: KEY,
    type: "issue",
  });
  const asks = [
    askRecord("ask-1", null),
    askRecord("ask-2", { at: "2026-11-06T00:00:00Z", selected: ["Yes"], text: "", user: "alice" }),
  ];
  const aimed: StatementTarget = {
    artifact: "spec",
    author: BOB,
    quote: "The page draws one box. It sits at the foot.",
    row: "spec#b-r1",
  };
  const sendKey = ["issue-composer", KEY];
  const request: SentRequest = {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body: "On the statement", replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: KEY, kind: "issue" },
    plainDelivery: "steer",
    replyTo: null,
    sendKey: "send-1",
    statement: { artifact: aimed.artifact, quote: aimed.quote, row: aimed.row },
  };
  const user = userEvent.setup();
  // Dispatch refuses the comment.
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(400, { code: "INVALID_INPUT", error: "The comment was refused." })
  );
  const first = renderPage(root, asks);
  try {
    const press = async (nodeId: string, key: string) => {
      const row = document.querySelector<HTMLElement>(`[data-node-row][data-node-id="${nodeId}"]`);
      if (row === null) throw new Error(`the tree drew no row for ${nodeId}`);
      await act(async () => row.focus());
      await user.keyboard(key);
    };
    await press("spec#b-r1", "r");
    await waitFor(() => expect(first.writer().statement).toEqual(aimed));
    act(() => {
      heldSends(first.client).send(sendKey, request);
    });
    await waitFor(() => expect(heldSends(first.client).get(sendKey)?.status).toBe("refused"));

    const aims: readonly { readonly aim: () => Promise<void>; readonly name: string }[] = [
      { aim: () => press("comment-1", "r"), name: "a reply" },
      { aim: () => press("spec#b-r2", "r"), name: "another statement" },
      { aim: () => press("ask-1", "a"), name: "an answer" },
      { aim: () => act(async () => first.writer().pick(open, "No")), name: "an option" },
      {
        aim: () => act(async () => first.writer().changeAnswer(answered)),
        name: "a change of answer",
      },
      {
        aim: () => act(async () => first.writer().edit("comment-1", "My own words")),
        name: "an edit",
      },
    ];
    for (const { aim, name } of aims) {
      await aim();
      expect(first.writer().statement, name).toEqual(aimed);
      expect(first.writer().reply, name).toBeNull();
      expect(first.writer().answer, name).toBeUndefined();
      expect(first.writer().editing, name).toBeUndefined();
    }

    // The writer mounted again in the same tab, over the send Dispatch refused.
    first.view.unmount();
    const again = renderPage(root, asks, first.client);
    const held = { artifact: aimed.artifact, quote: aimed.quote, row: aimed.row };
    expect(again.writer().statement).toEqual(held);
    await press("comment-1", "r");
    expect(again.writer().statement).toEqual(held);
    expect(again.writer().reply).toBeNull();
    await press("spec#b-r1", "r");
    await waitFor(() => expect(again.writer().statement).toEqual(aimed));
  } finally {
    cleanup();
    createComment.mockRestore();
  }
});

// The reviewer's finding 5942007779 and decision 152: only a `requirement` block of the spec is
// a statement. The plan's own `requirement` block is a block and nothing more, so its focused row
// draws no Reply - Copy reference alone - and `r` on it leaves the writer aimed at nothing: no
// comment is anchored to the plan from the box.
test("a requirement block of the plan draws no Reply, and r on it aims the writer at nothing", async () => {
  const user = userEvent.setup();
  const planned = node({
    id: "plan-md#b-r9",
    reference: `dispatch://${KEY}/artifact/plan-md#b-r9`,
    source: { id: "artifact-plan", issue: KEY, kind: "artifact" },
    text: "The plan restates a requirement.",
    type: "requirement",
  });
  const page = renderPage(node({ children: [planned], id: KEY, type: "issue" }), []);
  try {
    const row = page.view.container.querySelector<HTMLElement>(
      `[data-node-row][data-node-id="${planned.id}"]`
    );
    if (row === null) throw new Error("the tree drew no row for the plan's block");
    await act(async () => row.focus());
    await waitFor(() => expect(row.querySelector("[data-row-copy]")).not.toBeNull());
    const acts = [...row.querySelectorAll("[data-row-act]")].map((control) =>
      control.getAttribute("data-row-act")
    );
    expect(acts).toEqual([]);
    await user.keyboard("r");
    expect(page.writer().statement).toBeUndefined();
    expect(page.writer().reply).toBeNull();
    expect(replyItemOf(planned)).toBeUndefined();
  } finally {
    page.view.unmount();
  }
});
