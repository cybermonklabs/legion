import { beforeAll, expect, type Mock, mock, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  type RenderHookResult,
  type RenderResult,
  render,
  renderHook,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement, ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { countEngineParses, displayTree } from "../../../__tests__/display-trees";
import { installIntersectionObserver } from "../../../__tests__/intersection-observer";
import { api } from "../../../api/client";
import { issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type {
  Actor,
  CheckBlock,
  IssueCheckResults,
  IssueCoverage,
  IssueDetails,
} from "../../../api/types";
import { actorLabelText } from "../../refs/AgentName";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { type PublishedReferences, ReferenceNamesProvider } from "../../refs/reference-names";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { appKeymap } from "../../shell/keymap";
import { measureNearWords } from "../clamp-fixtures";
import { artifact, issueDetails } from "../decisions-filter/decisions-fixtures";
import { type IssueTreeReads, type TreeNode, targetNaming } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { NO_FAILED_READS } from "../outline-reads";
import {
  BLOCK_ATTRIBUTE_LABELS,
  CHECK_WHEN_WORDS,
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GLYPH_TOKENS,
  NODE_FACT_LABELS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
  STATE_LINES,
  sentenceCase,
  VERIFICATION_WORDS,
} from "../page-words";
import { presetView } from "../presets";
import { blockReads, Publisher, publishedNames } from "../projection-fixture";
import { ViewStage } from "../slots/stage";
import { drawStage, REFUSED, stubReads } from "../slots/stage-fixtures";
import { useVerificationReads } from "../slots/view-verification";
import { treeContext } from "../tree-drafts";
import { treeAnswer } from "../tree-read/tree-fixtures";
import type { TreeAnswer } from "../tree-read/tree-read";
import { shortCommit } from "./statement-state";
import {
  type FailedRead,
  queryTyped,
  VerificationFilterBody,
  type VerificationRead,
  type VerificationReads,
} from "./VerificationFilterBody";
import {
  COMMIT,
  check,
  checkNode,
  ISSUE_KEY,
  node,
  OTHER_COMMIT,
  result,
  statementNode,
  VERIFICATION_SLUG,
  verificationView,
} from "./verification-fixtures";

/**
 * What the Verification filter's body draws, read as a reader reads it, against the approved
 * picture `page-v3-verification-1440.png`.
 *
 * How the model reads the issue's results is `statement-state.test.ts`; these rows are about
 * what the page says: the covered count, the one commit, the word and the shape of each state,
 * the gap on a statement no check serves, and the facts a check's body opens. Where the three
 * columns stand side by side and where they stack is the browser's own layout, which no DOM of
 * a unit test lays out, so the widths are read by the browser suite (`e2e/issue-page-v.e2e.ts`).
 */

// A check's words are drawn from the results read's reading of them, which `displayTree` reads
// with the page's own engine.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** What a case gives the body beside its statements and its checks: the words the reader typed
 *  (`query`), which the view is filtered by (`presetView`), the nodes an address named, how the
 *  three reads stand (`reads`), which is all of them answered where a case gives nothing else,
 *  the view with no query where a case builds it itself (`whole`), which is otherwise built as
 *  the slot builds it, whether the body is drawn inside the stage the page draws a view in
 *  (`staged`), which moves the focus where a failed read's line leaves with it or the view's
 *  tree itself takes it (`ViewStage`), and what the page publishes of its batched names read
 *  (`published`), which is what it publishes for the issue's statements where a case gives
 *  nothing else (`publishedStatements`). */
interface DrawOptions {
  readonly named?: readonly string[];
  readonly published?: PublishedReferences;
  readonly query?: string;
  readonly reads?: VerificationReads;
  readonly staged?: boolean;
  readonly whole?: TreeNode;
}

/** Every statement `root` holds, at any depth, that has an address. */
function statementsOf(root: TreeNode): TreeNode[] {
  return [
    ...(root.statement === true && root.reference !== undefined ? [root] : []),
    ...root.children.flatMap(statementsOf),
  ];
}

/** What the page publishes for `statements`: what its batched read names them by, by the page's
 *  own rule (`targetNaming`) over the reads it names them from (`blockReads`, over the issue the
 *  statements are blocks of, unless a case hands the reads it built its chips from), and every
 *  statement's address as one the page asks for, answered or not yet. */
function publishedStatements(
  statements: readonly TreeNode[],
  reads: IssueTreeReads = blockReads(issueDetails(), statements)
): PublishedReferences {
  return {
    ...targetNaming(reads),
    asked: new Set(
      statements.flatMap(({ reference }) => (reference === undefined ? [] : [reference]))
    ),
  };
}

/** A query client for one drawn body, which answers nothing on its own. */
function testClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** The body over the statements given, with `checks` as the issue's results read, drawn from
 *  the Verification filter's own view: the tree it leaves and the rows it marks as context, and
 *  the view with no query as the slot builds it, which is given only where a query is typed: a
 *  query that is not empty once trimmed, as `presetView` reads it. It is drawn inside
 *  `NoTooltips`, as the slot draws it, so an author is named as the page names one, and where
 *  `staged` inside `ViewStage` around that, as the page's stage draws the slot; under the names
 *  store, with `published` published to it and handed to the body, as the slot hands it, and no
 *  names line (`NO_FAILED_READS`), as the slot hands it while no names read stands failed. A case
 *  that draws the body again with other reads passes it to `rerender` with the same `client`. */
function bodyOf(
  statements: readonly TreeNode[],
  checks: readonly CheckBlock[],
  {
    named = [],
    published: givenPublished,
    query = "",
    reads = { state: "answered" },
    staged = false,
    whole: given,
  }: DrawOptions = {},
  client: QueryClient = testClient()
): ReactElement {
  const issue = node({ children: statements, id: ISSUE_KEY, text: "An issue", type: "issue" });
  const view = presetView(issue, "verification", query, new Set(named));
  const whole =
    given ??
    (queryTyped(query) ? presetView(issue, "verification", "", new Set(named)).root : undefined);
  const published = givenPublished ?? publishedStatements(statementsOf(issue));
  const slot = (
    <NoTooltips>
      <Publisher published={published} />
      <VerificationFilterBody
        checks={checks}
        context={view.context}
        namesLine={NO_FAILED_READS}
        published={published}
        reads={reads}
        view={view.root}
        whole={whole}
      />
    </NoTooltips>
  );
  return (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <MemoryRouter>{staged ? <ViewStage>{slot}</ViewStage> : slot}</MemoryRouter>
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
}

/** The body `bodyOf` builds, drawn. */
function drawBody(
  statements: readonly TreeNode[],
  checks: readonly CheckBlock[],
  options: DrawOptions = {}
): RenderResult {
  return render(bodyOf(statements, checks, options));
}

/** Draws `view` again as `body`, a body `bodyOf` built with `staged`, and lets the stage place
 *  the focus: it places it a microtask after the render a failed read's line left in with the
 *  focus, and is told of the rows or the line its tree waits for a microtask after the render
 *  that draws them (`ViewStage`). */
async function redrawStaged(view: RenderResult, body: ReactElement): Promise<void> {
  view.rerender(body);
  await act(async () => undefined);
}

/** The text of every element matching `selector`, in the order they are drawn. */
function textsOf(view: RenderResult, selector: string): string[] {
  return [...view.container.querySelectorAll(selector)].map((element) =>
    (element.textContent ?? "").replace(/\s+/gu, " ").trim()
  );
}

/** What the page says in its own words: the text of each `[data-page-chrome]` element with
 *  every `[data-author-text]` descendant taken out, which is how `page-words.ts` says a reader of
 *  the page's words reads them, word by word. */
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

/** Each key a handler held back, in the order pressed, from now until `stop` is called. */
function holdKeys(): { held: string[]; stop: () => void } {
  const held: string[] = [];
  const hold = (event: KeyboardEvent) => {
    if (event.defaultPrevented) held.push(event.key);
  };
  document.addEventListener("keydown", hold);
  return { held, stop: () => document.removeEventListener("keydown", hold) };
}

/** The reads named refused once before they first answered, each by its name with its own
 *  Retry, which notes that read in `retried`, and none of them running again. The record holds
 *  them in the order named, which the body does not draw them by. */
function refusedReads(
  retried: VerificationRead[],
  ...reads: VerificationRead[]
): VerificationReads {
  const failed: Partial<Record<VerificationRead, FailedRead>> = {};
  for (const read of reads) {
    failed[read] = { failure: 1, retry: () => retried.push(read), retrying: false };
  }
  return { failed, state: "failed" };
}

/** The coverage read's answer for an issue with nothing to cover: no statement and no gap. */
const NO_COVERAGE: IssueCoverage = {
  gaps: [],
  has_plan: false,
  issue: ISSUE_KEY,
  requirements: [],
  spec_artifact_id: "",
};

/** The results read's answer for an issue with no check, which needs no blocks read. */
const NO_RESULTS: IssueCheckResults = {
  checks: [],
  issue: ISSUE_KEY,
  verification_artifact_id: "",
};

/** A check's words with a link and a reference in them. */
const LINKED_WORDS = "Read [the runbook](https://example.com/runbook) and dispatch://CORE-2 first.";

/** The node of a check whose words are `LINKED_WORDS`, with the results read's reading of them
 *  (`text_tree`), which `checkNode` leaves out. */
function linkedCheckNode(block: CheckBlock): TreeNode {
  return { ...checkNode(block), tree: displayTree(LINKED_WORDS) };
}

/** CORE-2 as its summary answers it: the name a reference to it in a stored text reads as, which
 *  the row reads itself where no batched read beside the body names it (R8, issue page decision
 *  350: a reference reads as its target's name, the short form gone). */
const CORE_2 = {
  documents: [],
  key: "CORE-2",
  project: "CORE",
  status: "in_progress",
  title: "Drain the queue",
} as const;

/** Answers the read of CORE-2's summary (`CORE_2`) until the returned spy is restored. */
function readsCore2() {
  return spyOn(api, "getIssueSummary").mockResolvedValue({ ...CORE_2, documents: [] });
}

// How many statements carry a check is drawn as covered, and no count, legend or heading reads
// proven or met for coverage; and the page names one commit for the issue's results, drawn once
// where the results are read.
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

// No result draws a commit on its own line, and the page names one commit for the issue's
// results, drawn once. With a check's facts open, each result's facts open with the check's, and
// they draw what the result recorded but its commit. No result draws a commit anywhere but the
// summary's one line, neither the commit the page names nor another a result ran at, whole or in
// the ten characters the summary draws.
test("with a check's facts open, no result draws its commit anywhere but the summary", async () => {
  const user = userEvent.setup();
  const busy = check({
    block_id: "c-one",
    results: [
      result({ commit_sha: OTHER_COMMIT, created_at: "2026-11-13T09:00:00Z", id: "older" }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "newest" }),
    ],
  });
  const view = drawBody([statementNode("r1", "The page draws it.", [checkNode(busy)])], [busy]);
  try {
    // The earlier result drawn too, and the check's facts opened by the focus on its words.
    await user.click(view.container.querySelector("[data-verification-earlier]") as Element);
    act(() =>
      view.container.querySelector<HTMLElement>("[data-verification-check-words]")?.focus()
    );
    expect(
      view.container.querySelector("[data-verification-check]")?.getAttribute("aria-expanded")
    ).toBe("true");
    // Each result's facts are drawn, its verdict among them.
    expect(
      [...view.container.querySelectorAll("[data-verification-result]")].map((drawn) => [
        drawn.getAttribute("data-node-id"),
        (drawn.querySelector("[data-node-body]")?.textContent ?? "").includes(
          RESULT_VERDICT_WORDS.pass
        ),
      ])
    ).toEqual([
      ["newest", true],
      ["older", true],
    ]);
    expect(textsOf(view, "[data-verification-commit]")).toEqual([
      `${VERIFICATION_WORDS.commit} ${shortCommit(COMMIT)}`,
    ]);
    // Everything the body draws but the summary's commit line.
    const body = view.container.querySelector("[data-verification-body]")?.cloneNode(true);
    expect(body).toBeInstanceOf(Element);
    for (const line of (body as Element).querySelectorAll("[data-verification-commit]")) {
      line.remove();
    }
    const rest = body?.textContent ?? "";
    for (const sha of [COMMIT, OTHER_COMMIT]) {
      expect(rest, `a result draws the commit ${sha}`).not.toContain(shortCommit(sha));
      expect(rest, `a result draws the commit ${sha}`).not.toContain(sha);
    }
  } finally {
    view.unmount();
  }
});

// Each of the seven states a check can be in is told apart by a word and a shape.
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
      drawn.map((held) => GLYPH_TOKENS.states.check[held.state as CheckState])
    );
    expect(new Set(drawn.map((held) => held.shape)).size).toBe(7);
    expect(new Set(drawn.map((held) => held.word)).size).toBe(7);
  } finally {
    view.unmount();
  }
});

