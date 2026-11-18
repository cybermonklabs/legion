import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery, useQueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect, useMemo } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../../api/client";
import type { Actor, DecisionKeep } from "../../../api/types";
import {
  pageFocusedRow,
  pageInkText,
  pageOwnWordsRule,
  pageStateText,
} from "../../../theme/classes";
import { MarginProvider } from "../../margin/margin-context";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import type { OutlineRequest } from "../IssueTree";
import { artifactsBySlug } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { OutlineRequestProvider } from "../outline-request";
import { KEY_REPORTS } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  EMPTY_STATE_LINES,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
} from "../page-words";
import { presetView } from "../presets";
import {
  issueTreeKey,
  issueTreeQuery,
  subscribeTreeFreshness,
  type TreeAnswer,
  type TreeAnswerNode,
  viewKey,
  viewQuery,
} from "../tree-read/tree-read";
import { DecisionsFilterBody } from "./DecisionsFilterBody";
import {
  answeredAsk,
  ask,
  changeComment,
  closing,
  decision,
  decisionNode,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
  READER_ACTOR,
  SPEC_DOCUMENT,
  shaped,
  specRef,
  statement,
  treeAnswer,
} from "./decisions-fixtures";
import type { DecisionsAsk } from "./decisions-parts";

/**
 * What the Decisions filter's body draws, read as a reader reads it (dispatch://INFRA-1196 R2,
 * R9, R22, R35). Which entry stands in which part is the model's, read in
 * `decisions-parts.test.ts`; these rows read the drawing: the four parts under their headings,
 * the words each state reads, Keep and Change on the review list alone, the person's answer
 * character for character, and where the focus goes. How a row is laid out and where a long
 * question is cut are the browser's to say, which no DOM of a unit test lays out, so the
 * browser suite's c-row, c-decisions and c-review-list read them.
 */

/** The page's line for an issue the view draws nothing for, which the slot hands the body. */
const EMPTY_LINE = EMPTY_STATE_LINES.decisions;

/** The link every entry ends on: the page's words and the mark after them. */
const IN_CONVERSATION = `${DECISIONS_WORDS.in_conversation} ›`;

/** The foot's control, by the page's words it ends on, whatever count it holds. */
const FOOT_CONTROL = { name: (name: string) => name.endsWith(DECISIONS_WORDS.agents_many) };

/** What the person wrote, with the line breaks and the spaces they typed. */
const STORED = "Keep the rail.\n\n  It is 56 px wide -  no wider.\n";

const KEPT: DecisionKeep = {
  artifact_id: SPEC_DOCUMENT.id,
  block_id: "kept",
  changed_version: 1,
  kept_at: "2026-11-15T09:00:00Z",
  kept_by: READER,
};

/** One issue that holds an entry of every part: an open question, two decisions a statement
 *  rests on that wait, one the reader kept, an answered ask with the agent's write-up of it
 *  under it, a declined ask, and two decisions no statement rests on. */
function wholeIssue(): {
  asks: DecisionsAsk[];
  children: TreeNode[];
  made: TreeAnswerNode[];
} {
  return {
    asks: [
      closing("a-declined", {
        at: "2026-11-13T09:00:00Z",
        kind: "declined",
        reason: "The release owner decides this one.",
      }),
    ],
    children: [
      statement("s1"),
      statement("s2"),
      statement("s3"),
      openAsk("q-open", true, {
        options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
        text: "At which width does the header condense?",
      }),
      answeredAsk(
        "a-answered",
        { at: "2026-11-14T09:00:00Z", selected: ["Changes needed"], text: STORED },
        {
          children: [decisionNode("reading", { text: "The reader wants the rail kept." })],
          options: [{ label: "Works" }, { label: "Changes needed" }],
          text: "Does the built page work for you?",
        }
      ),
      ask("a-declined", { state: "declined", text: "On a phone, where does the box sit?" }),
      decisionNode("first", { at: "2026-11-10T12:00:00Z" }),
      decisionNode("second", { at: "2026-11-11T12:00:00Z", unsure: true }),
      decisionNode("kept"),
      decisionNode("minor"),
      decisionNode("replaced"),
    ],
    made: [
      decision("reading", { asks: ["a-answered"], kind: "person", waiting_review: false }),
      decision("first", {
        changed_at: "2026-11-10T12:00:00Z",
        links: [shaped(specRef("s1"))],
      }),
      decision("second", {
        changed_at: "2026-11-11T12:00:00Z",
        links: [shaped(specRef("s1")), shaped(specRef("s2")), shaped(specRef("s3"))],
        unsure: true,
      }),
      decision("kept", { kept: KEPT, links: [shaped(specRef("s2"))], waiting_review: false }),
      decision("minor"),
      decision("replaced", {
        links: [shaped(specRef("s1"))],
        replaced: true,
        waiting_review: false,
      }),
    ],
  };
}

const D = (block: string) => decisionNode(block).id;

/** The page's own `issue` scope, which every one of the outline's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** Where the page is, so a row that follows a link reads the address it went to. */
function Where(): ReactNode {
  const { hash, pathname } = useLocation();
  return <output data-testid="where">{`${pathname}${hash}`}</output>;
}

interface Drawn extends RenderResult {
  readonly client: QueryClient;
  /** Draws the body again with other props, as the page does when its address moves. */
  readonly again: (props: BodyProps) => void;
  /** What the server holds from now on, which every read of the tree made after answers. */
  readonly serve: (next: Partial<Served>) => void;
}

interface BodyProps {
  /** The issue's nodes as a later read leaves them, where the tree grew since the body was
   *  first drawn (`useIssueTree.ts` builds it read by read); the body is drawn over that tree,
   *  with the view the slot hands it, unless `view` says otherwise. */
  readonly children?: readonly TreeNode[];
  /** Whether the body is on the page: false takes it off, as the page does when the view is
   *  turned off, and true draws a new one. */
  readonly drawn?: boolean;
  readonly isClosed?: boolean;
  readonly request?: OutlineRequest;
  readonly reveal?: string;
  readonly view?: TreeNode;
  readonly waitingPress?: number;
}

/** What the server holds of the issue's decisions: the decision blocks of its spec, and each
 *  comment the tree read hangs under one of them, which is a Change a person asked of it. */
interface Served {
  readonly changes: readonly TreeAnswerNode[];
  readonly made: readonly TreeAnswerNode[];
}

/** The tree read and its views as the client answers them in a case: from what the server holds
 *  (`renderBody`'s `served`). Put back after each case. */
let treeReads: Mock<typeof api.getIssueTree> | undefined;
afterEach(() => {
  treeReads?.mockRestore();
  treeReads = undefined;
});

/**
 * The body's reads of the issue's tree as the page holds them: the tree read (`useIssueTree.ts`),
 * and the decisions view, which `ViewReadProvider` reads while the filter is on and
 * `useIssueTree.ts` sets on the tree's root (`TreeNode.views`), which is where the body reads it;
 * and the issue's comments, which `useIssueTree.ts` holds too, so a write of them is read again.
 * All are answered before the body is drawn, as the cache holds them once the page is; a read
 * made again - a Keep's (`DecisionActions.tsx`), or the one the page's subscription makes after a
 * write of the issue's comments, as a Change's send is (`subscribeTreeFreshness`) - answers what
 * the server holds then (`treeReads`).
 */
function HeldReads({
  children,
  root,
}: {
  children: (root: TreeNode) => ReactNode;
  root: TreeNode;
}): ReactNode {
  const client = useQueryClient();
  useEffect(() => subscribeTreeFreshness(client, KEY), [client]);
  useQuery(issueTreeQuery(KEY));
  useQuery({ queryFn: () => api.listComments(KEY), queryKey: ["comments", KEY] });
  const held = useQuery(viewQuery(KEY, "decisions")).data;
  const rooted = useMemo(
    () => (held === undefined ? root : { ...root, views: { decisions: held } }),
    [held, root]
  );
  return children(rooted);
}

