import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
} from "@playwright/test";

import type { Actor, ArtifactUploadResponse } from "../web/src/api/types";
import {
  BLOCK_TYPE_NAMES,
  CONTROL_LABELS,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  type NodeType,
} from "../web/src/features/issue-page/page-words";
import { buildDispatchReference } from "../web/src/features/refs/routes";
import {
  answerAsk,
  baseUrl,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editArtifact,
  getIssue,
  patchIssue,
  recordCheckResult,
  userHeaders,
} from "./api";
import { blockId } from "./editor";
import { withIssuePage } from "./issue-page-option";
import type { Box } from "./layout";
import { ONE_PIXEL_PNG } from "./sends";
import { asUser } from "./users";

/**
 * What the rows of part G (`e2e/issue-page-g.e2e.ts`) read of the new
 * issue page, and the one issue they seed. A later part imports these readers and does not copy
 * them (decision 2 of the parts).
 *
 * The page is the one the reader turns on (`withIssuePage`), opened at the issue's own address
 * with `page.goto`; the body is `slots/body.tsx`'s: G's rows drew every branch unfolded, and
 * part I's outline later, which folds to the frontier: so a row that reads a node unfolds every
 * folded branch first (`unfoldEveryBranch`), and holds on either body.
 *
 * The DOM a row reads: each node is a `role="treeitem"` `[data-node-row]` inside the
 * `role="tree"` named "Issue tree", carrying `data-node-id`, `data-node-type`, `data-node-state`
 * and `aria-level`; inside it the bullet `[data-node-bullet]` (the fold control where the node
 * holds nodes), the words `[data-node-words]` with their trail of chips `[data-node-chip]`, the
 * group `[data-node-margin]` (tag `[data-node-tag]`, author `[data-node-author]`, time
 * `[data-node-time]`, state `[data-node-state]`), the body `[data-node-body]` of the focused or
 * pinned row, and the focused row's acts `[data-row-act]` and `[data-row-copy]`.
 *
 * A reader answers through one `page.evaluate` and returns what is there - an empty list for a
 * page that draws none of it - and a row asserts on that answer.
 */

/** The two widths every row of this part reads. */
export const DESKTOP = { height: 900, width: 1440 } as const;
export const PHONE = { height: 844, width: 390 } as const;

/** The reader every row signs in as. */
export const READER = "alice";

/** The selectors the readers pass into the browser. */
const SELECTORS = {
  answerAuthor: "[data-answer-author]",
  answerLine: "[data-node-answer]",
  answerText: "[data-answer-text]",
  gap: "[data-gap]",
  issuePage: "[data-issue-page]",
  nodeAuthor: "[data-node-author]",
  nodeBody: "[data-node-body]",
  nodeChip: "[data-node-chip]",
  nodeFirstLine: "[data-node-first-line]",
  nodeGlyph: "svg[data-glyph]",
  nodeMargin: "[data-node-margin]",
  nodeRow: "[data-node-row]",
  nodeState: "[data-node-state]",
  nodeTag: "[data-node-tag]",
  nodeUnsure: "[data-node-unsure]",
  nodeWords: "[data-node-words]",
  parentLine: "[data-node-parent-line]",
} as const;

// ---------------------------------------------------------------------------------------------
// Signing in and opening the page.
// ---------------------------------------------------------------------------------------------

/**
 * Runs `body` on a page signed in as the reader, with the new issue page turned on, after `seed`
 * has written the issue it reads. The suite's `test` empties the database before each row
 * (`e2e/test.ts`), so a row seeds once; the viewport starts at the desktop width in every
 * project, and a row that reads the phone sets it itself.
 */
export async function phase<T>(
  browser: Browser,
  seed: () => Promise<T>,
  body: (page: Page, seeded: T) => Promise<void>
): Promise<void> {
  const seeded = await seed();
  const context: BrowserContext = await asUser(browser, READER);
  try {
    await withIssuePage(context, READER);
    const page = await context.newPage();
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await body(page, seeded);
  } finally {
    await context.close();
  }
}

/** Opens issue `key` at its own address and waits for the new page's tree. */
export async function openIssue(page: Page, key: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await expectIssuePageDrawn(page);
}

/** Fails unless the new page is on screen with its tree, so every reading after it reads the
 *  new page and not today's. */
export async function expectIssuePageDrawn(page: Page): Promise<void> {
  await expect(issuePage(page), "the page draws no [data-issue-page] root").toHaveCount(1);
  await expect(tree(page), 'the page draws no role="tree" named "Issue tree"').toHaveCount(1);
}

// ---------------------------------------------------------------------------------------------
// The page's parts, as locators.
// ---------------------------------------------------------------------------------------------

export function issuePage(page: Page): Locator {
  return page.locator(SELECTORS.issuePage);
}

export function tree(page: Page): Locator {
  return issuePage(page).getByRole("tree", { name: CONTROL_LABELS.tree });
}

export function nodeRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.nodeRow);
}

export function nodeRow(page: Page, id: string): Locator {
  return nodeRows(page).filter({ has: page.locator(`:scope[data-node-id="${cssValue(id)}"]`) });
}

export function rowBody(row: Locator): Locator {
  return row.locator(SELECTORS.nodeBody);
}

/** The node's words as one row draws them whole (R1, R2). */
export function rowWords(row: Locator): Locator {
  return row.locator(SELECTORS.nodeWords).first();
}

/** The control that opens the rest of the words a row cut at six lines, and closes them. */
export function rowMore(row: Locator): Locator {
  return row.locator("[data-node-more]");
}

/** One of the focused row's acts under its words: Reply, Answer or Change answer (R7). */
export function rowAct(row: Locator, act: "answer" | "change-answer" | "reply"): Locator {
  return row.locator(`[data-row-act="${act}"]`);
}

/** Puts the focus on node `id`'s row, as a press or a key puts it there, and waits until the row
 *  holds it and draws its body, which every row draws while it has the focus: a row draws its
 *  card whether or not it has the focus, so the card says nothing of it. */
export async function focusRow(page: Page, id: string): Promise<Locator> {
  const row = nodeRow(page, id).first();
  await row.focus();
  await expect(row).toBeFocused();
  await expect(rowBody(row)).toHaveCount(1);
  return row;
}

/** Leaves whatever control holds the focus. `body.focus()` does not do it: `body` takes no
 *  focus and the control keeps it. */
export async function focusThePage(page: Page): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect.poll(() => page.evaluate(() => document.activeElement === document.body)).toBe(true);
}

/** A `"` inside an attribute value would end the selector's string, and no id the page draws
 *  holds one; a value that did would otherwise match nothing silently. */
function cssValue(value: string): string {
  expect(value, `${value} cannot be written into a selector`).not.toContain('"');
  return value;
}

/** How many presses `unfoldEveryBranch` makes before it gives up: well past the branches the
 *  seed holds, so a tree that draws the control whatever is pressed fails here rather than at
 *  the row's deadline. */
