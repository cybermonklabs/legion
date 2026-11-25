import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { renderToString } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { answerQuestionOf, NAMED_QUESTION } from "../../__tests__/question-answers";
import { installResizeObserver } from "../../__tests__/scroll-box";
import { api } from "../../api/client";
import type {
  AskRead,
  Decision,
  DecisionKeep,
  IssueDecisions,
  IssueDetails,
} from "../../api/types";
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
    documentId: "artifact-decisions",
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

// A heading is markdown its writer wrote, and it names the card and words a link, neither of
// which holds markup: both read the heading's plain-text projection, so a link in it is its
// words there and never the link's source. Drawn as the characters it is, the card would be
// named `Decision: Keep **notices** in [a table](https://example.test/table)`.
test("a heading that holds markdown names the card and its link in plain words", async () => {
  const view = renderCard({
    heading: "Keep **notices** in [a table](https://example.test/table)",
  });
  try {
    expect(
      await screen.findByRole("article", { name: "Decision: Keep notices in a table" })
    ).toBeDefined();
    expect(screen.getByRole("link", { name: "Keep notices in a table" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts/decisions-md#b-d-1"
    );
  } finally {
    view.unmount();
  }
});

// The passage is a record of the issue's `decisions.md`, so the card hands the renderer that
// document as the passage's source and a `#id` in it names a question block of that document,
// as it does in the Decisions panel. With no source the `#q2` stays the characters it is and
// the document's question map is never read.
test("a `#id` in the passage names the question of the decisions document", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "artifact-decisions",
    questions: [{ ask: "ask-2", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-2" }],
  });
  // The link reads its ask for a name; the read stays out, which is no part of this row.
  const getAsk = spyOn(api, "getAsk").mockReturnValue(Promise.withResolvers<AskRead>().promise);
  const view = renderCard({ text: "Settled by #q2 before the rollout." });
  try {
    const linked = () =>
      [...view.container.querySelectorAll("[data-decision-text] [data-dispatch-ref]")].map(
        (reference) => reference.getAttribute("data-dispatch-ref")
      );
    await waitFor(() => expect(linked()).toEqual(["dispatch://CORE-1/ask/ask-2"]));
    expect(getReferenceQuestions).toHaveBeenCalledWith("artifact", "artifact-decisions");
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
  }
});

// The heading is a record of `decisions.md` too, so the card's name reads a `#id` in it as the
// question it names in that document. Handed no record, the name keeps `#q2`.
test("a #id in the heading names the question of the decisions document in the card's name", async () => {
  const answers = answerQuestionOf("artifact", "artifact-decisions");
  const view = renderCard({ heading: "Settle #q2 first" });
  try {
    expect(
      await screen.findByRole("article", { name: `Decision: Settle ${NAMED_QUESTION} first` })
    ).toBeDefined();
  } finally {
    view.unmount();
    answers.restore();
  }
});

// Until the reader's engine has answered - the first paint, and for good where its chunk never
// loads - the card is named after its heading as written, never `Decision: ` with no words and an
// empty link (the reading list's `decision-heading-words`). A paint with no effects run is that
// state.
test("before the engine answers, the card is named after its heading as written", () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const html = renderToString(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <DecisionCard issueKey="CORE-1" item={item({})} onSelect={() => {}} selected={false} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  expect(html).toContain('aria-label="Decision: Store notices in a table"');
  expect(html).toContain(">Store notices in a table</a>");
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
      documentId: DOCUMENT_ID,
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

// The decisions' region stands as tall as its lowest card and follows a card's height the moment
// the card resizes, with no render of the list: a region that read the cards' heights at render
// kept a card's earlier height until an unrelated render - a hover - read it again, and the
// margin's scroll moved under the reader's press, as the anchored comments' did.
test("the decisions' region follows its card's height when the card resizes, with no render", () => {
  const resize = installResizeObserver();
  let height = 300;
  const decision = item({});
  const view = render(
    <MemoryRouter>
      <DecisionList
        blockPlacements={new Map([["p-plan", { pos: 5, top: 120 }]])}
        decisions={[decision]}
        hoveredItemId={undefined}
        isClosed={false}
        issueKey="CORE-1"
        onSelect={() => {}}
        selectedItemId={undefined}
      />
    </MemoryRouter>
  );
  try {
    const region = screen.getByRole("region", { name: "Decisions" });
    const wrapper = region.firstElementChild;
    if (!(wrapper instanceof HTMLElement)) throw new Error("the card has no wrapper");
    Object.defineProperty(wrapper, "offsetHeight", { configurable: true, get: () => height });
    resize.resize(wrapper);
    expect(region.style.minHeight).toBe(`${120 + 300 + 8}px`);
    height = 250;
    resize.resize(wrapper);
    expect(region.style.minHeight).toBe(`${120 + 250 + 8}px`);
  } finally {
    view.unmount();
    resize.restore();
  }
});