function renderBody(
  {
    asks = [],
    changes = [],
    children,
    made = [],
  }: {
    asks?: DecisionsAsk[];
    changes?: TreeAnswerNode[];
    children: TreeNode[];
    made?: TreeAnswerNode[];
  },
  props: BodyProps = {},
  {
    requests = false,
  }: {
    /** Whether the page's outline requests are held above the body, as the issue page holds
     *  them (`slots/providers.tsx`), with a control beside the body to take the focus to. */
    requests?: boolean;
  } = {}
): Drawn {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  let served: Served = { changes, made };
  const readTree = () => treeAnswer([...served.made, ...served.changes]);
  const readView = () => treeAnswer(served.made);
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation(async (_key, query) =>
    query?.view === "decisions" ? readView() : readTree()
  );
  // The reads the page's tree is built from, under the keys it reads them by, and the
  // decisions view the page holds.
  client.setQueryData(issueTreeKey(KEY), readTree());
  client.setQueryData(viewKey(KEY, "decisions"), readView());
  client.setQueryData(["asks", KEY], asks);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  const root = issueTree(children);
  // The view the slot hands the body where no address names a node and no words are typed
  // (`slots/view-decisions.tsx`).
  const decisionsView = presetView(root, "decisions", "", new Set()).root;
  const body = ({
    children: grown,
    drawn = true,
    isClosed = false,
    request,
    reveal,
    view,
    waitingPress,
  }: BodyProps) => {
    const tree = grown === undefined ? root : issueTree(grown);
    const shown =
      view ??
      (grown === undefined ? decisionsView : presetView(tree, "decisions", "", new Set()).root);
    const list = drawn ? (
      <HeldReads root={tree}>
        {(held) => (
          <DecisionsFilterBody
            artifacts={artifactsBySlug(issue)}
            emptyLine={EMPTY_LINE}
            holdsEscape={false}
            isClosed={isClosed}
            issue={issue}
            reader={READER}
            {...(request === undefined ? {} : { request })}
            {...(reveal === undefined ? {} : { reveal })}
            root={held}
            view={shown}
            {...(waitingPress === undefined ? {} : { waitingPress })}
          />
        )}
      </HeldReads>
    ) : null;
    return (
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <KeymapProvider>
            <MarginProvider>
              <IssueScope />
              <Where />
              {/* Inside `NoTooltips`, as the view's slot draws it, so an author is named as the
                  page names one. */}
              <NoTooltips>
                {requests ? (
                  <OutlineRequestProvider>
                    <button type="button">Elsewhere</button>
                    {list}
                  </OutlineRequestProvider>
                ) : (
                  list
                )}
              </NoTooltips>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const view = render(body(props));
  return {
    ...view,
    again: (next) => view.rerender(body(next)),
    client,
    serve: (next) => {
      served = { ...served, ...next };
    },
  };
}

/** An element's text as a reader reads it: every run of white space one space. */
function words(element: Element | null | undefined): string | null {
  return element == null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
}

function rowOf(view: RenderResult, id: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${id}"]`);
  if (row === null) throw new Error(`the body draws no row for ${id}`);
  return row;
}

/** The id of each row one part draws, in the order it draws them. */
function rowsOfPart(view: RenderResult, part: string): string[] {
  return [
    ...view.container.querySelectorAll(`[data-decisions-part="${part}"] [data-node-row]`),
  ].map((row) => row.getAttribute("data-node-id") ?? "");
}

function focusedRow(view: RenderResult): string | undefined {
  const active = document.activeElement;
  return [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")]
    .find((row) => row === active || row.contains(active))
    ?.getAttribute("data-node-id") as string | undefined;
}

/** What the page writes of its own inside `scope`, each run with the author's text taken out,
 *  as the browser suite's c-plain-words reads the page's chrome. */
function chromeOf(scope: Element): string[] {
  return [...scope.querySelectorAll("[data-page-chrome]")]
    .map((element) => {
      const copy = element.cloneNode(true) as Element;
      for (const authored of copy.querySelectorAll("[data-author-text]")) authored.remove();
      return (copy.textContent ?? "").replace(/\s+/g, " ").trim();
    })
    .filter((text) => text !== "");
}

// R2, c-decisions: four parts under their own headings and in this order: the open questions,
// the decisions waiting for review, the reader's own decisions newest first, and the folded
// foot. No other sentence stands over them or between them (R22).
test("the four parts are drawn under their headings, in order, and no sentence explains them", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());

  const parts = [...view.container.querySelectorAll("[data-decisions-part]")].map((part) => ({
    count: words(part.querySelector("[data-decisions-count]")),
    heading: words(part.querySelector("[data-decisions-heading] [data-page-chrome], button")),
    part: part.getAttribute("data-decisions-part"),
    rows: [...part.querySelectorAll("[data-node-row]")].map((row) =>
      row.getAttribute("data-node-id")
    ),
  }));
  expect(parts).toEqual([
    {
      count: "1",
      heading: DECISIONS_WORDS.parts.open_questions,
      part: "open_questions",
      rows: ["q-open"],
    },
    // Newest first, by the time each was made.
    {
      count: "2",
      heading: DECISIONS_WORDS.parts.review,
      part: "review",
      rows: [D("second"), D("first")],
    },
    // The decision the reader kept, then the asks by the time each closed.
    {
      count: "3",
      heading: DECISIONS_WORDS.parts.yours,
      part: "yours",
      rows: [D("kept"), "a-answered", "a-declined"],
    },
    // Folded at the foot: its heading says how many it holds, and none is drawn.
    {
      count: "2",
      heading: `▸ 2 ${DECISIONS_WORDS.agents_many}`,
      part: "agents",
      rows: [],
    },
  ]);

  // The headings are the whole of what the page says of the parts: outside the rows, the body
  // writes nothing else of its own, and no paragraph stands over or between them.
  const outside = view.container.querySelector("[data-decisions-body]")?.cloneNode(true) as Element;
  for (const row of outside.querySelectorAll("[data-node-row]")) row.remove();
  expect(chromeOf(outside)).toEqual([
    DECISIONS_WORDS.parts.open_questions,
    DECISIONS_WORDS.parts.review,
    DECISIONS_WORDS.parts.yours,
    `▸ ${DECISIONS_WORDS.agents_many}`,
  ]);
  expect(outside.querySelectorAll("p").length).toBe(0);

  // One landmark named by the filter holds the list (R19), and in it one tree under the
  // outline's own name, each headed part a group named by its heading, and every entry a row
  // of it.
  expect(view.getByRole("region", { name: PRESET_NAMES.decisions })).toBe(
    view.container.querySelector("[data-decisions-body]") as HTMLElement
  );
  const tree = view.getByRole("tree", { name: CONTROL_LABELS.tree });
  expect(view.getAllByRole("tree").length).toBe(1);
  expect(
    [...tree.querySelectorAll('[role="group"]')].map((group) =>
      words(
        view.container.ownerDocument.getElementById(group.getAttribute("aria-labelledby") ?? "")
      )
    )
  ).toEqual([
    DECISIONS_WORDS.parts.open_questions,
    DECISIONS_WORDS.parts.review,
    DECISIONS_WORDS.parts.yours,
  ]);
  expect(
    [...tree.querySelectorAll("[data-node-row]")].every(
      (row) => row.getAttribute("role") === "treeitem"
    )
  ).toBe(true);

  // A press on a part's heading or on its count is a press on no entry, so it moves no focus:
  // with the focus on no row, no row takes it, and with it on a row, it stays there.
  await user.click(view.getByText(DECISIONS_WORDS.parts.review));
  expect(focusedRow(view)).toBeUndefined();
  const count = view.container.querySelector(
    '[data-decisions-part="yours"] [data-decisions-count]'
  );
  if (count === null) throw new Error("the reader's own decisions draw no count");
  await user.click(count);
  expect(focusedRow(view)).toBeUndefined();
  await act(async () => rowOf(view, D("first")).focus());
  await user.click(view.getByText(DECISIONS_WORDS.parts.open_questions));
  expect(focusedRow(view)).toBe(D("first"));
  // A press on a link of another row moves nothing while it is held: the row the reader is on
  // keeps its body open until the press is over, and the pressed row takes the focus then, so
  // the press ends on the link it went down on, and the link is followed.
  const pressedRow = rowOf(view, D("kept"));
  const pressedLink = pressedRow.querySelector<HTMLElement>("a[data-entry-link]");
  if (pressedLink === null) throw new Error("the kept decision draws no link");
  await user.pointer({ keys: "[MouseLeft>]", target: pressedLink });
  expect(rowOf(view, D("first")).querySelector("[data-node-body]")).not.toBeNull();
  expect(pressedRow.querySelector("[data-node-body]")).toBeNull();
  await user.pointer({ keys: "[/MouseLeft]", target: pressedLink });
  await waitFor(() => expect(pressedRow.querySelector("[data-node-body]")).not.toBeNull());
  expect(focusedRow(view)).toBe(D("kept"));
  expect(rowOf(view, D("first")).querySelector("[data-node-body]")).toBeNull();
  expect(view.getByTestId("where").textContent).toBe(`/issues/${KEY}#b-kept`);

  // The foot unfolds from its own control, which is a plain button in the tab order, and
  // folds again. Its rows stand after it and are the tree's own.
  const foot = view.getByRole("button", FOOT_CONTROL);
  expect(foot.getAttribute("aria-expanded")).toBe("false");
  expect(foot.hasAttribute("tabindex")).toBe(false);
  expect(tree.hasAttribute("aria-owns")).toBe(false);
  await user.click(foot);
  expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
  expect(foot.getAttribute("aria-expanded")).toBe("true");
  expect(words(foot)).toBe(`▾ 2 ${DECISIONS_WORDS.agents_many}`);
  const footRows = view.container.querySelector('[data-decisions-part="agents"] [role="group"]');
  expect(tree.getAttribute("aria-owns")).toBe(footRows?.id ?? "no group");
  expect(footRows?.getAttribute("aria-labelledby")).toBe(foot.id);
  await user.click(foot);
  expect(rowsOfPart(view, "agents")).toEqual([]);

  // Tab from the row the reader stands on goes to the foot's control, which is inside the
  // list: the row is drawn as focused no more, its body closed and its card plain.
  const first = rowOf(view, D("first"));
  await act(async () => first.focus());
  expect(first.querySelector("[data-node-body]")).not.toBeNull();
  expect(first.querySelector("[data-node-card]")?.className).toContain(pageFocusedRow);
  await user.tab();
  expect(document.activeElement).toBe(foot);
  expect(first.querySelector("[data-node-body]")).toBeNull();
  expect(first.querySelector("[data-node-card]")?.className).not.toContain(pageFocusedRow);
});

// R22: the foot's words agree with its count.
test("the foot's heading agrees with how many decisions it holds", () => {
  const view = renderBody({ children: [decisionNode("minor")], made: [decision("minor")] });
  expect(words(view.container.querySelector('[data-decisions-part="agents"] button'))).toBe(
    `▸ 1 ${DECISIONS_WORDS.agents_one}`
  );
});

// R2: a part that holds nothing is not drawn, and an issue that holds no ask and no decision
// says so in the page's one line for that.
test("a part that holds nothing is not drawn, and an empty issue says so once", () => {
  const some = renderBody({ children: [openAsk("q-open")] });
  expect(
    [...some.container.querySelectorAll("[data-decisions-part]")].map((part) =>
      part.getAttribute("data-decisions-part")
    )
  ).toEqual(["open_questions"]);
  expect(some.queryByText(EMPTY_LINE)).toBeNull();
  some.unmount();

  const none = renderBody({ children: [statement("s1")] });
  expect(none.container.querySelectorAll("[data-decisions-part]").length).toBe(0);
  expect(none.getByText(EMPTY_LINE)).toBeTruthy();
  // The outline's tree is still on the page, as it is under every filter.
  expect(none.getByRole("tree", { name: CONTROL_LABELS.tree })).toBeTruthy();
});

/**
 * The body as the page draws it before the reads its decisions come from hold an answer: the
 * tree read and the decisions view each answered by `read` (one that never answers, or one that
 * fails), the asks and the comments read, and the page's tree as `useIssueTree.ts` builds it
 * with no tree read answered, which holds no node of the read.
 */
function renderUnread(read: () => Promise<never>): RenderResult {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation(read);
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  const root = issueTree([]);
  const view = presetView(root, "decisions", "", new Set()).root;
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <NoTooltips>
              <HeldReads root={root}>
                {(held) => (
                  <DecisionsFilterBody
                    artifacts={artifactsBySlug(issue)}
                    emptyLine={EMPTY_LINE}
                    holdsEscape={false}
                    isClosed={false}
                    issue={issue}
                    reader={READER}
                    root={held}
                    view={view}
                  />
                )}
              </HeldReads>
            </NoTooltips>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** How one of the reads the view's decisions come from stands when the body is drawn. */
type ReadState = "answered" | "failed" | "pending";

/** The states each of those reads can stand in, in the order a table walks them. */
const READ_STATES: readonly ReadState[] = ["pending", "answered", "failed"];

/** What a read that stands as `state` gives back: `answer`, a failure at once, or nothing ever. */
function answerAs<T>(state: ReadState, answer: T): Promise<T> {
  if (state === "answered") return Promise.resolve(answer);
  if (state === "failed") {
    return Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" }));
  }
  return new Promise<never>(() => {});
}

/**
 * The body as the page draws it with each read its decisions come from standing as `reads`
 * says. The tree read and the decisions view read each answer `made`, and the asks read answers
 * no ask. A read that failed failed before the body was drawn, as the page's own observer of it
 * left it, and fails at once each time it is read again; a read that is out never answers. The
 * page's tree is as `useIssueTree.ts` builds it: the issue alone until the tree read answers, and
 * `children` under it once it has.
 */
async function renderReads(
  reads: { readonly asks: ReadState; readonly tree: ReadState; readonly view: ReadState },
  made: readonly TreeAnswerNode[],
  children: readonly TreeNode[]
): Promise<RenderResult> {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation((_key, query) =>
    answerAs(query?.view === "decisions" ? reads.view : reads.tree, treeAnswer(made))
  );
  const hold = async (queryKey: readonly unknown[], state: ReadState, answer: unknown) => {
    if (state === "answered") client.setQueryData(queryKey, answer);
    if (state === "failed") {
      await client.prefetchQuery({ queryFn: () => answerAs(state, answer), queryKey });
    }
  };
  await hold(issueTreeKey(KEY), reads.tree, treeAnswer(made));
  await hold(viewKey(KEY, "decisions"), reads.view, treeAnswer(made));
  await hold(["asks", KEY], reads.asks, []);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  const root = issueTree(reads.tree === "answered" ? children : []);
  const view = presetView(root, "decisions", "", new Set()).root;
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <NoTooltips>
              <HeldReads root={root}>
                {(held) => (
                  <DecisionsFilterBody
                    artifacts={artifactsBySlug(issue)}
                    emptyLine={EMPTY_LINE}
                    holdsEscape={false}
                    isClosed={false}
                    issue={issue}
                    reader={READER}
                    root={held}
                    view={view}
                  />
                )}
              </HeldReads>
            </NoTooltips>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// R2 and decision 98, the reviewer's R1 finding on decisions-entries.ts (review 5934996192): the
// view's decisions are the decisions view read's, else the tree read's, so while neither has
// answered the view does not know what the issue holds. It draws no line that says the issue
// has no decisions: a cold load of the Decisions view's address would say so of every issue.
test("the view says nothing of an issue's decisions while neither of their reads has answered", async () => {
  const view = renderUnread(() => new Promise<never>(() => {}));
  try {
    await waitFor(() => expect(treeReads, "the tree was never read").toHaveBeenCalled());
    expect(
      view.queryByText(EMPTY_LINE),
      "the view says the issue has no decisions before any read of them answered"
    ).toBeNull();
  } finally {
    view.unmount();
  }
});

// R2 and decision 98: where the tree read and the decisions view read both failed, the view
// holds no answer of either, and says the decisions could not be read, with a way to read them
// again, as the Spec view's column says it of the same reads; it never says the issue has none.
// Seen on the real stack at 4ce5ad5f, whose server answers the tree read 404: the Decisions view
// of an issue with decisions read "This issue has no decisions yet."
test("the view says its decisions could not be read where both of their reads failed", async () => {
  const view = renderUnread(() =>
    Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" }))
  );
  try {
    await waitFor(
      () =>
        expect(
          view.queryByText(ANSWER_LINES.decisions_unreadable),
          "the view does not say its decisions could not be read"
        ).not.toBeNull(),
      { timeout: 3000 }
    );
    expect(
      view.queryByText(EMPTY_LINE),
      "the view says the issue has no decisions where their reads failed"
    ).toBeNull();
  } finally {
    view.unmount();
  }
});

// The view's rows are the tree read's: the page's tree holds the issue alone until that read
// answers (`useIssueTree.ts`), so a decisions view read that answered first gives the view a
// decision with no row to draw it on. Until the tree read answers the view does not know what the
// issue holds, and says neither that it holds no decisions nor that they could not be read.
test("the view says nothing of an issue's decisions while the tree read is out, though its view read answered", async () => {
  const view = await renderReads(
    { asks: "answered", tree: "pending", view: "answered" },
    [decision("first", { links: [shaped(specRef("s1"))] })],
    [statement("s1"), decisionNode("first")]
  );
  try {
    await waitFor(() => expect(treeReads, "the tree was never read").toHaveBeenCalled());
    expect(
      view.queryByText(EMPTY_LINE),
      "the view says the issue has no decisions while the tree read its rows come from is out"
    ).toBeNull();
    expect(
      view.queryByText(ANSWER_LINES.decisions_unreadable),
      "the view says its decisions could not be read while the tree read is out"
    ).toBeNull();
  } finally {
    view.unmount();
  }
});

// Where the tree read failed with nothing held, the view has no row to draw a decision on, though
// the decisions view read answered: it says its decisions could not be read, with a Retry that
// reads the tree again, and never that the issue has none.
test("the view says its decisions could not be read where the tree read failed, though its view read answered", async () => {
  const user = userEvent.setup();
  const view = await renderReads(
    { asks: "answered", tree: "failed", view: "answered" },
    [decision("first", { links: [shaped(specRef("s1"))] })],
    [statement("s1"), decisionNode("first")]
  );
  try {
    await waitFor(
      () =>
        expect(
          view.queryByText(ANSWER_LINES.decisions_unreadable),
          "the view does not say its decisions could not be read where the tree read failed"
        ).not.toBeNull(),
      { timeout: 3000 }
    );
    expect(
      view.queryByText(EMPTY_LINE),
      "the view says the issue has no decisions where the tree read failed"
    ).toBeNull();
    const before = treeReads?.mock.calls.length ?? 0;
    await user.click(view.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(
        treeReads?.mock.calls.slice(before).map(([, query]) => query?.view),
        "Retry did not read the tree again, or read the decisions view too"
      ).toEqual([undefined])
    );
  } finally {
    view.unmount();
  }
});

// Where the tree read and the decisions view read both failed, the line that says so carries a
// Retry that reads both again. Once they answer, the line goes and the view draws the decision
// they hold in its part, and no line says the issue has no decisions.
test("Retry on the decisions' failed line reads them again and draws what they hold", async () => {
  const user = userEvent.setup();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The server fails every read of the tree until Retry is pressed, and then holds one decision
  // a statement rests on, which waits for the reader's review.
  let down = true;
  const made = [decision("first", { links: [shaped(specRef("s1"))] })];
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation(async () => {
    if (down) throw new ApiError(503, { code: "UNAVAILABLE", error: "down" });
    return treeAnswer(made);
  });
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  // The page's tree as `useIssueTree.ts` builds it from `children`: no node of the read before
  // the tree read answers, and the read's nodes once it has.
  const body = (children: TreeNode[]) => {
    const root = issueTree(children);
    const view = presetView(root, "decisions", "", new Set()).root;
    return (
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <KeymapProvider>
            <MarginProvider>
              <IssueScope />
              <NoTooltips>
                <HeldReads root={root}>
                  {(held) => (
                    <DecisionsFilterBody
                      artifacts={artifactsBySlug(issue)}
                      emptyLine={EMPTY_LINE}
                      holdsEscape={false}
                      isClosed={false}
                      issue={issue}
                      reader={READER}
                      root={held}
                      view={view}
                    />
                  )}
                </HeldReads>
              </NoTooltips>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const drawn = render(body([]));
  try {
    await waitFor(
      () =>
        expect(
          drawn.queryByText(ANSWER_LINES.decisions_unreadable),
          "the view does not say its decisions could not be read"
        ).not.toBeNull(),
      { timeout: 3000 }
    );
    down = false;
    await user.click(drawn.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(
        client.getQueryData<TreeAnswer>(issueTreeKey(KEY)),
        "Retry did not read the tree again"
      ).toEqual(treeAnswer(made));
      expect(
        client.getQueryData<TreeAnswer>(viewKey(KEY, "decisions")),
        "Retry did not read the decisions view again"
      ).toEqual(treeAnswer(made));
    });
    drawn.rerender(body([statement("s1"), decisionNode("first")]));
    await waitFor(() =>
      expect(rowsOfPart(drawn, "review"), "the view does not draw the decision it read").toEqual([
        D("first"),
      ])
    );
    expect(
      drawn.queryByText(ANSWER_LINES.decisions_unreadable),
      "the view still says its decisions could not be read once they were"
    ).toBeNull();
    expect(
      drawn.queryByText(EMPTY_LINE),
      "the view says the issue has no decisions where it drew one"
    ).toBeNull();
  } finally {
    drawn.unmount();
  }
});

// R2 and decision 98: the view's first part is the open questions, which come from the issue's
// asks read, as a person's decisions do. Where that read failed with nothing held, the view can
// draw the decisions the tree holds but not one question, so it says its open questions could not
// be read, with a Retry that reads the asks again, and never draws the decisions alone as though
// no question waited. Its seam names the asks read as one of its three reads
// (`decisions-entries.ts`), and `known` already waits for it.
test("the view says its open questions could not be read where the asks read failed, and Retry reads the asks again", async () => {
  const user = userEvent.setup();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const made = [decision("first", { links: [shaped(specRef("s1"))] })];
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation(async () => treeAnswer(made));
  client.setQueryData(issueTreeKey(KEY), treeAnswer(made));
  client.setQueryData(viewKey(KEY, "decisions"), treeAnswer(made));
  client.setQueryData(["comments", KEY], []);
  // The asks read failed, as the page's own observer of it left it (`useIssueTree.ts`).
  await client.prefetchQuery({
    queryFn: () => Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" })),
    queryKey: ["asks", KEY],
  });
  const asksRead = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const issue = issueDetails();
  const root = issueTree([statement("s1"), decisionNode("first")]);
  const view = presetView(root, "decisions", "", new Set()).root;
  const drawn = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <NoTooltips>
              <HeldReads root={root}>
                {(held) => (
                  <DecisionsFilterBody
                    artifacts={artifactsBySlug(issue)}
                    emptyLine={EMPTY_LINE}
                    holdsEscape={false}
                    isClosed={false}
                    issue={issue}
                    reader={READER}
                    root={held}
                    view={view}
                  />
                )}
              </HeldReads>
            </NoTooltips>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(
      () =>
        expect(
          drawn.queryByText(ANSWER_LINES.questions_unreadable),
          "the view does not say its open questions could not be read where the asks read failed"
        ).not.toBeNull(),
      { timeout: 3000 }
    );
    await user.click(drawn.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(asksRead, "Retry did not read the asks again").toHaveBeenCalledTimes(1)
    );
  } finally {
    drawn.unmount();
    asksRead.mockRestore();
  }
});

// Where the decisions' reads and the asks read have all failed with nothing held, the view draws
// two lines that name two different reads, and each line's Retry reads its own reads alone
// again: the decisions' the tree read and the decisions view, the asks' the asks read.
test("where the decisions' reads and the asks read both failed, each line names its own read and its Retry reads only that", async () => {
  const user = userEvent.setup();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation(async () => {
    throw new ApiError(503, { code: "UNAVAILABLE", error: "down" });
  });
  client.setQueryData(["comments", KEY], []);
  // The asks read failed, as the page's own observer of it left it (`useIssueTree.ts`).
  await client.prefetchQuery({
    queryFn: () => Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" })),
    queryKey: ["asks", KEY],
  });
  const asksRead = spyOn(api, "listIssueAsks").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "down" })
  );
  const issue = issueDetails();
  const root = issueTree([]);
  const view = presetView(root, "decisions", "", new Set()).root;
  const drawn = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <NoTooltips>
              <HeldReads root={root}>
                {(held) => (
                  <DecisionsFilterBody
                    artifacts={artifactsBySlug(issue)}
                    emptyLine={EMPTY_LINE}
                    holdsEscape={false}
                    isClosed={false}
                    issue={issue}
                    reader={READER}
                    root={held}
                    view={view}
                  />
                )}
              </HeldReads>
            </NoTooltips>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const retryOf = (words: string): HTMLButtonElement => {
    const button = drawn
      .getByText(words)
      .closest('[role="alert"]')
      ?.querySelector<HTMLButtonElement>("button");
    if (button == null) throw new Error(`the line "${words}" draws no Retry`);
    return button;
  };
  try {
    await waitFor(
      () => {
        expect(
          drawn.queryByText(ANSWER_LINES.decisions_unreadable),
          "the view does not say its decisions could not be read"
        ).not.toBeNull();
        expect(
          drawn.queryByText(ANSWER_LINES.questions_unreadable),
          "the view does not say its open questions could not be read"
        ).not.toBeNull();
      },
      { timeout: 3000 }
    );
    expect(drawn.queryByText(EMPTY_LINE), "the view says the issue has no decisions").toBeNull();

    const treeBefore = treeReads.mock.calls.length;
    await user.click(retryOf(ANSWER_LINES.questions_unreadable));
    await waitFor(() =>
      expect(
        asksRead,
        "the open questions' Retry did not read the asks again"
      ).toHaveBeenCalledTimes(1)
    );
    expect(
      treeReads.mock.calls.length,
      "the open questions' Retry read the tree or the decisions view again"
    ).toBe(treeBefore);

    await user.click(retryOf(ANSWER_LINES.decisions_unreadable));
    await waitFor(() =>
      expect(
        treeReads?.mock.calls.length,
        "the decisions' Retry did not read the tree or the decisions view again"
      ).toBeGreaterThan(treeBefore)
    );
    expect(asksRead, "the decisions' Retry read the asks again").toHaveBeenCalledTimes(1);
  } finally {
    drawn.unmount();
    asksRead.mockRestore();
  }
});

