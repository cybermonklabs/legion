import { spyOn } from "bun:test";
import { readFileSync } from "node:fs";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import { http } from "msw";
import { type ReactNode, useMemo, useState } from "react";
import {
  type Location,
  MemoryRouter,
  type NavigateFunction,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { graphNode } from "../../../__tests__/graph-fixture";
import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { issueAskThreadsQuery } from "../../../api/queries";
import type {
  Anchor,
  Comment,
  DisplayTree,
  IssueDecisions,
  IssueDetails,
  MessageRead,
} from "../../../api/types";
import { DESKTOP_WIDTH } from "../../margin/margin-fixture";
import { buildDispatchReference, parseIssuePath } from "../../refs/routes";
import { useKeymapScope } from "../../shell/keymap";
import {
  artifact,
  decision,
  decisionsRead,
  issueDetails,
  KEY,
  node,
  SPEC_DOCUMENT,
  shaped,
  specRef,
} from "../decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "../IssueOutlinePage";
import { IssueTree } from "../IssueTree";
import { presetPath } from "../issue-address";
import { nodeWords, type TreeNode } from "../issue-tree";
import { type MarginDrawer, MarginSlotProvider } from "../margin-slot";
import { readComment } from "../outline-fixtures";
import type { Preset } from "../page-words";
import { Publisher, publishedTree } from "../projection-fixture";
import { citedReferences } from "../row-references";
import { ProvidersSlot } from "../slots/providers";
import { StageSlot } from "../slots/stage";
import { AppProviders, inDocument, stubReads, treeOf } from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { commentNode, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { check, result, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import type { TreeAnchor } from "./on-text";
import { TextMargin } from "./TextMargin";

/**
 * What the margin part's unit tests share (`views.test.tsx`, `column.test.tsx`,
 * `selection.test.tsx`, `caret.test.tsx`, `row-sites.test.tsx`): one issue that holds a statement
 * of its spec, the plan task and the check that serve it, and an agent's decision, each with one
 * comment on its words, drawn as the page's root draws it (`IssueOutlinePage`) at an address and
 * a window width. The comment on text is what part T's tree read answers for it on B0: a node
 * under the node it is on, with the `anchor` naming that node's document and block, and the same
 * `anchor` on the comments read's comment. It holds too the answers the recorder read from the
 * real server (`recorded`), which `on-text.test.ts` and `text-threads.test.ts` read.
 */

/** The plan and the Verification document of the issue, beside its spec. */
export const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");
export const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The block ids of the statement of the spec, the plan task and the check that serve it, and the
 *  agent's decision in the spec. */
export const STATEMENT = "r1";
export const PLAN_TASK = "t1";
export const CHECK = "c1";
export const DECISION = "d1";
/** A block of the spec that is no statement and no decision, which Spec and Verification draw as
 *  a row of its own, and the result recorded against the check. */
export const OTHER = "o1";
export const RESULT = "res1";

/** The words of each of those blocks, so a quote taken from them is in them. */
export const STATEMENT_WORDS = "The page draws the plan.";
export const PLAN_TASK_WORDS = "Draw the plan beside the spec.";
export const CHECK_WORDS = "The page draws c1.";
export const DECISION_WORDS = "The decision d1.";
export const OTHER_WORDS = "A note beside the spec.";
export const OTHER_SECOND = "Its second block.";
export const RESULT_WORDS = "The result words.";

/** A risk of the issue, which the tree read holds where a case asks for it (`SeededReads.risk`):
 *  a node read-only in the room (R6), which Conversation draws as a row under the issue, and whose
 *  words take a comment on the node itself (G2). The read answers a risk's heading as its text. */
export const RISK = "risk-1";
export const RISK_WORDS = "A slow read hides the page from its reader.";

/** A comment of the issue's reader, alice, that is on the words `quote` of `block` of `document`:
 *  anchored at that block, as the comments read answers it. */
export interface TextThreadSeed {
  /** The comment, as the comments read answers it, anchor included. */
  readonly comment: Comment;
  /** The block it is on. */
  readonly block: string;
  /** The tree id of the node that block is drawn as (`blockNodeId`). */
  readonly node: string;
  /** The words it quotes. */
  readonly quote: string;
}

/** A comment of the issue's reader, alice, on the words `quote` of `block` of `document`,
 *  written `at` minutes past the hour. */
export function threadOn(
  id: string,
  document: { readonly id: string; readonly slug: string },
  block: string,
  quote: string,
  at: number
): TextThreadSeed {
  const anchor: Anchor = {
    artifact_id: document.id,
    block_id: block,
    mark_id: "",
    orphaned: false,
    quote,
    version: 1,
  };
  return {
    block,
    comment: { ...readComment(id, at), anchor },
    node: blockNodeId(document.slug, block),
    quote,
  };
}

/** One comment on the words of each block the views draw: the statement (Conversation, Spec,
 *  Verification, Graph), the plan task (Plan), the check (Verification) and the decision
 *  (Decisions). */
export const ON_STATEMENT = threadOn("on-r1", SPEC_DOCUMENT, STATEMENT, "draws", 1);
export const ON_PLAN_TASK = threadOn("on-t1", PLAN_DOCUMENT, PLAN_TASK, "beside the spec", 2);
export const ON_CHECK = threadOn("on-c1", VERIFICATION_DOCUMENT, CHECK, "draws c1", 3);
export const ON_DECISION = threadOn("on-d1", SPEC_DOCUMENT, DECISION, "decision d1", 4);
export const TEXT_THREADS: readonly TextThreadSeed[] = [
  ON_STATEMENT,
  ON_PLAN_TASK,
  ON_CHECK,
  ON_DECISION,
];

/** A reply in the thread of `ON_STATEMENT`: a comment that answers it. */
export const REPLY_ON_STATEMENT = readComment("reply-on-r1", 5, "on-r1");

/** The issue, with its plan and its Verification document too, each in its role. */
export function seededIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT, VERIFICATION_DOCUMENT],
    plan_artifact_id: PLAN_DOCUMENT.id,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
}

/** A comment's anchor as the comments read answers it, as the tree read answers it on the
 *  comment's node (`IssueTreeAnchor`), which leaves out a block it names none of. */
export function treeAnchorOf(anchor: Anchor): TreeAnchor {
  const { block_id: block, ...members } = anchor;
  return block === null ? members : { ...members, block_id: block };
}

/** A text of one block for each of `paragraphs`, as the server reads a stored text for display. */
function blocksOf(...paragraphs: string[]): DisplayTree {
  return {
    content: paragraphs.map((text) => ({ content: [{ text, type: "text" }], type: "paragraph" })),
    type: "doc",
  };
}

/** The tree read's nodes for the issue: each block (but those of `without`, a read that has paged
 *  them away), then the comment on its words as a node under it with the `anchor` the read answers
 *  it (B0: `model/issue_tree.go`), then the issue's own posts, and its risk where `risk` asks for
 *  it. `threads` names the comments on text the answer holds. */
export function seededNodes(
  threads: readonly TextThreadSeed[],
  { posts = [], risk = false, without = [] }: Pick<SeededReads, "posts" | "risk" | "without"> = {}
): TreeAnswerNode[] {
  const blocks = [
    treeNode(STATEMENT, "requirement", KEY, {
      ...inDocument(SPEC_DOCUMENT),
      text: STATEMENT_WORDS,
    }),
    treeNode(PLAN_TASK, "plan_task", STATEMENT, {
      ...inDocument(PLAN_DOCUMENT),
      text: PLAN_TASK_WORDS,
    }),
    treeNode(CHECK, "check", STATEMENT, {
      ...inDocument(VERIFICATION_DOCUMENT),
      text: CHECK_WORDS,
    }),
    treeNode(DECISION, "decision", KEY, {
      ...inDocument(SPEC_DOCUMENT, 1),
      text: DECISION_WORDS,
    }),
    treeNode(RESULT, "result", CHECK, { text: RESULT_WORDS }),
    treeNode(OTHER, "requirement", KEY, {
      ...inDocument(PLAN_DOCUMENT, 1),
      text: `${OTHER_WORDS}\n\n${OTHER_SECOND}`,
      text_tree: blocksOf(OTHER_WORDS, OTHER_SECOND),
    }),
  ].filter((node) => !without.includes(node.block_id ?? node.id));
  return [
    ...blocks,
    ...threads.map(({ comment, node }) =>
      treeNode(comment.id, "comment", node, {
        ...(comment.anchor === null ? {} : { anchor: treeAnchorOf(comment.anchor) }),
        author: [comment.author],
        made_at: comment.created_at,
        text: comment.body,
      })
    ),
    ...posts.map((post) => commentNode(post)),
    ...(risk
      ? [treeNode(RISK, "risk", KEY, { attributes: { heading: RISK_WORDS }, text: RISK_WORDS })]
      : []),
  ];
}

/** The decisions read: the agent's decision, tied to the statement it shaped, both in the entries
 *  the Decisions view reads and in the `decisions` member the Spec view reads. */
function decisionsOf(): IssueDecisions {
  const made = decision(DECISION, { links: [shaped(specRef(STATEMENT))] });
  return { ...decisionsRead([made]), decisions: [made] };
}

/** The tuples of the seeded tree: the plan task and the check each serve the statement. */
export const SERVES = [
  treeTuple("serves", PLAN_TASK, STATEMENT),
  treeTuple("serves", CHECK, STATEMENT),
];

/** What each read answers while a case draws: `threads` the comments on text the reads hold
 *  (change it and read again to load a thread), `without` the block ids the tree read leaves
 *  out. */
export interface SeededReads {
  /** The comments of the issue that are no thread on text: posts under the issue. */
  posts?: readonly Comment[];
  /** Whether the tree read holds the issue's risk (`RISK`). */
  risk?: boolean;
  threads: readonly TextThreadSeed[];
  /** The block ids the tree read leaves out, as a read that pages their nodes away does. */
  without?: readonly string[];
  /** Leave the comments read to a test that observes the HTTP request. */
  wireComments?: boolean;
}

/** Answers every read the page and its views are drawn from for `seededIssue`, and the shell's
 *  own two, with `reads.threads` as the comments on text. Each read answers what `reads` holds
 *  when it is sent, so a case that changes `reads.threads` and reads again draws the change. The
 *  spies are those `stubReads` set; the returned list restores them. */
export function answerSeededReads(reads: SeededReads): { mockRestore(): void }[] {
  const restore = stubReads();
  if (reads.wireComments) spyOn(api, "listComments").mockRestore();
  restore.push(
    spyOn(api, "getIssue").mockResolvedValue(seededIssue()),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: true,
      issue: KEY,
      requirements: [
        {
          block_id: STATEMENT,
          checks: 1,
          gaps: [],
          plan_tasks: 1,
          ref: `dispatch://${KEY}/spec#b-${STATEMENT}`,
          text: STATEMENT_WORDS,
        },
      ],
      spec_artifact_id: SPEC_DOCUMENT.id,
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue(decisionsOf()),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [
        check({
          block_id: CHECK,
          results: [result({ check_block_id: CHECK, id: RESULT, observed: RESULT_WORDS })],
          text: CHECK_WORDS,
        }),
      ],
      issue: KEY,
      verification_artifact_id: VERIFICATION_DOCUMENT.id,
    }),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
      const nodes = seededNodes(reads.threads, reads);
      if (query.view === undefined) return treeOf(nodes, SERVES);
      const own: Readonly<Record<string, string>> = {
        decisions: "decision",
        plan: "plan_task",
        spec: "requirement",
        verification: "check",
      };

      const drawn = nodes.flatMap((node): TreeAnswerNode[] => {
        if (node.supertag === own[query.view as string]) return [{ ...node, role: "query" }];
        if (query.view === "verification" && node.supertag === "result") {
          return [{ ...node, role: "query" }];
        }
        if (query.view === "verification" && node.supertag === "requirement") {
          return [{ ...node, role: "context" }];
        }
        return [];
      });
      // Each view read answers the comments on the text of each node it returns, as context
      // after them, as the server answers them on B0 (`fixtures/tree-b0-*.json`).
      const onDrawn = nodes.flatMap((node): TreeAnswerNode[] => {
        const anchor = node.anchor;
        const onText =
          node.supertag === "comment" &&
          anchor !== undefined &&
          drawn.some(
            (one) =>
              one.document?.artifact_id === anchor.artifact_id && one.block_id === anchor.block_id
          );
        return onText ? [{ ...node, role: "context" }] : [];
      });
      return treeOf([...drawn, ...onDrawn], SERVES);
    })
  );
  if (!reads.wireComments) {
    restore.push(
      spyOn(api, "listComments").mockImplementation(async () => [
        ...reads.threads.map(({ comment }) => comment),
        ...(reads.posts ?? []),
        ...(reads.threads.includes(ON_STATEMENT) ? [REPLY_ON_STATEMENT] : []),
      ])
    );
  }
  // An address that names a comment reads that comment (`getComment`), as the page's own read of
  // it does.
  restore.push(
    spyOn(api, "getComment").mockImplementation(async (id) => {
      const held = [
        ...reads.threads.map(({ comment }) => comment),
        ...(reads.posts ?? []),
        REPLY_ON_STATEMENT,
      ].find((comment) => comment.id === id);
      if (held === undefined) throw new Error(`no comment ${id} is seeded`);
      return { comment: held, replies: null };
    })
  );
  // The header's own read of the project's architecture sources, and what references the issue
  // that Details reads (nothing does), answered with the API's empty shapes.
  answerRequests(
    http.get("/api/v1/projects/:project/architecture-sources", () => Response.json([])),
    http.get("/api/v1/references", () =>
      Response.json({
        edges: [],
        node: graphNode({
          id: KEY,
          issue_key: KEY,
          kind: "issue",
          ref: buildDispatchReference({ key: KEY, kind: "issue" }),
        }),
      })
    )
  );
  return restore;
}

