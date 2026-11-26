import { beforeAll, expect, type Mock, mock, spyOn, test } from "bun:test";
import { Schema } from "@milkdown/kit/prose/model";
import { EditorState } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import { onlineManager, QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter, type NavigateFunction, useLocation } from "react-router-dom";

import { type HeldNotifications, holdNotifications } from "../../../__tests__/commits";
import { displayTree } from "../../../__tests__/display-trees";
import { nextFrame } from "../../../__tests__/frames";
import { installIntersectionObserver } from "../../../__tests__/intersection-observer";
import { installResizeObserver } from "../../../__tests__/scroll-box";
import { api } from "../../../api/client";
import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type {
  Actor,
  CheckBlock,
  DecisionLink,
  IssueAskRow,
  IssueCheckResults,
  IssueCoverage,
  IssueDetails,
} from "../../../api/types";
import { pageFocusedRow } from "../../../theme/classes";
import { MarginProvider } from "../../margin/margin-context";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../../margin/margin-fixture";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import {
  type PublishedReferences,
  ReferenceNamesProvider,
  usePublishReferenceNames,
} from "../../refs/reference-names";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { measureNearWords } from "../clamp-fixtures";
import {
  artifact,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
  SPEC_SLUG,
} from "../decisions-filter/decisions-fixtures";
import { conversationPath } from "../decisions-filter/decisions-parts";
import { DocumentStage, DocumentStages } from "../document-stage";
import { FocusedNodeProvider, useFocusedNode } from "../focused-node";
import type { BriefCounts } from "../IssueThreeThings";
import type { OutlineRequest } from "../IssueTree";
import { presetPath } from "../issue-address";
import { buildIssueTree, type TreeAsk, type TreeNode } from "../issue-tree";
import * as roomTarget from "../model-rows/room-target";
import { type CaretAt, caretInBlock } from "../node-caret";
import { shortDay } from "../node-time";
import { TWO_THREADS } from "../outline-fixtures";
import { NO_FAILED_READS } from "../outline-reads";
import { OutlineRequestProvider } from "../outline-request";
import { AUTHOR_TEXT_ATTRIBUTE, KEY_REPORTS } from "../page-structure";
import {
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_WORDS,
  NODE_TYPE_NAMES,
  SPEC_WORDS,
  STATE_LINES,
  VERIFICATION_WORDS,
} from "../page-words";
import { presetView } from "../presets";
import { publishedNames, publishedTree } from "../projection-fixture";
import { ViewStage } from "../slots/stage";
import { drawStage } from "../slots/stage-fixtures";
import { nodeRowSelectorOf } from "../tree-focus";
import { decisionBehind } from "../tree-keys";
import {
  commentNode,
  treeAnswer,
  treeNode,
  treeTuple,
  wholeTreeOnly,
} from "../tree-read/tree-fixtures";
import {
  outlineQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  type TreeTuple,
  viewKey,
  viewQuery,
} from "../tree-read/tree-read";
import { issueCommentsQuery, issueMessagesQuery } from "../useIssueTree";
import type { OnEditing } from "../useNodeEditing";
import { check, checkNode, result } from "../verification-filter/verification-fixtures";
import { SpecFilterBody } from "./SpecFilterBody";
import {
  answeredAsk,
  answerNode,
  askRead,
  coverageRead,
  decision,
  decisionNode,
  issueDetails,
  shaped,
  specViewAnswer,
  statement,
  statementRef,
} from "./spec-fixtures";

/**
 * What the Spec filter's body draws, read as a reader reads it (R5, R17, R22, R35, the approved
 * picture `page-v3-spec-1440.png`).
 *
 * Which decision shaped which statement is read in `spec-decided-by.test.ts`; these rows are
 * about what the page says and what a reader can reach: the legend's words, the two columns'
 * headings, the words of each cell and of each statement's own line, the link to the whole of a
 * decision, and where the keys take the focus. Where the two columns stand side by side and
 * where they stack is the browser's own layout, which no DOM of a unit test lays out, so the
 * widths are read by the browser suite.
 */

/** The page's own `issue` scope, which every one of the outline's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** What the page publishes of its one batched read (`useIssueTree.ts`, `publishedNames`), which a
 *  reference in a node's drawn words is named from (`RowText`) and a chip of the tree is named
 *  by, published to the store as the page publishes it. */
function Named({
  children,
  published,
}: {
  children: ReactNode;
  published: PublishedReferences;
}): ReactNode {
  usePublishReferenceNames(published);
  return children;
}

/** The node the page's Details would draw the facts of, written where a case reads it. */
function FocusedProbe(): ReactNode {
  const focused = useFocusedNode();
  return <output data-focused-node={focused?.id ?? ""} data-focused-type={focused?.type ?? ""} />;
}

/** The Spec view read as the page holds it while the Spec view is on: read by an observer that
 *  is enabled, as `ViewReadProvider` reads it, and what it holds set on the issue's node, as
 *  `useIssueTree` sets it (`TreeNode.views`). */
function HeldView({
  children,
  root,
}: {
  children: (root: TreeNode) => ReactNode;
  root: TreeNode;
}): ReactNode {
  const held = useQuery(viewQuery(KEY, "spec")).data;
  const rooted = useMemo(
    () => (held === undefined ? root : { ...root, views: { spec: held } }),
    [held, root]
  );
  return children(rooted);
}

/** Where the page's router stands, written where a case reads it: a key that goes to a node in
 *  the conversation moves it there. */
function LocationProbe(): ReactNode {
  const { hash, pathname, search } = useLocation();
  return <output data-location={`${pathname}${search}${hash}`} />;
}

/** Where the page's router stands, as `LocationProbe` writes it. */
function locationOf(view: RenderResult): string {
  return view.container.querySelector("[data-location]")?.getAttribute("data-location") ?? "";
}

/** The reads the page's tree fills (`useIssueTree.ts`), each by an observer that fetches as the
 *  tree's own do, under the same keys and with the same requests: the body only watches them,
 *  so this stands in for the tree above it, which no view takes off the page. */
function TreeReads({ issue }: { issue: IssueDetails }): ReactNode {
  useQuery(issueCoverageQuery(issue.key));
  useQuery(issueResultsQuery(issue.key));
  useQuery(issueAskThreadsQuery(issue.key));
  useQuery(outlineQuery(issue.key, "conversation"));
  return null;
}

/** How one of the page's reads stands when the body is drawn. */
type ReadState = "answered" | "failed" | "out";

/** The two reads the body's tree comes through `getIssueTree` for, by what they ask: the tree
 *  read with no view (`blocks`), which holds the spec's and the Verification document's blocks,
 *  and the Spec view read (`specView`, `view: "spec"`), which the column's decisions come from. */
type TreeRead = "blocks" | "specView";

/** The spies on the page's reads that the body reads, so a case can answer one again. */
interface Reads {
  readonly asks: Mock<typeof api.listIssueAskThreads>;
  readonly coverage: Mock<typeof api.getIssueCoverage>;
  readonly results: Mock<typeof api.getIssueResults>;
  /** The one `getIssueTree`, which answers each read by the view it asks for. */
  readonly tree: Mock<typeof api.getIssueTree>;
  /** Makes one read through `tree` stand as `state` says from then on: answered as the case
   *  answered it, failed, or never answering. */
  readonly treeStands: (read: TreeRead, state: ReadState) => void;
  /** Puts every read back as it was. */
  readonly mockRestore: () => void;
}

/** What a read that stands as `state` gives back: `answer`, a failure, or nothing ever. */
function answerAs<T>(state: ReadState, answer: T): Promise<T> {
  if (state === "answered") return Promise.resolve(answer);
  if (state === "failed") return Promise.reject(new Error("offline"));
  return new Promise<never>(() => {});
}

/** The results read over `checks`, as the page's read answers it. */
function resultsOf(checks: readonly CheckBlock[]): IssueCheckResults {
  return { checks: [...checks], issue: KEY, verification_artifact_id: "" };
}

/** The tree read's answer with no node in it: each case draws the tree it is given, and the body
 *  reads from this read only whether the documents' blocks are in. */
const NO_NODES: TreeAnswer = treeAnswer([]);

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The body over `tree`, as the Spec view's slot draws it, under the reads the page's tree fills
 *  (`TreeReads`), with `made` as the nodes of the Spec view read's answer, `tuples` as that
 *  answer's tuples, `asks` as the issue's asks read and `checks` as its results read.
 *  `asksRead`, `specView`, `results`, `coverage` and `blocks` (the tree read) each say how that
 *  read stands - answered, failed, or still out - and each is answered unless a case says so;
 *  the Spec view read is held as the page holds it (`HeldView`). `verification`, where a case
 *  names it, gives the issue that document, and the tree read then stands as `verification`
 *  says; `names` is what the page's batched reference read named; `staleTime` is how long an
 *  answer stays fresh, which the shell sets to 30 s; `stage` draws the body inside the stage the
 *  page draws every view in (`ViewStage`), which places the focus on a view's load and moves it
 *  on as a failed read's line that held it leaves. The page's provider of the Contents list's
 *  requests stands above the body, so `request` is taken as the page takes it; `redraw(false)`
 *  takes the body off the page and `redraw(true)` draws a new one under the same provider and
 *  reads, as the view turned off and on again does, and `redraw(true, issue)` draws the body and
 *  the reads over `issue` from then on, as the page does once the issue read answers anew. */
function drawBody(
  tree: TreeNode,
  made: readonly TreeAnswerNode[] = [],
  {
    asks = [],
    asksRead = "answered",
    blocks = "answered",
    checks = [],
    context = new Set<string>(),
    counts,
    coverage = "answered",
    editor,
    named = [],
    names,
    onEditing = () => {},
    reader = READER,
    request,
    results = "answered",
    reveal,
    specView = "answered",
    stage = false,
    staleTime = Number.POSITIVE_INFINITY,
    tuples = [],
    verification,
  }: {
    asks?: readonly TreeAsk[];
    asksRead?: ReadState;
    blocks?: ReadState;
    checks?: readonly CheckBlock[];
    context?: Set<string>;
    counts?: BriefCounts;
    coverage?: ReadState;
    editor?: ReactNode;
    named?: readonly string[];
    names?: ReadonlyMap<string, string>;
    onEditing?: OnEditing;
    reader?: string;
    request?: OutlineRequest;
    results?: ReadState;
    reveal?: string;
    specView?: ReadState;
    stage?: boolean;
    staleTime?: number;
    tuples?: readonly TreeTuple[];
    verification?: ReadState;
  } = {}
): {
  client: QueryClient;
  read: Reads;
  redraw: (drawn: boolean, issue?: IssueDetails) => void;
  view: RenderResult;
} {
  const base = issueDetails();
  const issue = {
    ...base,
    ...(verification === undefined ? {} : { verification_artifact_id: "artifact-ver" }),
    artifacts: [
      ...base.artifacts,
      ...(verification === undefined ? [] : [artifact("artifact-ver", "verification-md")]),
    ],
  };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime } },
  });
  const spies = {
    asks: spyOn(api, "listIssueAskThreads"),
    coverage: spyOn(api, "getIssueCoverage"),
    results: spyOn(api, "getIssueResults"),
    tree: spyOn(api, "getIssueTree"),
  };
  /** Leaves one read as `state` says: answered by `answer`, failed, or never answering. */
  const settle = (
    state: ReadState,
    spy: {
      mockRejectedValue(error: Error): unknown;
      mockReturnValue(value: Promise<never>): unknown;
    },
    answer: () => void
  ): void => {
    if (state === "answered") answer();
    else if (state === "failed") spy.mockRejectedValue(new Error("offline"));
    else spy.mockReturnValue(new Promise<never>(() => {}));
  };
  // The tree read holds the blocks of the spec and of the Verification document alike, so where
  // a case names `verification` that read stands as it says; the Spec view read stands as
  // `specView` says. Each one answered is held under its key as the page holds it.
  const treeAnswers: Readonly<Record<TreeRead, TreeAnswer>> = {
    blocks: NO_NODES,
    specView: specViewAnswer(made, tuples),
  };
  const treeStanding: Record<TreeRead, ReadState> = { blocks: verification ?? blocks, specView };
  spies.tree.mockImplementation((_key, query) => {
    const asked =
      query?.view === undefined ? "blocks" : query.view === "spec" ? "specView" : undefined;
    if (asked === undefined) {
      return Promise.reject(new Error(`the body read the ${query?.view} view`));
    }
    return answerAs(treeStanding[asked], treeAnswers[asked]);
  });
  if (treeStanding.blocks === "answered") {
    client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeAnswers.blocks);
  }
  if (treeStanding.specView === "answered") {
    client.setQueryData(viewKey(KEY, "spec"), treeAnswers.specView);
  }
  // The issue's asks, which say which question a person answered (`decisionNodes`). The fixtures
  // carry the members of an ask the page reads (`TreeAsk`), not every member the route answers,
  // so a read of them again answers what the cache holds.
  settle(asksRead, spies.asks, () => {
    client.setQueryData(issueAskThreadsQuery(KEY).queryKey, [...asks] as never);
    spies.asks.mockImplementation(
      async () =>
        (client.getQueryData<IssueAskRow[]>(issueAskThreadsQuery(KEY).queryKey) ?? []) as never
    );
  });
  settle(results, spies.results, () => {
    client.setQueryData(issueResultsQuery(KEY).queryKey, resultsOf(checks));
    spies.results.mockResolvedValue(resultsOf(checks));
  });
  settle(coverage, spies.coverage, () => {
    client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
    spies.coverage.mockResolvedValue(coverageRead([]));
  });
  const read: Reads = {
    ...spies,
    mockRestore: () => {
      for (const spy of Object.values(spies)) spy.mockRestore();
    },
    treeStands: (asked, state) => {
      treeStanding[asked] = state;
    },
  };
  // What the page publishes, which the store holds and the body hands its rows
  // (`PublishedReferenceNames`), one value, as the page's tree makes one: the names a case adds
  // for what only a read of the target would name, and every chip the tree holds, named by its
  // reading, which stands over a case's name for the same target.
  const chipsPublished = publishedTree(tree);
  const published: PublishedReferences =
    names === undefined
      ? chipsPublished
      : {
          ...chipsPublished,
          asked: new Set([...chipsPublished.asked, ...names.keys()]),
          readings: new Map([...publishedNames(names).readings, ...chipsPublished.readings]),
        };
  // The body over `over`, under the Spec view read the page holds (`HeldView`).
  const bodyOf = (over: IssueDetails): ReactNode => (
    <HeldView root={tree}>
      {(root) => (
        <SpecFilterBody
          context={context}
          counts={counts}
          emptyLine={EMPTY_STATE_LINES.spec}
          holdsEscape={false}
          issue={over}
          namesLine={NO_FAILED_READS}
          onEditing={onEditing}
          published={published}
          reader={reader}
          // The case holds each read as it says, so an address lands as soon as its row is drawn.
          readsSettled
          {...(request === undefined ? {} : { request })}
          {...(reveal === undefined ? {} : { reveal })}
          root={root}
          view={presetView(tree, "spec", "", new Set(named)).root}
        />
      )}
    </HeldView>
  );
  let shown: IssueDetails = issue;
  let body = bodyOf(shown);
  const page = (drawn: boolean): ReactNode => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <Named published={published}>
          <MemoryRouter>
            <KeymapProvider>
              <MarginProvider>
                <OutlineRequestProvider>
                  <FocusedNodeProvider>
                    <IssueScope />
                    <FocusedProbe />
                    <LocationProbe />
                    <TreeReads issue={shown} />
                    <DocumentStages>
                      {drawn ? stage ? <ViewStage>{body}</ViewStage> : body : null}
                      {editor}
                    </DocumentStages>
                  </FocusedNodeProvider>
                </OutlineRequestProvider>
              </MarginProvider>
            </KeymapProvider>
          </MemoryRouter>
        </Named>
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
  const view = render(page(true));
  return {
    client,
    read,
    redraw: (drawn, other) => {
      if (other !== undefined) {
        shown = other;
        body = bodyOf(other);
      }
      view.rerender(page(drawn));
    },
    view,
  };
}

/** The text of every element matching `selector`, in the order they are drawn. */
function textsOf(view: RenderResult, selector: string): string[] {
  return [...view.container.querySelectorAll(selector)].map((element) =>
    (element.textContent ?? "").trim()
  );
}

function rowOf(view: RenderResult, id: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(nodeRowSelectorOf(id));
  if (row === null) throw new Error(`the body draws no row for ${id}`);
  return row;
}

/** The node Details would draw the facts of, by its id and its kind. */
function focusedNodeIn(view: RenderResult): { id: string; type: string } {
  const probe = view.container.querySelector("[data-focused-node]");
  return {
    id: probe?.getAttribute("data-focused-node") ?? "",
    type: probe?.getAttribute("data-focused-type") ?? "",
  };
}

/** The failed-read line whose alert says `line`: the alert names the failure, and the line beside
 *  it holds its Retry. */
function alertSaying(view: RenderResult, line: string): HTMLElement {
  const alert = [...view.container.querySelectorAll<HTMLElement>('[role="alert"]')].find(
    (one) => one.textContent?.includes(line) === true
  );
  const drawn = alert?.closest<HTMLElement>("[data-failed-read]");
  if (drawn === undefined || drawn === null) {
    throw new Error(`the body draws no error line saying "${line}"`);
  }
  return drawn;
}