// Each read the view's decisions come from can be out, answered or failed when the body is drawn,
// and the view draws only what the reads that answered say: a decision's row once the tree read
// its rows come from has answered; the line that its decisions could not be read where the tree
// read failed, or is out while the decisions view read failed; the line that its open questions
// could not be read where the asks read failed; and the line that the issue has no decisions only
// where it holds none and the tree read and the asks read have both answered.
test("every state of the tree read, the decisions view read and the asks read draws what the view knows", async () => {
  const issues = [
    {
      children: [statement("s1"), decisionNode("first")],
      holds: true,
      made: [decision("first", { links: [shaped(specRef("s1"))] })],
    },
    { children: [statement("s1")], holds: false, made: [] },
  ];
  const combinations = READ_STATES.flatMap((tree) =>
    READ_STATES.flatMap((view) => READ_STATES.map((asks) => ({ asks, tree, view })))
  );
  for (const { children, holds, made } of issues) {
    for (const reads of combinations) {
      const combination = `an issue that holds ${holds ? "one waiting decision" : "no decision"}, with the tree read ${reads.tree}, the decisions view read ${reads.view} and the asks read ${reads.asks}`;
      const drawn = await renderReads(reads, made, children);
      try {
        await waitFor(
          () => {
            expect(
              drawn.container.querySelector(`[data-node-row][data-node-id="${D("first")}"]`) !==
                null,
              `${combination}: whether the decision's row is drawn`
            ).toBe(holds && reads.tree === "answered");
            expect(
              drawn.queryByText(ANSWER_LINES.decisions_unreadable) !== null,
              `${combination}: whether the view says its decisions could not be read`
            ).toBe(
              reads.tree === "failed" || (reads.tree === "pending" && reads.view === "failed")
            );
            expect(
              drawn.queryByText(ANSWER_LINES.questions_unreadable) !== null,
              `${combination}: whether the view says its open questions could not be read`
            ).toBe(reads.asks === "failed");
            expect(
              drawn.queryByText(EMPTY_LINE) !== null,
              `${combination}: whether the view says the issue has no decisions`
            ).toBe(!holds && reads.tree === "answered" && reads.asks === "answered");
          },
          { timeout: 3000 }
        );
      } finally {
        drawn.unmount();
      }
    }
  }
});