/** The `change` listeners of one media query a window's width answers. */
interface WidthQuery {
  readonly listeners: Set<() => void>;
  readonly query: string;
}

/** One `(min-width: …)` or `(max-width: …)` clause, in `px` or `rem`. */
const WIDTH_CLAUSE = /^\(\s*(min|max)-width\s*:\s*(\d*\.?\d+)(px|rem)\s*\)$/;

/** Whether a window `width` px wide matches `query`, which holds when each clause joined by `and`
 *  holds (the rule `stubViewport` has, which cannot change its width). */
function widthMatches(width: number, query: string): boolean {
  return query.split(/\s+and\s+/i).every((clause) => {
    const match = WIDTH_CLAUSE.exec(clause.trim());
    if (match === null) throw new Error(`the window stub cannot read the media query "${query}"`);
    const [, bound, value, unit] = match;
    const px = Number(value) * (unit === "rem" ? 16 : 1);
    return bound === "min" ? width >= px : width <= px;
  });
}

/** A window of `initial` px that the case can resize: every media query answers for the width it
 *  has now, and `set` sends each query's `change` listeners, inside `act`, as a resize does.
 *  `restore` puts the browser's own `matchMedia` back. */
export function windowOf(initial: number): {
  readonly restore: () => void;
  readonly set: (width: number) => Promise<void>;
  readonly width: () => number;
} {
  let width = initial;
  const queries = new Set<WidthQuery>();
  const original = window.matchMedia;
  window.matchMedia = ((query: string) => {
    const held: WidthQuery = { listeners: new Set(), query };
    queries.add(held);
    return {
      addEventListener: (_type: string, listener: () => void) => held.listeners.add(listener),
      addListener: (listener: () => void) => held.listeners.add(listener),
      dispatchEvent: () => true,
      get matches() {
        return widthMatches(width, query);
      },
      media: query,
      onchange: null,
      removeEventListener: (_type: string, listener: () => void) => held.listeners.delete(listener),
      removeListener: (listener: () => void) => held.listeners.delete(listener),
    } as unknown as MediaQueryList;
  }) as typeof window.matchMedia;
  return {
    restore: () => {
      window.matchMedia = original;
    },
    set: (next) =>
      act(async () => {
        width = next;
        for (const held of queries) for (const listener of [...held.listeners]) listener();
      }),
    width: () => width,
  };
}

