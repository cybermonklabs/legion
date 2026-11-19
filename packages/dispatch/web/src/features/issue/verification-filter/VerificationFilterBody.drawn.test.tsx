import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import type { CheckBlock } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import {
  BLOCK_ATTRIBUTE_LABELS,
  CHECK_WHEN_WORDS,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GLYPH_TOKENS,
  NODE_FACT_LABELS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  sentenceCase,
  VERIFICATION_WORDS,
} from "../page-words";
import { shortCommit } from "./statement-state";
import { VerificationFilterBody } from "./VerificationFilterBody";
import {
  COMMIT,
  check,
  checkNode,
  node,
  OTHER_COMMIT,
  result,
  statementNode,
  verificationPreset,
} from "./verification-fixtures";

/**
 * What the Verification filter's body draws, read as a reader reads it
 * (dispatch://INFRA-1196 R17, R2, R10, R22, and the approved picture
 * `page-v3-verification-1440.png`).
 *
 * How the model reads the issue's results is `statement-state.test.ts`; these rows are about
 * what the page says: the covered count, the one commit, the word and the shape of each state,
 * the gap on a statement no check serves, and the facts a check's body opens. Where the three
 * columns stand side by side and where they stack is the browser's own layout, which no DOM of
 * a unit test lays out, so the widths are read by the browser suite (c-checks-readable).
 */

/** The body over the statements given, with `checks` as the issue's results read, drawn from
 *  the Verification filter's own view: the tree it leaves and the rows it marks as context. */
function drawBody(
  statements: readonly TreeNode[],
  checks: readonly CheckBlock[],
  { named = [] }: { named?: readonly string[] } = {}
): RenderResult {
  const view = verificationPreset(statements, named);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <VerificationFilterBody
          checks={checks}
          context={view.context}
          emptyLine={EMPTY_STATE_LINES.verification}
          view={view.root}
        />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** The text of every element matching `selector`, in the order they are drawn. */
function textsOf(view: RenderResult, selector: string): string[] {
  return [...view.container.querySelectorAll(selector)].map((element) =>
    (element.textContent ?? "").replace(/\s+/gu, " ").trim()
  );
}

/** What the page says in its own words: the text of each `[data-page-chrome]` element with
 *  every `[data-author-text]` descendant taken out, which is what c-plain-words reads word by
 *  word (`e2e/issue-page.ts`'s `chromeTexts`). */
function chromeTexts(view: RenderResult): string[] {
  return [...view.container.querySelectorAll("[data-page-chrome]")]
    .map((element) => {
      const copy = element.cloneNode(true) as Element;
      for (const authored of copy.querySelectorAll("[data-author-text]")) authored.remove();
      return (copy.textContent ?? "").replace(/\s+/gu, " ").trim();
    })
    .filter((text) => text !== "");
}

/** A check that passed at the commit the page names. */
function passing(blockId: string): CheckBlock {
  return check({
    block_id: blockId,
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: blockId, id: `result-${blockId}` })],
  });
}

/** A check that failed at that commit. */
function failing(blockId: string): CheckBlock {
  return check({
    block_id: blockId,
    pass_stands: { answer: "no" },
    results: [
      result({ check_block_id: blockId, failed: 1, id: `result-${blockId}`, verdict: "fail" }),
    ],
  });
}

// R17: "How many statements carry a check is drawn as covered, and no count, legend or heading
// reads proven or met for coverage", and "The page names one commit for the issue's results ...
// drawn once where the results are read".
test("the summary counts the statements that carry a check as covered and names one commit", () => {
  const met = passing("c-one");
  const unrun = check({ block_id: "c-two", pass_stands: { answer: "no" }, results: [] });
  const view = drawBody(
    [
      statementNode("r1", "The page draws the checks.", [checkNode(met)]),
      statementNode("r2", "The page draws their results.", [checkNode(unrun)]),
      statementNode("r3", "Nothing serves this one.", []),
    ],
    [met, unrun]
  );
  try {
    expect(textsOf(view, "[data-verification-covered]")).toEqual([
      `2 ${VERIFICATION_WORDS.of} 3 ${VERIFICATION_WORDS.covered}`,
    ]);
    expect(VERIFICATION_WORDS.covered).toBe("covered");
    const summary = view.container.querySelector("[data-verification-summary]");
    expect(summary?.textContent).not.toContain("proven");
    expect(textsOf(view, "[data-verification-commit]")).toEqual([
      `${VERIFICATION_WORDS.commit} ${shortCommit(COMMIT)}`,
    ]);
    // One commit for the issue's results, and no result draws one of its own on its line.
    expect(
      [...view.container.querySelectorAll("[data-verification-result]")].filter((drawn) =>
        (drawn.textContent ?? "").includes(shortCommit(COMMIT))
      )
    ).toEqual([]);
  } finally {
    view.unmount();
  }
});

