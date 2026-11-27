import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/statement-rules";
import { createComment, createIssue, createProject, editArtifact, getArtifact } from "./api";
import { PROJECT } from "./issue-layout-seed-support";
import { requirementBlockId, specDocument } from "./issue-layout-seed-text";
import { issuePage } from "./issue-page";
import { DESKTOP } from "./issue-page-g";
import { caretInRowBlock, onScreen, openPreset } from "./issue-page-i";
import { settledScrollTop } from "./mermaid-helpers";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * How the Spec view's rows read the new issue page's Spec view and its Contents slot
 * (`e2e/issue-page-j.e2e.ts`): the readers that `e2e/issue-page.ts` (the page's root and its
 * frame), `e2e/issue-page-g.ts` (one node's row, and the tree as rows) and `e2e/issue-page-i.ts`
 * (the outline) do not hold, and the seeds of the view's own.
 *
 * What the Spec view and the Contents slot draw, beyond what those files name:
 *
 * - **The issue's three sentences** `[data-testid="issue-three-things"]`, each an `li` whose
 *   label is `[data-page-chrome]` and whose sentence is `[data-author-text]`: in the Contents
 *   slot (the Contents pane and the Outline sheet), and as the Spec view's first group.
 * - **The two rows under them**, one `button[data-brief-row="plan" | "verification"]` each, named
 *   by its label alone, with what it counts in `[data-brief-counts]`: the Plan control's count and
 *   noun, or the number of checks and its noun `[data-brief-total]` and, for each state that
 *   holds one, `[data-brief-state="<state>"]` with its shape, count and word.
 * - **The Contents list**, one `[data-contents-entry="<node id>"]` per entry, its line
 *   `[data-contents-line]`, and above it the toggle named `CONTROL_LABELS.focus_branch`.
 * - **The Spec view** `[data-spec-body]`: its legend `[data-spec-legend]` of four
 *   `[data-spec-legend-mark]`, its column heads `[data-spec-head]`, its tree `[data-spec-tree]`,
 *   one `[data-spec-group]` per parent and one `[data-spec-run]` per run of statements, its
 *   decision cells `[data-spec-cell]` (`data-said`, `data-spec-statements`), its statements
 *   `[data-spec-statement]` (`data-spec-mark`), each with the count of the checks that serve it,
 *   the link `[data-spec-checks]`, where a check serves it, and under each statement the line
 *   `[data-spec-because]` naming its decision: the question it answers `[data-spec-question]`,
 *   as the cell draws it too, the quote's link `[data-spec-because-link]`, and
 *   `[data-spec-changed-since]` where the statement changed after its tie.
 */

// ---------------------------------------------------------------------------------------------
// The seed.
// ---------------------------------------------------------------------------------------------

/** One issue of `seedChildWithSpec`: its key, its spec's slug, and the block ids of the
 *  statements its spec holds, in document order. */
export interface IssueWithSpec {
  readonly key: string;
  readonly requirements: readonly string[];
  readonly specSlug: string;
}

export interface ChildWithSpecSeed {
  readonly child: IssueWithSpec;
  readonly parent: IssueWithSpec;
}

/** A statement of a seeded spec: one `requirement` block. */
export function statementBlock(id: string, words: string): string {
  return `:::requirement{#${id}}\n${words}\n:::`;
}

/** The slug of the spec `createIssue` made for an issue, by its artifact's id. */
async function specSlugOf(artifactId: string): Promise<string> {
  if (artifactId === "") throw new Error("the seeded issue holds no spec");
  return (await getArtifact(artifactId)).slug;
}

/**
 * A parent and its child, each with a spec of its own, in the project the issue-layout seeds
 * make: the parent's summary and its two statements, then the child's summary and its two
 * statements, each in words of its own. The child's Spec view draws its own two statements and
 * none of its parent's (R6; `useIssueTree.ts` reads the issue's own coverage).
 * `e2e/issue-layout-seed.ts`'s seeds give a parent no spec, so none of them can show that. An
 * issue created under an ancestor whose spec holds a statement may hold none of its own
 * (`REQUIREMENT_BELONGS_TO_ANCESTOR`), so the child is created while its parent's spec holds its
 * summary alone, and the parent's two statements are written after the summary once the child
 * stands.
 */
export async function seedChildWithSpec(): Promise<ChildWithSpecSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const parentRequirements = ["r-parent-1", "r-parent-2"] as const;
  const parentSummary = "summary";
  const parent = await createIssue({
    force: true,
    project: PROJECT,
    spec:
      `:::callout{#${parentSummary} title="What this issue is"}\n` +
      "Every part of the spring release ships together.\n:::\n",
    title: "The spring release ships as one",
  });
  const childRequirements = ["r-child-1", "r-child-2"] as const;
  const child = await createIssue({
    force: true,
    parent: parent.key,
    project: PROJECT,
    spec: `${[
      ':::callout{#summary title="What this issue is"}\n' +
        "The release notes ship with the release. They name every change a customer sees. " +
        "Each change links the pull request that made it.\n:::",
      statementBlock(childRequirements[0], "The notes list each change under the part it changed."),
      statementBlock(childRequirements[1], "The notes are published an hour before the release."),
    ].join("\n\n")}\n`,
    title: "The release notes ship with the spring release",
  });
  const parentStatements = [
    statementBlock(parentRequirements[0], "The release ships on one day for every customer."),
    statementBlock(parentRequirements[1], "The release can be rolled back within ten minutes."),
  ].join("\n\n");
  await editArtifact(parent.primary_artifact_id, {
    ops: [{ after: `block:${parentSummary}`, markdown: parentStatements, op: "insert" }],
  });
  return {
    child: {
      key: child.key,
      requirements: childRequirements,
      specSlug: await specSlugOf(child.primary_artifact_id),
    },
    parent: {
      key: parent.key,
      requirements: parentRequirements,
      specSlug: await specSlugOf(parent.primary_artifact_id),
    },
  };
}