/** A client as the page's tests take one: no read tried again. */
export function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** What a case holds of the page it drew, from inside the page's providers. */
export interface PageHold {
  /** The Details drawer the shell holds, where the case gave the page one. */
  drawer?: MarginDrawer;
  /** The router's location as the page last drew it. */
  location?: Location;
  /** Moves the router, as a followed link does. */
  navigate?: NavigateFunction;
}

/** Holds the router's `navigate` for the case, inside the router. */
function NavigateProbe({ held }: { held: PageHold }): ReactNode {
  held.location = useLocation();
  held.navigate = useNavigate();
  return null;
}

/** The shell's Details drawer, with its `open` kept in state, around `children`, as `app.tsx`
 *  holds it above the page. */
function DrawerAround({
  children,
  held,
}: {
  readonly children: ReactNode;
  readonly held: PageHold;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const drawer = useMemo<MarginDrawer>(() => ({ open, setOpen, setPage: () => {} }), [open]);
  held.drawer = drawer;
  return (
    <MarginSlotProvider drawer={drawer} drawsSlot={false}>
      {children}
    </MarginSlotProvider>
  );
}

/** What `drawPage` and `drawFrame` give back: the render, the window, what the page holds, and
 *  `finish`, which unmounts the page and puts the window back. */
export type DrawnPage = RenderResult & {
  readonly finish: () => void;
  readonly held: PageHold;
  readonly window: ReturnType<typeof windowOf>;
};

/** `content` at the address `entry`, beside what holds the router for the case, under what the
 *  app provides above the page, in a window `width` px wide that `window.set` resizes. Details'
 *  drawer is the shell's own (`drawer`) where the case wants Details. */
function drawIn(
  client: QueryClient,
  entry: string,
  content: ReactNode,
  { drawer, width }: { readonly drawer: boolean; readonly width: number }
): DrawnPage {
  const sized = windowOf(width);
  const held: PageHold = {};
  const page = (
    <>
      <NavigateProbe held={held} />
      {content}
    </>
  );
  const view = render(
    <MemoryRouter initialEntries={[entry]}>
      <AppProviders client={client}>
        {drawer ? <DrawerAround held={held}>{page}</DrawerAround> : page}
      </AppProviders>
    </MemoryRouter>
  );
  return {
    ...view,
    finish: () => {
      view.unmount();
      sized.restore();
    },
    held,
    window: sized,
  };
}

/** The page's root, `IssueOutlinePage`, at the issue's address of `preset` under what the app
 *  provides above it, in a window `width` px wide (`DESKTOP_WIDTH`, 1440 px, where none is given)
 *  that `window.set` resizes. Details' drawer is the shell's own (`drawer`) where the case wants
 *  Details. `beside` is drawn after the root, under the same router and providers: what a case
 *  needs beside the page that reads none of the page's own providers (a composer's opener, a
 *  row's sheet), while the root keys its slots by the issue as the page does. */
export function drawPage(
  client: QueryClient,
  preset: Preset,
  {
    after = "",
    beside = null,
    drawer = false,
    width = DESKTOP_WIDTH,
  }: { after?: string; beside?: ReactNode; drawer?: boolean; width?: number } = {}
): DrawnPage {
  const page = (
    <>
      <IssueOutlinePage />
      {beside}
    </>
  );
  return drawIn(client, `${presetPath(KEY, preset)}${after}`, page, { drawer, width });
}

/** The page's root as `IssueOutlinePage` draws it - the stage in the issue's column and the
 *  column of comments on text after it, in the page's providers - with `children` drawn inside
 *  those providers beside them in place of the root's own control (`SelectionComment`): what a
 *  case needs to reach what the providers hold (the control, a composer's opener), drawn once.
 *  The page's root takes no such child, so the cases that need one draw this; the cases that need
 *  only the page draw `drawPage`. */
function FrameRoot({
  children,
  issue,
}: {
  readonly children: ReactNode;
  readonly issue: IssueDetails;
}): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <div className="flex min-w-0 items-start">
        <div className="flex min-w-0 flex-1 flex-col" data-testid="issue-column">
          <StageSlot issue={issue} route={route} />
        </div>
        <TextMargin issue={issue} route={route} />
      </div>
      {children}
    </ProvidersSlot>
  );
}

