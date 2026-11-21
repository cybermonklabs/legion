import { spyOn } from "bun:test";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { displayTree } from "../../__tests__/display-trees";
import { graphNode } from "../../__tests__/graph-fixture";
import {
  architectureTree,
  issueCollaborators,
  plannerAgent,
  searchResponse,
} from "../../__tests__/search-fixture";
import { api } from "../../api/client";
import type { Actor, CommentWithTrees, IssueAskRow } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { useCurrentPoint } from "./current-point";
import { IssueComposer } from "./IssueComposer";
import { IssueTree } from "./IssueTree";
import type { TreeNode } from "./issue-tree";
import { NoTooltips } from "./NoTooltips";
import {
  type OutlineWriter,
  OutlineWriterProvider,
  type ReplyItem,
  useOutlineWriter,
} from "./outline-writer";
import { ANSWER_LINES, type NodeType } from "./page-words";

/**
 * The new issue page's box at the foot of its outline, drawn as the page draws it, for a test of
 * the box (`IssueComposer.test.tsx`) and for the box's draft matrix
 * (`conversation/draft-matrix.issue-page-outline.test.tsx`): under the page's writer and its words
 * (`slots/providers.tsx`), at the foot of the outline the body slot draws (`IssueTree`), or of a
 * view's rows where a view is on, as `IssueOutlinePage.tsx` draws the stage and the foot slot.
 * The outline is bob's comment, the reader's own, an open question, the question the reader
 * answered, and a statement of the spec; every read the box and the writer make is answered from
 * the test's own records (`answerBoxReads`), and what the box draws is read back as the reader
 * sees it.
 */

export const KEY = "CORE-1";

/** Who reads the page, and so who writes from the box. */
export const READER = "alice";
export const ALICE: Actor = { id: READER, kind: "user" };
export const BOB: Actor = { id: "bob", kind: "user" };

/** A statement of two paragraphs, one word of it marked, as the spec stores it; and its whole
 *  words as the server matches a quote against them (`wordsOf`). */
export const STATEMENT_WORDS = "The page draws **one** box.\n\nIt sits at the foot.";
export const STATEMENT_QUOTE = "The page draws one box. It sits at the foot.";

/** The record a statement's words are a block of: the issue's spec (`tree-drafts.ts`). */
export const SPEC_SOURCE = { id: "artifact-spec", issue: KEY, kind: "artifact" } as const;

/** The reads the box and the writer draw from, under the keys the outline reads them by: a
 *  test's own, and what the stubs of `answerBoxReads` answer when a write makes the page read one
 *  again. A test may change them as it goes. */
export interface BoxReads {
  asks: IssueAskRow[];
  comments: CommentWithTrees[];
}

/** Question `id` as the issue's asks read sends it. */
export function askRecord(id: string, fields: Partial<IssueAskRow> = {}): IssueAskRow {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "carol", kind: "user" },
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind: "question",
    last_reply: null,
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Yes" }, { label: "No" }],
    question: `Question ${id}?`,
    state: "open",
    urgency: "med",
    ...fields,
  };
}

/** Comment `id` by `author` as the issue's comments read sends it. */
export function commentRecord(
  id: string,
  author: Actor,
  body: string,
  fields: Partial<CommentWithTrees> = {}
): CommentWithTrees {
  const tree = displayTree(body);
  return {
    anchor: null,
    ask_id: null,
    author,
    body,
    ...(tree === undefined ? {} : { body_tree: tree }),
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
    ...fields,
  };
}

/** A node of the outline's tree, with nothing on it but what `partial` gives. */
export function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
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

/** The row of question `id` in the outline. */
export function askNode(id: string, state: "answered" | "open"): TreeNode {
  return node({
    author: { id: "carol", kind: "user" },
    id,
    options: [{ label: "Yes" }, { label: "No" }],
    question: state === "open",
    reference: `dispatch://${KEY}/ask/${id}`,
    state,
    text: `Question ${id}?`,
    type: "ask",
  });
}

/** The row of comment `id` by `author` in the outline. */
export function commentNode(id: string, author: Actor, text: string): TreeNode {
  return node({
    author,
    id,
    reference: `dispatch://${KEY}/comment/${id}`,
    text,
    type: "comment",
  });
}

