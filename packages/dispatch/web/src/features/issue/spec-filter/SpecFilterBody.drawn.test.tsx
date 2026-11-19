import { expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../../api/client";
import { issueResultsQuery } from "../../../api/queries";
import type { CheckBlock, Decision } from "../../../api/types";
import { MarginProvider } from "../../margin/margin-context";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import type { TreeNode } from "../issue-tree";
import { KEY_REPORTS } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_WORDS,
  SPEC_WORDS,
  VERIFICATION_WORDS,
} from "../page-words";
import { presetView } from "../presets";
import { check, checkNode, result } from "../verification-filter/verification-fixtures";
import { SpecFilterBody } from "./SpecFilterBody";
import {
  answeredAsk,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  node,
  READER,
  shaped,
  statement,
  statementRef,
} from "./spec-fixtures";

/**
 * What the Spec filter's body draws, read as a reader reads it (dispatch://INFRA-1196 R5, R17,
 * R22, R35, the approved picture `page-v3-spec-1440.png`).
 *
 * Which decision shaped which statement is read in `spec-decided-by.test.ts`; these rows are
 * about what the page says and what a reader can reach: the legend's words, the two columns'
 * headings, the words of each cell and of each statement's own line, the link to the whole of a
 * decision, and where the keys take the focus. Where the two columns stand side by side and
 * where they stack is the browser's own layout, which no DOM of a unit test lays out, so the
 * widths are read by the browser suite.
 */

/** The page's own `issue` scope, which every one of the outline's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** The body over `tree`, with `made` as the issue's decisions read and `checks` as its results
 *  read. `decisions: "out"` leaves the decisions read unanswered, and `"failed"` fails it. */
function drawBody(
  tree: TreeNode,
  made: readonly Decision[] = [],
  {
    checks = [],
    context = new Set<string>(),
    decisions = "answered",
    named = [],
    reader = READER,
    reveal,
  }: {
    checks?: readonly CheckBlock[];
    context?: Set<string>;
    decisions?: "answered" | "failed" | "out";
    named?: readonly string[];
    reader?: string;
    reveal?: string;
  } = {}
): { read: Mock<typeof api.getIssueDecisions>; view: RenderResult } {
  const answer = decisionsRead(made);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(issueResultsQuery(KEY).queryKey, { checks: [...checks] } as never);
  const read = spyOn(api, "getIssueDecisions");
  if (decisions === "answered") {
    client.setQueryData(["decisions", KEY], answer);
    read.mockResolvedValue(answer);
  } else if (decisions === "failed") {
    read.mockRejectedValue(new Error("offline"));
  } else {
    read.mockReturnValue(new Promise(() => {}));
  }
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <KeymapProvider>
          <MarginProvider>
            <IssueScope />
            <SpecFilterBody
              context={context}
              emptyLine={EMPTY_STATE_LINES.spec}
              holdsEscape={false}
              issue={issueDetails()}
              reader={reader}
              reading={false}
              {...(reveal === undefined ? {} : { reveal })}
              root={tree}
              view={presetView(tree, "spec", "", new Set(named)).root}
            />
          </MarginProvider>
        </KeymapProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  return { read, view };
}

/** The text of every element matching `selector`, in the order they are drawn. */
function textsOf(view: RenderResult, selector: string): string[] {
  return [...view.container.querySelectorAll(selector)].map((element) =>
    (element.textContent ?? "").trim()
  );
}

function rowOf(view: RenderResult, id: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${id}"]`);
  if (row === null) throw new Error(`the body draws no row for ${id}`);
  return row;
}

