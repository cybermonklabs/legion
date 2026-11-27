import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderToString } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { answerQuestionOf, NAMED_QUESTION } from "../../__tests__/question-answers";
import { installResizeObserver } from "../../__tests__/scroll-box";
import { api } from "../../api/client";
import type { AskRead, DecisionEntry } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { DecisionCard, DecisionList } from "./DecisionCard";
import type { DecisionMarginItem } from "./useMarginItems";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const words =
  ":::decision{#d-1}\nStore notices in a table.\n\nA table keeps them through a restart.\n:::\n";

function item(decision: Partial<DecisionEntry>): DecisionMarginItem {
  const full: DecisionEntry = {
    answer: null,
    answer_state: null,
    answered_at: null,
    answered_by: null,
    changed_at: "2026-10-02T00:00:00Z",
    changed_version: 2,
    decided_by: { id: "session-1", kind: "session" },
    declined: false,
    declined_note: null,
    document: {
      artifact_id: "choices-id",
      block: "CORE-1/choices-md#d-1",
      block_id: "d-1",
      ref: "dispatch://CORE-1/artifact/choices-md",
    },
    from_ask: [],
    kept: null,
    kept_by: null,
    kept_text_sum: null,
    kind: "agent",
    multiple: null,
    node: { id: "CORE-1/choices-md#d-1", kind: "block" },
    options: null,
    question: null,
    readings: [],
    ref: "dispatch://CORE-1/artifact/choices-md#b-d-1",
    replaced: false,
    resolution: null,
    selected: null,
    shaped: [],
    text: words,
    text_sha256: "sum-d-1",
    uncertainty: null,
    uncertainty_reason: null,
    urgency: null,
    waiting_review: true,
    ...decision,
  };
  return {
    blockId: "p-plan",
    // The card draws the reading Go made of the decision's text and parses none of it itself,
    // so a decision this test invents carries the reading the page's own engine makes of that
    // text (`__tests__/display-trees`).
    decision: { ...full, text_tree: displayTree(full.text) },
    documentId: full.document?.artifact_id,
    id: `decision:${full.node.id}:CORE-1/spec#p-plan`,
    kind: "decision",
  };
}

function renderCard(decision: Partial<DecisionEntry>, onSelect: (id: string) => void = () => {}) {
  return render(
    <MemoryRouter>
      <DecisionCard item={item(decision)} onSelect={onSelect} selected={false} />
    </MemoryRouter>
  );
}

test("a decision card names the decision, who made it, and opens the node that holds it", async () => {
  const view = renderCard({});
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table." });
    expect(card).toHaveAttribute(
      "data-margin-item",
      "decision:CORE-1/choices-md#d-1:CORE-1/spec#p-plan"
    );
    expect(card.getAttribute("data-decision-unsure")).toBeNull();
    expect(screen.getByText("Agent's decision")).toBeDefined();
    // The preview draws the head of the decision's own words, its blocks run together.
    expect(await screen.findByText(/A table keeps them through a restart\./)).toBeDefined();
    const link = screen.getByRole("link", { name: "Store notices in a table." });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/artifacts/choices-md#b-d-1");
  } finally {
    view.unmount();
  }
});