// Met where every check of it passed at the commit the page names, Not met where one failed
// there, Unknown otherwise - here a pass the server reads at another commit than the one it
// compares the check with, so no pass of it stands: a check stands as the server reads it,
// `commit_stands` and `pass_stands`, as the page's model reads it everywhere.
test("a statement reads Met, Not met or Unknown by how its checks stand at that commit", () => {
  const met = passing("c-one");
  const failed = failing("c-two");
  const elsewhere = check({
    block_id: "c-three",
    pass_stands: { answer: "no" },
    results: [
      result({
        check_block_id: "c-three",
        commit_sha: OTHER_COMMIT,
        commit_stands: { answer: "other_commit" },
        id: "result-three",
      }),
    ],
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

// A statement no check serves carries its gap, where the picture draws "No check proves this".
// The page says it in its own words and names the kind of gap it is.
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

// While a document of the issue is still being indexed no statement is drawn as lacking a check:
// where its gap would be, the statement draws that document's line, in the server's own words.
test("a statement whose coverage waits on a document still being indexed draws its line and no gap", () => {
  const owed = "spec.md version 12 is still being indexed; its coverage shows once that ends.";
  const view = drawBody(
    [{ ...statementNode("r1", "The page waits for the index.", []), coverageNotes: [owed] }],
    []
  );
  try {
    expect(view.container.querySelector("[data-gap]")).toBeNull();
    expect(
      [...view.container.querySelectorAll("[data-coverage-note]")].map((note) => note.textContent)
    ).toEqual([owed]);
  } finally {
    view.unmount();
  }
});

// Where the coverage read counts more checks for a statement than the view with no query draws
// under it, the rest serve it from an issue under this one. The statement is covered, carries no
// gap, and draws one line after its checks with how many. A words query that leaves out the
// statement's own check neither calls that check "elsewhere" nor changes how the statement
// stands, what the summary counts or what the chips count and narrow by.
test("a statement that a child's check covers draws one line of the checks drawn elsewhere", async () => {
  const user = userEvent.setup();
  const own = passing("c-one");
  const partly = {
    ...statementNode("r1", "One check of this issue serves it.", [checkNode(own)]),
    counts: { checks: 2, planTasks: 0 },
  };
  const childOnly = {
    ...statementNode("r2", "Only the checks of an issue under this one serve it.", []),
    counts: { checks: 3, planTasks: 0 },
  };
  /** Each line of the checks drawn elsewhere: the statement whose card holds it, what it reads,
   *  its count as text of the reader's own, and whether its words are the page's. */
  const linesOf = (drawn: RenderResult) =>
    [...drawn.container.querySelectorAll("[data-verification-elsewhere]")].map((line) => [
      line.closest("[data-verification-card]")?.getAttribute("data-node-id"),
      (line.textContent ?? "").replace(/\s+/gu, " ").trim(),
      line.querySelector("[data-author-text]")?.textContent,
      line.hasAttribute("data-page-chrome"),
    ]);

  const unfiltered = drawBody([partly, childOnly], [own]);
  try {
    expect(linesOf(unfiltered)).toEqual([
      [partly.id, `1 ${VERIFICATION_WORDS.elsewhere_one}`, "1", true],
      [childOnly.id, `3 ${VERIFICATION_WORDS.elsewhere_many}`, "3", true],
    ]);
    expect([VERIFICATION_WORDS.elsewhere_one, VERIFICATION_WORDS.elsewhere_many]).toEqual([
      "more check elsewhere",
      "more checks elsewhere",
    ]);
    // The line stands after the statement's group of checks, inside its card.
    const line = unfiltered.container.querySelector("[data-verification-elsewhere]");
    expect(line?.previousElementSibling?.getAttribute("role")).toBe("group");
    // A statement only a child's checks serve carries no gap, and the covered count holds it.
    expect(unfiltered.container.querySelectorAll("[data-gap]")).toHaveLength(0);
    expect(textsOf(unfiltered, "[data-verification-covered]")).toEqual([
      `2 ${VERIFICATION_WORDS.of} 2 ${VERIFICATION_WORDS.covered}`,
    ]);

    // The query's words are the first statement's and no check's, so it leaves that statement
    // with no check drawn under it, and the second statement out.
    const filtered = drawBody([partly, childOnly], [own], { query: "this issue serves" });
    try {
      expect(filtered.container.querySelectorAll("[data-verification-check]")).toHaveLength(0);
      expect(
        [...filtered.container.querySelectorAll("[data-verification-card]")].map((card) =>
          card.getAttribute("data-node-id")
        )
      ).toEqual([partly.id]);
      // Its own check is still not "elsewhere".
      expect(linesOf(filtered)).toEqual([
        [partly.id, `1 ${VERIFICATION_WORDS.elsewhere_one}`, "1", true],
      ]);
      /** How the first statement stands: its state, its word, its shape and its card's edge. */
      const standing = (drawn: RenderResult) => {
        const card = drawn.container.querySelector("[data-verification-card]");
        return [
          card?.getAttribute("data-statement-state"),
          card?.querySelector("[data-statement-state]")?.textContent,
          card?.querySelector("svg[data-glyph]")?.getAttribute("data-glyph"),
          card?.classList.contains("border-l-page-failed"),
        ];
      };
      expect(standing(unfiltered)).toEqual([
        "met",
        VERIFICATION_WORDS.states.met,
        GLYPH_TOKENS.statements.met,
        false,
      ]);
      expect(standing(filtered)).toEqual(standing(unfiltered));
      // The summary and the chips are the issue's, as they are with no query.
      for (const selector of [
        "[data-verification-covered]",
        "[data-verification-legend]",
        "[data-verification-check-legend]",
        "[data-verification-chip]",
      ]) {
        expect(textsOf(filtered, selector)).toEqual(textsOf(unfiltered, selector));
      }
      // The bar's fills, each by its width, in both.
      const [filteredBar, unfilteredBar] = [filtered, unfiltered].map((drawn) =>
        [...drawn.container.querySelectorAll("[data-verification-bar] > i")].map((fill) =>
          fill.getAttribute("style")
        )
      );
      expect(filteredBar).toEqual(unfilteredBar);
      expect(textsOf(filtered, "[data-verification-covered]")).toEqual([
        `2 ${VERIFICATION_WORDS.of} 2 ${VERIFICATION_WORDS.covered}`,
      ]);
      expect(textsOf(filtered, "[data-verification-chip]")).toEqual([
        `${VERIFICATION_WORDS.all} 2`,
        `${VERIFICATION_WORDS.states.met} 1`,
        `${VERIFICATION_WORDS.states.not_met} 0`,
        `${VERIFICATION_WORDS.states.unknown} 1`,
      ]);
      // The chip Met narrows by the state the statement reads, so it draws it.
      await user.click(
        filtered.container.querySelector('[data-verification-chip="met"]') as Element
      );
      expect(
        [...filtered.container.querySelectorAll("[data-verification-card]")].map((card) =>
          card.getAttribute("data-node-id")
        )
      ).toEqual([partly.id]);
    } finally {
      filtered.unmount();
    }
  } finally {
    unfiltered.unmount();
  }

  // A count at or below the checks the view draws under the statement draws no line, nor does a
  // statement the coverage read counts nothing for; and until the reads have answered no line is
  // drawn at all.
  const two = passing("c-two");
  const even = {
    ...statementNode("r3", "Its one check is all the coverage read counts.", [checkNode(own)]),
    counts: { checks: 1, planTasks: 0 },
  };
  const fewer = {
    ...statementNode("r4", "The coverage read counts fewer checks than are drawn.", [
      checkNode(own),
      checkNode(two),
    ]),
    counts: { checks: 1, planTasks: 0 },
  };
  const uncounted = statementNode("r5", "The coverage read counts nothing for it.", [
    checkNode(two),
  ]);
  const none = drawBody([even, fewer, uncounted], [own, two]);
  try {
    expect(none.container.querySelectorAll("[data-verification-card]")).toHaveLength(3);
    expect(linesOf(none)).toEqual([]);
  } finally {
    none.unmount();
  }
  const waiting = drawBody([partly, childOnly], [own], { reads: { state: "waiting" } });
  try {
    expect(linesOf(waiting)).toEqual([]);
  } finally {
    waiting.unmount();
  }
});

// A command renders as code with a copy control and `when` reads as words, and a check draws all
// four of its attributes and a result what it recorded. The body opens where the reader's focus
// reaches the check and closes when the focus leaves it; a press keeps it open once the focus has
// gone, and a press on an open one closes it. The check's row says whether it is open, and its
// words, a tab stop with no role, take the press by a pointer, Enter or Space, as its latest
// result's cell takes a pointer's.
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
    const card = view.container.querySelector("[data-verification-check]");
    const words = view.container.querySelector<HTMLElement>("[data-verification-check-words]");
    expect(words).not.toBeNull();
    expect([
      words?.tabIndex,
      words?.hasAttribute("role"),
      words?.hasAttribute("aria-expanded"),
    ]).toEqual([0, false, false]);
    expect(card?.getAttribute("aria-expanded")).toBe("false");
    act(() => words?.focus());
    expect(card?.getAttribute("aria-expanded")).toBe("true");

    expect((card?.textContent ?? "").includes(CHECK_WHEN_WORDS.before_merge)).toBe(true);
    expect(card?.textContent).not.toContain("before_merge");
    const commands = [...view.container.querySelectorAll("[data-node-command]")];
    expect(commands.length).toBeGreaterThan(0);
    expect(commands.map((command) => command.tagName)).toEqual(commands.map(() => "CODE"));
    expect(
      view.container.querySelectorAll(`[aria-label="${CONTROL_LABELS.copy_command}"]`).length
    ).toBe(commands.length);
    // The screenshot is drawn as a picture: the opened facts hold an `img` for it. Whether its
    // bytes load is row 1's to read in the browser (`e2e/issue-page-v.e2e.ts`).
    expect(view.container.querySelectorAll("[data-node-body] img").length).toBeGreaterThan(0);

    // The focus leaving it closes what the focus opened.
    act(() => words?.blur());
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    expect(card?.getAttribute("aria-expanded")).toBe("false");
    // The reader pressed it open, so it stays open with the focus elsewhere.
    await user.click(words as Element);
    act(() => words?.blur());
    expect(view.container.querySelectorAll("[data-node-body]").length).toBeGreaterThan(0);
    // And a press on an open one closes it, which is what the control says it does.
    await user.click(words as Element);
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    expect(card?.getAttribute("aria-expanded")).toBe("false");

    // Enter and Space on the words are the same press, the focus on them, and Space scrolls
    // nothing: each key's own default is held back.
    expect(document.activeElement).toBe(words);
    const { held, stop } = holdKeys();
    try {
      await user.keyboard("{Enter}");
      expect(card?.getAttribute("aria-expanded")).toBe("true");
      expect(view.container.querySelectorAll("[data-node-body]").length).toBeGreaterThan(0);
      await user.keyboard(" ");
      expect(card?.getAttribute("aria-expanded")).toBe("false");
      expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    } finally {
      stop();
    }
    expect(held).toEqual(["Enter", " "]);

    // A pointer's press on the latest result's cell opens the facts, and they stay open once
    // the focus that press brought has gone.
    act(() => words?.blur());
    expect(card?.getAttribute("aria-expanded")).toBe("false");
    await user.click(view.container.querySelector("[data-verification-result]") as Element);
    act(() => (document.activeElement as HTMLElement | null)?.blur());
    expect(card?.getAttribute("aria-expanded")).toBe("true");
    expect(view.container.querySelectorAll("[data-node-body]").length).toBeGreaterThan(0);
  } finally {
    view.unmount();
  }
});

// A statement is drawn as context, as the filter's own view marks it, with its words and the
// node its words came from as one line on its row.
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

// A statement's parent line is the link a reader follows to the node its words came from where
// that node has an address, as the outline's parent line is (`node-line.tsx`'s
// `NodeParentLine`), and the link is a tab stop, since the tree's keys move between rows and
// reach no link inside one. A parent with no address is named by its words alone.
test("a statement's parent line links the node it came from where that node has an address", () => {
  const served = passing("c-one");
  const other = passing("c-two");
  const linked = statementNode("r1", "The reader reaches the comment this came from.", [
    checkNode(served),
  ]);
  const unlinked = statementNode("r2", "The reader reads where this came from.", [
    checkNode(other),
  ]);
  const addressed = node({
    children: [linked],
    id: "comment-1",
    reference: `dispatch://${ISSUE_KEY}/comment/comment-1`,
    text: "Where do I see what proves this?",
    type: "comment",
  });
  const unaddressed = node({
    children: [unlinked],
    id: "comment-2",
    text: "And where do I see this one?",
    type: "comment",
  });
  const view = drawBody([addressed, unaddressed], [served, other]);
  try {
    const withAddress = view.container.querySelector(
      `[data-verification-card][data-node-id="${linked.id}"] [data-node-parent-line]`
    );
    expect([
      withAddress?.getAttribute("data-parent-id"),
      withAddress?.getAttribute("data-parent-type"),
      withAddress?.textContent,
    ]).toEqual([addressed.id, "comment", `${NODE_TYPE_NAMES.comment}${addressed.text}`]);
    expect(
      [...(withAddress?.querySelectorAll<HTMLAnchorElement>("a") ?? [])].map((link) => [
        link.getAttribute("href"),
        link.tabIndex,
        link.textContent,
        link.hasAttribute("data-author-text"),
      ])
    ).toEqual([[`/issues/${ISSUE_KEY}/comments/comment-1`, 0, addressed.text, true]]);

    const withoutAddress = view.container.querySelector(
      `[data-verification-card][data-node-id="${unlinked.id}"] [data-node-parent-line]`
    );
    expect([
      withoutAddress?.getAttribute("data-parent-id"),
      withoutAddress?.getAttribute("data-parent-type"),
      withoutAddress?.textContent,
    ]).toEqual([unaddressed.id, "comment", `${NODE_TYPE_NAMES.comment}${unaddressed.text}`]);
    expect(withoutAddress?.querySelectorAll("a")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

/** A session nobody named, which the page calls by its word for an agent. */
const UNTITLED: Actor = { id: "0f8e2c4a-1111-4222-8333-444455556666", kind: "session" };

/** When the posts below were deleted, and when edited. */
const DELETED_AT = "2026-11-05T12:00:00Z";
const EDITED_AT = "2026-11-05T13:00:00Z";

/** A comment a session nobody named deleted, as the model holds it (`tree-drafts.ts`'s
 *  `addTurns`): its delete, and the deleted line as its text, the actor named as no registry
 *  knows it and the time as stored, with no reading. */
function deletedComment(id: string, children: readonly TreeNode[] = []): TreeNode {
  return node({
    children,
    deletedAt: DELETED_AT,
    deletedBy: UNTITLED,
    id,
    reference: `dispatch://${ISSUE_KEY}/comment/${id}`,
    text: `${CONTROL_LABELS.post_deleted_by} ${actorLabelText(UNTITLED)} · ${DELETED_AT}`,
    type: "comment",
  });
}

// R2 and R3 of dispatch://INFRA-1523: a statement's parent line is the outline's own
// (`NodeRow.tsx`'s `ParentLink`), so it carries a corrected post's marks. A comment its author
// deleted is drawn as its deleted line, its actor named as the page names an author and never as
// the model's line holds it, with the time as stored; one its author edited says ` · edited`
// after its line. Either stays the link to the comment, and a tab stop.
test("a statement's parent line draws a deleted comment as its deleted line, and an edited one with its mark", () => {
  const served = passing("c-one");
  const other = passing("c-two");
  const fromDeleted = statementNode("r1", "The reader reaches the comment this came from.", [
    checkNode(served),
  ]);
  const fromEdited = statementNode("r2", "The reader reads what the comment says now.", [
    checkNode(other),
  ]);
  const edited = node({
    children: [fromEdited],
    editedAt: EDITED_AT,
    id: "comment-2",
    reference: `dispatch://${ISSUE_KEY}/comment/comment-2`,
    text: "Where do I see what proves this?",
    type: "comment",
  });
  const view = drawBody([deletedComment("comment-1", [fromDeleted]), edited], [served, other]);
  try {
    const lineOf = (statement: TreeNode) =>
      view.container.querySelector(
        `[data-verification-card][data-node-id="${statement.id}"] [data-node-parent-line]`
      );
    const linksOf = (line: Element | null) =>
      [...(line?.querySelectorAll<HTMLAnchorElement>("a") ?? [])].map((link) => [
        link.getAttribute("href"),
        link.tabIndex,
      ]);

    const gone = lineOf(fromDeleted);
    const opening = `${CONTROL_LABELS.post_deleted_by} ${CONTROL_LABELS.unnamed_author} · `;
    const deletedLine = gone?.querySelector("a [data-post-deleted]");
    expect(deletedLine?.textContent?.slice(0, opening.length)).toBe(opening);
    expect(gone?.textContent).not.toContain(actorLabelText(UNTITLED));
    expect(gone?.textContent).not.toContain(DELETED_AT);
    expect(linksOf(gone)).toEqual([[`/issues/${ISSUE_KEY}/comments/comment-1`, 0]]);

    const marked = lineOf(fromEdited);
    expect(marked?.querySelector("a")?.textContent).toBe(
      `${edited.text} · ${CONTROL_LABELS.post_edited}`
    );
    expect(linksOf(marked)).toEqual([[`/issues/${ISSUE_KEY}/comments/comment-2`, 0]]);
  } finally {
    view.unmount();
  }
});

// The body draws the newest result of each check, as the approved picture does; its earlier
// results are one line with their count, which opens them in place, and the result the check's
// state stands on is marked wherever it is drawn.
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

// The results read answers again on every result recorded or withdrawn while the reader is in
// the view (`api/event-refresh.ts`), so a check's newest result changes under the reader. Its row
// under a statement is the same row after it: the focus on its words stays on the same element,
// the facts that focus opened stay open, and so do the earlier results the reader opened.
test("a check under a statement keeps the reader's focus and its open earlier results when its newest result changes", async () => {
  const user = userEvent.setup();
  const earlier = result({ created_at: "2026-11-13T09:00:00Z", id: "earlier" });
  const newest = result({ created_at: "2026-11-14T09:00:00Z", id: "newest" });
  const landed = result({
    created_at: "2026-11-15T09:00:00Z",
    id: "landed",
    observed: "42 passed",
  });
  const before = check({ block_id: "c-one", results: [earlier, newest] });
  const after = check({ block_id: "c-one", results: [earlier, newest, landed] });
  // One client for both reads, as the page keeps one while its reads answer again.
  const client = testClient();
  const drawn = (block: CheckBlock) =>
    bodyOf([statementNode("r1", "The page draws it.", [checkNode(block)])], [block], {}, client);
  const view = render(drawn(before));
  try {
    /** The check's row as the reader sees it: whether its facts and its earlier results are
     *  open, and the results it draws, by their ids. */
    const standing = () => {
      const row = view.container.querySelector("[data-verification-check]");
      return {
        earlier: row?.querySelector("[data-verification-earlier]")?.getAttribute("aria-expanded"),
        opened: row?.getAttribute("aria-expanded"),
        results: [...(row?.querySelectorAll("[data-verification-result]") ?? [])].map((one) =>
          one.getAttribute("data-node-id")
        ),
      };
    };
    await user.click(view.container.querySelector("[data-verification-earlier]") as Element);
    const words = view.container.querySelector<HTMLElement>("[data-verification-check-words]");
    act(() => words?.focus());
    expect(document.activeElement).toBe(words);
    expect(standing()).toEqual({
      earlier: "true",
      opened: "true",
      results: ["newest", "earlier"],
    });

    view.rerender(drawn(after));
    // The same element of the check's words, still in the page, still holds the focus.
    expect(view.container.querySelector("[data-verification-check-words]")).toBe(words);
    expect(words?.isConnected).toBe(true);
    expect(document.activeElement).toBe(words);
    expect(standing()).toEqual({
      earlier: "true",
      opened: "true",
      results: ["landed", "newest", "earlier"],
    });
  } finally {
    view.unmount();
  }
});

// The seven states a check can be in are named once over the table, each by its word and its
// shape with how many checks stand in it, so no shape of the table goes unnamed.
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
      (Object.keys(NODE_STATE_WORDS.check) as CheckState[]).map((state) => [
        state,
        GLYPH_TOKENS.states.check[state],
        `${state === "passed" || state === "failed" ? 1 : 0}${NODE_STATE_WORDS.check[state]}`,
      ])
    );
  } finally {
    view.unmount();
  }
});

// The chips of the picture, in the page's words for the three states: pressing one draws the
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

// No chip is held back from the reader. The keyboard reaches a chip whose state holds no
// statement, as it reaches every chip, and its press draws the page's line for that state rather
// than a table with nothing in it.
test("a chip whose state holds no statement draws the page's line for it", async () => {
  const user = userEvent.setup();
  const met = passing("c-one");
  const view = drawBody(
    [statementNode("r1", "Every check of this one passed.", [checkNode(met)])],
    [met]
  );
  try {
    const unknown = view.container.querySelector<HTMLButtonElement>(
      '[data-verification-chip="unknown"]'
    );
    expect(textsOf(view, '[data-verification-chip="unknown"]')).toEqual([
      `${VERIFICATION_WORDS.states.unknown} 0`,
    ]);
    expect(view.container.querySelectorAll("[data-verification-chip][disabled]")).toHaveLength(0);
    expect(view.container.querySelectorAll("[data-verification-none]")).toHaveLength(0);

    // From the chip All, Tab reaches Met, then Not met, which holds no statement either, then
    // Unknown; Enter presses it.
    act(() => view.container.querySelector<HTMLElement>('[data-verification-chip="all"]')?.focus());
    await user.tab();
    await user.tab();
    await user.tab();
    expect(document.activeElement).toBe(unknown);
    await user.keyboard("{Enter}");
    expect(unknown?.getAttribute("aria-pressed")).toBe("true");
    expect(textsOf(view, "[data-verification-none]")).toEqual([
      EMPTY_STATE_LINES.verification_none,
    ]);
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(0);

    // A second press draws every statement again, and the line goes.
    await user.click(unknown as Element);
    expect(unknown?.getAttribute("aria-pressed")).toBe("false");
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(1);
    expect(view.container.querySelectorAll("[data-verification-none]")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// Every word the page draws of its own comes from a declared list, and no identifier,
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
// the page.
test("an issue with no checks and no statements draws the page's own empty line", () => {
  const view = drawBody([], []);
  try {
    expect(textsOf(view, "[data-verification-body]")).toEqual([EMPTY_STATE_LINES.verification]);
  } finally {
    view.unmount();
  }
});

// A query that matches nothing keeps the issue's row, which says how the whole issue stands as it
// does with no query - the covered count, the bar, the legend of the seven states and the chips -
// and the table says under its headings, in the page's line for no match, that the query left
// nothing, as it says a chip's state holds no statement. An issue with nothing at all draws the
// page's line that it has no checks under a query too, alone in the tree with no row: the line for
// no match would say the issue holds something the query left out.
test("a query that matches nothing keeps the summary of the whole issue and says so under it", () => {
  const met = passing("c-one");
  const failed = failing("c-two");
  const loose = passing("c-loose");
  const under = [
    statementNode("r1", "Every check of this one passed.", [checkNode(met)]),
    statementNode("r2", "One check of this one failed.", [checkNode(failed)]),
    statementNode("r3", "Nothing serves this one.", []),
    checkNode(loose),
  ];
  const checks = [met, failed, loose];
  const unfiltered = drawBody(under, checks);
  const filtered = drawBody(under, checks, { query: "zebra quartz" });
  try {
    // Nothing of the issue holds the query's words, so no card and no row of a check is drawn.
    expect(
      filtered.container.querySelectorAll(
        "[data-verification-card], [data-verification-check], [data-verification-other]"
      )
    ).toHaveLength(0);
    // The issue's row is still the tree's first row and its one tab stop.
    const issueRow = filtered.container.querySelector('[role="tree"] > [data-verification-issue]');
    expect(issueRow?.getAttribute("tabindex")).toBe("0");
    expect(issueRow?.querySelector("[data-verification-summary]")).not.toBeNull();
    // Its summary, its legend of seven and its chips are the whole issue's, as with no query.
    for (const selector of [
      "[data-verification-covered]",
      "[data-verification-legend]",
      "[data-verification-check-legend]",
      "[data-verification-commit]",
      "[data-verification-chip]",
    ]) {
      expect(textsOf(filtered, selector), selector).toEqual(textsOf(unfiltered, selector));
    }
    const fills = (drawn: RenderResult) =>
      [...drawn.container.querySelectorAll("[data-verification-bar] > i")].map((fill) =>
        fill.getAttribute("style")
      );
    expect(fills(filtered)).toEqual(fills(unfiltered));
    expect(textsOf(filtered, "[data-verification-covered]")).toEqual([
      `2 ${VERIFICATION_WORDS.of} 3 ${VERIFICATION_WORDS.covered}`,
    ]);
    expect(filtered.container.querySelectorAll("[data-check-legend-state]")).toHaveLength(7);
    expect(textsOf(filtered, "[data-verification-chip]")).toEqual([
      `${VERIFICATION_WORDS.all} 3`,
      `${VERIFICATION_WORDS.states.met} 1`,
      `${VERIFICATION_WORDS.states.not_met} 1`,
      `${VERIFICATION_WORDS.states.unknown} 1`,
    ]);
    // The line is drawn once, the innermost element holding it, in the table under its
    // headings.
    const line = EMPTY_STATE_LINES.no_match;
    const lines = [...filtered.container.querySelectorAll("[data-verification-body] *")].filter(
      (element) =>
        element.textContent === line &&
        ![...element.children].some((child) => child.textContent === line)
    );
    expect(lines).toHaveLength(1);
    expect(lines[0]?.closest("[data-verification-table]")).not.toBeNull();
    expect(lines[0]?.previousElementSibling?.hasAttribute("data-verification-head")).toBe(true);
    expect(unfiltered.container.textContent).not.toContain(line);
  } finally {
    filtered.unmount();
    unfiltered.unmount();
  }

  const empty = drawBody([], [], { query: "zebra quartz" });
  try {
    expect(empty.container.querySelectorAll("[data-node-row]")).toHaveLength(0);
    expect(empty.container.querySelectorAll("[data-verification-table]")).toHaveLength(0);
    expect(textsOf(empty, '[data-verification-body] > [role="tree"]')).toEqual([
      EMPTY_STATE_LINES.verification,
    ]);
  } finally {
    empty.unmount();
  }
});

// A query whose words a result holds, and neither its check's nor its statement's, keeps the
// result alone, which the filter lifts under the statement with the check as its parent line
// (`presets.ts`). The body draws that result with its check, read from the issue's reading, under
// the statement and as a row like any other, so what the query kept is on screen. Where it is an
// earlier result of its check, the check draws it under its latest with its line of earlier
// results shut, and no other earlier result: from the row's first drawing, and when the query
// comes to keep it while the row is already drawn. Pressing the line draws every earlier result,
// each once, and pressing it again leaves the kept one drawn.
test("a query that keeps only a result draws it with its check under its statement", async () => {
  const user = userEvent.setup();
  const failure = "Timed out waiting for the zebra column.";
  /** Each card the body draws: its statement, and each check under it with its row's id and
   *  the words of each result it draws, in order. */
  const cardsOf = (drawn: RenderResult) =>
    [...drawn.container.querySelectorAll("[data-verification-card]")].map((card) => [
      card.getAttribute("data-node-id"),
      [...card.querySelectorAll("[data-verification-check]")].map((row) => [
        row.getAttribute("data-node-id"),
        row.getAttribute("data-verification-row"),
        [...row.querySelectorAll("[data-verification-result] [data-node-words]")].map(
          (words) => words.textContent
        ),
      ]),
    ]);
  /** The id of each row of the tree, in the order they stand. */
  const rowsOf = (drawn: RenderResult) =>
    [...drawn.container.querySelectorAll("[data-node-row]")].map((row) =>
      row.getAttribute("data-verification-row")
    );

  // The query keeps the check's latest result.
  const latest = check({
    block_id: "c-one",
    results: [result({ check_block_id: "c-one", id: "result-latest", observed: failure })],
  });
  const statement = statementNode("r1", "The page draws it.", [checkNode(latest)]);
  const latestRow = `${statement.id} ${checkNode(latest).id}`;
  const kept = drawBody([statement], [latest], { query: "zebra" });
  try {
    expect(cardsOf(kept)).toEqual([[statement.id, [[checkNode(latest).id, latestRow, [failure]]]]]);
    expect(rowsOf(kept)).toEqual([ISSUE_KEY, statement.id, latestRow]);
    expect(kept.container.querySelectorAll("[data-verification-loose]")).toHaveLength(0);
    expect(kept.container.querySelectorAll("[data-verification-none]")).toHaveLength(0);
  } finally {
    kept.unmount();
  }

  // The query keeps one of two earlier results: the check draws its latest and that one, and
  // leaves the other folded behind the line, which stays shut until it is pressed.
  const thrice = check({
    block_id: "c-two",
    results: [
      result({
        check_block_id: "c-two",
        created_at: "2026-11-12T09:00:00Z",
        id: "result-oldest",
        observed: "40 passed",
      }),
      result({
        check_block_id: "c-two",
        created_at: "2026-11-13T09:00:00Z",
        id: "result-earlier",
        observed: failure,
      }),
      result({ check_block_id: "c-two", created_at: "2026-11-14T09:00:00Z", id: "result-newest" }),
    ],
  });
  const served = statementNode("r2", "The page draws it again.", [checkNode(thrice)]);
  const thriceRow = `${served.id} ${checkNode(thrice).id}`;
  /** The one card of `served`, its check drawing the results whose words are `results`. */
  const servedCard = (results: string[]) => [
    [served.id, [[checkNode(thrice).id, thriceRow, results]]],
  ];
  const drawnEarlier = servedCard(["41 passed", failure]);
  const earlier = drawBody([served], [thrice], { query: "zebra" });
  try {
    const line = earlier.container.querySelector("[data-verification-earlier]");
    expect(cardsOf(earlier)).toEqual(drawnEarlier);
    expect(line?.getAttribute("aria-expanded")).toBe("false");
    // Pressed, the line draws every earlier result, the kept one among them, each once.
    await user.click(line as Element);
    expect(line?.getAttribute("aria-expanded")).toBe("true");
    expect(cardsOf(earlier)).toEqual(servedCard(["41 passed", failure, "40 passed"]));
    // Pressed again, it folds them and leaves the kept one drawn.
    await user.click(line as Element);
    expect(line?.getAttribute("aria-expanded")).toBe("false");
    expect(cardsOf(earlier)).toEqual(drawnEarlier);
  } finally {
    earlier.unmount();
  }

  // The reader types the query while the check's row is drawn: the row stays the same element,
  // and the query that comes to keep one earlier result draws it, the line still shut.
  const client = testClient();
  const typing = render(bodyOf([served], [thrice], {}, client));
  try {
    const row = typing.container.querySelector("[data-verification-check]");
    const earlierOf = () =>
      row?.querySelector("[data-verification-earlier]")?.getAttribute("aria-expanded");
    expect(earlierOf()).toBe("false");
    expect(cardsOf(typing)).toEqual(servedCard(["41 passed"]));
    typing.rerender(bodyOf([served], [thrice], { query: "zebra" }, client));
    expect(typing.container.querySelector("[data-verification-check]")).toBe(row);
    expect(earlierOf()).toBe("false");
    expect(cardsOf(typing)).toEqual(drawnEarlier);
  } finally {
    typing.unmount();
  }
});

// A check whose `serves` names no statement is a row of the filter, and so is a node an address
// named, which outranks the filter. The body draws both, so nothing the filter kept is lost.
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

// R2 and R3 of dispatch://INFRA-1523: a comment an address named is drawn with the marks the
// outline's row draws it with once its author corrected it. One its author deleted draws the
// deleted line in place of its words, its actor named as the page names an author and never as
// the model's text holds it, with the time as stored; one its author edited draws its words and
// says ` · edited` after its kind, outside its words, in the page's own words; one nobody edited
// says nothing of an edit.
test("a deleted comment an address named draws its deleted line, and an edited one its words and its mark", () => {
  const gone = deletedComment("comment-1");
  const edited = node({
    editedAt: EDITED_AT,
    id: "comment-2",
    reference: `dispatch://${ISSUE_KEY}/comment/comment-2`,
    text: "A comment its author edited.",
    type: "comment",
  });
  const unedited = node({ id: "comment-3", text: "A comment nobody edited.", type: "comment" });
  const view = drawBody(
    [statementNode("r1", "The page draws it.", []), gone, edited, unedited],
    [],
    { named: [gone.id, edited.id, unedited.id] }
  );
  try {
    const rowOf = (other: TreeNode) =>
      view.container.querySelector(`[data-verification-other][data-node-id="${other.id}"]`);

    const deletedRow = rowOf(gone);
    const opening = `${CONTROL_LABELS.post_deleted_by} ${CONTROL_LABELS.unnamed_author} · `;
    const deletedLine = deletedRow?.querySelector("[data-node-words] [data-post-deleted]");
    expect(deletedLine?.textContent?.slice(0, opening.length)).toBe(opening);
    expect(deletedRow?.textContent).not.toContain(actorLabelText(UNTITLED));
    expect(deletedRow?.textContent).not.toContain(DELETED_AT);

    const editedRow = rowOf(edited);
    expect(editedRow?.querySelector("[data-node-words]")?.textContent).toBe(edited.text);
    const mark = editedRow?.querySelector("[data-node-edited]");
    expect([mark?.textContent, mark?.hasAttribute("data-page-chrome")]).toEqual([
      ` · ${CONTROL_LABELS.post_edited}`,
      true,
    ]);
    expect(mark?.closest("[data-node-words]")).toBeNull();
    expect(rowOf(unedited)?.querySelector("[data-node-edited]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// The view keeps one tree, which holds everything it draws. Its first row is the issue's own,
// holding the summary and the chips, and it is the tree's one tab stop, so `t k` leaves the
// view's top on screen and the next Tab on the chip All. An issue with nothing to draw draws no
// row, and while a read is out the tree holds nothing at all and the body nothing but the tree.
test("the view's tree holds the issue's row first, and only it is a tab stop", () => {
  const met = passing("c-one");
  const loose = passing("c-loose");
  const named = node({ id: "comment-1", text: "A comment an address named.", type: "comment" });
  const view = drawBody(
    [
      statementNode("r1", "The page draws it.", [checkNode(met)]),
      statementNode("r2", "Nothing serves this one.", []),
      checkNode(loose),
      named,
    ],
    [met, loose],
    { named: [named.id] }
  );
  try {
    const body = view.container.querySelector("[data-verification-body]");
    const trees = view.container.querySelectorAll('[role="tree"]');
    expect(trees).toHaveLength(1);
    const tree = trees[0];
    expect(body?.childElementCount).toBe(1);
    expect(body?.firstElementChild).toBe(tree);
    expect(tree?.getAttribute("aria-label")).toBe(CONTROL_LABELS.tree);
    expect(tree?.getAttribute("tabindex")).toBe("-1");

    const rows = [...view.container.querySelectorAll('[role="treeitem"]')];
    const issueRow = rows[0];
    expect(tree?.firstElementChild).toBe(issueRow);
    expect(issueRow?.hasAttribute("data-verification-issue")).toBe(true);
    expect(issueRow?.hasAttribute("data-node-row")).toBe(true);
    expect(issueRow?.getAttribute("data-node-id")).toBe(ISSUE_KEY);
    expect(issueRow?.getAttribute("aria-level")).toBe("1");
    expect(issueRow?.getAttribute("tabindex")).toBe("0");
    expect(issueRow?.querySelector("[data-verification-summary]")).not.toBeNull();
    expect(issueRow?.querySelector("[data-verification-chips]")).not.toBeNull();
    // Every other row, in the order the page draws them: each statement and its checks, the
    // check that serves no statement, and the node the address named. None is a tab stop.
    expect(
      rows
        .slice(1)
        .map((row) => [
          row.getAttribute("data-node-type"),
          row.getAttribute("aria-level"),
          row.getAttribute("tabindex"),
          row.hasAttribute("data-node-row"),
        ])
    ).toEqual([
      ["requirement", "1", "-1", true],
      ["check", "2", "-1", true],
      ["requirement", "1", "-1", true],
      ["check", "1", "-1", true],
      ["comment", "1", "-1", true],
    ]);
    // A statement's checks are one group of rows under it, drawn open; a statement no check
    // serves holds no group and says nothing of being open.
    const served = rows[1];
    expect(served?.querySelectorAll('[role="group"]')).toHaveLength(1);
    expect(served?.querySelector('[role="group"] > [role="treeitem"]')).toBe(rows[2] ?? null);
    expect(served?.getAttribute("aria-expanded")).toBe("true");
    expect(rows[3]?.querySelectorAll('[role="group"]')).toHaveLength(0);
    expect(rows[3]?.hasAttribute("aria-expanded")).toBe(false);
    // The outline's own mark is no mark of this view's rows.
    expect(view.container.querySelectorAll("[data-row-key]")).toHaveLength(0);
  } finally {
    view.unmount();
  }

  const empty = drawBody([], []);
  try {
    expect(empty.container.querySelectorAll("[data-node-row]")).toHaveLength(0);
    expect(textsOf(empty, '[data-verification-body] > [role="tree"]')).toEqual([
      EMPTY_STATE_LINES.verification,
    ]);
  } finally {
    empty.unmount();
  }

  const waiting = drawBody([statementNode("r1", "The page draws it.", [checkNode(met)])], [met], {
    reads: { state: "waiting" },
  });
  try {
    const tree = waiting.container.querySelector('[data-verification-body] > [role="tree"]');
    expect(tree).not.toBeNull();
    expect(tree?.childElementCount).toBe(0);
    expect(textsOf(waiting, "[data-verification-body]")).toEqual([""]);
    expect(waiting.container.querySelectorAll("[data-node-row]")).toHaveLength(0);
    expect(waiting.container.querySelector("[data-verification-body]")?.childElementCount).toBe(1);
    expect(waiting.container.querySelectorAll("[data-verification-unreadable]")).toHaveLength(0);
  } finally {
    waiting.unmount();
  }
});

// A read refused before it first answers holds nothing to draw from, so the body says so over
// its tree, one line for each such read in the page's words for it with a Retry that asks that
// read alone again. It draws no row, since a statement no blocks read placed its check under, or
// a check no results read says how it stands, would say what is not so, and it keeps its tree
// with nothing in it, as while a read is out. No line stands inside the tree, whose own structure
// an alert would break.
test("a read that fails before it first answers says so with a Retry, for each of the view's three reads", async () => {
  const user = userEvent.setup();
  const met = passing("c-one");
  const statements = [statementNode("r1", "The page draws it.", [checkNode(met)])];
  // The outline's words for each read (`STATE_LINES`): which checks serve each statement, the
  // checks' results, and the Verification document for the blocks read.
  const words: Readonly<Record<VerificationRead, string>> = {
    blocks: STATE_LINES.verification_unreadable,
    coverage: STATE_LINES.coverage_unreadable,
    results: STATE_LINES.results_unreadable,
  };
  expect([words.coverage, words.results, words.blocks]).toEqual([
    "Could not load which checks serve each statement.",
    "Could not load the checks' results.",
    "Could not load the verification document.",
  ]);
  /** Each failed-read line the body draws, in order: the read it names, its words (the alert's
   *  own text, which the shared line draws as the alert itself) and the name of each of its
   *  buttons. */
  const linesOf = (drawn: RenderResult) =>
    [...drawn.container.querySelectorAll<HTMLElement>("[data-verification-unreadable]")].map(
      (line) => [
        line.getAttribute("data-verification-unreadable"),
        within(line).getByRole("alert").textContent,
        within(line)
          .getAllByRole("button")
          .map((button) => button.textContent),
      ]
    );
  /** What the body holds beside its lines: how many cards, rows and trees it draws, and its last
   *  element, under every line, which is its one tree: its role, how many children it holds,
   *  and whether a line stands inside it. */
  const treeOf = (drawn: RenderResult) => {
    const last = drawn.container.querySelector("[data-verification-body]")?.lastElementChild;
    return {
      cards: drawn.container.querySelectorAll("[data-verification-card]").length,
      children: last?.childElementCount,
      lineInside: last?.querySelector('[role="alert"]') !== null,
      role: last?.getAttribute("role"),
      rows: drawn.container.querySelectorAll("[data-node-row]").length,
      trees: drawn.container.querySelectorAll('[role="tree"]').length,
    };
  };
  const emptyTree = { cards: 0, children: 0, lineInside: false, role: "tree", rows: 0, trees: 1 };

  for (const read of ["coverage", "results", "blocks"] as const) {
    const retried: VerificationRead[] = [];
    const view = drawBody(statements, [met], { reads: refusedReads(retried, read) });
    try {
      expect(linesOf(view)).toEqual([[read, words[read], ["Retry"]]]);
      expect(treeOf(view)).toEqual(emptyTree);
      await user.click(within(view.container).getByRole("button", { name: "Retry" }));
      expect(retried).toEqual([read]);
    } finally {
      view.unmount();
    }
  }

  // All three failed: three lines in the body's own order, coverage, results, blocks, and each
  // Retry asks its own read and no other.
  const retried: VerificationRead[] = [];
  const all = drawBody(statements, [met], {
    reads: refusedReads(retried, "coverage", "results", "blocks"),
  });
  try {
    expect(linesOf(all)).toEqual([
      ["coverage", words.coverage, ["Retry"]],
      ["results", words.results, ["Retry"]],
      ["blocks", words.blocks, ["Retry"]],
    ]);
    expect(treeOf(all)).toEqual(emptyTree);
    // Each line's own Retry, pressed in another order than the lines stand in.
    const [coverage, results, blocks] = [
      ...all.container.querySelectorAll<HTMLElement>("[data-verification-unreadable]"),
    ].map((line) => within(line).getByRole("button", { name: "Retry" }));
    await user.click(blocks);
    await user.click(coverage);
    await user.click(results);
    expect(retried).toEqual(["blocks", "coverage", "results"]);
  } finally {
    all.unmount();
  }
});

// A screen reader reads a failed read's alert each time it is drawn. The line keys its alert by
// the read's count of refusals (`FailedReadLine`'s `failure`), so each new refusal is said again,
// and a run that pauses while the browser is offline, which ends the run with no new refusal,
// does not say the failure again.
test("a failed read's alert is drawn again for each refusal, and not when its run pauses with no new refusal", () => {
  const met = passing("c-one");
  const statements = [statementNode("r1", "The page draws it.", [checkNode(met)])];
  const retry = () => undefined;
  /** The coverage read refused `failure` times, running again or not as `retrying` says. */
  const refused = (failure: number, retrying: boolean): VerificationReads => ({
    failed: { coverage: { failure, retry, retrying } },
    state: "failed",
  });
  // One client for every drawing, as the page keeps one while its reads answer again.
  const client = testClient();
  const view = render(bodyOf(statements, [met], { reads: refused(1, true) }, client));
  /** The alert on the coverage read's line. */
  const alert = () =>
    view.container.querySelector('[data-verification-unreadable="coverage"] [role="alert"]');
  try {
    const first = alert();
    expect(first, "the refused coverage read's line drew no alert").not.toBeNull();
    // Its run paused while the browser is offline: it runs no more, and was refused no more.
    view.rerender(bodyOf(statements, [met], { reads: refused(1, false) }, client));
    expect(alert(), "a run that paused with no new refusal drew the line's alert again").toBe(
      first
    );
    // Refused again.
    view.rerender(bodyOf(statements, [met], { reads: refused(2, false) }, client));
    expect(alert(), "a new refusal left the line's old alert in place").not.toBe(first);
  } finally {
    view.unmount();
  }
});

// A press by the keyboard never leaves the focus on the page's body. The body is handed its reads
// by hand here, so each step is a line that leaves the page, with the focus on its Retry, as its
// read stops reading as refused, and the stage the page draws the view in (`ViewStage`) keeps the
// focus in the view: on the next refused read's Retry, on the view's tree while the read is out,
// back on the read's Retry when it is refused again, and on the tree's tab stop, the issue's row,
// once it answers.
test("Enter on a Retry keeps the focus in the view: on the next refused read's Retry, on the empty tree while the read is out, on its Retry when refused again, and on the issue's row once it answers", async () => {
  const user = userEvent.setup();
  const met = passing("c-one");
  const statements = [statementNode("r1", "The page draws it.", [checkNode(met)])];
  const retried: VerificationRead[] = [];
  const failed = (...reads: VerificationRead[]) => refusedReads(retried, ...reads);
  // One client for every drawing, as the page keeps one while its reads answer again.
  const client = testClient();
  const view = render(
    bodyOf(statements, [met], { reads: failed("coverage", "blocks"), staged: true }, client)
  );
  const redraw = (reads: VerificationReads) =>
    redrawStaged(view, bodyOf(statements, [met], { reads, staged: true }, client));
  /** The Retry on the line of `read`. */
  const retryOf = (read: VerificationRead) =>
    within(
      view.container.querySelector<HTMLElement>(
        `[data-verification-unreadable="${read}"]`
      ) as HTMLElement
    ).getByRole("button", { name: "Retry" });
  const tree = () => view.container.querySelector('[role="tree"]');
  try {
    act(() => retryOf("coverage").focus());
    await user.keyboard("{Enter}");
    expect(retried).toEqual(["coverage"]);
    // The coverage read is out again, and the blocks read is still refused.
    await redraw(failed("blocks"));
    expect(document.activeElement, "the focus left the view with coverage's line").toBe(
      retryOf("blocks")
    );
    await user.keyboard("{Enter}");
    expect(retried).toEqual(["coverage", "blocks"]);
    // The blocks read is out again, and no read is refused.
    await redraw({ state: "waiting" });
    expect(document.activeElement).not.toBe(document.body);
    expect(document.activeElement, "the focus left the view while the read is out").toBe(tree());
    // Refused again: its line comes back, and its Retry takes the focus from the tree.
    await redraw(failed("blocks"));
    expect(document.activeElement, "the read refused again left the focus on the tree").toBe(
      retryOf("blocks")
    );
    await user.keyboard("{Enter}");
    expect(retried).toEqual(["coverage", "blocks", "blocks"]);
    await redraw({ state: "waiting" });
    expect(document.activeElement).toBe(tree());
    // It answers: the tree's tab stop, the issue's row, takes the focus.
    await redraw({ state: "answered" });
    expect(document.activeElement, "the rows that came left the focus on the tree").toBe(
      view.container.querySelector("[data-verification-issue]")
    );
  } finally {
    view.unmount();
  }
});

// The body says the reads that failed in one order of its own, coverage, results, blocks,
// whatever order it is given them in, so which Retry takes the focus when a line leaves with it
// never hangs on the order the slot filled its record in. Given the reads blocks, results,
// coverage, it draws coverage, results, blocks; once coverage's line leaves with the focus, the
// focus goes to results' Retry, the body's next line, and not to blocks', which the record names
// first.
test("the failed reads' lines stand in the body's own order whatever order they are given in, and the focus moves from the first to the next", async () => {
  const user = userEvent.setup();
  const met = passing("c-one");
  const statements = [statementNode("r1", "The page draws it.", [checkNode(met)])];
  const retried: VerificationRead[] = [];
  // One client for every drawing, as the page keeps one while its reads answer again.
  const client = testClient();
  const view = render(
    bodyOf(
      statements,
      [met],
      { reads: refusedReads(retried, "blocks", "results", "coverage"), staged: true },
      client
    )
  );
  /** The read each failed-read line names, in the order the lines stand. */
  const lines = () =>
    [...view.container.querySelectorAll("[data-verification-unreadable]")].map((line) =>
      line.getAttribute("data-verification-unreadable")
    );
  /** The Retry on the line of `read`. */
  const retryOf = (read: VerificationRead) =>
    within(
      view.container.querySelector<HTMLElement>(
        `[data-verification-unreadable="${read}"]`
      ) as HTMLElement
    ).getByRole("button", { name: "Retry" });
  try {
    expect(lines(), "the lines stand in the order the reads were given in").toEqual([
      "coverage",
      "results",
      "blocks",
    ]);
    const [first] = within(view.container).getAllByRole("button", { name: "Retry" });
    expect(first, "the first Retry is another read's than coverage's").toBe(retryOf("coverage"));
    act(() => first?.focus());
    await user.keyboard("{Enter}");
    expect(retried).toEqual(["coverage"]);
    // The coverage read is out again, and the results and blocks reads are still refused, the
    // record holding the blocks read first.
    await redrawStaged(
      view,
      bodyOf(
        statements,
        [met],
        { reads: refusedReads(retried, "blocks", "results"), staged: true },
        client
      )
    );
    expect(lines(), "the lines left stand in the order the reads were given in").toEqual([
      "results",
      "blocks",
    ]);
    expect(document.activeElement, "the focus moved elsewhere than the next line's Retry").toBe(
      retryOf("results")
    );
  } finally {
    view.unmount();
  }
});

// A line can leave with no press too, and the focus stays in the view all the same: on the view's
// tree while the read is out, on the read's Retry when it is refused again, and on the tree's tab
// stop, the issue's row, once it answers. The page asks a refused read again with no press, on
// the window's focus and on an event that names its key. The case hands the body those reads by
// hand, one after another, and presses nothing.
test("a refused read's line that leaves with no press keeps the focus in the view: on the empty tree while the read is out, on its Retry when refused again, and on the issue's row once it answers", async () => {
  const met = passing("c-one");
  const statements = [statementNode("r1", "The page draws it.", [checkNode(met)])];
  const refused = refusedReads([], "results");
  // One client for every drawing, as the page keeps one while its reads answer again.
  const client = testClient();
  const view = render(bodyOf(statements, [met], { reads: refused, staged: true }, client));
  const redraw = (reads: VerificationReads) =>
    redrawStaged(view, bodyOf(statements, [met], { reads, staged: true }, client));
  /** The Retry on the results read's line. */
  const retry = () =>
    within(
      view.container.querySelector<HTMLElement>(
        '[data-verification-unreadable="results"]'
      ) as HTMLElement
    ).getByRole("button", { name: "Retry" });
  try {
    act(() => retry().focus());
    expect(document.activeElement).toBe(retry());
    // Asked again with nothing pressed: the read is out, and its line is gone.
    await redraw({ state: "waiting" });
    expect(document.activeElement, "the focus left the view while the read is out").toBe(
      view.container.querySelector('[role="tree"]')
    );
    // Refused again: its line comes back, and its Retry takes the focus from the tree.
    await redraw(refused);
    expect(document.activeElement, "the read refused again left the focus on the tree").toBe(
      retry()
    );
    // Asked again once more, it answers: the tree's tab stop, the issue's row, takes the focus.
    await redraw({ state: "answered" });
    expect(document.activeElement, "the read that answered left the focus off the view").toBe(
      view.container.querySelector("[data-verification-issue]")
    );
  } finally {
    view.unmount();
  }
});

// `t k` with a refused read puts the focus on that read's Retry, not on the empty tree, which has
// nothing to act on and from which Tab never reaches the Retry. The page's chord focuses the tree,
// which holds no row to take its tab stop, so the stage hands the tree's focus to the Retry. A
// tree with rows keeps a focus that a click on its headings gives it: only rows that come where
// the tree drew none take it.
test("with a read refused, the view's tree gives its focus to that read's Retry", () => {
  const met = passing("c-one");
  const statements = [statementNode("r1", "The page draws it.", [checkNode(met)])];
  const refused = drawBody(statements, [met], { reads: refusedReads([], "results"), staged: true });
  try {
    const tree = refused.container.querySelector<HTMLElement>('[role="tree"]');
    act(() => tree?.focus());
    expect(document.activeElement, "the focused tree kept the focus with a read refused").toBe(
      within(
        refused.container.querySelector<HTMLElement>(
          '[data-verification-unreadable="results"]'
        ) as HTMLElement
      ).getByRole("button", { name: "Retry" })
    );
  } finally {
    refused.unmount();
  }

  const client = testClient();
  const drawn = render(bodyOf(statements, [met], { staged: true }, client));
  try {
    const tree = drawn.container.querySelector<HTMLElement>('[role="tree"]');
    act(() => tree?.focus());
    expect(
      document.activeElement,
      "a tree with rows gave the focus a click on its headings gave it to a row"
    ).toBe(tree);
  } finally {
    drawn.unmount();
  }
});

/** The Verification document's blocks as the slot hands them to the view's reads
 *  (`useVerificationReads`): the tree read's, its answer held. */
const HELD_TREE = {
  data: [],
  errorUpdateCount: 0,
  isError: false,
  isFetching: false,
  isSuccess: true,
  refetch: () => Promise.resolve(),
};

// Staleness is per observer. The view's own observers of the coverage and the results
// (`useVerificationReads`) hold what they read until an event refreshes it, so observers that
// mount 31 s after the page read both, past the app's 30 s (`main.tsx`), as the stage mounts the
// view's slot when the view is turned on, send no request. Nothing is pressed here: the hook is
// mounted over a cache whose answers are 31 s old.
test("the view's own reads of the coverage and the results send no request when they mount 31 s after the page read them", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const answeredAt = Date.now() - 31_000;
  client.setQueryData(issueCoverageQuery(ISSUE_KEY).queryKey, NO_COVERAGE, {
    updatedAt: answeredAt,
  });
  client.setQueryData(issueResultsQuery(ISSUE_KEY).queryKey, NO_RESULTS, { updatedAt: answeredAt });
  const coverageRead = spyOn(api, "getIssueCoverage").mockResolvedValue(NO_COVERAGE);
  const resultsRead = spyOn(api, "getIssueResults").mockResolvedValue(NO_RESULTS);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  try {
    const pressed = renderHook(() => useVerificationReads(ISSUE_KEY, HELD_TREE), { wrapper });
    try {
      await act(() => Bun.sleep(20));
      expect(pressed.result.current.reads).toEqual({ state: "answered" });
      expect(coverageRead, "the view read the coverage again").toHaveBeenCalledTimes(0);
      expect(resultsRead, "the view read the results again").toHaveBeenCalledTimes(0);
    } finally {
      pressed.unmount();
    }
  } finally {
    coverageRead.mockRestore();
    resultsRead.mockRestore();
    client.clear();
  }
});

/**
 * What the reads stand at, and how often each of the view's reads was asked, once the read
 * `refused` was refused before it first answered and the Retry `useVerificationReads` gives its
 * line was called: over the Verification document's blocks held (`HELD_TREE`), so nothing asks
 * the tree read again, which the blocks are in, but a Retry that asked the wrong read.
 */
async function retried(
  refused: "coverage" | "results"
): Promise<{ asked: Record<VerificationRead, number>; reads: VerificationReads }> {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = { ...HELD_TREE, refetch: mock(() => Promise.resolve()) };
  const coverageRead = spyOn(api, "getIssueCoverage").mockResolvedValue(NO_COVERAGE);
  const resultsRead = spyOn(api, "getIssueResults").mockResolvedValue(NO_RESULTS);
  if (refused === "coverage") coverageRead.mockRejectedValueOnce(new Error("Refused."));
  else resultsRead.mockRejectedValueOnce(new Error("Refused."));
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(() => useVerificationReads(ISSUE_KEY, tree), { wrapper });
  try {
    await waitFor(() => expect(hook.result.current.reads.state).toBe("failed"));
    const before = hook.result.current.reads;
    const failed = before.state === "failed" ? before.failed : undefined;
    expect(Object.keys(failed ?? {})).toEqual([refused]);
    act(() => failed?.[refused]?.retry());
    await act(() => Bun.sleep(20));
    return {
      asked: {
        blocks: tree.refetch.mock.calls.length,
        coverage: coverageRead.mock.calls.length,
        results: resultsRead.mock.calls.length,
      },
      reads: hook.result.current.reads,
    };
  } finally {
    hook.unmount();
    coverageRead.mockRestore();
    resultsRead.mockRestore();
    client.clear();
  }
}

// Each read refused before it first answered comes with its own Retry (`useVerificationReads`),
// which the body draws on that read's line, so no read's name is decoded on the way to it: the
// Retry of a refused coverage read asks the coverage read again, and no other read.
test("the Retry of a refused coverage read asks the coverage read again and no other", async () => {
  const { asked, reads } = await retried("coverage");
  expect(asked, "the Retry asked another read than the coverage").toEqual({
    blocks: 0,
    coverage: 2,
    results: 1,
  });
  expect(reads).toEqual({ state: "answered" });
});

// The same of a refused results read: its Retry asks the results read again, and no other read.
test("the Retry of a refused results read asks the results read again and no other", async () => {
  const { asked, reads } = await retried("results");
  expect(asked, "the Retry asked another read than the results").toEqual({
    blocks: 0,
    coverage: 1,
    results: 2,
  });
  expect(reads).toEqual({ state: "answered" });
});

/** The view's reads as `refusedCoverage` mounts them, its coverage read refused once. */
interface RefusedCoverage {
  /** The coverage read's spy: refused once, then answering `NO_COVERAGE`, unless a case queues
   *  another answer on it, which its next request takes. */
  readonly coverageRead: Mock<typeof api.getIssueCoverage>;
  /** The hook mounted, of which the cases read how the reads stand. */
  readonly hook: RenderHookResult<{ readonly reads: VerificationReads }, unknown>;
  /** Unmounts the hook, restores both spies and clears the client. */
  readonly restore: () => void;
}

/**
 * The view's reads (`useVerificationReads`) over a client of their own that asks no read again
 * by itself, over an issue with no check, once the coverage read was refused the first time it
 * was asked and reads as failed; the results read answers. A case that needs the coverage read's
 * next request to answer otherwise queues that answer on `coverageRead` before it asks again: the
 * spy's queued answers run in order, so one queued after the refusal is the next request's.
 */
async function refusedCoverage(): Promise<RefusedCoverage> {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const coverageRead = spyOn(api, "getIssueCoverage").mockResolvedValue(NO_COVERAGE);
  coverageRead.mockRejectedValueOnce(new Error("Refused."));
  const resultsRead = spyOn(api, "getIssueResults").mockResolvedValue(NO_RESULTS);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(() => useVerificationReads(ISSUE_KEY, HELD_TREE), { wrapper });
  const restore = () => {
    hook.unmount();
    coverageRead.mockRestore();
    resultsRead.mockRestore();
    client.clear();
  };
  try {
    await waitFor(() => expect(hook.result.current.reads.state).toBe("failed"));
  } catch (error) {
    restore();
    throw error;
  }
  return { coverageRead, hook, restore };
}

// TanStack Query puts a read that holds no answer back to `pending` while it runs again, so a
// read refused before it first answered, asked again, would read as out rather than failed and
// take its line, with the Retry the reader's focus may be on, off the page until it answered or
// was refused again. It still reads as failed while the new request is out, its Retry running,
// and leaves the failed state once it answers.
test("a refused read asked again reads as failed, with its Retry running, while the new request is out, and as answered once it answers", async () => {
  const { coverageRead, hook, restore } = await refusedCoverage();
  try {
    const refused = hook.result.current.reads;
    expect(refused, "the refused coverage read does not read as failed, its Retry at rest").toEqual(
      {
        failed: { coverage: { failure: 1, retry: expect.any(Function), retrying: false } },
        state: "failed",
      }
    );
    // The coverage read's request after its refusal, which answers when the case says.
    const again = Promise.withResolvers<IssueCoverage>();
    coverageRead.mockReturnValueOnce(again.promise);
    act(() => (refused.state === "failed" ? refused.failed.coverage?.retry() : undefined));
    await waitFor(() => expect(coverageRead).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(
        hook.result.current.reads,
        "the coverage read asked again does not read as failed, its Retry running, while its request is out"
      ).toEqual({
        failed: { coverage: { failure: 1, retry: expect.any(Function), retrying: true } },
        state: "failed",
      })
    );
    await act(async () => {
      again.resolve(NO_COVERAGE);
      await again.promise;
    });
    await waitFor(() =>
      expect(
        hook.result.current.reads,
        "the coverage read that answered does not read as answered"
      ).toEqual({ state: "answered" })
    );
  } finally {
    restore();
  }
});

// Under the Verification filter the Verification document's blocks are the view's own read
// (`useTreeRead`'s `blocksOf`), and the blocks line runs by that read (`useBlocksRunning`), not by
// Conversation's, which the page reads beside the view. On the page's own stage, with
// Conversation's read out and the view read refused, the blocks line's Retry sends the view read
// again, the line is busy while it runs, and it leaves once the view read answers.
test("with Conversation's read out, the blocks line's Retry sends the refused Verification view read, and is busy while it runs", async () => {
  const user = userEvent.setup();
  const stubs = stubReads();
  const base = issueDetails();
  const issue: IssueDetails = {
    ...base,
    artifacts: [...(base.artifacts ?? []), artifact("artifact-verification", VERIFICATION_SLUG)],
    verification_artifact_id: "artifact-verification",
  };
  // Conversation's read never answers; the Verification view read is refused, and then answers
  // when the case says.
  const again = Promise.withResolvers<TreeAnswer>();
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
    const asked = query as { readonly view?: string };
    if (asked.view !== "verification") return Promise.withResolvers<never>().promise;
    viewReads += 1;
    return viewReads === 1 ? Promise.reject(REFUSED) : again.promise;
  });
  const view = drawStage("", { issue, preset: "verification" });
  try {
    const line = await waitFor(() => {
      const drawn = view.container.querySelector<HTMLElement>(
        '[data-verification-unreadable="blocks"]'
      );
      expect(drawn, "the view drew no line for its refused blocks").not.toBeNull();
      return drawn as HTMLElement;
    });
    const retry = within(line).getByRole("button", { name: "Retry" });
    await user.click(retry);
    await waitFor(() => expect(viewReads, "the Retry sent no Verification view read").toBe(2));
    expect(retry.getAttribute("aria-busy"), "the line is not busy while the view read runs").toBe(
      "true"
    );
    await act(async () => again.resolve(treeAnswer([])));
    await waitFor(() =>
      expect(view.container.querySelector('[data-verification-unreadable="blocks"]')).toBeNull()
    );
  } finally {
    view.unmount();
    for (const stub of stubs) stub.mockRestore();
  }
});

// TanStack Query pauses a run while the browser is offline, the read pending and not fetching, so
// a read refused before it first answered and asked again offline would read as neither in error
// nor running, and a rule that read `isFetching` or `isError` would take its line and its Retry
// off the page until the browser was back. It still reads as failed while its run waits, asks
// nothing while the browser is offline, and reads as answered once the browser is back and the
// read answers.
test("a refused read asked again while the browser is offline still reads as failed, and as answered once the browser is back and the read answers", async () => {
  const { coverageRead, hook, restore } = await refusedCoverage();
  try {
    const refused = hook.result.current.reads;
    act(() => onlineManager.setOnline(false));
    act(() => (refused.state === "failed" ? refused.failed.coverage?.retry() : undefined));
    await act(() => Bun.sleep(20));
    expect(
      hook.result.current.reads,
      "the coverage read asked again while the browser is offline does not still read as failed"
    ).toEqual({
      failed: { coverage: { failure: 1, retry: expect.any(Function), retrying: false } },
      state: "failed",
    });
    expect(
      coverageRead,
      "the coverage read was asked again while the browser is offline"
    ).toHaveBeenCalledTimes(1);
    act(() => onlineManager.setOnline(true));
    await waitFor(() =>
      expect(
        hook.result.current.reads,
        "the coverage read that answered once the browser was back does not read as answered"
      ).toEqual({ state: "answered" })
    );
  } finally {
    onlineManager.setOnline(true);
    restore();
  }
});

// An agent that recorded a result is named by its role's word, as the outline's rows name one
// (`agent-role.ts`), and its session's whole title is drawn nowhere in the view, its opened facts
// included; a person is named by their login, and a result nobody recorded by the page's own
// words for it.
test("an agent that recorded a result is named by its role's word", async () => {
  const user = userEvent.setup();
  const byAgent = check({
    block_id: "c-agent",
    results: [
      result({
        actor: {
          id: "session-9",
          kind: "session",
          origin: { session_title: "Legion tester · ACME-1" },
        },
        check_block_id: "c-agent",
        id: "by-agent",
      }),
    ],
  });
  const byPerson = check({
    block_id: "c-person",
    results: [
      result({ actor: { id: "alice", kind: "user" }, check_block_id: "c-person", id: "by-person" }),
    ],
  });
  const byNobody = check({
    block_id: "c-nobody",
    results: [result({ actor: undefined, check_block_id: "c-nobody", id: "by-nobody" })],
  });
  const view = drawBody(
    [
      statementNode("r1", "Three checks serve it.", [
        checkNode(byAgent),
        checkNode(byPerson),
        checkNode(byNobody),
      ]),
    ],
    [byAgent, byPerson, byNobody]
  );
  try {
    expect(
      Object.fromEntries(
        [...view.container.querySelectorAll("[data-verification-result]")].map((drawn) => [
          drawn.getAttribute("data-node-id"),
          drawn.querySelector("[data-node-author]")?.textContent,
        ])
      )
    ).toEqual({
      "by-agent": "tester",
      "by-nobody": CONTROL_LABELS.unknown_author,
      "by-person": "alice",
    });
    // With every check's facts open too, nothing of the title is drawn: not the issue it names,
    // nor the middle dot that set that issue apart from the role.
    for (const words of view.container.querySelectorAll("[data-verification-check-words]")) {
      await user.click(words);
    }
    expect(
      [...view.container.querySelectorAll("[data-verification-check]")].map((row) =>
        row.getAttribute("aria-expanded")
      )
    ).toEqual(["true", "true", "true"]);
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(6);
    const drawn = view.container.querySelector("[data-verification-body]")?.textContent ?? "";
    expect(drawn).not.toContain("ACME-1");
    expect(drawn).not.toContain("Legion");
    expect(textsOf(view, "[data-node-author]").filter((name) => name.includes("·"))).toEqual([]);
  } finally {
    view.unmount();
  }
});

// A check's words are drawn whole through `RowText` from the results read's reading of them
// (`text_tree`), so a link in them is a link and a reference is named by its target's name,
// never spelled as its address (R8, issue page decision 350). The words are no button, so no
// control holds a link, and a press on a link is the link's own: it neither opens nor shuts the
// check's facts.
test("a check's words draw their links from the results read's tree", async () => {
  const user = userEvent.setup();
  const summary = readsCore2();
  const linked = check({ block_id: "c-one", text: LINKED_WORDS });
  const view = drawBody(
    [statementNode("r1", "The page draws it.", [linkedCheckNode(linked)])],
    [linked]
  );
  try {
    const card = view.container.querySelector("[data-verification-check]");
    const words = view.container.querySelector("[data-verification-check-words]");
    const runbook = await waitFor(() => {
      const found = words?.querySelector<HTMLElement>('a[href="https://example.com/runbook"]');
      expect(found ?? null).not.toBeNull();
      expect(words?.querySelector('a[data-row-reference="dispatch://CORE-2"]')?.textContent).toBe(
        "CORE-2 Drain the queue"
      );
      return found as HTMLElement;
    });
    expect(view.container.textContent).not.toContain("dispatch://");
    expect(view.container.querySelectorAll("button a")).toHaveLength(0);

    // The page follows no link in this test.
    view.container.addEventListener("click", (event) => event.preventDefault());
    await user.click(runbook);
    act(() => runbook.blur());
    expect(card?.getAttribute("aria-expanded")).toBe("false");
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
  } finally {
    summary.mockRestore();
    view.unmount();
  }
});

// R8 and issue page decision 350 over a statement's parent line, which the view draws as the
// outline draws a parent line (`node-line.tsx`'s `NodeParentLine`): a reference in the parent's
// words reads as the words its author wrote while the page's batched read has not answered for
// it, and as its target's name once it has, and the line reads none itself.
test("a statement's parent line reads a reference as its target's name, and as its author's words until it resolves", async () => {
  const address = "dispatch://CORE-2/spec#b-r1";
  const words = `See ${address} first.`;
  const name = "the queue's drain";
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const served = passing("c-one");
  const statement = statementNode("r1", "The page draws it.", [checkNode(served)]);
  const parent = node({ children: [statement], id: "comment-1", text: words, type: "comment" });
  const client = testClient();
  const drawn = (published: PublishedReferences) =>
    bodyOf([parent], [served], { published }, client);
  const view = render(drawn(publishedNames(new Map(), { asked: [address] })));
  const line = () => view.container.querySelector("[data-node-parent-line]")?.textContent;
  try {
    expect(line(), "the parent line drew the reference before its name came").toBe(
      `${NODE_TYPE_NAMES.comment}${words}`
    );
    expect(
      getReferences,
      "the parent line read the reference the page asks for"
    ).not.toHaveBeenCalled();
    view.rerender(drawn(publishedNames(new Map([[address, name]]))));
    await waitFor(() =>
      expect(line(), "the parent line did not name the target").toBe(
        `${NODE_TYPE_NAMES.comment}See ${name} first.`
      )
    );
    expect(
      getReferences,
      "the parent line read the reference the page named"
    ).not.toHaveBeenCalled();
  } finally {
    getReferences.mockRestore();
    view.unmount();
  }
});

// The body hands its rows what the page publishes of its batched names read in the render that
// draws them (`published`), as the outline does, and the page publishes to the names store only
// after the rows have rendered (`Publisher`): so a reference the page asks for waits on the
// page's answer from a row's first render, drawn as its author wrote it meanwhile, and is never
// read by the row; a row that read the store alone read it itself first.
test("a row of the view waits on the page's read for a reference the page asks for", async () => {
  const address = "dispatch://CORE-2/spec#b-r1";
  const words = `See ${address} first.`;
  const name = "the queue's drain";
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const served = passing("c-one");
  const statement = statementNode("r1", words, [checkNode(served)]);
  const client = testClient();
  const drawn = (published: PublishedReferences) =>
    bodyOf([statement], [served], { published }, client);
  const view = render(drawn(publishedNames(new Map(), { asked: [address] })));
  const statementWords = () =>
    view.container.querySelector("[data-verification-card] [data-node-words]")?.textContent ?? "";
  try {
    await waitFor(() => expect(statementWords()).toContain(words));
    expect(getReferences, "a row read the reference the page asks for").not.toHaveBeenCalled();
    view.rerender(drawn(publishedNames(new Map([[address, name]]))));
    await waitFor(() => expect(statementWords()).toContain(`See ${name} first.`));
    expect(getReferences, "a row read the reference the page named").not.toHaveBeenCalled();
  } finally {
    getReferences.mockRestore();
    view.unmount();
  }
});

// A result's observed words are drawn from the results read's reading of them (`observed_tree`),
// as every stored text of the view is drawn from its tree where its read sent one, so the page
// parses none of them on its own thread.
test("a result's observed words are drawn from the results read's reading, with no parse of their own", () => {
  const observed = "41 passed, **2** skipped.";
  const block = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: "c-one", id: "result-c-one", observed })],
  });
  const built = checkNode(block);
  const read = (one: TreeNode): TreeNode => ({ ...one, tree: displayTree(one.text) });
  const checked = { ...read(built), children: built.children.map(read) };
  const statement = read(statementNode("r1", "The page draws it.", [checked]));
  const parses = countEngineParses();
  try {
    const view = drawBody([statement], [block]);
    try {
      expect(
        view.container.querySelector("[data-verification-result] [data-node-words]")?.textContent
      ).toBe("41 passed, 2 skipped.");
      expect(parses.count(), "the page parsed a stored text the read sent a reading of").toBe(0);
    } finally {
      view.unmount();
    }
  } finally {
    parses.restore();
  }
});

// A check that serves two statements is drawn under each and names the other as every chip of
// the page names a block: by the line the names store projects from the reading the page
// published for that statement, its first block that holds words, cut at a word within 32
// characters, with no mark the author typed and no markdown pass over the words, so words that
// open on "1." keep it, and an address inside them read as its short form, since a name inside a
// name resolves nothing. A statement that reads as no words, one whose words are a picture
// alone, is named by nothing, with no comma for it, as is one the view holds no statement for;
// and a check whose only other statement is such a one draws no "Also under" at all.
test("a check under two statements names the other by its words with no mark of markdown", async () => {
  const summary = readsCore2();
  const twice = passing("c-one");
  const thrice = passing("c-two");
  const pictured = passing("c-three");
  const counted = passing("c-four");
  // Each statement with the coverage read's reading of its words (`text_tree`). The numbered
  // one is stored as a store holds words that open on a number, so its reading keeps the "1."
  // as words; a markdown pass over those words reads them as a list and drops it.
  const read = (statement: TreeNode): TreeNode => ({
    ...statement,
    tree: displayTree(statement.text),
  });
  const plain = read(statementNode("r1", "The page draws it."));
  const marked = read(
    statementNode("r2", "**Do** `x` dispatch://CORE-2 now, then the rest of it, in order.")
  );
  const picture = read(statementNode("r3", "![](https://example.com/shot.png)"));
  const numbered = read(statementNode("r4", "1\\. Stand the page up first."));
  // A statement of no card of this view, which a check may still name.
  const stranger = read(statementNode("r9", "A statement this view does not draw."));
  // The reads the page names every statement from, and a check's row under one statement naming
  // the others it serves as the tree names them, from those reads: each chip is the page's own
  // for its target (`treeContext`), and the page publishes from the same reads.
  const all = [plain, marked, picture, numbered, stranger];
  const reads = blockReads(issueDetails(), all);
  const { chipOf } = treeContext(reads);
  const under = (block: CheckBlock, others: readonly TreeNode[]): TreeNode => ({
    ...checkNode(block),
    alsoUnder: others.map((other) => chipOf(other.reference ?? "")),
  });
  const view = drawBody(
    [
      {
        ...plain,
        children: [
          under(twice, [marked]),
          under(thrice, [picture, marked, stranger]),
          under(pictured, [picture]),
          under(counted, [numbered]),
        ],
      },
      { ...marked, children: [under(twice, [plain]), under(thrice, [plain, picture])] },
      { ...picture, children: [under(thrice, [plain, marked]), under(pictured, [plain])] },
      { ...numbered, children: [under(counted, [plain])] },
    ],
    [twice, thrice, pictured, counted],
    { published: publishedStatements(all, reads) }
  );
  try {
    // The trail of a check's row under a statement, and the words of each target it names.
    const trailOf = (statement: TreeNode, block: CheckBlock) => {
      const trails = view.container.querySelectorAll(
        `[data-verification-card][data-node-id="${statement.id}"] [data-verification-check][data-node-id="${checkNode(block).id}"] [data-node-trail]`
      );
      return [...trails].map((trail) => ({
        names: [...trail.querySelectorAll("[data-node-chip]")].map((chip) =>
          (chip.textContent ?? "").replace(/^, /u, "")
        ),
        text: trail.textContent,
      }));
    };
    const [first] = trailOf(plain, twice);
    const named = first?.names[0] ?? "";
    expect(first?.names).toHaveLength(1);
    expect(named, `"Also under" names r2 by no words: "${named}"`).not.toBe("");
    expect(named, `"Also under" draws a backtick: "${named}"`).not.toContain("`");
    expect(named, `"Also under" draws an asterisk: "${named}"`).not.toContain("*");
    // A bare address reads as its target's short form, never as `dispatch://`.
    expect(named, `"Also under" draws an address: "${named}"`).not.toContain("dispatch://");
    expect(
      named.length,
      `"Also under" names r2 by more than 32 characters: "${named}"`
    ).toBeLessThanOrEqual(32);
    expect(named).toBe("Do x CORE-2 now, then the rest…");
    expect(trailOf(plain, twice)).toEqual([
      { names: [named], text: `${CONTROL_LABELS.also_under} ${named}` },
    ]);
    expect(trailOf(marked, twice)).toEqual([
      { names: [plain.text], text: `${CONTROL_LABELS.also_under} ${plain.text}` },
    ]);
    // The picture and the statement the view does not draw name nothing, and no comma stands
    // for either.
    expect(trailOf(plain, thrice)).toEqual([
      { names: [named], text: `${CONTROL_LABELS.also_under} ${named}` },
    ]);
    expect(trailOf(marked, thrice)).toEqual([
      { names: [plain.text], text: `${CONTROL_LABELS.also_under} ${plain.text}` },
    ]);
    expect(trailOf(picture, thrice)).toEqual([
      {
        names: [plain.text, named],
        text: `${CONTROL_LABELS.also_under} ${plain.text}, ${named}`,
      },
    ]);
    // A check whose only other statement is the picture says it is under nothing else.
    expect(trailOf(plain, pictured)).toEqual([]);
    expect(trailOf(picture, pictured)).toEqual([
      { names: [plain.text], text: `${CONTROL_LABELS.also_under} ${plain.text}` },
    ]);
    // Words that open on a number keep it ("1. Stand").
    expect(trailOf(plain, counted)).toEqual([
      {
        names: ["1. Stand the page up first."],
        text: `${CONTROL_LABELS.also_under} 1. Stand the page up first.`,
      },
    ]);
    expect(trailOf(numbered, counted)).toEqual([
      { names: [plain.text], text: `${CONTROL_LABELS.also_under} ${plain.text}` },
    ]);
    // The statement's own words name CORE-2 by its read, as every reference in a row's words is
    // named (issue page decision 350), and spell no address.
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-verification-body]")?.textContent ?? ""
      ).not.toContain("dispatch://")
    );
  } finally {
    summary.mockRestore();
    view.unmount();
  }
});

