import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { installResizeObserver, type ResizeControl } from "../../__tests__/scroll-box";
import { issueAskThreadsQuery, issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DecisionsFilterBody } from "./decisions-filter/DecisionsFilterBody";
import {
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
} from "./decisions-filter/decisions-fixtures";
import { DocumentStages } from "./document-stage";
import { FocusedNodeProvider } from "./focused-node";
import { artifactsBySlug } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { NoTooltips } from "./NoTooltips";
import { NO_FAILED_READS } from "./outline-reads";
import { OutlineRequestProvider } from "./outline-request";
import { EMPTY_STATE_LINES } from "./page-words";
import { PlanFilterBody } from "./plan-filter/PlanFilterBody";
import { presetView } from "./presets";
import { Publisher, publishedTree } from "./projection-fixture";
import {
  commentsUnder,
  drawOutline,
  IssueScope,
  idsOf,
  type LayoutByHand,
  layoutByHand,
  type TasksByHand,
  tasksByHand,
} from "./slices-fixtures";
import { ViewStage } from "./slots/stage";
import { AppProviders, focusedRow } from "./slots/stage-fixtures";
import { SpecFilterBody } from "./spec-filter/SpecFilterBody";
import {
  coverageRead,
  decisionsRead as specDecisionsRead,
  issueDetails as specIssue,
  statement,
} from "./spec-filter/spec-fixtures";
import { blockNodeId } from "./tree-drafts";
import { treeAnswer } from "./tree-read/tree-fixtures";
import { outlineQuery } from "./tree-read/tree-read";

/**
 * A view's first focus moves no page (R3: nothing moves after the page draws it, but by the
 * reader's own act). A load of a view's address that names no row gives the view's first row the
 * focus (`slots/stage.tsx`'s `ViewStage`), without a scroll; but the row places itself on screen
 * by the page's one rule as the focus comes to it, and again whenever it grows
 * (`NodeRow.tsx`'s `useRowOnScreen`), so the page scrolled to it once it was drawn. CI run
 * 45878460106 read it on the seed: at 390 px, where the issue's children stand above the view's
 * rows, Spec, Plan, Decisions and Graph scrolled by 1,185 to 1,421 px (the view row, which stays
 * at the top, by 183 of them); at 1440 px Spec by 33 px, Plan by 13 and Decisions by 68; and each
 * again by 20 to 42 px as that row grew when a later read answered. Verification's rows place
 * themselves by no such rule, and it scrolled by none of these. The outline already opens on its
 * first row so (`IssueTree.tsx`'s `unplaced`).
 *
 * The row the view opens on keeps the focus where it stands until the reader's first act: it is
 * placed neither as it takes the focus nor as it grows, and the reader's first key places the row
 * it moves to as every key does.
 *
 * happy-dom lays nothing out, so the page is laid out by hand, each row 100 px tall from 1,300 px
 * down the page in a window 600 px tall, as the children's section left the view's rows at 390 px
 * (`layoutByHand`); the rows' size observers are called by hand (`installResizeObserver`), and the
 * slices are tasks run by hand (`tasksByHand`).
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** How far down the page the view's first row stands: under the issue's children at 390 px. */
const BELOW_THE_CHILDREN = 1300;

let layout: LayoutByHand;
let sizes: ResizeControl;
let tasks: TasksByHand;
beforeEach(() => {
  layout = layoutByHand({ band: true, top: BELOW_THE_CHILDREN });
  sizes = installResizeObserver();
  tasks = tasksByHand();
});
afterEach(() => {
  tasks.restore();
  sizes.restore();
  layout.restore();
});

/** A client whose reads stay as a case sets them. */
function settledClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** The plan document's slug, which a task's node id and address name. */
const PLAN = "plan-md";

/** The Plan view's chart over 12 tasks, `t00` on, every read answered, in the stage. */
function drawPlan(): RenderResult {
  const plan = idsOf(12, "t").map((blockId, position) =>
    node({
      id: blockNodeId(PLAN, blockId),
      position,
      reference: `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`,
      text: `Task ${blockId}`,
      type: "plan_task",
    })
  );
  const root = node({ children: plan, held: plan.length, id: KEY, type: "issue" });
  const published = publishedTree(root);
  return render(
    <ReferenceNamesProvider>
      <MemoryRouter>
        <QueryClientProvider client={settledClient()}>
          <KeymapProvider>
            <MarginProvider>
              <FocusedNodeProvider>
                <IssueScope />
                <Publisher published={published} />
                <ViewStage>
                  <PlanFilterBody
                    artifacts={new Map()}
                    context={new Set()}
                    emptyLine={EMPTY_STATE_LINES.plan}
                    focusReveal
                    holdsEscape={false}
                    isClosed={false}
                    namesLine={NO_FAILED_READS}
                    onEditing={() => {}}
                    plan={plan}
                    published={published}
                    reads={{ state: "answered" }}
                    reveal={undefined}
                    root={root}
                    rootRow={false}
                  />
                </ViewStage>
              </FocusedNodeProvider>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    </ReferenceNamesProvider>
  );
}

