import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../../api/client";
import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type { Decision, IssueAskThreadRow, IssueDetails } from "../../../api/types";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ReferenceNamesProvider } from "../../refs/reference-names";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { issueTree, KEY, READER } from "../decisions-filter/decisions-fixtures";
import { DocumentStages } from "../document-stage";
import { FocusedNodeProvider } from "../focused-node";
import type { TreeNode } from "../issue-tree";
import { NO_FAILED_READS } from "../outline-reads";
import { OutlineRequestProvider } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { Publisher, publishedTree } from "../projection-fixture";
import { treeAnswer } from "../tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswer } from "../tree-read/tree-read";
import { SpecFilterBody } from "./SpecFilterBody";
import {
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

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(() => {
  cleanup();
});

const NO_THREAD = {
  answers: [],
  replies: [],
} as const;

function asThreadRow(id: string, text: string): IssueAskThreadRow {
  return { ...askRead(id, text), ...NO_THREAD };
}

function WatchReads({
  issue,
  watchOutline,
}: {
  readonly issue: IssueDetails;
  readonly watchOutline: boolean;
}) {
  useQuery(issueCoverageQuery(issue.key));
  useQuery(issueDecisionsQuery(issue.key, true));
  useQuery(issueResultsQuery(issue.key));
  useQuery(issueAskThreadsQuery(issue.key));
  useQuery({ ...outlineQuery(issue.key, "conversation"), enabled: watchOutline });
  return null;
}

function renderSpec({
  asks,
  decisions = [],
  outlineFetching,
  root,
  view,
}: {
  readonly asks?: readonly IssueAskThreadRow[];
  readonly decisions?: readonly Decision[];
  readonly outlineFetching: boolean;
  readonly root: TreeNode;
  readonly view: TreeNode;
}) {
  const issue = issueDetails();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const spies = [
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead(["r1", "r2"])),
    spyOn(api, "getIssueResults").mockResolvedValue({ checks: [], issue: KEY }),
    spyOn(api, "getIssueDecisions").mockResolvedValue(decisionsRead(decisions)),
    spyOn(api, "listIssueAskThreads").mockImplementation(() =>
      asks === undefined ? new Promise<never>(() => {}) : Promise.resolve([...asks])
    ),
    spyOn(api, "getIssueTree").mockImplementation(() =>
      outlineFetching ? new Promise<TreeAnswer>(() => {}) : Promise.resolve(treeAnswer([]))
    ),
  ];
  client.setQueryData(whoAmIQuery().queryKey, { email: `${READER}@example.com`, login: READER });
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead(["r1", "r2"]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, { checks: [], issue: KEY });
  if (outlineFetching) void client.prefetchQuery(outlineQuery(KEY, "conversation"));
  client.setQueryData(issueDecisionsQuery(KEY, true).queryKey, decisionsRead(decisions));
  if (asks !== undefined) {
    client.setQueryData(issueAskThreadsQuery(KEY).queryKey, [...asks]);
  }
  const published = publishedTree(root);
  const rendered = render(
    <MemoryRouter initialEntries={[`/issues/${KEY}/spec`]}>
      <QueryClientProvider client={client}>
        <ReferenceNamesProvider>
          <Publisher published={published} />
          <KeymapProvider>
            <MarginProvider>
              <OutlineRequestProvider>
                <FocusedNodeProvider>
                  <DocumentStages>
                    <WatchReads issue={issue} watchOutline={outlineFetching} />
                    <SpecFilterBody
                      context={new Set()}
                      counts={undefined}
                      emptyLine={EMPTY_STATE_LINES.spec}
                      holdsEscape={false}
                      issue={issue}
                      namesLine={NO_FAILED_READS}
                      onEditing={() => {}}
                      published={published}
                      reader={READER}
                      readsSettled={false}
                      root={root}
                      view={view}
                    />
                  </DocumentStages>
                </FocusedNodeProvider>
              </OutlineRequestProvider>
            </MarginProvider>
          </KeymapProvider>
        </ReferenceNamesProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    restore: () => {
      rendered.unmount();
      for (const spy of spies) spy.mockRestore();
    },
    view: rendered,
  };
}

test("at Spec's first drawing with the asks read held and Conversation's outline read still out, a statement under an answered ask draws its because line and heading at once; without the outline read in flight, it does not", () => {
  const r1 = statement("r1", {
    parentLine: { id: "ask-1", line: "Does the built page work for you?", type: "ask" },
  });
  const emptyRoot = issueTree([]);
  const specView = issueTree([r1]);
  const asks = [asThreadRow("ask-1", "Keep the first drawing steady.")];

  // Desktop (1440 px): Conversation's outline read is in flight, so the ask will arrive in `root`.
  const desktop = renderSpec({ asks, outlineFetching: true, root: emptyRoot, view: specView });
  try {
    const because = desktop.view.container.querySelector("[data-spec-because]");
    expect(because?.textContent, "the statement drew no because line at first drawing").toContain(
      "Keep the first drawing steady."
    );
    expect(desktop.view.container.textContent).toContain("Does the built page work for you?");
  } finally {
    desktop.restore();
  }

  // Phone (390 px): Conversation's outline read is not sent, so `root` will never carry the ask.
  const phone = renderSpec({ asks, outlineFetching: false, root: emptyRoot, view: specView });
  try {
    expect(
      phone.view.container.querySelector("[data-spec-because]"),
      "without Conversation's read in flight, Spec invented a because line that 390 px does not draw"
    ).toBeNull();
  } finally {
    phone.restore();
  }
});

test("places held, row 7: while the asks read is out, a statement the decisions read shows gets a person's decision holds a place with no data-spec-because, and a statement with no decision holds none", () => {
  const made = [
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
    }),
  ];
  const tree = issueTree([statement("r1"), statement("r2"), decisionNode("d1")]);
  const drawn = renderSpec({
    asks: undefined,
    decisions: made,
    outlineFetching: false,
    root: tree,
    view: issueTree([statement("r1"), statement("r2")]),
  });
  try {
    const row1 = drawn.view.container.querySelector('[data-node-id="spec#b-r1"]');
    const row2 = drawn.view.container.querySelector('[data-node-id="spec#b-r2"]');
    expect(row1?.querySelector("[data-spec-because]")).toBeNull();
    expect(
      row1?.querySelector('[aria-hidden="true"][data-spec-because-held]'),
      "the statement known to get a because line held no place"
    ).not.toBeNull();
    expect(
      row2?.querySelector('[aria-hidden="true"][data-spec-because-held]'),
      "a statement with no decision held a place"
    ).toBeNull();
  } finally {
    drawn.restore();
  }
});