/** The issue of `seedTallSpec`: its key, its spec's artifact id and slug, the block ids of the
 *  statements its spec holds, in document order, and the id of its one comment. */
export interface TallSpecSeed extends IssueWithSpec {
  readonly commentId: string;
  readonly specArtifactId: string;
}

/**
 * One issue whose spec holds sixty statements (`specDocument`), so that the Spec view's
 * statements alone stand well past one screen at 1440 by 900, and one comment on the issue
 * itself, which answers no other and is anchored to nothing, in the project the issue-layout
 * seeds make. Each statement's words are a sentence or two (`requirementText`), on a row at least
 * 30 px tall: one line of words, 24 px, with 3 px above and below it (`SpecFilterBody.tsx`'s
 * `Statement`). So sixty rows stand at least 1,800 px tall, twice the screen's height, below the
 * view's legend and column heads. A row that lands on a row of the view below the first screen
 * reads it: the file's other seeds draw too few statements to put a row below the screen.
 */
export async function seedTallSpec(): Promise<TallSpecSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const count = 60;
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: count, summary: false }),
    title: "The Spec view draws a spec longer than one screen",
  });
  const comment = await createComment(issue.key, {
    body: "Does every requirement of the redesign need a row of its own?",
  });
  return {
    commentId: comment.id,
    key: issue.key,
    requirements: Array.from({ length: count }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: issue.primary_artifact_id,
    specSlug: await specSlugOf(issue.primary_artifact_id),
  };
}

/**
 * `seedTallSpec`'s issue with one block more in its spec, after its third statement: the markdown
 * `block` writes once the issue stands, which is a statement and anything it names, such as the
 * text of a footnote it references. A seed for `phase`.
 */
export function seedStatement(
  block: (seeded: TallSpecSeed) => Promise<string> | string
): () => Promise<TallSpecSeed> {
  return async () => {
    const seeded = await seedTallSpec();
    await editArtifact(seeded.specArtifactId, {
      ops: [
        { after: `block:${requirementBlockId(3)}`, markdown: await block(seeded), op: "insert" },
      ],
    });
    return seeded;
  };
}

// ---------------------------------------------------------------------------------------------
// The readers.
// ---------------------------------------------------------------------------------------------

/** The issue's three sentences under "In short", wherever they are drawn. */
export function threeThings(page: Page): Locator {
  return page.getByTestId("issue-three-things");
}

/** One of the two rows under the sentences in `scope`: the button that turns the Plan filter on
 *  or the one that turns the Verification filter on. */
export function briefRow(scope: Locator | Page, filter: "plan" | "verification"): Locator {
  return scope.locator(`[data-brief-row="${filter}"]`);
}

/** What the second row draws under its label: the number of checks with its noun, then each
 *  state that holds a check, in the order they are drawn. */