const BRANCHES_OPENED_AT_MOST = 200;

/**
 * Opens every branch a fold left shut, as a reader does to read past one: these rows were drawn
 * with every branch open before the outline, which folds to the frontier and draws a
 * branch deeper than the levels it draws - one below 1024 px, three from there up - as one
 * `[data-branch-more]` line counting its nodes. So a row that reads what a branch holds presses
 * each fold control and each such line's control, until neither is left, rather than asserting
 * that the first drawing carried it. It ends with the focus on no row: a press may leave it on
 * the row it opened, and a focused row draws its body, while the rows read the tree as it stands
 * with no row focused.
 */
export async function unfoldEveryBranch(page: Page): Promise<void> {
  const controls = nodeRows(page)
    .getByRole("button", { exact: true, name: CONTROL_LABELS.unfold })
    .or(issuePage(page).locator("[data-branch-more] button"));
  for (let pressed = 0; pressed < BRANCHES_OPENED_AT_MOST; pressed += 1) {
    const first = controls.first();
    // A caller waits for the tree it opens first - a settled load, or an expect.poll that reads
    // the rows after it, which opens a shut branch drawn after this count on its next pass.
    if ((await first.count()) === 0) {
      await focusThePage(page);
      return;
    }
    await first.click();
  }
  expect(
    await controls.count(),
    `the tree still draws a folded branch after ${BRANCHES_OPENED_AT_MOST} openings`
  ).toBe(0);
}

/** How long `expectSeedDrawn` waits for the seed's rows and the names their chips read. */
const SEED_DRAWN_MS = 30_000;

/**
 * Waits until the page draws a row for every node the seed wrote that a row reads, and the names
 * of what their chips cite. The tree is drawn with the issue alone; its comments, messages,
 * questions, decisions, results and blocks arrive after it, and the batched names last
 * (`useIssueTree.ts`), so a reading taken as soon as the tree exists may find a row, or a chip's
 * name, not drawn yet. The needs chip of `t2` draws nothing until the names have come
 * (`NodeRow.tsx`'s `Chip`), so it stands for them. Each poll first opens every branch a fold
 * left shut (`unfoldEveryBranch`), since a node that arrives late may arrive folded.
 */
export async function expectSeedDrawn(page: Page, seeded: RowSeed): Promise<void> {
  const ids = [
    seeded.child,
    seeded.blocker,
    seeded.comment,
    seeded.linked,
    seeded.pictured,
    seeded.longMessage,
    seeded.longComment,
    seeded.answeredLong,
    seeded.waitingLong,
    seeded.openQuestion,
    seeded.answered,
    seeded.shaping,
    seeded.changed,
    seeded.change,
    seeded.unsure,
    seeded.raw,
    seeded.result,
    ...seeded.replies,
    blockNode(seeded.slugs.spec, "r-one"),
    blockNode(seeded.slugs.spec, "r-cites"),
    blockNode(seeded.slugs.spec, "r-ties"),
    blockNode(seeded.slugs.plan, "t1"),
    blockNode(seeded.slugs.plan, "t2"),
    blockNode(seeded.slugs.verification, "c1"),
  ];
  const needs = blockNode(seeded.slugs.plan, "t2");
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        const rows = await drawnRows(page);
        return {
          missing: ids.filter((id) => rowOf(rows, id) === undefined),
          named: (rowOf(rows, needs)?.chips.length ?? 0) > 0,
        };
      },
      {
        message: "the page never drew every seeded row and the names its chips read",
        timeout: SEED_DRAWN_MS,
      }
    )
    .toEqual({ missing: [], named: true });
}

// ---------------------------------------------------------------------------------------------
// The readers: one `page.evaluate` each.
// ---------------------------------------------------------------------------------------------

/** How wide one box lays its text out against how wide it draws it. */
export interface LineBox {
  readonly client: number;
  readonly nowrap: boolean;
  readonly scroll: number;
}

/** One row of the tree, as the page draws it. */
export interface DrawnRow {
  /** The answerer an answered ask's own row names, or null where the row draws no answer. */
  readonly answerAuthor: string | null;
  /** The text of the answer line an answered ask draws on its own row, or null where none. */
  readonly answerLine: string | null;
  /** The whole of what the person wrote, every word and every paragraph kept, in order: each
   *  paragraph the renderer draws, joined by the blank line the store holds between them, or
   *  the text as drawn where the answer draws no paragraph; null where none is drawn. */
  readonly answerText: string | null;
  /** The author the row's group names, or null where it names none. An open body names an
   *  author of its own, "Author unknown" included, which this leaves out. */
  readonly author: string | null;
  /** The `data-glyph` token of the shape drawn beside the group's author, or null where none
   *  is. */
  readonly authorShape: string | null;
  /** Whether the row draws a body of its own. */
  readonly body: boolean;
  /** Where the row is, or null while it is not rendered. */
  readonly box: Box | null;
  /** The text of each chip on the row, in order. */
  readonly chips: readonly string[];
  readonly context: boolean;
  /** `aria-expanded`, or null for a row that holds no children. */
  readonly expanded: string | null;
  /** The one line a folded row draws, or null on a row drawn whole, whose words are `words`. */
  readonly firstLine: string | null;
  /** How wide that one line lays its text out against how wide it draws it, or null on a row
   *  drawn whole. */
  readonly firstLineBox: LineBox | null;
  /** `data-folded-count`, or null for a row that is not folded. */
  readonly foldedCount: string | null;
  /** The `data-gap-kind` of each gap the row draws, in order. */
  readonly gapKinds: readonly string[];
  /** The `data-glyph` token of each shape drawn on the row, with its accessible name. */
  readonly glyphs: readonly { readonly name: string; readonly token: string }[];
  readonly id: string | null;
  /** `aria-level`, as drawn. */
  readonly level: string | null;
  /** The one line a filter's row draws for its parent, or null where it draws none. */
  readonly parent: { readonly id: string; readonly line: string; readonly type: string } | null;
  /** Whether the row's body is pinned open. */
  readonly pinned: boolean;
  readonly role: string | null;
  /** `data-node-state`, the model's own token, or null for a type it gives no state. */
  readonly state: string | null;
  /** The state word the row draws, or null where it draws none. */
  readonly stateWord: string | null;
  readonly tabIndex: number;
  /** The whole visible text of the row, its body included. */
  readonly text: string;
  /** `data-node-type`, the model's own token for the kind of node. */
  readonly type: string | null;
  /** The one word a decision the model marks unsure says of itself, or null where none. */
  readonly unsure: string | null;
  readonly visible: boolean;
  /** The node's words as the row draws them whole, or null where it draws its one line. */
  readonly words: string | null;
  /** How wide the box of a folded row's one line lays it out against how wide it draws it, or
   *  null on a row drawn whole. */
  readonly wordsBox: LineBox | null;
}