// R17: the legend names each mark in the words the Verification filter says how a statement
// stands, so one statement reads the same in both views, and none of them reads proven or
// covered for a statement whose checks passed.
test("the legend names how a statement stands, in the Verification filter's own words", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(
      [...view.container.querySelectorAll("[data-spec-legend-mark]")].map((mark) => [
        mark.getAttribute("data-spec-legend-mark"),
        mark.textContent,
      ])
    ).toEqual([
      ["met", VERIFICATION_WORDS.states.met],
      ["not_met", VERIFICATION_WORDS.states.not_met],
      ["unknown", VERIFICATION_WORDS.states.unknown],
      ["no_check", SPEC_WORDS.no_check],
    ]);
    const legend = view.container.querySelector("[data-spec-legend]")?.textContent ?? "";
    expect(legend).not.toContain("proven");
    expect(legend).not.toContain("covered");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R22: the page draws Statement where it drew Requirement.
test("the two columns are headed Decided by and Statement, never Requirement", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(textsOf(view, "[data-spec-head] span")).toEqual(["Decided by", "Statement"]);
    expect(view.container.textContent).not.toContain("Requirement");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: the decision stands once beside the statements it shaped, headed by the words it came
// from, with who decided it, when, what they chose, and a link to the whole answer.
test("a person's decision stands once beside its statements, in their own words", () => {
  const ask = answeredAsk("ask-1", {
    selected: ["Changes needed"],
    text: "The node types are all, like, completely so you have to click. And I can't write.",
  });
  const { read, view } = drawBody(
    issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision({
        asks: ["ask-1"],
        block_id: "d1",
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      }),
    ]
  );
  try {
    const cells = [...view.container.querySelectorAll("[data-spec-cell]")];
    expect(cells).toHaveLength(1);
    expect(cells[0]?.getAttribute("data-said")).toBe("you");
    expect(cells[0]?.getAttribute("data-spec-statements")).toBe("spec#b-r1 spec#b-r2");
    expect(textsOf(view, "[data-spec-said]")).toEqual([SPEC_WORDS.said_you]);
    expect(textsOf(view, "[data-spec-quote]")).toEqual([
      "“The node types are all, like, completely so you have to click.”",
    ]);
    const source = view.container.querySelector("[data-spec-source]")?.textContent ?? "";
    expect(source).toContain("Does the built page work for you?");
    expect(source).toContain(READER);
    expect(source).toContain("14 Nov");
    expect(source).toContain(`${SPEC_WORDS.chose} Changes needed`);
    const whole = view.container.querySelector("[data-spec-whole]");
    expect(whole?.textContent?.trim()).toBe(`${SPEC_WORDS.whole_answer} ›`);
    expect(whole?.getAttribute("href")).toBe(`/issues/${KEY}/asks/ask-1`);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5, the tester's F7: one line on each statement's own row, with no focus and no hover: the
// words the decision came from, as a link to that decision, who decided and when.
test("each statement carries one line naming the decision that shaped it, with a link to it", () => {
  const ask = answeredAsk("ask-1", { text: "Put the decisions beside the statements. Please." });
  const { read, view } = drawBody(
    issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision({
        asks: ["ask-1"],
        block_id: "d1",
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      }),
    ]
  );
  try {
    const lines = textsOf(view, "[data-spec-because]");
    expect(lines).toHaveLength(2);
    for (const line of lines) {
      expect(line).toContain(SPEC_WORDS.because);
      expect(line).toContain("“Put the decisions beside the statements.”");
      expect(line).toContain(READER);
      expect(line).toContain("14 Nov");
    }
    const links = [...view.container.querySelectorAll("[data-spec-because-link]")];
    expect(links.map((link) => link.getAttribute("href"))).toEqual([
      `/issues/${KEY}/asks/ask-1`,
      `/issues/${KEY}/asks/ask-1`,
    ]);
    // Out of the tab order, as every control inside a row is (R9).
    expect(links.map((link) => link.getAttribute("tabindex"))).toEqual(["-1", "-1"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: a statement whose own words changed after the decision says so, on the statement's line
// and in the decision's cell; one that did not change says nothing.
test("a statement that changed after its decision says Changed since", () => {
  const tie = (block: string, changed: string | null) => {
    const link = shaped(statementRef(block));
    return { ...link, to: { ...link.to, changed_at: changed } };
  };
  const { read, view } = drawBody(
    issueTree([statement("r1"), statement("r2"), decisionNode("d1")]),
    [
      decision({
        block_id: "d1",
        kind: "agent",
        links: [tie("r1", "2026-11-15T09:00:00Z"), tie("r2", "2026-11-12T09:00:00Z")],
      }),
    ]
  );
  try {
    expect(rowOf(view, "spec#b-r1").querySelector("[data-spec-changed]")?.textContent).toBe(
      SPEC_WORDS.changed_since
    );
    expect(rowOf(view, "spec#b-r2").querySelector("[data-spec-changed]")).toBeNull();
    expect(view.container.querySelector("[data-spec-cell] [data-spec-changed]")?.textContent).toBe(
      SPEC_WORDS.changed_since
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R35: a statement with no decision beside it is drawn as such, which is what makes a missing
// tie visible.
test("a statement no decision shaped says so where its decision would stand", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([EMPTY_STATE_LINES.spec_no_decision_one]);
    expect(textsOf(view, "[data-spec-because]")).toEqual([]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// Two statements no decision shaped stand under one cell, whose words agree with their count.
test("several statements no decision shaped say so in words that agree with their count", () => {
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    expect(textsOf(view, "[data-spec-nothing]")).toEqual([EMPTY_STATE_LINES.spec_no_decision_many]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R35's sentence is true only once the decisions read has answered: while it is out the column
// says nothing of a statement, and a read that failed says so in the page's own line.
test("the column says no decision shaped a statement only once the decisions are read", async () => {
  const out = drawBody(issueTree([statement("r1")]), [], { decisions: "out" });
  try {
    expect(textsOf(out.view, "[data-spec-nothing]")).toEqual([]);
    expect(out.view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
  } finally {
    out.view.unmount();
    out.read.mockRestore();
  }
  const failed = drawBody(issueTree([statement("r1")]), [], { decisions: "failed" });
  try {
    await waitFor(() =>
      expect(failed.view.container.textContent).toContain(ANSWER_LINES.decisions_unreadable)
    );
    expect(textsOf(failed.view, "[data-spec-nothing]")).toEqual([]);
  } finally {
    failed.view.unmount();
    failed.read.mockRestore();
  }
});

// R5: an agent's decision draws its text and the one word the model holds for how sure it was;
// R2: wherever a decision is drawn, one waiting for review says so.
test("an agent's decision draws its own words, how sure it was and how it stands", () => {
  const made = decisionNode("d1", {
    state: "waiting_review",
    text: "One batched read names every address.",
    unsure: true,
    waiting: true,
  });
  const { read, view } = drawBody(issueTree([statement("r1"), made]), [
    decision({
      block_id: "d1",
      kind: "agent",
      links: [shaped(statementRef("r1"))],
      text: "One batched read names every address.",
      unsure: true,
      waiting_review: true,
    }),
  ]);
  try {
    const cell = view.container.querySelector("[data-spec-cell]");
    expect(cell?.getAttribute("data-said")).toBe("agent");
    expect(cell?.className).toContain("border-dashed");
    expect(textsOf(view, "[data-spec-said]")).toEqual([SPEC_WORDS.said_agent]);
    expect(textsOf(view, "[data-spec-quote]")).toEqual(["“One batched read names every address.”"]);
    const source = view.container.querySelector("[data-spec-source]")?.textContent ?? "";
    expect(source).toContain("Waiting for review");
    expect(source).toContain("Unsure");
    expect(view.container.querySelector("[data-spec-whole]")?.textContent?.trim()).toBe(
      `${SPEC_WORDS.whole_decision} ›`
    );
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: where the answered question is the statement's own parent, its parent line and its
// decision line are one line, so the question is drawn once.
test("the question a statement hangs under is drawn once, as the group's heading", () => {
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { text: "Keep the memo in the pane. It reads better there." }, [
        statement("r1"),
      ]),
    ])
  );
  try {
    const heading = view.container.querySelector("[data-spec-group-heading]")?.textContent ?? "";
    expect(heading).toContain("question");
    expect(heading).toContain("Does the built page work for you?");
    const source = view.container.querySelector("[data-spec-source]")?.textContent ?? "";
    expect(source).not.toContain("Does the built page work for you?");
    expect(textsOf(view, "[data-spec-quote]")).toEqual(["“Keep the memo in the pane.”"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R5: "The focus on the line draws the whole answer word for word". The cell draws the first
// sentence until it has the focus, and then the answer character for character as the store
// holds it; `w` on a statement is the key that puts the focus there.
test("the focus on a decision draws the person's whole answer, character for character", async () => {
  const stored = "Keep the memo in the pane.\n\n  It reads better there -  really.";
  const user = userEvent.setup();
  const { read, view } = drawBody(
    issueTree([
      answeredAsk("ask-1", { text: stored }),
      statement("r1"),
      // The tree names every block a decision links as a chip on it (`tree-drafts.ts`), which
      // is the edge `w` follows (`tree-keys.ts`'s `decisionBehind`).
      decisionNode("d1", {
        chips: [{ answered: true, gone: false, name: "r1", ref: statementRef("r1") }],
      }),
    ]),
    [
      decision({
        asks: ["ask-1"],
        block_id: "d1",
        kind: "person",
        links: [shaped(statementRef("r1"))],
      }),
    ]
  );
  try {
    expect(view.container.querySelector("[data-spec-whole-answer]")).toBeNull();
    act(() => rowOf(view, "spec#b-r1").focus());
    await user.keyboard("w");
    const cell = view.container.querySelector<HTMLElement>("[data-spec-cell]");
    expect(document.activeElement).toBe(cell);
    expect(view.container.querySelector("[data-spec-whole-answer]")?.textContent).toBe(stored);
    act(() => rowOf(view, "spec#b-r1").focus());
    expect(view.container.querySelector("[data-spec-whole-answer]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R11, the tester's F16: the statements are the tree's rows, the arrows move between them with
// one tab stop, and a key with nowhere to go says so at the statement it was pressed on.
test("the arrows move between the statements, and a key with nowhere to go reports at its row", async () => {
  const user = userEvent.setup();
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]));
  try {
    const first = rowOf(view, "spec#b-r1");
    const second = rowOf(view, "spec#b-r2");
    expect([first.tabIndex, second.tabIndex]).toEqual([0, -1]);
    act(() => first.focus());
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(second);
    expect([first.tabIndex, second.tabIndex]).toEqual([-1, 0]);
    await user.keyboard("w");
    expect(second.querySelector("[data-row-report]")?.textContent).toBe(KEY_REPORTS.no_decision);
    expect(first.querySelector("[data-row-report]")).toBeNull();
    // Moving on lets the report go.
    await user.keyboard("{ArrowUp}");
    expect(view.container.querySelector("[data-row-report]")).toBeNull();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R19: an address that names a statement (`/spec#b-<block>`) puts the focus on it.
test("an address naming a statement puts the focus on it", async () => {
  const { read, view } = drawBody(issueTree([statement("r1"), statement("r2")]), [], {
    reveal: "spec#b-r2",
  });
  try {
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view, "spec#b-r2")));
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R17, R6: each statement carries how it stands at the right of its row, by a shape with a name
// of its own, so the mark is never colour alone; a statement no check serves carries that.
test("each statement carries how it stands, by a shape with its own name", () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const failed = check({
    block_id: "c2",
    pass_stands: { answer: "no" },
    results: [result({ id: "f1", verdict: "fail" })],
  });
  const unrun = check({ block_id: "c3", pass_stands: { answer: "no" }, results: [] });
  const { read, view } = drawBody(
    issueTree([
      statement("r1", { children: [checkNode(passed)] }),
      statement("r2", { children: [checkNode(failed)] }),
      statement("r3", { children: [checkNode(unrun)] }),
      statement("r4", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
    ]),
    [],
    { checks: [passed, failed, unrun] }
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-spec-statement]")].map((row) => [
        row.getAttribute("data-spec-mark"),
        row.querySelector(":scope > svg[data-glyph]")?.getAttribute("aria-label"),
      ])
    ).toEqual([
      ["met", VERIFICATION_WORDS.states.met],
      ["not_met", VERIFICATION_WORDS.states.not_met],
      ["unknown", VERIFICATION_WORDS.states.unknown],
      ["no_check", GAP_WORDS.no_check],
    ]);
    // No two of the four shapes are one shape.
    const paths = [
      ...view.container.querySelectorAll("[data-spec-statement] > svg[data-glyph] path"),
    ].map((path) => path.getAttribute("d"));
    expect(new Set(paths).size).toBe(4);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// Ruling 1 and ruling 6: a statement's own words carry its number where the spec wrote one, and
// are drawn as the outline draws an agent's words - whole here, where no layout cuts them.
test("a statement's words are drawn as its author wrote them", () => {
  const words =
    "**R1.** Every node's words are on the page without opening it; an agent's long words " +
    "show their first six lines and the rest one key away, and his words are never cut.";
  const { read, view } = drawBody(issueTree([statement("r1", { text: words })]));
  try {
    expect(
      view.container.querySelector("[data-spec-statement] [data-node-words]")?.textContent
    ).toBe(words);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R2: a row the filter draws only to place another is drawn faint and marked as context.
test("a statement drawn as context is marked as such", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]), [], {
    context: new Set(["spec#b-r1"]),
  });
  try {
    expect(
      view.container.querySelector("[data-spec-statement]")?.hasAttribute("data-node-context")
    ).toBe(true);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A statement of this body is a row of the page: the readers that walk the outline's rows find
// it by the attributes every row carries (`e2e/issue-page.ts`'s `drawnRows` and `nodeRow`),
// which is what c-presets compares a filter's drawn ids with.
test("a statement is drawn as a row of the page, with the id and kind every row carries", () => {
  const { read, view } = drawBody(issueTree([statement("r1")]));
  try {
    const row = view.container.querySelector("[data-node-row]");
    expect(row?.getAttribute("data-node-id")).toBe("spec#b-r1");
    expect(row?.getAttribute("data-node-type")).toBe("requirement");
    expect(row?.getAttribute("aria-level")).toBe("1");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R20: a node an address names outranks the filter. It is no statement: it is drawn by its
// kind and its words, with no mark and no decision cell beside it.
test("a row the address named that is no statement is drawn by its kind, with no mark", () => {
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  const { read, view } = drawBody(tree, [], { named: ["comment-1"] });
  try {
    const other = view.container.querySelector("[data-spec-other]");
    expect(other?.getAttribute("data-node-id")).toBe("comment-1");
    expect(other?.textContent).toContain("comment");
    expect(other?.textContent).toContain("A remark the address names.");
    expect(other?.querySelector("svg[data-glyph]")).toBeNull();
    expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(1);
    expect(view.container.querySelectorAll("[data-spec-cell]")).toHaveLength(1);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// An issue whose spec holds no statement says so, in the words the outline says it in.
test("an issue with no statements says so in the page's own line", () => {
  const { read, view } = drawBody(issueTree([]));
  try {
    expect(view.container.querySelector("[data-spec-empty]")?.textContent).toBe(
      EMPTY_STATE_LINES.spec
    );
    expect(view.container.querySelectorAll("[data-spec-statement]")).toHaveLength(0);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// The body is the outline the page draws under the Spec filter, so it is one tree, named as
// every other filter's outline is: every browser row reads the page by that name
// (`e2e/issue-layout.e2e.ts`'s `expectIssuePageDrawn`). Its items are each statement and the
// decision beside it, which is what the focus moves between (R5's `w`).
test("the body draws one tree, named as the outline is, of the statements and their decisions", () => {
  const { read, view } = drawBody(issueTree([statement("r1"), decisionNode("d1")]), [
    decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] }),
  ]);
  try {
    const tree = view.container.querySelector('[role="tree"]');
    expect(tree?.getAttribute("aria-label")).toBe(CONTROL_LABELS.tree);
    expect(
      [...view.container.querySelectorAll('[role="treeitem"]')].map((item) =>
        item.hasAttribute("data-spec-cell") ? "cell" : item.getAttribute("data-node-id")
      )
    ).toEqual(["cell", "spec#b-r1"]);
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// R10, R17: a gap the mark does not already say is drawn under the statement's words, in the
// few words the page knows it by; the one for a statement no check serves is the mark at the
// right of the row, and is not said twice.
test("a gap the mark does not say is drawn under the statement, and no check is not said twice", () => {
  const { read, view } = drawBody(
    issueTree([
      statement("r1", { gaps: ["shaped_disagreement"] }),
      statement("r2", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
    ])
  );
  try {
    const rows = [...view.container.querySelectorAll("[data-spec-statement]")];
    expect(
      [...(rows[0]?.querySelectorAll("[data-gap-kind]") ?? [])].map((gap) => [
        gap.getAttribute("data-gap-kind"),
        gap.textContent,
      ])
    ).toEqual([["shaped_disagreement", GAP_WORDS.shaped_disagreement]]);
    expect(rows[1]?.querySelectorAll("[data-gap-kind]")).toHaveLength(0);
    expect(rows[1]?.getAttribute("data-spec-mark")).toBe("no_check");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});