/** `drawPage`'s window and drawer, with `FrameRoot` in the page's place and `children` beside the
 *  stage and the column. */
export function drawFrame(
  client: QueryClient,
  preset: Preset,
  children: ReactNode,
  { drawer = false, width = DESKTOP_WIDTH }: { drawer?: boolean; width?: number } = {}
): DrawnPage {
  return drawIn(
    client,
    presetPath(KEY, preset),
    <FrameRoot issue={seededIssue()}>{children}</FrameRoot>,
    { drawer, width }
  );
}

/** The page's own scope for the tree's keys. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** Every reference the words of `node` and of each node under it cite (`citedReferences`). */
function citedUnder(node: TreeNode): string[] {
  return [...citedReferences(nodeWords(node)), ...node.children.flatMap(citedUnder)];
}

/** The outline's rows of `children`, each a node of its own kind, under the issue, as the page's
 *  body draws a tree, under the page's providers and the reader alice: a tree the case builds by
 *  hand, for the kinds the page's reads draw only under a whole issue. The rows are the rows of
 *  `IssueTree`, whose `[data-node-words]` the margin part reads. The page asks its batched read
 *  for each chip's target and each reference the nodes' words cite (`useIssueTree.ts`), so a row
 *  waits on the page for each and reads none itself; one the case names no reading for stays
 *  drawn as its author wrote it. */