// R17: "Each of the seven states a check can be in is told apart by a word and a shape".
test("each of the seven states a check can be in is drawn by its own word and shape", () => {
  const withdrawn = {
    actor: { id: "tester", kind: "session" as const },
    created_at: "2026-11-15T10:00:00Z",
    reason: "The run was wrong.",
  };
  const blocks: readonly CheckBlock[] = [
    passing("c-passed"),
    failing("c-failed"),
    check({ block_id: "c-no-result", pass_stands: { answer: "no" }, results: [] }),
    check({
      block_id: "c-older",
      pass_stands: { answer: "no" },
      results: [result({ check_block_id: "c-older", check_words_moved: true, id: "result-older" })],
    }),
    check({
      block_id: "c-retracted",
      pass_stands: { answer: "no" },
      results: [
        result({ check_block_id: "c-retracted", id: "result-retracted", retracted: withdrawn }),
      ],
    }),
    check({
      block_id: "c-unmarked",
      pass_stands: { answer: "unknown" },
      results: [
        result({
          check_block_id: "c-unmarked",
          commit_stands: { answer: "unknown" },
          id: "result-unmarked",
        }),
      ],
    }),
    check({
      block_id: "c-unreadable",
      pass_stands: undefined as unknown as never,
      results: [result({ check_block_id: "c-unreadable", id: "result-unreadable" })],
    }),
  ];
  const view = drawBody(
    [statementNode("r1", "The page draws the checks.", blocks.map(checkNode))],
    blocks
  );
  try {
    const drawn = [...view.container.querySelectorAll("[data-verification-check]")].map((card) => ({
      shape: card.querySelector("svg[data-glyph]")?.getAttribute("data-glyph"),
      state: card.getAttribute("data-node-state"),
      word: card.querySelector("[data-node-state]")?.textContent,
    }));
    expect(drawn.map((held) => held.state)).toEqual([
      "passed",
      "failed",
      "no-result",
      "older",
      "retracted",
      "unmarked",
      "unreadable",
    ]);
    expect(drawn.map((held) => held.word)).toEqual([
      NODE_STATE_WORDS.check.passed,
      NODE_STATE_WORDS.check.failed,
      NODE_STATE_WORDS.check["no-result"],
      NODE_STATE_WORDS.check.older,
      NODE_STATE_WORDS.check.retracted,
      NODE_STATE_WORDS.check.unmarked,
      NODE_STATE_WORDS.check.unreadable,
    ]);
    // Every one of the seven is told from the other six by a shape as well as by a word.
    expect(drawn.map((held) => held.shape)).toEqual(
      drawn.map((held) => GLYPH_TOKENS.states.check[held.state as never])
    );
    expect(new Set(drawn.map((held) => held.shape)).size).toBe(7);
    expect(new Set(drawn.map((held) => held.word)).size).toBe(7);
  } finally {
    view.unmount();
  }
});

// R17: Met where every check of it passed at the commit the page names, Not met where one
// failed there, Unknown otherwise - here a pass recorded against another commit.
test("a statement reads Met, Not met or Unknown by how its checks stand at that commit", () => {
  const met = passing("c-one");
  const failed = failing("c-two");
  const elsewhere = check({
    block_id: "c-three",
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: "c-three", commit_sha: OTHER_COMMIT, id: "result-three" })],
  });
  const view = drawBody(
    [
      statementNode("r1", "Every check of this one passed.", [checkNode(met)]),
      statementNode("r2", "One check of this one failed.", [checkNode(failed)]),
      statementNode("r3", "This one was proved somewhere else.", [checkNode(elsewhere)]),
    ],
    [met, failed, elsewhere]
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-verification-card]")].map((card) => [
        card.getAttribute("data-statement-state"),
        card.querySelector("[data-statement-state]")?.textContent,
      ])
    ).toEqual([
      ["met", VERIFICATION_WORDS.states.met],
      ["not_met", VERIFICATION_WORDS.states.not_met],
      ["unknown", VERIFICATION_WORDS.states.unknown],
    ]);
    expect([VERIFICATION_WORDS.states.met, VERIFICATION_WORDS.states.not_met]).toEqual([
      "Met",
      "Not met",
    ]);
    // The check of the third says why: its result was recorded against another commit.
    expect(textsOf(view, "[data-verification-check] [data-node-state]").at(-1)).toBe(
      NODE_STATE_WORDS.check.older
    );
  } finally {
    view.unmount();
  }
});

