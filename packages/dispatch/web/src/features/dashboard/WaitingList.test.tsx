import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import type { IssueAskRow, IssueDetails } from "../../api/types";
import { FocusedNodeProvider, useFocusedNode } from "../issue-page/focused-node";
import { DETAILS_ATTRIBUTE } from "../issue-page/margin-slot";
import { NoTooltips } from "../issue-page/NoTooltips";
import { CONTROL_LABELS, NODE_STATE_WORDS, PRESET_NAMES } from "../issue-page/page-words";
import { AppProviders } from "../issue-page/slots/stage-fixtures";
import { NOTHING_PUBLISHED } from "../refs/reference-names";
import { buildDispatchReference } from "../refs/routes";
import asksOpenBody from "./fixtures/asks-open.json";
import issueBody from "./fixtures/issue.json";
import type { WaitingItem } from "./useWaitingItems";
import { type DashboardPage, WaitingList } from "./WaitingList";

// A stale approval of what waits (dispatch://INFRA-5702/spec#b-r-in-place, R5): the one approval
// control, inside `NoTooltips`, in the page's words for the latest version. And an ask item on
// the new page, which sets the page's focused node to its ask when the focus goes inside it
// (dispatch://INFRA-5702/spec#b-r-reads, c-dashboard-new (4)).

const ISSUE: IssueDetails = issueBody as unknown as IssueDetails;

/** What the page's focused node is, drawn where a case can read it. */
function FocusedProbe(): ReactNode {
  const focused = useFocusedNode();
  return (
    <output data-testid="focused-probe">
      {focused === undefined ? "" : JSON.stringify(focused)}
    </output>
  );
}

/** The list of `items` on `page`, under the page's providers and its focused node, beside a
 *  control of the page and the control that opens Details (`DETAILS_ATTRIBUTE`). */
function drawList(items: readonly WaitingItem[], page: DashboardPage) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <AppProviders client={client}>
        <FocusedNodeProvider>
          <NoTooltips>
            <WaitingList
              decisionsDocument={undefined}
              drawn={5}
              issue={ISSUE}
              items={items}
              names={{ excerptOf: () => "", published: NOTHING_PUBLISHED }}
              onMore={() => {}}
              page={page}
              viewer="alice"
            />
          </NoTooltips>
          <FocusedProbe />
          <button type="button">a control elsewhere</button>
          <button {...{ [DETAILS_ATTRIBUTE]: "" }} type="button">
            the Details control
          </button>
        </FocusedNodeProvider>
      </AppProviders>
    </MemoryRouter>
  );
}

test("a stale approval draws the chip with the version approved and the newer one", () => {
  const base = (ISSUE.artifacts ?? []).find(
    (artifact) => artifact.id === ISSUE.verification_artifact_id
  );
  if (base === undefined) throw new Error("the issue fixture holds no Verification document");
  const document = {
    ...base,
    approval: {
      at: "2026-11-25T10:00:00Z",
      by: { id: "alice", kind: "user" },
      latest_version: 3,
      state: "stale",
      version: 2,
    },
  } as typeof base;
  const item: WaitingItem = {
    at: "2026-11-25T10:00:00Z",
    document,
    id: document.id,
    kind: "stale",
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <AppProviders client={client}>
        <NoTooltips>
          <WaitingList
            decisionsDocument={undefined}
            drawn={5}
            issue={ISSUE}
            items={[item]}
            names={{ excerptOf: () => "", published: NOTHING_PUBLISHED }}
            onMore={() => {}}
            page="today"
            viewer="alice"
          />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    const drawn = screen.getByTestId(`dashboard-item-stale-${document.id}`);
    expect(drawn.textContent).toContain(PRESET_NAMES.verification);
    expect(
      within(drawn).getByRole("button", {
        name: `Approved v2 · ${CONTROL_LABELS.approval_latest} v3`,
      })
    ).toBeTruthy();
    expect(within(drawn).getByRole("button", { name: "Approve v3" })).toBeTruthy();
    expect(within(drawn).getByRole("button", { name: "Request changes" })).toBeTruthy();
    expect(screen.getByTestId("dashboard-waiting-count").textContent).toBe("1");
  } finally {
    view.unmount();
  }
});

// On the new page an ask item that takes the focus - the focus goes inside it, as to its answer
// box - sets the page's focused node to that ask, as `FocusedNode` reads a node: by the ask's own
// id, as the ask it is, by its address, its words, who asked it and how it stands. Details draws
// that node's facts (part M, dispatch://INFRA-3948), which this case leaves to part M.
test("on the new page an ask item that takes the focus sets the page's focused node to its ask", () => {
  const ask = {
    ...(asksOpenBody[0] as unknown as IssueAskRow),
    issue_key: ISSUE.key,
  } satisfies IssueAskRow;
  const item: WaitingItem = { ask, at: ask.created_at, id: ask.id, kind: "question" };
  const view = drawList([item], "new");
  try {
    const probe = screen.getByTestId("focused-probe");
    expect(probe.textContent, "the focused node was set before the focus moved").toBe("");
    const drawn = screen.getByTestId(`dashboard-item-question-${ask.id}`);
    const answer = drawn.querySelector<HTMLElement>("[data-ask-answer]");
    if (answer === null) throw new Error("the question's item draws no answer box");
    act(() => answer.focus());
    expect(JSON.parse(probe.textContent ?? "")).toEqual({
      author: ask.author,
      id: ask.id,
      item: { id: ask.id, kind: "ask" },
      reference: buildDispatchReference({ id: ask.id, key: ISSUE.key, kind: "ask" }),
      source: { id: ask.id, issue: ISSUE.key, kind: "ask" },
      state: NODE_STATE_WORDS.ask.open,
      text: ask.question,
      tree: ask.question_tree,
      type: "ask",
    });

    // The focus going to Details keeps the ask; going anywhere else hands the page back to the
    // issue itself, as the outline does.
    act(() => screen.getByRole("button", { name: "the Details control" }).focus());
    expect(probe.textContent, "the focus going to Details let go of the ask").not.toBe("");
    act(() => answer.focus());
    act(() => screen.getByRole("button", { name: "a control elsewhere" }).focus());
    expect(probe.textContent, "the focus leaving the item kept the ask").toBe("");
  } finally {
    view.unmount();
  }
});

// Today's page draws no Details panel, so its ask item sets no focused node.
test("on today's page an ask item that takes the focus sets no focused node", () => {
  const ask = {
    ...(asksOpenBody[0] as unknown as IssueAskRow),
    issue_key: ISSUE.key,
  } satisfies IssueAskRow;
  const item: WaitingItem = { ask, at: ask.created_at, id: ask.id, kind: "question" };
  const view = drawList([item], "today");
  try {
    const answer = screen
      .getByTestId(`dashboard-item-question-${ask.id}`)
      .querySelector<HTMLElement>("[data-ask-answer]");
    if (answer === null) throw new Error("the question's item draws no answer box");
    act(() => answer.focus());
    expect(screen.getByTestId("focused-probe").textContent).toBe("");
  } finally {
    view.unmount();
  }
});