export function drawTree(
  children: readonly TreeNode[]
): RenderResult & { readonly client: QueryClient } {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  for (const read of ["asks", "comments", "issue-messages"]) client.setQueryData([read, KEY], []);
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  const root = node({ children: [...children], id: KEY, type: "issue" });
  const chips = publishedTree(root);
  const published = { ...chips, asked: new Set([...chips.asked, ...citedUnder(root)]) };
  const view = render(
    <MemoryRouter>
      <AppProviders client={client}>
        <ProvidersSlot issue={seededIssue()} route={{ key: KEY, kind: "issue" }}>
          <IssueScope />
          <Publisher published={published} />
          <IssueTree
            artifacts={new Map()}
            context={new Set()}
            emptyLine="Nothing here."
            focusReveal={false}
            holdsEscape={false}
            isClosed={false}
            onEditing={() => {}}
            published={published}
            reveal={undefined}
            root={root}
            rootRow
          />
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
  return { ...view, client };
}

/** Reads again every read the page holds, as an event or a write's re-read does, inside `act`. */
export async function readAgain(client: QueryClient): Promise<void> {
  await act(async () => {
    await client.invalidateQueries();
  });
}

/** The row drawn for the node `nodeId` in `view`: the first element that carries it. */
export function rowOf(view: RenderResult, nodeId: string): HTMLElement | null {
  return view.container.querySelector<HTMLElement>(`[data-node-id="${CSS.escape(nodeId)}"]`);
}

/** The row `view` draws for `nodeId`, and its own words element. */
export function located(
  view: RenderResult,
  nodeId: string
): { readonly row: HTMLElement; readonly words: HTMLElement } {
  const row = rowOf(view, nodeId);
  if (row === null) throw new Error(`no row is drawn for ${nodeId}`);
  const words = [...row.querySelectorAll<HTMLElement>("[data-node-words]")].find(
    (one) => one.closest("[data-node-id]") === row
  );
  if (words === undefined) throw new Error(`the row of ${nodeId} draws no words element`);
  return { row, words };
}

/** The width in px that `element` has at a window `windowWidth` px wide, from the width it draws
 *  in its class (`w-[280px]`, and `min-[1280px]:w-[320px]` from 1280 px up) or its style, or
 *  undefined where it draws none. happy-dom lays nothing out, so the classes are the width. */
export function widthAt(element: HTMLElement, windowWidth: number): number | undefined {
  const fromStyle = /^(\d+(?:\.\d+)?)px$/.exec(element.style.width);
  if (fromStyle?.[1] !== undefined) return Number(fromStyle[1]);
  let best: { readonly from: number; readonly width: number } | undefined;
  for (const token of element.className.split(/\s+/)) {
    const match = /^(?:min-\[(\d+)px\]:)?w-\[(\d+)px\]$/.exec(token);
    if (match?.[2] === undefined) continue;
    const from = match[1] === undefined ? 0 : Number(match[1]);
    if (from <= windowWidth && (best === undefined || from >= best.from)) {
      best = { from, width: Number(match[2]) };
    }
  }
  return best?.width;
}

/**
 * A window whose page the test scrolls: `scrollTo` moves it, and the browser's own scroll to a
 * focused element, which a script's `focus()` with no `preventScroll` does, jumps it to `jumpTo`
 * (the head of the column, where a card stands before the stack places it). Only a card or a
 * field that is not in focus yet jumps it: those are what this part focuses by script, and a
 * pointer's focus scrolls nothing. Each `scrollIntoView` is recorded with the element and its
 * options. `restore` puts the page back as it was.
 */
export function scrollingPage(
  startAt: number,
  jumpTo: number
): {
  readonly revealed: { readonly element: Element; readonly options: unknown }[];
  readonly restore: () => void;
  readonly scrollY: () => number;
} {
  let y = startAt;
  const revealed: { readonly element: Element; readonly options: unknown }[] = [];
  const own = Object.getOwnPropertyDescriptor(window, "scrollY");
  Object.defineProperty(window, "scrollY", { configurable: true, get: () => y });
  const scrollTo = spyOn(window, "scrollTo").mockImplementation(((_left: number, top: number) => {
    y = top;
  }) as typeof window.scrollTo);
  const focus = HTMLElement.prototype.focus;
  HTMLElement.prototype.focus = function (this: HTMLElement, options?: FocusOptions) {
    const byScript =
      this.matches("[data-margin-item], textarea") && this !== document.activeElement;
    if (byScript && options?.preventScroll !== true) y = jumpTo;
    focus.call(this, options);
  };
  const reveal = Object.getOwnPropertyDescriptor(Element.prototype, "scrollIntoView");
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value(this: Element, options?: unknown) {
      revealed.push({ element: this, options });
    },
    writable: true,
  });
  return {
    restore: () => {
      scrollTo.mockRestore();
      HTMLElement.prototype.focus = focus;
      if (own === undefined) Reflect.deleteProperty(window, "scrollY");
      else Object.defineProperty(window, "scrollY", own);
      if (reveal === undefined) Reflect.deleteProperty(Element.prototype, "scrollIntoView");
      else Object.defineProperty(Element.prototype, "scrollIntoView", reveal);
    },
    revealed,
    scrollY: () => y,
  };
}