// R17: the legend names each mark in the words the Verification filter says how a statement
// stands, so one statement reads the same in both views, and none of them reads proven or
// covered for a statement whose checks passed.
test("the legend names how a statement stands, in the Verification filter's own words", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(
      [...view.container.querySelectorAll("[data-spec-legend-mark]")].map((mark) => [
        mark.getAttribute("data-spec-legend-mark"),
        mark.textContent,
      ])
    ).toEqual([
      ["met", VERIFICATION_WORDS.states.met],
      ["not_met", VERIFICATION_WORDS.states.not_met],
      ["unknown", VERIFICATION_WORDS.states.unknown],
      ["no_check", SPEC_WORDS.no_check],
    ]);
    const legend = view.container.querySelector("[data-spec-legend]")?.textContent ?? "";
    expect(legend).not.toContain("proven");
    expect(legend).not.toContain("covered");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R22: the page draws Statement where it drew Requirement.
test("the two columns are headed Decided by and Statement, never Requirement", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(textsOf(view, "[data-spec-head] span")).toEqual(["Decided by", "Statement"]);
    expect(view.container.textContent).not.toContain("Requirement");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: the decision stands once beside the statements it shaped, headed by the words it came
// from, with who decided it, when, what they chose, and a link to the whole answer.
test("a person's decision stands once beside its statements, in their own words", () => {
  const ask = answeredAsk("ask-1", {
    selected: ["Changes needed"],
    text: "The node types are all, like, completely so you have to click. And I can't write.",
  });
  const { read, view } = drawBody(
    issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision("d1", {
        decision: {
          asks: ["ask-1"],
          kind: "person",
          links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
        },
      }),
    ]
  );
  try {
    const cells = [...view.container.querySelectorAll("[data-spec-cell]")];
    expect(cells).toHaveLength(1);
    expect(cells[0]?.getAttribute("data-said")).toBe("you");
    expect(cells[0]?.getAttribute("data-spec-statements")).toBe("spec#b-r1 spec#b-r2");
    expect(textsOf(view, "[data-spec-said]")).toEqual([SPEC_WORDS.said_you]);
    expect(textsOf(view, "[data-spec-quote]")).toEqual([
      "“The node types are all, like, completely so you have to click.”",
    ]);
    const source = view.container.querySelector("[data-spec-source]")?.textContent ?? "";
    expect(source).toContain("Does the built page work for you?");
    expect(source).toContain(READER);
    expect(source).toContain("14 Nov");
    expect(source).toContain(`${SPEC_WORDS.chose} Changes needed`);
    const whole = view.container.querySelector("[data-spec-whole]");
    expect(whole?.textContent?.trim()).toBe(`${SPEC_WORDS.whole_answer} ›`);
    expect(whole?.getAttribute("href")).toBe(`/issues/${KEY}/asks/ask-1`);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5, the tester's F7: one line on each statement's own row, with no focus and no hover: after
// "because:", the question the person answered by its first sentence, then the words the
// decision came from, as a link to that decision, who decided and when.
test("each statement carries one line naming the decision that shaped it, with a link to it", () => {
  const ask = answeredAsk("ask-1", { text: "Put the decisions beside the statements. Please." });
  const { read, view } = drawBody(
    issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision("d1", {
        decision: {
          asks: ["ask-1"],
          kind: "person",
          links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
        },
      }),
    ]
  );
  try {
    const lines = textsOf(view, "[data-spec-because]");
    expect(lines).toHaveLength(2);
    for (const line of lines) {
      expect(line).toContain(
        `${SPEC_WORDS.because} Does the built page work for you? “Put the decisions beside the statements.”`
      );
      expect(line).toContain(READER);
      expect(line).toContain("14 Nov");
    }
    const links = [...view.container.querySelectorAll("[data-spec-because-link]")];
    expect(links.map((link) => link.getAttribute("href"))).toEqual([
      `/issues/${KEY}/asks/ask-1`,
      `/issues/${KEY}/asks/ask-1`,
    ]);
    // Out of the tab order, as every control inside a row is (R9).
    expect(links.map((link) => link.getAttribute("tabindex"))).toEqual(["-1", "-1"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A person's decision is the question they answered, which
// carries `shaped` ties in the Spec view's context, and no decision node restates it. It stands
// beside each statement a tie names, in their own words, and a statement no tie names stands
// with none.
test("a person's decision stands beside each statement its shaped tie names", () => {
  const words = "Put the decisions beside the statements. Please.";
  const { read, view } = drawBody(
    issueTree([
      statement("r1"),
      statement("r2"),
      statement("r3"),
      answeredAsk("ask-1", { text: words }),
    ]),
    [
      // The view's query takes the spec's statements; the question is there as context.
      ...["r1", "r2", "r3"].map((block, position) =>
        answerNode(block, "requirement", {
          document: { artifact_id: "artifact-spec", position, slug: SPEC_SLUG },
          role: "query",
        })
      ),
      answerNode("ask-1", "ask", { role: "context" }),
    ],
    {
      asks: [askRead("ask-1", words)],
      tuples: [treeTuple("shaped", "ask-1", "r1"), treeTuple("shaped", "ask-1", "r3")],
    }
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-spec-cell]")].map((cell) => [
        cell.getAttribute("data-spec-statements"),
        cell.getAttribute("data-decision-id"),
        cell.getAttribute("data-said"),
      ])
    ).toEqual([
      ["spec#b-r1", "ask-1", "you"],
      ["spec#b-r2", null, null],
      ["spec#b-r3", "ask-1", "you"],
    ]);
    expect(textsOf(view, "[data-spec-quote]")).toEqual([
      "“Put the decisions beside the statements.”",
      "“Put the decisions beside the statements.”",
    ]);
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([EMPTY_STATE_LINES.spec_no_decision_one]);
    for (const block of ["r1", "r3"]) {
      const line = rowOf(view, `spec#b-${block}`).querySelector("[data-spec-because]");
      expect(line?.textContent).toContain("“Put the decisions beside the statements.”");
      expect(line?.textContent).toContain(READER);
      expect(line?.querySelector("[data-spec-because-link]")?.getAttribute("href")).toBe(
        `/issues/${KEY}/asks/ask-1`
      );
    }
    expect(rowOf(view, "spec#b-r2").querySelector("[data-spec-because]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// The words that say no decision shaped a statement wait for the asks read too: a person's
// decision is a question they answered, which only that read says, so while it is out the column
// says nothing of a statement such a decision shaped - nor of any other - and once it answers the
// decision stands beside each.
test("while the asks read is out, no statement a person's decision shaped reads that none did", async () => {
  const words = "Put the decisions beside the statements. Please.";
  const { client, read, view } = drawBody(
    issueTree([
      statement("r1"),
      statement("r2"),
      statement("r3"),
      answeredAsk("ask-1", { text: words }),
    ]),
    [
      ...["r1", "r2", "r3"].map((block, position) =>
        answerNode(block, "requirement", {
          document: { artifact_id: "artifact-spec", position, slug: SPEC_SLUG },
          role: "query",
        })
      ),
      answerNode("ask-1", "ask", { role: "context" }),
    ],
    {
      asksRead: "out",
      tuples: [treeTuple("shaped", "ask-1", "r1"), treeTuple("shaped", "ask-1", "r3")],
    }
  );
  try {
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([]);
    expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(3);

    act(() => {
      client.setQueryData(issueAskThreadsQuery(KEY).queryKey, [askRead("ask-1", words)] as never);
    });
    await waitFor(() =>
      expect(
        [...view.container.querySelectorAll("[data-spec-cell]")].map((cell) =>
          cell.getAttribute("data-decision-id")
        )
      ).toEqual(["ask-1", null, "ask-1"])
    );
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([EMPTY_STATE_LINES.spec_no_decision_one]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R2 and decision 98: a person's decision is a question they answered, which only the asks read
// says, so where that read failed with nothing held the column cannot draw a person's decision
// beside the statements it shaped. It says the open questions could not be read, on a line of its
// own beside the Spec view read's, and draws no person's decision missing in silence, nor a line
// that no decision shaped a statement.
test("where the asks read failed, the column says the open questions could not be read", async () => {
  const words = "Put the decisions beside the statements. Please.";
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), answeredAsk("ask-1", { text: words })]),
    [
      ...["r1", "r2"].map((block, position) =>
        answerNode(block, "requirement", {
          document: { artifact_id: "artifact-spec", position, slug: SPEC_SLUG },
          role: "query",
        })
      ),
      answerNode("ask-1", "ask", { role: "context" }),
    ],
    { asksRead: "failed", tuples: [treeTuple("shaped", "ask-1", "r1")] }
  );
  try {
    await waitFor(
      () =>
        expect(
          view.container.textContent,
          "the column does not say its open questions could not be read where the asks read failed"
        ).toContain(STATE_LINES.asks_unreadable),
      { timeout: 3000 }
    );
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R35: a statement with no decision beside it is drawn as such, which is what makes a missing
// tie visible.
test("a statement no decision shaped says so where its decision would stand", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([EMPTY_STATE_LINES.spec_no_decision_one]);
    expect(textsOf(view, "[data-spec-because]")).toEqual([]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// Two statements no decision shaped stand under one cell, whose words agree with their count.
test("several statements no decision shaped say so in words that agree with their count", () => {
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([EMPTY_STATE_LINES.spec_no_decision_many]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// The words that say no decision shaped a statement are true only once the Spec view read the
// decisions come from has answered: while it is out the column says nothing of a statement, and a
// read that failed says so in the page's own line.
test("the column says no decision shaped a statement only once the decisions are read", async () => {
  const out = drawBody(issueTree([statement("r1")]), [], { specView: "out" });
  try {
    expect(textsOf(out.view, "[data-spec-nothing]")).toEqual([]);
    expect(out.view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
  } finally {
    out.view.unmount();
    out.read.mockRestore();
  }
  const failed = drawBody(issueTree([statement("r1")]), [], { specView: "failed" });
  try {
    await waitFor(() =>
      expect(failed.view.container.textContent).toContain(STATE_LINES.statements_unreadable)
    );
    expect(textsOf(failed.view, "[data-spec-nothing]")).toEqual([]);
  } finally {
    failed.view.unmount();
    failed.read.mockRestore();
  }
});

// Decision 450: a refused Spec view read is one failure, which the view says once, in the
// statements' line with that line's one Retry. The column's decisions come from the same read, so
// a line of their own would say the one failure twice.
test("a failed Spec view read draws one message, the statements', and no decision line's", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([statement("r1")]), [], { specView: "failed" });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    expect(
      textsOf(view, '[data-failed-read] [role="alert"]'),
      "the one refused read was not said once, as the statements'"
    ).toEqual([STATE_LINES.statements_unreadable]);
    expect(view.container.textContent).not.toContain(STATE_LINES.issue_decisions_unreadable);
    expect(
      view.container.querySelectorAll("[data-failed-read] button"),
      "the statements' line holds other than its one Retry"
    ).toHaveLength(1);
    read.treeStands("specView", "answered");
    const before = askedOf(read);
    await user.click(within(line).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(
        view.container.querySelector('[role="alert"]'),
        "the statements' line still stands after its Retry answered"
      ).toBeNull()
    );
    expect(askedOf(read).specView, "the Retry did not ask the Spec view read once").toBe(
      before.specView + 1
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// The Spec view read and the asks read can each be out, answered or failed when the body is
// drawn, with the statements' reads answered, and the column draws only what the reads that
// answered say: the statements' line, and no decisions' line, where the Spec view read failed
// with nothing held; the line that its open questions could not be read where the asks read
// failed; and a statement's words that no decision shaped it, and a person's decision beside the
// statement it shaped, only where both have answered.
test("every state of the Spec view read and the asks read draws what the column knows", async () => {
  const words = "Put the decisions beside the statements. Please.";
  const states: readonly ReadState[] = ["out", "answered", "failed"];
  for (const specView of states) {
    for (const asks of states) {
      const combination = `the Spec view read ${specView} and the asks read ${asks}`;
      const { read, view } = drawBody(
        issueTree([statement("r1"), statement("r2"), answeredAsk("ask-1", { text: words })]),
        [
          ...["r1", "r2"].map((block, position) =>
            answerNode(block, "requirement", {
              document: { artifact_id: "artifact-spec", position, slug: SPEC_SLUG },
              role: "query",
            })
          ),
          answerNode("ask-1", "ask", { role: "context" }),
        ],
        {
          asks: [askRead("ask-1", words)],
          asksRead: asks,
          specView,
          tuples: [treeTuple("shaped", "ask-1", "r1")],
        }
      );
      try {
        const both = specView === "answered" && asks === "answered";
        await waitFor(
          () => {
            const text = view.container.textContent ?? "";
            expect(
              text.includes(STATE_LINES.statements_unreadable),
              `${combination}: whether the column says its statements could not be read`
            ).toBe(specView === "failed");
            expect(
              text.includes(STATE_LINES.issue_decisions_unreadable),
              `${combination}: whether the column says its decisions could not be read too`
            ).toBe(false);
            expect(
              text.includes(STATE_LINES.asks_unreadable),
              `${combination}: whether the column says its open questions could not be read`
            ).toBe(asks === "failed");
            expect(
              view.container.querySelector("[data-spec-nothing]") !== null,
              `${combination}: whether a statement says no decision shaped it`
            ).toBe(both);
            expect(
              [...view.container.querySelectorAll("[data-spec-cell]")]
                .filter((cell) => cell.getAttribute("data-decision-id") === "ask-1")
                .map((cell) => cell.getAttribute("data-spec-statements")),
              `${combination}: the statements the person's decision stands beside`
            ).toEqual(both ? ["spec#b-r1"] : []);
          },
          { timeout: 3000 }
        );
      } finally {
        view.unmount();
        read.mockRestore();
      }
    }
  }
});

// R5: an agent's decision draws its own words whole in its cell, as a row draws a node's words
// (`RowText`, in a block, since a text drawn with its own links stands in no link), while the
// line under the statement quotes their first line, and the one word the model holds for how
// sure it was; R2: wherever a decision is drawn, one waiting for review says so.
test("an agent's decision draws its own words, how sure it was and how it stands", () => {
  const text = "One batched read names every address. The page asks the server once.";
  const made = decisionNode("d1", { state: "waiting_review", text, unsure: true, waiting: true });
  const { read, view } = drawBody(issueTree([statement("r1"), made]), [
    decision("d1", {
      text,
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
        unsure: true,
        waiting_review: true,
      },
    }),
  ]);
  try {
    const cell = view.container.querySelector("[data-spec-cell]");
    expect(cell?.getAttribute("data-said")).toBe("agent");
    expect(cell?.className).toContain("border-dashed");
    expect(textsOf(view, "[data-spec-said]")).toEqual([SPEC_WORDS.said_agent]);
    const quote = cell?.querySelector("[data-spec-quote]");
    expect(quote?.tagName).toBe("DIV");
    expect(quote?.querySelector("[data-node-words]")?.textContent).toBe(text);
    expect(textsOf(view, "[data-spec-because-link]")).toEqual([
      "“One batched read names every address.”",
    ]);
    const source = view.container.querySelector("[data-spec-source]")?.textContent ?? "";
    expect(source).toContain("Waiting for review");
    expect(source).toContain("Unsure");
    expect(view.container.querySelector("[data-spec-whole]")?.textContent?.trim()).toBe(
      `${SPEC_WORDS.whole_decision} ›`
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R2, R3 (decision 435): wherever a decision is drawn, the line beside a statement among them, one
// a person kept reads "Kept by" with the keeper's name, and one a person asked to change reads
// "Changed by" with that person's name, where it read "Change requested"; each name is drawn as
// an author's text, and the state keeps its own token.
test("a statement's decision line reads Kept by and Changed by with the person's name", () => {
  const kept = decisionNode("d1", {
    keptBy: { id: "alice", kind: "user" },
    state: "kept",
    text: "Keep the rail narrow.",
  });
  const changed = decisionNode("d2", {
    changedBy: { id: "bob", kind: "user" },
    state: "change_requested",
    text: "Draw it once.",
  });
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2"), kept, changed]), [
    decision("d1", {
      text: "Keep the rail narrow.",
      decision: {
        kept: {
          artifact_id: "artifact-dec",
          block_id: "d1",
          changed_version: 1,
          kept_at: "2026-11-15T09:00:00Z",
          kept_by: "alice",
        },
        kind: "agent",
        links: [shaped(statementRef("r1"))],
        waiting_review: false,
      },
    }),
    decision("d2", {
      text: "Draw it once.",
      decision: { kind: "agent", links: [shaped(statementRef("r2"))], waiting_review: true },
    }),
  ]);
  try {
    const drawn = view.container.querySelectorAll("[data-spec-source] [data-node-state]");
    expect([...drawn].map((state) => state.getAttribute("data-node-state"))).toEqual([
      "kept",
      "change_requested",
    ]);
    expect(textsOf(view, "[data-spec-source] [data-node-state]")).toEqual([
      "Kept by alice",
      "Changed by bob",
    ]);
    expect(textsOf(view, "[data-kept-by][data-author-text]")).toEqual(["alice"]);
    expect(textsOf(view, "[data-changed-by][data-author-text]")).toEqual(["bob"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: the decisions read holds no author for a decision, so a cell, a group's heading and a
// because line draw none - no name, no shape, and no dot where it would stand - and only the
// focused cell says "Author unknown". An agent is named by its role's word, as a row names one.
test("a decision with no author draws none, and says Author unknown with the focus", () => {
  const planner: Actor = {
    id: "session-planner",
    kind: "session",
    origin: { session_title: "Legion planner · ACME-1" },
  };
  const day = shortDay("2026-11-13T07:00:00Z");
  const { read, view } = drawBody(
    issueTree([
      node({
        at: "2026-11-13T07:00:00Z",
        children: [statement("r1")],
        id: "comment-1",
        text: "Where the statements go.",
        type: "comment",
      }),
      statement("r2"),
      statement("r3"),
      // Decisions as the tree builds them from the decisions read, which names no author
      // (`tree-drafts.ts`): the first with the time of its last change, the second with none.
      node({
        at: "2026-11-13T07:00:00Z",
        id: "decisions-md#b-d1",
        reference: `dispatch://${KEY}/artifact/decisions-md#b-d1`,
        text: "The decision d1.",
        type: "decision",
      }),
      node({
        id: "decisions-md#b-d2",
        reference: `dispatch://${KEY}/artifact/decisions-md#b-d2`,
        text: "The decision d2.",
        type: "decision",
      }),
      decisionNode("d3", { author: planner }),
    ]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
      decision("d2", {
        decision: {
          changed_at: null,
          kind: "agent",
          links: [shaped(statementRef("r2"))],
        },
      }),
      decision("d3", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r3"))],
        },
      }),
    ]
  );
  const cellOf = (block: string) =>
    view.container.querySelector<HTMLElement>(
      `[data-spec-cell][data-node-id="decisions-md#b-${block}"]`
    );
  try {
    const heading = view.container.querySelector("[data-spec-group-heading]");
    for (const drawn of [
      heading,
      cellOf("d1"),
      cellOf("d2"),
      rowOf(view, "spec#b-r1").querySelector("[data-spec-because]"),
      rowOf(view, "spec#b-r2").querySelector("[data-spec-because]"),
    ]) {
      expect(drawn).not.toBeNull();
      expect(drawn?.textContent).not.toContain(CONTROL_LABELS.unknown_author);
      expect(drawn?.querySelector('svg[data-glyph^="author-"]')).toBeNull();
    }
    expect(heading?.querySelector("p")?.textContent).toBe(`${NODE_TYPE_NAMES.comment} · ${day}`);
    expect(cellOf("d1")?.querySelector("[data-spec-source]")?.textContent).toBe(day);
    expect(textsOf(view, "[data-spec-because]")).toEqual([
      `${SPEC_WORDS.because} “The decision d1.” — ${day}`,
      `${SPEC_WORDS.because} “The decision d2.”`,
      `${SPEC_WORDS.because} “The decision d3.” — planner · ${day}`,
    ]);
    expect(cellOf("d3")?.querySelector("[data-spec-author]")?.textContent).toBe("planner");
    act(() => cellOf("d1")?.focus());
    expect(cellOf("d1")?.querySelector("[data-spec-source]")?.textContent).toBe(
      `${CONTROL_LABELS.unknown_author} · ${day}`
    );
    expect(cellOf("d1")?.querySelector('svg[data-glyph="author-unknown"]')).not.toBeNull();
    // Nothing but the focused cell says it.
    expect(view.container.textContent?.split(CONTROL_LABELS.unknown_author)).toHaveLength(2);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5 and the tester's F20: an agent's words that cite
// a statement and quote a typed block draw the statement by its name, never its address, and
// none of the block's source lines: whole in the cell through `RowText`, its link out of the tab
// order as every control inside a row is (R9), and as one line of plain words on the because
// line.
test("an agent's decision draws its words with each reference named, in its cell and on its because line", async () => {
  const cited = statementRef("r4");
  const name = "Every statement is drawn whole";
  const words = [
    `Keep ${cited} as its author wrote it.`,
    "",
    ":::requirement{#r4}",
    "Each row is one line.",
    ":::",
  ].join("\n");
  const tree = displayTree(words);
  if (tree === undefined) throw new Error("the engine read no tree of the decision's words");
  // The cell names the statement by its own read in the commit before the page's names reach the
  // store, which reads the spec's blocks.
  const blocks = spyOn(api, "getReferences").mockResolvedValue({ edges: [] } as never);
  const { read, view } = drawBody(
    issueTree([
      statement("r4", { text: `**R4.** ${name}.` }),
      // The tree names the statement a decision shaped by a chip on the decision, from the
      // batched read (`tree-drafts.ts`), whose name the page publishes, which a plain line names
      // it by (`referenceStandIns`).
      decisionNode("d1", {
        chips: [{ answered: true, gone: false, reading: { words: name }, ref: cited }],
        text: words,
        tree,
      }),
    ]),
    [
      decision("d1", {
        text: words,
        text_tree: tree,
        decision: {
          kind: "agent",
          links: [shaped(cited)],
        },
      }),
    ],
    { names: new Map([[cited, name]]) }
  );
  try {
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-cell] a[data-row-reference]")?.textContent
      ).toBe(name)
    );
    await waitFor(() =>
      expect(
        view.container
          .querySelector("[data-spec-cell] a[data-row-reference]")
          ?.getAttribute("tabindex"),
        "the cell's link is in the tab order"
      ).toBe("-1")
    );
    for (const selector of ["[data-spec-cell]", "[data-spec-because]"]) {
      const drawn = view.container.querySelector(selector)?.textContent ?? "";
      expect(drawn).toContain(name);
      expect(drawn).not.toContain("dispatch://");
      expect(drawn).not.toContain(":::");
    }
    expect(view.container.querySelector("[data-spec-because-link]")?.textContent).toBe(
      `“Keep ${name} as its author wrote it.”`
    );
  } finally {
    view.unmount();
    blocks.mockRestore();
    read.mockRestore();
  }
});

// R5: the cell gives the question an answer answers by its first sentence whole, ending at its
// first sentence end as the answer's quote does and with no limit of characters, also where the
// group's heading is that question. The heading draws the question's first line, which R1 cuts
// at 100 characters inside a longer sentence; the cell, and the line under the statement before
// the quote, draw the whole sentence and nothing of the next.
test("a cell whose group is headed by its question draws the question's first sentence whole", () => {
  const first =
    "Should the Spec view draw every statement of the spec whole, with each of its words, rather than cut it at six lines?";
  expect(first.length).toBeGreaterThan(100);
  const ask = {
    ...answeredAsk("ask-1", { text: "Keep the memo in the pane. It reads better there." }, [
      statement("r1"),
    ]),
    text: `${first} Then a second sentence follows it.`,
  };
  const { read, view } = drawBody(issueTree([ask]));
  try {
    const heading =
      view.container.querySelector("[data-spec-group-heading] > p:last-child")?.textContent ?? "";
    expect(heading.endsWith("…"), "the heading is not the question's first line, cut by R1").toBe(
      true
    );
    expect(first.startsWith(heading.slice(0, -1))).toBe(true);
    const cell = view.container.querySelector("[data-spec-cell]");
    expect(cell?.getAttribute("data-decision-id")).toBe("ask-1");
    expect(
      textsOf(view, "[data-spec-cell] [data-spec-question]"),
      "the cell under its question's heading does not draw that question's first sentence whole"
    ).toEqual([first]);
    expect(
      textsOf(view, "[data-spec-because] [data-spec-question]"),
      "the because line does not draw the question's first sentence whole"
    ).toEqual([first]);
    const because = textsOf(view, "[data-spec-because]")[0] ?? "";
    expect(because.startsWith(`${SPEC_WORDS.because} ${first} “Keep the memo in the pane.”`)).toBe(
      true
    );
    for (const drawn of [cell?.querySelector("[data-spec-source]")?.textContent ?? "", because]) {
      expect(drawn, "a whole first sentence is drawn with an ellipsis").not.toContain("…");
      expect(drawn, "the question's second sentence is drawn").not.toContain("Then a second");
    }
    expect(textsOf(view, "[data-spec-quote]")).toEqual(["“Keep the memo in the pane.”"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

/** `ref`'s tie as `shaped` makes it, made at `created`, whose far end - the statement - last
 *  changed at `changed`. */
function tieChangedAt(ref: string, created: string, changed: string | null): DecisionLink {
  const tie = shaped(ref);
  return { ...tie, created_at: created, to: { ...tie.to, changed_at: changed } };
}

// R5: where a statement's words changed after the tie that names its decision, its because line
// says "Changed since": the tie's far end's `changed_at` is later than the tie's `created_at`,
// both as the decision's links in the Spec view read answer them, read as the times they name. A
// statement the decision named after its last change says nothing of it, and so does r3, whose
// two times would order the other way read as text: its far end changed 0.12 s past the second,
// before the tie made 0.123456 s past it, though "…00.12Z" sorts after "…00.123456Z".
test("a statement changed after its tie says Changed since", () => {
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), statement("r3"), decisionNode("d1")]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [
            tieChangedAt(statementRef("r1"), "2026-11-14T10:00:00Z", "2026-11-15T08:00:00Z"),
            tieChangedAt(statementRef("r2"), "2026-11-14T10:00:00Z", "2026-11-14T09:00:00Z"),
            tieChangedAt(
              statementRef("r3"),
              "2026-11-14T10:00:00.123456Z",
              "2026-11-14T10:00:00.12Z"
            ),
          ],
        },
      }),
    ]
  );
  const lineOf = (block: string): string =>
    rowOf(view, `spec#b-${block}`).querySelector("[data-spec-because]")?.textContent ?? "";
  try {
    expect(lineOf("r1")).toContain(`${SPEC_WORDS.because} “The decision d1.”`);
    expect(
      lineOf("r1").endsWith(` · ${SPEC_WORDS.changed_since}`),
      "the statement changed after its tie does not say Changed since after who and when"
    ).toBe(true);
    for (const block of ["r2", "r3"]) {
      expect(lineOf(block)).toContain(`${SPEC_WORDS.because} “The decision d1.”`);
      expect(lineOf(block), `${block}, unchanged since its tie, says Changed since`).not.toContain(
        SPEC_WORDS.changed_since
      );
    }
    expect(textsOf(view, "[data-spec-changed-since]")).toEqual([SPEC_WORDS.changed_since]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: an agent's decision's words are drawn whole in its cell, with no limit of lines, with the
// focus on the cell or without it. The words are laid out 9,600 px tall, past the six lines at
// which the outline cuts an agent's words behind "more · N words" (`useClamp`); the cell draws
// every line of them and no such control.
test("an agent's decision draws its words whole in its cell at any length, with the focus or without it", async () => {
  const words = Array.from({ length: 60 }, (_unused, at) => `word${at + 1}`).join(" ");
  const text = `Draw every statement whole. ${words}`;
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), decisionNode("d1", { text })]),
    [decision("d1", { text, decision: { kind: "agent", links: [shaped(statementRef("r1"))] } })]
  );
  const cell = view.container.querySelector<HTMLElement>("[data-spec-cell][data-decision-id]");
  const expectWhole = async (when: string): Promise<void> => {
    // The page measures only the words of rows near the viewport, so a cut would be read once
    // they are measured.
    await measureNearWords(near);
    const drawn = cell?.querySelector<HTMLElement>("[data-spec-quote] [data-node-words]");
    expect(drawn?.textContent, `the cell's words are not drawn ${when}`).toBe(text);
    expect(drawn?.style.maxHeight, `the cell's words are cut ${when}`).toBe("");
    expect(cell?.querySelector("[data-node-more]"), `the cell draws "more" ${when}`).toBeNull();
  };
  try {
    if (cell === null) throw new Error("the body draws no cell for the agent's decision");
    await expectWhole("with no focus");
    act(() => cell.focus());
    await expectWhole("with the focus on the cell");
    act(() => rowOf(view, "spec#b-r2").focus());
    await expectWhole("once the focus left the cell");
  } finally {
    view.unmount();
    read.mockRestore();
    near.restore();
    restore();
  }
});

// R5: "The focus on the line draws the whole answer word for word". The cell draws the first
// sentence until it has the focus, and then the answer character for character as the store
// holds it; `w` on a statement is the key that puts the focus there.
test("the focus on a decision draws the person's whole answer, character for character", async () => {
  const stored = "Keep the memo in the pane.\n\n  It reads better there -  really.";
  const user = userEvent.setup();
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { text: stored }),
      statement("r1"),
      // The tree names every block a decision links as a chip on it, as part F's model does
      // (`tree-drafts.ts`).
      decisionNode("d1", {
        chips: [{ answered: true, gone: false, reading: { words: "r1" }, ref: statementRef("r1") }],
      }),
    ]),
    [
      decision("d1", {
        decision: {
          asks: ["ask-1"],
          kind: "person",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ]
  );
  try {
    expect(view.container.querySelector("[data-spec-whole-answer]")).toBeNull();
    act(() => rowOf(view, "spec#b-r1").focus());
    await user.keyboard("w");
    const cell = view.container.querySelector<HTMLElement>("[data-spec-cell]");
    expect(document.activeElement).toBe(cell);
    expect(view.container.querySelector("[data-spec-whole-answer]")?.textContent).toBe(stored);
    act(() => rowOf(view, "spec#b-r1").focus());
    expect(view.container.querySelector("[data-spec-whole-answer]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R11, the tester's F16: the statements are the tree's rows, the arrows move between them with
// one tab stop, and a key with nowhere to go says so at the statement it was pressed on.
test("the arrows move between the statements, and a key with nowhere to go reports at its row", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    const first = rowOf(view, "spec#b-r1");
    const second = rowOf(view, "spec#b-r2");
    expect([first.tabIndex, second.tabIndex]).toEqual([0, -1]);
    act(() => first.focus());
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(second);
    expect([first.tabIndex, second.tabIndex]).toEqual([-1, 0]);
    await user.keyboard("w");
    expect(second.querySelector("[data-row-report]")?.textContent).toBe(KEY_REPORTS.no_decision);
    expect(first.querySelector("[data-row-report]")).toBeNull();
    // Moving on lets the report go.
    await user.keyboard("{ArrowUp}");
    expect(view.container.querySelector("[data-row-report]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

test("a key holds the focused item only when the page answered it", async () => {
  const user = userEvent.setup();
  const sizes = installResizeObserver();
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    const first = rowOf(view, "spec#b-r1");
    const second = rowOf(view, "spec#b-r2");
    act(() => first.focus());
    // The hold watches the page for rows drawn above the item. The row's own placing
    // watches only the row, so it cannot produce this observation.
    for (const key of ["{PageDown}", " ", "{PageUp}"]) {
      await user.keyboard(key);
      expect(document.activeElement).toBe(first);
      expect(sizes.observed(), `${key} started a hold`).not.toContain(document.body);
    }

    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(second);
    expect(sizes.observed(), "ArrowDown did not start a hold").toContain(document.body);
    await user.keyboard(" ");
    expect(sizes.observed(), "Space kept the hold from ArrowDown").not.toContain(document.body);

    // End on the last row moves no focus, yet the keymap answers it and prevents its default,
    // so it still holds the row on screen while earlier rows load above it.
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(second);
    expect(sizes.observed(), "End on the last row did not start a hold").toContain(document.body);
    await user.keyboard("{PageDown}");
    expect(sizes.observed(), "PageDown kept the hold from End").not.toContain(document.body);
  } finally {
    view.unmount();
    read.mockRestore();
    sizes.restore();
  }
});

// R19: an address that names a statement (`/spec#b-<block>`) puts the focus on it.
test("an address naming a statement puts the focus on it", async () => {
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]), [], {
    reveal: "spec#b-r2",
  });
  try {
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r2")));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

/** The tree read for the stage's cases: the issue's two threads, each comment a node of it, as
 *  the outline's own cases answer it (`IssueTree.test.tsx`'s `treeReadOf`), and a `requirement`
 *  block of the spec for each of `statements`, the statements the tree read holds. */
function threadsTree(statements: readonly string[] = []): TreeAnswer {
  return treeAnswer([
    treeNode(KEY, "issue", null),
    ...statements.map((id, position) =>
      treeNode(id, "requirement", KEY, {
        document: { artifact_id: "artifact-spec", position, slug: "spec" },
        text: `The page draws ${id}.`,
      })
    ),
    ...TWO_THREADS.map((comment) => commentNode(comment)),
  ]);
}

/** Answers the tree read with `threadsTree` of `statements`, and the Spec view's read of it
 *  never, so the view is drawn off the whole tree, as before the page holds its view read
 *  (`presets.ts`). */
function answerThreadsTree(statements: readonly string[] = []) {
  return spyOn(api, "getIssueTree").mockImplementation(wholeTreeOnly(threadsTree(statements)));
}

/** `client` holding every read the page's tree is built from, answered: the issue holds two
 *  threads of comments and nothing else, as the outline's own cases seed them
 *  (`IssueTree.test.tsx`'s `seedBodyReads`), and the tree read holds them as nodes
 *  (`threadsTree`). Who is reading is each case's own. */
function holdTreeReads(client: QueryClient): void {
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, resultsOf([]));
  client.setQueryData(viewKey(KEY, "spec"), treeAnswer([]));
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(issueCommentsQuery(KEY).queryKey, [...TWO_THREADS]);
  client.setQueryData(issueMessagesQuery(KEY).queryKey, []);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, threadsTree());
}

// The names read is the page's tree's, read last, once no read the tree is built from is still
// out, and the Spec view draws from it too: the name of every reference the issue's three
// sentences and a plain line cite. A refused names read says so in its own line, after the lines of
// the view's own reads, as the outline draws it last, and its Retry asks the names again. The
// stage is the page's own with the Spec view's slot as filled, which reads the tree and passes
// the names' line on to the body; with that line dropped, "In short" stood as a bare heading and
// nothing said why.
test("a failed names read says so in its own line, after the view's own, and its Retry asks the names again", async () => {
  const user = userEvent.setup();
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Every read the page's tree is built from answered but the Spec view read, which is refused,
  // so the view draws a line of its own before the names': the statements', for the one read the
  // spec's blocks and the decisions both come from (`useTreeRead`, decision 450).
  client.setQueryData(["whoami"], { kind: "user", login: READER });
  holdTreeReads(client);
  client.removeQueries({ queryKey: viewKey(KEY, "spec") });
  const trees = spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) =>
    query.view === undefined ? Promise.resolve(threadsTree()) : Promise.reject(new Error("offline"))
  );
  const names = spyOn(api, "getReferenceTargets").mockRejectedValue(new Error("offline"));
  const view = drawStage("", { client, issue, preset: "spec" });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.names_unreadable));
    const lines = '[data-spec-body] [data-failed-read] [role="alert"]';
    expect(textsOf(view, lines), "the names' line is not drawn after the view's own").toEqual([
      STATE_LINES.statements_unreadable,
      STATE_LINES.names_unreadable,
    ]);
    names.mockResolvedValue({ targets: [] } as never);
    const asked = names.mock.calls.length;
    await user.click(within(line).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(textsOf(view, lines), "the names' line stayed once the names answered").toEqual([
        STATE_LINES.statements_unreadable,
      ])
    );
    expect(names.mock.calls.length, "the names' Retry did not ask the names again").toBeGreaterThan(
      asked
    );
  } finally {
    view.unmount();
    names.mockRestore();
    trees.mockRestore();
  }
});

// Under the Spec filter the spec's blocks are the Spec view read's (`useTreeRead`'s `blocksOf`),
// and the statements line runs by that read (`useBlocksRunning`), not by Conversation's, which
// the page reads beside the view from 1024 px up. With Conversation's read out and the view read
// refused, the line's Retry sends the view read again, the line is busy while it runs, and it
// leaves once the view read answers.
test("with Conversation's read out, the statements line's Retry sends the refused Spec view read, and is busy while it runs", async () => {
  const user = userEvent.setup();
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: READER });
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, resultsOf([]));
  client.setQueryData(["decisions", KEY], { decisions: [], document: null, open_questions: [] });
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(["comments", KEY], TWO_THREADS);
  client.setQueryData(["issue-messages", KEY], []);
  // Conversation's read never answers; the Spec view read is refused, and then answers when the
  // case says.
  const again = Promise.withResolvers<TreeAnswer>();
  let specReads = 0;
  const trees = spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
    const asked = query as { readonly kind?: unknown; readonly view?: string };
    if (asked.view !== "spec") return Promise.withResolvers<never>().promise;
    specReads += 1;
    return specReads === 1 ? Promise.reject(new Error("offline")) : again.promise;
  });
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  void client.prefetchQuery(outlineQuery(KEY, "conversation"));
  const view = drawStage("", { client, issue, preset: "spec" });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    expect(
      client.getQueryState(outlineQuery(KEY, "conversation").queryKey)?.fetchStatus,
      "Conversation's read is not out"
    ).toBe("fetching");
    const retry = within(line).getByRole("button", { name: "Retry" });
    await user.click(retry);
    await waitFor(() => expect(specReads, "the Retry sent no Spec view read").toBe(2));
    expect(retry.getAttribute("aria-busy"), "the line is not busy while the view read runs").toBe(
      "true"
    );
    await act(async () => again.resolve(treeAnswer([])));
    await waitFor(() =>
      expect(view.container.textContent).not.toContain(STATE_LINES.statements_unreadable)
    );
  } finally {
    view.unmount();
    names.mockRestore();
    trees.mockRestore();
  }
});

// The Spec view's slot reads who is reading as the tree reads it, kept fresh by events and never
// read again by its age (`EVENT_FRESH`). Under the app's 30 s `staleTime` (`main.tsx`) the view
// opened more than 30 s after whoami answered asked for it again, a request of the view's own.
test("the Spec view opened more than 30 s after whoami answered asks for it nothing", async () => {
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  holdTreeReads(client);
  const trees = answerThreadsTree();
  client.setQueryData(
    whoAmIQuery().queryKey,
    { event_head: "0", kind: "user", login: READER },
    { updatedAt: Date.now() - 31_000 }
  );
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: READER,
  });
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const view = drawStage("", { client, issue, preset: "spec" });
  try {
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-body]"),
        "the stage drew no Spec view"
      ).not.toBeNull()
    );
    expect(whoAmI, "the Spec view asked for whoami again").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    whoAmI.mockRestore();
    names.mockRestore();
    trees.mockRestore();
  }
});

// The address's statement takes the tab stop only where the reader has not acted since they
// followed the address, in whatever render the view first draws it. The coverage and the tree
// read, which the statements come from and the settling waits on as well, are held, so the view
// draws no statement and the stage finds no row to give the focus, which stays on the page's
// body. The reader turns the wheel there. The reads then answer together, and the statements are
// first drawn in the render the reads settle in: the landing yields to the wheel, and the stage
// gives the focus it finds on nothing to the tab-stop row, the first statement, and not the
// address's, which a spec taller than the screen draws below it.
test("a Spec address whose statements are drawn only once the reads settle keeps the stop off the address's statement where the reader acted first", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: READER });
  holdTreeReads(client);
  client.removeQueries({ queryKey: issueCoverageQuery(KEY).queryKey });
  client.removeQueries({ queryKey: outlineQuery(KEY, "conversation").queryKey });
  const answered = Promise.withResolvers<void>();
  const coverage = spyOn(api, "getIssueCoverage").mockImplementation(async () => {
    await answered.promise;
    return coverageRead(["r1", "r2"]);
  });
  const whole = wholeTreeOnly(threadsTree(["r1", "r2"]));
  const trees = spyOn(api, "getIssueTree").mockImplementation(async (key, query) => {
    await answered.promise;
    return whole(key, query);
  });
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const view = drawStage("#b-r2", { client, issue: issueDetails(), preset: "spec" });
  try {
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-tree]"),
        "the stage drew no Spec view"
      ).toBeInTheDocument()
    );
    expect(
      view.container.querySelector("[data-spec-body] [data-node-row]"),
      "the view drew a statement before the reads answered"
    ).toBeNull();
    await act(async () => window.dispatchEvent(new Event("wheel")));
    await act(async () => answered.resolve());
    const first = await waitFor(() => {
      const row = rowOf(view, "spec#b-r1");
      expect(
        row,
        `the statements drawn after a turn of the wheel left the focus on ${document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId ?? document.activeElement?.tagName}`
      ).toHaveFocus();
      return row;
    });
    // The frame a landing would have asked for as the reads settled.
    await act(() => nextFrame());
    const addressed = rowOf(view, "spec#b-r2");
    expect(
      first,
      `the reads settling moved the focus to ${document.activeElement?.tagName}`
    ).toHaveFocus();
    expect(
      [first.tabIndex, addressed.tabIndex],
      "the address's statement took the tab stop after the reader acted"
    ).toEqual([0, -1]);
  } finally {
    view.unmount();
    coverage.mockRestore();
    trees.mockRestore();
    names.mockRestore();
  }
});

// The stage's controller of the reader's acts is a following's own only from that following's
// first commit, so the render that draws a view at a new address still reads the last
// following's, which the press that followed the new address ended. The outline is drawn with
// every read answered, and the reader presses, as on a link to a statement, which opens the Spec
// view at the statement's address: the view draws its statements in its first render, from the
// reads the page holds, and the address's statement takes the tab stop and the focus, since the
// reader has not acted since they followed that address.
test("the Spec view opened at a statement after a press on the outline gives that statement the stop and the focus", async () => {
  const issue = issueDetails();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: READER });
  holdTreeReads(client);
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead(["r1", "r2"]));
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, threadsTree(["r1", "r2"]));
  const trees = answerThreadsTree(["r1", "r2"]);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const held: { navigate?: NavigateFunction } = {};
  const view = drawStage("", { client, held, issue, preset: "conversation" });
  try {
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-issue-body] [data-node-row]"),
        "the stage drew no outline"
      ).toBeInTheDocument()
    );
    await act(async () => window.dispatchEvent(new Event("pointerdown")));
    await act(async () => held.navigate?.(`${presetPath(KEY, "spec")}#b-r2`));
    const addressed = await waitFor(() => {
      const row = rowOf(view, "spec#b-r2");
      expect(
        row,
        `the Spec view opened at the statement's address left the focus on ${document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId ?? document.activeElement?.tagName}`
      ).toHaveFocus();
      return row;
    });
    // The frame the landing asked for as the view was drawn.
    await act(() => nextFrame());
    expect(
      addressed,
      `the landing moved the focus to ${document.activeElement?.tagName}`
    ).toHaveFocus();
    expect(
      [rowOf(view, "spec#b-r1").tabIndex, addressed.tabIndex],
      "the address's statement took no tab stop"
    ).toEqual([-1, 0]);
  } finally {
    view.unmount();
    names.mockRestore();
    trees.mockRestore();
  }
});