/** Every row the page draws, in document order. */
export function drawnRows(page: Page): Promise<DrawnRow[]> {
  return page.evaluate((selectors) => {
    const text = (element: Element | null): string | null =>
      element === null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) return null;
      return { height: rect.height, width: rect.width, x: rect.x, y: rect.y };
    };
    // The renderer draws each paragraph of a stored text as a <p> of its own, so the blank line
    // between two of them is a block boundary and no character of the page: the answer's words
    // are its paragraphs' text, joined as the store joins them. An answer drawn with no <p> - as
    // stored, where the read sent no reading - is its text as drawn.
    const answerWords = (element: Element | null): string | null => {
      if (element === null) return null;
      const paragraphs = [...element.querySelectorAll("p")];
      return paragraphs.length === 0
        ? element.textContent
        : paragraphs.map((paragraph) => paragraph.textContent ?? "").join("\n\n");
    };
    const lineBox = (element: Element | null | undefined) =>
      element === null || element === undefined
        ? null
        : {
            client: element.clientWidth,
            nowrap: window.getComputedStyle(element).whiteSpace === "nowrap",
            scroll: element.scrollWidth,
          };
    const rows = [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)];
    return rows.map((row) => {
      const firstLine = row.querySelector(selectors.nodeFirstLine);
      const answerLine = row.querySelector(selectors.answerLine);
      const parentLine = row.querySelector(`[data-node-content] > ${selectors.parentLine}`);
      return {
        answerAuthor: text(answerLine?.querySelector(selectors.answerAuthor) ?? null),
        answerLine: text(answerLine),
        answerText: answerWords(row.querySelector(selectors.answerText)),
        author: text(row.querySelector(`${selectors.nodeMargin} ${selectors.nodeAuthor}`)),
        authorShape:
          row
            .querySelector(`${selectors.nodeMargin} ${selectors.nodeAuthor} ${selectors.nodeGlyph}`)
            ?.getAttribute("data-glyph") ?? null,
        body: row.querySelector(selectors.nodeBody) !== null,
        box: box(row),
        chips: [...row.querySelectorAll(selectors.nodeChip)].map((chip) => text(chip) ?? ""),
        context: row.hasAttribute("data-node-context"),
        expanded: row.getAttribute("aria-expanded"),
        firstLine: text(firstLine),
        firstLineBox: lineBox(firstLine),
        foldedCount: row.getAttribute("data-folded-count"),
        gapKinds: [...row.querySelectorAll(selectors.gap)].map(
          (gap) => gap.getAttribute("data-gap-kind") ?? ""
        ),
        glyphs: [...row.querySelectorAll(selectors.nodeGlyph)].map((glyph) => ({
          name: glyph.getAttribute("aria-label") ?? text(glyph.querySelector("title")) ?? "",
          token: glyph.getAttribute("data-glyph") ?? "",
        })),
        id: row.getAttribute("data-node-id"),
        level: row.getAttribute("aria-level"),
        parent:
          parentLine === null
            ? null
            : {
                id: parentLine.getAttribute("data-parent-id") ?? "",
                line: text(parentLine.querySelector("[data-author-text]")) ?? "",
                type: parentLine.getAttribute("data-parent-type") ?? "",
              },
        pinned: row.querySelector(`${selectors.nodeBody}[data-pinned]`) !== null,
        role: row.getAttribute("role"),
        state: row.getAttribute("data-node-state"),
        stateWord: text(row.querySelector(selectors.nodeState)),
        tabIndex: (row as HTMLElement).tabIndex,
        text: text(row) ?? "",
        type: row.getAttribute("data-node-type"),
        unsure: text(row.querySelector(selectors.nodeUnsure)),
        visible: row.getClientRects().length > 0,
        words: text(row.querySelector(selectors.nodeWords)),
        wordsBox: lineBox(firstLine?.parentElement),
      };
    });
  }, SELECTORS);
}

/** The drawn row of node `id`, or undefined where the page draws none. */
export function rowOf(rows: readonly DrawnRow[], id: string): DrawnRow | undefined {
  return rows.find((row) => row.id === id);
}

/** The row `id` sits under, by the tree's own `aria-level`: the nearest row before it one level
 *  up. `null` for a row at the top level, and `undefined` for a row the page does not draw. */
export async function parentOfRow(page: Page, id: string): Promise<string | null | undefined> {
  const rows = await drawnRows(page);
  const index = rows.findIndex((row) => row.id === id);
  if (index === -1) return undefined;
  const level = Number(rows[index]?.level ?? "");
  if (!Number.isFinite(level)) return null;
  for (let before = index - 1; before >= 0; before -= 1) {
    const candidate = rows[before];
    if (candidate !== undefined && Number(candidate.level ?? "") === level - 1) {
      return candidate.id;
    }
  }
  return null;
}

/** Where the parts of one row stand on the page, in CSS pixels (R1): its bullet, the left edge
 *  of its words and their first line, its tag, its byline and its time, and their colours. */
export interface RowLayout {
  readonly author: { readonly color: string; readonly name: string } | null;
  readonly byline: { readonly left: number; readonly top: number } | null;
  /** The time's computed colour. */
  readonly bylineColor: string | null;
  /** The left of the bullet's dot, its computed fill, and the fill its kind's token resolves to.
   *  The dot, not its box: on a phone a fold control's box reaches past the bullet's column on
   *  both sides (`NODE_ROW_LAYOUT.foldControl`), and its dot stands where every bullet's does.
   *  A requirement's kind is a statement's in the spec alone, and a block's anywhere else. */
  readonly bullet: { readonly fill: string; readonly kindFill: string; readonly left: number };
  /** The row's own box: its height, which a one-line row holds to 29 px at 1440 px. */
  readonly height: number;
  readonly context: boolean;
  /** The fold control a row that holds nodes draws: its name and whether it is the bullet. */
  readonly fold: { readonly isBullet: boolean; readonly name: string } | null;
  readonly id: string;
  /** The words' first line: its top, its bottom and the right end of its last character. */
  readonly line: { readonly bottom: number; readonly right: number; readonly top: number };
  /** The text of the control that opens a cut text, or null where the words are whole. */
  readonly more: string | null;
  /** Whether these are the reader's own words, which carry the ink rule. */
  readonly own: boolean;
  /** The page's quiet grey, as computed, for comparison with the byline. */
  readonly quiet: string;
  /** Whether the ink rule is drawn at the content's leading edge. */
  readonly rule: boolean;
  /** Whether the row is a statement: a `requirement` block of the spec. A requirement of any
   *  other document is a block and nothing more (decision 246). */
  readonly statement: boolean;
  /** The kind's tag: its box, its words, and the shape it draws, by its glyph token. */
  readonly tag: {
    readonly glyph: string | null;
    readonly left: number;
    readonly right: number;
    readonly text: string;
    readonly top: number;
  } | null;
  readonly time: string | null;
  readonly type: string;
  /** Whether the first thing the row's content draws is the node's words. */
  readonly wordsFirst: boolean;
  /** How many pixels of the words the row's box hides. */
  readonly wordsHidden: number;
  /** The left edge of the node's words, or of a folded row's one line. */
  readonly wordsLeft: number;
}

