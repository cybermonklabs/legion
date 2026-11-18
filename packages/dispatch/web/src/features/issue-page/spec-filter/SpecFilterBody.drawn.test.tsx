import { beforeAll, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import { installIntersectionObserver } from "../../../__tests__/intersection-observer";
import { api } from "../../../api/client";
import { issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type {
  Actor,
  CheckBlock,
  IssueAskRow,
  IssueCheckResults,
  IssueDetails,
} from "../../../api/types";
import { pageFocusedRow } from "../../../theme/classes";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { measureNearWords } from "../clamp-fixtures";
import {
  artifact,
  issueTree,
  KEY,
  node,
  READER,
  SPEC_SLUG,
} from "../decisions-filter/decisions-fixtures";
import { FocusedNodeProvider, useFocusedNode } from "../focused-node";
import type { OutlineRequest } from "../IssueTree";
import { buildIssueTree, type TreeAsk, type TreeNode } from "../issue-tree";
import { shortDay } from "../node-time";
import { OutlineRequestProvider } from "../outline-request";
import { KEY_REPORTS } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_WORDS,
  NODE_TYPE_NAMES,
  SPEC_WORDS,
  VERIFICATION_WORDS,
} from "../page-words";
import { presetView } from "../presets";
import { ReferenceNamesProvider, usePublishReferenceNames } from "../reference-names";
import { decisionBehind } from "../tree-keys";
import {
  issueTreeQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  type TreeTuple,
  viewKey,
  viewQuery,
} from "../tree-read/tree-read";
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

/** The names the page's one batched read brought, published as the page publishes them
 *  (`useIssueTree.ts`), which a reference in a node's drawn words is named from (`RowText`). */
function Named({
  children,
  names,
}: {
  children: ReactNode;
  names: ReadonlyMap<string, string> | undefined;
}): ReactNode {
  usePublishReferenceNames(names);
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

/** The reads the page's tree fills (`useIssueTree.ts`), each by an observer that fetches as the
 *  tree's own do, under the same keys and with the same requests: the body only watches them,
 *  so this stands in for the tree above it, which no view takes off the page. */
function TreeReads({ issue }: { issue: IssueDetails }): ReactNode {
  useQuery(issueCoverageQuery(issue.key));
  useQuery(issueResultsQuery(issue.key));
  useQuery({ queryFn: () => api.listIssueAsks(issue.key, "all"), queryKey: ["asks", issue.key] });
  useQuery(issueTreeQuery(issue.key));
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
  readonly asks: Mock<typeof api.listIssueAsks>;
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
const NO_NODES: TreeAnswer = {
  converted: false,
  cursor: null,
  nodes: [],
  tree: null,
  tuples: [],
  view_of: null,
};

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The body over `tree`, as the Spec view's slot draws it, under the reads the page's tree fills
 *  (`TreeReads`), with `made` as the nodes of the Spec view read's answer, `tuples` as that
 *  answer's tuples, `asks` as the issue's asks read and `checks` as its results read.
 *  `asksRead`, `specView`, `results`, `coverage` and `blocks` (the tree read) each say how that
 *  read stands - answered, failed, or still out - and each is answered unless a case says so;
 *  the Spec view read is held as the page holds it (`HeldView`). `verification` and `plan`,
 *  where a case names them, give the issue that document, and the tree read stands as
 *  `verification` says where a case names it; the decisions document's blocks, as the plan's,
 *  are the tree read's and no read of their own, so `decisionBlocks` and `plan` refuse nothing;
 *  `names` is what the page's batched reference read named; `staleTime` is how long an answer
 *  stays fresh, which the shell sets to 30 s. The page's provider of the Contents list's requests
 *  stands above the body, so `request` is taken as the page takes it; `redraw(false)` takes the
 *  body off the page and `redraw(true)` draws a new one under the same provider and reads, as
 *  the view turned off and on again does. */
function drawBody(
  tree: TreeNode,
  made: readonly TreeAnswerNode[] = [],
  {
    asks = [],
    asksRead = "answered",
    blocks = "answered",
    checks = [],
    context = new Set<string>(),
    coverage = "answered",
    named = [],
    names,
    plan,
    reader = READER,
    request,
    results = "answered",
    reveal,
    specView = "answered",
    staleTime = Number.POSITIVE_INFINITY,
    tuples = [],
    verification,
  }: {
    asks?: readonly TreeAsk[];
    asksRead?: ReadState;
    blocks?: ReadState;
    checks?: readonly CheckBlock[];
    context?: Set<string>;
    coverage?: ReadState;
    decisionBlocks?: ReadState;
    named?: readonly string[];
    names?: ReadonlyMap<string, string>;
    plan?: ReadState;
    reader?: string;
    request?: OutlineRequest;
    results?: ReadState;
    reveal?: string;
    specView?: ReadState;
    staleTime?: number;
    tuples?: readonly TreeTuple[];
    verification?: ReadState;
  } = {}
): {
  client: QueryClient;
  read: Reads;
  redraw: (drawn: boolean) => void;
  view: RenderResult;
} {
  const base = issueDetails();
  const issue = {
    ...base,
    ...(verification === undefined ? {} : { verification_artifact_id: "artifact-ver" }),
    ...(plan === undefined ? {} : { plan_artifact_id: "artifact-plan" }),
    artifacts: [
      ...base.artifacts,
      ...(verification === undefined ? [] : [artifact("artifact-ver", "verification-md")]),
      ...(plan === undefined ? [] : [artifact("artifact-plan", "plan-md")]),
    ],
  };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime } },
  });
  const spies = {
    asks: spyOn(api, "listIssueAsks"),
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
    client.setQueryData(issueTreeQuery(KEY).queryKey, treeAnswers.blocks);
  }
  if (treeStanding.specView === "answered") {
    client.setQueryData(viewKey(KEY, "spec"), treeAnswers.specView);
  }
  // The issue's asks, which say which question a person answered (`decisionNodes`). The fixtures
  // carry the members of an ask the page reads (`TreeAsk`), not every member the route answers,
  // so a read of them again answers what the cache holds.
  settle(asksRead, spies.asks, () => {
    client.setQueryData(["asks", KEY], [...asks]);
    spies.asks.mockImplementation(
      async () => client.getQueryData<IssueAskRow[]>(["asks", KEY]) ?? []
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
  const page = (drawn: boolean): ReactNode => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <Named names={names}>
          <MemoryRouter>
            <KeymapProvider>
              <MarginProvider>
                <OutlineRequestProvider>
                  <FocusedNodeProvider>
                    <IssueScope />
                    <FocusedProbe />
                    <TreeReads issue={issue} />
                    {drawn ? (
                      <HeldView root={tree}>
                        {(root) => (
                          <SpecFilterBody
                            context={context}
                            emptyLine={EMPTY_STATE_LINES.spec}
                            holdsEscape={false}
                            issue={issue}
                            reader={reader}
                            {...(request === undefined ? {} : { request })}
                            {...(reveal === undefined ? {} : { reveal })}
                            root={root}
                            view={presetView(tree, "spec", "", new Set(named)).root}
                          />
                        )}
                      </HeldView>
                    ) : null}
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
  return { client, read, redraw: (drawn) => view.rerender(page(drawn)), view };
}

/** The text of every element matching `selector`, in the order they are drawn. */
function textsOf(view: RenderResult, selector: string): string[] {
  return [...view.container.querySelectorAll(selector)].map((element) =>
    (element.textContent ?? "").trim()
  );
}

function rowOf(view: RenderResult, id: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${id}"]`);
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

/** The error line that says `line`, which holds its Retry. */
function alertSaying(view: RenderResult, line: string): HTMLElement {
  const alert = [...view.container.querySelectorAll<HTMLElement>('[role="alert"]')].find(
    (one) => one.textContent?.includes(line) === true
  );
  if (alert === undefined) throw new Error(`the body draws no error line saying "${line}"`);
  return alert;
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
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
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

// R5, the tester's F7: one line on each statement's own row, with no focus and no hover: the
// words the decision came from, as a link to that decision, who decided and when.
test("each statement carries one line naming the decision that shaped it, with a link to it", () => {
  const ask = answeredAsk("ask-1", { text: "Put the decisions beside the statements. Please." });
  const { read, view } = drawBody(
    issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision("d1", {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      }),
    ]
  );
  try {
    const lines = textsOf(view, "[data-spec-because]");
    expect(lines).toHaveLength(2);
    for (const line of lines) {
      expect(line).toContain(SPEC_WORDS.because);
      expect(line).toContain("“Put the decisions beside the statements.”");
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
          mark: "query",
        })
      ),
      answerNode("ask-1", "ask", { mark: "context" }),
    ],
    {
      asks: [askRead("ask-1", words)],
      tuples: [
        { from: "ask-1", kind: "shaped", to: "r1" },
        { from: "ask-1", kind: "shaped", to: "r3" },
      ],
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
          mark: "query",
        })
      ),
      answerNode("ask-1", "ask", { mark: "context" }),
    ],
    {
      asksRead: "out",
      tuples: [
        { from: "ask-1", kind: "shaped", to: "r1" },
        { from: "ask-1", kind: "shaped", to: "r3" },
      ],
    }
  );
  try {
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([]);
    expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(3);

    act(() => {
      client.setQueryData(["asks", KEY], [askRead("ask-1", words)]);
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
          mark: "query",
        })
      ),
      answerNode("ask-1", "ask", { mark: "context" }),
    ],
    { asksRead: "failed", tuples: [{ from: "ask-1", kind: "shaped", to: "r1" }] }
  );
  try {
    await waitFor(
      () =>
        expect(
          view.container.textContent,
          "the column does not say its open questions could not be read where the asks read failed"
        ).toContain(ANSWER_LINES.answers_unreadable),
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
      expect(failed.view.container.textContent).toContain(ANSWER_LINES.decisions_unreadable)
    );
    expect(textsOf(failed.view, "[data-spec-nothing]")).toEqual([]);
  } finally {
    failed.view.unmount();
    failed.read.mockRestore();
  }
});