// The Spec view draws the names read's line as the tree says it, speaking or not
// (`NamesFailure.speaks`): a line that stands again for a refusal it already said is drawn
// without speaking. A reply cites another issue, so the names read of the tree's set names it;
// that read is refused. The reply goes with the tree read's next answer, the names read of the
// smaller set answers, and the line leaves. The reply comes back: the names read is back on the
// first set, which holds no answer and the refusal the line counted, and runs again, so the line
// stands again, with no live role.
test("a names line that stands again in the Spec view for a refusal it counted is drawn without speaking", async () => {
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  holdTreeReads(client);
  const cited = TWO_THREADS.map((comment) =>
    comment.id === "reply-b" ? { ...comment, body: "Read dispatch://CORE-8 first." } : comment
  );
  const threads = (comments: typeof cited) =>
    treeAnswer([treeNode(KEY, "issue", null), ...comments.map((one) => commentNode(one))]);
  client.setQueryData(issueCommentsQuery(KEY).queryKey, cited);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, threads(cited));
  const trees = answerThreadsTree();
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: READER });
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  names.mockRejectedValueOnce(new Error("offline"));
  const view = drawStage("", { client, issue, preset: "spec" });
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.names_unreadable));
    const fewer = cited.filter((comment) => comment.id !== "reply-b");
    await act(async () => {
      client.setQueryData(issueCommentsQuery(KEY).queryKey, fewer);
      client.setQueryData(outlineQuery(KEY, "conversation").queryKey, threads(fewer));
    });
    await waitFor(() =>
      expect(
        view.queryByText(STATE_LINES.names_unreadable),
        "the names' line stayed once the names of the smaller set answered"
      ).toBeNull()
    );
    names.mockReturnValueOnce(new Promise<never>(() => {}) as never);
    await act(async () => {
      client.setQueryData(issueCommentsQuery(KEY).queryKey, cited);
      client.setQueryData(outlineQuery(KEY, "conversation").queryKey, threads(cited));
    });
    const words = await waitFor(() => view.getByText(STATE_LINES.names_unreadable));
    expect(words, "the names' line said again a refusal it had counted").not.toHaveAttribute(
      "role"
    );
    expect(view.queryByRole("alert"), "a line standing again spoke").toBeNull();
  } finally {
    view.unmount();
    names.mockRestore();
    trees.mockRestore();
  }
});