/** Every visible row's layout, in document order. `spec` is the slug of the issue's spec, the one
 *  document whose requirements are statements. */
export function rowLayouts(page: Page, spec: string): Promise<RowLayout[]> {
  const given = { selectors: SELECTORS, spec };
  return page.evaluate(({ selectors, spec }) => {
    const computed = (property: "backgroundColor" | "color", token: string): string => {
      const probe = document.createElement("i");
      probe.style[property] = `var(--color-${token})`;
      document.body.append(probe);
      const value = getComputedStyle(probe)[property];
      probe.remove();
      return value;
    };
    const quiet = computed("color", "page-quiet");
    const KIND: Record<string, string> = {
      ask: "page-question",
      callout: "page-issue",
      check: "page-check",
      child_issue: "page-child",
      decision: "page-decision",
      issue: "page-issue",
      plan_task: "page-step",
      requirement: "page-statement",
      result: "page-check",
    };
    const rows = [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)];
    return rows
      .filter((row) => row.getClientRects().length > 0)
      .map((row) => {
        const type = row.getAttribute("data-node-type") ?? "";
        const id = row.getAttribute("data-node-id") ?? "";
        const statement = type === "requirement" && id.startsWith(`${spec}#`);
        const author = row.querySelector(selectors.nodeAuthor);
        // A requirement outside the spec is drawn as a block, in a block's colour: a callout's
        // (decision 246).
        const kindToken =
          type === "requirement" && !statement
            ? "page-issue"
            : (KIND[type] ??
              (author?.getAttribute("data-author-kind") === "person"
                ? "page-person"
                : "page-bullet"));
        const content = row.querySelector("[data-node-content]");
        const words =
          row.querySelector(selectors.nodeWords) ??
          row.querySelector(selectors.nodeFirstLine)?.parentElement ??
          null;
        const bullet = row.querySelector("[data-node-bullet]");
        const dot = bullet?.querySelector("i") ?? null;
        // The words' first line: the boxes of their text alone, the first line being those
        // whose top is the first box's.
        const rects: DOMRect[] = [];
        if (words !== null) {
          const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
          for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
            const range = document.createRange();
            range.selectNodeContents(node);
            rects.push(...[...range.getClientRects()].filter((rect) => rect.width > 0));
          }
        }
        const top = rects[0]?.top ?? 0;
        const first = rects.filter((rect) => Math.abs(rect.top - top) < 4);
        const tag = row.querySelector(selectors.nodeTag);
        const tagBox = tag?.getBoundingClientRect();
        const bylineBox = row.querySelector("[data-node-byline]")?.getBoundingClientRect();
        const time = row.querySelector("[data-node-time]");
        const name = author?.querySelector("[data-author-text], [data-page-chrome]") ?? null;
        const fold = bullet?.matches("button") === true ? bullet : null;
        const card = row.querySelector("[data-node-card]") ?? row;
        return {
          author:
            name === null
              ? null
              : { color: getComputedStyle(name).color, name: (name.textContent ?? "").trim() },
          bullet: {
            fill: dot === null ? "" : getComputedStyle(dot).backgroundColor,
            kindFill: computed("backgroundColor", kindToken),
            left: (dot ?? bullet)?.getBoundingClientRect().left ?? -1,
          },
          byline: bylineBox === undefined ? null : { left: bylineBox.left, top: bylineBox.top },
          bylineColor: time === null ? null : getComputedStyle(time).color,
          context: row.hasAttribute("data-node-context"),
          fold:
            row.getAttribute("aria-expanded") === null
              ? null
              : {
                  isBullet: fold !== null,
                  name:
                    (fold ?? row.querySelector("button[aria-label]"))?.getAttribute("aria-label") ??
                    "",
                },
          height: card.getBoundingClientRect().height,
          id,
          line: {
            bottom: Math.max(...first.map((rect) => rect.bottom), top),
            right: Math.max(...first.map((rect) => rect.right), 0),
            top,
          },
          more:
            row.querySelector("[data-node-more]")?.textContent?.replace(/\s+/g, " ").trim() ?? null,
          own: content?.hasAttribute("data-node-own") === true,
          quiet,
          rule: content !== null && getComputedStyle(content).boxShadow.includes("inset"),
          statement,
          tag:
            tag === null || tagBox === undefined
              ? null
              : {
                  glyph: tag.querySelector("svg[data-glyph]")?.getAttribute("data-glyph") ?? null,
                  left: tagBox.left,
                  right: tagBox.right,
                  text: (tag.textContent ?? "").trim(),
                  top: tagBox.top,
                },
          time: time?.textContent ?? null,
          type,
          wordsFirst: content?.firstElementChild === words,
          wordsHidden: words === null ? 0 : Math.max(0, words.scrollHeight - words.clientHeight),
          wordsLeft: words?.getBoundingClientRect().left ?? -1,
        };
      });
  }, given);
}

/** The kinds of node that draw no tag: what somebody said. */
const UNTYPED_KINDS: readonly string[] = ["comment", "message"];

/**
 * R1 on every row one view draws, read off the page's own layout: the words of every node start
 * at one left edge from the node's own bullet, and the reader's own words at a second, inside it,
 * behind their ink rule; nothing stands between a bullet and its words; a node that holds others
 * has its bullet as its fold control; the bullet is its kind's colour; a typed kind draws one tag
 * naming it - a child issue's is its key - with its author and its time after it, in grey, and a
 * turn draws none. A requirement is a statement in the spec alone, with the statement's colour,
 * word and shape; one of any other document is a block, in a block's colour, whose tag says
 * "requirement" and draws no shape (decision 246). At 761 px and above the tag stands at the
 * right end of the words' first line.
 */
