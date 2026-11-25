import { beforeAll, expect, type Mock, mock, spyOn, test } from "bun:test";
import { Schema } from "@milkdown/kit/prose/model";
import { EditorState } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import {
  onlineManager,
  QueryClient,
  QueryClientProvider,
  useQueries,
  useQuery,
} from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { type HeldNotifications, holdNotifications } from "../../../__tests__/commits";
import { displayTree } from "../../../__tests__/display-trees";
import { installIntersectionObserver } from "../../../__tests__/intersection-observer";
import { installResizeObserver } from "../../../__tests__/scroll-box";
import { api } from "../../../api/client";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type {
  Actor,
  CheckBlock,
  Decision,
  DecisionLink,
  IssueCheckResults,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
} from "../../../api/types";
import { pageFocusedRow } from "../../../theme/classes";
import { issueDecisionsQuery } from "../../doc/spec-marks";
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
  openAsk,
  READER,
} from "../decisions-filter/decisions-fixtures";
import { conversationPath } from "../decisions-filter/decisions-parts";
import { DocumentStage, DocumentStages } from "../document-stage";
import { FocusedNodeProvider, useFocusedNode } from "../focused-node";
import type { BriefCounts } from "../IssueThreeThings";
import type { OutlineRequest } from "../IssueTree";
import { presetPath } from "../issue-address";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { shortDay } from "../node-time";
import { TWO_THREADS } from "../outline-fixtures";
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
import { ReferenceNamesProvider, usePublishReferenceNames } from "../reference-names";
import { ViewStage } from "../slots/stage";
import { drawStage } from "../slots/stage-fixtures";
import { nodeRowSelectorOf } from "../tree-focus";
import { decisionBehind } from "../tree-keys";
import {
  issueAsksQuery,
  issueCommentsQuery,
  issueMessagesQuery,
  treeDocumentIds,
} from "../useIssueTree";
import type { OnEditing } from "../useNodeEditing";
import { check, checkNode, result } from "../verification-filter/verification-fixtures";
import { SpecFilterBody } from "./SpecFilterBody";
import {
  answeredAsk,
  askRead,
  coverageRead,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  shaped,
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
  useQuery(issueDecisionsQuery(issue.key, true));
  useQuery(issueResultsQuery(issue.key));
  useQuery(issueAsksQuery(issue.key));
  useQueries({ queries: treeDocumentIds(issue).map((id) => artifactBlocksQuery(id)) });
  return null;
}

/** How one of the page's reads stands when the body is drawn. */
type ReadState = "answered" | "failed" | "out";

/** A read's answer as `state` says: `value`, a refusal, or none ever. */
function answerAs<T>(state: ReadState, value: T): Promise<T> {
  if (state === "answered") return Promise.resolve(value);
  if (state === "failed") return Promise.reject(new Error("offline"));
  return new Promise<never>(() => {});
}

/** The spies on the page's reads that the body reads, so a case can answer one again. */
interface Reads {
  readonly asks: Mock<typeof api.listIssueAsks>;
  readonly blocks: Mock<typeof api.getArtifactBlocks>;
  readonly coverage: Mock<typeof api.getIssueCoverage>;
  readonly decisions: Mock<typeof api.getIssueDecisions>;
  readonly results: Mock<typeof api.getIssueResults>;
  /** Puts every read back as it was. */
  readonly mockRestore: () => void;
}