/** The Decisions view's list of 12 open questions that wait on the reader, `q00` on, every read
 *  answered, in the stage. */
function drawDecisions(): RenderResult {
  const client = settledClient();
  client.setQueryData(["decisions", KEY], decisionsRead([]));
  client.setQueryData(["asks", KEY, "thread"], []);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  const root = issueTree(idsOf(12, "q").map((id) => openAsk(id)));
  const published = publishedTree(root);
  return render(
    <MemoryRouter>
      <AppProviders client={client}>
        <Publisher published={published} />
        <IssueScope />
        <NoTooltips>
          <ViewStage>
            <DecisionsFilterBody
              artifacts={artifactsBySlug(issue)}
              emptyLine={EMPTY_STATE_LINES.decisions}
              holdsEscape={false}
              isClosed={false}
              issue={issue}
              namesLine={NO_FAILED_READS}
              published={published}
              reader={READER}
              root={root}
              view={presetView(root, "decisions", "", new Set()).root}
            />
          </ViewStage>
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
}

/** The reads the page's tree fills, each by an observer as the tree's own (`useIssueTree.ts`),
 *  which the Spec body only watches. */
function SpecTreeReads({ issue }: { issue: IssueDetails }): ReactNode {
  useQuery(issueCoverageQuery(issue.key));
  useQuery(issueDecisionsQuery(issue.key, true));
  useQuery(issueResultsQuery(issue.key));
  useQuery(issueAskThreadsQuery(issue.key));
  useQuery(outlineQuery(issue.key, "conversation"));
  return null;
}

/** The Spec view of 12 statements, `r00` on, with every read it waits on answered, in the
 *  stage. */
function drawSpec(): RenderResult {
  const blocks = idsOf(12, "r");
  const issue = specIssue();
  const client = settledClient();
  client.setQueryData(["decisions", KEY], specDecisionsRead([]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, {
    checks: [],
    issue: KEY,
    verification_artifact_id: "",
  });
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead(blocks));
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeAnswer([]));
  const tree: TreeNode = issueTree(blocks.map((block) => statement(block)));
  const published = publishedTree(tree);
  return render(
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <Publisher published={published} />
        <MemoryRouter>
          <KeymapProvider>
            <MarginProvider>
              <OutlineRequestProvider>
                <FocusedNodeProvider>
                  <IssueScope />
                  <SpecTreeReads issue={issue} />
                  <ViewStage>
                    <DocumentStages>
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
                        readsSettled
                        root={tree}
                        view={presetView(tree, "spec", "", new Set()).root}
                      />
                    </DocumentStages>
                  </ViewStage>
                </FocusedNodeProvider>
              </OutlineRequestProvider>
            </MarginProvider>
          </KeymapProvider>
        </MemoryRouter>
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
}

/** Graph's outline of 12 comments, `c00` on, drawn in the stage as Graph's slot draws it: the
 *  outline takes no first focus of its own there (`rootRow` off), and the stage gives it. */
function drawGraph(): RenderResult {
  return drawOutline({ root: commentsUnder(idsOf(12)), rootRow: false }, { stage: true }).view;
}

/** Each view whose rows place themselves, how it is drawn, and its first two rows' node ids. */
const VIEWS = [
  { draw: drawSpec, name: "Spec", rows: ["spec#b-r00", "spec#b-r01"] },
  { draw: drawPlan, name: "Plan", rows: [blockNodeId(PLAN, "t00"), blockNodeId(PLAN, "t01")] },
  { draw: drawDecisions, name: "Decisions", rows: ["q00", "q01"] },
  { draw: drawGraph, name: "Graph", rows: ["c00", "c01"] },
] as const;

for (const { draw, name, rows } of VIEWS) {
  test(`${name}'s first row takes the focus where it stands, as it grows too, and the reader's first key places the row it moves to`, async () => {
    const user = userEvent.setup();
    const [first, second] = rows;
    const view = draw();
    try {
      await waitFor(() => expect(focusedRow(), `${name} gave its first row no focus`).toBe(first));
      const row = document.activeElement?.closest<HTMLElement>("[data-node-row]");
      if (row === null || row === undefined) throw new Error(`${name} focused no row`);
      expect(layout.scrolls, `${name}'s first focus scrolled the page to its row`).toEqual([]);

      // A later read answers, and the row grows under the focus.
      sizes.resize(row);
      expect(layout.scrolls, `${name}'s first row placed itself as it grew`).toEqual([]);

      await user.keyboard("j");
      expect(focusedRow(), `j on ${name}'s first row did not move to the next`).toBe(second);
      expect(
        layout.scrolls.length,
        `${name}'s row the reader's key moved to was not brought on screen`
      ).toBeGreaterThan(0);
    } finally {
      view.unmount();
    }
  });
}
