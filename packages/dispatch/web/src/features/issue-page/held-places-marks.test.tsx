import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueCheckResults, IssueCoverage, IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { parseIssuePath } from "../refs/routes";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_DOCUMENT,
  SPEC_SLUG,
  specRef,
} from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { IssueTreeOwnerProvider } from "./issue-tree-owner";
import { TWO_THREADS, waitInAct } from "./outline-fixtures";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import {
  AppProviders,
  commentNodes,
  focusedRow,
  inDocument,
  stubReads,
  treeOf,
} from "./slots/stage-fixtures";
import { blockNodeId } from "./tree-drafts";
import { treeNode, treeTuple } from "./tree-read/tree-fixtures";
import { check, result, VERIFICATION_SLUG } from "./verification-filter/verification-fixtures";

/**
 * The places the page holds before their reads answer (R3, "The places held" of the plan), row
 * 6: a row's results and coverage marks. The outline draws its rows once the reads that place
 * them have answered, and the coverage and the results answer later (`slots/body.tsx`): a
 * statement's counts of what serves it and a check's state. Each row that one of those reads can
 * mark is drawn from its first drawing with a slot of the mark's height, and the mark is drawn into
 * that slot, so the row keeps its height and no row under it moves.
 *
 * A unit test's DOM lays nothing out, so here a row's height is what decides it: each element the
 * row draws, with its classes and its style, and the slot's own box, whatever it holds. The boxes
 * the browser lays out are read by row 1 of `c-load-1440` and `c-load-390`
 * (`e2e/issue-page-load.e2e.ts`).
 */

/** The issue's Verification document, beside its spec. */
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The issue with its Verification document in its role, so its checks draw a state. */
function verifiedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), VERIFICATION_DOCUMENT],
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
    queries: [
      {
        query: "verification",
        artifact_id: VERIFICATION_DOCUMENT.id,
        takes: ["check", "result"],
        subtree: `issue:${KEY}`,
      },
    ],
  };
}

/** The block ids of the spec's one statement and of the Verification document's check that
 *  serves it. */
const STATEMENT = "r1";
const CHECK = "c1";

/** The issue's first thread, a head and its reply: the outline's first rows, so the page opens on
 *  the head, and not on a row a case reads the box of. */
const THREAD = TWO_THREADS.slice(0, 2);

/** The rows of the statement and of the check. */
const STATEMENT_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(SPEC_SLUG, STATEMENT)}"]`;
const CHECK_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(VERIFICATION_SLUG, CHECK)}"]`;

/** The coverage read's answer: the statement served by one check and one plan task, with no gap. */
const COVERAGE: IssueCoverage = {
  gaps: [],
  has_plan: true,
  issue: KEY,
  requirements: [
    {
      block_id: STATEMENT,
      checks: 1,
      gaps: [],
      plan_tasks: 1,
      ref: specRef(STATEMENT),
      text: "The page draws the plan.",
    },
  ],
  spec_artifact_id: SPEC_DOCUMENT.id,
};

/** The results read's answer: the check, with a pass recorded against it. */
const RESULTS: IssueCheckResults = {
  checks: [
    check({
      artifact_id: VERIFICATION_DOCUMENT.id,
      block_id: CHECK,
      results: [result({ check_block_id: CHECK, id: "result-1" })],
    }),
  ],
  issue: KEY,
  verification_artifact_id: VERIFICATION_DOCUMENT.id,
};

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
// Every read the stage reaches, answered (`stubReads`): the tree read with a thread of the issue,
// whose head is the outline's first row, which the page opens on, then the statement under the
// issue and the check under the statement it serves, each in its document, and each view's read
// (`?view=`) with an empty view. At 1440 px.
beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "listComments").mockResolvedValue([...THREAD]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined
        ? treeOf(
            [
              ...commentNodes(THREAD),
              treeNode(STATEMENT, "requirement", KEY, {
                ...inDocument(SPEC_DOCUMENT),
                text: "The page draws the plan.",
              }),
              treeNode(CHECK, "check", STATEMENT, {
                ...inDocument(VERIFICATION_DOCUMENT),
                text: "The page draws the plan in one frame.",
              }),
            ],
            [treeTuple("serves", CHECK, STATEMENT)]
          )
        : treeOf([])
    ),
  ];
  restoreViewport = stubViewport(DESKTOP_WIDTH);
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The page's stage at Conversation's address for `issue`, as the page's root draws it
 *  (`IssueOutlinePage.tsx`): under the page's providers and the tree's owner. */
function PageStage({ issue }: { readonly issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <IssueTreeOwnerProvider issue={issue}>
        <StageSlot issue={issue} route={route} />
      </IssueTreeOwnerProvider>
    </ProvidersSlot>
  );
}

/** What decides an element's box where nothing is laid out: its tag, its classes, its style and
 *  the same of each element it holds, but of `slot`, whose own box alone is read, whatever it
 *  holds. */