/** A rectangle as a layout would measure it. */
export function rectOf(left: number, top: number, width: number, height: number): DOMRect {
  return new DOMRect(left, top, width, height);
}

/** The words of the row `row`: the text node of its words element that holds `words`, and where in
 *  it they start. */
function wordsIn(row: Element, words: string): { readonly node: Text; readonly offset: number } {
  const holder = row.querySelector("[data-node-words]") ?? row;
  const walker = document.createTreeWalker(holder, NodeFilter.SHOW_TEXT);
  for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
    const at = text.textContent?.indexOf(words) ?? -1;
    if (at >= 0) return { node: text as Text, offset: at };
  }
  throw new Error(`the row draws no text node that holds "${words}"`);
}

/** Selects `words` inside `row`, as a drag over them does, and sends `selectionchange`. */
export function selectWords(row: Element, words: string): void {
  const { node: text, offset } = wordsIn(row, words);
  const range = document.createRange();
  range.setStart(text, offset);
  range.setEnd(text, offset + words.length);
  setSelection(range);
}

/** Selects from the start of `first` in `rowA` to the end of `second` in `rowB`. */
export function selectAcross(rowA: Element, first: string, rowB: Element, second: string): void {
  const from = wordsIn(rowA, first);
  const to = wordsIn(rowB, second);
  const range = document.createRange();
  range.setStart(from.node, from.offset);
  range.setEnd(to.node, to.offset + second.length);
  setSelection(range);
}