// R10: a statement no check serves carries its gap, where the picture draws "No check proves
// this". The page says it in its own words and names the kind of gap it is.
test("a statement no check serves carries its gap in full", () => {
  const served = passing("c-one");
  const view = drawBody(
    [
      statementNode("r1", "The page draws it.", [checkNode(served)]),
      {
        ...statementNode("r2", "Nothing serves this one.", []),
        gaps: ["no_check"],
      },
    ],
    [served]
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-gap]")].map((gap) => [
        gap.getAttribute("data-gap-kind"),
        gap.textContent,
      ])
    ).toEqual([["no_check", GAP_LINES.no_check]]);
    expect(GAP_LINES.no_check).toBe("No check serves this statement.");
  } finally {
    view.unmount();
  }
});

// R17: "a command renders as code with a copy control ... and `when` reads as words", and R15:
// a check draws all four of its attributes and a result what it recorded. The body opens where
// the reader's focus reaches the check and closes when the focus leaves it; a press keeps it
// open once the focus has gone, and a press on an open one closes it (R6).
test("a check's facts open where the reader's focus is: when as words, the run as a command", async () => {
  const user = userEvent.setup();
  const shot = "dispatch://CORE-1/artifact/screenshot-png@v1";
  const seeded = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [result({ id: "result-one", screenshots: [shot] })],
  });
  const view = drawBody([statementNode("r1", "The page draws it.", [checkNode(seeded)])], [seeded]);
  try {
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    const words = view.container.querySelector<HTMLElement>("[data-verification-check-words]");
    expect(words).not.toBeNull();
    act(() => words?.focus());
    expect(words?.getAttribute("aria-expanded")).toBe("true");

    const card = view.container.querySelector("[data-verification-check]");
    expect((card?.textContent ?? "").includes(CHECK_WHEN_WORDS.before_merge)).toBe(true);
    expect(card?.textContent).not.toContain("before_merge");
    const commands = [...view.container.querySelectorAll("[data-node-command]")];
    expect(commands.length).toBeGreaterThan(0);
    expect(commands.map((command) => command.tagName)).toEqual(commands.map(() => "CODE"));
    expect(
      view.container.querySelectorAll(`[aria-label="${CONTROL_LABELS.copy_command}"]`).length
    ).toBe(commands.length);
    // The screenshot is drawn as a picture, through the bytes route every other picture of
    // Dispatch is drawn through.
    expect(view.container.querySelectorAll("[data-node-body] img").length).toBeGreaterThan(0);

    // The focus leaving it closes what the focus opened.
    act(() => words?.blur());
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    expect(words?.getAttribute("aria-expanded")).toBe("false");
    // The reader pressed it open, so it stays open with the focus elsewhere.
    await user.click(words as Element);
    act(() => words?.blur());
    expect(view.container.querySelectorAll("[data-node-body]").length).toBeGreaterThan(0);
    // And a press on an open one closes it, which is what the control says it does.
    await user.click(words as Element);
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    expect(words?.getAttribute("aria-expanded")).toBe("false");
  } finally {
    view.unmount();
  }
});