/** The row of the spec's statement, `spec#b-r1`. */
export function statementNode(): TreeNode {
  const tree = displayTree(STATEMENT_WORDS);
  if (tree === undefined) throw new Error("the engine read no statement");
  return node({
    id: "spec#b-r1",
    reference: `dispatch://${KEY}/spec#b-r1`,
    source: SPEC_SOURCE,
    statement: true,
    text: STATEMENT_WORDS,
    tree,
    type: "requirement",
  });
}

/** The outline the body slot draws: bob's comment, the reader's own, an open question, the
 *  question the reader answered, and a statement of the spec. */
export function outline(): TreeNode {
  return node({
    children: [
      commentNode("comment-1", BOB, "Earlier words"),
      commentNode("comment-2", ALICE, "My own words"),
      askNode("ask-1", "open"),
      askNode("ask-2", "answered"),
      statementNode(),
    ],
    id: KEY,
    type: "issue",
  });
}

/** bob's comment as a row's Reply hands it to the writer. */
export const BOBS_COMMENT: ReplyItem = {
  author: BOB,
  id: "comment-1",
  kind: "comment",
  text: "Earlier words",
};

/** Where the box stands, as the foot slot reads it off the address (`slots/foot.tsx`). */
export interface Place {
  readonly closed?: boolean;
  readonly documentOpen?: boolean;
  readonly viewOn?: boolean;
}

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** A view's rows as the stage draws them in place of the outline (`slots/stage.tsx`): one tree,
 *  its row the tab stop, with the Reply and the Answer a view's row hands the writer. */
function ViewRows(): ReactNode {
  const writer = useOutlineWriter();
  return (
    <div aria-label="Spec" role="tree">
      <div aria-selected="true" role="treeitem" tabIndex={0}>
        <button onClick={() => writer?.replyTo(BOBS_COMMENT)} type="button">
          Reply
        </button>
        <button onClick={() => writer?.answerNode(askNode("ask-1", "open"))} type="button">
          Answer
        </button>
      </div>
    </div>
  );
}

/** The body and the foot of the page: the outline, or a view's rows while a view is on, and the
 *  box after them, as `IssueOutlinePage.tsx` draws the stage and the foot slot. */
function Page({
  held,
  place,
  root,
}: {
  held: { writer?: OutlineWriter };
  place: Place;
  root: TreeNode;
}): ReactNode {
  held.writer = useOutlineWriter();
  // The conversation's current point, as the body slot reads it from the whole tree (R12).
  const point = useCurrentPoint(root);
  return (
    <div data-issue-page="">
      {place.viewOn === true ? (
        <ViewRows />
      ) : (
        <IssueTree
          artifacts={new Map()}
          context={new Set()}
          emptyLine="Nothing here."
          filtering={false}
          focusReveal={false}
          holdsEscape={false}
          isClosed={place.closed === true}
          onEditing={() => {}}
          point={point}
          reveal={undefined}
          root={root}
          rootRow
        />
      )}
      <IssueComposer
        documentOpen={place.documentOpen === true}
        isClosed={place.closed === true}
        issueKey={KEY}
        viewOn={place.viewOn === true}
      />
    </div>
  );
}

/** The box's page as `drawBoxPage` draws it, and the moves a test makes on it. */
export interface BoxPage {
  /** Focuses the row of `nodeId` and presses `key` there, as the reader does. */
  readonly press: (user: UserEvent, nodeId: string, key: string) => Promise<void>;
  /** Draws the box in another place on the same mount: a view turned on or off, a document
   *  opened, the issue closed. */
  readonly redraw: (at: Place) => void;
  /** Draws the outline from another tree on the same mount, as the read a landed write makes
   *  does. */
  readonly redrawOutline: (drawn: TreeNode) => void;
  readonly view: RenderResult;
  readonly writer: () => OutlineWriter;
}

/** The page over `client`, every read the writer and the box make answered from `reads` before it
 *  mounts, the box at `place` under `root`. A client an earlier page was drawn over is that page
 *  mounted again in the same tab, its held sends and its kept drafts with it; signing the reader
 *  in on it is the test's. */