/** Selects `range` and sends `selectionchange`. */
export function setSelection(range: Range): void {
  const selection = window.getSelection();
  if (selection === null) throw new Error("the window holds no selection");
  selection.removeAllRanges();
  selection.addRange(range);
  document.dispatchEvent(new Event("selectionchange"));
}

/** Collapses the selection and sends `selectionchange`. */
export function clearSelection(): void {
  window.getSelection()?.removeAllRanges();
  document.dispatchEvent(new Event("selectionchange"));
}

/** The column of comments on text the page drew, named `name` (the margin's words), once it is
 *  drawn within `within` ms, or null where it is not: a case that asserts the column fails with
 *  its own words rather than with a wait that ran out. */
export async function columnOf(
  view: RenderResult,
  name: string,
  within = 1500
): Promise<HTMLElement | null> {
  try {
    return await waitFor(
      () => {
        const column = view.queryByRole("region", { hidden: true, name });
        if (column === null) throw new Error("the column is not drawn");
        return column;
      },
      { timeout: within }
    );
  } catch {
    return null;
  }
}

/** The answers the recorder read from the real server at part W's head (`fixtures/`, gate B0),
 *  each named for the read and the gate it needs. The recorder names each instant of a file by its
 *  place in that file's order (`instant-001`, ...); each is read back as a time in that order, as
 *  the page's code reads a time. */