// The Spec view read and the asks read can each be out, answered or failed when the body is
// drawn, with the statements' reads answered, and the column draws only what the reads that
// answered say: the line that its decisions could not be read where the Spec view read failed
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
              mark: "query",
            })
          ),
          answerNode("ask-1", "ask", { mark: "context" }),
        ],
        {
          asks: [askRead("ask-1", words)],
          asksRead: asks,
          specView,
          tuples: [{ from: "ask-1", kind: "shaped", to: "r1" }],
        }
      );
      try {
        const both = specView === "answered" && asks === "answered";
        await waitFor(
          () => {
            const text = view.container.textContent ?? "";
            expect(
              text.includes(ANSWER_LINES.decisions_unreadable),
              `${combination}: whether the column says its decisions could not be read`
            ).toBe(specView === "failed");
            expect(
              text.includes(ANSWER_LINES.answers_unreadable),
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
      kind: "agent",
      links: [shaped(statementRef("r1"))],
      text,
      unsure: true,
      waiting_review: true,
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
      decision("d1", { kind: "agent", links: [shaped(statementRef("r1"))] }),
      decision("d2", {
        changed_at: null,
        kind: "agent",
        links: [shaped(statementRef("r2"))],
      }),
      decision("d3", { kind: "agent", links: [shaped(statementRef("r3"))] }),
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
// none of the block's source lines: whole in the cell through `RowText`, and as one line of plain
// words on the because line.
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
  const { read, view } = drawBody(
    issueTree([
      statement("r4", { text: `**R4.** ${name}.` }),
      // The tree names the statement a decision shaped by a chip on the decision, from the
      // batched read (`tree-drafts.ts`), which is the name the page gives it
      // (`referenceStandIns`).
      decisionNode("d1", {
        chips: [{ answered: true, gone: false, name, ref: cited }],
        text: words,
        tree,
      }),
    ]),
    [
      decision("d1", {
        kind: "agent",
        links: [shaped(cited)],
        text: words,
        text_tree: tree,
      }),
    ],
    { names: new Map([[cited, `:::requirement{#r4}\n${name}\n:::`]]) }
  );
  try {
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-cell] a[data-row-reference]")?.textContent
      ).toBe(name)
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
    read.mockRestore();
  }
});