/** The results read over `checks`, as the page's read answers it. */
function resultsOf(checks: readonly CheckBlock[]): IssueCheckResults {
  return { checks: [...checks], issue: KEY, verification_artifact_id: "" };
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The body over `tree`, as the Spec view's slot draws it, under the reads the page's tree fills
 *  (`TreeReads`), with `made` as the issue's decisions read and `checks` as its results read.
 *  `asks`, `decisions`, `results`, `coverage` and `blocks` (every document's blocks but those a
 *  case names below) each say how that read stands - answered, failed, or still out - and each
 *  is answered unless a case says so; `verification` and `plan`, where a case names them, give
 *  the issue that document, whose blocks read stands so, and `decisionBlocks` says how the
 *  decisions document's blocks read stands; `names` is what the page's batched reference read
 *  named; `staleTime` is how long an answer stays fresh, which the shell sets to 30 s; `stage`
 *  draws the body inside the stage the page draws every view in (`ViewStage`), which places the
 *  focus on a view's load and moves it on as a failed read's line that held it leaves. The
 *  page's provider of the Contents list's requests stands above the body, so `request` is taken
 *  as the page takes it; `redraw(false)` takes the body off the page and `redraw(true)` draws a
 *  new one under the same provider and reads, as the view turned off and on again does, and
 *  `redraw(true, issue)` draws the body and the reads over `issue` from then on, as the page
 *  does once the issue read answers anew. */
function drawBody(
  tree: TreeNode,
  made: readonly Decision[] = [],
  {
    asks = "answered",
    blocks = "answered",
    checks = [],
    context = new Set<string>(),
    counts,
    coverage = "answered",
    decisionBlocks,
    decisions = "answered",
    editor,
    named = [],
    names,
    onEditing = () => {},
    plan,
    reader = READER,
    request,
    results = "answered",
    reveal,
    stage = false,
    staleTime = Number.POSITIVE_INFINITY,
    verification,
  }: {
    asks?: ReadState;
    blocks?: ReadState;
    checks?: readonly CheckBlock[];
    context?: Set<string>;
    counts?: BriefCounts;
    coverage?: ReadState;
    decisionBlocks?: ReadState;
    decisions?: ReadState;
    editor?: ReactNode;
    named?: readonly string[];
    names?: ReadonlyMap<string, string>;
    onEditing?: OnEditing;
    plan?: ReadState;
    reader?: string;
    request?: OutlineRequest;
    results?: ReadState;
    reveal?: string;
    stage?: boolean;
    staleTime?: number;
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
    blocks: spyOn(api, "getArtifactBlocks"),
    coverage: spyOn(api, "getIssueCoverage"),
    decisions: spyOn(api, "getIssueDecisions"),
    results: spyOn(api, "getIssueResults"),
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
  settle(decisions, spies.decisions, () => {
    const answer = decisionsRead(made);
    client.setQueryData(["decisions", KEY], answer);
    spies.decisions.mockResolvedValue(answer);
  });
  settle(results, spies.results, () => {
    client.setQueryData(issueResultsQuery(KEY).queryKey, resultsOf(checks));
    spies.results.mockResolvedValue(resultsOf(checks));
  });
  settle(coverage, spies.coverage, () => {
    client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
    spies.coverage.mockResolvedValue(coverageRead([]));
  });
  settle(asks, spies.asks, () => {
    client.setQueryData(["asks", KEY], []);
    spies.asks.mockResolvedValue([]);
  });
  // Each document's blocks read stands as the case says: the Verification document's as
  // `verification`, the plan's as `plan`, the decisions document's as `decisionBlocks`, and
  // every other document's - the spec's - as `blocks`.
  const blocksOf = (id: string): ReadState =>
    (id === "artifact-ver"
      ? verification
      : id === "artifact-plan"
        ? plan
        : id === issue.decisions_artifact_id
          ? decisionBlocks
          : undefined) ?? blocks;
  for (const id of treeDocumentIds(issue)) {
    if (blocksOf(id) === "answered") client.setQueryData(artifactBlocksQuery(id).queryKey, []);
  }
  spies.blocks.mockImplementation((id) => answerAs(blocksOf(id), []));
  const read: Reads = {
    ...spies,
    mockRestore: () => {
      for (const spy of Object.values(spies)) spy.mockRestore();
    },
  };
  const bodyOf = (over: IssueDetails): ReactNode => (
    <SpecFilterBody
      context={context}
      counts={counts}
      emptyLine={EMPTY_STATE_LINES.spec}
      holdsEscape={false}
      issue={over}
      onEditing={onEditing}
      reader={reader}
      // The case holds each read as it says, so an address lands as soon as its row is drawn.
      readsSettled
      {...(request === undefined ? {} : { request })}
      {...(reveal === undefined ? {} : { reveal })}
      root={tree}
      view={presetView(tree, "spec", "", new Set(named)).root}
    />
  );
  let shown: IssueDetails = issue;
  let body = bodyOf(shown);
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
      decision({
        asks: ["ask-1"],
        block_id: "d1",
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

// R5, the tester's F7: one line on each statement's own row, with no focus and no hover: after
// "because:", the question the person answered by its first sentence, then the words the
// decision came from, as a link to that decision, who decided and when.
test("each statement carries one line naming the decision that shaped it, with a link to it", () => {
  const ask = answeredAsk("ask-1", { text: "Put the decisions beside the statements. Please." });
  const { read, view } = drawBody(
    issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision({
        asks: ["ask-1"],
        block_id: "d1",
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
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

// R35's sentence is true only once the decisions read has answered: while it is out the column
// says nothing of a statement, and a read that failed says so in the page's own line.
test("the column says no decision shaped a statement only once the decisions are read", async () => {
  const out = drawBody(issueTree([statement("r1")]), [], { decisions: "out" });
  try {
    expect(textsOf(out.view, "[data-spec-nothing]")).toEqual([]);
    expect(out.view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
  } finally {
    out.view.unmount();
    out.read.mockRestore();
  }
  const failed = drawBody(issueTree([statement("r1")]), [], { decisions: "failed" });
  try {
    await waitFor(() =>
      expect(failed.view.container.textContent).toContain(STATE_LINES.issue_decisions_unreadable)
    );
    expect(textsOf(failed.view, "[data-spec-nothing]")).toEqual([]);
  } finally {
    failed.view.unmount();
    failed.read.mockRestore();
  }
});

// R35's sentence waits on the asks read as well: a person's answer, which their decision is
// drawn by, and the answered question a statement hangs under, which heads its run, both come
// with it. While that read is out or refused the column says nothing of a statement no decision
// it holds shaped.
test("the column says no decision shaped a statement only once the asks are read", async () => {
  for (const asks of ["out", "failed"] as const) {
    const { read, view } = drawBody(issueTree([statement("r1")]), [], { asks });
    try {
      if (asks === "failed") await waitFor(() => alertSaying(view, STATE_LINES.asks_unreadable));
      expect(
        textsOf(view, "[data-spec-nothing]"),
        `the column says no decision shaped r1 while the asks read is ${asks}`
      ).toEqual([]);
      expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
    } finally {
      view.unmount();
      read.mockRestore();
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
    decision({
      block_id: "d1",
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
      decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] }),
      decision({
        block_id: "d2",
        changed_at: null,
        kind: "agent",
        links: [shaped(statementRef("r2"))],
      }),
      decision({ block_id: "d3", kind: "agent", links: [shaped(statementRef("r3"))] }),
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
      decision({
        block_id: "d1",
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
// both of today's decisions read, read as the times they name. A statement the decision named
// after its last change says nothing of it, and so does r3, whose two times would order the
// other way read as text: its far end changed 0.12 s past the second, before the tie made
// 0.123456 s past it, though "…00.12Z" sorts after "…00.123456Z".
test("a statement changed after its tie says Changed since", () => {
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), statement("r3"), decisionNode("d1")]),
    [
      decision({
        block_id: "d1",
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
    [decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))], text })]
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
      // The tree names every block a decision links as a chip on it, as the page's model does
      // (`tree-drafts.ts`).
      decisionNode("d1", {
        chips: [{ answered: true, gone: false, name: "r1", ref: statementRef("r1") }],
      }),
    ]),
    [
      decision({
        asks: ["ask-1"],
        block_id: "d1",
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

/** `client` holding every read the page's tree is built from, answered: the issue holds two
 *  threads of comments and nothing else, as the outline's own cases seed them
 *  (`IssueTree.test.tsx`'s `seedBodyReads`). Who is reading is each case's own. */
function holdTreeReads(client: QueryClient): void {
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, resultsOf([]));
  client.setQueryData(issueDecisionsQuery(KEY, true).queryKey, {
    decisions: [],
    document: null,
    open_questions: [],
  });
  client.setQueryData(issueAsksQuery(KEY).queryKey, []);
  client.setQueryData(issueCommentsQuery(KEY).queryKey, [...TWO_THREADS]);
  client.setQueryData(issueMessagesQuery(KEY).queryKey, []);
}

// The names reads are the page's tree's, read last, once no read the tree is built from is still
// out, and the Spec view draws from them too: the issue's three sentences, and the name of every
// reference a plain line cites. A refused names read says so in its own line, after the lines of
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
  // Every read the page's tree is built from answered but the decisions read, which is refused,
  // so the view draws a line of its own before the names'.
  client.setQueryData(["whoami"], { kind: "user", login: READER });
  holdTreeReads(client);
  client.removeQueries({ queryKey: issueDecisionsQuery(KEY, true).queryKey });
  const decisions = spyOn(api, "getIssueDecisions").mockRejectedValue(new Error("offline"));
  const names = spyOn(api, "getReferenceTargets").mockRejectedValue(new Error("offline"));
  const view = drawStage("", { client, issue, preset: "spec" });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.names_unreadable));
    const lines = '[data-spec-body] [data-failed-read] [role="alert"]';
    expect(textsOf(view, lines), "the names' line is not drawn after the view's own").toEqual([
      STATE_LINES.issue_decisions_unreadable,
      STATE_LINES.names_unreadable,
    ]);
    names.mockResolvedValue({ targets: [] } as never);
    const asked = names.mock.calls.length;
    await user.click(within(line).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(textsOf(view, lines), "the names' line stayed once the names answered").toEqual([
        STATE_LINES.issue_decisions_unreadable,
      ])
    );
    expect(names.mock.calls.length, "the names' Retry did not ask the names again").toBeGreaterThan(
      asked
    );
  } finally {
    view.unmount();
    decisions.mockRestore();
    names.mockRestore();
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
  }
});

// The Spec view draws the names reads' line as the tree says it, speaking or not
// (`NamesFailure.speaks`): a line that stands again for a refusal it already said is drawn
// without speaking. The names read of the tree's set is refused; a comment goes, the names read
// of the smaller set answers, and the line leaves. The comment comes back: the names read is back
// on the first set, which holds no answer and the refusal the line counted, and runs again, so
// the line stands again, with no live role.
test("a names line that stands again in the Spec view for a refusal it counted is drawn without speaking", async () => {
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  holdTreeReads(client);
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: READER });
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  names.mockRejectedValueOnce(new Error("offline"));
  const view = drawStage("", { client, issue, preset: "spec" });
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.names_unreadable));
    await act(async () => {
      client.setQueryData(
        issueCommentsQuery(KEY).queryKey,
        TWO_THREADS.filter((comment) => comment.id !== "reply-b")
      );
    });
    await waitFor(() =>
      expect(
        view.queryByText(STATE_LINES.names_unreadable),
        "the names' line stayed once the names of the smaller set answered"
      ).toBeNull()
    );
    names.mockReturnValueOnce(new Promise<never>(() => {}) as never);
    await act(async () => {
      client.setQueryData(issueCommentsQuery(KEY).queryKey, [...TWO_THREADS]);
    });
    const words = await waitFor(() => view.getByText(STATE_LINES.names_unreadable));
    expect(words, "the names' line said again a refusal it had counted").not.toHaveAttribute(
      "role"
    );
    expect(view.queryByRole("alert"), "a line standing again spoke").toBeNull();
  } finally {
    view.unmount();
    names.mockRestore();
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

// Where the view draws no row, the page keymap's `reachPart` puts the focus on the tree itself,
// and it stays there: the focus is in the view, never on the document's body.
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

// A statement's own words carry its number where the spec wrote one, and are drawn as its author
// wrote them, whole.
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
    decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] }),
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
// decision its run is headed by. The page's model hangs a person's decision under the question it
// answers, at the tree's end, and an agent's under the issue, before the statements, so the
// first decision of the tree's order that names r4 is the agent's, which only informed it and
// heads no run.
test("w on a statement goes to the cell beside it, whichever decision the tree's order names first", async () => {
  const made = [
    decision({
      asks: ["ask-1"],
      block_id: "d2",
      kind: "person",
      links: [shaped(statementRef("r3")), shaped(statementRef("r4"))],
    }),
    decision({ block_id: "d5", kind: "agent", links: [shaped(statementRef("r4"))] }),
  ];
  const tree = buildIssueTree({
    asks: [askRead("ask-1", "Keep both statements.")],
    coverage: coverageRead(["r3", "r4"]),
    decisions: decisionsRead(made),
    issue: issueDetails(),
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
    decision({
      block_id: "d1",
      kind: "agent",
      links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
    }),
    decision({
      block_id: "d2",
      kind: "agent",
      links: [shaped(statementRef("r3")), shaped(statementRef("r4"))],
    }),
    decision({ block_id: "d3", kind: "agent", links: [shaped(statementRef("r5"))] }),
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
      decision({
        block_id: "d1",
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
    read.blocks.mockRejectedValue(new Error("offline"));
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

// The architect's decision on R35 and R5: an issue that holds no statement says so while its
// coverage and blocks are asked again and when that ask fails, and a cell that says no decision
// shaped its statement still says so when the decisions read fails again: only a read that failed
// with nothing held says it failed (review 5928280634 on pull request 4234, the reviewer's
// finding b, which named the empty line too).
test("a read that fails again over the statements and decisions the page holds keeps the empty line and the cells, and says nothing failed", async () => {
  const empty = drawBody(issueTree([]));
  try {
    expect(empty.view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
      EMPTY_STATE_LINES.spec
    );
    empty.read.coverage.mockRejectedValue(new Error("offline"));
    empty.read.blocks.mockRejectedValue(new Error("offline"));
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
    read.decisions.mockRejectedValue(new Error("offline"));
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["decisions", KEY] });
    });
    await waitFor(() => expect(client.getQueryState(["decisions", KEY])?.status).toBe("error"));
    expect(
      view.container.querySelector("[data-spec-nothing]"),
      "a failed read again over the held decisions takes the cell's words away"
    ).not.toBeNull();
    expect(view.container.textContent).not.toContain(STATE_LINES.issue_decisions_unreadable);
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
    read.blocks.mockResolvedValue([]);
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

// R8: a press is counted in the editor's text. The view draws a reference to an issue by the
// issue's key and title, while the editor holds the reference as it is written, hidden under a
// title of its own that takes no caret (`@legion/proof-editor`'s `reference-decorations.ts`), so a
// press after the reference counts it as written. A plain editable on the stage stands in for
// the editor's drawing of the statement: its title out of the editable text, the reference as
// written hidden in it. Counted as drawn, the press on "first" would put the caret 8 characters
// into the words after the reference rather than 3.
test("a press after a reference counts the reference as the editor holds it", async () => {
  const user = userEvent.setup();
  const cited = "dispatch://CORE-2";
  const after = " first, then the rest.";
  const text = `Read ${cited}${after}`;
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
          <p>
            {"Read "}
            <span contentEditable={false}>CORE-2 The other issue</span>
            <span hidden>{cited}</span>
            {after}
          </p>
        </section>
      </div>
    </DocumentStage>
  );
  const { read, view } = drawBody(
    issueTree([statement("r1", { text, tree: displayTree(text) })]),
    [],
    { editor, names: new Map([[cited, "The other issue"]]), onEditing }
  );
  const original = Object.getOwnPropertyDescriptor(document, "caretPositionFromPoint");
  try {
    const row = rowOf(view, "spec#b-r1");
    const link = await waitFor(() => {
      const found = row.querySelector(`[data-node-words] a[data-row-reference="${cited}"]`);
      expect(found?.textContent).toBe("CORE-2 The other issue");
      return found;
    });
    const pressed = link?.nextSibling;
    if (!(pressed instanceof Text) || pressed.parentElement === null) {
      throw new Error("the statement draws no words after its reference");
    }
    expect(pressed.data).toBe(after);
    // The press is on "first", 3 characters into the words after the reference.
    const point = mock(() => ({ offsetNode: pressed, offset: 3 }));
    Object.defineProperty(document, "caretPositionFromPoint", { configurable: true, value: point });
    await user.pointer({ keys: "[MouseLeft]", target: pressed.parentElement });
    await waitFor(() => {
      const words = row.querySelector('[data-block-id="r1"] p')?.lastChild;
      expect(words?.textContent).toBe(after);
      expect(window.getSelection()?.anchorNode).toBe(words);
      expect(window.getSelection()?.anchorOffset).toBe(3);
    });
  } finally {
    if (original === undefined) Reflect.deleteProperty(document, "caretPositionFromPoint");
    else Object.defineProperty(document, "caretPositionFromPoint", original);
    view.unmount();
    editorView.destroy();
    read.mockRestore();
  }
});

/**
 * Presses statement r1's drawn text `pressed`, `into` characters into it, and waits for the caret
 * to stand `at` characters into `lands`, the last text of the statement's paragraph in its
 * editor; both default to the press's own, for words the two draw alike after the kind a case
 * reads. The statement's words are `text`, drawn from `tree`, the reading the server sends,
 * where one is given, and as stored where none is, with `names` as the page's batched read named
 * its references. A plain editable on the stage stands in for the editor's drawing of the
 * statement, as in the case above: `held` is its paragraph, each kind drawn as the editor draws
 * it. `drawn` reads the statement's drawn words before the press.
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

// Inline HTML, such as a placeholder `<name>`, is drawn as its characters, while the editor holds
// it as an atom the caret cannot enter, so a press after it counts it as nothing. Counted as
// drawn, the press on "then" would put the caret 6 characters on, after "the".
test("a press after inline html lands in the word pressed", async () => {
  const after = " and then the rest.";
  const text = `Kept by <name>${after}`;
  await pressAfterKind({
    drawn: (words) => {
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
    into: 7,
    pressed: after,
    text,
    tree: displayTree(text),
  });
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

// A hard break is a line break in both, which holds no text, so a press after it counts nothing
// for it.
test("a press after a hard break lands in the word pressed", async () => {
  const after = "Line two, then the rest.";
  const text = `Line one.\\\n${after}`;
  await pressAfterKind({
    drawn: (words) => {
      expect(words.querySelector("br")).not.toBeNull();
    },
    held: (
      <>
        {"Line one."}
        <br />
        {after}
      </>
    ),
    into: 6,
    pressed: after,
    text,
    tree: displayTree(text),
  });
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

// A statement whose read sent no reading of its words is drawn as stored, its inline HTML as the
// characters it was written as, with no mark of what the editor holds there, so the count of the
// press runs on by the 6 characters of `<name>`, to the end of "the". The word pressed is found
// with the words beside it where the editor holds them, and the caret goes as far into it.
test("an uncounted inline kind still puts the caret in the pressed word", async () => {
  const after = " and then the rest.";
  const text = `Kept by <name>${after}`;
  await pressAfterKind({
    at: 7,
    drawn: (words) => {
      expect(words.querySelector('[data-type="html"]')).toBeNull();
      expect(words.textContent).toContain(text);
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
    into: 21,
    lands: after,
    pressed: text,
    text,
  });
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
      decision({
        asks: ["ask-1"],
        block_id: "d1",
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
    [decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r2"))] })]
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

/** How many times each of the reads the body draws from has been asked for. */
function askedOf(read: Reads): Record<string, number> {
  return {
    asks: read.asks.mock.calls.length,
    blocks: read.blocks.mock.calls.length,
    coverage: read.coverage.mock.calls.length,
    decisions: read.decisions.mock.calls.length,
    results: read.results.mock.calls.length,
  };
}

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
      await waitFor(() =>
        expect(client.getQueryState(artifactBlocksQuery(document).queryKey)?.status).toBe("error")
      );
      expect(rowOf(view, "spec#b-r1").getAttribute("data-spec-mark"), document).toBe("met");
      expect(view.container.querySelector('[role="alert"]'), document).toBeNull();
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: a refused decisions read says so in its own line over the tree, and its Retry asks
// that read alone again, after which each run's cell draws the decision that shaped it.
test("a refused decisions read says so in its own line, and its Retry asks that read alone again", async () => {
  const user = userEvent.setup();
  const made = [decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] })];
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), made, {
    decisions: "failed",
  });
  try {
    const alert = await waitFor(() => alertSaying(view, STATE_LINES.issue_decisions_unreadable));
    expect(view.container.querySelectorAll('[role="alert"]')).toHaveLength(1);
    expect(view.container.querySelector("[data-spec-cell][data-decision-id]")).toBeNull();
    read.decisions.mockResolvedValue(decisionsRead(made));
    const before = askedOf(read);
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-spec-cell]")?.getAttribute("data-decision-id")
      ).toBe("decisions-md#b-d1")
    );
    expect(askedOf(read)).toEqual({ ...before, decisions: before.decisions + 1 });
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
  const made = [decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] })];
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), made, {
    decisions: "failed",
  });
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.issue_decisions_unreadable));
    const answer = Promise.withResolvers<IssueDecisions>();
    read.decisions.mockReturnValue(answer.promise);
    await user.click(within(line).getByRole("button", { name: "Retry" }));
    expect(alertSaying(view, STATE_LINES.issue_decisions_unreadable)).toBe(line);
    expect(within(line).getByRole("button", { name: "Retry" }).getAttribute("aria-busy")).toBe(
      "true"
    );
    await act(async () => answer.resolve(decisionsRead(made)));
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
  const made = [decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] })];
  const { client, read, redraw, view } = drawBody(
    issueTree([statement("r1"), decisionNode("d1")]),
    made,
    { decisions: "failed" }
  );
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.issue_decisions_unreadable));
    // The read is tried once more after a refusal, so a run of it has a second try, which waits
    // while the browser is offline. The body `redraw(true)` alone draws is the same element,
    // which React does not draw again, so its read would keep the options it had: the body is
    // taken off the page and drawn afresh, and its read takes the new ones.
    client.setQueryDefaults(["decisions", KEY], { retry: 1, retryDelay: 30 });
    redraw(false);
    redraw(true);
    const line = alertSaying(view, STATE_LINES.issue_decisions_unreadable);
    const first = within(line).getByRole("alert");
    const retry = within(line).getByRole("button", { name: "Retry" });
    // The run's first try starts while the browser is online; the browser goes offline before
    // that try is refused, so the second try pauses until it is online again.
    act(() => retry.click());
    act(() => onlineManager.setOnline(false));
    await act(() => Bun.sleep(100));
    expect(client.getQueryState(["decisions", KEY])?.fetchStatus).toBe("paused");
    expect(alertSaying(view, STATE_LINES.issue_decisions_unreadable)).toBe(line);
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
  const blocks = artifactBlocksQuery("artifact-spec").queryKey;
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
    read.blocks.mockReturnValue(refusal.promise);
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