/**
 * The body over `client`, drawn over the page's tree as `useIssueTree.ts` builds it from
 * `children`: the issue alone until the case hands it the nodes of a tree read that answered.
 * The case puts the reads in `client` and answers the tree read and its views (`treeReads`).
 * `again` draws the body over the tree built from other nodes, as the page does once the tree
 * read answers.
 */
function renderOver(
  client: QueryClient,
  children: TreeNode[]
): RenderResult & { readonly again: (children: TreeNode[]) => void } {
  const issue = issueDetails();
  const body = (nodes: TreeNode[]) => {
    const root = issueTree(nodes);
    const view = presetView(root, "decisions", "", new Set()).root;
    return (
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <KeymapProvider>
            <MarginProvider>
              <IssueScope />
              <NoTooltips>
                <HeldReads root={root}>
                  {(held) => (
                    <DecisionsFilterBody
                      artifacts={artifactsBySlug(issue)}
                      emptyLine={EMPTY_LINE}
                      holdsEscape={false}
                      isClosed={false}
                      issue={issue}
                      reader={READER}
                      root={held}
                      view={view}
                    />
                  )}
                </HeldReads>
              </NoTooltips>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const drawn = render(body(children));
  return { ...drawn, again: (nodes) => drawn.rerender(body(nodes)) };
}

/** The line a failed read draws over the list, read as a reader reads it: what it says, and its
 *  Retry; null where the list draws no line for `read`. */
function failedLine(drawn: RenderResult, read: "asks" | "decisions") {
  const line = drawn.container.querySelector(`[data-decisions-unreadable="${read}"]`);
  return line === null
    ? null
    : { retry: words(line.querySelector("button")), says: words(line.querySelector("p")) };
}

/** The Retry of the line a failed read draws over the list. */
function retryOn(drawn: RenderResult, read: "asks" | "decisions"): HTMLElement | null {
  return drawn.container.querySelector(`[data-decisions-unreadable="${read}"] button`);
}

// A read that failed before it first answered says so in a line of its own over the list, with
// a Retry, in place of the entries it would give, and never that the issue holds nothing. The
// tree's own focus, which `t d` gives it, goes to that Retry where the list draws no row, and the
// focus stays in the list once the line goes: at no point is it left on the page's body.
test("a failed read's line takes the focus the empty tree holds, and the focus stays in the list once it goes", async () => {
  const user = userEvent.setup();
  const made = [decision("first", { links: [shaped(specRef("s1"))] })];

  // The decisions' reads, the tree read and the decisions view, fail until the reader presses
  // Retry with Enter; then they answer, and the tree the page builds from that read holds the
  // decision once the page draws it.
  let down = true;
  treeReads ??= spyOn(api, "getIssueTree");
  treeReads.mockImplementation(async () => {
    if (down) throw new ApiError(503, { code: "UNAVAILABLE", error: "the store is not reachable" });
    return treeAnswer(made);
  });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  const failed = renderOver(client, []);
  try {
    await waitFor(
      () =>
        expect(failedLine(failed, "decisions")).toEqual({
          retry: "Retry",
          says: ANSWER_LINES.decisions_unreadable,
        }),
      { timeout: 3000 }
    );
    expect(failedLine(failed, "asks")).toBeNull();
    expect(failed.container.querySelectorAll("[data-decisions-part]").length).toBe(0);
    expect(failed.queryByText(EMPTY_LINE)).toBeNull();
    const tree = failed.getByRole("tree", { name: CONTROL_LABELS.tree });
    expect(tree.contains(retryOn(failed, "decisions"))).toBe(false);
    await act(async () => tree.focus());
    expect(document.activeElement).toBe(retryOn(failed, "decisions"));

    down = false;
    await user.keyboard("{Enter}");
    await waitFor(() => expect(client.getQueryData(issueTreeKey(KEY))).toBeDefined());
    expect(failedLine(failed, "decisions")).toBeNull();
    // The line is gone and the page's tree does not hold the decision yet: the tree holds the
    // focus, in the list.
    expect(document.activeElement).toBe(tree);
    failed.again([statement("s1"), decisionNode("first")]);
    await waitFor(() => expect(focusedRow(failed)).toBe(D("first")));
    expect(document.activeElement).not.toBe(document.body);
  } finally {
    failed.unmount();
  }

  // The asks read fails the same way, as the page's own observer of it left it
  // (`useIssueTree.ts`): its line in place of the questions it would give, no line that the
  // issue holds nothing, and the tree's focus on its Retry where no row is drawn.
  const asksDown = async (over: QueryClient) => {
    await over.prefetchQuery({
      queryFn: () => Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" })),
      queryKey: ["asks", KEY],
    });
  };
  const refusedAsks = spyOn(api, "listIssueAsks").mockRejectedValue(new Error("refused"));
  try {
    treeReads.mockImplementation(async () => treeAnswer([]));
    const bare = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    bare.setQueryData(issueTreeKey(KEY), treeAnswer([]));
    bare.setQueryData(viewKey(KEY, "decisions"), treeAnswer([]));
    bare.setQueryData(["comments", KEY], []);
    await asksDown(bare);
    const noRow = renderOver(bare, [statement("s1")]);
    await waitFor(() =>
      expect(failedLine(noRow, "asks")).toEqual({
        retry: "Retry",
        says: ANSWER_LINES.questions_unreadable,
      })
    );
    expect(failedLine(noRow, "decisions")).toBeNull();
    expect(noRow.container.querySelectorAll("[data-decisions-part]").length).toBe(0);
    expect(noRow.queryByText(EMPTY_LINE)).toBeNull();
    await act(async () => noRow.getByRole("tree", { name: CONTROL_LABELS.tree }).focus());
    expect(document.activeElement).toBe(retryOn(noRow, "asks"));
    noRow.unmount();

    // With the decisions drawn, their rows take the focus and the Retry does not: a focus on the
    // tree, as the page's chord gives one where it finds no tab-stop row, or on the tab-stop row
    // itself goes to a decision. Shift+Tab from that row takes the focus up to the Retry over the
    // list, which is off every row, as the foot's control is, so the row the reader left is drawn
    // as one they are not on.
    treeReads.mockImplementation(async () => treeAnswer(made));
    const held = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    held.setQueryData(issueTreeKey(KEY), treeAnswer(made));
    held.setQueryData(viewKey(KEY, "decisions"), treeAnswer(made));
    held.setQueryData(["comments", KEY], []);
    await asksDown(held);
    const partial = renderOver(held, [statement("s1"), decisionNode("first")]);
    await waitFor(() => expect(retryOn(partial, "asks")).not.toBeNull());
    await act(async () => partial.getByRole("tree", { name: CONTROL_LABELS.tree }).focus());
    expect(focusedRow(partial)).toBe(D("first"));
    await act(async () => (document.activeElement as HTMLElement).blur());
    const row = rowOf(partial, D("first"));
    await act(async () => row.focus());
    expect(focusedRow(partial)).toBe(D("first"));
    expect(row.querySelector("[data-node-body]")).not.toBeNull();
    expect(row.querySelector("[data-node-card]")?.className).toContain(pageFocusedRow);
    await user.tab({ shift: true });
    expect(document.activeElement).toBe(retryOn(partial, "asks"));
    expect(row.querySelector("[data-node-body]")).toBeNull();
    expect(row.querySelector("[data-node-card]")?.className).not.toContain(pageFocusedRow);
    expect(focusedRow(partial)).toBeUndefined();
    partial.unmount();
  } finally {
    refusedAsks.mockRestore();
  }
});

// R8: `t d` gives the view's tree the focus as the view opens, and on a running page that is
// before a failed read says so: the page's read is out as the view mounts, and the refusal comes
// back a second or so later. Where the list draws no row, the focus the tree holds then goes to
// that read's Retry once its line is drawn, for each of the two lines - the decisions', whose
// reads are the tree read and the decisions view, and the asks' - so Enter on it asks again; it
// is never left on a tree that holds nothing.
test("a read refused after the tree took the focus gives that focus to its Retry", async () => {
  for (const read of ["decisions", "asks"] as const) {
    const answer = Promise.withResolvers<never>();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    client.setQueryData(["comments", KEY], []);
    treeReads ??= spyOn(api, "getIssueTree");
    const asks = spyOn(api, "listIssueAsks");
    if (read === "decisions") {
      treeReads.mockReturnValue(answer.promise);
      client.setQueryData(["asks", KEY], []);
    } else {
      treeReads.mockImplementation(async () => treeAnswer([]));
      client.setQueryData(issueTreeKey(KEY), treeAnswer([]));
      client.setQueryData(viewKey(KEY, "decisions"), treeAnswer([]));
      // The page's own read of the asks, which the view watches and does not send.
      asks.mockReturnValue(answer.promise);
      void client.prefetchQuery({
        queryFn: () => api.listIssueAsks(KEY, "all"),
        queryKey: ["asks", KEY],
      });
    }
    const view = renderOver(client, [statement("s1")]);
    try {
      const tree = view.getByRole("tree", { name: CONTROL_LABELS.tree });
      await act(async () => tree.focus());
      expect(document.activeElement).toBe(tree);
      await act(async () => {
        answer.reject(
          new ApiError(503, { code: "UNAVAILABLE", error: "the store is not reachable" })
        );
        await answer.promise.catch(() => undefined);
      });
      // Where the focus is, by name: a Retry by the read its line is for.
      const focusIs = () => {
        const active = document.activeElement;
        const line = active?.closest("[data-decisions-unreadable]");
        if (line != null) return `the ${line.getAttribute("data-decisions-unreadable")} Retry`;
        return active === tree ? "the tree" : (active?.tagName ?? "nothing");
      };
      await waitFor(() => expect(failedLine(view, read)).not.toBeNull());
      await waitFor(() => expect(focusIs()).toBe(`the ${read} Retry`));
    } finally {
      view.unmount();
      asks.mockRestore();
    }
  }
});

// R2: wherever a decision is drawn, one waiting for a person's review reads "Waiting for
// review", one a person kept "Kept by" with their name, and a replaced one "Replaced", each by
// a word and a shape (R6).
test("a decision reads how it stands in the three states the page names", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  const state = (id: string) => {
    const row = rowOf(view, id);
    const word = row.querySelector("[data-node-margin] [data-node-state]");
    const box = word?.parentElement;
    return {
      // The box that holds the state's word and its shape carries its colour: a review row's
      // state is violet, and a foot row's grey.
      colour: (["decision", "quiet"] as const).find((tone) =>
        box?.classList.contains(pageStateText[tone])
      ),
      shape: box?.querySelector("svg")?.getAttribute("aria-label") ?? null,
      token: row.getAttribute("data-node-state"),
      word: words(word),
    };
  };
  expect(state(D("second"))).toEqual({
    colour: "decision",
    shape: GLYPH_NAMES[GLYPH_TOKENS.states.decision.waiting_review],
    token: "waiting_review",
    word: NODE_STATE_WORDS.decision.waiting_review,
  });
  // The name is the person's own, drawn as an author's text and never as an id of the page's.
  expect(state(D("kept"))).toEqual({
    colour: "decision",
    shape: GLYPH_NAMES[GLYPH_TOKENS.states.decision.kept],
    token: "kept",
    word: `${NODE_STATE_WORDS.decision.kept} alice`,
  });
  expect(
    rowOf(view, D("kept")).querySelector("[data-kept-by][data-author-text]")?.textContent
  ).toBe("alice");
  expect(state(D("replaced"))).toEqual({
    colour: "quiet",
    shape: GLYPH_NAMES[GLYPH_TOKENS.states.decision.replaced],
    token: "replaced",
    word: NODE_STATE_WORDS.decision.replaced,
  });
  // A decision that waits and is tied to nothing reads the model's own state where it stands
  // (R35: "a second rule decides the mark on a row").
  expect(state(D("minor"))).toMatchObject({
    colour: "quiet",
    word: NODE_STATE_WORDS.decision.waiting_review,
  });
  // The one word the model holds for an agent's doubt, on the decision it marks.
  expect(words(rowOf(view, D("second")).querySelector("[data-node-unsure]"))).toBe(
    CONTROL_LABELS.unsure
  );
  expect(rowOf(view, D("first")).querySelector("[data-node-unsure]")).toBeNull();
});

// R35: the review list's decisions each draw Keep and Change with no focus on them, and no
// other row of the filter draws either: not the reader's own decisions, not a question, and
// not a decision no statement rests on, whether or not it waits.
test("Keep and Change are drawn on the review list's rows and on no other", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  const controls = (id: string) =>
    [...rowOf(view, id).querySelectorAll("button[data-row-action]")].map((button) => [
      button.getAttribute("data-row-action"),
      button.textContent,
    ]);
  expect(document.activeElement?.closest("[data-node-row]") ?? null).toBeNull();
  for (const id of [D("second"), D("first")]) {
    expect(controls(id)).toEqual([
      ["keep", CONTROL_LABELS.keep_decision],
      ["change", CONTROL_LABELS.change_decision],
    ]);
  }
  for (const id of [D("kept"), D("minor"), D("replaced"), "q-open", "a-answered", "a-declined"]) {
    expect(controls(id)).toEqual([]);
  }
  // Nor does the focus draw them on a row that is no entry of the list.
  await act(async () => rowOf(view, D("minor")).focus());
  expect(controls(D("minor"))).toEqual([]);
  // Every control inside a row is out of the tab order (R9): of everything in the body that
  // can take the focus, only the row the tab stop rests on and the foot's own control are
  // reachable by Tab, and every other one carries tabindex -1.
  const reachable = [
    ...view.container.querySelectorAll<HTMLElement>(
      "[data-decisions-body] :is(button, a[href], input, textarea, select, [tabindex])"
    ),
  ].filter((element) => element.getAttribute("tabindex") !== "-1");
  expect(
    reachable
      .map((element) =>
        element.hasAttribute("data-node-row")
          ? "the row"
          : element.hasAttribute("data-decisions-heading")
            ? "the foot's control"
            : element.outerHTML
      )
      .sort()
  ).toEqual(["the foot's control", "the row"]);
});