export function recorded<Shape>(name: string): Shape {
  const text = readFileSync(new URL(`./fixtures/${name}`, import.meta.url), "utf8");
  return JSON.parse(text, (_field, value: unknown) =>
    typeof value === "string"
      ? value.replace(/instant-(\d+)/gu, (_named, place: string) => {
          const seconds = Number(place);
          const minute = String(Math.floor(seconds / 60)).padStart(2, "0");
          return `2026-11-05T00:${minute}:${String(seconds % 60).padStart(2, "0")}Z`;
        })
      : value
  ) as Shape;
}

/** The recorded reads of Conversation's tree, of the comments and of the messages, read anew at
 *  each call. */
export function recordedReads(): {
  readonly comments: Comment[];
  readonly conversation: TreeAnswer;
  readonly messages: MessageRead[];
} {
  return {
    comments: recorded<{ comments: Comment[] }>("comments.json").comments,
    conversation: recorded<{ read: TreeAnswer }>("tree-b0-conversation.json").read,
    messages: recorded<{ messages: MessageRead[] }>("messages.json").messages,
  };
}

/** The recorded issue, `key`, with the documents its answers name by their recorded ids. */
export function recordedIssue(): IssueDetails {
  return {
    ...issueDetails(),
    artifacts: [
      artifact("specArtifactId", "spec", true),
      artifact("verificationArtifactId", "verification-md"),
      artifact("planArtifactId", "plan-md"),
      artifact("uncitedArtifactId", "background-md"),
    ],
    key: "key",
    plan_artifact_id: "planArtifactId",
    primary_artifact_id: "specArtifactId",
    verification_artifact_id: "verificationArtifactId",
  };
}