// A line's alert is said again for a refusal new to the line where one of its reads changes key.
// The statements line stands on the spec's refused blocks; the issue then names a spec document of
// another id, whose blocks read was refused too before - another view read it under the same key
// - so the line stands on that read at once. Each blocks read was refused once, so a count that
// took the read under the new key for the read before it would add nothing; the line adds the new
// query's refusal. The page's read of the new spec's blocks then stays out, so only the change of
// key can say the alert again.
test("a line's alert is said again where the issue names a spec of another id whose blocks read was refused too", async () => {
  const { client, read, redraw, view } = drawBody(issueTree([statement("r1")]), [], {
    blocks: "failed",
  });
  const other = artifact("artifact-spec-2", "spec-2", true);
  const blocks = artifactBlocksQuery(other.id);
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    const first = within(line).getByRole("alert");
    await act(() => client.prefetchQuery(blocks));
    expect(client.getQueryState(blocks.queryKey)?.errorUpdateCount).toBe(1);
    read.blocks.mockReturnValue(new Promise<never>(() => {}));
    const base = issueDetails();
    redraw(true, {
      ...base,
      artifacts: [other, ...base.artifacts.filter(({ primary }) => !primary)],
      primary_artifact_id: other.id,
    });
    expect(alertSaying(view, STATE_LINES.statements_unreadable)).toBe(line);
    expect(
      within(line).getByRole("alert"),
      "the refusal of the new spec's blocks was not said again"
    ).not.toBe(first);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A line's alert is said again only for a refusal new to the line, which counts each refusal once
// under the key it was refused under. The statements line stands on the spec's refused blocks;
// the issue names a spec of another id, whose blocks read was refused too, and the alert is said
// again. The issue then names the first spec again: the line stands on its blocks read, which
// holds no answer and the refusal the line has counted, so the alert is not drawn afresh.
test("a line's alert is not said again where the issue names again a spec whose refused blocks read the line counted", async () => {
  const { client, read, redraw, view } = drawBody(issueTree([statement("r1")]), [], {
    blocks: "failed",
  });
  const other = artifact("artifact-spec-2", "spec-2", true);
  try {
    const line = await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    const first = within(line).getByRole("alert");
    await act(() => client.prefetchQuery(artifactBlocksQuery(other.id)));
    read.blocks.mockReturnValue(new Promise<never>(() => {}));
    const base = issueDetails();
    redraw(true, {
      ...base,
      artifacts: [other, ...base.artifacts.filter(({ primary }) => !primary)],
      primary_artifact_id: other.id,
    });
    const second = within(line).getByRole("alert");
    expect(second, "the refusal of the new spec's blocks was not said again").not.toBe(first);
    redraw(true, base);
    expect(alertSaying(view, STATE_LINES.statements_unreadable)).toBe(line);
    expect(
      within(line).getByRole("alert"),
      "the first spec's blocks, whose refusal the line counted, said it again"
    ).toBe(second);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A line that stands again for a refusal it counted has said it, and is drawn without speaking.
// The statements line stands on the spec's refused blocks; the issue names a spec of another id,
// whose blocks read is out with no refusal, so no read of the line stands failed and it leaves.
// The issue then names the first spec again: the line stands again on its blocks read, which
// holds no answer and the refusal the line counted, and is drawn with no live role.
test("a line that stands again where the issue names again a spec whose refused blocks read it counted is drawn without speaking", async () => {
  const { read, redraw, view } = drawBody(issueTree([statement("r1")]), [], { blocks: "failed" });
  const other = artifact("artifact-spec-2", "spec-2", true);
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    read.blocks.mockReturnValue(new Promise<never>(() => {}));
    const base = issueDetails();
    redraw(true, {
      ...base,
      artifacts: [other, ...base.artifacts.filter(({ primary }) => !primary)],
      primary_artifact_id: other.id,
    });
    expect(
      view.queryByText(STATE_LINES.statements_unreadable),
      "the line stayed on a read out with no refusal"
    ).toBeNull();
    redraw(true, base);
    const words = view.getByText(STATE_LINES.statements_unreadable);
    expect(words, "the line said again a refusal it had counted").not.toHaveAttribute("role");
    expect(view.queryByRole("alert"), "a line standing again quietly spoke").toBeNull();
    const line = words.closest<HTMLElement>("[data-failed-read]");
    if (line === null) throw new Error("the line's words stand outside a failed-read line");
    expect(within(line).getByRole("button", { name: "Retry" })).toBeInTheDocument();
  } finally {
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
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
    }),
  ];
  // The tree holds no question the asks read has not brought.
  const tree = issueTree([statement("r1"), decisionNode("d1")]);
  const out = drawBody(tree, made, { asks: "out" });
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
  const refused = drawBody(tree, made, { asks: "failed" });
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
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
    }),
    decision({ block_id: "d2", kind: "agent", links: [shaped(statementRef("r2"))] }),
  ];
  const tree = issueTree([
    statement("r1"),
    statement("r2"),
    decisionNode("d1"),
    decisionNode("d2"),
  ]);
  for (const asks of ["out", "failed"] as const) {
    const { read, view } = drawBody(tree, made, { asks });
    try {
      if (asks === "failed") await waitFor(() => alertSaying(view, STATE_LINES.asks_unreadable));
      for (const id of ["r1", "r2"]) {
        const row = rowOf(view, `spec#b-${id}`);
        act(() => row.focus());
        await user.keyboard("w");
        expect(
          row.querySelector("[data-row-report]")?.textContent ?? "",
          `w on statement ${id} says no decision is recorded while the asks read is ${asks}`
        ).not.toBe(KEY_REPORTS.no_decision);
      }
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: `w` on a statement whose cell draws no decision says that none is recorded only
// once the decisions read is in. While that read is out or refused every cell is empty, and `w`
// says nothing, since a decision may yet stand there.
test("w on a statement says nothing of a missing decision while the decisions read is out", async () => {
  const user = userEvent.setup();
  for (const decisions of ["out", "failed"] as const) {
    const { read, view } = drawBody(issueTree([statement("r1")]), [], { decisions });
    try {
      if (decisions === "failed")
        await waitFor(() => alertSaying(view, STATE_LINES.issue_decisions_unreadable));
      const row = rowOf(view, "spec#b-r1");
      act(() => row.focus());
      await user.keyboard("w");
      expect(
        row.querySelector("[data-row-report]")?.textContent ?? "",
        `w on r1 says no decision is recorded while the decisions read is ${decisions}`
      ).not.toBe(KEY_REPORTS.no_decision);
    } finally {
      view.unmount();
      read.mockRestore();
    }
  }
});

// R5 and R35: `w` on a row that is no statement walks the decisions for the one behind it, so
// while the decisions read is out and it finds none it says nothing, rather than that no
// decision is recorded behind the row, which the view cannot know yet.
test("w on an address-named row says nothing of a missing decision while the decisions read is out", async () => {
  const user = userEvent.setup();
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  const { read, view } = drawBody(tree, [], { decisions: "out", named: ["comment-1"] });
  try {
    const row = rowOf(view, "comment-1");
    act(() => row.focus());
    await user.keyboard("w");
    expect(
      row.querySelector("[data-row-report]")?.textContent ?? "",
      "w on comment-1 says no decision is recorded while the decisions read is out"
    ).toBe("");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R11: `q` walks the open questions, which the asks read brings, so it says there is no next
// question once that read is in, and nothing while it is out, since a question may yet arrive.
// No question comes from the decisions read, so `q` does not wait on it.
test("q says there is no next question once the asks read is in, and nothing while it is out", async () => {
  const user = userEvent.setup();
  for (const [out, said] of [
    [{ asks: "out" }, ""],
    [{ decisions: "out" }, KEY_REPORTS.no_next_question],
  ] as const) {
    const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]), [], out);
    try {
      const row = rowOf(view, "spec#b-r1");
      act(() => row.focus());
      await user.keyboard("q");
      expect(
        row.querySelector("[data-row-report]")?.textContent ?? "",
        `q on r1 while the ${Object.keys(out)[0]} read is out`
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
    chips: [{ answered: true, gone: false, name: "A remark", ref: "comment-1" }],
  });
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
    behind,
  ]);
  const made = [decision({ block_id: "d1", kind: "agent" })];
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

// R5 and R35: which decision heads which run is known only once the decisions read and the asks
// read are in. While the asks read is out a person's decision heads its run with an empty cell,
// so `w` on an address-named row whose decision that is goes nowhere and says nothing, rather
// than that no decision is recorded behind the row. Whether the node `w` found is a decision is
// read off the tree it was found in, so with the body's decisions read out as well `w` still
// does not take it for a question.
test("w on an address-named row says nothing of its decision while the asks read is out", async () => {
  const user = userEvent.setup();
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
    decisionNode("d1", {
      chips: [{ answered: true, gone: false, name: "A remark", ref: "comment-1" }],
    }),
  ]);
  const made = [
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
    }),
  ];
  for (const out of [{ asks: "out" }, { asks: "out", decisions: "out" }] as const) {
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
  const { read, view } = drawBody(tree, [], { asks: "out", checks: [passed] });
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
  const { read, view } = drawBody(issueTree([]), [], { blocks: "failed" });
  try {
    await waitFor(() => alertSaying(view, STATE_LINES.statements_unreadable));
    expect(view.container.querySelector("[data-spec-empty]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R1: the first group draws the two rows under the three sentences from the counts the body is
// given, above the first statement, and each row's press turns its filter on. The body counts
// nothing itself: what stands under each label is what the slot handed it.
test("the first group draws the two rows from the counts it is given, and a press turns a filter on", async () => {
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
    [
      decision({
        asks: ["ask-1"],
        block_id: "d1",
        kind: "person",
        links: [shaped(statementRef("r1"))],
      }),
    ]
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
      decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] }),
      decision({ block_id: "d2", kind: "agent", links: [shaped(statementRef("r1"))] }),
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
// section with no text - is named by its kind's word wherever the view quotes it, in its cell
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
      decision({
        asks: ["ask-1"],
        block_id: "d1",
        kind: "person",
        links: [shaped(statementRef("r1"))],
      }),
      decision({ block_id: "d2", kind: "agent", links: [shaped(statementRef("r2"))], text: "" }),
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
    decisions: "failed",
    results: "failed",
    stage: true,
  });
  try {
    const results = await waitFor(() => alertSaying(view, STATE_LINES.spec_results_unreadable));
    const decisions = await waitFor(() =>
      alertSaying(view, STATE_LINES.issue_decisions_unreadable)
    );
    act(() => within(decisions).getByRole("button", { name: "Retry" }).focus());
    read.decisions.mockResolvedValue(decisionsRead([]));
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.container.textContent).not.toContain(STATE_LINES.issue_decisions_unreadable)
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