// R5: where the answered question is the statement's own parent, its parent line and its
// decision line are one line, so the question is drawn once.
test("the question a statement hangs under is drawn once, as the group's heading", () => {
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { text: "Keep the memo in the pane. It reads better there." }, [
        statement("r1"),
      ]),
    ])
  );
  try {
    const heading = view.container.querySelector("[data-spec-group-heading]")?.textContent ?? "";
    expect(heading).toContain("question");
    expect(heading).toContain("Does the built page work for you?");
    const source = view.container.querySelector("[data-spec-source]")?.textContent ?? "";
    expect(source).not.toContain("Does the built page work for you?");
    expect(textsOf(view, "[data-spec-quote]")).toEqual(["“Keep the memo in the pane.”"]);
  } finally {
    view.unmount();
    read.mockRestore();
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
        chips: [{ answered: true, gone: false, name: "r1", ref: statementRef("r1") }],
      }),
    ]),
    [
      decision("d1", {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
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
// are drawn as the outline draws an agent's words - whole here, where no layout cuts them.
test("a statement's words are drawn as its author wrote them", () => {
  const words =
    "**R1.** Every node's words are on the page without opening it; an agent's long words " +
    "show their first six lines and the rest one key away, and his words are never cut.";
  const { read, view } = drawBody(issueTree([statement("r1", { text: words })]));
  try {
    expect(
      view.container.querySelector("[data-spec-statement] [data-node-words]")?.textContent
    ).toBe(words);
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
    decision("d1", { kind: "agent", links: [shaped(statementRef("r1"))] }),
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
// few words the page knows it by: while the words are whole it stands in the trail after their
// last word, a direct child of the words' box, and
// nowhere else in the row. The one for a statement no check serves is the mark at the right of
// the row, and is not said twice: that statement draws no trail at all.
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
      asks: ["ask-1"],
      kind: "person",
      links: [shaped(statementRef("r3")), shaped(statementRef("r4"))],
    }),
    decision("d5", { kind: "agent", links: [shaped(statementRef("r4"))] }),
  ];
  // The tree read's answer: the spec's two statements, and the question the person's decision
  // answers, each under the issue.
  const answered = (
    id: string,
    supertag: string,
    extra: Partial<TreeAnswerNode> = {}
  ): TreeAnswerNode => ({
    attributes: {},
    author: [],
    id,
    made_at: "2026-11-05T00:00:00Z",
    parent: KEY,
    rank: null,
    state: "live",
    supertag,
    text: "",
    visibility: "project",
    ...extra,
  });
  const tree = buildIssueTree({
    asks: [askRead("ask-1", "Keep both statements.")],
    coverage: coverageRead(["r3", "r4"]),
    issue: issueDetails(),
    tree: {
      converted: false,
      cursor: null,
      nodes: [
        answered("d5", "decision", {
          document: { artifact_id: "artifact-dec", position: 0, slug: "decisions-md" },
          links: made[1]?.links ?? [],
        }),
        ...["r3", "r4"].map((id, position) =>
          answered(id, "requirement", {
            document: { artifact_id: "artifact-spec", position, slug: "spec" },
          })
        ),
        answered("ask-1", "ask"),
      ],
      tree: null,
      tuples: [{ from: "d5", kind: "shaped", to: statementRef("r4") }],
      view_of: null,
    },
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
      kind: "agent",
      links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
    }),
    decision("d2", {
      kind: "agent",
      links: [shaped(statementRef("r3")), shaped(statementRef("r4"))],
    }),
    decision("d3", { kind: "agent", links: [shaped(statementRef("r5"))] }),
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
        kind: "agent",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
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
    const alert = await waitFor(() => alertSaying(view, ANSWER_LINES.statements_unreadable));
    expect(view.container.querySelector("[data-spec-empty]")).toBeNull();
    read.coverage.mockResolvedValue(coverageRead([]));
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
        EMPTY_STATE_LINES.spec
      )
    );
    expect(view.container.textContent).not.toContain(ANSWER_LINES.statements_unreadable);
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
    const alert = await waitFor(() => alertSaying(view, ANSWER_LINES.results_unreadable));
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
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeDefined();
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
    expect(view.container.textContent).not.toContain(ANSWER_LINES.results_unreadable);
    expect(view.container.textContent).not.toContain(ANSWER_LINES.statements_unreadable);
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
    expect(empty.view.container.textContent).not.toContain(ANSWER_LINES.statements_unreadable);
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
    expect(view.container.textContent).not.toContain(ANSWER_LINES.decisions_unreadable);
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
    const alert = await waitFor(() => alertSaying(view, ANSWER_LINES.results_unreadable));
    expect(rowOf(view, "spec#b-r1").hasAttribute("data-spec-mark")).toBe(false);
    read.treeStands("blocks", "answered");
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark")).toBe("met")
    );
    expect(view.container.textContent).not.toContain(ANSWER_LINES.results_unreadable);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