export interface ChecksDrawn {
  readonly states: readonly {
    readonly count: string;
    readonly shape: string | null;
    readonly state: string | null;
    readonly word: string;
  }[];
  readonly total: string;
}

/** The checks `row`, the second row under the sentences, counts. */
export function checksDrawn(row: Locator): Promise<ChecksDrawn> {
  return row.evaluate((element) => ({
    states: [...element.querySelectorAll("[data-brief-state]")].map((part) => ({
      count: part.querySelector("[data-author-text]")?.textContent?.trim() ?? "",
      shape: part.querySelector("svg")?.getAttribute("data-glyph") ?? null,
      state: part.getAttribute("data-brief-state"),
      word: part.querySelector("[data-page-chrome]")?.textContent?.trim() ?? "",
    })),
    total: (element.querySelector("[data-brief-total]")?.textContent ?? "").trim(),
  }));
}

/** The author named inside `scope`, where the page names one. */
export function rowAuthor(scope: Locator): Locator {
  return scope.locator("[data-node-author]");
}

/** The rows of the Contents list, wherever it is drawn: the pane, or the Outline sheet. */
export function drawnContentsEntries(scope: Locator): Locator {
  return scope.locator("[data-contents-entry]");
}

/** The Focus branch toggle above the Contents list in `scope`. */
export function focusBranchControl(scope: Locator): Locator {
  return scope.getByRole("button", { exact: true, name: CONTROL_LABELS.focus_branch });
}

/**
 * What is wrong with the line a Contents entry draws of `text`, read from the browser's own
 * layout in the face the page draws in now: the line must fit the box the entry leaves it, and
 * must hold the next word of `text` too wherever that word and the ellipsis fit there (R1's
 * first-line rule, cut at a word to the entry's width). Empty where the line is the right one.
 */
export function contentsLineFaults(entry: Locator, text: string): Promise<string[]> {
  return entry.evaluate((element, text) => {
    const line = element.querySelector<HTMLElement>("[data-contents-line]");
    if (line === null) return ["the entry draws no line"];
    const shown = line.textContent ?? "";
    const words = shown.endsWith("…") ? shown.slice(0, -1) : shown;
    if (!text.startsWith(words)) {
      return [`the entry draws "${shown}", which "${text}" does not begin with`];
    }
    const room = line.getBoundingClientRect().width;
    const drawn = document.createRange();
    drawn.selectNodeContents(line);
    const width = drawn.getBoundingClientRect().width;
    const faults: string[] = [];
    if (width > room + 1) {
      faults.push(`"${shown}" is ${width.toFixed(1)} px, past its ${room.toFixed(1)} px box`);
    }
    const next = text.slice(words.length).trim().split(" ")[0] ?? "";
    if (next === "") return faults;
    // The line with its next word, laid out beside it in the same face and size.
    const probe = line.cloneNode(false) as HTMLElement;
    probe.textContent = `${words} ${next}…`;
    probe.style.cssText =
      "position: absolute; visibility: hidden; flex: none; width: auto; white-space: nowrap";
    line.after(probe);
    const longer = probe.getBoundingClientRect().width;
    probe.remove();
    if (longer <= room - 2) {
      faults.push(
        `"${shown}" leaves out "${next}", though "${words} ${next}…" is ${longer.toFixed(1)} px, inside its ${room.toFixed(1)} px box`
      );
    }
    return faults;
  }, text);
}

/** One Decided by cell of the Spec view: whose words it draws, the statements its bracket
 *  spans, and its drawn text. */
export interface DecidedByCell {
  readonly ids: readonly string[];
  readonly said: string | null;
  readonly text: string;
}

/** Each Decided by cell of the Spec view, in the order they are drawn. */
export function decidedByCells(page: Page): Promise<DecidedByCell[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll("[data-spec-cell]")].map((cell) => ({
      ids: (cell.getAttribute("data-spec-statements") ?? "").split(" ").filter(Boolean),
      said: cell.getAttribute("data-said"),
      text: (cell.textContent ?? "").replace(/\s+/g, " ").trim(),
    }))
  );
}

/** The one line under each statement of the Spec view that names the decision that shaped it,
 *  by statement id; `""` for a statement that draws none. */
export function becauseLines(page: Page): Promise<Record<string, string>> {
  return page.evaluate(() =>
    Object.fromEntries(
      [...document.querySelectorAll("[data-spec-statement]")].map((row) => [
        row.getAttribute("data-node-id") ?? "",
        (row.querySelector("[data-spec-because]")?.textContent ?? "").replace(/\s+/g, " ").trim(),
      ])
    )
  );
}

