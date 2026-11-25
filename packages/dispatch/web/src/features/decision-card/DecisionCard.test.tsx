import { expect, mock, test } from "bun:test";
import { within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { Decision } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { fromDecision } from "./adapters";
import {
  type CardChangeAct,
  type CardDecision,
  type CardKeepAct,
  type CardPaths,
  DecisionCard,
  type DecisionTextRenderer,
} from "./DecisionCard";
import {
  DECISION_ACT_WORDS,
  DECISION_KIND_WORDS,
  DECISION_LINE_WORDS,
  DECISION_LINK_WORDS,
  DECISION_STATE_WORDS,
} from "./decision-card-words";

/**
 * The decision card (R19) and its setting (R20), drawn from each shape a read gives a decision:
 * the bare `decisions` member through `fromDecision`, and a decision shaped as INFRA-1150's
 * `entries` and as INFRA-1253's tree read, each built here as a `CardDecision` directly, since
 * neither read is on `main` and no adapter exists for either.
 */

const PATHS: CardPaths = {
  ask: (id) => `/issues/CORE-1/asks/${id}`,
  statement: (blockId) => `/issues/CORE-1/spec#b-${blockId}`,
};

/** Draws the text the card hands it as it is, so a row reads which text that was. */
const renderText: DecisionTextRenderer = (text) => (
  <p data-testid="decision-words">{text.markdown}</p>
);

function model(fields: Partial<Decision> = {}): Decision {
  return {
    asks: [],
    block_id: "store",
    changed_at: "2026-11-01T00:00:00Z",
    changed_version: 3,
    heading: "Store notices in a table",
    kept: null,
    kind: "agent",
    links: [],
    ref: "dispatch://CORE-1/artifact/decisions-md#b-store",
    replaced: false,
    text: "## Store notices in a table\n\nA table keeps them queryable.",
    text_sha256: "sum-store",
    unsure: false,
    waiting_review: true,
    ...fields,
  };
}

function drawCard(
  decision: CardDecision,
  {
    authorLine = false,
    change,
    keep,
  }: { authorLine?: boolean; change?: CardChangeAct; keep?: CardKeepAct } = {}
) {
  const view = render(
    <MemoryRouter>
      <DecisionCard
        authorLine={authorLine}
        change={change}
        decision={decision}
        keep={keep}
        paths={PATHS}
        renderText={renderText}
      />
    </MemoryRouter>
  );
  const card = view.container.querySelector<HTMLElement>("[data-decision-card]");
  if (card === null) throw new Error("the card drew no root marked data-decision-card");
  return { card, view };
}

/** A decision with every part of R19 drawn: its doubt and reason, two statements, a state word,
 *  and Keep and Change. */
const WHOLE: CardDecision = {
  asks: [],
  changed_at: "2026-11-01T00:00:00Z",
  heading: "Store notices in a table",
  kind: "agent",
  links: [
    {
      block_id: "p1",
      changed_at: "2026-11-03T00:00:00Z",
      created_at: "2026-11-02T00:00:00Z",
      stale: true,
      text: "Notices are stored.\nEach in a row.",
    },
    {
      block_id: "p2",
      changed_at: null,
      created_at: "2026-11-02T00:00:00Z",
      stale: false,
      text: "Old rows expire.",
    },
  ],
  replaced: false,
  text: "## Store notices in a table\n\nA table keeps them queryable.",
  uncertainty_reason: "The index may be slow.",
  unsure: true,
  waiting_review: true,
};

const KEEP: CardKeepAct = { error: null, keep: () => {}, pending: false };
const CHANGE: CardChangeAct = {
  change: () => {},
  error: null,
  pending: false,
  quote: "A table keeps them queryable.",
};

/** Whether `first` stands before `second` in the document. */
function before(first: Element, second: Element): boolean {
  return (first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
}

test("the first line is the bullet, the heading, and the kind and the time at its right end", () => {
  const { card, view } = drawCard(WHOLE);
  try {
    const bullet = card.querySelector("[data-decision-bullet]");
    const heading = within(card).getByRole("heading", { name: WHOLE.heading });
    const kind = within(card).getByText(DECISION_KIND_WORDS.agent);
    const time = card.querySelector("time");
    expect(bullet).not.toBeNull();
    expect(time?.getAttribute("datetime")).toBe("2026-11-01T00:00:00Z");
    for (const [first, second] of [
      [bullet, heading],
      [heading, kind],
      [kind, time],
    ] as const) {
      expect(first !== null && second !== null && before(first, second)).toBe(true);
    }
  } finally {
    view.unmount();
  }
});

test("the text is drawn whole by the caller's renderer", () => {
  const { card, view } = drawCard(WHOLE);
  try {
    expect(within(card).getByTestId("decision-words").textContent).toBe(WHOLE.text);
  } finally {
    view.unmount();
  }
});

test("the level of uncertainty and its reason stand under the text", () => {
  const { card, view } = drawCard(WHOLE);
  try {
    const level = within(card).getByText(DECISION_LINE_WORDS.unsure);
    const reason = within(card).getByText("The index may be slow.");
    expect(before(within(card).getByTestId("decision-words"), level)).toBe(true);
    expect(before(level, reason)).toBe(true);
  } finally {
    view.unmount();
  }
});

test("each statement is a link by its first line, with Changed since and out of date, and never by its block's id", () => {
  const nameless: CardDecision = {
    ...WHOLE,
    links: [
      ...WHOLE.links,
      { block_id: "p3", created_at: "2026-11-02T00:00:00Z", stale: true, text: undefined },
    ],
  };
  const { card, view } = drawCard(nameless);
  try {
    const links = within(card)
      .getAllByRole("link")
      .map((link) => [link.textContent, link.getAttribute("href")]);
    expect(links).toEqual([
      ["Notices are stored.", "/issues/CORE-1/spec#b-p1"],
      ["Old rows expire.", "/issues/CORE-1/spec#b-p2"],
    ]);
    expect(card.textContent).not.toContain("p3");
    expect(within(card).getAllByText(DECISION_LINK_WORDS.changed_since)).toHaveLength(1);
    expect(within(card).getAllByText(DECISION_LINK_WORDS.out_of_date)).toHaveLength(1);
    expect(card.textContent).not.toContain("Shaped in the spec:");
  } finally {
    view.unmount();
  }
});

test("a person's decision links each ask it answers", () => {
  const { card, view } = drawCard({
    ...WHOLE,
    asks: ["ask-1", "ask-2"],
    kind: "person",
    waiting_review: false,
  });
  try {
    expect(within(card).getByText(DECISION_KIND_WORDS.person)).toBeDefined();
    expect(
      within(card)
        .getAllByRole("link", { name: DECISION_LINK_WORDS.the_ask })
        .map((link) => link.getAttribute("href"))
    ).toEqual(["/issues/CORE-1/asks/ask-1", "/issues/CORE-1/asks/ask-2"]);
  } finally {
    view.unmount();
  }
});

test("the state word is the decision's own", () => {
  const states: readonly [CardDecision, string][] = [
    [WHOLE, DECISION_STATE_WORDS.waiting_review],
    [
      { ...WHOLE, kept_by: { id: "alice", kind: "user" }, waiting_review: false },
      `${DECISION_STATE_WORDS.kept_by} alice`,
    ],
    [{ ...WHOLE, replaced: true, waiting_review: false }, DECISION_STATE_WORDS.replaced],
  ];
  for (const [decision, word] of states) {
    const { card, view } = drawCard(decision);
    try {
      expect(card.querySelector("[data-decision-state]")?.textContent).toBe(word);
    } finally {
      view.unmount();
    }
  }
});

test("Keep and Change stand on a waiting decision alone, after every other part", () => {
  const waiting = drawCard(WHOLE, { change: CHANGE, keep: KEEP });
  try {
    const keep = within(waiting.card).getByRole("button", { name: DECISION_ACT_WORDS.keep });
    within(waiting.card).getByRole("button", { name: DECISION_ACT_WORDS.change });
    const parts = [
      waiting.card.querySelector("h3"),
      waiting.card.querySelector("[data-decision-text]"),
      waiting.card.querySelector("[data-decision-level]"),
      waiting.card.querySelector("[data-decision-statements]"),
      waiting.card.querySelector("[data-decision-state]"),
      keep,
    ];
    for (const [index, part] of parts.entries()) {
      const next = parts[index + 1];
      if (next === undefined) continue;
      expect(part !== null && next !== null && before(part, next), `part ${index}`).toBe(true);
    }
  } finally {
    waiting.view.unmount();
  }
  for (const decision of [
    { ...WHOLE, kept_by: { id: "alice", kind: "user" }, waiting_review: false },
    { ...WHOLE, replaced: true },
  ]) {
    const { card, view } = drawCard(decision, { change: CHANGE, keep: KEEP });
    try {
      expect(within(card).queryAllByRole("button")).toHaveLength(0);
    } finally {
      view.unmount();
    }
  }
});

test("Keep and Change call the acts the caller hands", async () => {
  const user = userEvent.setup();
  const keep = mock(() => {});
  const change = mock((_body: string, sent: () => void) => sent());
  const { card, view } = drawCard(WHOLE, {
    change: { ...CHANGE, change },
    keep: { ...KEEP, keep },
  });
  try {
    await user.click(within(card).getByRole("button", { name: DECISION_ACT_WORDS.keep }));
    expect(keep).toHaveBeenCalledTimes(1);
    await user.click(within(card).getByRole("button", { name: DECISION_ACT_WORDS.change }));
    const form = within(card).getByRole("form", {
      name: `${DECISION_ACT_WORDS.change} ${WHOLE.heading}`,
    });
    expect(within(form).getByTestId("decision-quote").textContent).toBe(CHANGE.quote);
    await user.type(within(form).getByLabelText(DECISION_ACT_WORDS.your_change), "Use a queue.");
    await user.click(within(form).getByRole("button", { name: DECISION_ACT_WORDS.send_change }));
    expect(change).toHaveBeenCalledTimes(1);
    expect(change.mock.calls[0]?.[0]).toBe("Use a queue.");
    // The act said the change landed, so the form closes.
    expect(within(card).queryByRole("form")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a write the reader stopped draws the words the tab drew on main", async () => {
  const user = userEvent.setup();
  const stopped = new SendStoppedError();
  const { card, view } = drawCard(WHOLE, {
    change: { ...CHANGE, error: stopped },
    keep: { ...KEEP, error: stopped },
  });
  try {
    expect(within(card).getByRole("alert").textContent).toBe(
      stoppedWriteNote("the Keep of “Store notices in a table”", "made", {
        anew: "Pressing Keep again sends a new Keep.",
      })
    );
    await user.click(within(card).getByRole("button", { name: DECISION_ACT_WORDS.change }));
    const form = within(card).getByRole("form");
    expect(within(form).getByRole("alert").textContent).toBe(
      stoppedWriteNote("your change", "posted", { anew: "Send posts it again as a new comment." })
    );
  } finally {
    view.unmount();
  }
});

/** A decision shaped as INFRA-1150's `entries` would hand it: its author, its level and its
 *  reason apart from its text, the person who kept it, and a change a person asked of it. Built
 *  directly: no adapter for that read exists on `main`. */
const ENTRY: CardDecision = {
  asks: [],
  change: { author: { id: "carol", kind: "user" } },
  changed_at: "2026-11-01T00:00:00Z",
  decided_by: { id: "dana", kind: "user" },
  heading: "Store notices in a table",
  kept_by: { id: "alice", kind: "user" },
  kind: "agent",
  links: [],
  replaced: false,
  text: "## Store notices in a table\n\nA table keeps them queryable.",
  uncertainty: "high",
  uncertainty_reason: "The index may be slow.",
  unsure: false,
  waiting_review: false,
};

/** The person's answer the tree-shaped decision below holds. */
const ANSWER = "Start with the first batch.";

/** A person's decision shaped as INFRA-1253's tree read would hand it: the person's answer and
 *  its author, beside the section's text. Built directly: no adapter for that read exists on
 *  `main`. */
const TREE_PERSON: CardDecision = {
  answer: ANSWER,
  asks: ["ask-1"],
  changed_at: "2026-11-02T00:00:00Z",
  decided_by: { id: "erin", kind: "user" },
  heading: "Which batch runs first",
  kind: "person",
  links: [],
  replaced: false,
  text: "## Which batch runs first\n\nDecided in the ask.",
  unsure: false,
  waiting_review: false,
};

/** The bare `decisions` member, an agent's decision its maker was unsure of, kept by a person. */
function bareMember(): CardDecision {
  return fromDecision(
    model({
      kept: {
        artifact_id: "doc",
        block_id: "store",
        changed_version: 3,
        kept_at: "2026-11-02T00:00:00Z",
        kept_by: "alice",
      },
      text: "## Store notices in a table\n\nA table keeps them queryable.\n\nUnsure: the index may be slow.",
      unsure: true,
      waiting_review: false,
    })
  );
}

test("setting off: the bare member draws no author, Unsure as its one level word, and its state", () => {
  const { card, view } = drawCard(bareMember());
  try {
    expect(card.textContent).not.toContain(DECISION_LINE_WORDS.author_unknown);
    expect(within(card).getByText(DECISION_LINE_WORDS.unsure)).toBeDefined();
    expect(card.querySelector("time")?.getAttribute("datetime")).toBe("2026-11-01T00:00:00Z");
    // The reason is the `Unsure:` paragraph inside the text, which the renderer draws.
    expect(within(card).getByTestId("decision-words").textContent).toContain(
      "Unsure: the index may be slow."
    );
    expect(card.querySelector("[data-decision-state]")?.textContent).toBe(
      `${DECISION_STATE_WORDS.kept_by} alice`
    );
  } finally {
    view.unmount();
  }
});

test("setting off: an entries-shaped decision draws no author, no level but Unsure, its reason, and no Changed by", () => {
  const { card, view } = drawCard(ENTRY);
  try {
    expect(card.textContent).not.toContain("dana");
    expect(card.textContent).not.toContain(DECISION_LINE_WORDS.author_unknown);
    expect(card.textContent).not.toContain("high");
    expect(within(card).getByText(DECISION_LINE_WORDS.unsure)).toBeDefined();
    expect(within(card).getByText("The index may be slow.")).toBeDefined();
    expect(card.textContent).not.toContain(DECISION_STATE_WORDS.changed_by);
    expect(card.querySelector("[data-decision-state]")?.textContent).toBe(
      `${DECISION_STATE_WORDS.kept_by} alice`
    );
  } finally {
    view.unmount();
  }
});

test("setting off: a tree-shaped person's decision draws the person's answer and the ask it answers, and no author", () => {
  const { card, view } = drawCard(TREE_PERSON);
  try {
    expect(within(card).getByText(DECISION_KIND_WORDS.person)).toBeDefined();
    expect(within(card).getByTestId("decision-words").textContent).toBe(ANSWER);
    expect(within(card).getByRole("link", { name: DECISION_LINK_WORDS.the_ask })).toBeDefined();
    expect(card.textContent).not.toContain("erin");
    expect(card.querySelector("[data-decision-state]")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("setting on: the bare member draws Author unknown and Unsure as its level", () => {
  const { card, view } = drawCard(bareMember(), { authorLine: true });
  try {
    expect(within(card).getByText(DECISION_LINE_WORDS.author_unknown)).toBeDefined();
    expect(within(card).getByText(DECISION_LINE_WORDS.unsure)).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("setting on: an entries-shaped decision draws its author, its level and the change's author", () => {
  const { card, view } = drawCard(ENTRY, { authorLine: true });
  try {
    expect(within(card).getByText("dana")).toBeDefined();
    expect(card.textContent).not.toContain(DECISION_LINE_WORDS.author_unknown);
    expect(within(card).getByText("high")).toBeDefined();
    expect(card.querySelector("[data-decision-state]")?.textContent).toBe(
      `${DECISION_STATE_WORDS.changed_by} carol`
    );
  } finally {
    view.unmount();
  }
});

test("setting on: a tree-shaped person's decision draws its author", () => {
  const { card, view } = drawCard(TREE_PERSON, { authorLine: true });
  try {
    expect(within(card).getByText("erin")).toBeDefined();
    expect(card.textContent).not.toContain(DECISION_LINE_WORDS.author_unknown);
    expect(within(card).getByTestId("decision-words").textContent).toBe(ANSWER);
  } finally {
    view.unmount();
  }
});