export function expectRowLayouts(
  layouts: readonly RowLayout[],
  where: string,
  ownDrawn: boolean,
  width: number
): void {
  expect(layouts.length, `${where} draws no row`).toBeGreaterThan(0);
  const edge = (row: RowLayout) => Math.round(row.wordsLeft - row.bullet.left);
  const others = [...new Set(layouts.filter((row) => !row.own).map(edge))];
  const own = [...new Set(layouts.filter((row) => row.own).map(edge))];
  expect(
    others,
    `${where}: the words of the nodes but the reader's start at more than one edge from their bullet`
  ).toHaveLength(1);
  expect(own.length, `${where}: the reader's own words start at more than one edge`).toBeLessThan(
    2
  );
  if (ownDrawn) {
    expect(own, `${where} draws none of the reader's own words`).toHaveLength(1);
    expect(
      own[0] ?? 0,
      `${where}: the reader's own words do not stand at their own edge, inside every other node's`
    ).toBeGreaterThan(others[0] ?? 0);
  }
  for (const row of layouts) {
    const at = `the ${row.type} ${row.id} on ${where}`;
    expect(row.wordsFirst, `${at} draws something between its bullet and its words`).toBe(true);
    expect(row.rule, `${at} draws the ink rule on words that are not the reader's`).toBe(row.own);
    expect(row.bullet.left, `${at} draws its bullet right of its words`).toBeLessThan(
      row.wordsLeft
    );
    if (row.fold !== null) {
      expect(row.fold.isBullet, `${at} holds nodes and its bullet is not its fold control`).toBe(
        true
      );
      expect([CONTROL_LABELS.fold, CONTROL_LABELS.unfold], `${at}'s fold control`).toContain(
        row.fold.name
      );
    }
    if (!row.context) {
      expect(row.bullet.fill, `${at}'s bullet is not its kind's colour`).toBe(row.bullet.kindFill);
    }
    if (UNTYPED_KINDS.includes(row.type)) {
      expect(row.tag, `${at} draws a tag, which a turn of the conversation has none of`).toBeNull();
    } else {
      expect(row.tag?.text, `${at} does not name its kind on one tag`).toBe(
        row.type === "child_issue"
          ? row.id
          : row.type === "requirement" && !row.statement
            ? BLOCK_TYPE_NAMES.requirement
            : NODE_TYPE_NAMES[row.type as NodeType]
      );
      if (row.type === "requirement") {
        expect(
          row.tag?.glyph ?? null,
          row.statement
            ? `${at} draws no statement's shape on its tag`
            : `${at} draws a statement's shape on its tag, and it is no statement`
        ).toBe(row.statement ? GLYPH_TOKENS.types.requirement : null);
      }
      if (row.line.right > 0 && width > 760) {
        expect(
          row.tag?.left ?? -1,
          `${at} draws its tag before the end of its words' first line`
        ).toBeGreaterThanOrEqual(row.line.right - 1);
        expect(
          row.tag?.top ?? -1,
          `${at} draws its tag above its first line`
        ).toBeGreaterThanOrEqual(row.line.top - 8);
        expect(row.tag?.top ?? 1e6, `${at} draws its tag under its first line`).toBeLessThan(
          row.line.bottom
        );
      }
      if (row.byline !== null) {
        const after =
          row.byline.left >= (row.tag?.right ?? 0) - 1 || row.byline.top > (row.tag?.top ?? 0) + 1;
        expect(after, `${at} draws its author and its time before its tag`).toBe(true);
      }
    }
    if (row.author !== null && row.author.name !== READER) {
      expect(row.author.color, `${at} draws its author other than in grey`).toBe(row.quiet);
    }
    if (row.time !== null) {
      expect(row.bylineColor, `${at} draws its time other than in grey`).toBe(row.quiet);
    }
  }
}

/** What rulings 4 and 5 read of one row (R1, R4, R5): the line of the kind's tag, the author and
 *  the time against the row's words, the state under it, the chips, the links in its words and
 *  in a child's facts, and its fold control's box. */
export interface RowGroup {
  readonly author: string | null;
  /** Each chip: the words it names its target by, all it draws, and whether it stands inside
   *  the box of the node's words. */
  readonly chips: readonly {
    readonly inWords: boolean;
    readonly name: string;
    readonly text: string;
  }[];
  /** The bottom of what the row draws under its bullet's line. */
  readonly contentBottom: number;
  /** Whether the row's words are cut, which draws the chips beside the control that opens the
   *  rest. */
  readonly cut: boolean;
  readonly fold: { readonly height: number; readonly width: number } | null;
  /** The page's grey for a bullet and its ink, as computed. */
  readonly grey: string;
  readonly ink: string;
  /** The group's first line: how many lines of its own line height it takes, and its top. */
  readonly group: { readonly lines: number; readonly top: number } | null;
  readonly id: string;
  /** Each link in the row's words and in a child's facts: its colour, the colour of the words it
   *  stands in, its underline's, and whether it is a child's blocker (`[data-child-facts]`). */
  readonly links: readonly {
    readonly color: string;
    readonly facts: boolean;
    readonly underline: string;
    readonly words: string;
  }[];
  readonly open: boolean;
  readonly stateTop: number | null;
  readonly type: string;
}

/** Every visible row's group, chips and links, in document order. */
export function rowGroups(page: Page): Promise<RowGroup[]> {
  return nodeRows(page).evaluateAll((rows) => {
    const text = (element: Element | null | undefined): string | null =>
      element == null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
    const computed = (token: string): string => {
      const probe = document.createElement("i");
      probe.style.color = `var(--color-${token})`;
      document.body.append(probe);
      const color = getComputedStyle(probe).color;
      probe.remove();
      return color;
    };
    const grey = computed("page-bullet");
    const ink = computed("page-ink");
    return rows
      .filter((row) => row.getClientRects().length > 0)
      .map((row) => {
        const words = row.querySelector("[data-node-words]");
        const margin = row.querySelector("[data-node-margin]");
        const line = margin?.firstElementChild ?? null;
        const lineBox = line?.getBoundingClientRect();
        const pitch = line === null ? 0 : Number.parseFloat(getComputedStyle(line).lineHeight);
        const fold = row.querySelector("button[data-node-bullet]")?.getBoundingClientRect();
        return {
          author: text(margin?.querySelector("[data-node-author]")),
          chips: [...row.querySelectorAll("[data-node-chip]")].map((chip) => ({
            inWords: words?.contains(chip) === true,
            name: text(chip.querySelector("[data-chip-name]")) ?? "",
            text: text(chip) ?? "",
          })),
          contentBottom:
            row.querySelector("[data-node-content]")?.getBoundingClientRect().bottom ?? 0,
          cut: row.querySelector("[data-node-more]") !== null,
          fold: fold === undefined ? null : { height: fold.height, width: fold.width },
          grey,
          group:
            lineBox === undefined || lineBox.height === 0
              ? null
              : {
                  lines: Math.round(lineBox.height / (Number.isFinite(pitch) ? pitch : 24)),
                  top: lineBox.top,
                },
          id: row.getAttribute("data-node-id") ?? "",
          ink,
          links: [...(words?.querySelectorAll("a[href]") ?? [])]
            // The node's own words and a child's facts, not its chips - at the end of the words
            // or in place (`data-row-chip`) - which are links of the muted grey.
            .filter(
              (link) =>
                link.closest("[data-node-trail]") === null && !link.hasAttribute("data-row-chip")
            )
            .map((link) => ({
              color: getComputedStyle(link).color,
              facts: link.closest("[data-child-facts]") !== null,
              underline: getComputedStyle(link).textDecorationColor,
              words: link.parentElement === null ? "" : getComputedStyle(link.parentElement).color,
            })),
          open: row.querySelector("[data-node-body]") !== null,
          stateTop: margin?.querySelector("[data-node-state]")?.getBoundingClientRect().top ?? null,
          type: row.getAttribute("data-node-type") ?? "",
        };
      });
  });
}