interface Box {
  readonly boxes: readonly Box[];
  readonly className: string | null;
  readonly style: string | null;
  readonly tag: string;
}

function boxOf(element: Element, slot: Element | null): Box {
  return {
    boxes: element === slot ? [] : [...element.children].map((child) => boxOf(child, slot)),
    className: element.getAttribute("class"),
    style: element.getAttribute("style"),
    tag: element.tagName,
  };
}

/** The row `selector` finds in `container`, which must be drawn. */
function rowOf(container: HTMLElement, selector: string): HTMLElement {
  const row = container.querySelector<HTMLElement>(selector);
  if (row === null) throw new Error(`the outline drew no row ${selector}`);
  return row;
}

/** The slot a statement's row holds for its counts. */
function countsSlotOf(row: HTMLElement): HTMLElement | null {
  return row.querySelector<HTMLElement>("[data-node-counts]");
}

/** The line of a row's margin that draws its state (`NODE_ROW_LAYOUT.marginState`): the one under
 *  the line of its tag, its author and its time. */
function stateLineOf(row: HTMLElement): Element | null {
  return row.querySelector("[data-node-margin]")?.children[1] ?? null;
}

test("a row's results and coverage marks draw in a slot of the mark's height, and the row's height is the same before and after those reads answer", async () => {
  const coverage = Promise.withResolvers<IssueCoverage>();
  const results = Promise.withResolvers<IssueCheckResults>();
  stubs.push(
    spyOn(api, "getIssueCoverage").mockReturnValue(coverage.promise),
    spyOn(api, "getIssueResults").mockReturnValue(results.promise)
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <PageStage issue={verifiedIssue()} />
      </AppProviders>
    </MemoryRouter>
  );
  try {
    // The rows are drawn with the coverage and the results still out: they wait on neither.
    await waitFor(() => {
      expect(client.isFetching(), "a read other than the coverage and the results is out").toBe(2);
      rowOf(view.container, STATEMENT_ROW);
      rowOf(view.container, CHECK_ROW);
    });
    await waitInAct(50);
    const statement = rowOf(view.container, STATEMENT_ROW);
    const checkRow = rowOf(view.container, CHECK_ROW);
    const countsSlot = countsSlotOf(statement);
    expect(
      countsSlot,
      "the statement's row held no slot for its counts with the coverage out"
    ).not.toBeNull();
    expect(countsSlot?.textContent, "the counts' slot drew counts with the coverage out").toBe("");
    expect(
      countsSlot?.classList.contains("min-h-4") && countsSlot.classList.contains("text-xs"),
      "the counts' slot holds no line of the counts' height (`text-xs`, 16 px)"
    ).toBe(true);
    const stateLine = stateLineOf(checkRow);
    expect(
      stateLine?.querySelector("[data-node-state]")?.getAttribute("data-node-state"),
      "the check's row held no state line with the results out"
    ).toBe("no-result");
    const before = {
      countsSlot: countsSlot === null ? null : boxOf(countsSlot, countsSlot),
      stateLine: stateLine === null ? null : boxOf(stateLine, stateLine),
      statement: boxOf(statement, countsSlot),
    };

    await act(async () => {
      coverage.resolve(COVERAGE);
      results.resolve(RESULTS);
    });
    // The page opens once every read the rows come from has answered, on the thread's head.
    await waitFor(() => {
      expect(client.isFetching(), "a read is still out").toBe(0);
      expect(
        countsSlotOf(statement)?.textContent,
        "the statement's counts were not drawn once the coverage answered"
      ).toContain("1");
      expect(
        stateLineOf(checkRow)?.querySelector("[data-node-state]")?.getAttribute("data-node-state"),
        "the check's state was not drawn once the results answered"
      ).not.toBe("no-result");
      expect(focusedRow(), "the page opened on another row than the thread's head").toBe(
        THREAD[0]?.id
      );
    });
    await waitInAct(50);

    expect(
      rowOf(view.container, STATEMENT_ROW),
      "the statement's row was drawn again as another element"
    ).toBe(statement);
    expect(
      countsSlotOf(statement),
      "the statement's counts were drawn in no slot its row held before the coverage answered"
    ).toBe(countsSlot);
    expect(
      stateLineOf(checkRow),
      "the check's state was drawn in no line its row held before the results answered"
    ).toBe(stateLine);
    expect(
      {
        countsSlot: countsSlot === null ? null : boxOf(countsSlot, countsSlot),
        stateLine: stateLine === null ? null : boxOf(stateLine, stateLine),
        statement: boxOf(statement, countsSlot),
      },
      "a box of the statement's row, of its counts' slot or of the check's state line changed when the coverage and the results answered"
    ).toEqual(before);
    view.unmount();
  } finally {
    client.clear();
  }
});
