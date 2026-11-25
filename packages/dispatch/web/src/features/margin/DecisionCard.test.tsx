import { beforeAll, expect, test } from "bun:test";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { DecisionEntry } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { DecisionCard } from "./DecisionCard";
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
