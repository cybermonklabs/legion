import { beforeAll, expect, test } from "bun:test";
import { screen } from "@testing-library/react";
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