/** Lays every element out `height` px tall until the answer is called, as
 *  `NodeRow.drawn.test.tsx` lays a row out: no DOM of a unit test lays anything out, and a
 *  statement's cut reads the words' own boxes (`CutWords`, through `NodeRow.tsx`'s
 *  `measureClamp`). */
function layOut(height: number): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get: () => height,
  });
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 0, 600, height)
  );
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
    box.mockRestore();
  };
}

// The keyboard reaches each control of the view by Tab: each chip, the "more" control of a cut
// statement, each check's words, each link in a check's words, each link in the words of a node
// an address named, the line of earlier results and, in the opened facts, the copy control of a
// command. This reads that each of them holds tab index 0, so none is out of the tab order; the
// order Tab takes them in is read in the browser, where row 4 of `e2e/issue-page-v.e2e.ts` walks
// it. The view passes `tabStops` to `CutWords`, `NodeFacts` and `RowText`, so none of them takes
// its controls out of the tab order. In document order the issue's row comes before the chip
// All, so the Tab after `t k` reaches the first chip. The page measures only the words of rows
// near the viewport, so the rows' words are brought near and measured before the cut is read.
test("each control of the view is a tab stop", async () => {
  const user = userEvent.setup();
  const summary = readsCore2();
  const linked = check({
    block_id: "c-one",
    results: [
      result({ created_at: "2026-11-13T09:00:00Z", id: "earlier" }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "newest" }),
    ],
    text: LINKED_WORDS,
  });
  const plain = passing("c-two");
  // A comment an address named, whose words hold a link: the view draws it as a row of its own,
  // and the tree's keys reach no link inside a row.
  const noted = "See [the notes](https://example.com/notes) first.";
  const named = node({ id: "comment-1", text: noted, tree: displayTree(noted), type: "comment" });
  // Sixty words over 400 lines of 24 px: six lines are drawn and 59 words are held back, more
  // than the twenty a cut must hide, so the statement is cut and draws its "more" control.
  const long = Array.from({ length: 60 }, (_unused, at) => `word${at + 1}`).join(" ");
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  try {
    const view = drawBody(
      [statementNode("r1", long, [linkedCheckNode(linked), checkNode(plain)]), named],
      [linked, plain],
      { named: [named.id] }
    );
    try {
      // The page cuts only the words of rows near the viewport.
      await measureNearWords(near);
      // In document order the issue's row, the tree's one tab stop, comes before the chip All.
      expect(
        [...view.container.querySelectorAll("[data-verification-issue], [data-verification-chip]")]
          .slice(0, 2)
          .map((drawn) =>
            drawn.hasAttribute("data-verification-issue")
              ? "issue"
              : drawn.getAttribute("data-verification-chip")
          )
      ).toEqual(["issue", "all"]);

      const chips = [...view.container.querySelectorAll<HTMLElement>("[data-verification-chip]")];
      expect(chips).toHaveLength(4);
      const more = view.container.querySelector<HTMLElement>(
        "[data-verification-card] [data-node-more]"
      );
      expect(more?.textContent?.startsWith(CONTROL_LABELS.read_more)).toBe(true);
      const words = [
        ...view.container.querySelectorAll<HTMLElement>("[data-verification-check-words]"),
      ];
      expect(words).toHaveLength(2);

      // The reader presses the first check's facts open, so they stay drawn wherever the focus
      // goes next.
      await user.click(words[0] as Element);
      act(() => words[0]?.blur());
      expect(
        [...view.container.querySelectorAll("[data-verification-check]")].map((row) =>
          row.getAttribute("aria-expanded")
        )
      ).toEqual(["true", "false"]);
      const links = await waitFor(() => {
        expect(
          words[0]?.querySelector('a[href="https://example.com/runbook"]') ?? null
        ).not.toBeNull();
        expect(words[0]?.querySelector('a[data-row-reference="dispatch://CORE-2"]')).not.toBeNull();
        return [
          ...view.container.querySelectorAll<HTMLElement>("[data-verification-check-words] a"),
        ];
      });
      expect(links).toHaveLength(2);
      const otherLinks = [
        ...view.container.querySelectorAll<HTMLElement>("[data-verification-other] a"),
      ];
      expect(otherLinks.map((link) => link.getAttribute("href"))).toEqual([
        "https://example.com/notes",
      ]);
      const copies = [
        ...view.container.querySelectorAll<HTMLElement>(
          `[aria-label="${CONTROL_LABELS.copy_command}"]`
        ),
      ];
      expect(copies.length).toBeGreaterThan(0);
      const earlier = [
        ...view.container.querySelectorAll<HTMLElement>("[data-verification-earlier]"),
      ];
      expect(earlier).toHaveLength(1);

      // Each control by its name, so a control out of the tab order is named where it fails.
      const controls: [string, HTMLElement | null][] = [
        ...chips.map((chip): [string, HTMLElement] => [
          `chip ${chip.getAttribute("data-verification-chip")}`,
          chip,
        ]),
        ["more", more],
        ...words.map((one, at): [string, HTMLElement] => [`words of check ${at + 1}`, one]),
        ...links.map((link): [string, HTMLElement] => [`link ${link.getAttribute("href")}`, link]),
        ...otherLinks.map((link): [string, HTMLElement] => [
          `link of the named row ${link.getAttribute("href")}`,
          link,
        ]),
        ...copies.map((copy, at): [string, HTMLElement] => [`copy control ${at + 1}`, copy]),
        ...earlier.map((line): [string, HTMLElement] => ["earlier results", line]),
      ];
      expect(controls.map(([name, control]) => [name, control?.tabIndex])).toEqual(
        controls.map(([name]) => [name, 0])
      );
    } finally {
      view.unmount();
    }
  } finally {
    near.restore();
    restore();
    summary.mockRestore();
  }
});

