import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { api } from "../../api/client";
import type { Decision, DecisionKeep, IssueDecisions, IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { DecisionCard, DecisionList } from "./DecisionCard";
import type { DecisionMarginItem, MarkPlacement } from "./useMarginItems";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

function item(decision: Partial<Decision>): DecisionMarginItem {
  const full: Decision = {
    asks: [],
    block_id: "d-1",
    changed_at: "2026-10-02T00:00:00Z",
    changed_version: 2,
    heading: "Store notices in a table",
    kept: null,
    kind: "agent",
    links: [],
    ref: "dispatch://CORE-1/artifact/decisions-md#b-d-1",
    replaced: false,
    text: "A table keeps them through a restart.",
    text_sha256: "sum-d-1",
    unsure: false,
    waiting_review: true,
    ...decision,
  };
  return {
    blockId: "p-plan",
    // The card draws the reading Go made of the decision's text and parses none of it itself,
    // so a decision this test invents carries the reading the page's own engine makes of that
    // text (`__tests__/display-trees`).
    decision: { ...full, text_tree: displayTree(full.text) },
    id: `decision:${full.block_id}:p-plan`,
    kind: "decision",
  };
}

function renderCard(decision: Partial<Decision>, onSelect: (id: string) => void = () => {}) {
  return render(
    <MemoryRouter>
      <DecisionCard issueKey="CORE-1" item={item(decision)} onSelect={onSelect} selected={false} />
    </MemoryRouter>
  );
}

test("a decision card names the decision, who made it, and opens it in decisions.md", async () => {
  const view = renderCard({});
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table" });
    expect(card).toHaveAttribute("data-margin-item", "decision:d-1:p-plan");
    expect(card.getAttribute("data-decision-unsure")).toBeNull();
    expect(screen.getByText("Agent's decision")).toBeDefined();
    expect(await screen.findByText("A table keeps them through a restart.")).toBeDefined();
    const link = screen.getByRole("link", { name: "Store notices in a table" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/artifacts/decisions-md#b-d-1");
  } finally {
    view.unmount();
  }
});

test("an unsure decision is drawn in the unsure colour and says so; a person's says whose", () => {
  const view = renderCard({ kind: "person", unsure: true });
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table" });
    expect(card).toHaveAttribute("data-decision-unsure", "true");
    expect(screen.getByText("Unsure")).toBeDefined();
    expect(screen.getByText("A person's decision")).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("selecting a decision card selects its margin item", async () => {
  const user = userEvent.setup();
  const selected: string[] = [];
  const view = renderCard({}, (id) => selected.push(id));
  try {
    await user.click(screen.getByRole("article", { name: "Decision: Store notices in a table" }));
    expect(selected).toEqual(["decision:d-1:p-plan"]);
  } finally {
    view.unmount();
  }
});

const DOCUMENT_ID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
const NO_PLACEMENTS: ReadonlyMap<string, MarkPlacement> = new Map();

/** The margin's decisions as `useMarginItems` makes them, from the decisions read the page
 *  holds: one decision beside two blocks of the spec is two cards. */
function TwoCopies(): ReactNode {
  const read = useQuery(issueDecisionsQuery("CORE-1", false));
  const items = (read.data?.decisions ?? []).flatMap((decision) =>
    ["p-plan", "p-test"].map((blockId) => ({
      blockId,
      decision,
      id: `decision:${decision.block_id}:${blockId}`,
      kind: "decision" as const,
    }))
  );
  return (
    <DecisionList
      blockPlacements={NO_PLACEMENTS}
      decisions={items}
      hoveredItemId={undefined}
      isClosed={false}
      issueKey="CORE-1"
      onSelect={() => {}}
      selectedItemId={undefined}
    />
  );
}

// R21: a decision beside two statements draws two cards, each with Keep and Change. Both read
// the one decisions read, so a Keep pressed on one is drawn on both. The document the Keep names
// comes from the issue read and the decisions read the page holds, and the margin reads neither.
test("Keep on one copy settles the other", async () => {
  const user = userEvent.setup();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  client.setQueryData(["issue", "CORE-1"], {
    artifacts: [{ id: DOCUMENT_ID, kind: "doc", primary: false, slug: "decisions-md" }],
    key: "CORE-1",
  } as unknown as IssueDetails);
  client.setQueryData<IssueDecisions>(["decisions", "CORE-1"], {
    decisions: [item({}).decision],
    document: "dispatch://CORE-1/artifact/decisions-md",
    open_questions: [],
  });
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: DOCUMENT_ID,
    block_id: "d-1",
    changed_version: 2,
    kept_at: "2026-10-03T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  const view = render(
    <MemoryRouter>
      <TwoCopies />
    </MemoryRouter>,
    {
      wrapper: ({ children }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );
  try {
    const copies = screen.getAllByRole("article", { name: "Decision: Store notices in a table" });
    expect(copies).toHaveLength(2);
    const keeps = screen.getAllByRole("button", { name: "Keep" });
    expect(keeps).toHaveLength(2);
    expect(screen.getAllByRole("button", { name: "Change" })).toHaveLength(2);
    await user.click(keeps[0] as HTMLElement);
    await waitFor(() => expect(screen.queryAllByRole("button", { name: "Keep" })).toHaveLength(0));
    expect(keep).toHaveBeenCalledTimes(1);
    expect(keep.mock.calls[0]?.slice(0, 3)).toEqual([DOCUMENT_ID, "d-1", "sum-d-1"]);
    expect(screen.getAllByText("Kept by alice")).toHaveLength(2);
  } finally {
    view.unmount();
    keep.mockRestore();
    client.clear();
  }
});