/** Lays every element out `height` px tall until the answer is called, as
 *  `NodeRow.drawn.test.tsx` does: no DOM of a unit test lays anything out, and a statement's cut
 *  reads its words' own boxes (`measureClamp`). */
function layOut(height: number): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get: () => height,
  });
  const box = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 0, 600, height)
  );
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
    box.mockRestore();
  };
}

// Review 5928280634 on pull request 4234 (thread 4592007127): a press focuses what it lands on as
// it goes down, and a statement taking the focus for a link inside it drew its words whole under
// the pointer - the cut and "more" gone - while the statement that had the focus lost its look,
// so the link moved off the pointer and the press followed nothing. The focus a press gives a
// statement is taken once the press is over, as the outline takes a row's (`IssueTree.tsx`'s
// `focusRow`). happy-dom lays nothing out, so this reads the order: nothing the body draws changes
// while the press is held, and then the pressed statement takes the focus.
test("a press on a link inside a cut statement leaves every statement as drawn until it ends, and then the statement takes the focus", async () => {
  const user = userEvent.setup();
  const words = Array.from({ length: 60 }, (_unused, at) => `word${at + 1}`).join(" ");
  const long = `**R2.** Read [the guide](https://example.com/guide) first. ${words}`;
  // 64 words over 400 lines of 24 px: six lines are drawn and the rest held back.
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2", { text: long, tree: displayTree(long) })])
  );
  const looksFocused = (id: string) => rowOf(view, id).className.includes(pageFocusedRow);
  try {
    act(() => rowOf(view, "spec#b-r1").focus());
    expect(looksFocused("spec#b-r1")).toBe(true);
    const pressed = rowOf(view, "spec#b-r2");
    const link = await waitFor(() => {
      const found = pressed.querySelector<HTMLElement>("[data-node-words] a");
      if (found === null) throw new Error("the cut statement draws no link in its words");
      return found;
    });
    // The page cuts only the words of rows near the viewport.
    await measureNearWords(near);
    await waitFor(() => expect(pressed.querySelector("[data-node-more]")).not.toBeNull());
    // What is read is who holds the focus, not where the link leads.
    link.addEventListener("click", (event) => event.preventDefault());

    await user.pointer({ keys: "[MouseLeft>]", target: link });
    expect(document.activeElement, "the press did not focus the link it went down on").toBe(link);
    expect(looksFocused("spec#b-r2"), "the held press drew the pressed statement focused").toBe(
      false
    );
    expect(
      pressed.querySelector("[data-node-more]"),
      "the held press drew the cut statement's words whole"
    ).not.toBeNull();
    expect(
      looksFocused("spec#b-r1"),
      "the held press took the focused look from the statement that had it"
    ).toBe(true);
    expect(focusedNodeIn(view).id, "the held press moved Details").toBe("spec#b-r1");

    await user.pointer({ keys: "[/MouseLeft]" });
    await waitFor(() => expect(looksFocused("spec#b-r2")).toBe(true));
    expect(pressed.querySelector("[data-node-more]")).toBeNull();
    expect(looksFocused("spec#b-r1")).toBe(false);
    expect(focusedNodeIn(view)).toEqual({ id: "spec#b-r2", type: "requirement" });
  } finally {
    view.unmount();
    read.mockRestore();
    restore();
    near.restore();
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
        chips: [{ answered: true, gone: false, name: "r1", ref: statementRef("r1") }],
      }),
    ]),
    [
      decision("d1", {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
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
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
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
    [decision("d1", { kind: "agent", links: [shaped(statementRef("r2"))] })]
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
        await waitFor(() => alertSaying(view, ANSWER_LINES.answers_unreadable));
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

// R5, R17 and R35: the view waits on the reads of what it draws and on no other. A refused read
// of a document it draws nothing from - the plan, the decisions document's blocks - changes
// nothing in it: every statement is drawn and marked, and no line says a read failed. Waiting on
// every document's blocks, the view left every statement unmarked, with no empty line, no
// sentences and nothing saying why, as two review findings and the tester saw.
test("a refused read of a document the view draws nothing from leaves every statement drawn and marked", async () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const tree = issueTree([statement("r1", { children: [checkNode(passed)] })]);
  for (const [refused, document] of [
    [{ plan: "failed" }, "artifact-plan"],
    [{ decisionBlocks: "failed" }, issueDetails().decisions_artifact_id],
  ] as const) {
    const { client, read, view } = drawBody(tree, [], { checks: [passed], ...refused });
    try {
      // That document's blocks are the tree read's, which holds its answer.
      await waitFor(() =>
        expect(client.getQueryState(issueTreeQuery(KEY).queryKey)?.status).toBe("success")
      );
      expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark"), document).toBe("met");
      expect(view.container.querySelector('[role="alert"]'), document).toBeNull();
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: a refused Spec view read, which the column's decisions come from, says so in its
// own line over the tree, and its Retry asks that read alone again, after which each run's cell
// draws the decision that shaped it.
test("a refused Spec view read says so in its own line, and its Retry asks that read alone again", async () => {
  const user = userEvent.setup();
  const made = [decision("d1", { kind: "agent", links: [shaped(statementRef("r1"))] })];
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), made, {
    specView: "failed",
  });
  try {
    const alert = await waitFor(() => alertSaying(view, ANSWER_LINES.decisions_unreadable));
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

// R5 and R9: a person's decision is drawn by the answer they gave, whose question the asks read
// brings. While that read is out the decision is drawn as nothing - never as an agent's record
// of it - and a refused one says so in its own line.
test("a person's decision is drawn as nothing while the asks read is out, and a refused asks read says so", async () => {
  const made = [
    decision("d1", { asks: ["ask-1"], kind: "person", links: [shaped(statementRef("r1"))] }),
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
    await waitFor(() => alertSaying(refused.view, ANSWER_LINES.answers_unreadable));
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
      asks: ["ask-1"],
      kind: "person",
      links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
    }),
    decision("d2", { kind: "agent", links: [shaped(statementRef("r2"))] }),
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
        await waitFor(() => alertSaying(view, ANSWER_LINES.answers_unreadable));
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
        await waitFor(() => alertSaying(view, ANSWER_LINES.decisions_unreadable));
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

// R11: `q` with no open question to go to says so only once every read the body draws from is
// in. While the asks read or the Spec view read is out a question may yet arrive, and `q` says
// nothing.
test("q says nothing of a next question while the asks or the Spec view read is out", async () => {
  const user = userEvent.setup();
  for (const out of [{ asksRead: "out" }, { specView: "out" }] as const) {
    const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]), [], out);
    try {
      const row = rowOf(view, "spec#b-r1");
      act(() => row.focus());
      await user.keyboard("q");
      expect(
        row.querySelector("[data-row-report]")?.textContent ?? "",
        `q on r1 says there is no next question while ${Object.keys(out)[0]} is out`
      ).not.toBe(KEY_REPORTS.no_next_question);
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: a refused read of the spec's blocks says the statements could not be read, in the
// line the view already says it in, and the issue's sentences say nothing, rather than that the
// spec has no summary, which the view cannot know.
test("a refused read of the spec's blocks says the statements could not be read, and the sentences say nothing", async () => {
  const { read, view } = drawBody(issueTree([]), [], { blocks: "failed" });
  try {
    await waitFor(() => alertSaying(view, ANSWER_LINES.statements_unreadable));
    expect(view.container.textContent).not.toContain(EMPTY_STATE_LINES.spec_summary);
    expect(view.container.querySelector("[data-spec-empty]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R7 and R9: a refused read leaves the view nothing to act on but its Retry, which Tab from the
// tree never reaches. So the first line's Retry takes the focus where the view's key or its
// address left it on nothing, and a reader whose focus is anywhere else keeps it; once a Retry
// the reader pressed brings the read in, the view's first row takes the focus.
test("a refused read's Retry takes the focus left on nothing, and the first row takes it once a pressed Retry brings the read in", async () => {
  const user = userEvent.setup();
  const tree = issueTree([statement("r1"), statement("r2")]);
  const elsewhere = document.createElement("button");
  document.body.append(elsewhere);
  elsewhere.focus();
  const kept = drawBody(tree, [], { coverage: "failed" });
  try {
    await waitFor(() => alertSaying(kept.view, ANSWER_LINES.statements_unreadable));
    expect(document.activeElement, "a Retry took the focus from elsewhere").toBe(elsewhere);
  } finally {
    kept.view.unmount();
    kept.read.mockRestore();
    elsewhere.remove();
  }
  const { read, view } = drawBody(tree, [], { coverage: "failed" });
  try {
    const alert = await waitFor(() => alertSaying(view, ANSWER_LINES.statements_unreadable));
    const retry = within(alert).getByRole("button", { name: "Retry" });
    await waitFor(() => expect(document.activeElement).toBe(retry));
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
  // The page names the statement by the words of the first chip naming it (`useReferenceNames`).
  const made = decisionNode("d1", {
    chips: [{ answered: true, gone: false, name: "R2. Every address", ref: statementRef("r2") }],
  });
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { selected: [label], text: "" }),
      statement("r1"),
      statement("r2"),
      made,
    ]),
    [decision("d1", { asks: ["ask-1"], kind: "person", links: [shaped(statementRef("r1"))] })]
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
      decision("d1", { kind: "agent", links: [shaped(statementRef("r1"))] }),
      decision("d2", { kind: "agent", links: [shaped(statementRef("r1"))] }),
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
      decision("d1", { asks: ["ask-1"], kind: "person", links: [shaped(statementRef("r1"))] }),
      decision("d2", { kind: "agent", links: [shaped(statementRef("r2"))], text: "" }),
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
  const { read, view } = drawBody(issueTree([]), [], { coverage: "failed" });
  try {
    const alert = await waitFor(() => alertSaying(view, ANSWER_LINES.statements_unreadable));
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