// R2 and the architect's ruling on rows: a statement is drawn as context, as the filter's own
// view marks it, with its words and the node its words came from as one line on its row.
test("a statement is marked as context, with its words and the node they came from", () => {
  const served = passing("c-one");
  const statement = statementNode(
    "r1",
    "The reader reads every word of this statement, which runs past a hundred characters so that nothing about it is cut.",
    [checkNode(served)]
  );
  const askedBy = node({
    children: [statement],
    id: "comment-1",
    text: "Where do I see what proves this?",
    type: "comment",
  });
  const view = drawBody([askedBy], [served]);
  try {
    const card = view.container.querySelector("[data-verification-card]");
    expect(card?.hasAttribute("data-node-context")).toBe(true);
    expect(card?.getAttribute("data-node-id")).toBe(statement.id);
    expect(card?.querySelector("[data-node-words]")?.textContent).toBe(statement.text);
    expect([
      card?.querySelector("[data-node-parent-line]")?.getAttribute("data-parent-type"),
      card?.querySelector("[data-node-parent-line]")?.textContent,
    ]).toEqual(["comment", `${NODE_TYPE_NAMES.comment}${askedBy.text}`]);
  } finally {
    view.unmount();
  }
});

// Ruling 6: the body draws the newest result of each check; its earlier results are one line
// with their count, which opens them in place, and the result the check's state stands on is
// marked wherever it is drawn.
test("a check draws its newest result, folds the rest behind their count, and marks the one that stands", async () => {
  const user = userEvent.setup();
  const busy = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({ created_at: "2026-11-12T09:00:00Z", id: "oldest", observed: "38 passed" }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "newest", observed: "41 passed" }),
      result({ created_at: "2026-11-13T09:00:00Z", id: "middle", observed: "40 passed" }),
    ],
  });
  const view = drawBody([statementNode("r1", "The page draws it.", [checkNode(busy)])], [busy]);
  try {
    const drawn = () =>
      [...view.container.querySelectorAll("[data-verification-result]")].map((one) => [
        one.getAttribute("data-node-id"),
        one.hasAttribute("data-result-stands"),
      ]);
    expect(drawn()).toEqual([["newest", true]]);
    expect(
      view.container
        .querySelector("[data-result-stands]")
        ?.textContent?.startsWith(VERIFICATION_WORDS.stands)
    ).toBe(true);
    const earlier = view.container.querySelector("[data-verification-earlier]");
    expect(earlier?.textContent?.replace(/\s+/gu, " ")).toBe(
      `2 ${VERIFICATION_WORDS.earlier_many}`
    );
    expect(earlier?.getAttribute("aria-expanded")).toBe("false");
    await user.click(earlier as Element);
    expect(earlier?.getAttribute("aria-expanded")).toBe("true");
    expect(drawn()).toEqual([
      ["newest", true],
      ["middle", false],
      ["oldest", false],
    ]);
  } finally {
    view.unmount();
  }
});

// R17, R6: the seven states a check can be in are named once over the table, each by its word
// and its shape with how many checks stand in it, so no shape of the table goes unnamed.
test("the summary names all seven states a check can be in, with how many checks stand in each", () => {
  const met = passing("c-one");
  const failed = failing("c-two");
  const view = drawBody(
    [
      statementNode("r1", "Every check of this one passed.", [checkNode(met)]),
      // One check under two statements is one check.
      statementNode("r2", "One check of this one failed.", [checkNode(failed), checkNode(met)]),
    ],
    [met, failed]
  );
  try {
    expect(
      [...view.container.querySelectorAll("[data-check-legend-state]")].map((one) => [
        one.getAttribute("data-check-legend-state"),
        one.querySelector("svg[data-glyph]")?.getAttribute("data-glyph"),
        (one.textContent ?? "").replace(/\s+/gu, " ").trim(),
      ])
    ).toEqual(
      (Object.keys(NODE_STATE_WORDS.check) as (keyof typeof NODE_STATE_WORDS.check)[]).map(
        (state) => [
          state,
          GLYPH_TOKENS.states.check[state],
          `${state === "passed" || state === "failed" ? 1 : 0}${NODE_STATE_WORDS.check[state]}`,
        ]
      )
    );
  } finally {
    view.unmount();
  }
});