// R2, R35: Keep is `DecisionActions.tsx`'s own write. The kept decision leaves the review list
// for the reader's own decisions, where it reads "Kept by" with their name, once the decisions
// view the Keep reads again says so, and the focus goes to the decision that stood after it.
test("a kept decision moves to the reader's own decisions and the focus to the next", async () => {
  const user = userEvent.setup();
  const whole = wholeIssue();
  const view = renderBody(whole);
  // The Keep the server writes, which the decisions view read after it answers.
  const written = { ...KEPT, block_id: "second", kept_at: "2026-11-16T09:00:00Z" };
  const kept = spyOn(api, "keepDecision").mockImplementation(async () => {
    view.serve({
      made: whole.made.map((made) =>
        made.id === "second" ? { ...made, kept: written, waiting_review: false } : made
      ),
    });
    return written as never;
  });
  try {
    const keep = rowOf(view, D("second")).querySelector('button[data-row-action="keep"]');
    if (keep === null) throw new Error("the review row draws no Keep");
    await user.click(keep);
    await waitFor(() => expect(kept).toHaveBeenCalled());
    // The Keep is of the words this row drew: the block and the sum of its text.
    expect(kept.mock.calls[0]?.slice(0, 3)).toEqual([SPEC_DOCUMENT.id, "second", "sha256:second"]);

    await waitFor(() => expect(rowsOfPart(view, "review")).toEqual([D("first")]));
    expect(rowsOfPart(view, "yours")).toEqual([D("second"), D("kept"), "a-answered", "a-declined"]);
    expect(
      words(rowOf(view, D("second")).querySelector("[data-node-margin] [data-node-state]"))
    ).toBe(`${NODE_STATE_WORDS.decision.kept} alice`);
    expect(rowOf(view, D("second")).querySelectorAll("button[data-row-action]").length).toBe(0);
    await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
  } finally {
    kept.mockRestore();
  }
});

// R6: the focus goes on to the next decision for the reader's own Keep or Change alone. A
// decision that leaves the review list by no act of the reader on this list - a Change sent
// from another page, a read that says another person kept it, even after a Keep of the
// reader's that Dispatch refused, or that it was replaced - keeps the focus where it now
// stands: under Your decisions, or in the foot, which unfolds for it.
test("a decision that leaves the review list by no act of the reader keeps the focus", async () => {
  const whole = wholeIssue();
  // The address names the decision, and a Change on it arrives in the tree read.
  const changed = renderBody(whole, { reveal: D("second") });
  await waitFor(() => expect(focusedRow(changed)).toBe(D("second")));
  await act(async () =>
    changed.client.setQueryData(
      issueTreeKey(KEY),
      treeAnswer([
        ...whole.made,
        changeComment("change-second", "second", "2026-11-16T09:00:00Z", "Name the read, please."),
      ])
    )
  );
  await waitFor(() => expect(rowsOfPart(changed, "review")).toEqual([D("first")]));
  expect(rowsOfPart(changed, "yours")[0]).toBe(D("second"));
  expect(rowOf(changed, D("second")).getAttribute("data-node-state")).toBe("change_requested");
  expect(focusedRow(changed)).toBe(D("second"));
  changed.unmount();

  // The reader is on the decision, and the decisions view, read again, says bob kept it.
  const keptByBob = treeAnswer(
    whole.made.map((made) =>
      made.id === "second"
        ? {
            ...made,
            kept: { ...KEPT, block_id: "second", kept_by: "bob" },
            waiting_review: false,
          }
        : made
    )
  );
  const kept = renderBody(whole);
  await act(async () => rowOf(kept, D("second")).focus());
  await act(async () => kept.client.setQueryData(viewKey(KEY, "decisions"), keptByBob));
  await waitFor(() => expect(rowsOfPart(kept, "review")).toEqual([D("first")]));
  expect(rowsOfPart(kept, "yours")).toContain(D("second"));
  expect(
    words(rowOf(kept, D("second")).querySelector("[data-node-margin] [data-node-state]"))
  ).toBe(`${NODE_STATE_WORDS.decision.kept} bob`);
  expect(focusedRow(kept)).toBe(D("second"));
  kept.unmount();

  // The reader is on the decision, and the decisions view, read again, says it was replaced,
  // which puts it in the folded foot.
  const replaced = renderBody(whole);
  await act(async () => rowOf(replaced, D("second")).focus());
  await act(async () =>
    replaced.client.setQueryData(
      viewKey(KEY, "decisions"),
      treeAnswer(
        whole.made.map((made) =>
          made.id === "second" ? { ...made, replaced: true, waiting_review: false } : made
        )
      )
    )
  );
  await waitFor(() => expect(rowsOfPart(replaced, "review")).toEqual([D("first")]));
  await waitFor(() => expect(rowsOfPart(replaced, "agents")).toContain(D("second")));
  expect(rowOf(replaced, D("second")).getAttribute("data-node-state")).toBe("replaced");
  await waitFor(() => expect(focusedRow(replaced)).toBe(D("second")));
  replaced.unmount();

  // The reader pressed Keep on the decision and Dispatch refused it, which the row says; then the
  // decisions view says bob kept it. A refused Keep is no review of the reader's, so the focus
  // stays on the decision where it now stands, not on the next one.
  const user = userEvent.setup();
  const refusal = spyOn(api, "keepDecision").mockRejectedValue(
    new ApiError(409, {
      code: "DECISION_CHANGED",
      error: "The decision changed since you read it.",
    })
  );
  const refused = renderBody(whole);
  try {
    const keep = rowOf(refused, D("second")).querySelector<HTMLElement>(
      'button[data-row-action="keep"]'
    );
    if (keep === null) throw new Error("the review row draws no Keep");
    await user.click(keep);
    await waitFor(() =>
      expect(
        rowOf(refused, D("second")).querySelector('[data-entry-actions] [role="alert"]')
      ).not.toBeNull()
    );
    expect(focusedRow(refused)).toBe(D("second"));
    await act(async () => refused.client.setQueryData(viewKey(KEY, "decisions"), keptByBob));
    await waitFor(() => expect(rowsOfPart(refused, "review")).toEqual([D("first")]));
    await waitFor(() => expect(focusedRow(refused)).toBe(D("second")));
  } finally {
    refusal.mockRestore();
  }
});