// The view's tree answers the outline's keys by a handler of its own. `j`, `k`, the arrows, Home
// and End move the focus from the row it is on, or from the row that holds the control it is on,
// and the tab stop moves with it; Enter on a row presses its checks' facts, `o` keeps them drawn,
// and a pointer opens them from a check's latest result, from its words and from its statement's
// words. The tree holds back each key it acts on and no other, and a key that goes on with a
// chord the reader began is the page's keymap's, so `t k` from inside the view reaches it.
test("the view's tree answers the outline's keys, and a check's facts open from its words, its result and its statement", async () => {
  const user = userEvent.setup();
  const one = passing("c-one");
  const two = passing("c-two");
  const loose = passing("c-loose");
  const served = statementNode("r1", "The page draws it.", [checkNode(one), checkNode(two)]);
  const unserved = statementNode("r2", "Nothing serves this one.", []);
  const view = drawBody([served, unserved, checkNode(loose)], [one, two, loose]);
  // Each key the view held back, in the order pressed.
  const { held, stop } = holdKeys();
  try {
    const rows = [...view.container.querySelectorAll<HTMLElement>('[role="treeitem"]')];
    // The rows in the order the page draws them: the issue's, each statement followed by its
    // checks, and the check that serves no statement.
    expect(rows.map((row) => row.getAttribute("data-node-id"))).toEqual([
      ISSUE_KEY,
      served.id,
      checkNode(one).id,
      checkNode(two).id,
      unserved.id,
      checkNode(loose).id,
    ]);
    const names = ["issue", "r1", "c-one", "c-two", "r2", "c-loose"];
    /** The row the focus is on, and each row that holds the tree's tab stop, by their names. */
    const standing = () => ({
      focus: names[rows.indexOf(document.activeElement as HTMLElement)],
      stop: rows.flatMap((row, place) =>
        row.getAttribute("tabindex") === "0" ? names[place] : []
      ),
    });
    /** Whether each check's facts are drawn, as its row says. */
    const opened = () =>
      [rows[2], rows[3], rows[5]].map((row) => row?.getAttribute("aria-expanded"));

    act(() => rows[0]?.focus());
    expect(standing()).toEqual({ focus: "issue", stop: ["issue"] });
    await user.keyboard("j");
    expect(standing()).toEqual({ focus: "r1", stop: ["r1"] });
    await user.keyboard("{ArrowDown}");
    expect(standing()).toEqual({ focus: "c-one", stop: ["c-one"] });
    await user.keyboard("k");
    expect(standing()).toEqual({ focus: "r1", stop: ["r1"] });
    await user.keyboard("{ArrowUp}");
    expect(standing()).toEqual({ focus: "issue", stop: ["issue"] });
    await user.keyboard("{End}");
    expect(standing()).toEqual({ focus: "c-loose", stop: ["c-loose"] });
    await user.keyboard("{Home}");
    expect(standing()).toEqual({ focus: "issue", stop: ["issue"] });
    // Right steps into a statement's checks and Left back out to the statement; on any other
    // row neither moves, and neither is held back there.
    await user.keyboard("{ArrowRight}");
    expect(standing()).toEqual({ focus: "issue", stop: ["issue"] });
    await user.keyboard("j{ArrowRight}");
    expect(standing()).toEqual({ focus: "c-one", stop: ["c-one"] });
    await user.keyboard("{ArrowLeft}");
    expect(standing()).toEqual({ focus: "r1", stop: ["r1"] });
    await user.keyboard("{End}{ArrowLeft}");
    expect(standing()).toEqual({ focus: "c-loose", stop: ["c-loose"] });
    await user.keyboard("k{ArrowRight}");
    expect(standing()).toEqual({ focus: "r2", stop: ["r2"] });
    // A key with Ctrl, Alt or Meta held is no key of the tree's.
    await user.keyboard("{Control>}j{/Control}{Alt>}k{/Alt}{Meta>}{Home}{/Meta}");
    expect(standing()).toEqual({ focus: "r2", stop: ["r2"] });
    // A control inside a row moves the reader as the row does: a chip is the issue's, and a
    // check's words are the check's.
    act(() => view.container.querySelector<HTMLElement>('[data-verification-chip="all"]')?.focus());
    await user.keyboard("j");
    expect(standing()).toEqual({ focus: "r1", stop: ["r1"] });
    act(() => rows[2]?.querySelector<HTMLElement>("[data-verification-check-words]")?.focus());
    await user.keyboard("j");
    expect(standing()).toEqual({ focus: "c-two", stop: ["c-two"] });
    expect(held).toEqual([
      "j",
      "ArrowDown",
      "k",
      "ArrowUp",
      "End",
      "Home",
      "j",
      "ArrowRight",
      "ArrowLeft",
      "End",
      "k",
      "j",
      "j",
    ]);
    held.length = 0;

    // On a check's row the focus drew its facts; Enter shuts them and a second Enter opens them,
    // which keeps them drawn once the focus moves on.
    await user.keyboard("k");
    expect(standing().focus).toBe("c-one");
    expect(opened()).toEqual(["true", "false", "false"]);
    await user.keyboard("{Enter}");
    expect(opened()).toEqual(["false", "false", "false"]);
    await user.keyboard("{Enter}");
    expect(opened()).toEqual(["true", "false", "false"]);
    await user.keyboard("j");
    expect(standing().focus).toBe("c-two");
    expect(opened()).toEqual(["true", "true", "false"]);
    // `o` keeps what the focus drew once the focus moves on, and a second `o` lets it go.
    await user.keyboard("oj");
    expect(standing().focus).toBe("r2");
    expect(opened()).toEqual(["true", "true", "false"]);
    await user.keyboard("ko");
    expect(opened()).toEqual(["true", "true", "false"]);
    await user.keyboard("j");
    expect(opened()).toEqual(["true", "false", "false"]);
    // Enter on a statement's row opens the facts of each of its checks, and where all of them
    // are drawn a second Enter shuts them.
    await user.keyboard("{Home}j");
    expect(standing().focus).toBe("r1");
    await user.keyboard("{Enter}");
    expect(opened()).toEqual(["true", "true", "false"]);
    await user.keyboard("{Enter}");
    expect(opened()).toEqual(["false", "false", "false"]);
    expect(view.container.querySelectorAll("[data-node-body]")).toHaveLength(0);
    // Enter and `o` on the issue's row press nothing, and are not held back.
    await user.keyboard("{Home}{Enter}o");
    expect(opened()).toEqual(["false", "false", "false"]);
    expect(held).toEqual([
      "k",
      "Enter",
      "Enter",
      "j",
      "o",
      "j",
      "k",
      "o",
      "j",
      "Home",
      "j",
      "Enter",
      "Enter",
      "Home",
    ]);

    // A pointer's press on the statement's words does what Enter on its row does.
    const words = rows[1]?.querySelector("[data-node-words]");
    expect(words?.textContent).toBe(served.text);
    await user.click(words as Element);
    expect(opened()).toEqual(["true", "true", "false"]);
    await user.click(words as Element);
    expect(opened()).toEqual(["false", "false", "false"]);
    // And a press on a check's latest result or on its words opens its facts, which stay drawn
    // once the focus has gone.
    await user.click(rows[2]?.querySelector("[data-verification-result]") as Element);
    await user.click(rows[3]?.querySelector("[data-verification-check-words]") as Element);
    await user.keyboard("{Home}");
    expect(standing().focus).toBe("issue");
    expect(opened()).toEqual(["true", "true", "false"]);

    // The page's keymap listens on `window`, as `KeymapProvider` binds it. While a chord the
    // reader began is pending, no key is the tree's: `j` moves nothing, and `k` goes on with
    // `t` to the keymap's `t k`.
    let turnedOff = 0;
    const unregister = appKeymap.register("global", [
      {
        id: "verification-off",
        keys: "t k",
        label: "Turn the view off",
        run: () => {
          turnedOff += 1;
        },
      },
    ]);
    const keymap = render(<KeymapProvider>{null}</KeymapProvider>);
    try {
      held.length = 0;
      await user.keyboard("t");
      expect(appKeymap.pending()).toBe("t");
      await user.keyboard("j");
      expect(standing()).toEqual({ focus: "issue", stop: ["issue"] });
      await user.keyboard("tk");
      expect(turnedOff).toBe(1);
      expect(appKeymap.pending()).toBe("");
      expect(standing()).toEqual({ focus: "issue", stop: ["issue"] });
      // The keymap's own hold comes after the document's listener; the tree held back none.
      expect(held).toEqual([]);
    } finally {
      keymap.unmount();
      unregister();
      // A chord a failed step left half typed is let go, so no later test types into it.
      appKeymap.handleKeyDown(new KeyboardEvent("keydown", { key: "Escape" }));
    }
  } finally {
    stop();
    view.unmount();
  }
});