// The chips of the picture, in the words R17 gives the three states: pressing one draws the
// statements in that state alone, pressing it again draws them all.
test("a chip draws the statements in one state alone, and pressing it again draws them all", async () => {
  const user = userEvent.setup();
  const met = passing("c-one");
  const failed = failing("c-two");
  const view = drawBody(
    [
      statementNode("r1", "Every check of this one passed.", [checkNode(met)]),
      statementNode("r2", "One check of this one failed.", [checkNode(failed)]),
    ],
    [met, failed]
  );
  try {
    expect(textsOf(view, "[data-verification-chip]")).toEqual([
      `${VERIFICATION_WORDS.all} 2`,
      `${VERIFICATION_WORDS.states.met} 1`,
      `${VERIFICATION_WORDS.states.not_met} 1`,
      `${VERIFICATION_WORDS.states.unknown} 0`,
    ]);
    const metChip = view.container.querySelector('[data-verification-chip="met"]');
    await user.click(metChip as Element);
    expect(
      [...view.container.querySelectorAll("[data-verification-card]")].map((card) =>
        card.getAttribute("data-statement-state")
      )
    ).toEqual(["met"]);
    expect(metChip?.getAttribute("aria-pressed")).toBe("true");

    await user.click(metChip as Element);
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(2);
    expect(metChip?.getAttribute("aria-pressed")).toBe("false");
  } finally {
    view.unmount();
  }
});

// R22: every word the page draws of its own comes from a declared list, and no identifier,
// slug or schema token is drawn where a word belongs.
test("every word the body draws of its own comes from the page's declared lists", () => {
  const seeded = passing("c-one");
  const view = drawBody(
    [
      statementNode("r1", "The page draws it.", [checkNode(seeded)]),
      { ...statementNode("r2", "Nothing serves this one.", []), gaps: ["no_check"] },
    ],
    [seeded]
  );
  try {
    const declared = new Set<string>();
    for (const words of [
      Object.values(VERIFICATION_WORDS).flatMap((word) =>
        typeof word === "string" ? [word] : Object.values(word)
      ),
      Object.values(NODE_STATE_WORDS.check),
      Object.values(NODE_TYPE_NAMES),
      Object.values(GAP_LINES),
      Object.values(CONTROL_LABELS),
      Object.values(NODE_FACT_LABELS),
      Object.values(BLOCK_ATTRIBUTE_LABELS),
      Object.values(CHECK_WHEN_WORDS),
      Object.values(RESULT_VERDICT_WORDS),
      Object.values(RESULT_STANDS_WORDS),
      Object.values(EMPTY_STATE_LINES),
    ]) {
      for (const word of words) {
        if (typeof word !== "string") continue;
        declared.add(word);
        declared.add(sentenceCase(word));
      }
    }
    // The page's own punctuation, which names nothing and says nothing.
    for (const mark of ["·", "—", ",", ""]) declared.add(mark);
    const undeclared = chromeTexts(view).filter(
      (drawn) => !declared.has(drawn.replace(/\s*·\s*$/u, "").trim())
    );
    expect(undeclared).toEqual([]);
  } finally {
    view.unmount();
  }
});

// A body with nothing to draw says so in the page's own line for it, and says nothing about
// the page (R22).
test("an issue with no checks and no statements draws the page's own empty line", () => {
  const view = drawBody([], []);
  try {
    expect(textsOf(view, "[data-verification-body]")).toEqual([EMPTY_STATE_LINES.verification]);
  } finally {
    view.unmount();
  }
});

// R19, R20: a check whose `serves` names no statement is a row of the filter, and so is a node
// an address named, which outranks the filter. The body draws both, so nothing the filter kept
// is lost.
test("a check that serves no statement, and a node an address named, are drawn", () => {
  const loose = passing("c-loose");
  const named = node({ id: "comment-1", text: "A comment an address named.", type: "comment" });
  const view = drawBody(
    [statementNode("r1", "The page draws it.", []), checkNode(loose), named],
    [loose],
    {
      named: [named.id],
    }
  );
  try {
    // The check's own words, and under its state the words its latest result observed.
    expect(textsOf(view, "[data-verification-loose] [data-node-words]")).toEqual([
      "The page draws c-loose.",
      "41 passed",
    ]);
    expect(textsOf(view, "[data-verification-loose] span[data-node-state]")).toEqual([
      NODE_STATE_WORDS.check.passed,
    ]);
    expect(
      [...view.container.querySelectorAll("[data-verification-other]")].map((drawn) => [
        drawn.getAttribute("data-node-type"),
        drawn.querySelector("[data-node-words]")?.textContent,
      ])
    ).toEqual([["comment", named.text]]);
  } finally {
    view.unmount();
  }
});