// R6, the class of the review list's case above (the tester's round 2): an entry that moves to
// another part of the list by no act of the reader keeps the focus where it now stands. Another
// person answers the open question the reader is on, which moves it under Your decisions; the
// decisions view, read again, ties the foot's decision the reader is on to a statement, which
// moves it to the review list. Each row is drawn again in its new part, and the focus is on it
// there, not on the page.
test("an entry that moves to another part by no act of the reader keeps the focus", async () => {
  const whole = wholeIssue();
  const answered = renderBody(whole);
  await act(async () => rowOf(answered, "q-open").focus());
  expect(focusedRow(answered)).toBe("q-open");
  answered.again({
    children: whole.children.map((child) =>
      child.id === "q-open"
        ? answeredAsk(
            "q-open",
            { at: "2026-11-16T09:00:00Z", by: { id: "bob", kind: "user" }, text: "At 1280 px." },
            {
              options: [
                { description: "Where the rail fits.", label: "1280 px" },
                { label: "1024 px" },
              ],
              text: "At which width does the header condense?",
            }
          )
        : child
    ),
  });
  await waitFor(() => expect(rowsOfPart(answered, "yours")).toContain("q-open"));
  expect(rowsOfPart(answered, "open_questions")).toEqual([]);
  await waitFor(() => expect(focusedRow(answered)).toBe("q-open"));
  answered.unmount();

  const tied = renderBody(whole);
  await act(async () => tied.getByRole("button", FOOT_CONTROL).click());
  await waitFor(() => expect(rowsOfPart(tied, "agents")).toContain(D("minor")));
  await act(async () => rowOf(tied, D("minor")).focus());
  expect(focusedRow(tied)).toBe(D("minor"));
  await act(async () =>
    tied.client.setQueryData(
      viewKey(KEY, "decisions"),
      treeAnswer(
        whole.made.map((made) =>
          made.id === "minor" ? { ...made, links: [shaped(specRef("s3"))] } : made
        )
      )
    )
  );
  await waitFor(() => expect(rowsOfPart(tied, "review")).toContain(D("minor")));
  await waitFor(() => expect(focusedRow(tied)).toBe(D("minor")));
});

// A closed issue takes no write, so the review list keeps its rows and draws no control.
test("a closed issue draws the review list with no Keep and no Change", () => {
  const view = renderBody(wholeIssue(), { isClosed: true });
  expect(rowsOfPart(view, "review")).toEqual([D("second"), D("first")]);
  expect(view.container.querySelectorAll("button[data-row-action]").length).toBe(0);
});

// Decision 110 (2), R6: an entry the reader is on that another person deletes takes its row,
// and the focus with it, off the page. The focus goes to the entry that stood after it in its
// part, else to the one before it where it was the last, else, where its part is left empty,
// to the view's first row, else, where no row is left, to the tree: never to the page.
test("a focused entry that another person deletes gives the focus to its neighbour", async () => {
  /** Reads the issue again with `block`'s decision deleted: gone from the tree and from the
   *  decisions view. */
  const deleted = async (
    view: Drawn,
    issue: { children: TreeNode[]; made: TreeAnswerNode[] },
    block: string
  ) => {
    await act(async () => {
      view.client.setQueryData(
        viewKey(KEY, "decisions"),
        treeAnswer(issue.made.filter((made) => made.id !== block))
      );
      view.again({ children: issue.children.filter((child) => child.id !== D(block)) });
    });
  };
  const whole = wholeIssue();

  // The newer of the two decisions that wait for review: the focus goes to the older, after it.
  const next = renderBody(whole);
  await act(async () => rowOf(next, D("second")).focus());
  await deleted(next, whole, "second");
  expect(rowsOfPart(next, "review")).toEqual([D("first")]);
  await waitFor(() => expect(focusedRow(next)).toBe(D("first")));
  next.unmount();

  // The older, the last of them: the focus goes to the newer, before it.
  const last = renderBody(whole);
  await act(async () => rowOf(last, D("first")).focus());
  await deleted(last, whole, "first");
  expect(rowsOfPart(last, "review")).toEqual([D("second")]);
  await waitFor(() => expect(focusedRow(last)).toBe(D("second")));
  last.unmount();

  // The reader's one decision, the only entry of its part: the part goes, and the focus goes to
  // the view's first row, the open question, not to the decision drawn just before it.
  const own = {
    children: [statement("s1"), openAsk("q-open"), decisionNode("first"), decisionNode("kept")],
    made: [
      decision("first", { links: [shaped(specRef("s1"))] }),
      decision("kept", { kept: KEPT, links: [shaped(specRef("s1"))], waiting_review: false }),
    ],
  };
  const only = renderBody(own);
  expect(rowsOfPart(only, "yours")).toEqual([D("kept")]);
  await act(async () => rowOf(only, D("kept")).focus());
  await deleted(only, own, "kept");
  expect(only.container.querySelector('[data-decisions-part="yours"]')).toBeNull();
  await waitFor(() => expect(focusedRow(only)).toBe("q-open"));
  only.unmount();

  // The view's one entry: no row is left to take the focus, and the tree keeps it, which leaves
  // it in the list.
  const lone = {
    children: [statement("s1"), decisionNode("first")],
    made: [decision("first", { links: [shaped(specRef("s1"))] })],
  };
  const alone = renderBody(lone);
  await act(async () => rowOf(alone, D("first")).focus());
  await deleted(alone, lone, "first");
  expect(alone.container.querySelector("[data-node-row]")).toBeNull();
  await waitFor(() => expect(document.activeElement).toBe(alone.getByRole("tree")));
  // The focus is off every row, so what the deleted row's focus opened is let go: the same
  // decision read back draws as a row the reader is not on.
  await act(async () => {
    alone.client.setQueryData(viewKey(KEY, "decisions"), treeAnswer(lone.made));
    alone.again({ children: lone.children });
  });
  await waitFor(() => expect(rowOf(alone, D("first"))).toBeTruthy());
  expect(rowOf(alone, D("first")).querySelector("[data-node-body]")).toBeNull();
});

// R2, R9, c-decisions: an answered ask draws its question and its options, and the person's
// answer character for character as the store holds it, with no sentence of the page's beside
// it restating it and no reading: the agent's write-up of the answer is no row of the filter.
test("an answered ask draws the person's answer as stored, and no reading", async () => {
  const view = renderBody(wholeIssue());
  const row = rowOf(view, "a-answered");

  expect(words(row.querySelector("[data-node-words]"))).toBe("Does the built page work for you?");
  expect(
    [...row.querySelectorAll("[data-node-option]")].map((option) => [
      option.getAttribute("data-node-option"),
      option.textContent,
    ])
  ).toEqual([
    ["", "○Works"],
    ["chosen", "●Changes needed"],
  ]);
  // Every character, the line breaks and the doubled spaces included.
  expect(row.querySelector("[data-answer-text]")?.textContent).toBe(STORED);
  expect(words(row.querySelector("[data-answer-author]"))).toBe("alice");
  // The reader's own answer carries the ink rule their own words carry (R34): each class of the
  // page's rule, so the reading holds whatever classes the theme writes the rule with.
  const answer = row.querySelector("[data-node-answer]");
  const missing = pageOwnWordsRule.split(" ").filter((name) => !answer?.classList.contains(name));
  expect(missing).toEqual([]);

  // With the focus on it the same holds, and what the page writes of its own on the row is the
  // kind, the state, the word naming the answer, the link and the body's one control.
  await act(async () => row.focus());
  expect(row.querySelector("[data-answer-text]")?.textContent).toBe(STORED);
  // (the two marks are the options' own: the one chosen and the one not).
  expect(chromeOf(row).sort()).toEqual(
    [
      NODE_TYPE_NAMES.ask,
      "·",
      NODE_STATE_WORDS.ask.answered,
      "○",
      "●",
      CONTROL_LABELS.answer,
      IN_CONVERSATION,
      CONTROL_LABELS.keep_open,
    ].sort()
  );

  // The agent's write-up of the answer is drawn nowhere in the filter.
  expect(view.container.querySelector(`[data-node-id="${D("reading")}"]`)).toBeNull();
  expect(view.queryByText("The reader wants the rail kept.")).toBeNull();
});

// R2: a declined ask reads as a question the person declined, never as an answer: its state
// word, who declined it by a name and a shape, and the note they wrote, word for word.
test("a declined ask says who declined it and draws no answer", () => {
  const view = renderBody(wholeIssue());
  const row = rowOf(view, "a-declined");

  expect(row.getAttribute("data-node-state")).toBe("declined");
  expect(words(row.querySelector("[data-node-margin] [data-node-state]"))).toBe(
    NODE_STATE_WORDS.ask.declined
  );
  expect(words(row.querySelector("[data-ask-declined] p"))).toBe(
    `alice ${DECISIONS_WORDS.declined}`
  );
  expect(row.querySelector("[data-declined-by] svg")?.getAttribute("data-glyph")).toBe(
    GLYPH_TOKENS.authors.person
  );
  expect(row.querySelector("[data-declined-note]")?.textContent).toBe(
    "The release owner decides this one."
  );
  expect(row.querySelector("[data-node-answer]")).toBeNull();
  expect(row.querySelector("[data-answer-text]")).toBeNull();
});

// c-decisions: "The decision that shaped three requirements is one entry with one node id, and
// it reaches all three. Each entry links to the node that holds it."
test("a decision reaches each statement it shaped, and every entry links to its node", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  expect(view.container.querySelectorAll(`[data-node-id="${D("second")}"]`).length).toBe(1);
  const informs = rowOf(view, D("second")).querySelector("[data-decision-informs]");
  expect(words(informs?.querySelector("[data-page-chrome]"))).toBe(DECISIONS_WORDS.informs);
  expect(
    [...(informs?.querySelectorAll("a") ?? [])].map((link) => [
      link.textContent,
      link.getAttribute("href"),
    ])
  ).toEqual([
    // Each statement's own words, "The page draws sN. It draws nothing else.", cut by the
    // chips' rule within 32 characters.
    ["The page draws s1. It draws…", `/issues/${KEY}#b-s1`],
    ["The page draws s2. It draws…", `/issues/${KEY}#b-s2`],
    ["The page draws s3. It draws…", `/issues/${KEY}#b-s3`],
  ]);
  // A decision tied to nothing names nothing.
  expect(rowOf(view, D("minor")).querySelector("[data-decision-informs]")).toBeNull();

  const link = (id: string) => {
    const found = rowOf(view, id).querySelector("a[data-entry-link]");
    return [words(found), found?.getAttribute("href") ?? null];
  };
  expect(link("q-open")).toEqual([IN_CONVERSATION, `/issues/${KEY}/asks/q-open`]);
  expect(link("a-answered")).toEqual([IN_CONVERSATION, `/issues/${KEY}/asks/a-answered`]);
  expect(link(D("first"))).toEqual([IN_CONVERSATION, `/issues/${KEY}#b-first`]);
  expect(link(D("minor"))).toEqual([IN_CONVERSATION, `/issues/${KEY}#b-minor`]);
  // Every row the body draws carries one.
  expect(
    [...view.container.querySelectorAll("[data-node-row]")].filter(
      (row) => row.querySelectorAll("a[data-entry-link]").length !== 1
    )
  ).toEqual([]);
  view.unmount();

  // A parent with no words of its own is named by its kind's word, once, and never by its id
  // (R7): here an open question that holds no words, which the decision was made under.
  const wordless = renderBody({
    children: [
      statement("s1"),
      openAsk("q2", true, { children: [decisionNode("wordless")], text: "" }),
    ],
    made: [decision("wordless", { links: [shaped(specRef("s1"))] })],
  });
  const parent = rowOf(wordless, D("wordless")).querySelector("[data-node-parent-line]");
  expect(parent?.getAttribute("data-parent-id")).toBe("q2");
  expect(words(parent)).toBe(NODE_TYPE_NAMES.ask);
});