// Each row of the tree carries its own id (`data-verification-row`), which the tree's keys and
// presses read from the row itself, so no id is paired with a row by where it stands among the
// others. The ids are the tree's rows in reading order, the issue's first and each check under a
// statement by that placement of it; and a key that moves onto a check that serves no statement,
// or onto a node an address named, gives the tab stop to that row's own element, where Enter
// presses that check and no other.
test("each row of the view's tree carries its own id, and a key gives the tab stop to the row it moves onto", async () => {
  const user = userEvent.setup();
  const one = passing("c-one");
  const loose = passing("c-loose");
  const served = statementNode("r1", "The page draws it.", [checkNode(one)]);
  const unserved = statementNode("r2", "Nothing serves this one.", []);
  const named = node({ id: "comment-1", text: "A comment an address named.", type: "comment" });
  const view = drawBody([served, unserved, checkNode(loose), named], [one, loose], {
    named: [named.id],
  });
  try {
    const rows = [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")];
    expect(rows.map((row) => row.getAttribute("data-verification-row"))).toEqual([
      ISSUE_KEY,
      served.id,
      `${served.id} ${checkNode(one).id}`,
      unserved.id,
      checkNode(loose).id,
      named.id,
    ]);
    expect(view.container.querySelectorAll('[role="treeitem"]')).toHaveLength(rows.length);
    // The loose check's own element, and the named node's.
    expect(rows[4]?.hasAttribute("data-verification-loose")).toBe(true);
    expect(rows[5]?.hasAttribute("data-verification-other")).toBe(true);
    const names = ["issue", "r1", "c-one", "r2", "c-loose", "comment"];
    /** The row the focus is on, and each row whose own element holds the tree's tab stop, by
     *  their names. */
    const standing = () => ({
      focus: names[rows.indexOf(document.activeElement as HTMLElement)],
      stops: rows.flatMap((row, at) => (row.getAttribute("tabindex") === "0" ? [names[at]] : [])),
    });

    act(() => rows[0]?.focus());
    await user.keyboard("jjj");
    expect(standing()).toEqual({ focus: "r2", stops: ["r2"] });
    await user.keyboard("j");
    expect(standing()).toEqual({ focus: "c-loose", stops: ["c-loose"] });
    await user.keyboard("j");
    expect(standing()).toEqual({ focus: "comment", stops: ["comment"] });
    await user.keyboard("k");
    expect(standing()).toEqual({ focus: "c-loose", stops: ["c-loose"] });
    // On the loose check's row the focus drew its facts; Enter shuts them, on that row alone.
    const opened = () =>
      [...view.container.querySelectorAll("[data-verification-check]")].map((row) =>
        row.getAttribute("aria-expanded")
      );
    expect(opened()).toEqual(["false", "true"]);
    await user.keyboard("{Enter}");
    expect(opened()).toEqual(["false", "false"]);
  } finally {
    view.unmount();
  }
});

// Under `?q=` with a word one earlier result holds, a check with 26 results standing once drew
// all 26 and its line of earlier results open, 15,190 px tall. A query that keeps one earlier
// result of a check keeps that result and no other: the check draws it, and the earlier results
// the query did not keep stay folded behind their line, so the view draws no result that the
// reader neither kept by the query nor opened.
test("a query that keeps one earlier result draws that result and leaves the others folded", () => {
  const kept = "The page drew the okapi column twice.";
  const many = check({
    block_id: "c-many",
    results: [
      result({
        check_block_id: "c-many",
        created_at: "2026-11-11T09:00:00Z",
        id: "result-kept",
        observed: kept,
      }),
      result({
        check_block_id: "c-many",
        created_at: "2026-11-12T09:00:00Z",
        id: "result-second",
        observed: "40 passed",
      }),
      result({
        check_block_id: "c-many",
        created_at: "2026-11-13T09:00:00Z",
        id: "result-third",
        observed: "41 passed",
      }),
      result({
        check_block_id: "c-many",
        created_at: "2026-11-14T09:00:00Z",
        id: "result-newest",
        observed: "42 passed",
      }),
    ],
  });
  const statement = statementNode("r1", "The page draws it.", [checkNode(many)]);
  const view = drawBody([statement], [many], {
    query: "okapi",
    whole: verificationView([statement]),
  });
  try {
    const drawn = textsOf(view, "[data-verification-result] [data-node-words]");
    expect(drawn, "the result the query kept is not drawn").toContain(kept);
    expect(
      drawn.filter((words) => words === "40 passed" || words === "41 passed"),
      "an earlier result the query did not keep is drawn"
    ).toEqual([]);
  } finally {
    view.unmount();
  }
});

/** The words of the earlier result of `MANY_RESULTS` that "okapi" is in, as it is in the
 *  check's own words. */
const OKAPI_RESULT = "The page drew the okapi column twice.";

/** A check of four results whose newest reads "42 passed", and whose own words hold "zebra",
 *  which none of its results' do, and "okapi", which its oldest result's do too. */
const MANY_RESULTS = check({
  block_id: "c-many",
  results: [
    result({
      check_block_id: "c-many",
      created_at: "2026-11-11T09:00:00Z",
      id: "result-kept",
      observed: OKAPI_RESULT,
    }),
    result({
      check_block_id: "c-many",
      created_at: "2026-11-12T09:00:00Z",
      id: "result-second",
      observed: "40 passed",
    }),
    result({
      check_block_id: "c-many",
      created_at: "2026-11-13T09:00:00Z",
      id: "result-third",
      observed: "41 passed",
    }),
    result({
      check_block_id: "c-many",
      created_at: "2026-11-14T09:00:00Z",
      id: "result-newest",
      observed: "42 passed",
    }),
  ],
  text: "The zebra page draws the okapi column.",
});

/** `MANY_RESULTS`'s row under its one statement, under `query`, as the reader reads it: its
 *  state, the words of each result it draws, in order, and its line of earlier results with
 *  whether that line is open. */
function manyResultsRow(query: string) {
  const statement = statementNode("r1", "The page draws it.", [checkNode(MANY_RESULTS)]);
  const drawn = drawBody([statement], [MANY_RESULTS], { query });
  try {
    return {
      line: textsOf(drawn, "[data-verification-earlier]"),
      open: drawn.container
        .querySelector("[data-verification-earlier]")
        ?.getAttribute("aria-expanded"),
      results: textsOf(drawn, "[data-verification-result] [data-node-words]"),
      state: textsOf(drawn, "[data-verification-check] span[data-node-state]"),
    };
  } finally {
    drawn.unmount();
  }
}

/** What `manyResultsRow` reads with no query: its newest result alone, and its line of three
 *  earlier results, shut. */
const MANY_RESULTS_ROW = {
  line: [`3 ${VERIFICATION_WORDS.earlier_many}`],
  open: "false",
  results: ["42 passed"],
  state: [NODE_STATE_WORDS.check.passed],
};

// A query that matches a check's own words keeps of its results only those whose words it
// matches too, and the check is still drawn as the issue's reading of it, as a check a query keeps
// only a result of is: its state, its newest result and its line of earlier results with their
// whole count, shut, and under the newest each earlier result the query kept. So matching more of
// a check never draws less of it: a query on the check's words and one earlier result's draws
// what a query on that result's words alone draws.
test("a query on a check's words and one earlier result's draws the check's newest result, the whole count of its earlier results, and that result", () => {
  const kept = { ...MANY_RESULTS_ROW, results: ["42 passed", OKAPI_RESULT] };
  expect(manyResultsRow("")).toEqual(MANY_RESULTS_ROW);
  // "twice" is in that result's words alone, and "okapi" in the check's words too.
  expect(manyResultsRow("twice")).toEqual(kept);
  expect(manyResultsRow("okapi"), "a query on the check's and a result's words drew less").toEqual(
    kept
  );
});

// A query on a check's own words alone keeps none of its results, and draws the check as no
// query draws it: its newest result, and its line of earlier results with their whole count, shut.
test("a query on a check's words alone draws the check's newest result and the whole count of its earlier results", () => {
  expect(manyResultsRow("zebra"), "a query on the check's words drew less").toEqual(
    MANY_RESULTS_ROW
  );
});

// A query of spaces alone is no query: `presetView` trims what the reader typed, and the view with
// no query is given to the body only where the query is not empty once trimmed. So the check is
// drawn as with no query, its newest result alone and its line of earlier results shut, and no
// earlier result of it counts as one a query kept.
test("a query of spaces alone draws the check's newest result alone and its line of earlier results shut, as no query does", () => {
  expect(manyResultsRow("   "), "a query of spaces alone drew more than no query").toEqual(
    MANY_RESULTS_ROW
  );
});

// A check that serves no statement is a row of the table of its own, and a query keeps it as it
// keeps a statement's check: a query that matches only an earlier result of it lifts that result
// to the top with the check as its parent line (`presets.ts`), and the body draws the check, read
// from the issue's reading, with that result under its newest and its line of earlier results
// shut, rather than the result as a bare row. A loose check the query matches by its own words is
// the issue's reading of it too, and the loose checks stand in the issue's order, each a row of
// the tree that a key moves onto and gives the tab stop to.
test("a query that keeps only a result of a check that serves no statement draws that check with the result", async () => {
  const user = userEvent.setup();
  const kept = "The page drew the okapi column twice.";
  const first = check({
    block_id: "c-first",
    results: [
      result({
        check_block_id: "c-first",
        created_at: "2026-11-12T09:00:00Z",
        id: "result-first-kept",
        observed: kept,
      }),
      result({
        check_block_id: "c-first",
        created_at: "2026-11-13T09:00:00Z",
        id: "result-first-middle",
        observed: "41 passed",
      }),
      result({
        check_block_id: "c-first",
        created_at: "2026-11-14T09:00:00Z",
        id: "result-first-newest",
        observed: "42 passed",
      }),
    ],
  });
  const second = check({
    block_id: "c-second",
    results: [result({ check_block_id: "c-second", id: "result-second" })],
    text: "The okapi page draws c-second.",
  });
  const view = drawBody([checkNode(first), checkNode(second)], [first, second], {
    query: "okapi",
  });
  try {
    /** Each check of the table's own: its row's id, the words of each result it draws, in
     *  order, and its line of earlier results with whether that line is open. */
    const looseOf = () =>
      [...view.container.querySelectorAll("[data-verification-loose]")].map((row) => {
        const line = row.querySelector("[data-verification-earlier]");
        return [
          row.getAttribute("data-verification-row"),
          [...row.querySelectorAll("[data-verification-result] [data-node-words]")].map(
            (words) => words.textContent
          ),
          (line?.textContent ?? "").replace(/\s+/gu, " ").trim(),
          line?.getAttribute("aria-expanded"),
        ];
      });
    expect(looseOf()).toEqual([
      [checkNode(first).id, ["42 passed", kept], `2 ${VERIFICATION_WORDS.earlier_many}`, "false"],
      [checkNode(second).id, ["41 passed"], "", undefined],
    ]);
    // The kept result is drawn with its check, and as no row of its own.
    expect(view.container.querySelectorAll("[data-verification-other]")).toHaveLength(0);
    // The tree's rows: the issue's, then each loose check, and a key gives the first its stop.
    const rows = [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")];
    expect(rows.map((row) => row.getAttribute("data-verification-row"))).toEqual([
      ISSUE_KEY,
      checkNode(first).id,
      checkNode(second).id,
    ]);
    act(() => rows[0]?.focus());
    await user.keyboard("j");
    expect(document.activeElement).toBe(rows[1]);
    expect(rows.map((row) => row.getAttribute("tabindex"))).toEqual(["-1", "0", "-1"]);
  } finally {
    view.unmount();
  }
});