/**
 * Rulings 4 and 5 on every row one view draws. At 761 px and above a row that is not open draws
 * its kind, its author and its time on one line, and its state on a second line under it; at
 * 760 px and below that line stands under the row's words. No row draws the page's words for an
 * author nobody recorded. Each tie is a chip inside the box of the node's words, named by at most
 * 32 characters and by no address, slug or short form of one (`addresses`). A link in a node's
 * words is the words' own colour, a child's blocker the ink, each underlined in the bullets'
 * grey. On a phone a fold control is 24 px square at least (WCAG 2.5.8).
 */
export function expectRowGroups(
  groups: readonly RowGroup[],
  where: string,
  width: number,
  addresses: RegExp
): void {
  for (const row of groups) {
    const at = `the ${row.type} ${row.id} on ${where}`;
    expect(
      row.author,
      `${at} draws the page's words for an author nobody recorded on its row`
    ).not.toBe(CONTROL_LABELS.unknown_author);
    if (row.group !== null && width > 760) {
      if (!row.open) {
        expect(
          row.group.lines,
          `${at} draws its kind, its author and its time on more than one line`
        ).toBe(1);
      }
      if (row.stateTop !== null) {
        expect(
          row.stateTop,
          `${at} draws its state on the line of its kind, its author and its time`
        ).toBeGreaterThan(row.group.top + 8);
      }
    }
    if (row.group !== null && width <= 760) {
      expect(
        row.group.lines,
        `${at} draws its kind, its author and its time on more than one line`
      ).toBe(1);
      expect(
        row.group.top,
        `${at} draws its kind, its author and its time beside its words and not under them`
      ).toBeGreaterThanOrEqual(row.contentBottom - 1);
    }
    for (const chip of row.chips) {
      expect(
        chip.name.length,
        `${at} names a target by more than 32 characters: "${chip.name}"`
      ).toBeLessThanOrEqual(32);
      expect(
        addresses.test(chip.text),
        `${at} draws an address or a slug on a chip: "${chip.text}"`
      ).toBe(false);
      if (!row.cut) {
        expect(chip.inWords, `${at} draws the chip "${chip.text}" on a line under its words`).toBe(
          true
        );
      }
    }
    for (const link of row.links) {
      expect(
        link.color,
        link.facts
          ? `${at} draws a key that blocks it in another colour than the ink`
          : `${at} draws a link in its words in another colour than the words'`
      ).toBe(link.facts ? row.ink : link.words);
      expect(
        link.underline,
        `${at} underlines a link in its words in another colour than the bullets' grey`
      ).toBe(row.grey);
    }
    if (width <= 760 && row.fold !== null) {
      expect(
        Math.min(row.fold.width, row.fold.height),
        `${at}'s fold control is smaller than 24 px at ${width} px`
      ).toBeGreaterThanOrEqual(24);
    }
  }
}

// ---------------------------------------------------------------------------------------------
// The seed: one issue that holds every node the part's rows read.
// ---------------------------------------------------------------------------------------------

/** The project the seed makes. */
const PROJECT = "ROW";

/** The title the planner's session stamps on what it writes, which the registry does not list,
 *  so the page names it by this title alone. */
export const PLANNER_TITLE = "Planner (issue layout)";
export const plannerActor: Actor = {
  id: "issue-page-g-planner",
  kind: "session",
  origin: { session_title: PLANNER_TITLE },
};
const asPlanner = { actor: plannerActor, as: "agent" as const };

/** The session that records the seeded result: an Envoy session id is a UUID, which the page
 *  must never draw in an author's place, and it stamps no title at all. */
export const recorderActor: Actor = {
  id: "019a1c0a-6d4b-7e8a-bf39-4a1b5c8d9e07",
  kind: "session",
};

/** `count` words in paragraphs of forty, each word told apart by its place in the text. */
export function longWords(lead: string, count: number): string {
  const words = Array.from({ length: count }, (_unused, at) => `${lead}${at + 1}`);
  const paragraphs: string[] = [];
  for (let at = 0; at < words.length; at += 40) {
    paragraphs.push(`${words.slice(at, at + 40).join(" ")}.`);
  }
  return paragraphs.join("\n\n");
}

/** The node id of block `block` of the document `slug` (`tree-drafts.ts`'s `blockNodeId`). */
export function blockNode(slug: string, block: string): string {
  return `${slug}#b-${block}`;
}

/** The words of the decision a person asks to change, which the change's anchor quotes. */
const CHANGED_DECISION_WORDS = "One read names every node the rows draw.";

/** The words of the one-line statement `r-one`: 32 characters, so a chip that names the block
 *  by its first words draws them whole. */
export const ONE_LINE_WORDS = "The page draws one row per node.";

/** What the seed wrote, by the ids the page draws its rows under. */
export interface RowSeed {
  readonly key: string;
  /** The slug of each document the issue holds: its spec, its notes, its plan, its
   *  Verification document and its decisions. */
  readonly slugs: {
    readonly decisions: string;
    readonly notes: string;
    readonly plan: string;
    readonly spec: string;
    readonly verification: string;
  };
  /** The child with a blocker and a session's claim, and the child that blocks it. */
  readonly child: string;
  readonly blocker: string;
  readonly childTitle: string;
  /** The reader's own short comment, and the agent's message with a link and "UTF-16". */
  readonly comment: string;
  readonly linked: string;
  /** The reader's two replies "Read.", one to each of the planner's two messages. */
  readonly replies: readonly string[];
  /** The picture's comment, and the picture's own slug. */
  readonly pictured: string;
  readonly picture: string;
  /** The 160-word texts c-cut reads. */
  readonly longMessage: string;
  readonly longComment: string;
  readonly answeredLong: string;
  readonly waitingLong: string;
  readonly answerWords: string;
  /** The open question with two options that waits on the reader, and an answered one. */
  readonly openQuestion: string;
  readonly answered: string;
  /** The decision that shaped a statement and waits for review, the decision a person asked to
   *  change, and the comment that asks it. */
  readonly shaping: string;
  readonly changed: string;
  readonly change: string;
  /** The decision the seed marks unsure, whose words cite `t1` by its address, and the one that
   *  quotes a typed block and cites `r-one` by its address. */
  readonly unsure: string;
  readonly raw: string;
  /** The result the untitled session recorded against `c1`. */
  readonly result: string;
}