/** Where a box of the page is, in the viewport's pixels. */
export interface SpecBox {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
  readonly width: number;
}

/** One run of the Spec view, `[data-spec-run]`: the box of its decision cell and of its first
 *  statement, each null where the run draws none. */
export interface SpecRun {
  readonly cell: SpecBox | null;
  readonly first: SpecBox | null;
}

/** Each run of the Spec view, in the order they are drawn. */
export function specRuns(page: Page): Promise<SpecRun[]> {
  return page.locator("[data-issue-page] [data-spec-run]").evaluateAll((runs) =>
    runs.map((run) => {
      const box = (element: Element | null) => {
        if (element === null) return null;
        const { bottom, left, right, top, width } = element.getBoundingClientRect();
        return { bottom, left, right, top, width };
      };
      return {
        cell: box(run.querySelector("[data-spec-cell]")),
        first: box(run.querySelector("[data-spec-statement]")),
      };
    })
  );
}

/** The text of each `[data-page-chrome]` element inside `scope`, which is the page's own words,
 *  with what a nested `[data-author-text]` holds left out: a person's or an agent's words inside
 *  the page's own, such as the count in "more · 180 words", are theirs (R7). */
export function ownWords(scope: Locator): Promise<string[]> {
  return scope.locator("[data-page-chrome]").evaluateAll((elements) =>
    elements.map((element) => {
      const own = element.cloneNode(true) as Element;
      for (const theirs of own.querySelectorAll("[data-author-text]")) theirs.replaceWith(" ");
      return own.textContent ?? "";
    })
  );
}

/** Fails unless `target`, the element an address landed on, comes to stand on screen as
 *  `onScreen` reads it: clear of what stays at the top of the screen and above the box that stays
 *  at its foot. Each try that finds it off screen reads where it stands: its top and bottom, the
 *  viewport's height, how tall the page is and how far it is scrolled. A failure names `what`
 *  landed there and those figures as the last try read them, which together say where an element
 *  that left the screen went. */
export async function expectOnScreen(target: Locator, what: string): Promise<void> {
  await expect
    .poll(
      async () => {
        if (await onScreen(target)) return "on screen";
        if ((await target.count()) === 0) return `${what} draws no focused element`;
        const at = await target.first().evaluate((element) => {
          const box = element.getBoundingClientRect();
          return {
            bottom: box.bottom,
            height: window.innerHeight,
            scrollHeight: document.documentElement.scrollHeight,
            scrollY: window.scrollY,
            top: box.top,
          };
        });
        return `${what}: the focused element stands from ${at.top} px to ${at.bottom} px in a viewport ${at.height} px tall, on a page ${at.scrollHeight} px tall scrolled ${at.scrollY} px, not on screen`;
      },
      { message: `${what} leaves the focused element off screen` }
    )
    .toBe("on screen");
}

/** A point on the drawn word `word` of `statement`'s words, in the viewport: 1 px into its second
 *  character, half way down its line; null where the words draw no such word. */
function drawnWordPoint(
  statement: Locator,
  word: string
): Promise<{ x: number; y: number } | null> {
  return statement.evaluate((row, wanted) => {
    const words = row.querySelector("[data-node-words]");
    if (words === null) return null;
    const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
    for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
      const found = ` ${text.textContent ?? ""} `.indexOf(` ${wanted} `);
      if (found < 0) continue;
      const range = document.createRange();
      range.setStart(text, found + 1);
      range.setEnd(text, found + 2);
      const box = range.getBoundingClientRect();
      return { x: box.left + 1, y: (box.top + box.bottom) / 2 };
    }
    return null;
  }, word);
}

/** The word the caret stands in, or the one a point of the page would put it in: the run of
 *  characters that are no space around `offset` in the text of `node`. Read in the page. */
export function wordAtCaret(
  page: Page,
  at: { x: number; y: number } | "selection"
): Promise<string> {
  return page.evaluate((point) => {
    let node: Node | null = null;
    let offset = 0;
    if (point === "selection") {
      const selection = window.getSelection();
      node = selection?.anchorNode ?? null;
      offset = selection?.anchorOffset ?? 0;
    } else {
      const position = document.caretPositionFromPoint(point.x, point.y);
      node = position?.offsetNode ?? null;
      offset = position?.offset ?? 0;
    }
    const text = node?.nodeType === Node.TEXT_NODE ? (node.textContent ?? "") : "";
    let start = offset;
    let end = offset;
    while (start > 0 && !/\s/.test(text.charAt(start - 1))) start -= 1;
    while (end < text.length && !/\s/.test(text.charAt(end))) end += 1;
    return text.slice(start, end);
  }, at);
}