// The page acts on each of the Contents list's asks
// once, whichever outline or body took it (`outline-request.tsx`), so a body drawn again - the
// view turned off and on again - does not move the reader back to an ask it already went to.
test("a body drawn again does not act on a request the page already took", async () => {
  const { read, redraw, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), statement("r3")]),
    [],
    { request: { focusBranch: false, id: "spec#b-r2", seq: 1 } }
  );
  try {
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r2")));
    act(() => rowOf(view, "spec#b-r3").focus());
    redraw(false);
    redraw(true);
    await act(async () => {});
    expect(view.container.querySelector("[data-spec-tree]")).not.toBeNull();
    expect(document.activeElement).not.toBe(rowOf(view, "spec#b-r2"));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A focus on the tree itself goes on to its tab-stop row, so the reader is on a row and the
// tree's keys work from there.
test("a focus on the tree goes on to its tab-stop row", () => {
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    act(() => view.container.querySelector<HTMLElement>("[data-spec-tree]")?.focus());
    expect(document.activeElement).toBe(rowOf(view, "spec#b-r1"));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// Where the view draws no row, part D's `reachPart` puts the focus on the tree itself, and it
// stays there: the focus is in the view, never on the document's body.
test("a view with no statement holds the focus on its tree", () => {
  const { read, view } = drawBody(issueTree([]));
  try {
    const tree = view.container.querySelector<HTMLElement>("[data-spec-tree]");
    expect(tree?.tabIndex).toBe(-1);
    expect(() => act(() => tree?.focus())).not.toThrow();
    expect(document.activeElement).toBe(tree);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R17, R6: each statement carries how it stands at the right of its row, by a shape with a name
// of its own, so the mark is never colour alone; a statement no check serves carries that.
test("each statement carries how it stands, by a shape with its own name", () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const failed = check({
    block_id: "c2",
    pass_stands: { answer: "no" },
    results: [result({ id: "f1", verdict: "fail" })],
  });
  const unrun = check({ block_id: "c3", pass_stands: { answer: "no" }, results: [] });
  const { read, view } = drawBody(
    issueTree([
      statement("r1", { children: [checkNode(passed)] }),
      statement("r2", { children: [checkNode(failed)] }),
      statement("r3", { children: [checkNode(unrun)] }),
      statement("r4", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
    ]),
    [],
    { checks: [passed, failed, unrun] }
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-spec-statement]")].map((row) => [
        row.getAttribute("data-spec-mark"),
        row.querySelector(":scope > svg[data-glyph]")?.getAttribute("aria-label"),
      ])
    ).toEqual([
      ["met", VERIFICATION_WORDS.states.met],
      ["not_met", VERIFICATION_WORDS.states.not_met],
      ["unknown", VERIFICATION_WORDS.states.unknown],
      ["no_check", GAP_WORDS.no_check],
    ]);
    // No two of the four shapes are one shape.
    const paths = [
      ...view.container.querySelectorAll("[data-spec-statement] > svg[data-glyph] path"),
    ].map((path) => path.getAttribute("d"));
    expect(new Set(paths).size).toBe(4);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// Ruling 1 and ruling 6: a statement's own words carry its number where the spec wrote one, and
// are drawn as its author wrote them, whole: the shared renderer draws the stored markdown, so
// the marks the author typed around the number are drawn as the bold they say and are no
// characters of the words.
test("a statement's words are drawn as its author wrote them", () => {
  const rest =
    " Every node's words are on the page without opening it; an agent's long words " +
    "show their first six lines and the rest one key away, and his words are never cut.";
  const { read, view } = drawBody(issueTree([statement("r1", { text: `**R1.**${rest}` })]));
  try {
    expect(
      view.container.querySelector("[data-spec-statement] [data-node-words]")?.textContent
    ).toBe(`R1.${rest}`);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R6: a statement's words are whole, with no limit of lines, with the focus or without it, and
// the chips it carries stay after its last word, as a row of the outline draws its trail. The
// words are laid out 9,600 px tall, past the six lines at which the outline cuts an agent's
// words behind "more · N words" with a fade (`useClamp`); the row draws every line, no such
// control and no fade, and its trail stays the last child of its words' box.
test("a statement's words are drawn whole at any length, with no more control, with the focus or without it", async () => {
  const words = Array.from({ length: 60 }, (_unused, at) => `word${at + 1}`).join(" ");
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  const { read, view } = drawBody(
    issueTree([
      statement("r1"),
      statement("r2", { gaps: ["shaped_disagreement"], text: `**R2.** ${words}` }),
    ])
  );
  const expectWhole = async (when: string): Promise<void> => {
    // The page measures only the words of rows near the viewport, so a cut would be read once
    // they are measured.
    await measureNearWords(near);
    const row = rowOf(view, "spec#b-r2");
    const box = row.querySelector<HTMLElement>("[data-node-words]");
    expect(box?.textContent, `the statement's words are not drawn ${when}`).toContain("word60");
    expect(box?.style.maxHeight, `the statement's words are cut ${when}`).toBe("");
    expect(box?.className, `the statement's words are clipped ${when}`).not.toContain("clip");
    expect(box?.querySelector(':scope > span[aria-hidden="true"]'), `a fade ${when}`).toBeNull();
    expect(row.querySelector("[data-node-more]"), `the statement draws "more" ${when}`).toBeNull();
    expect(
      box?.lastElementChild?.hasAttribute("data-node-trail"),
      `the statement's chips do not follow its last word ${when}`
    ).toBe(true);
  };
  try {
    await expectWhole("with no focus");
    act(() => rowOf(view, "spec#b-r2").focus());
    await expectWhole("with the focus on it");
    act(() => rowOf(view, "spec#b-r1").focus());
    await expectWhole("once the focus left it");
  } finally {
    view.unmount();
    read.mockRestore();
    near.restore();
    restore();
  }
});

// R6: at the right end of a statement's first line, before its mark, stands the count of the
// checks that serve it - the coverage read's `checks`, which the tree carries on the node - as a
// number and its word, outside the statement's words. It is a link to the Verification view with
// the focus on that statement's row, out of the tab order, and `f` reaches it as it reaches every
// link of a row. A statement no check serves draws no count.
test("a statement's count of checks links to the Verification view at its row, and is drawn only where a check serves it", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(
    issueTree([
      statement("r1", { counts: { checks: 3, planTasks: 0 } }),
      statement("r2", { counts: { checks: 1, planTasks: 0 } }),
      statement("r3", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
    ])
  );
  const countOf = (block: string): HTMLElement | null =>
    rowOf(view, `spec#b-${block}`).querySelector<HTMLElement>("[data-spec-checks]");
  try {
    const three = countOf("r1");
    if (three === null) throw new Error("a statement three checks serve draws no count");
    expect(three.textContent).toBe(`3 ${VERIFICATION_WORDS.check_many}`);
    expect(countOf("r2")?.textContent).toBe(`1 ${VERIFICATION_WORDS.check_one}`);
    expect(countOf("r3"), "a statement no check serves draws a count").toBeNull();
    expect(three.getAttribute("href")).toBe(`/issues/${KEY}/verification#b-r1`);
    expect(countOf("r2")?.getAttribute("href")).toBe(`/issues/${KEY}/verification#b-r2`);
    expect(three.getAttribute("tabindex"), "the count is in the tab order").toBe("-1");
    const row = rowOf(view, "spec#b-r1");
    expect(row.querySelector("[data-node-words] [data-spec-checks]")).toBeNull();
    const mark = row.querySelector(":scope > svg[data-glyph]");
    if (mark === null) throw new Error("the statement draws no mark");
    expect(
      three.compareDocumentPosition(mark) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the count does not stand before the mark"
    ).not.toBe(0);
    act(() => row.focus());
    await user.keyboard("f");
    expect(document.activeElement, "f does not reach the count").toBe(three);
    await user.click(three);
    expect(locationOf(view)).toBe(`/issues/${KEY}/verification#b-r1`);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R2: a row the filter draws only to place another is drawn faint and marked as context.
test("a statement drawn as context is marked as such", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]), [], {
    context: new Set(["spec#b-r1"]),
  });
  try {
    expect(
      view.container.querySelector("[data-spec-statement]")?.hasAttribute("data-node-context")
    ).toBe(true);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A statement of this body is a row of the page: the readers that walk the outline's rows find
// it by the attributes every row carries (`e2e/issue-page.ts`'s `drawnRows` and `nodeRow`),
// which is what c-presets compares a filter's drawn ids with.
test("a statement is drawn as a row of the page, with the id and kind every row carries", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    const row = view.container.querySelector("[data-node-row]");
    expect(row?.getAttribute("data-node-id")).toBe("spec#b-r1");
    expect(row?.getAttribute("data-node-type")).toBe("requirement");
    expect(row?.getAttribute("aria-level")).toBe("1");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R20: a node an address names outranks the filter. It is no statement: it is drawn by its
// kind and its words, with no mark and no decision cell beside it.
test("a row the address named that is no statement is drawn by its kind, with no mark", () => {
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  const { read, view } = drawBody(tree, [], { named: ["comment-1"] });
  try {
    const other = view.container.querySelector("[data-spec-other]");
    expect(other?.getAttribute("data-node-id")).toBe("comment-1");
    expect(other?.textContent).toContain("comment");
    expect(other?.textContent).toContain("A remark the address names.");
    expect(other?.querySelector("svg[data-glyph]")).toBeNull();
    expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
    expect(view.container.querySelectorAll("[data-spec-cell]")).toHaveLength(1);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

test("a row the address keeps is drawn whole with the focus elsewhere", async () => {
  const text = `${"These words take a line of their own. ".repeat(60)}[Guide](/settings)`;
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  const { read, view } = drawBody(
    issueTree([
      statement("r1"),
      node({ id: "comment-1", text, tree: displayTree(text), type: "comment" }),
    ]),
    [],
    { named: ["comment-1"] }
  );
  try {
    const row = rowOf(view, "comment-1");
    const statementRow = rowOf(view, "spec#b-r1");
    act(() => row.focus());
    act(() => statementRow.focus());
    await measureNearWords(near);
    expect(statementRow).toHaveFocus();
    expect(row.querySelector("[data-node-more]")).toBeNull();
    expect(row.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight).toBe("");
    expect(within(row).getByRole("link", { name: "Guide" })).toHaveAttribute("tabindex", "-1");
  } finally {
    view.unmount();
    read.mockRestore();
    near.restore();
    restore();
  }
});

// An issue whose spec holds no statement says so, in the words the outline says it in.
test("an issue with no statements says so in the page's own line", () => {
  const { read, view } = drawBody(issueTree([]));
  try {
    expect(view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
      EMPTY_STATE_LINES.spec
    );
    expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(0);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// The body is the outline the page draws under the Spec filter, so it is one tree, named as
// every other filter's outline is: every browser row reads the page by that name
// (`e2e/issue-layout.e2e.ts`'s `expectIssuePageDrawn`). Its items are each statement and the
// decision beside it, which is what the focus moves between (R5's `w`).
test("the body draws one tree, named as the outline is, of the statements and their decisions", () => {
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), [
    decision("d1", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  try {
    const tree = view.container.querySelector('[role="tree"]');
    expect(tree?.getAttribute("aria-label")).toBe(CONTROL_LABELS.tree);
    expect(
      [...view.container.querySelectorAll('[role="treeitem"]')].map((item) =>
        item.hasAttribute("data-spec-cell") ? "cell" : item.getAttribute("data-node-id")
      )
    ).toEqual(["cell", "spec#b-r1"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R10, R17: a gap the mark does not already say is drawn with the statement's words, in the
// few words the page knows it by: it stands in the trail after their last word, a direct child
// of the words' box, and nowhere else in the row. The one for a statement no check serves is the
// mark at the right of the row, and is not said twice: that statement draws no trail at all.
test("a gap the mark does not say is drawn after the statement's words, and no check is not said twice", () => {
  const { read, view } = drawBody(
    issueTree([
      statement("r1", { gaps: ["shaped_disagreement"] }),
      statement("r2", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
    ])
  );
  try {
    const rows = [...view.container.querySelectorAll("[data-spec-statement]")];
    expect(
      rows[0]?.querySelector("[data-node-words]")?.lastElementChild?.hasAttribute("data-node-trail")
    ).toBe(true);
    expect(
      [
        ...(rows[0]?.querySelectorAll("[data-node-words] > [data-node-trail] [data-gap-kind]") ??
          []),
      ].map((gap) => [gap.getAttribute("data-gap-kind"), gap.textContent])
    ).toEqual([["shaped_disagreement", GAP_WORDS.shaped_disagreement]]);
    expect(rows[0]?.querySelectorAll("[data-node-trail]")).toHaveLength(1);
    expect(rows[0]?.querySelectorAll("[data-gap-kind]")).toHaveLength(1);
    expect(rows[0]?.querySelector("[data-node-more]")).toBeNull();
    expect(rows[1]?.querySelectorAll("[data-gap-kind]")).toHaveLength(0);
    expect(rows[1]?.querySelector("[data-node-trail]")).toBeNull();
    expect(rows[1]?.getAttribute("data-spec-mark")).toBe("no_check");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5, review finding 4590606091: `w` on a statement goes to the cell beside it, which draws the
// decision its run is headed by. Part F's model hangs a person's decision under the question it
// answers, at the tree's end, and an agent's under the issue, before the statements, so the
// first decision of the tree's order that names r4 is the agent's, which only informed it and
// heads no run.
test("w on a statement goes to the cell beside it, whichever decision the tree's order names first", async () => {
  const made = [
    decision("d2", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r3")), shaped(statementRef("r4"))],
      },
    }),
    decision("d5", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r4"))],
      },
    }),
  ];
  // The tree read's answer: the agent's decision, the spec's two statements, and the question the
  // person's decision answers, each under the issue.
  const tree = buildIssueTree({
    asks: [askRead("ask-1", "Keep both statements.")],
    coverage: coverageRead(["r3", "r4"]),
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode("d5", "decision", KEY, {
          decision: { links: made[1]?.decision?.links ?? [] },
          document: { artifact_id: "artifact-dec", position: 0, slug: "decisions-md" },
        }),
        ...["r3", "r4"].map((id, position) =>
          treeNode(id, "requirement", KEY, {
            document: { artifact_id: "artifact-spec", position, slug: "spec" },
          })
        ),
        treeNode("ask-1", "ask", KEY),
      ],
      [treeTuple("shaped", "d5", statementRef("r4"))]
    ),
  });
  expect(decisionBehind(tree, "spec#b-r4")?.id).toBe("decisions-md#b-d5");
  const user = userEvent.setup();
  const { read, view } = drawBody(tree, made);
  try {
    const cells = [...view.container.querySelectorAll<HTMLElement>("[data-spec-cell]")];
    expect(cells.map((cell) => cell.getAttribute("data-spec-statements"))).toEqual([
      "spec#b-r3 spec#b-r4",
    ]);
    for (const id of ["spec#b-r3", "spec#b-r4"]) {
      act(() => rowOf(view, id).focus());
      await user.keyboard("w");
      expect(document.activeElement).toBe(cells[0] ?? null);
      expect(view.container.querySelector("[data-row-report]")).toBeNull();
    }
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R6, review finding 4590606103: a cell is no row of the keymap's, which would start again from
// the first statement of the view. From a cell, `j` and ArrowDown move on to the statement after
// its run, and `k` and ArrowUp back to the one before it.
test("from a decision's cell, j and ArrowDown move to the statement after its run, k and ArrowUp to the one before", async () => {
  const made = [
    decision("d1", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      },
    }),
    decision("d2", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r3")), shaped(statementRef("r4"))],
      },
    }),
    decision("d3", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r5"))],
      },
    }),
  ];
  const user = userEvent.setup();
  const { read, view } = drawBody(
    issueTree([
      ...["r1", "r2", "r3", "r4", "r5"].map((block) => statement(block)),
      ...["d1", "d2", "d3"].map((block) => decisionNode(block)),
    ]),
    made
  );
  try {
    for (const [from, key, to] of [
      ["r3", "j", "r5"],
      ["r5", "k", "r4"],
      ["r3", "{ArrowUp}", "r2"],
      ["r2", "{ArrowDown}", "r3"],
    ] as const) {
      act(() => rowOf(view, `spec#b-${from}`).focus());
      await user.keyboard("w");
      expect(document.activeElement?.hasAttribute("data-spec-cell")).toBe(true);
      await user.keyboard(key);
      expect(document.activeElement).toBe(rowOf(view, `spec#b-${to}`));
    }
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R6, R12, review finding 4590606112: the focused look and Details follow the focus wherever it
// goes in the tree - onto a decision's cell, or onto a link inside another statement - and the
// statement that had it lets both go.
test("a cell or a link inside a statement takes the focus, its look and Details from the statement that had it", async () => {
  const user = userEvent.setup();
  const linked = "**R2.** Read [the guide](https://example.com/guide) first.";
  const { read, view } = drawBody(
    issueTree([
      statement("r1"),
      // A statement's words with the server's reading of them, which draws the link as one.
      statement("r2", { text: linked, tree: displayTree(linked) }),
      decisionNode("d1"),
    ]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
        },
      }),
    ]
  );
  const looksFocused = (id: string) => rowOf(view, id).className.includes(pageFocusedRow);
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    expect(looksFocused("spec#b-r1")).toBe(true);
    expect(focusedNodeIn(view)).toEqual({ id: "spec#b-r1", type: "requirement" });
    await user.keyboard("w");
    expect(document.activeElement?.hasAttribute("data-spec-cell")).toBe(true);
    expect(looksFocused("spec#b-r1")).toBe(false);
    expect(focusedNodeIn(view)).toEqual({ id: "decisions-md#b-d1", type: "decision" });
    act(() => rowOf(view, "spec#b-r1").focus());
    const link = await waitFor(() => {
      const found = rowOf(view, "spec#b-r2").querySelector<HTMLElement>("[data-node-words] a");
      if (found === null) throw new Error("the second statement draws no link in its words");
      return found;
    });
    act(() => link.focus());
    expect(looksFocused("spec#b-r2")).toBe(true);
    expect(looksFocused("spec#b-r1")).toBe(false);
    expect(focusedNodeIn(view)).toEqual({ id: "spec#b-r2", type: "requirement" });
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// Review finding 4590606120: a press on what is no item of the tree - a group's heading - leaves
// the focus where it was, where the tree would take it as the nearest focusable box and hand it
// on to the tab-stop statement.
test("a press on a group's heading leaves the focus where it was", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(
    issueTree([
      node({
        children: [statement("r1")],
        id: "comment-1",
        text: "Where the statements go.",
        type: "comment",
      }),
      statement("r2"),
    ])
  );
  try {
    const heading = view.container.querySelector<HTMLElement>("[data-spec-group-heading]");
    if (heading === null) throw new Error("the body draws no group heading");
    // Nothing has the focus, and the tab stop is the first statement, where the tree would hand
    // a focus it took on.
    await user.pointer({ keys: "[MouseLeft]", target: heading });
    expect(view.container.contains(document.activeElement)).toBe(false);
    // A press on a statement does focus it, so the press above was one that could have.
    await user.pointer({ keys: "[MouseLeft]", target: rowOf(view, "spec#b-r2") });
    expect(document.activeElement).toBe(rowOf(view, "spec#b-r2"));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R9: a group's heading draws its parent's own words as its author's, whatever they say, so a
// parent whose author wrote exactly its kind's word is drawn in their words; only a parent with
// no words at all is named by the page's word for its kind.
test("a group's heading draws its parent's words as its author's, even where they are its kind's word", () => {
  const { read, view } = drawBody(
    issueTree([
      node({
        children: [statement("r1")],
        id: "comment-1",
        text: NODE_TYPE_NAMES.comment,
        type: "comment",
      }),
      node({ children: [statement("r2")], id: "comment-2", text: "", type: "comment" }),
    ])
  );
  try {
    const lines = [...view.container.querySelectorAll("[data-spec-group-heading] > p:last-child")];
    expect(lines.map((line) => line.textContent)).toEqual([
      NODE_TYPE_NAMES.comment,
      NODE_TYPE_NAMES.comment,
    ]);
    expect(
      lines.map((line) => line.hasAttribute(AUTHOR_TEXT_ATTRIBUTE)),
      "the heading draws its author's own words as the page's"
    ).toEqual([true, false]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R35, review finding 4590606126: the empty line says the issue holds no statement, which is
// true only once the coverage and the spec's blocks are read. While either is out the body says
// nothing; a read that failed says so, with a way to read again.
test("an issue says it has no statements only once they are read, and a failed read says so with Retry", async () => {
  for (const out of [{ coverage: "out" }, { blocks: "out" }] as const) {
    const drawn = drawBody(issueTree([]), [], out);
    try {
      expect(drawn.view.container.querySelector("[data-spec-empty]")).toBeNull();
    } finally {
      drawn.view.unmount();
      drawn.read.mockRestore();
    }
  }
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([]), [], { coverage: "failed" });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    expect(view.container.querySelector("[data-spec-empty]")).toBeNull();
    read.coverage.mockResolvedValue(coverageRead([]));
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
        EMPTY_STATE_LINES.spec
      )
    );
    expect(view.container.textContent).not.toContain(STATE_LINES.statements_unreadable);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R17, review finding 4590606132: a mark drawn before the results or the coverage answered would
// say Unknown or no check of a statement that is Met, so a statement carries none until both
// have; results that could not be read say so, with a way to read again.
test("a statement carries no mark until its results and coverage are read, and failed results say so with Retry", async () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1", { children: [checkNode(passed)] })]);
  for (const out of [{ results: "out" }, { coverage: "out" }] as const) {
    const drawn = drawBody(tree, [], { checks: [passed], ...out });
    try {
      const row = rowOf(drawn.view, "spec#b-r1");
      expect(row.hasAttribute("data-spec-mark")).toBe(false);
      expect(row.querySelector(":scope > svg[data-glyph]")).toBeNull();
    } finally {
      drawn.view.unmount();
      drawn.read.mockRestore();
    }
  }
  const user = userEvent.setup();
  const { read, view } = drawBody(tree, [], { checks: [passed], results: "failed" });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.spec_results_unreadable));
    expect(rowOf(view, "spec#b-r1").hasAttribute("data-spec-mark")).toBe(false);
    read.results.mockResolvedValue(resultsOf([passed]));
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("met")
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R6 and R17: a statement's mark is how it stands at the commit the page names, and the checks
// that say so come from the Verification document's blocks, which the page reads beside the
// results and the coverage. While that read is out a statement that is Met would read Unknown,
// and where it failed it would stay Unknown with nothing said; on a running build at 8caa4357,
// with that read refused, LAY-2's Met and Not met statements all read Unknown with no error
// line for over ten seconds (review 5928280634 on pull request 4234, the reviewer's finding a).
test("a statement carries no mark while the Verification document's blocks are out, and a failed read says so with Retry", async () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1")]);
  const waiting = drawBody(tree, [], { checks: [passed], verification: "out" });
  try {
    const row = rowOf(waiting.view, "spec#b-r1");
    expect(
      row.hasAttribute("data-spec-mark"),
      "a statement is marked before its checks are read"
    ).toBe(false);
  } finally {
    waiting.view.unmount();
    waiting.read.mockRestore();
  }
  const { read, view } = drawBody(tree, [], { checks: [passed], verification: "failed" });
  try {
    const alert = await waitFor(() => {
      const shown = view.container.querySelector<HTMLElement>('[role="alert"]');
      if (shown === null)
        throw new Error("a failed read of the Verification document says nothing");
      return shown;
    });
    // The shared failed-read line draws its Retry beside its alert, in the line that holds both.
    const line = alert.closest<HTMLElement>("[data-failed-read]") ?? alert;
    expect(within(line).getByRole("button", { name: "Retry" })).toBeDefined();
    expect(rowOf(view, "spec#b-r1").hasAttribute("data-spec-mark")).toBe(false);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R6 and R17: a read the page holds an answer for and asks again keeps that answer while it is
// out, and a second ask that fails changes nothing the page drew from the first. On a running
// build at 8caa4357 an upload to LAY-2 made the page read its results and coverage again; with
// both refused, the view drew "Could not load the statements." over the 13 statements it still
// drew, and every mark went (review 5928280634 on pull request 4234, the reviewer's finding b).
test("a read that fails again over the answer the page holds keeps every mark and says nothing failed", async () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1", { children: [checkNode(passed)] })]);
  const { client, read, view } = drawBody(tree, [], { checks: [passed] });
  try {
    await waitFor(() =>
      expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("met")
    );
    read.results.mockRejectedValue(new Error("offline"));
    read.coverage.mockRejectedValue(new Error("offline"));
    read.treeStands("blocks", "failed");
    await act(async () => {
      await client.invalidateQueries();
    });
    await waitFor(() =>
      expect(client.getQueryState(issueResultsQuery(KEY).queryKey)?.status).toBe("error")
    );
    expect(
      rowOf(view, "spec#b-r1").getAttribute("data-spec-mark"),
      "a failed read again over the held results drops the statement's mark"
    ).toBe("met");
    expect(view.container.textContent).not.toContain(STATE_LINES.spec_results_unreadable);
    expect(view.container.textContent).not.toContain(STATE_LINES.statements_unreadable);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// The architect's decision 98 on R35 and R5: an issue that holds no statement says so while its
// coverage and blocks are asked again and when that ask fails, and a cell that says no decision
// shaped its statement still says so when the Spec view read fails again: only a read that failed
// with nothing held says it failed (review 5928280634 on pull request 4234, the reviewer's
// finding b, which named the empty line too). The Spec view is read again as the page reads it
// after a change of the issue, with its answer kept while the read is out
// (`subscribeTreeFreshness`), so the cell stands on the answer the body last held.
test("a read that fails again over the statements and decisions the page holds keeps the empty line and the cells, and says nothing failed", async () => {
  const empty = drawBody(issueTree([]));
  try {
    expect(empty.view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
      EMPTY_STATE_LINES.spec
    );
    empty.read.coverage.mockRejectedValue(new Error("offline"));
    empty.read.treeStands("blocks", "failed");
    await act(async () => {
      await empty.client.invalidateQueries();
    });
    await waitFor(() =>
      expect(empty.client.getQueryState(issueCoverageQuery(KEY).queryKey)?.status).toBe("error")
    );
    expect(
      empty.view.container.querySelector("[data-spec-empty]")?.textContent,
      "a failed read again over the held coverage takes the empty line away"
    ).toBe(EMPTY_STATE_LINES.spec);
    expect(empty.view.container.textContent).not.toContain(STATE_LINES.statements_unreadable);
  } finally {
    empty.view.unmount();
    empty.read.mockRestore();
  }
  const { client, read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(view.container.querySelector("[data-spec-nothing]")).not.toBeNull();
    read.treeStands("specView", "failed");
    await act(async () => {
      await client.resetQueries({ exact: true, queryKey: viewKey(KEY, "spec") });
    });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    expect(client.getQueryData(viewKey(KEY, "spec"))).toBeUndefined();
    expect(
      view.container.querySelector("[data-spec-nothing]"),
      "a failed read again over the held Spec view takes the cell's words away"
    ).not.toBeNull();
    expect(view.container.textContent).not.toContain(STATE_LINES.statements_unreadable);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R6 and R17: the error line a failed read of the Verification document's blocks draws reads
// those blocks again, and the statement takes its mark once they hold an answer.
test("Retry over a failed read of the Verification document's blocks reads them again and marks the statement", async () => {
  const user = userEvent.setup();
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1", { children: [checkNode(passed)] })]);
  const { read, view } = drawBody(tree, [], { checks: [passed], verification: "failed" });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.spec_results_unreadable));
    expect(rowOf(view, "spec#b-r1").hasAttribute("data-spec-mark")).toBe(false);
    read.treeStands("blocks", "answered");
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("met")
    );
    expect(view.container.textContent).not.toContain(STATE_LINES.spec_results_unreadable);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

/** Lays every element out `height` px tall until the answer is called, as
 *  `NodeRow.drawn.test.tsx` does: no DOM of a unit test lays anything out, and a row's cut reads
 *  its words' own boxes (`measureClamp`), so a case that shows a statement or a cell drawn whole
 *  lays its words out taller than a cut at six lines would leave them. */
function layOut(height: number): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get: () => height,
  });
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 0, 600, height)
  );
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
    box.mockRestore();
  };
}

// Review 5928280634 on pull request 4234 (thread 4592007127): a press focuses what it lands on as
// it goes down, so a statement that took the focus for a link inside it at once would draw itself
// focused under the pointer while the statement that had the focus lost its look, and the press
// could end on something else and follow nothing. The focus a press gives a statement is taken
// once the press is over, as the outline takes a row's (`IssueTree.tsx`'s `focusRow`). happy-dom
// lays nothing out, so this reads the order: nothing the body draws changes while the press is
// held, and then the pressed statement takes the focus.
test("a press on a link inside a statement leaves every statement as drawn until it ends, and then the statement takes the focus", async () => {
  const user = userEvent.setup();
  const linked = "**R2.** Read [the guide](https://example.com/guide) first.";
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2", { text: linked, tree: displayTree(linked) })])
  );
  const looksFocused = (id: string) => rowOf(view, id).className.includes(pageFocusedRow);
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    expect(looksFocused("spec#b-r1")).toBe(true);
    const pressed = rowOf(view, "spec#b-r2");
    const link = await waitFor(() => {
      const found = pressed.querySelector<HTMLElement>("[data-node-words] a");
      if (found === null) throw new Error("the statement draws no link in its words");
      return found;
    });
    // What is read is who holds the focus, not where the link leads.
    link.addEventListener("click", (event) => event.preventDefault());

    await user.pointer({ keys: "[MouseLeft>]", target: link });
    expect(document.activeElement, "the press did not focus the link it went down on").toBe(link);
    expect(looksFocused("spec#b-r2"), "the held press drew the pressed statement focused").toBe(
      false
    );
    expect(
      looksFocused("spec#b-r1"),
      "the held press took the focused look from the statement that had it"
    ).toBe(true);
    expect(focusedNodeIn(view).id, "the held press moved Details").toBe("spec#b-r1");

    await user.pointer({ keys: "[/MouseLeft]" });
    await waitFor(() => expect(looksFocused("spec#b-r2")).toBe(true));
    expect(looksFocused("spec#b-r1")).toBe(false);
    expect(focusedNodeIn(view)).toEqual({ id: "spec#b-r2", type: "requirement" });
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

/** A marker for the editor on the real document stage, without opening a document room. */
function specEditor(): ReactNode {
  return (
    <DocumentStage artifactId="artifact-spec">
      <div data-test-spec-editor="" tabIndex={-1} />
    </DocumentStage>
  );
}

test("Enter on a statement puts the caret in its words through the outline's editing", async () => {
  const user = userEvent.setup();
  const onEditing = mock<OnEditing>(() => {});
  const { read, view } = drawBody(
    issueTree([statement("r1", { gaps: ["shaped_disagreement"] }), statement("r2")]),
    [],
    { editor: specEditor(), onEditing }
  );
  try {
    const row = rowOf(view, "spec#b-r1");
    const words = row.querySelector("[data-node-words]");
    const original = "The page draws r1.";
    expect(words?.textContent).toContain(original);
    expect(words?.querySelector("[data-node-trail]")).not.toBeNull();
    expect(row).toHaveAttribute("data-editable");
    act(() => row.focus());
    await user.keyboard("{Enter}");

    expect(locationOf(view)).toBe("/");
    expect(onEditing).toHaveBeenCalledWith("artifact-spec", expect.any(Function));
    const editor = words?.querySelector<HTMLElement>("[data-test-spec-editor]");
    expect(editor).toBeInTheDocument();
    expect(words?.textContent).not.toContain(original);
    expect(words?.querySelector("[data-node-trail]")).toBeNull();
    expect(row.querySelector("[data-node-trail]")).toBeInTheDocument();
    expect(rowOf(view, "spec#b-r2").textContent).toContain("The page draws r2.");

    act(() => editor?.focus());
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(row);
    expect(editor).not.toBeInTheDocument();
    expect(words?.textContent).toContain(original);
    expect(onEditing.mock.calls.at(-1)?.[0]).toBeUndefined();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

test("a press on a statement's words puts the caret in them, and a press on a link in them follows the link", async () => {
  const user = userEvent.setup();
  const onEditing = mock<OnEditing>(() => {});
  const destination = "/settings";
  const linked = `Read [the settings page](${destination}) first.`;
  const { read, view } = drawBody(
    issueTree([statement("r1", { text: linked, tree: displayTree(linked) }), statement("r2")]),
    [],
    { editor: specEditor(), onEditing }
  );
  try {
    const row = rowOf(view, "spec#b-r1");
    const link = await within(row).findByRole("link", { name: "the settings page" });
    const words = link.closest<HTMLElement>("[data-node-words]");
    if (words === null) throw new Error("the link is not in the statement's words");
    act(() => row.focus());
    onEditing.mockClear();
    await user.click(link);
    expect(locationOf(view)).toBe(destination);
    expect(onEditing).not.toHaveBeenCalled();
    expect(link).toBeInTheDocument();

    // A press on a row that already holds focus must work without a new focus event.
    act(() => row.focus());
    await user.pointer({ keys: "[MouseLeft>]", target: words });
    expect(onEditing).not.toHaveBeenCalled();
    expect(link).toBeInTheDocument();
    await user.pointer({ keys: "[/MouseLeft]" });
    await waitFor(() =>
      expect(onEditing).toHaveBeenCalledWith("artifact-spec", expect.any(Function))
    );
    expect(words.querySelector("[data-test-spec-editor]")).toBeInTheDocument();
    expect(link).not.toBeInTheDocument();
    expect(locationOf(view)).toBe(destination);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

test("a press on a statement's words passes its point to the outline's editing", async () => {
  const user = userEvent.setup();
  // The hold goes on a ProseMirror view; a plain editable on the stage stands in for its drawing.
  const editorView = new EditorView(null, {
    state: EditorState.create({
      schema: new Schema({ nodes: { doc: { content: "text*" }, text: {} } }),
    }),
  });
  const onEditing: OnEditing = (artifactId, onEditor) => {
    if (artifactId !== undefined) onEditor?.({ view: editorView });
  };
  const editor = (
    <DocumentStage artifactId="artifact-spec">
      <div contentEditable suppressContentEditableWarning>
        <section data-block-id="r1">
          <header contentEditable={false}>Requirement</header>
          <p>The page draws r1.</p>
        </section>
      </div>
    </DocumentStage>
  );
  const { read, view } = drawBody(
    issueTree([statement("r1", { text: "The page draws r1." })]),
    [],
    {
      editor,
      onEditing,
    }
  );
  const original = Object.getOwnPropertyDescriptor(document, "caretPositionFromPoint");
  try {
    const row = rowOf(view, "spec#b-r1");
    const words = within(row).getByText("The page draws r1.");
    const point = mock(() => ({ offsetNode: words.firstChild, offset: 6 }));
    Object.defineProperty(document, "caretPositionFromPoint", { configurable: true, value: point });
    const caretIsAt = (offset: number) =>
      waitFor(() => {
        const text = row.querySelector('[data-block-id="r1"] p')?.firstChild;
        expect(text).toBeDefined();
        expect(window.getSelection()?.anchorNode).toBe(text);
        expect(window.getSelection()?.anchorOffset).toBe(offset);
      });
    await user.pointer({ keys: "[MouseLeft]", target: words, coords: { x: 72, y: 153 } });
    await caretIsAt(6);
    expect(point).toHaveBeenCalledWith(72, 153);

    await user.keyboard("{Escape}");
    expect(row).toHaveFocus();
    point.mockClear();
    await user.keyboard("{Enter}");
    await caretIsAt(0);
    expect(point).not.toHaveBeenCalled();
  } finally {
    if (original === undefined) Reflect.deleteProperty(document, "caretPositionFromPoint");
    else Object.defineProperty(document, "caretPositionFromPoint", original);
    view.unmount();
    editorView.destroy();
    read.mockRestore();
  }
});

// A press that drags across a statement's words selects them, as on any words of the page, and
// the click that ends it on the same words opens no editor in their place: the selection stays,
// for the reader to copy.
test("a drag that selects a statement's words keeps the selection and opens no editor", async () => {
  const user = userEvent.setup();
  const onEditing = mock<OnEditing>(() => {});
  const { read, view } = drawBody(
    issueTree([statement("r1", { text: "The page draws r1." }), statement("r2")]),
    [],
    { editor: specEditor(), onEditing }
  );
  try {
    const row = rowOf(view, "spec#b-r1");
    const words = within(row).getByText("The page draws r1.");
    await user.pointer([
      { keys: "[MouseLeft>]", offset: 0, target: words },
      { offset: 8, target: words },
      { keys: "[/MouseLeft]", target: words },
    ]);
    expect(window.getSelection()?.toString(), "the drag selected no words").toBe("The page");
    // The task an editor the press opened would open in.
    await act(async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
    expect(onEditing, "the drag opened the editor").not.toHaveBeenCalledWith(
      "artifact-spec",
      expect.any(Function)
    );
    expect(row.querySelector("[data-test-spec-editor]")).toBeNull();
    expect(window.getSelection()?.toString(), "the drag's selection was lost").toBe("The page");
    expect(row).toHaveFocus();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R8: a press is counted in the editor's text. The view draws a reference to an issue by the
// issue's key and title, while the editor holds the reference as it is written, hidden under a
// title of its own that takes no caret (`@legion/proof-editor`'s `reference-decorations.ts`), so a
// press after the reference counts it as written. A plain editable on the stage stands in for
// the editor's drawing of the statement: its title out of the editable text, the reference as
// written hidden in it. Counted as drawn, the press on "first" would put the caret 8 characters
// into the words after the reference rather than 3.
test("a press after a reference counts the reference as the editor holds it", async () => {
  const cited = "dispatch://CORE-2";
  const after = " first, then the rest.";
  const text = `Read ${cited}${after}`;
  await pressAfterKind({
    drawn: (words) => {
      const link = words.querySelector(`a[data-row-reference="${cited}"]`);
      expect(link?.textContent).toBe("CORE-2 The other issue");
      expect(link?.nextSibling?.textContent).toBe(after);
    },
    held: (
      <>
        {"Read "}
        <span contentEditable={false}>CORE-2 The other issue</span>
        <span hidden>{cited}</span>
        {after}
      </>
    ),
    into: 3,
    names: new Map([[cited, "The other issue"]]),
    pressed: after,
    text,
    tree: displayTree(text),
  });
});

/**
 * Presses statement r1's drawn text `pressed`, `into` characters into it, and waits for the caret
 * to stand `at` characters into `lands`, the last text of the statement's paragraph in its
 * editor; both default to the press's own, for words the two draw alike after the kind a case
 * reads. The statement's words are `text`, drawn from `tree`, the reading the server sends,
 * where one is given, and as stored where none is, with `names` as the page's batched read named
 * its references. A plain editable on the stage stands in for the editor's drawing of the
 * statement: `held` is its paragraph, each kind drawn as the editor draws it. `drawn` reads the
 * statement's drawn words before the press.
 */
async function pressAfterKind({
  at,
  drawn,
  held,
  into,
  lands,
  names,
  pressed,
  text,
  tree,
}: {
  at?: number;
  drawn: (words: HTMLElement) => void;
  held: ReactNode;
  into: number;
  lands?: string;
  names?: ReadonlyMap<string, string>;
  pressed: string;
  text: string;
  tree?: TreeNode["tree"];
}): Promise<void> {
  const user = userEvent.setup();
  const editorView = new EditorView(null, {
    state: EditorState.create({
      schema: new Schema({ nodes: { doc: { content: "text*" }, text: {} } }),
    }),
  });
  const onEditing: OnEditing = (artifactId, onEditor) => {
    if (artifactId !== undefined) onEditor?.({ view: editorView });
  };
  const editor = (
    <DocumentStage artifactId="artifact-spec">
      <div contentEditable suppressContentEditableWarning>
        <section data-block-id="r1">
          <p>{held}</p>
        </section>
      </div>
    </DocumentStage>
  );
  const { read, view } = drawBody(issueTree([statement("r1", { text, tree })]), [], {
    editor,
    names,
    onEditing,
  });
  const original = Object.getOwnPropertyDescriptor(document, "caretPositionFromPoint");
  try {
    const row = rowOf(view, "spec#b-r1");
    const words = await waitFor(() => {
      const found = row.querySelector<HTMLElement>("[data-node-words]");
      if (found === null) throw new Error("the statement draws no words");
      drawn(found);
      return found;
    });
    const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
    let node = walker.nextNode();
    while (node !== null && node.textContent !== pressed) node = walker.nextNode();
    if (node === null || node.parentElement === null) {
      throw new Error(`the statement draws no text "${pressed}"`);
    }
    const point = mock(() => ({ offsetNode: node, offset: into }));
    Object.defineProperty(document, "caretPositionFromPoint", { configurable: true, value: point });
    await user.pointer({ keys: "[MouseLeft]", target: node.parentElement });
    await waitFor(() => {
      const last = row.querySelector('[data-block-id="r1"] p')?.lastChild;
      expect(last?.textContent).toBe(lands ?? pressed);
      expect(window.getSelection()?.anchorNode).toBe(last);
      expect(window.getSelection()?.anchorOffset).toBe(at ?? into);
    });
  } finally {
    if (original === undefined) Reflect.deleteProperty(document, "caretPositionFromPoint");
    else Object.defineProperty(document, "caretPositionFromPoint", original);
    view.unmount();
    editorView.destroy();
    read.mockRestore();
  }
}

/** Statement r1's words for `pressAfterKind`: a placeholder in inline HTML, `<name>`, and the
 *  words after it, drawn from the server's reading and held by the editor as an atom. */
function withInlineHtml() {
  const after = " first, then the rest.";
  const text = `Kept by <name>${after}`;
  return {
    drawn: (words: HTMLElement) => {
      expect(words.querySelector('[data-type="html"]')?.textContent).toBe("<name>");
    },
    held: (
      <>
        {"Kept by "}
        <span contentEditable={false} data-type="html">
          {"<name>"}
        </span>
        {after}
      </>
    ),
    pressed: after,
    text,
    tree: displayTree(text),
  };
}

// Inline HTML, such as a placeholder `<name>`, is drawn as its characters, while the editor holds
// it as an atom the caret cannot enter, so a press after it counts it as nothing. Counted as
// drawn, the press on "first," would put the caret 6 characters on, in "then": the word before
// it is `<name>` in the view and "by" in the editor, so the word pressed is found nowhere to set
// the count right.
test("a press after inline html lands in the word pressed", async () => {
  await pressAfterKind({ ...withInlineHtml(), into: 3 });
});

// The atom holds no text, so the end of the text before it and the start of the text after it
// have one count. A press at the start of the text after it - on the left half of its first
// character - puts the caret there, after the atom, and not before it.
test("a press at the start of the text after inline html lands after it", async () => {
  await pressAfterKind({ ...withInlineHtml(), into: 0 });
});

// A mention of a block is drawn as a chip by the block's words, while the editor holds the
// mention as it is written, hidden under a title of its own that takes no caret, so a press after
// it counts it as written, as a reference to an issue counts.
test("a press after a mention lands in the word pressed", async () => {
  const cited = `dispatch://${KEY}/spec#b-r2`;
  const after = " first, then the rest.";
  const text = `Read ${cited}${after}`;
  await pressAfterKind({
    drawn: (words) => {
      const chip = words.querySelector(`a[data-row-reference="${cited}"]`);
      expect(chip).toHaveAttribute("data-row-chip");
      expect(chip?.textContent).not.toBe(cited);
    },
    held: (
      <>
        {"Read "}
        <span contentEditable={false}>The other statement</span>
        <span hidden>{cited}</span>
        {after}
      </>
    ),
    into: 3,
    names: new Map([[cited, "The other statement"]]),
    pressed: after,
    text,
    tree: displayTree(text),
  });
});

// A picture of an artifact is drawn by its caption's link until its picture loads, while the
// editor holds it as an atom the caret cannot enter, its own fallback link inside it, so a press
// after it counts it as nothing. The summary that names its picture never answers here.
test("a press after an image lands in the word pressed", async () => {
  const after = " first, then the rest.";
  const text = `See ![shot.png](dispatch://${KEY}/artifact/shot-png)${after}`;
  const summary = spyOn(api, "getIssueSummary").mockReturnValue(new Promise(() => {}));
  try {
    await pressAfterKind({
      drawn: (words) => {
        expect(words.querySelector("[data-dispatch-picture]")?.textContent).toBe("shot.png");
      },
      held: (
        <>
          {"See "}
          <span contentEditable={false} data-dispatch-doc-picture="">
            <a href="/">{`${KEY}/artifact/shot-png`}</a>
          </span>
          {after}
        </>
      ),
      into: 3,
      pressed: after,
      text,
      tree: displayTree(text),
    });
  } finally {
    summary.mockRestore();
  }
});

/** Statement r1's words for `pressAfterKind` with a hard break between two lines, drawn from the
 *  server's reading and held by the editor as a line break. */
function withHardBreak() {
  const after = "Line two, then the rest.";
  const text = `Line one.\\\n${after}`;
  return {
    drawn: (words: HTMLElement) => {
      expect(words.querySelector("br")).not.toBeNull();
    },
    held: (
      <>
        {"Line one."}
        <br />
        {after}
      </>
    ),
    pressed: after,
    text,
    tree: displayTree(text),
  };
}

// A hard break is a line break in both, which holds no text, so a press after it counts nothing
// for it.
test("a press after a hard break lands in the word pressed", async () => {
  await pressAfterKind({ ...withHardBreak(), into: 6 });
});

// The break holds no text, so the end of the line before it and the start of the line after it
// have one count. A press at the start of the line after it - on the left half of its first
// character - puts the caret there, after the break, and not at the end of the line before.
test("a press at the start of the text after a hard break lands after the break", async () => {
  await pressAfterKind({ ...withHardBreak(), into: 0 });
});

// A footnote's reference is drawn as its label, while the editor holds it as an atom the caret
// cannot enter, so a press after it counts it as nothing.
test("a press after a footnote reference lands in the word pressed", async () => {
  const after = " first, then the rest.";
  const text = `Read the note[^1]${after}\n\n[^1]: The note.`;
  await pressAfterKind({
    drawn: (words) => {
      expect(words.querySelector('[data-type="footnote_reference"]')?.textContent).toBe("1");
    },
    held: (
      <>
        {"Read the note"}
        <sup contentEditable={false} data-type="footnote_reference">
          1
        </sup>
        {after}
      </>
    ),
    into: 3,
    pressed: after,
    text,
    tree: displayTree(text),
  });
});

// A statement whose reading draws a token as plain characters, with no mark of what the editor
// holds there, is an inline kind the count leaves uncounted: the editor holds `[atom]` as an atom
// the caret cannot enter, so the count of the press runs on by its 6 characters, to the end of
// "the". The word pressed is found with the words beside it where the editor holds them, and the
// caret goes as far into it.
test("an uncounted inline kind still puts the caret in the pressed word", async () => {
  const after = " and then the rest.";
  const text = `Kept by [atom]${after}`;
  await pressAfterKind({
    at: 7,
    drawn: (words) => {
      expect(
        words.querySelector(
          '[data-row-written], [data-type="html"], [data-type="footnote_reference"], [data-dispatch-picture]'
        )
      ).toBeNull();
      expect(words.textContent).toContain(text);
    },
    held: (
      <>
        {"Kept by "}
        <span contentEditable={false}>{"[atom]"}</span>
        {after}
      </>
    ),
    into: 21,
    lands: after,
    pressed: text,
    text,
    tree: {
      content: [{ content: [{ text, type: "text" }], type: "paragraph" }],
      type: "doc",
    },
  });
});

// An atom that ends a statement's block holds no text, so the editor's text ends before it. A
// press past the atom (`pressInWords`'s `next`) puts the caret after it, in its paragraph, where
// the block ends, and a press inside it puts the caret before it, at the end of the text before
// it. A plain editable stands in for the editor: each atom drawn `contenteditable="false"`, then
// the separator and the break ProseMirror draws after an atom that ends a paragraph. The picture
// has loaded, so it holds no text.
test("a press past an atom that ends a statement's block puts the caret after it, and one inside it before it", () => {
  const host = document.createElement("div");
  document.body.append(host);
  const at: CaretAt = { x: 1, y: 2, offset: "Read the note".length, word: ["the ", "note", "", 4] };
  try {
    for (const atom of [
      '<span contenteditable="false" data-type="html">&lt;name&gt;</span>',
      '<sup contenteditable="false" data-type="footnote_reference">1</sup>',
      '<span contenteditable="false" data-dispatch-doc-picture=""><img alt="shot.png"></span>',
    ]) {
      host.innerHTML = `<div contenteditable="true"><section data-block-id="r1"><p>Read the note${atom}<img class="ProseMirror-separator" alt=""><br class="ProseMirror-trailingBreak"></p></section></div>`;
      const paragraph = host.querySelector("p");
      expect(caretInBlock(host, "r1", { ...at, next: true }), atom).toBe(true);
      expect(window.getSelection()?.anchorNode, atom).toBe(paragraph);
      expect(window.getSelection()?.anchorOffset, atom).toBe(2);
      expect(caretInBlock(host, "r1", at), atom).toBe(true);
      expect(window.getSelection()?.anchorNode, atom).toBe(paragraph?.firstChild);
      expect(window.getSelection()?.anchorOffset, atom).toBe("Read the note".length);
    }
  } finally {
    host.remove();
  }
});

// The same order on a decision's cell: a press on the cell draws the person's whole answer only
// once it ends, and the statement that had the focus keeps its look until then; a press on that
// statement keeps the whole answer drawn until it ends, and then the statement takes the focus.
test("a press on a decision's cell or away from it changes what the body draws only once it ends", async () => {
  const stored = "Keep the memo in the pane.\n\n  It reads better there -  really.";
  const user = userEvent.setup();
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { text: stored }),
      statement("r1"),
      decisionNode("d1", {
        chips: [{ answered: true, gone: false, reading: { words: "r1" }, ref: statementRef("r1") }],
      }),
    ]),
    [
      decision("d1", {
        decision: {
          asks: ["ask-1"],
          kind: "person",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ]
  );
  const whole = () => view.container.querySelector("[data-spec-whole-answer]")?.textContent;
  const looksFocused = () => rowOf(view, "spec#b-r1").className.includes(pageFocusedRow);
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    const cell = view.container.querySelector<HTMLElement>("[data-spec-cell]");
    if (cell === null) throw new Error("the body draws no cell");

    await user.pointer({ keys: "[MouseLeft>]", target: cell });
    expect(document.activeElement, "the press did not focus the cell it went down on").toBe(cell);
    expect(whole(), "the held press drew the whole answer").toBeUndefined();
    expect(looksFocused(), "the held press took the statement's focused look").toBe(true);
    await user.pointer({ keys: "[/MouseLeft]" });
    await waitFor(() => expect(whole()).toBe(stored));
    expect(looksFocused()).toBe(false);

    await user.pointer({ keys: "[MouseLeft>]", target: rowOf(view, "spec#b-r1") });
    expect(document.activeElement).toBe(rowOf(view, "spec#b-r1"));
    expect(whole(), "the held press on a statement closed the cell's whole answer").toBe(stored);
    expect(looksFocused(), "the held press drew the statement focused").toBe(false);
    await user.pointer({ keys: "[/MouseLeft]" });
    await waitFor(() => expect(looksFocused()).toBe(true));
    expect(whole()).toBeUndefined();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R11: the focused row is never under a sticky header. The view's column
// heads stay at the top under the band, and the page's rule for placing a focused row clears the
// band alone (`bringOnScreen`), so a statement the keys moved to stood with its first line under
// the heads (the tester's reading of row (7) of `issue-page-j.e2e.ts` on a running build at
// 8caa4357). happy-dom lays nothing out, so the boxes are given: the heads from 0 to 31 px, a
// statement at 27 px and a cell's words at 10 px, each on screen and clear of the band; each one
// a key moves the focus to is scrolled down below the heads, by the rule's 8 px.
test("a statement or a cell a key moves the focus to is scrolled down from under the column heads", async () => {
  const user = userEvent.setup();
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    if (this.hasAttribute("data-spec-head")) return new DOMRect(0, 0, 1000, 31);
    if (this.dataset.nodeId === "spec#b-r2") return new DOMRect(0, 27, 600, 48);
    if (this.parentElement?.hasAttribute("data-spec-cell") === true && this.tagName === "DIV") {
      return new DOMRect(0, 10, 280, 120);
    }
    return new DOMRect(0, 200, 600, 48);
  });
  const scrolls = spyOn(window, "scrollBy").mockImplementation(() => {});
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r2"))],
        },
      }),
    ]
  );
  // `scrollBy`'s first overload takes a number; the body calls the one that takes options.
  const scrolledBy = (): unknown[] => scrolls.mock.calls.map((call): unknown => call[0]);
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    expect(scrolledBy(), "a statement clear of the heads was scrolled").toEqual([]);
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(rowOf(view, "spec#b-r2"));
    expect(scrolledBy(), "the statement stays under the heads").toContainEqual({
      top: 27 - 31 - 8,
    });
    scrolls.mockClear();
    await user.keyboard("w");
    expect(document.activeElement?.getAttribute("data-decision-id")).toBe("decisions-md#b-d1");
    expect(scrolledBy(), "the cell's words stay under the heads").toContainEqual({
      top: 10 - 31 - 8,
    });
  } finally {
    view.unmount();
    read.mockRestore();
    scrolls.mockRestore();
    boxes.mockRestore();
  }
});

/** The page a hold case reads, which happy-dom does not lay out: its size observers, which the
 *  case calls by hand (`resize`); its boxes - the column heads from 0 to 31 px, each element the
 *  case's own `boxOf` gives a box, and every other at 200 px; and `window.scrollBy`, which calls
 *  the case's optional `scroll` and records each call (`scrolledBy`). `restore` gives each back. */
function layOutPage(
  boxOf: (element: HTMLElement) => DOMRect | undefined,
  scroll?: (delta: number) => void
) {
  const sizes = installResizeObserver();
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    if (this.hasAttribute("data-spec-head")) return new DOMRect(0, 0, 1000, 31);
    return boxOf(this) ?? new DOMRect(0, 200, 600, 48);
  });
  const scrolls = spyOn(window, "scrollBy").mockImplementation(
    (options?: number | ScrollToOptions) => {
      if (typeof options === "object") scroll?.(options.top ?? 0);
    }
  );
  return {
    resize: sizes.resize,
    restore: () => {
      scrolls.mockRestore();
      boxes.mockRestore();
      sizes.restore();
    },
    // `scrollBy`'s first overload takes a number; the body calls the one that takes options.
    scrolledBy: (): unknown[] => scrolls.mock.calls.map((call): unknown => call[0]),
  };
}

test("a drag over an unfocused statement's words gives it the focus and moves no page", async () => {
  const user = userEvent.setup();
  const page = layOutPage((element) =>
    element.dataset.nodeId === "spec#b-r2" && element.hasAttribute("data-spec-statement")
      ? new DOMRect(0, -200, 600, 3 * window.innerHeight)
      : undefined
  );
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2", { text: "The page draws r2." })])
  );
  try {
    const first = rowOf(view, "spec#b-r1");
    const tall = rowOf(view, "spec#b-r2");
    const words = within(tall).getByText("The page draws r2.");
    act(() => first.focus());
    expect(tall).not.toHaveFocus();

    await user.pointer([
      { keys: "[MouseLeft>]", offset: 0, target: words },
      { offset: 8, target: words },
      { keys: "[/MouseLeft]", target: words },
    ]);
    await waitFor(() => expect(tall.className).toContain(pageFocusedRow));
    expect(tall).toHaveFocus();
    expect(window.getSelection()?.toString()).toBe("The page");
    page.resize(tall);
    page.resize(document.body);
    expect(page.scrolledBy(), "the press placed the tall statement").toEqual([]);

    await user.keyboard("{ArrowUp}");
    expect(first).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(tall).toHaveFocus();
    expect(page.scrolledBy(), "the later key did not place the tall statement").toContainEqual({
      top: -200 - 31 - 8,
    });
  } finally {
    view.unmount();
    read.mockRestore();
    page.restore();
  }
});

// A key or a press holds the item it leaves the focus in where it stands while the page grows
// (`holdWhereItStands`), and does not bring it on screen again by the page's rule, which puts an
// item taller than the screen at its top. A decision cell is as tall as its whole run of
// statements, and only its words stay at the top of the screen: deep in a run taller than the
// screen, a hold that brought the cell on screen would take the page to the top of the run at a
// press on those words, and the statement the reader was reading would leave the screen.
// happy-dom lays nothing out, so the boxes are given - the heads from 0 to 31 px, the cell from
// two screens above the window to two below it, its words at 75 px, and every statement on
// screen - and the page's size observers are called by hand, as the browser calls them as they
// start and as the page grows.
test("a press on a decision cell's words deep in a run taller than the screen moves no page", async () => {
  const user = userEvent.setup();
  const page = layOutPage((element) => {
    if (element.hasAttribute("data-spec-cell")) {
      return new DOMRect(0, -2 * window.innerHeight, 280, 4 * window.innerHeight);
    }
    if (element.hasAttribute("data-spec-cell-words")) return new DOMRect(0, 75, 280, 120);
    return undefined;
  });
  const run = ["r1", "r2", "r3"];
  const { read, view } = drawBody(
    issueTree([...run.map((block) => statement(block)), decisionNode("d1")]),
    [
      decision("d1", {
        decision: { kind: "agent", links: run.map((block) => shaped(statementRef(block))) },
      }),
    ]
  );
  try {
    act(() => rowOf(view, "spec#b-r2").focus());
    const cell = view.container.querySelector<HTMLElement>("[data-spec-cell]");
    const words = cell?.querySelector<HTMLElement>("[data-spec-quote]");
    if (cell == null || words == null) throw new Error("the body draws no decision's words");
    await user.click(words);
    await waitFor(() => expect(document.activeElement).toBe(cell));
    // The observers' first call, in the frame after they start, and the page growing.
    page.resize(document.body);
    page.resize(document.body);
    expect(page.scrolledBy(), "the press moved the page").toEqual([]);
  } finally {
    view.unmount();
    read.mockRestore();
    page.restore();
  }
});

// The same hold, of a statement taller than the screen that the reader has read on into: `f`
// moves the focus to a link of its words on screen, and Escape back to the statement, and the page
// stays where the reader had it. A hold that brought the statement on screen would take the page
// to its top at each key. The statement is given a box on screen as the focus comes to it, which
// the page's rule leaves where it is, and then one from a screen above the window to two below it.
test("a key that moves the focus to a statement taller than the screen moves no page", async () => {
  const user = userEvent.setup();
  let readOn = false;
  const page = layOutPage((element) =>
    readOn && element.dataset.nodeId === "spec#b-r2" && element.hasAttribute("data-spec-statement")
      ? new DOMRect(0, -window.innerHeight, 600, 3 * window.innerHeight)
      : undefined
  );
  const linked = "**R2.** Read [the guide](https://example.com/guide) first.";
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2", { text: linked, tree: displayTree(linked) })])
  );
  try {
    const tall = rowOf(view, "spec#b-r2");
    const link = await waitFor(() => {
      const found = tall.querySelector<HTMLElement>("[data-node-words] a");
      if (found === null) throw new Error("the statement draws no link in its words");
      return found;
    });
    act(() => tall.focus());
    readOn = true;
    await user.keyboard("f");
    expect(document.activeElement, "f did not reach the link in the statement's words").toBe(link);
    page.resize(document.body);
    page.resize(document.body);
    await user.keyboard("{Escape}");
    expect(document.activeElement, "Escape did not give the statement back").toBe(tall);
    page.resize(document.body);
    page.resize(document.body);
    expect(page.scrolledBy(), "a key moved the page").toEqual([]);
  } finally {
    view.unmount();
    read.mockRestore();
    page.restore();
  }
});

// Rows drawn above the statement a key moved the focus to push it down the page, and the hold
// scrolls the page by as far, so the statement stands where it stood (`holdWhereItStands`).
// happy-dom lays nothing out, so the boxes are given: the heads from 0 to 31 px, each statement on
// screen, and the statement held 120 px further down once the page has grown.
test("a page that grows above a held statement keeps the statement where it stands", async () => {
  const user = userEvent.setup();
  let top = 200;
  const page = layOutPage((element) =>
    element.dataset.nodeId === "spec#b-r2" && element.hasAttribute("data-spec-statement")
      ? new DOMRect(0, top, 600, 48)
      : undefined
  );
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(rowOf(view, "spec#b-r2"));
    top += 120;
    page.resize(document.body);
    expect(
      page.scrolledBy(),
      "the hold left the statement where the page's growth pushed it"
    ).toEqual([{ top: 120 }]);
  } finally {
    view.unmount();
    read.mockRestore();
    page.restore();
  }
});

test("a hold keeps the row where the reader left it when the page scrolls it of its own accord", async () => {
  const user = userEvent.setup();
  let contentTop = 200;
  let scroll = 0;
  let limit = 95;
  const page = layOutPage(
    (element) =>
      element.dataset.nodeId === "spec#b-r2"
        ? new DOMRect(0, contentTop - scroll, 600, 48)
        : undefined,
    (delta) => {
      scroll = Math.min(scroll + delta, limit);
    }
  );
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    await user.keyboard("{ArrowDown}");
    expect(rowOf(view, "spec#b-r2")).toHaveFocus();
    page.resize(document.body);

    // The page scrolls without a reader act, then draws more content above the row.
    scroll = 50;
    act(() => window.dispatchEvent(new Event("scroll")));
    contentTop += 120;
    page.resize(document.body);
    expect(page.scrolledBy(), "the page's scroll changed the target of the hold").toEqual([
      { top: 70 },
    ]);
    expect(contentTop - scroll).toBe(225);

    // The first correction hit the end of the page. More content below the row gives it room.
    limit = 200;
    page.resize(document.body);
    expect(contentTop - scroll).toBe(200);
    expect(page.scrolledBy()).toEqual([{ top: 70 }, { top: 25 }]);
  } finally {
    view.unmount();
    read.mockRestore();
    page.restore();
  }
});

// The focus task after a press places a statement below the heads. The hold uses the top after
// that placing, and a later resize corrects only the movement from there.
test("a placing after the press counts as where the held statement stands", async () => {
  const user = userEvent.setup();
  let top = 15;
  const page = layOutPage(
    (element) =>
      element.dataset.nodeId === "spec#b-r2" && element.hasAttribute("data-spec-statement")
        ? new DOMRect(0, top, 600, 48)
        : undefined,
    (delta) => {
      top -= delta;
    }
  );
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    const pressed = rowOf(view, "spec#b-r2");
    // The statement's bullet, outside its words, so the press holds the statement rather than
    // opening its editor.
    const bullet = pressed.querySelector<HTMLElement>(":scope > span");
    if (bullet === null) throw new Error("the statement draws no bullet");
    await user.click(bullet);
    await waitFor(() => expect(pressed.className).toContain(pageFocusedRow));
    expect(top).toBe(39);
    page.resize(document.body);
    expect(page.scrolledBy(), "the hold scrolled the page back over the placing").toEqual([
      { top: -24 },
    ]);
    top += 40;
    page.resize(document.body);
    expect(
      page.scrolledBy(),
      "the hold left the placed statement where the page's growth pushed it"
    ).toEqual([{ top: -24 }, { top: 40 }]);
  } finally {
    view.unmount();
    read.mockRestore();
    page.restore();
  }
});

/** How many times each of the reads the body draws from has been asked for: the tree read
 *  (`blocks`) and the Spec view read (`specView`) by the view each asks `getIssueTree` for. */
function askedOf(read: Reads): Record<string, number> {
  const views = read.tree.mock.calls.map(([, query]) => query?.view);
  return {
    asks: read.asks.mock.calls.length,
    blocks: views.filter((view) => view === undefined).length,
    coverage: read.coverage.mock.calls.length,
    results: read.results.mock.calls.length,
    specView: views.filter((view) => view === "spec").length,
  };
}

// R35's sentence waits on the asks read as well: a person's answer, which their decision is
// drawn by, and the answered question a statement hangs under, which heads its run, both come
// with it. While that read is out or refused the column says nothing of a statement no decision
// it holds shaped.
test("the column says no decision shaped a statement only once the asks are read", async () => {
  for (const asksRead of ["out", "failed"] as const) {
    const { read, view } = drawBody(issueTree([statement("r1")]), [], { asksRead });
    try {
      if (asksRead === "failed")
        await waitFor(() => alertSaying(view, STATE_LINES.asks_unreadable));
      expect(
        textsOf(view, "[data-spec-nothing]"),
        `the column says no decision shaped r1 while the asks read is ${asksRead}`
      ).toEqual([]);
      expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: a refused Spec view read, which the column's decisions come from, says so in the
// statements' line over the tree - the one line for that read, with no decisions' line beside it
// (decision 450) - and its Retry asks that read alone again, after which each run's cell draws
// the decision that shaped it.
test("a refused Spec view read says so in the statements' line, and its Retry asks that read alone again", async () => {
  const user = userEvent.setup();
  const made = [
    decision("d1", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ];
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), made, {
    specView: "failed",
  });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    expect(view.container.querySelectorAll('[role="alert"]')).toHaveLength(1);
    expect(view.container.querySelector("[data-spec-cell][data-decision-id]")).toBeNull();
    read.treeStands("specView", "answered");
    const before = askedOf(read);
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-cell]")?.getAttribute("data-decision-id")
      ).toBe("decisions-md#b-d1")
    );
    expect(askedOf(read)).toEqual({ ...before, specView: before.specView + 1 });
    expect(view.container.querySelector('[role="alert"]')).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A refused read's line stays over the tree while its Retry runs the read again, its Retry busy,
// so the focus that Retry holds stays where it was, and the line leaves once the read answers.
test("a refused read's line stays, its Retry busy, while the read runs again, and leaves once it answers", async () => {
  const user = userEvent.setup();
  const made = [
    decision("d1", { decision: { kind: "agent", links: [shaped(statementRef("r1"))] } }),
  ];
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), made, {
    specView: "failed",
  });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    // The Spec view read's next run is held until the case answers it.
    const answer = Promise.withResolvers<TreeAnswer>();
    read.tree.mockImplementation((_key, query) =>
      query?.view === "spec" ? answer.promise : Promise.resolve(NO_NODES)
    );
    await user.click(within(line).getByRole("button", { name: "Retry" }));
    expect(alertSaying(view, STATE_LINES.statements_unreadable)).toBe(line);
    expect(within(line).getByRole("button", { name: "Retry" }).getAttribute("aria-busy")).toBe(
      "true"
    );
    await act(async () => answer.resolve(specViewAnswer(made)));
    await waitFor(() => expect(view.container.querySelector('[role="alert"]')).toBeNull());
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-cell]")?.getAttribute("data-decision-id")
      ).toBe("decisions-md#b-d1")
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A refused read's alert is drawn afresh, so a screen reader says it again, for each new refusal
// of the read, and only then: a run of it that pauses while the browser is offline, refused
// nothing more, leaves the alert as it was.
test("a refused read's alert is said again for a new refusal, and not when its run pauses offline with no new refusal", async () => {
  const made = [
    decision("d1", { decision: { kind: "agent", links: [shaped(statementRef("r1"))] } }),
  ];
  const { client, read, redraw, view } = drawBody(
    issueTree([statement("r1"), decisionNode("d1")]),
    made,
    { specView: "failed" }
  );
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    // The read is tried once more after a refusal, so a run of it has a second try, which waits
    // while the browser is offline. The body `redraw(true)` alone draws is the same element,
    // which React does not draw again, so its read would keep the options it had: the body is
    // taken off the page and drawn afresh, and its read takes the new ones.
    client.setQueryDefaults(viewKey(KEY, "spec"), { retry: 1, retryDelay: 30 });
    redraw(false);
    redraw(true);
    const line = alertSaying(view, STATE_LINES.statements_unreadable);
    const first = within(line).getByRole("alert");
    const retry = within(line).getByRole("button", { name: "Retry" });
    // The run's first try starts while the browser is online; the browser goes offline before
    // that try is refused, so the second try pauses until it is online again.
    act(() => retry.click());
    act(() => onlineManager.setOnline(false));
    await act(() => Bun.sleep(100));
    expect(client.getQueryState(viewKey(KEY, "spec"))?.fetchStatus).toBe("paused");
    expect(alertSaying(view, STATE_LINES.statements_unreadable)).toBe(line);
    expect(
      within(line).getByRole("alert"),
      "a run that paused with no new refusal said the failure again"
    ).toBe(first);
    // Online again, the second try runs and is refused: a new refusal, said again.
    act(() => onlineManager.setOnline(true));
    await waitFor(() => expect(within(line).getByRole("alert")).not.toBe(first));
  } finally {
    onlineManager.setOnline(true);
    view.unmount();
    read.mockRestore();
  }
});

// A line's alert is drawn afresh, so a screen reader says it again, for each new refusal of a
// read the line stands failed on, even where another of its reads answers in the same render.
// The statements line stands on the coverage and the spec's blocks, each refused once, and its
// Retry asks both again: the coverage answers and the blocks are refused a second time, both
// before the query client tells their observers, which a browser does on a timer after each
// (`holdNotifications`), so the body draws both in one render. The refusals of the reads that
// stand failed come to two before that render (one of each read) and two after it (the blocks'
// two), so a count that summed them would not change, where the line's count rises with the new
// refusal.
test("a line's alert is said again for a new refusal of one read where another of its reads answers in the same render", async () => {
  const user = userEvent.setup();
  const { client, read, view } = drawBody(issueTree([statement("r1")]), [], {
    blocks: "failed",
    coverage: "failed",
  });
  const coverage = issueCoverageQuery(KEY).queryKey;
  // The spec's blocks are the tree read's.
  const blocks = outlineQuery(KEY, "conversation").queryKey;
  let notifications: HeldNotifications | undefined;
  try {
    // Both reads are refused once before the alert is taken, so it is the alert drawn for both
    // refusals.
    await waitFor(() => {
      expect(client.getQueryState(coverage)?.errorUpdateCount).toBe(1);
      expect(client.getQueryState(blocks)?.errorUpdateCount).toBe(1);
    });
    const line = alertSaying(view, STATE_LINES.statements_unreadable);
    const first = within(line).getByRole("alert");
    const answer = Promise.withResolvers<IssueCoverage>();
    const refusal = Promise.withResolvers<never>();
    read.coverage.mockReturnValue(answer.promise);
    read.tree.mockImplementation((_key, query) =>
      query?.view === undefined ? refusal.promise : Promise.resolve(specViewAnswer([]))
    );
    await user.click(within(line).getByRole("button", { name: "Retry" }));
    expect(within(line).getByRole("alert"), "a Retry with no new refusal said it again").toBe(
      first
    );
    notifications = holdNotifications();
    await act(async () => {
      answer.resolve(coverageRead([]));
      refusal.reject(new Error("offline"));
    });
    expect(client.getQueryState(coverage)?.data, "the coverage did not answer").toBeDefined();
    expect(client.getQueryState(blocks)?.errorUpdateCount, "the blocks were not refused").toBe(2);
    expect(within(line).getByRole("alert"), "the body drew a read it was not told of").toBe(first);
    notifications.tell();
    expect(alertSaying(view, STATE_LINES.statements_unreadable)).toBe(line);
    expect(
      within(line).getByRole("alert"),
      "a new refusal of the spec's blocks, as the coverage answered, was not said again"
    ).not.toBe(first);
  } finally {
    notifications?.tell();
    view.unmount();
    read.mockRestore();
  }
});

// A line's alert is drawn afresh, so a screen reader says it again, only for a new refusal of a
// read the line stands failed on. A read of the line that holds an answer keeps it when a
// refresh of it - an event's - is refused, and the line says nothing new of it, so with the
// spec's blocks refused and the coverage held, a refused refresh of the coverage leaves the
// statements line's alert as it was.
test("a refused refresh of a read the statements line holds an answer for leaves the line's alert as it was", async () => {
  const { client, read, view } = drawBody(issueTree([statement("r1")]), [], { blocks: "failed" });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    const alert = within(line).getByRole("alert");
    read.coverage.mockRejectedValue(new Error("offline"));
    const coverage = issueCoverageQuery(KEY).queryKey;
    await act(async () => {
      await client.refetchQueries({ queryKey: coverage });
    });
    await waitFor(() => {
      const state = client.getQueryState(coverage);
      expect(state?.errorUpdateCount, "the coverage's refresh was not refused").toBe(1);
      expect(state?.data, "the coverage's refresh took its answer away").toBeDefined();
    });
    expect(alertSaying(view, STATE_LINES.statements_unreadable)).toBe(line);
    expect(
      within(line).getByRole("alert"),
      "a refused refresh of a held read said the statements' failure again"
    ).toBe(alert);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5 and R9: a person's decision is drawn by the answer they gave, whose question the asks read
// brings. While that read is out the decision is drawn as nothing - never as an agent's record
// of it - and a refused one says so in its own line.
test("a person's decision is drawn as nothing while the asks read is out, and a refused asks read says so", async () => {
  const made = [
    decision("d1", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ];
  // The tree holds no question the asks read has not brought.
  const tree = issueTree([statement("r1"), decisionNode("d1")]);
  const out = drawBody(tree, made, { asksRead: "out" });
  try {
    expect(out.view.container.textContent).not.toContain(SPEC_WORDS.said_agent);
    expect(textsOf(out.view, "[data-spec-said]")).toEqual([]);
    expect(textsOf(out.view, "[data-spec-nothing]")).toEqual([]);
    expect(out.view.container.querySelector("[data-spec-because]")).toBeNull();
    expect(out.view.container.querySelector('[role="alert"]')).toBeNull();
  } finally {
    out.view.unmount();
    out.read.mockRestore();
  }
  const refused = drawBody(tree, made, { asksRead: "failed" });
  try {
    await waitFor(() => alertSaying(refused.view, STATE_LINES.asks_unreadable));
    expect(refused.view.container.textContent).not.toContain(SPEC_WORDS.said_agent);
  } finally {
    refused.view.unmount();
    refused.read.mockRestore();
  }
});

// R5 and R35: while the asks read is out or refused, the run a person's decision shaped stands
// with an empty cell, and `w` on its statements says nothing rather than that no decision is
// recorded behind them, which the view cannot know until that read answers.
test("w on a statement whose person's decision waits on the asks read reports no missing decision", async () => {
  const user = userEvent.setup();
  const made = [
    decision("d1", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      },
    }),
    decision("d2", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r2"))],
      },
    }),
  ];
  const tree = issueTree([
    statement("r1"),
    statement("r2"),
    decisionNode("d1"),
    decisionNode("d2"),
  ]);
  for (const asksRead of ["out", "failed"] as const) {
    const { read, view } = drawBody(tree, made, { asksRead });
    try {
      if (asksRead === "failed")
        await waitFor(() => alertSaying(view, STATE_LINES.asks_unreadable));
      for (const id of ["r1", "r2"]) {
        const row = rowOf(view, `spec#b-${id}`);
        act(() => row.focus());
        await user.keyboard("w");
        expect(
          row.querySelector("[data-row-report]")?.textContent ?? "",
          `w on statement ${id} says no decision is recorded while the asks read is ${asksRead}`
        ).not.toBe(KEY_REPORTS.no_decision);
      }
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: `w` on a statement whose cell draws no decision says that none is recorded only
// once the Spec view read the decisions come from is in. While that read is out or refused every
// cell is empty, and `w` says nothing, since a decision may yet stand there.
test("w on a statement says nothing of a missing decision while the Spec view read is out", async () => {
  const user = userEvent.setup();
  for (const specView of ["out", "failed"] as const) {
    const { read, view } = drawBody(issueTree([statement("r1")]), [], { specView });
    try {
      if (specView === "failed")
        await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
      const row = rowOf(view, "spec#b-r1");
      act(() => row.focus());
      await user.keyboard("w");
      expect(
        row.querySelector("[data-row-report]")?.textContent ?? "",
        `w on r1 says no decision is recorded while the Spec view read is ${specView}`
      ).not.toBe(KEY_REPORTS.no_decision);
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: `w` on a row that is no statement walks the decisions for the one behind it, so
// while the Spec view read the decisions come from is out and it finds none it says nothing,
// rather than that no decision is recorded behind the row, which the view cannot know yet.
test("w on an address-named row says nothing of a missing decision while the Spec view read is out", async () => {
  const user = userEvent.setup();
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  const { read, view } = drawBody(tree, [], { named: ["comment-1"], specView: "out" });
  try {
    const row = rowOf(view, "comment-1");
    act(() => row.focus());
    await user.keyboard("w");
    expect(
      row.querySelector("[data-row-report]")?.textContent ?? "",
      "w on comment-1 says no decision is recorded while the Spec view read is out"
    ).toBe("");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R11: `q` walks the open questions, which the asks read brings, so it says there is no next
// question once that read is in, and nothing while it is out, since a question may yet arrive.
// No question comes from the Spec view read, so `q` does not wait on it.
test("q says there is no next question once the asks read is in, and nothing while it is out", async () => {
  const user = userEvent.setup();
  for (const [out, said] of [
    [{ asksRead: "out" }, ""],
    [{ specView: "out" }, KEY_REPORTS.no_next_question],
  ] as const) {
    const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]), [], out);
    try {
      const row = rowOf(view, "spec#b-r1");
      act(() => row.focus());
      await user.keyboard("q");
      expect(
        row.querySelector("[data-row-report]")?.textContent ?? "",
        `q on r1 while ${Object.keys(out)[0]} is out`
      ).toBe(said);
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R11 and R35: each key waits on the reads it walks, and on no other. The results read is one
// only the marks are drawn from, so while it is out or refused `q` with no open question, `w` on
// a statement no decision shaped, and `w` on an address-named row with no decision behind it
// each still say where they could not go.
test("q and w still say where they could not go while the results read is out or refused", async () => {
  const user = userEvent.setup();
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  for (const results of ["out", "failed"] as const) {
    const { read, view } = drawBody(tree, [], { named: ["comment-1"], results });
    try {
      if (results === "failed")
        await waitFor(() => alertSaying(view, STATE_LINES.spec_results_unreadable));
      const statementRow = rowOf(view, "spec#b-r1");
      const other = rowOf(view, "comment-1");
      for (const [row, key, said] of [
        [statementRow, "q", KEY_REPORTS.no_next_question],
        [statementRow, "w", KEY_REPORTS.no_decision],
        [other, "w", KEY_REPORTS.no_decision],
      ] as const) {
        act(() => row.focus());
        await user.keyboard(key);
        expect(
          row.querySelector("[data-row-report]")?.textContent ?? "",
          `${key} on ${row.dataset.nodeId} while the results read is ${results}`
        ).toBe(said);
      }
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R11: the Spec view draws no row for an open question unless an address names one, so `q`
// with an open question to go to goes to it in the conversation, where the outline puts the
// focus on it. It says there is no next question only where the issue holds none.
test("q goes to the next open question in the conversation, where the view draws no row for it", async () => {
  const user = userEvent.setup();
  const question = openAsk("ask-2");
  const { read, view } = drawBody(issueTree([statement("r1"), question]));
  try {
    expect(conversationPath(question)).toBeDefined();
    const row = rowOf(view, "spec#b-r1");
    act(() => row.focus());
    await user.keyboard("q");
    expect(locationOf(view)).toBe(conversationPath(question) ?? "");
    expect(row.querySelector("[data-row-report]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: `w` on a row that is no statement - a node an address named - goes to the decision behind
// it. A decision that heads no run of the view has no cell here, so `w` goes to it in the
// conversation, as `q` goes to a question the view draws no row for, and never says that no
// decision is recorded behind the row.
test("w on an address-named row goes to its decision in the conversation where that decision heads no run", async () => {
  const user = userEvent.setup();
  const behind = decisionNode("d1", {
    chips: [{ answered: true, gone: false, reading: { words: "A remark" }, ref: "comment-1" }],
  });
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
    behind,
  ]);
  const made = [decision("d1", { decision: { kind: "agent" } })];
  const { read, view } = drawBody(tree, made, { named: ["comment-1"] });
  try {
    expect(decisionBehind(tree, "comment-1")?.id).toBe(behind.id);
    expect(conversationPath(behind)).toBeDefined();
    const row = rowOf(view, "comment-1");
    act(() => row.focus());
    await user.keyboard("w");
    expect(locationOf(view)).toBe(conversationPath(behind) ?? "");
    expect(row.querySelector("[data-row-report]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5 and R35: which decision heads which run is known only once the Spec view read and the asks
// read are in. While the asks read is out a person's decision heads its run with an empty cell,
// so `w` on an address-named row whose decision that is goes nowhere and says nothing, rather
// than that no decision is recorded behind the row. Whether the node `w` found is a decision is
// read off the tree it was found in, so with the Spec view read out as well `w` still does not
// take it for a question.
test("w on an address-named row says nothing of its decision while the asks read is out", async () => {
  const user = userEvent.setup();
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
    decisionNode("d1", {
      chips: [{ answered: true, gone: false, reading: { words: "A remark" }, ref: "comment-1" }],
    }),
  ]);
  const made = [
    decision("d1", {
      decision: { asks: ["ask-1"], kind: "person", links: [shaped(statementRef("r1"))] },
    }),
  ];
  for (const out of [{ asksRead: "out" }, { asksRead: "out", specView: "out" }] as const) {
    const { read, view } = drawBody(tree, made, { named: ["comment-1"], ...out });
    try {
      const reads = Object.keys(out).join(" and ");
      const row = rowOf(view, "comment-1");
      act(() => row.focus());
      await user.keyboard("w");
      expect(
        row.querySelector("[data-row-report]")?.textContent ?? "",
        `w on comment-1 reports while the ${reads} read is out`
      ).toBe("");
      expect(locationOf(view), `w on comment-1 left the view while the ${reads} read is out`).toBe(
        "/"
      );
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R17: a statement's mark comes from its results, its coverage and the Verification document's
// blocks, and from no other read, so it is drawn while the asks read is still out.
test("a statement carries its mark while the asks read is out", () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1", { children: [checkNode(passed)] })]);
  const { read, view } = drawBody(tree, [], { asksRead: "out", checks: [passed] });
  try {
    expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("met");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5 and R35: a refused read of the spec's blocks says the statements could not be read, in the
// line the view already says it in, rather than that the spec holds none.
test("a refused read of the spec's blocks says the statements could not be read", async () => {
  // Below 1024 px, where the view draws the sentences as its first group: from 1024 px up the
  // Contents pane holds them, and the view draws no group to say anything in (R13).
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const { read, view } = drawBody(issueTree([]), [], { blocks: "failed" });
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    expect(view.container.querySelector('[data-testid="issue-three-things"]')).not.toBeNull();
    expect(view.container.querySelector("[data-spec-empty]")).toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
    read.mockRestore();
  }
});

// R13: from 1024 px up the Contents pane stands beside the Spec view and holds the issue's three
// sentences, so the view draws no first group of them; below 1024 px no pane stands, and the view
// draws them as its first group, above its first statement, so they are drawn once at each width.
test("the Spec view draws the three sentences as its first group below 1024 px, and none beside the Contents pane from 1024 px up", () => {
  for (const width of [PHONE_WIDTH, DESKTOP_WIDTH]) {
    const restoreMatchMedia = stubViewport(width);
    const { read, view } = drawBody(issueTree([statement("r1")]));
    try {
      const groups = view.container.querySelectorAll(
        '[data-spec-body] [data-testid="issue-three-things"]'
      );
      expect(groups.length, `the Spec view's first groups at ${width} px`).toBe(
        width === PHONE_WIDTH ? 1 : 0
      );
      if (width === PHONE_WIDTH) {
        expect(
          (groups[0] as Element).compareDocumentPosition(rowOf(view, "spec#b-r1")) &
            Node.DOCUMENT_POSITION_FOLLOWING,
          "the sentences do not stand above the view's first statement"
        ).toBeTruthy();
      }
    } finally {
      view.unmount();
      read.mockRestore();
      restoreMatchMedia();
    }
  }
});

// R1: the first group draws the two rows under the three sentences from the counts the body is
// given, above the first statement, and each row's press turns its filter on. The body counts
// nothing itself: what stands under each label is what the slot handed it. Below 1024 px, where
// the view draws that group: from 1024 px up the Contents pane holds it (R13).
test("the first group draws the two rows from the counts it is given, and a press turns a filter on", async () => {
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const user = userEvent.setup();
  const counts: BriefCounts = {
    checks: {
      count: 3,
      states: [
        { count: 1, state: "failed" },
        { count: 2, state: "passed" },
      ],
    },
    plan: { count: 5, unit: "steps" },
  };
  const { read, view } = drawBody(issueTree([statement("r1")]), [], { counts });
  const rowOfFilter = (filter: "plan" | "verification"): HTMLElement => {
    const row = view.container.querySelector<HTMLElement>(`[data-brief-row="${filter}"]`);
    if (row === null) throw new Error(`the first group draws no row for ${filter}`);
    return row;
  };
  try {
    const plan = rowOfFilter("plan");
    const verification = rowOfFilter("verification");
    expect(plan.textContent).toContain("5 steps");
    expect(verification.textContent).toContain("3 checks");
    const first = rowOf(view, "spec#b-r1");
    for (const row of [plan, verification]) {
      expect(
        row.compareDocumentPosition(first) & Node.DOCUMENT_POSITION_FOLLOWING,
        `the ${row.getAttribute("data-brief-row")} row does not stand above the first statement`
      ).not.toBe(0);
    }
    await user.click(plan);
    expect(locationOf(view)).toBe(presetPath(KEY, "plan"));
    await user.click(verification);
    expect(locationOf(view)).toBe(presetPath(KEY, "verification"));
  } finally {
    view.unmount();
    read.mockRestore();
    restoreMatchMedia();
  }
});

// R7 and R9: a refused read leaves the view nothing to act on but its Retry, which Tab from the
// tree never reaches, and a reader whose focus is anywhere else keeps it. The stage the page draws
// the view in owns the focus on a load: where the view draws rows it gives a focus left on nothing
// to the tab-stop row, and a Retry only where no row is drawn (`ViewStage`). So here, with rows
// drawn, the row holds the focus at the load and the reader puts it on the Retry; once the Retry
// they pressed brings the read in, its line leaves and the view's first row takes the focus.
test("with rows drawn over a refused read the first row holds the focus, and takes it back once a pressed Retry brings the read in", async () => {
  const user = userEvent.setup();
  const tree = issueTree([statement("r1"), statement("r2")]);
  const elsewhere = document.createElement("button");
  document.body.append(elsewhere);
  elsewhere.focus();
  const kept = drawBody(tree, [], { coverage: "failed", stage: true });
  try {
    await waitFor(() => alertSaying(kept.view, STATE_LINES.statements_unreadable));
    expect(document.activeElement, "a Retry took the focus from elsewhere").toBe(elsewhere);
  } finally {
    kept.view.unmount();
    kept.read.mockRestore();
    elsewhere.remove();
  }
  const { read, view } = drawBody(tree, [], { coverage: "failed", stage: true });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    const retry = within(alert).getByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r1")));
    act(() => retry.focus());
    read.coverage.mockResolvedValue(coverageRead([]));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r1")));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R20: the view sends no request of its own. Each read it draws from is the page's tree's, which
// stays on the page, so the view drawn again - turned off and on - after the shell's 30 s have
// aged every answer asks for none of them again.
test("the body drawn again after its reads' answers have aged past 30 s sends no request", async () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1", { children: [checkNode(passed)] })]);
  const { read, redraw, view } = drawBody(tree, [], { checks: [passed], staleTime: 30_000 });
  const start = Date.now();
  const clock = spyOn(Date, "now");
  try {
    const before = askedOf(read);
    redraw(false);
    clock.mockReturnValue(start + 31_000);
    redraw(true);
    // A task's turn, in which a read asked for on the new body's mount would be sent.
    await act(async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
    expect(askedOf(read)).toEqual(before);
    expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("met");
  } finally {
    clock.mockRestore();
    view.unmount();
    read.mockRestore();
  }
});

// R5 and R22: a person who answered by options alone is quoted by the options they chose, and an
// option's label is a plain line like any other the view draws: a statement it cites is drawn by
// its name - in the cell's quote, in its line of what was chosen and in the statement's own line
// naming the decision - and never as an address.
test("an option label that cites a statement is drawn by the statement's name, never its address", () => {
  const label = `Keep ${statementRef("r2")} as written`;
  const named = "Keep R2. Every address as written";
  // The page names the statement by the name it published for it (`useIssueNames`).
  const made = decisionNode("d1", {
    chips: [
      {
        answered: true,
        gone: false,
        reading: { words: "R2. Every address" },
        ref: statementRef("r2"),
      },
    ],
  });
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { selected: [label], text: "" }),
      statement("r1"),
      statement("r2"),
      made,
    ]),
    [
      decision("d1", {
        decision: {
          asks: ["ask-1"],
          kind: "person",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ],
    // The page publishes the chip's name, which the names store hands the view.
    { names: new Map([[statementRef("r2"), "R2. Every address"]]) }
  );
  try {
    expect(textsOf(view, "[data-spec-quote]")).toEqual([`“${named}”`]);
    expect(view.container.querySelector("[data-spec-source]")?.textContent).toContain(
      `${SPEC_WORDS.chose} ${named}`
    );
    expect(textsOf(view, "[data-spec-because]")[0]).toContain(named);
    expect(view.container.textContent).not.toContain("dispatch://");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R2 and R5: a requirement of a document other than the spec - a plan's - is a block and nothing
// more. Named by the address, the view draws it as a row of its own kind, and never as a
// statement in a run, with a mark and a decision beside it (`isStatement`).
test("a requirement of another document the address names is drawn as no statement", () => {
  const plan = node({
    id: "plan-md#b-r9",
    text: "A requirement of the plan.",
    type: "requirement",
  });
  const { read, view } = drawBody(issueTree([statement("r1"), plan]), [], { named: [plan.id] });
  try {
    expect(
      [...view.container.querySelectorAll<HTMLElement>("[data-spec-statement]")].map(
        (row) => row.dataset.nodeId
      )
    ).toEqual(["spec#b-r1"]);
    expect(rowOf(view, plan.id).hasAttribute("data-spec-other")).toBe(true);
    expect(view.container.querySelectorAll("[data-spec-cell]")).toHaveLength(1);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: every other decision that shaped a statement is named on its line by its own words, cut as
// a chip cuts a name, so a long first line never runs the statement's line on.
test("a second decision is named on a statement's line by its words cut to a chip's length", () => {
  const long = "Name each tie by the words of the decision that made it, never by its address.";
  const { read, view } = drawBody(
    issueTree([statement("r1"), decisionNode("d1"), decisionNode("d2", { text: long })]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
      decision("d2", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ]
  );
  try {
    expect(textsOf(view, "[data-spec-informed]")).toEqual(["Name each tie by the words of…"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5 and R22: a decision with no words - an answer with no words and no option, an agent's
// decision with no text - is named by its kind's word wherever the view quotes it, in its cell
// and in the link on its statement's line, so no quote and no link is drawn empty.
test("a decision with no words is named by its kind's word, never by an empty quote", () => {
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { text: "" }),
      statement("r1"),
      statement("r2"),
      decisionNode("d1"),
      decisionNode("d2", { text: "" }),
    ]),
    [
      decision("d1", {
        decision: {
          asks: ["ask-1"],
          kind: "person",
          links: [shaped(statementRef("r1"))],
        },
      }),
      decision("d2", {
        text: "",
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r2"))],
        },
      }),
    ]
  );
  try {
    expect(view.container.textContent).not.toContain("“”");
    expect(textsOf(view, "[data-spec-quote]")[0]).toBe(NODE_TYPE_NAMES.ask);
    expect(textsOf(view, "[data-spec-because-link]")).toEqual([
      NODE_TYPE_NAMES.ask,
      NODE_TYPE_NAMES.decision,
    ]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R7 and R9: a pressed Retry that brings the reads in hands the focus to the view's first row,
// and where the view draws no row - an issue with no statement - to its tree, as a focus on the
// tree rests there; it never leaves the focus on nothing.
test("a Retry that brings in a view with no row leaves the focus on the tree", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([]), [], { coverage: "failed", stage: true });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    const retry = within(alert).getByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(retry));
    read.coverage.mockResolvedValue(coverageRead([]));
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
        EMPTY_STATE_LINES.spec
      )
    );
    await waitFor(() =>
      expect(document.activeElement).toBe(view.container.querySelector("[data-spec-tree]"))
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R7 and R9: where two reads were refused, the line whose Retry brings its read in leaves while
// it holds the focus, and the focus goes to the line still drawn, at its Retry: it never stays on
// nothing.
test("a Retry whose line leaves while another line stays hands the focus to that line's Retry", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([statement("r1")]), [], {
    specView: "failed",
    results: "failed",
    stage: true,
  });
  try {
    const results = await waitFor(() => alertSaying(view, STATE_LINES.spec_results_unreadable));
    const statements = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    act(() => within(statements).getByRole("button", { name: "Retry" }).focus());
    read.treeStands("specView", "answered");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.container.textContent).not.toContain(STATE_LINES.statements_unreadable)
    );
    await waitFor(() =>
      expect(document.activeElement).toBe(within(results).getByRole("button", { name: "Retry" }))
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R7 and R9: the focus a Retry held goes on once that line's own reads are in, whatever other
// read is still out: a Retry that brings in the statements while the results read is out hands
// the focus to the view's first row. With rows drawn, the stage gives the focus to the tab-stop
// row at the load (`ViewStage`), so the reader puts it on the Retry before pressing it.
test("a Retry hands the focus on once its own line's reads are in, while another read is still out", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]), [], {
    coverage: "failed",
    results: "out",
    stage: true,
  });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    const retry = within(alert).getByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r1")));
    act(() => retry.focus());
    read.coverage.mockResolvedValue(coverageRead([]));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r1")));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R20: the view sends no request of its own, and a read refused before it answered is asked
// again by its Retry and by nothing else: the view turned off and on again over a refused read
// asks for it no more times.
test("the body drawn again over a refused read asks for it no more times", async () => {
  const { client, read, redraw, view } = drawBody(issueTree([statement("r1")]), [], {
    coverage: "failed",
  });
  try {
    await waitFor(() =>
      expect(client.getQueryState(issueCoverageQuery(KEY).queryKey)?.status).toBe("error")
    );
    const before = read.coverage.mock.calls.length;
    redraw(false);
    redraw(true);
    // A task's turn, in which a read asked for on the new body's mount would be sent.
    await act(async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
    expect(read.coverage.mock.calls.length).toBe(before);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R1: a criterion of the view is drawn as a row of the statements' list, in the
// statements' bullet colour, with its tag, its author and its day, and no mark; its statements
// stand in the group it heads.
test("a criterion is a row of the statements' list, with its tag, its words and its author, and no mark", () => {
  const words = "A person reads the whole spec on a phone.";
  const { read, view } = drawBody(
    issueTree([
      statement("r1", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
      node({
        children: [
          node({
            at: "2026-11-15T10:00:00Z",
            author: { id: READER, kind: "user" },
            children: [statement("r2")],
            id: "criterion-1",
            text: words,
            type: "criterion",
          }),
        ],
        id: "comment-1",
        text: "Can I read the spec on my phone?",
        type: "comment",
      }),
    ])
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-spec-statement]")].map((row) =>
        row.getAttribute("data-node-id")
      )
    ).toEqual(["spec#b-r1", "criterion-1", "spec#b-r2"]);
    expect(view.container.querySelector("[data-spec-other]")).toBeNull();
    const row = rowOf(view, "criterion-1");
    const bulletOf = (one: HTMLElement) => one.querySelector(":scope > span > i")?.className;
    expect(row.getAttribute("data-node-type")).toBe("criterion");
    expect(row.querySelector("[data-node-tag]")?.textContent).toBe(NODE_TYPE_NAMES.criterion);
    expect(row.querySelector("[data-spec-author]")?.textContent).toBe(READER);
    expect(row.querySelector("[data-spec-criterion]")?.textContent).toContain(
      shortDay("2026-11-15T10:00:00Z")
    );
    expect(row.querySelector("[data-node-words]")?.textContent).toBe(words);
    expect(bulletOf(row)).toBe(bulletOf(rowOf(view, "spec#b-r1")));
    expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("no_check");
    expect(row.hasAttribute("data-spec-mark")).toBe(false);
    expect(row.querySelector(":scope > svg[data-glyph]")).toBeNull();
    const group = rowOf(view, "spec#b-r2").closest<HTMLElement>("[data-spec-group]");
    expect(group?.getAttribute("data-parent-id")).toBe("criterion-1");
    expect(group?.querySelector("[data-spec-group-heading]")?.textContent).toContain(words);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5, R9: on a converted issue, Enter on a statement and a press on a criterion's words
// each edit in their room's artifact; a parent's statement drawn as context is edited on its own
// issue and takes no caret.
test("Enter on a statement and a press on a criterion edit each in its document's room, and a parent's statement is edited on its own issue", async () => {
  const user = userEvent.setup();
  const rooms = spyOn(roomTarget, "useRoomFacts").mockReturnValue({
    converted: true,
    issueKey: KEY,
    tree: "artifact-tree",
    viewOf: null,
  });
  const onEditing = mock<OnEditing>(() => {});
  const own = statement("r1");
  const criterion = node({
    id: `${KEY}/tree#k1`,
    text: "A person reads the whole spec on a phone.",
    type: "criterion",
  });
  const above = statement("r9", { id: "r9", reference: "dispatch://CORE-2/spec#b-r9" });
  const editors = (
    <>
      <DocumentStage artifactId="artifact-spec">
        <div data-test-room-editor="artifact-spec" tabIndex={-1} />
      </DocumentStage>
      <DocumentStage artifactId="artifact-tree">
        <div data-test-room-editor="artifact-tree" tabIndex={-1} />
      </DocumentStage>
    </>
  );
  const { read, view } = drawBody(issueTree([own, criterion, above]), [], {
    context: new Set([above.id]),
    editor: editors,
    onEditing,
  });
  try {
    const statementRow = rowOf(view, own.id);
    const criterionRow = rowOf(view, criterion.id);
    const aboveRow = rowOf(view, above.id);
    expect(criterionRow).toHaveAttribute("data-editable");
    expect(aboveRow).not.toHaveAttribute("data-editable");

    act(() => statementRow.focus());
    await user.keyboard("{Enter}");
    expect(onEditing).toHaveBeenCalledWith("artifact-spec", expect.any(Function));
    expect(
      statementRow.querySelector('[data-test-room-editor="artifact-spec"]')
    ).toBeInTheDocument();

    const criterionWords = criterionRow.querySelector<HTMLElement>("[data-node-words]");
    if (criterionWords === null) throw new Error("criterion draws no words");
    await user.click(criterionWords);
    await waitFor(() =>
      expect(
        criterionRow.querySelector('[data-test-room-editor="artifact-tree"]')
      ).toBeInTheDocument()
    );
    expect(onEditing).toHaveBeenCalledWith("artifact-tree", expect.any(Function));

    act(() => aboveRow.focus());
    await user.keyboard("{Enter}");
    expect(aboveRow.querySelector("[data-row-report]")?.textContent).toBe(KEY_REPORTS.issue_above);
    expect(aboveRow.querySelector("[data-test-room-editor]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
    rooms.mockRestore();
  }
});