/** Uploads a picture to the issue as the reader, as the page's own upload does. */
async function uploadPicture(key: string, name: string): Promise<string> {
  const form = new FormData();
  form.append("name", name);
  form.append(
    "file",
    new Blob([Buffer.from(ONE_PIXEL_PNG, "base64")], { type: "image/png" }),
    name
  );
  const response = await fetch(
    new URL(`/api/v1/issues/${encodeURIComponent(key)}/artifacts`, baseUrl),
    { body: form, headers: await userHeaders(READER), method: "POST" }
  );
  expect(response.ok, `the upload of ${name} to ${key} was refused`).toBe(true);
  return ((await response.json()) as ArtifactUploadResponse).artifact.slug;
}

/**
 * One issue holding every node part G's rows read (c-row-layout, c-cut, c-author, c-writer of
 * part G's Verification document): a child with a blocker and a session's
 * claim; the reader's comment; an agent's message with a link and "UTF-16", and the reader's
 * reply to each of the agent's messages; the statement `r-cites`, citing `notes.md` and the
 * child, with bold words and two paragraphs (a `requirement` holds paragraphs and no list,
 * `blocks.json`); a one-line statement `r-one`; the statement `r-ties`, whose words cite `r-one`'s
 * block by its address (a requirement takes no attribute, so the citation is the tie);
 * a plan task that needs one opening "**1\. Stand"; a check, with a result an untitled session
 * recorded; a decision that shaped a statement, one opening "Unsure:" that cites `t1`, one
 * holding `:::requirement{#r1}` and a block's address, and one a person asked to change; a
 * picture; an open question with two options waiting on the reader, an answered one, and the
 * long texts c-cut reads. It is at most three levels deep (`DRAWN_LEVELS`).
 */
export async function seedRows(): Promise<RowSeed> {
  await createProject({ key: PROJECT, name: "Rows" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: `:::requirement{#r-one}\n${ONE_LINE_WORDS}\n:::\n`,
    title: "A node of an issue is drawn as one row",
  });
  const key = issue.key;
  const blocker = await createIssue({
    force: true,
    parent: key,
    project: PROJECT,
    title: "Read the names",
  });
  const childTitle = "Draw the child's row";
  const child = await createIssue({
    force: true,
    parent: key,
    project: PROJECT,
    title: childTitle,
  });
  await patchIssue(child.key, { blocked_by: [blocker.key] });
  await claimIssue(child.key, asPlanner);

  const notes = await createIssueArtifact(key, {
    content: "# Notes\n\nThe measurements behind the rows.\n",
    name: "notes.md",
  });
  const specSlug =
    ((await getIssue(key)).artifacts ?? []).find((artifact) => artifact.primary)?.slug ?? "";
  const spec = (block: string) => `dispatch://${key}/spec#b-${block}`;
  await editArtifact(issue.primary_artifact_id, {
    ops: [
      {
        after: "end",
        markdown:
          ":::requirement{#r-cites}\n" +
          `The **measured** rows are in dispatch://${key}/artifact/${notes.artifact.slug}, and ` +
          `dispatch://${child.key} draws them.\n\nOne row per node, and one edge per row.\n:::\n` +
          `\n:::requirement{#r-ties}\nEach row follows ${spec("r-one")}.\n:::\n`,
        op: "insert",
      },
    ],
  });

  const plan = await createIssueArtifact(key, {
    content: [
      `:::plan_task{#t1 serves="${spec("r-one")}"}\n**1\\. Stand the harness up and read the rows**\n:::`,
      `:::plan_task{#t2 needs="dispatch://${key}/artifact/plan-md#b-t1"}\nDraw the rows.\n:::`,
    ].join("\n\n"),
    name: "plan.md",
  });
  const verification = await createIssueArtifact(key, {
    content:
      `:::check{#c1 serves="${spec("r-one")}" surface="browser" run="bun run e2e" ` +
      `when="before_merge"}\nEach node is one row.\n:::\n`,
    name: "verification.md",
  });
  // A pass of a before_merge check names the negative control that would have caught its
  // failure: a server that holds that rule refuses one with none `400 NEGATIVE_CONTROL_REQUIRED`,
  // and one that does not stores it as it is given.
  const recorded = await recordCheckResult(
    key,
    {
      check: "c1",
      commit: "a".repeat(40),
      negative_control: "Drew one node as two rows and the run failed.",
      observed: "One row per node.",
      role: "tester",
      verdict: "pass",
    },
    { actor: recorderActor, as: "agent" }
  );

  const answerWords = longWords("said", 160);
  const answered = await createAsk(
    key,
    { question: "Which of the two layouts is the page?", urgency: "high" },
    asPlanner
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: [], text: answerWords });
  const answeredLong = await createAsk(
    key,
    { question: longWords("asked", 160), urgency: "high" },
    asPlanner
  );
  await answerAsk(answeredLong.id, { expected_edited_at: null, selected: [], text: "The first." });
  const waitingLong = await createAsk(
    key,
    { question: longWords("waits", 160), urgency: "high" },
    asPlanner
  );
  const openQuestion = await createAsk(
    key,
    {
      options: [{ label: "1280 px" }, { label: "1024 px" }],
      question: "At which width does the group move under the words?",
      urgency: "high",
    },
    asPlanner
  );

  const headings = {
    changed: "Name every node from one read",
    raw: "Keep the source out of the rows",
    shaping: "Draw a statement on one line",
    unsure: "Fold to the frontier and not to a depth",
  };
  const decisions = await createIssueArtifact(key, {
    content: [
      `## ${headings.shaping}\n\nShapes ${spec("r-one")}.`,
      `## ${headings.changed}\n\n${CHANGED_DECISION_WORDS} Shapes ${spec("r-cites")}.`,
      `## ${headings.unsure}\n\nUnsure: a reader who never finds the newest question would change this. ` +
        `It follows dispatch://${key}/artifact/${plan.artifact.slug}#b-t1.`,
      `## ${headings.raw}\n\nIt names ${spec("r-one")}.\n\n:::requirement{#r1}\nThe page draws no raw source.\n:::`,
    ].join("\n\n"),
    name: "decisions.md",
  });
  // A person asks to change one decision, by a comment anchored to its words after they last
  // changed (decision 45: the anchor names the document by its id).
  const change = await createComment(key, {
    anchor: { artifact: decisions.artifact.id, quote: CHANGED_DECISION_WORDS },
    body: "Name them from the tree instead.",
  });

  const comment = await createComment(key, { body: "These rows read well." });
  const longComment = await createComment(key, { body: longWords("mine", 160) });
  const longMessage = await createMessage(key, { body: longWords("plan", 160) }, asPlanner);
  const linked = await createMessage(
    key,
    {
      body: `Read [the notes](https://example.com/notes) with dispatch://${child.key} beside them, in UTF-16.`,
    },
    asPlanner
  );
  // The outline draws a message thread only once a person wrote in it (`agents-only.ts`), so the
  // reader answers each of the planner's two messages.
  const replies: string[] = [];
  for (const message of [longMessage, linked]) {
    replies.push((await createMessage(key, { body: "Read.", in_reply_to: message.id })).id);
  }
  const picture = await uploadPicture(key, "layout.png");
  const pictured = await createComment(key, {
    body: `The layout at 1440 px:\n\n![The layout at 1440 px](dispatch://${key}/artifact/${picture})`,
  });

  const decisionsSlug = decisions.artifact.slug;
  // A decision is addressed by its heading block, whose id the server gives.
  const decision = async (heading: string) =>
    blockNode(decisionsSlug, await blockId(decisions.artifact.id, `## ${heading}`));
  return {
    answered: answered.id,
    answeredLong: answeredLong.id,
    answerWords,
    blocker: blocker.key,
    change: change.id,
    changed: await decision(headings.changed),
    child: child.key,
    childTitle,
    comment: comment.id,
    key,
    linked: linked.id,
    longComment: longComment.id,
    longMessage: longMessage.id,
    openQuestion: openQuestion.id,
    picture,
    pictured: pictured.id,
    raw: await decision(headings.raw),
    replies,
    result: recorded.result.id,
    shaping: await decision(headings.shaping),
    slugs: {
      decisions: decisionsSlug,
      notes: notes.artifact.slug,
      plan: plan.artifact.slug,
      spec: specSlug,
      verification: verification.artifact.slug,
    },
    unsure: await decision(headings.unsure),
    waitingLong: waitingLong.id,
  };
}