// R5 (c), ruling 5: a decision's row draws no author in its margin, so its focused body says
// the model holds none, in the page's own words; an agent's margin names it by its role's word
// alone, so its focused body gives the session's whole title.
test("a focused row says Author unknown for a decision with no author, and gives an agent's whole title", async () => {
  const planner: Actor = {
    id: "session-planner",
    kind: "session",
    origin: { session_title: "Legion planner · CORE-1" },
  };
  const view = renderBody({
    children: [
      statement("s1"),
      openAsk("q-planner", true, { author: planner }),
      decisionNode("first"),
    ],
    made: [decision("first", { links: [shaped(specRef("s1"))] })],
  });

  const decided = rowOf(view, D("first"));
  await act(async () => decided.focus());
  expect(words(decided.querySelector("[data-node-body] [data-node-author]"))).toBe(
    CONTROL_LABELS.unknown_author
  );
  expect(decided.querySelector("[data-node-margin] [data-node-author]")).toBeNull();

  const asked = rowOf(view, "q-planner");
  await act(async () => asked.focus());
  expect(words(asked.querySelector("[data-node-margin] [data-node-author]"))).toBe("planner");
  expect(words(asked.querySelector("[data-node-body] [data-node-author-title]"))).toBe(
    "Legion planner · CORE-1"
  );
  // The whole title is the body's alone: the margin names the agent by its role's word.
  expect(asked.querySelector("[data-node-margin] [data-node-author-title]")).toBeNull();
});

// R5 (e), R22: a statement on a decision's "informs" line is named by its own words, cut at a
// word within 32 characters as a chip's are (`chipWords`, decision 83), and never by an address.
test("each statement on the informs line is named within 32 characters", () => {
  const view = renderBody({
    children: [
      statement("s1", { text: "Requirement 1 of the redesign stands on a row of its own." }),
      statement("s2"),
      decisionNode("first"),
    ],
    made: [decision("first", { links: [shaped(specRef("s1")), shaped(specRef("s2"))] })],
  });

  const informs = rowOf(view, D("first")).querySelector("[data-decision-informs]");
  const names = [...(informs?.querySelectorAll("a") ?? [])].map((link) => link.textContent ?? "");
  expect(names.length).toBe(2);
  const name = names[0] ?? "";
  expect(name.length).toBeLessThanOrEqual(32);
  expect(name.endsWith("…")).toBe(true);
  expect(name.startsWith("Requirement 1 of the redesign")).toBe(true);
  expect(name).not.toContain("dispatch://");
  expect(name).not.toContain("#b-");
  // The other statement is named by its own words too, "The page draws s2. It draws nothing
  // else.", cut the same way.
  expect(names[1]).toBe("The page draws s2. It draws…");
  expect(
    [...(informs?.querySelectorAll("[data-decision-statement]") ?? [])].map((shape) =>
      shape.getAttribute("data-decision-statement")
    )
  ).toEqual([statement("s1").id, statement("s2").id]);
  view.unmount();

  // A statement is named by its first block's words, as the outline's chip for it is: a short
  // first paragraph is the whole of its name, and the paragraph after it no part of it.
  const short = renderBody({
    children: [
      statement("s1", { text: "Rows stand alone.\n\nEach row draws one node of the tree." }),
      decisionNode("first"),
    ],
    made: [decision("first", { links: [shaped(specRef("s1"))] })],
  });
  expect(words(rowOf(short, D("first")).querySelector("[data-decision-statement] a"))).toBe(
    "Rows stand alone."
  );
});

// R9: the list is one tab stop and the outline's own keys work it: `j` and `k` move through
// the parts in the order they are drawn, Shift+K presses Keep, Enter goes to the entry's node
// in the conversation, and the key that cycles how much is drawn unfolds the foot.
test("the outline's keys move through the parts and work a row's controls", async () => {
  const user = userEvent.setup();
  const kept = spyOn(api, "keepDecision").mockReturnValue(Promise.withResolvers<never>().promise);
  const view = renderBody(wholeIssue());
  try {
    const stops = () => [...view.container.querySelectorAll('[tabindex="0"]')];
    expect(stops().map((stop) => stop.getAttribute("data-node-id"))).toEqual(["q-open"]);

    await act(async () => rowOf(view, "q-open").focus());
    await user.keyboard("j");
    expect(focusedRow(view)).toBe(D("second"));
    await user.keyboard("j");
    expect(focusedRow(view)).toBe(D("first"));
    await user.keyboard("j");
    expect(focusedRow(view)).toBe(D("kept"));
    await user.keyboard("k");
    expect(focusedRow(view)).toBe(D("first"));
    // The tab stop rests where the reader stands.
    expect(stops().map((stop) => stop.getAttribute("data-node-id"))).toEqual([D("first")]);

    await user.keyboard("{Shift>}K{/Shift}");
    await waitFor(() => expect(kept).toHaveBeenCalled());
    expect(kept.mock.calls[0]?.[1]).toBe("first");

    // The key that cycles how much is drawn unfolds the foot, and folds it again.
    await user.keyboard("z");
    expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
    await user.keyboard("{End}");
    expect(focusedRow(view)).toBe(D("replaced"));
    await user.keyboard("{Home}");
    expect(focusedRow(view)).toBe("q-open");
    await user.keyboard("z");
    expect(rowsOfPart(view, "agents")).toEqual([]);

    // Enter on a row goes to its node in the conversation.
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.getByTestId("where").textContent).toBe(`/issues/${KEY}/asks/q-open`)
    );

    // A fold with the focus on one of the foot's rows puts it on the foot's control, which
    // stays, rather than dropping it to the page with the row.
    await user.keyboard("z");
    await act(async () => rowOf(view, D("minor")).focus());
    await user.keyboard("z");
    expect(rowsOfPart(view, "agents")).toEqual([]);
    expect(document.activeElement).toBe(
      view.container.querySelector('[data-decisions-part="agents"] button')
    );

    // Let close on a row pinned open, pressed while another row has the focus, leaves the focus
    // where it is: the press moves none, so the body it closes takes none to the page.
    await act(async () => rowOf(view, D("kept")).focus());
    await user.keyboard("o");
    await act(async () => rowOf(view, "q-open").focus());
    expect(rowOf(view, D("kept")).querySelector("[data-node-body][data-pinned]")).not.toBeNull();
    await user.click(view.getByRole("button", { name: CONTROL_LABELS.let_close }));
    expect(document.activeElement).toBe(rowOf(view, "q-open"));
    expect(rowOf(view, "q-open").querySelector("[data-node-body]")).not.toBeNull();
    expect(rowOf(view, D("kept")).querySelector("[data-node-body]")).toBeNull();
  } finally {
    kept.mockRestore();
  }
});

// R14, R19: the Contents pane's list, which asks the outline to go to a node, and an address
// that names an entry, each put the focus on that entry's row, and unfold the foot where the
// entry is one of its own.
test("the page's ask and the address put the focus on an entry, unfolding the foot for it", async () => {
  const view = renderBody(wholeIssue());
  expect(focusedRow(view)).toBeUndefined();

  view.again({ request: { focusBranch: false, id: D("first"), seq: 1 } });
  await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
  // The same ask is acted on once: the reader's own move is not undone by it.
  await act(async () => rowOf(view, "q-open").focus());
  view.again({ request: { focusBranch: false, id: D("first"), seq: 1 } });
  expect(focusedRow(view)).toBe("q-open");

  expect(rowsOfPart(view, "agents")).toEqual([]);
  view.again({ reveal: D("replaced") });
  await waitFor(() => expect(focusedRow(view)).toBe(D("replaced")));
  expect(rowsOfPart(view, "agents")).toEqual([D("minor"), D("replaced")]);
});