export function drawBoxPage(
  client: QueryClient,
  reads: BoxReads,
  place: Place = {},
  root: TreeNode = outline()
): BoxPage {
  client.setQueryData(["asks", KEY], reads.asks);
  client.setQueryData(["comments", KEY], reads.comments);
  client.setQueryData(["issue-messages", KEY], []);
  client.setQueryData(["agents"], [plannerAgent()]);
  const held: { writer?: OutlineWriter } = {};
  const draw = (at: Place, drawn = root) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <NoTooltips>
              <OutlineWriterProvider issueKey={KEY}>
                <IssueScope />
                <Page held={held} place={at} root={drawn} />
              </OutlineWriterProvider>
            </NoTooltips>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw(place));
  return {
    press: async (user, nodeId, key) => {
      const row = rowOf(nodeId);
      await act(async () => row.focus());
      await user.keyboard(key);
    },
    redraw: (at) => view.rerender(draw(at)),
    redrawOutline: (drawn) => view.rerender(draw(place, drawn)),
    view,
    writer: () => {
      if (held.writer === undefined) throw new Error("the page drew no writer");
      return held.writer;
    },
  };
}

/** Answers every read the box and the writer make, and the reads behind the reference picker and
 *  an edit's blocks, which the box hosts, from `reads` as they stand when each read is made.
 *  Answers the comments read and the search the picker's `#` door reads, for a test to read, and
 *  what puts every read back. */
export function answerBoxReads(reads: BoxReads) {
  const commentsRead = spyOn(api, "listComments").mockImplementation(async () => reads.comments);
  const searchRead = spyOn(api, "search").mockResolvedValue(searchResponse([]));
  const others = [
    spyOn(api, "listIssueAsks").mockImplementation(async () => reads.asks),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([plannerAgent()]),
    spyOn(api, "getIssueCollaborators").mockResolvedValue(issueCollaborators()),
    spyOn(api, "getArchitecture").mockResolvedValue(architectureTree()),
    spyOn(api, "getKnownSessions").mockResolvedValue({ sessions: [] }),
    spyOn(api, "commentBlocks").mockImplementation(async (id) => ({
      blocks: [],
      indexed: false,
      item: graphNode({ id, kind: "comment" }),
    })),
  ];
  return {
    commentsRead,
    restore: () => {
      for (const spy of [commentsRead, searchRead, ...others]) spy.mockRestore();
    },
    searchRead,
  };
}

/** The box's section. */
export function composerBox(): HTMLElement {
  const found = document.querySelector<HTMLElement>('[data-testid="issue-composer"]');
  if (found === null) throw new Error("the page drew no box");
  return found;
}

/** The field of the line that writes on the issue, a reply and a statement's comment: drawn
 *  whether or not the line is hidden, so a test reads the draft it keeps. */
export function writeField(): HTMLTextAreaElement {
  const found = composerBox().querySelector<HTMLTextAreaElement>("[data-composer-write] textarea");
  if (found === null) throw new Error("the box drew no write line");
  return found;
}

/** The words of the target line the box draws, the × beside them left out; null with none. */
export function targetWords(): string | null {
  const line = [...composerBox().querySelectorAll<HTMLElement>("[data-composer-target]")].find(
    (one) => one.closest("[hidden]") === null
  );
  return line?.firstElementChild?.textContent ?? null;
}

/** The lines the box draws of the drafts it keeps for other targets than the one it shows, in
 *  their order. */
export function keptLines(): string[] {
  return within(composerBox())
    .queryAllByRole("status")
    .map((line) => line.textContent ?? "")
    .filter((text) => text.includes(" is kept"));
}

/** What the box says of what the reader wrote on the issue, and on a statement, that it keeps
 *  while it shows another target. */
export const KEPT_FOR_ISSUE = ANSWER_LINES.kept_on_issue;
export const KEPT_FOR_STATEMENT = ANSWER_LINES.kept_on_statement;

/** What the box says of a reply it keeps for `author`'s comment while it shows another target. */
export function keptReplyLine(author: string): string {
  return `Your reply to the comment by ${author} is kept. Press Reply on that comment to go on with it.`;
}

/** The outline's row of `nodeId`. */
export function rowOf(nodeId: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-node-row][data-node-id="${nodeId}"]`);
  if (row === null) throw new Error(`the outline drew no row for ${nodeId}`);
  return row;
}