// ---------------------------------------------------------------------------------------------
// Moves.
// ---------------------------------------------------------------------------------------------

/** Opens the Spec view of `seeded` at 1440 by 900 and waits until its first load has settled;
 *  the row of its statement `blockId`, whether the view draws it or not. */
export async function openSpecStatement(
  page: Page,
  seeded: IssueWithSpec,
  blockId: string
): Promise<Locator> {
  await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
  const body = issuePage(page).locator("[data-spec-body]");
  const traffic = watchApiRequests(page);
  await openPreset(page, seeded.key, "spec");
  await expectLoadSettled(traffic);
  return body.locator(
    `[data-spec-statement][data-node-id="${blockNodeId(seeded.specSlug, blockId)}"]`
  );
}

/** A turn of the wheel by `deltaY` px with the mouse at the screen's middle, which returns once
 *  the page has moved and stopped moving. */
async function turnWheel(page: Page, deltaY: number): Promise<void> {
  const wheelAt = await settledScrollTop(page);
  await page.mouse.move(DESKTOP.width / 2, DESKTOP.height / 2);
  await page.mouse.wheel(0, deltaY);
  await expect
    .poll(() => settledScrollTop(page), { message: "the wheel does not scroll the page" })
    .not.toBe(wheelAt);
}

/**
 * A turn of the wheel, the reader's own act, by `statement`'s top and 200 px more, which stands
 * that top 200 px above the screen; its failure calls `statement` the long statement, which is
 * the one a row seeds for this. Returns the bottom of the Spec view's column heads, read before
 * the wheel, and the statement's box, read once the page has stopped moving.
 */
export async function wheelPastTop(
  page: Page,
  statement: Locator
): Promise<{ box: Omit<SpecBox, "right">; headsBottom: number }> {
  const heads = issuePage(page).locator("[data-spec-body]").locator("[data-spec-head]");
  const headsBottom = await heads.evaluate((head) => head.getBoundingClientRect().bottom);
  const top = await statement.evaluate((row) => row.getBoundingClientRect().top);
  await turnWheel(page, top + 200);
  const box = await statement.evaluate((row) => {
    const { bottom, left, top: rowTop, width } = row.getBoundingClientRect();
    return { bottom, left, top: rowTop, width };
  });
  expect(box.top, "after the wheel the long statement's top is not above the screen").toBeLessThan(
    0
  );
  return { box, headsBottom };
}

/**
 * A turn of the wheel, the reader's own act, brings the word `pressed` of `statement`'s words to
 * the screen's middle; once the page has stopped moving, a press on that word puts the caret of
 * the statement's editor, block `blockId`, in it. A row names what the word stands after in its
 * two failures: `messages.noCaret` where the press puts no caret in the statement's words, and
 * `messages.otherWord` where the caret stands in another word.
 */
export async function pressWordAfterWheel(
  page: Page,
  statement: Locator,
  blockId: string,
  pressed: string,
  messages: { readonly noCaret: string; readonly otherWord: string }
): Promise<void> {
  const far = await drawnWordPoint(statement, pressed);
  expect(far, `the statement draws no word "${pressed}"`).not.toBeNull();
  await turnWheel(page, (far?.y ?? 0) - DESKTOP.height / 2);
  // `turnWheel` waited until the page stopped moving after the wheel, so one read of the word's
  // box is where it stands when it is pressed.
  const at = await drawnWordPoint(statement, pressed);
  expect(at, `the word "${pressed}" is not on the page after the wheel`).not.toBeNull();
  const point = at ?? { x: 0, y: 0 };
  expect(
    await wordAtCaret(page, point),
    `the point (${point.x}, ${point.y}) is not on the word "${pressed}"`
  ).toBe(pressed);
  await page.mouse.click(point.x, point.y);
  await expect.poll(() => caretInRowBlock(page, blockId), { message: messages.noCaret }).toBe(true);
  expect(await wordAtCaret(page, "selection"), messages.otherWord).toBe(pressed);
}