// R1, R6: an address that names a node that is neither a question nor a decision, a comment
// here, outranks the view: the comment is drawn first, under no heading and in no part, as the
// outline draws a row for its context, and the focus goes to it. It is no entry of the list,
// so an issue that holds no question and no decision still says so.
test("an address that names a comment draws its row first and focuses it", async () => {
  const said = node({
    at: "2026-11-12T09:00:00Z",
    author: READER_ACTOR,
    id: "c-said",
    reference: `dispatch://${KEY}/comment/c-said`,
    text: "The rail stays where it is.",
    type: "comment",
  });
  const children = [statement("s1"), said];
  const view = renderBody(
    { children },
    {
      reveal: said.id,
      view: presetView(issueTree(children), "decisions", "", new Set([said.id])).root,
    }
  );

  const first = view
    .getByRole("tree", { name: CONTROL_LABELS.tree })
    .querySelector<HTMLElement>("[data-node-row]");
  expect(first?.getAttribute("data-node-id")).toBe(said.id);
  expect(first?.closest("[data-decisions-part]") ?? null).toBeNull();
  expect(first?.getAttribute("aria-level")).toBe("1");
  await waitFor(() => expect(focusedRow(view)).toBe(said.id));

  const row = rowOf(view, said.id);
  // A row drawn for its context, as the outline marks one: no tag, since a comment is no typed
  // kind, its author out of the reader's ink, its words, and no state.
  expect(row.getAttribute("data-node-context")).toBe("");
  expect(row.querySelector("[data-node-tag]")).toBeNull();
  const author = row.querySelector("[data-node-margin] [data-node-author] [data-author-text]");
  expect(words(author)).toBe("alice");
  // Out of the reader's ink: no class of the page's ink is on it.
  expect(pageInkText.split(" ").filter((name) => author?.classList.contains(name))).toEqual([]);
  expect(words(row.querySelector("[data-node-words]"))).toBe("The rail stays where it is.");
  expect(row.hasAttribute("data-node-state")).toBe(false);
  expect(row.querySelector("[data-node-margin] [data-node-state]")).toBeNull();
  const link = row.querySelector("a[data-entry-link]");
  expect(words(link)).toBe(IN_CONVERSATION);
  expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/comments/${said.id}`);
  expect(row.querySelectorAll("button[data-row-action]").length).toBe(0);
  // No heading and no count stand over it, and the issue says it holds no decision.
  expect(view.container.querySelectorAll("[data-decisions-heading]").length).toBe(0);
  expect(view.getByText(EMPTY_LINE)).toBeTruthy();
});

// R6: the page holds which of the Contents list's requests an outline or a view took
// (`outline-request.tsx`), so a view drawn again, turned off and on, does not take the reader
// back to the request it took before.
test("a body drawn again does not act on a request the page already took", async () => {
  const request: OutlineRequest = { focusBranch: false, id: D("first"), seq: 1 };
  const view = renderBody(wholeIssue(), { request }, { requests: true });
  await waitFor(() => expect(focusedRow(view)).toBe(D("first")));

  const elsewhere = view.getByRole("button", { name: "Elsewhere" });
  await act(async () => elsewhere.focus());
  view.again({ drawn: false, request });
  expect(view.container.querySelector("[data-decisions-body]")).toBeNull();

  view.again({ request });
  expect(rowOf(view, D("first"))).toBeTruthy();
  await act(async () => {});
  expect(document.activeElement).toBe(elsewhere);
});

// R4, R6: a list that holds only the foot draws no row, so it has no tab stop to take the
// focus. A page opened on it with nothing focused gives the focus to the foot's control, and
// so does a focus on the tree, which is where `t d` puts it when the tree holds no tab stop
// (`useIssueKeymap.ts`): the tree element keeps no focus while the list draws a row or the
// foot.
test("a list that holds only the foot gives the focus to the foot's control", async () => {
  const user = userEvent.setup();
  const control = (drawn: RenderResult) =>
    drawn.container.querySelector('[data-decisions-part="agents"] button');
  const footOnly = { children: [decisionNode("minor")], made: [decision("minor")] };
  expect(document.activeElement).toBe(document.body);
  const view = renderBody(footOnly);
  await waitFor(() => expect(document.activeElement).toBe(control(view)));

  (document.activeElement as HTMLElement).blur();
  expect(document.activeElement).not.toBe(control(view));
  await act(async () => view.getByRole("tree", { name: CONTROL_LABELS.tree }).focus());
  expect(document.activeElement).toBe(control(view));
  view.unmount();

  // The tree is built read by read (`useIssueTree.ts`): a list that holds only the foot until
  // the asks read answers gives the focus to the foot's control, and once the question's row
  // is drawn the focus goes to it, where the stage would have put it.
  const early = renderBody(footOnly);
  await waitFor(() => expect(document.activeElement).toBe(control(early)));
  early.again({ children: [openAsk("q-open"), decisionNode("minor")] });
  await waitFor(() => expect(focusedRow(early)).toBe("q-open"));
  early.unmount();

  // A reader who presses the control keeps the focus on it, though the foot's rows it unfolds
  // are drawn.
  const pressed = renderBody(footOnly);
  await waitFor(() => expect(document.activeElement).toBe(control(pressed)));
  await user.click(control(pressed) as HTMLElement);
  expect(rowsOfPart(pressed, "agents")).toEqual([D("minor")]);
  expect(document.activeElement).toBe(control(pressed));
});

// R13, the tester's F12: the header's count of what waits on the reader turns the filter on
// and puts the focus on a thing that waits, in the filter's order: each open question whose
// turn is the reader's, then each decision of the review list. Each press goes to the next
// of them and round to the first again. A press that lands before the reads are in is taken
// once the list holds something that waits.
test("each press of the count of what waits goes to the next thing that waits", async () => {
  const whole = wholeIssue();
  const view = renderBody(whole, { waitingPress: 1 });
  await waitFor(() => expect(focusedRow(view)).toBe("q-open"));
  // The same press is taken once: the reader's own move is not undone by it.
  await act(async () => rowOf(view, D("kept")).focus());
  view.again({ waitingPress: 1 });
  expect(focusedRow(view)).toBe(D("kept"));
  // The next press goes to the next thing that waits, wherever the reader has gone since.
  view.again({ waitingPress: 2 });
  await waitFor(() => expect(focusedRow(view)).toBe(D("second")));
  view.again({ waitingPress: 3 });
  await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
  // And round to the first again.
  view.again({ waitingPress: 4 });
  await waitFor(() => expect(focusedRow(view)).toBe("q-open"));
  view.unmount();

  // With no question waiting on the reader, the newest decision of the review list.
  const noQuestion = renderBody(
    { ...whole, children: whole.children.filter((child) => child.id !== "q-open") },
    { waitingPress: 1 }
  );
  await waitFor(() => expect(focusedRow(noQuestion)).toBe(D("second")));
  noQuestion.unmount();

  // A press that lands before the decisions view holds the decision is taken once it does.
  const early = renderBody({ children: [statement("s1"), decisionNode("first")] });
  early.again({ waitingPress: 2 });
  expect(focusedRow(early)).toBeUndefined();
  await act(async () =>
    early.client.setQueryData(
      viewKey(KEY, "decisions"),
      treeAnswer([decision("first", { links: [shaped(specRef("s1"))] })])
    )
  );
  await waitFor(() => expect(focusedRow(early)).toBe(D("first")));
});

// A Change sent on a decision is a comment anchored to its words. Once the tree read hangs it
// under the decision - the send reads the tree again itself (`refreshQueries` of the tree's key,
// then `dropViews`) and then invalidates the comments read - the decision says a change was asked
// for, leaves the review list for the reader's own decisions, draws no Keep or Change, and draws
// what the reader asked under its words with a link to it in the conversation.
test("a decision the reader sent a Change on says so, leaves the review list and draws the change", async () => {
  const user = userEvent.setup();
  const whole = wholeIssue();
  const view = renderBody(whole);
  const create = spyOn(api, "createComment").mockImplementation(async () => {
    view.serve({
      changes: [
        changeComment("change-second", "second", "2026-11-16T09:00:00Z", "Name the read, please."),
      ],
    });
    return { id: "change-second" } as never;
  });
  const list = spyOn(api, "listComments").mockResolvedValue([]);
  try {
    expect(rowsOfPart(view, "review")).toEqual([D("second"), D("first")]);
    const change = rowOf(view, D("second")).querySelector('button[data-row-action="change"]');
    if (change === null) throw new Error("the review row draws no Change");
    await user.click(change);
    await user.type(view.getByLabelText(CONTROL_LABELS.your_change), "Name the read, please.");
    await user.click(view.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    // The comment is anchored to the decision's own block and words in the spec, the document
    // the decision is a block of.
    // Compared as `unknown`: the API's `AnchorInput` does not declare the `block_id` it carries.
    expect<unknown>(create.mock.calls[0]?.slice(0, 2)).toEqual([
      KEY,
      {
        anchor: { artifact: SPEC_DOCUMENT.id, block_id: "second", quote: "The decision second." },
        body: "Name the read, please.",
      },
    ]);

    // The comments and the tree are read again, and the decision is no longer the reader's to
    // review.
    await waitFor(() => expect(rowsOfPart(view, "review")).toEqual([D("first")]));
    // The reader sent it, so the focus goes on to the next decision of the review list.
    await waitFor(() => expect(focusedRow(view)).toBe(D("first")));
    expect(list).toHaveBeenCalled();
    expect(rowsOfPart(view, "yours")[0]).toBe(D("second"));
    const row = rowOf(view, D("second"));
    expect(row.getAttribute("data-node-state")).toBe("change_requested");
    expect(words(row.querySelector("[data-node-margin] [data-node-state]"))).toBe(
      NODE_STATE_WORDS.decision.change_requested
    );
    expect(row.querySelectorAll("button[data-row-action]").length).toBe(0);
    expect(row.querySelector("[data-change-words]")?.textContent).toBe("Name the read, please.");
    const link = row.querySelector("a[data-change-link]");
    expect(words(link)).toBe(DECISIONS_WORDS.change);
    expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/comments/change-second`);
    expect(words(row.querySelector("[data-change-by]"))).toBe("alice");
    // The count of the review list's heading moved with it.
    expect(
      words(view.container.querySelector('[data-decisions-part="review"] [data-decisions-count]'))
    ).toBe("1");
  } finally {
    create.mockRestore();
    list.mockRestore();
  }
});

// R2: the view draws four parts. What another person answered is no part of its own: it stands
// under Your decisions with the reader's own, newest first, and its answer names that person.
test("another person's answer stands under Your decisions with their name", () => {
  const view = renderBody({
    children: [
      answeredAsk("a-mine", { at: "2026-11-14T09:00:00Z", text: "Mine." }),
      answeredAsk("a-bobs", {
        at: "2026-11-13T09:00:00Z",
        by: { id: "bob", kind: "user" },
        text: "Bob's.",
      }),
    ],
  });
  expect(rowsOfPart(view, "yours")).toEqual(["a-mine", "a-bobs"]);
  expect(words(rowOf(view, "a-bobs").querySelector("[data-answer-author]"))).toBe("bob");
  expect(view.container.querySelector('[data-decisions-part="others"]')).toBeNull();
});

// The tester's F16: a key with nowhere to go says so at the row it was pressed on, and the
// report goes when the reader moves on to another row or out of the list (decision 105 as
// amended, `useReleaseFocus`). The focus leaving the list with no press under way, as Tab or a
// script moves it, takes the report away at once. A press on another control of the page keeps
// it drawn while the press is held, so nothing moves under the pointer, and takes it away once
// the press has ended and its click has landed. Pressed on no row, on the foot's control say,
// it says so at the list's tab stop, as the outline's keys do.
test("a key with nowhere to go reports at the row it was pressed on", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue(), {}, { requests: true });
  await act(async () => rowOf(view, "q-open").focus());
  await user.keyboard("w");
  expect(words(rowOf(view, "q-open").querySelector("[data-row-report]"))).toBe(
    KEY_REPORTS.no_decision
  );
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  await user.keyboard("j");
  expect(view.container.querySelector("[data-row-report]")).toBeNull();

  // A script's focus on another control takes the report away at once.
  await act(async () => rowOf(view, "q-open").focus());
  await user.keyboard("w");
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  const elsewhere = view.getByRole("button", { name: "Elsewhere" });
  await act(async () => elsewhere.focus());
  expect(view.container.querySelector("[data-row-report]")).toBeNull();

  // A press on it takes the focus as it goes down and keeps the report while it is held, and
  // the report goes once the press is let go.
  await act(async () => rowOf(view, "q-open").focus());
  await user.keyboard("w");
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  await user.pointer({ keys: "[MouseLeft>]", target: elsewhere });
  expect(document.activeElement).toBe(elsewhere);
  expect(view.container.querySelectorAll("[data-row-report]").length).toBe(1);
  await user.pointer({ keys: "[/MouseLeft]", target: elsewhere });
  await waitFor(() => expect(view.container.querySelector("[data-row-report]")).toBeNull());
  view.unmount();

  // An issue with no open question, and `q` pressed on the foot's control.
  const whole = wholeIssue();
  const noQuestion = renderBody({
    ...whole,
    children: whole.children.filter((child) => child.id !== "q-open"),
  });
  const control = noQuestion.container.querySelector<HTMLElement>(
    '[data-decisions-part="agents"] button'
  );
  await act(async () => control?.focus());
  await user.keyboard("q");
  expect(document.activeElement).toBe(control);
  expect(words(rowOf(noQuestion, D("second")).querySelector("[data-row-report]"))).toBe(
    KEY_REPORTS.no_next_question
  );
  expect(noQuestion.container.querySelectorAll("[data-row-report]").length).toBe(1);
});

// R20: the filter by words narrows the list to the nodes the filter's own view kept, and the
// keys that look past the drawn rows walk that view: `q` goes to the open question it kept, not
// to one it took out, which draws no row.
test("the body draws the entries the filter's view left", async () => {
  const whole = wholeIssue();
  const view = renderBody(whole, { view: issueTree([openAsk("q-open"), decisionNode("first")]) });
  expect(
    [...view.container.querySelectorAll("[data-node-row]")].map((row) =>
      row.getAttribute("data-node-id")
    )
  ).toEqual(["q-open", D("first")]);
  view.unmount();

  const user = userEvent.setup();
  const narrowed = renderBody(
    { ...whole, children: [...whole.children, openAsk("q-later")] },
    { view: issueTree([openAsk("q-later"), decisionNode("first")]) }
  );
  await act(async () => rowOf(narrowed, D("first")).focus());
  await user.keyboard("q");
  expect(focusedRow(narrowed)).toBe("q-later");
});

// R2: the filter is drawn at every width. Nothing the body draws is taken off the page at a
// width: no element of it carries a class that hides it, at any breakpoint. What each width
// lays out is the browser's to say (c-decisions at 1440 px and at 390 px).
test("nothing the body draws is hidden at a width", async () => {
  const user = userEvent.setup();
  const view = renderBody(wholeIssue());
  await user.click(view.getByRole("button", FOOT_CONTROL));

  const hiding = /(^|\s)(\S+:)?(hidden|invisible|sr-only)(\s|$)/;
  expect(
    [...view.container.querySelectorAll("[data-decisions-body], [data-decisions-body] *")]
      .filter((element) => hiding.test(element.getAttribute("class") ?? ""))
      .map((element) => element.getAttribute("class"))
  ).toEqual([]);
  expect(view.container.querySelectorAll("[data-decisions-body] [hidden]").length).toBe(0);
});