/** How many long rows of the planner's `seedLongRows` writes: whole, at 1440 px, more than ten
 *  screens of them. */
export const LONG_ROWS = 60;

/** How many of the planner's rows stand above the reader's own run of long rows, which is a
 *  third of the way down the page: well away from both the middle and the rows drawn at load. */
const ROWS_ABOVE_OWN = 20;

/** How many long rows of the reader's own `seedLongRows` writes: more than fill a screen. */
const OWN_ROWS = 6;

/** What `seedLongRows` wrote: the issue; the planner's long rows by node id, top to bottom; the
 *  reader's own long rows, top to bottom; the planner's row right above the reader's; and the
 *  planner's long statement, the first row of the body. */
export interface LongRowsSeed {
  readonly key: string;
  readonly rows: readonly string[];
  readonly own: readonly string[];
  readonly aboveOwn: string;
  readonly statement: string;
}

/**
 * One issue whose spec opens with one long statement the planner wrote, which a check of its
 * Verification document serves and no result proves, and whose conversation is the reader's
 * comment and, under it, `LONG_ROWS` replies of the planner's of 160 short words each, which a
 * row cuts at six lines at either width and draws whole shorter than a phone's screen, with a
 * run of `OWN_ROWS` replies of the reader's own of the same words after the first
 * `ROWS_ABOVE_OWN`, which no cut ever shortens. Drawn whole they make a page many screens tall,
 * so a jump into it lands on rows no batch measured. The statement is the body's first row, and
 * the check nothing proves keeps it open in an outline folded to what is open.
 */
export async function seedLongRows(): Promise<LongRowsSeed> {
  await createProject({ key: PROJECT, name: "Rows" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "A conversation of long rows",
  });
  const key = issue.key;
  const body = longWords("w", 160);
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: `:::requirement{#r-long}\n${body}\n:::\n`, op: "insert" }] },
    asPlanner
  );
  const serves = buildDispatchReference({ block: { id: "r-long" }, key, kind: "spec" });
  await createIssueArtifact(key, {
    content:
      `:::check{#c-long serves="${serves}" surface="browser" run="bun run e2e" ` +
      `when="before_merge"}\nThe long statement holds.\n:::\n`,
    name: "verification.md",
  });
  const specSlug =
    ((await getIssue(key)).artifacts ?? []).find((artifact) => artifact.primary)?.slug ?? "";
  // A question that waits on the reader, which an outline that opens with the focus on such a
  // question focuses in place of its first row, so the statement keeps its cut.
  await createAsk(key, { question: "At which width is the cut read?", urgency: "high" }, asPlanner);
  const head = await createComment(key, { body: "Each reply under this one is long." });
  const rows: string[] = [];
  const own: string[] = [];
  for (let at = 0; at < LONG_ROWS; at += 1) {
    if (at === ROWS_ABOVE_OWN) {
      for (let mine = 0; mine < OWN_ROWS; mine += 1) {
        own.push((await createComment(key, { body, reply_to: head.id })).id);
      }
    }
    rows.push((await createComment(key, { body, reply_to: head.id }, asPlanner)).id);
  }
  return {
    aboveOwn: rows[ROWS_ABOVE_OWN - 1] ?? "",
    key,
    own,
    rows,
    statement: blockNode(specSlug, "r-long"),
  };
}

/** How many columns the table of `seedWideNote`'s note holds: at 390 px, each headed by one word
 *  that no line breaks, far more than the width of a row's words. */
export const WIDE_NOTE_COLUMNS = 16;

/** What `seedWideNote` wrote: the issue, and its note's node id. */
export interface WideNoteSeed {
  readonly key: string;
  readonly note: string;
}

/**
 * One issue whose spec holds one note - a `callout` block, which the page names "note" - whose
 * body is a table of `WIDE_NOTE_COLUMNS` columns: a line of heads and a line of values. The note
 * is in the spec because the page draws no block of a `notes.md`: written there, the note is
 * drawn nowhere and the row has no table to read. A summary callout stands before it: the
 * outline takes the spec's first callout for the issue's summary and draws no row of it
 * (`spec-summary.ts`), so a note that stood first would have no row, and its table would be
 * drawn nowhere on the Conversation view.
 * A row cuts words taller than six lines (ruling 1), and the table is two lines tall, so its row
 * draws it whole. It is an issue of its own, so no seed another row reads changes,
 * and no row that reads every row of `seedRows` - c-row-layout reads where each typed row's first
 * line ends against its tag - meets a table wider than a row's words.
 */
export async function seedWideNote(): Promise<WideNoteSeed> {
  await createProject({ key: PROJECT, name: "Rows" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "A note holds a table wider than a phone",
  });
  const line = (cells: readonly string[]) => `| ${cells.join(" | ")} |`;
  const columns = Array.from({ length: WIDE_NOTE_COLUMNS }, (_unused, at) => at + 1);
  const table = [
    line(columns.map((column) => `column${column}`)),
    line(columns.map(() => "---")),
    line(columns.map((column) => `value${column}`)),
  ].join("\n");
  const summary =
    ':::callout{#summary kind="note"}\nWhat the issue is about, in a sentence.\n:::\n';
  const note = `:::callout{#n-wide kind="note"}\n${table}\n:::\n`;
  await editArtifact(issue.primary_artifact_id, {
    ops: [{ after: "end", markdown: `${summary}\n${note}`, op: "insert" }],
  });
  const specSlug =
    ((await getIssue(issue.key)).artifacts ?? []).find((artifact) => artifact.primary)?.slug ?? "";
  return { key: issue.key, note: blockNode(specSlug, "n-wide") };
}