// A decision's name is the words of its first block, as the server's read of a decision and the
// Decisions tab name it: a heading is named by its words, and the `###` that marks it a heading
// in the decision's markdown is none of them.
test("a decision that opens with a heading is named by the heading's words", () => {
  const view = renderCard({ text: ":::decision{#d1}\n### H\n\nWhy.\n:::\n" });
  try {
    expect(screen.getByRole("article", { name: "Decision: H" })).toBeDefined();
    expect(screen.getByRole("link", { name: "H" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts/choices-md#b-d-1"
    );
  } finally {
    view.unmount();
  }
});

test("an unsure decision is drawn in the unsure colour and says so; a person's says whose", () => {
  const view = renderCard({
    kind: "person",
    node: { id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", kind: "ask" },
    question: "Store notices in a table?",
    ref: "dispatch://CORE-1/ask/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    uncertainty: "high",
  });
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table?" });
    expect(card).toHaveAttribute("data-decision-unsure", "true");
    expect(screen.getByText("Unsure")).toBeDefined();
    expect(screen.getByText("A person's decision")).toBeDefined();
    expect(screen.getByRole("link", { name: "Store notices in a table?" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/asks/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
    );
  } finally {
    view.unmount();
  }
});

// A `decision` block is a person's decision where its decided_by names a person: kind is
// `agent` for every block, the node's shape, and the card names who decided from decided_by.
test("a decision block a person decided says it is a person's decision", () => {
  const view = renderCard({ decided_by: { id: "alice", kind: "person" }, waiting_review: false });
  try {
    expect(screen.getByText("A person's decision")).toBeDefined();
    expect(screen.queryByText("Agent's decision")).toBeNull();
  } finally {
    view.unmount();
  }
});

// The conversion of a decisions document moves a decision's `Unsure:` paragraph into its node's
// `uncertainty` and `uncertainty_reason`, so its words hold that paragraph no more. The preview
// runs the doubt on after the words, with the paragraph's words, as it ran the paragraph.
test("a converted decision's preview ends with the doubt its node holds", async () => {
  const view = renderCard({
    text: ':::decision{#d-1 uncertainty="high" uncertainty_reason="the index may be slow."}\nStore notices in a table.\n\nA table keeps them through a restart.\n:::\n',
    uncertainty: "high",
    uncertainty_reason: "the index may be slow.",
  });
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table." });
    await waitFor(() =>
      expect(card.querySelector("[data-markdown-preview]")?.textContent).toBe(
        "Store notices in a table. A table keeps them through a restart. Unsure: the index may be slow."
      )
    );
  } finally {
    view.unmount();
  }
});

// A decision whose words still hold their `Unsure:` paragraph previews that paragraph, and the
// doubt its node holds is not run on after it a second time.
test("an unconverted decision's preview holds its Unsure: paragraph once", async () => {
  const view = renderCard({
    text: ':::decision{#d-1 uncertainty="high"}\nStore notices in a table.\n\nUnsure: the index may be slow.\n:::\n',
    uncertainty: "high",
  });
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table." });
    await waitFor(() =>
      expect(card.querySelector("[data-markdown-preview]")?.textContent).toBe(
        "Store notices in a table. Unsure: the index may be slow."
      )
    );
  } finally {
    view.unmount();
  }
});

// A decision's name is markdown its writer wrote, and it names the card and words a link, neither
// of which holds markup: both read the name's plain-text projection, so a link in it is its words
// there and never the link's source. Drawn as the characters it is, the card would be named
// `Decision: Keep **notices** in [a table](https://example.test/table)`.
test("a heading that holds markdown names the card and its link in plain words", async () => {
  const view = renderCard({
    text: ":::decision{#d-1}\nKeep **notices** in [a table](https://example.test/table)\n:::\n",
  });
  try {
    expect(
      await screen.findByRole("article", { name: "Decision: Keep notices in a table" })
    ).toBeDefined();
    expect(screen.getByRole("link", { name: "Keep notices in a table" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts/choices-md#b-d-1"
    );
  } finally {
    view.unmount();
  }
});

// The passage is a record of the document the decision is written in, so the card hands the
// renderer that document as the passage's source and a `#id` in it names a question block of that
// document, as it does in the Decisions panel. With no source the `#q2` stays the characters it is
// and the document's question map is never read.
test("a `#id` in the passage names the question of the decision's document", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "choices-id",
    questions: [{ ask: "ask-2", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-2" }],
  });
  // The link reads its ask for a name; the read stays out, which is no part of this row.
  const getAsk = spyOn(api, "getAsk").mockReturnValue(Promise.withResolvers<AskRead>().promise);
  const view = renderCard({ text: "Settled by #q2 before the rollout." });
  try {
    const linked = () =>
      [...view.container.querySelectorAll("[data-markdown-preview] [data-dispatch-ref]")].map(
        (reference) => reference.getAttribute("data-dispatch-ref")
      );
    await waitFor(() => expect(linked()).toEqual(["dispatch://CORE-1/ask/ask-2"]));
    expect(getReferenceQuestions).toHaveBeenCalledWith("artifact", "choices-id");
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
  }
});

// The name is a record of that document too, so the card's name reads a `#id` in it as the
// question it names in that document. Handed no record, the name keeps `#q2`.
test("a #id in the heading names the question of the decision's document in the card's name", async () => {
  const answers = answerQuestionOf("artifact", "choices-id");
  const view = renderCard({ text: ":::decision{#d-1}\nSettle #q2 first\n:::\n" });
  try {
    expect(
      await screen.findByRole("article", { name: `Decision: Settle ${NAMED_QUESTION} first` })
    ).toBeDefined();
  } finally {
    view.unmount();
    answers.restore();
  }
});

// A person's decision is the answered ask, whose question is the ask's own record: the server
// reads a `#id` in it against the document the ask's block is on (`refs.ScopeOf`). The Decisions
// view names no document for an ask's node, so the card names the ask as the source, and its name
// reads the `#q2` as the question it names. Handed the document the view names, none, it would
// keep `#q2`.
test("a #id in a person's decision names the question of the ask's own record", async () => {
  const ask = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
  const answers = answerQuestionOf("ask", ask);
  const view = renderCard({
    document: null,
    kind: "person",
    node: { id: ask, kind: "ask" },
    question: "Settle #q2 first?",
    ref: `dispatch://CORE-1/ask/${ask}`,
    text: "Settle #q2 first?",
  });
  try {
    expect(
      await screen.findByRole("article", { name: `Decision: Settle ${NAMED_QUESTION} first?` })
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
        <DecisionCard item={item({})} onSelect={() => {}} selected={false} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  expect(html).toContain('aria-label="Decision: Store notices in a table."');
  expect(html).toContain(">Store notices in a table.</a>");
});

test("selecting a decision card selects its margin item", async () => {
  const user = userEvent.setup();
  const selected: string[] = [];
  const view = renderCard({}, (id) => selected.push(id));
  try {
    await user.click(screen.getByRole("article", { name: "Decision: Store notices in a table." }));
    expect(selected).toEqual(["decision:CORE-1/choices-md#d-1:CORE-1/spec#p-plan"]);
  } finally {
    view.unmount();
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
