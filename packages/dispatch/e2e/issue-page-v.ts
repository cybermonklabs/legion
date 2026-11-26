import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL_LABELS, type StatementState } from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifact,
  getIssueCoverage,
  recordCheckResult,
} from "./api";
import type { WholeIssueSeed } from "./issue-layout-seed";
import { HEAD_SHA, PROJECT, pause, recorderActor } from "./issue-layout-seed-support";
import { requirementBlockId, specBlockRef } from "./issue-layout-seed-text";
import { issuePage } from "./issue-page";
import { longWords } from "./issue-page-g";
import { openPreset } from "./issue-page-i";

/**
 * How the rows of `e2e/issue-page-v.e2e.ts` read the Verification view of the new issue page:
 * the readers that `e2e/issue-page.ts`, `e2e/issue-page-g.ts` and `e2e/issue-page-i.ts` do not
 * hold. They are a file of their own, so the rows of the page's other views never change it.
 *
 * What the view draws (`verification-filter/VerificationFilterBody.tsx`), its body
 * `[data-verification-body]`:
 *
 * - **A card** `[data-verification-card]` for each statement, with `data-node-id`,
 *   `data-statement-state` (met, not_met, unknown) and `data-node-context`. Inside it each check
 *   `[data-verification-check]` with `data-node-id` and `data-node-state`, its words the control
 *   `[data-verification-check-words]` that opens its facts, its latest result
 *   `[data-verification-result]` (`data-result-stands` on the one its state stands on), and the
 *   control `[data-verification-earlier]` that opens its earlier ones.
 * - **Over the cards** the issue's own row `[data-verification-issue]`, the tree's first row,
 *   holding the summary `[data-verification-summary]` with `[data-verification-covered]`,
 *   `[data-verification-bar]`, `[data-verification-legend]`, `[data-verification-check-legend]`
 *   (one `[data-check-legend-state]` for each of the seven states) and
 *   `[data-verification-commit]`, and the chips `[data-verification-chip]` with `aria-pressed`;
 *   then the table `[data-verification-table]` and its headings `[data-verification-head]`.
 * - **Under a statement's checks** the line `[data-verification-elsewhere]` that counts the
 *   checks of the issues under this one that serve it.
 * - **Over the tree**, for each read that failed before it first answered, one line
 *   `[data-verification-unreadable]` whose value names that read (`coverage`, `results` or
 *   `blocks`): the page's words for it and a "Retry" button that asks that read alone again.
 *
 * Each row of the tree (`[data-node-row]`), the issue's row, each card, each check and each
 * other row, carries its own id as `data-verification-row`; a check drawn under a statement
 * carries the statement's id and its own, joined by a space.
 *
 * Each measure over the cards fails when the view draws none, so a view that draws nothing
 * never passes a reading of what it draws. `openSettledView` opens `seedWholeIssue`'s issue and
 * reads its view once every read has answered, `refuseTreeRead` (`e2e/issue-page-t-tree.ts`)
 * refuses the tree read, which holds its Verification document's blocks, for the rows that read
 * the view while that read fails, and row 3's issue is `seedLinkedChecks`, at the end.
 */

/** The selectors the readers pass into the browser. */
const SELECTORS = {
  card: "[data-verification-card]",
  commit: "[data-verification-commit]",
  issuePage: "[data-issue-page]",
} as const;

/** The message every measure fails with where the view draws no card. */
const NO_CARD = "the Verification view draws no card";

/** How the rows and row 3's seed record a result: as the agent session that records every
 *  seeded result. */
export const asRecorder = { actor: recorderActor, as: "agent" as const };

/** What `read` answers in the page, given `SELECTORS`, which counts the view's cards as
 *  `cards`. Fails where the view draws no card. */
async function readCards<T extends { readonly cards: number }>(
  page: Page,
  read: (selectors: typeof SELECTORS) => T
): Promise<T> {
  const answer = await page.evaluate(read, SELECTORS);
  expect(answer.cards, NO_CARD).toBeGreaterThan(0);
  return answer;
}

/** The view's cards, one for each statement it draws. */
export function verificationCardBoxes(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.card);
}

/** A check's `run` command, drawn as code with its copy control beside it. */
export function nodeCommands(page: Page): Locator {
  return issuePage(page).locator("[data-node-command]");
}

export function copyCommandControls(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.copy_command });
}

/** The view's body, which holds its one tree. */
export function verificationBody(page: Page): Locator {
  return issuePage(page).locator("[data-verification-body]");
}

/** Each line the view draws over its tree for a read that failed before it first answered,
 *  whose `data-verification-unreadable` names that read; given `read`, that read's line alone. */
export function verificationUnreadable(
  page: Page,
  read?: "blocks" | "coverage" | "results"
): Locator {
  return issuePage(page).locator(
    read === undefined
      ? "[data-verification-unreadable]"
      : `[data-verification-unreadable="${read}"]`
  );
}

/** The issue's own row, the first row of the view's tree, which holds the summary and the
 *  chips. */
export function verificationIssueRow(page: Page): Locator {
  return issuePage(page).locator("[data-verification-issue]");
}

/** The card of the statement whose node id is `id`. */
export function verificationCard(page: Page, id: string): Locator {
  return issuePage(page).locator(`${SELECTORS.card}[data-node-id="${id}"]`);
}

/** Each row the view draws for the check whose node id is `id`: one under each statement it
 *  serves. */
export function verificationCheck(page: Page, id: string): Locator {
  return issuePage(page).locator(`[data-verification-check][data-node-id="${id}"]`);
}

/** The chip All, or the chip of one state a statement can be in. */
export function verificationChip(page: Page, chip: "all" | StatementState): Locator {
  return issuePage(page).locator(`[data-verification-chip="${chip}"]`);
}

/** One check the view draws under a statement. */
export interface DrawnCheck {
  /** How many results the check's fold holds beside its latest, as its control says it. */
  readonly earlier: string | null;
  readonly id: string;
  /** How many results the check draws. */
  readonly results: number;
  /** The `data-glyph` token of the shape its state is drawn with, or null where it draws none. */
  readonly shape: string | null;
  /** `data-node-state`, the model's own token for how the check stands. */
  readonly state: string | null;
  /** The word the page draws for its state. */
  readonly word: string | null;
}

/** One statement the view draws, with the checks that serve it. */
export interface DrawnStatement {
  readonly checks: readonly DrawnCheck[];
  readonly gapKinds: readonly string[];
  readonly id: string;
  /** `data-statement-state`: met, not_met or unknown. */
  readonly state: string | null;
  /** The word the page draws for that state. */
  readonly stateWord: string | null;
}

/** Every statement the view draws, in document order, with its checks and their latest
 *  results. Fails where the view draws no card. */
export async function verificationCards(page: Page): Promise<DrawnStatement[]> {
  const read = await readCards(page, ({ card, issuePage: issue }) => {
    const text = (element: Element | null): string =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    const cards = [...document.querySelectorAll(`${issue} ${card}`)];
    return {
      cards: cards.length,
      statements: cards.map((drawn) => ({
        checks: [...drawn.querySelectorAll("[data-verification-check]")].map((check) => ({
          earlier: text(check.querySelector("[data-verification-earlier]")) || null,
          id: check.getAttribute("data-node-id") ?? "",
          results: check.querySelectorAll("[data-verification-result]").length,
          shape:
            check
              .querySelector("span[data-node-state]")
              ?.parentElement?.querySelector("svg[data-glyph]")
              ?.getAttribute("data-glyph") ?? null,
          state: check.getAttribute("data-node-state"),
          word: text(check.querySelector("span[data-node-state]")) || null,
        })),
        gapKinds: [...drawn.querySelectorAll("[data-gap-kind]")].map(
          (gap) => gap.getAttribute("data-gap-kind") ?? ""
        ),
        id: drawn.getAttribute("data-node-id") ?? "",
        state: drawn.getAttribute("data-statement-state"),
        stateWord: text(drawn.querySelector("span[data-statement-state]")) || null,
      })),
    };
  });
  return read.statements;
}

/** The one commit the view names for the issue's results, or null where it names none or more
 *  than one. Fails where the view draws no card. */
export async function namedCommit(page: Page): Promise<string | null> {
  const read = await readCards(page, ({ card, commit, issuePage: issue }) => {
    const drawn = document.querySelectorAll(`${issue} ${commit}`);
    return {
      cards: document.querySelectorAll(`${issue} ${card}`).length,
      commit:
        drawn.length === 1 ? (drawn[0]?.querySelector("code")?.textContent ?? "").trim() : null,
    };
  });
  return read.commit;
}

/** One element that reaches past a side of the card it is drawn in. */
export interface Overflowing {
  /** The card's own description: its tag and its node id. */
  readonly card: string;
  /** How far past the card's left or right edge the element reaches, in CSS pixels. */
  readonly over: number;
  readonly what: string;
}

/** Every element whose painted box reaches more than a pixel sideways past the view's card it
 *  sits in: past its left edge or its right edge; above and below are not measured. Fails where
 *  the view draws no card. */
export async function boxesPastTheirCard(page: Page): Promise<Overflowing[]> {
  const read = await readCards(page, ({ card, issuePage: issue }) => {
    const describe = (element: Element): string => {
      // This runs in the page over a view the caller has already settled, and only names the
      // element.
      const id = element.closest(card)?.getAttribute("data-node-id") ?? "";
      const own = (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
      return `${element.tagName.toLowerCase()}${id === "" ? "" : ` in ${id}`} "${own}"`;
    };
    const cards = [...document.querySelectorAll(`${issue} ${card}`)];
    const found: { card: string; over: number; what: string }[] = [];
    for (const drawn of cards) {
      const bounds = drawn.getBoundingClientRect();
      if (bounds.width === 0) continue;
      for (const element of drawn.querySelectorAll("*")) {
        const rect = element.getBoundingClientRect();
        if (rect.width === 0 && rect.height === 0) continue;
        const over = Math.max(bounds.left - rect.left, rect.right - bounds.right);
        if (over > 1) found.push({ card: describe(drawn), over, what: describe(element) });
      }
    }
    return { cards: cards.length, found };
  });
  return read.found;
}

/** Two pieces of text drawn over each other. */
export interface Overlap {
  readonly first: string;
  readonly second: string;
}

/**
 * Every pair of texts inside the view's cards whose drawn lines share a point on screen. Each
 * text node that holds words is measured line by line (`Range.getClientRects`), so the words an
 * element draws beside a child element of its own - the line of earlier results, a statement's
 * "more", a paragraph that holds a link - are read as much as a leaf's, and no box that holds
 * another is read as overlapping it. The lines of one text node are never read against each
 * other: a wrapped line stands beside the next one's start, never over it. Text kept for a
 * screen reader alone (inside `.sr-only`, whose box clips it to a pixel while its words keep
 * their laid-out width) is no text the page draws, and is left out. Fails where the view draws
 * no card.
 */
export async function overlappingTexts(page: Page): Promise<Overlap[]> {
  const read = await readCards(page, ({ card, issuePage: issue }) => {
    const cards = [...document.querySelectorAll(`${issue} ${card}`)];
    const lines: { label: string; node: number; rect: DOMRect }[] = [];
    const range = document.createRange();
    let node = 0;
    for (const root of cards) {
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
        const words = (text.textContent ?? "").replace(/\s+/g, " ").trim();
        const holder = text.parentElement;
        if (words === "" || holder === null || holder.closest(".sr-only, option") !== null) {
          continue;
        }
        node += 1;
        const label = `${holder.tagName.toLowerCase()} "${words.slice(0, 40)}"`;
        range.selectNodeContents(text);
        for (const rect of range.getClientRects()) {
          if (rect.width > 0 && rect.height > 0) lines.push({ label, node, rect });
        }
      }
    }
    const found: { first: string; second: string }[] = [];
    // Each pair of text nodes is said once, however many of their lines overlap.
    const said = new Set<string>();
    for (let i = 0; i < lines.length; i += 1) {
      for (let j = i + 1; j < lines.length; j += 1) {
        const a = lines[i];
        const b = lines[j];
        if (a === undefined || b === undefined || a.node === b.node) continue;
        const apart =
          a.rect.right <= b.rect.left + 0.5 ||
          b.rect.right <= a.rect.left + 0.5 ||
          a.rect.bottom <= b.rect.top + 0.5 ||
          b.rect.bottom <= a.rect.top + 0.5;
        const pair = `${a.node}:${b.node}`;
        if (apart || said.has(pair)) continue;
        said.add(pair);
        found.push({ first: a.label, second: b.label });
      }
    }
    return { cards: cards.length, found };
  });
  return read.found;
}

/** How the view's body and its table stand at one width, in CSS pixels. */
export interface VerificationFrame {
  /** `[data-verification-body]`: its width, its computed top and bottom margins, and the width
   *  of the element around it, which is the stage's column. */
  readonly body: {
    readonly bottom: number;
    readonly parent: number;
    readonly top: number;
    readonly width: number;
  };
  /** The width of each column heading, in order, and of `[data-verification-table]`. A heading
   *  the width does not draw is 0 wide. */
  readonly columns: { readonly heads: readonly number[]; readonly table: number };
  /** The first card's computed left edge and top left corner, and the space between it and the
   *  second card, null where the view draws one card. */
  readonly firstCard: {
    readonly borderLeft: string;
    readonly gap: number | null;
    readonly radius: string;
  };
  /** The document's whole width, and the viewport's. */
  readonly page: { readonly scroll: number; readonly viewport: number };
}

/** The view's measure at the width the page is at. Fails where the view draws no card. */
export async function verificationFrame(page: Page): Promise<VerificationFrame> {
  const read = await readCards(page, ({ card, issuePage: issue }) => {
    const width = (element: Element | null | undefined): number =>
      element == null ? 0 : element.getBoundingClientRect().width;
    const body = document.querySelector(`${issue} [data-verification-body]`);
    const margins = body === null ? null : getComputedStyle(body);
    const cards = [...document.querySelectorAll(`${issue} ${card}`)];
    const [first, second] = cards;
    const edge = first === undefined ? null : getComputedStyle(first);
    // The headings are the cells of the head that hold no cell of their own: Statement, then
    // Checks and Latest result inside the rest of the row.
    const heads = [
      ...(document.querySelector(`${issue} [data-verification-head]`)?.querySelectorAll("span") ??
        []),
    ].filter((cell) => cell.children.length === 0);
    return {
      cards: cards.length,
      frame: {
        body: {
          bottom: Number.parseFloat(margins?.marginBottom ?? ""),
          parent: width(body?.parentElement),
          top: Number.parseFloat(margins?.marginTop ?? ""),
          width: width(body),
        },
        columns: {
          heads: heads.map((cell) => width(cell)),
          table: width(document.querySelector(`${issue} [data-verification-table]`)),
        },
        firstCard: {
          borderLeft: edge?.borderLeftWidth ?? "",
          gap:
            first === undefined || second === undefined
              ? null
              : second.getBoundingClientRect().top - first.getBoundingClientRect().bottom,
          radius: edge?.borderTopLeftRadius ?? "",
        },
        page: { scroll: document.documentElement.scrollWidth, viewport: window.innerWidth },
      },
    };
  });
  return read.frame;
}

/** How many checks the summary's legend counts in each of the seven states, by the state's
 *  token (`data-check-legend-state`); a state whose count it does not draw reads NaN. Fails
 *  where the view draws no card. */
export async function checkLegendCounts(page: Page): Promise<Record<string, number>> {
  const read = await readCards(page, ({ card, issuePage: issue }) => ({
    cards: document.querySelectorAll(`${issue} ${card}`).length,
    counts: [...document.querySelectorAll(`${issue} [data-check-legend-state]`)].map(
      (item): [string, number] => {
        // The count is the state's first span, after its shape and before its word.
        const count = item.querySelector(":scope > span")?.textContent;
        return [
          item.getAttribute("data-check-legend-state") ?? "",
          count == null ? Number.NaN : Number(count.replace(/\D/g, "")),
        ];
      }
    ),
  }));
  return Object.fromEntries(read.counts);
}

/** How many shapes the view's body draws, and the token of each one it draws with no box,
 *  which no reader sees: with forced colours each state must keep its shape. Fails where the
 *  view draws no card. */
export async function glyphsWithNoBox(
  page: Page
): Promise<{ readonly drawn: number; readonly empty: readonly string[] }> {
  const read = await readCards(page, ({ card, issuePage: issue }) => {
    const glyphs = [
      ...document.querySelectorAll(`${issue} [data-verification-body] svg[data-glyph]`),
    ];
    return {
      cards: document.querySelectorAll(`${issue} ${card}`).length,
      drawn: glyphs.length,
      empty: glyphs
        .filter((glyph) => {
          const box = glyph.getBoundingClientRect();
          return box.width <= 0 || box.height <= 0;
        })
        .map((glyph) => glyph.getAttribute("data-glyph") ?? ""),
    };
  });
  return { drawn: read.drawn, empty: read.empty };
}

/** What the page paints of the marks each state is told apart by in the view's cards. */
export interface MarkPaint {
  /** Whether the page reads `forced-colors: active`. */
  readonly forced: boolean;
  /** How many marks were read: each word of a check's state (`span[data-node-state]`) and of a
   *  statement's (`span[data-statement-state]`), and each shape (`svg[data-glyph]`). */
  readonly read: number;
  /** Each mark a reader does not see, and why. */
  readonly unseen: readonly string[];
}

/**
 * Whether each word and shape that tells a state apart in the view's cards is painted where a
 * reader sees it: with forced colours each state must keep its word and its shape.
 * What a mark says and which shape it is are the page's markup, which forced colours never
 * change, so this reads what is painted: the page renders the mark (`checkVisibility`, its
 * `visibility` and `opacity` read too), it has a box of some size, and its colour is not the
 * colour it stands on, the computed `background-color` of the nearest ancestor that paints one,
 * which is its card. A shape paints with its stroke, which `NodeGlyph` sets to the current
 * colour. Fails where the view draws no card.
 */
export async function paintedMarks(page: Page): Promise<MarkPaint> {
  const read = await readCards(page, ({ card, issuePage: issue }) => {
    const cards = [...document.querySelectorAll(`${issue} ${card}`)];
    const marks = cards.flatMap((drawn) => [
      ...drawn.querySelectorAll(
        "span[data-node-state], span[data-statement-state], svg[data-glyph]"
      ),
    ]);
    const unseen: string[] = [];
    for (const mark of marks) {
      // This runs in the page over a view the caller has already settled, and only names each mark.
      const glyph = mark.getAttribute("data-glyph");
      const id = mark.closest(card)?.getAttribute("data-node-id") ?? "";
      const what =
        glyph === null
          ? `the word "${(mark.textContent ?? "").trim()}" in ${id}`
          : `the shape ${glyph} in ${id}`;
      const style = getComputedStyle(mark);
      const paint =
        glyph === null || /^currentcolor$/iu.test(style.stroke) ? style.color : style.stroke;
      let ground = "";
      for (let under = mark.parentElement; under !== null && ground === ""; ) {
        const fill = getComputedStyle(under).backgroundColor;
        // A background of alpha 0 paints nothing: the one under it shows through.
        if (fill !== "transparent" && !/^rgba\(.*,\s*0\)$/u.test(fill)) ground = fill;
        under = under.parentElement;
      }
      const box = mark.getBoundingClientRect();
      if (!mark.checkVisibility({ opacityProperty: true, visibilityProperty: true })) {
        unseen.push(`${what} is not rendered`);
      }
      if (box.width <= 0 || box.height <= 0) unseen.push(`${what} draws no box`);
      if (paint === ground) unseen.push(`${what} is painted ${paint}, the colour it stands on`);
    }
    return {
      cards: cards.length,
      forced: matchMedia("(forced-colors: active)").matches,
      read: marks.length,
      unseen,
    };
  });
  return { forced: read.forced, read: read.read, unseen: read.unseen };
}

/** How a link in a check's words is drawn: its colour and the colour of the words it stands
 *  in, and its underline's line and colour. */
export interface LinkLook {
  readonly color: string;
  readonly line: string;
  readonly underline: string;
  readonly words: string;
}

export function linkLook(link: Locator): Promise<LinkLook> {
  return link.evaluate((element) => {
    const words = element.closest("[data-verification-check-words]") ?? element.parentElement;
    const drawn = getComputedStyle(element);
    return {
      color: drawn.color,
      line: drawn.textDecorationLine,
      underline: drawn.textDecorationColor,
      words: words === null ? "" : getComputedStyle(words).color,
    };
  });
}

/**
 * A walk by Tab alone: each call presses Tab until the control it names holds the keyboard focus
 * and answers how many presses that took, and the walk presses at most `presses` times over all
 * its calls. So a row names the controls in the order Tab must reach them, and fails with the
 * message of the first one Tab does not reach in what is left of the presses.
 */
export function tabWalk(
  page: Page,
  presses: number
): (target: Locator, message: string) => Promise<number> {
  let left = presses;
  return async (target, message) => {
    for (let pressed = 0; ; pressed += 1) {
      // Tab moves the focus within the press itself, so one reading after each press is settled.
      const reached = await target.evaluateAll((found) =>
        found.some((element) => element === document.activeElement)
      );
      if (reached) return pressed;
      expect(left, message).toBeGreaterThan(0);
      left -= 1;
      await page.keyboard.press("Tab");
    }
  };
}

// ---------------------------------------------------------------------------------------------
// The whole issue's view once every read has answered, what the view draws of it before, and
// its Verification document's blocks read refused.
// ---------------------------------------------------------------------------------------------

/** What the view draws of each statement `ids` names - the checks drawn under it, and how many
 *  lines it draws that say no check serves it or that checks serve it elsewhere - or null where
 *  it draws no card for it; and every check it draws as one that serves no statement. */
export interface ServedReading {
  readonly loose: readonly string[];
  readonly statements: Readonly<
    Record<string, { checks: string[]; elsewhere: number; gaps: number } | null>
  >;
}

export function readServed(page: Page, ids: readonly string[]): Promise<ServedReading> {
  return page.evaluate(
    ({ selectors, statementIds }) => {
      const issue = selectors.issuePage;
      const statements: Record<
        string,
        { checks: string[]; elsewhere: number; gaps: number } | null
      > = {};
      for (const id of statementIds) {
        const card = document.querySelector(`${issue} ${selectors.card}[data-node-id="${id}"]`);
        statements[id] =
          card === null
            ? null
            : {
                checks: [...card.querySelectorAll("[data-verification-check]")].map(
                  (check) => check.getAttribute("data-node-id") ?? ""
                ),
                elsewhere: card.querySelectorAll("[data-verification-elsewhere]").length,
                gaps: card.querySelectorAll("[data-gap-kind]").length,
              };
      }
      const loose = [
        ...document.querySelectorAll(`${issue} [data-verification-check][data-verification-loose]`),
      ].map((check) => check.getAttribute("data-node-id") ?? "");
      return { loose, statements };
    },
    { selectors: SELECTORS, statementIds: ids }
  );
}

/** Each thing `held` draws that `truth`, the view once every read has answered, does not: a
 *  check drawn as one that serves no statement, and a statement drawn with other checks, or with
 *  a line saying none serves it or that its checks stand elsewhere. A statement not drawn at all
 *  says nothing, so it contradicts nothing. */
export function contradictions(truth: ServedReading, held: ServedReading): string[] {
  const found = held.loose
    .filter((id) => !truth.loose.includes(id))
    .map((id) => `${id} is drawn as a check that serves no statement`);
  for (const [id, drawn] of Object.entries(held.statements)) {
    if (drawn === null) continue;
    const settled = truth.statements[id];
    if (JSON.stringify(drawn) !== JSON.stringify(settled)) {
      found.push(
        `${id} draws ${JSON.stringify(drawn)} where it settles on ${JSON.stringify(settled)}`
      );
    }
  }
  return found;
}

/** What `openSettledView` read of `seedWholeIssue`'s issue. */
export interface SettledView {
  /** The node ids of statements 1 to 6, in document order. */
  readonly ids: readonly string[];
  /** Each of the issue's six checks by its node id, with the node id of the statement it
   *  serves: check i serves statement i (`seedWholeIssue`). */
  readonly served: readonly { readonly check: string; readonly statement: string }[];
  /** The view once every read has answered, as `readServed` reads it. */
  readonly truth: ServedReading;
}

/** Opens the Verification view on `seedWholeIssue`'s issue and waits until each of statements 1
 *  to 6 draws the one check that serves it, which it does once every read has answered. */
export async function openSettledView(page: Page, seeded: WholeIssueSeed): Promise<SettledView> {
  const [{ slug: specSlug }, { slug: verificationSlug }] = await Promise.all([
    getArtifact(seeded.specArtifactId),
    getArtifact(seeded.verificationArtifactId),
  ]);
  // Check i serves statement i, for 1 to 6 (`seedWholeIssue`).
  const served = seeded.checks.all.map((check, at) => ({
    check: blockNodeId(verificationSlug, check),
    statement: blockNodeId(specSlug, requirementBlockId(at + 1)),
  }));
  const ids = served.map((one) => one.statement);
  await openPreset(page, seeded.key, "verification");
  await expect
    .poll(
      async () => {
        const now = await readServed(page, ids);
        return served.every((one) => now.statements[one.statement]?.checks.join() === one.check);
      },
      { message: "the settled view draws a statement without the check that serves it" }
    )
    .toBe(true);
  return { ids, served, truth: await readServed(page, ids) };
}

// ---------------------------------------------------------------------------------------------
// Row 3's issue: a check with links serving two statements, and a child's check serving a third.
// ---------------------------------------------------------------------------------------------

/** The block ids of statements A, B and C of row 3's spec, and of the issue's one check. */
const LINKED_BLOCKS = { a: "r-a", b: "r-b", c: "r-c", check: "c-linked" } as const;

/** Statement A: 200 words, which the view cuts at six lines. */
const STATEMENT_A_WORDS = longWords("stated", 200);

/** Statement B: more than 32 characters, so the line that names it by its first words cuts
 *  them at a word (`chipWords`). */
export const STATEMENT_B_WORDS =
  "The check that serves the first statement serves this one as well.";

/** Statement C, which only the child's check serves. */
const STATEMENT_C_WORDS = "Only a check of the issue under this one serves this statement.";

/** The address the check's words link to. */
export const RUNBOOK = "https://example.com/runbook";

/** What `seedLinkedChecks` wrote, by the node ids the view draws its rows under. */
export interface LinkedChecksSeed {
  readonly key: string;
  /** Statements A, B and C of the issue's spec, and the issue's one check. */
  readonly nodes: {
    readonly a: string;
    readonly b: string;
    readonly c: string;
    readonly check: string;
  };
  /** The `dispatch://` address of statement B, which the check's words cite. */
  readonly referenceToB: string;
}

/**
 * The issue row 3 of `e2e/issue-page-v.e2e.ts` reads: a spec of statements A (200 words), B and
 * C; a Verification document with one check that serves A and B, whose words hold a markdown
 * link to the runbook and the `dispatch://` address of B, with one result an agent's session
 * recorded; and a child whose Verification document holds one check that serves C. A `serves`
 * is written as `verificationDocument` writes one, `dispatch://<key>/spec#b-<id>`. The seed
 * waits until the coverage read counts one check for each statement, the child's included, so
 * the row reads the view and not the indexing.
 */
export async function seedLinkedChecks(): Promise<LinkedChecksSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: `${[
      [LINKED_BLOCKS.a, STATEMENT_A_WORDS],
      [LINKED_BLOCKS.b, STATEMENT_B_WORDS],
      [LINKED_BLOCKS.c, STATEMENT_C_WORDS],
    ]
      .map(([block, words]) => `:::requirement{#${block}}\n${words}\n:::`)
      .join("\n\n")}\n`,
    title: "A check's words draw their links in the Verification view",
  });
  const key = issue.key;
  const statement = (block: string) => specBlockRef(key, block);
  const verification = await createIssueArtifact(key, {
    content:
      "How the links are proved.\n\n" +
      `:::check{#${LINKED_BLOCKS.check} serves="${statement(LINKED_BLOCKS.a)} ${statement(LINKED_BLOCKS.b)}"` +
      ' surface="browser" run="bun run e2e -- e2e/issue-page-v.e2e.ts" when="before_merge"}\n' +
      `Read [the runbook](${RUNBOOK}) before the run, and then ${statement(LINKED_BLOCKS.b)}.\n:::\n`,
    name: "verification.md",
  });
  const child = await createIssue({
    force: true,
    parent: key,
    project: PROJECT,
    title: "Serve the last statement from the issue under it",
  });
  await createIssueArtifact(child.key, {
    content:
      "How the child is proved.\n\n" +
      `:::check{#c-below serves="${statement(LINKED_BLOCKS.c)}" surface="browser"` +
      ' run="bun run e2e" when="before_merge"}\n' +
      "The child's own check serves its parent's last statement.\n:::\n",
    name: "verification.md",
  });

  const deadline = Date.now() + 20_000;
  for (;;) {
    const counted = new Map(
      ((await getIssueCoverage(key)).requirements ?? []).map((one) => [one.block_id, one.checks])
    );
    if (
      [LINKED_BLOCKS.a, LINKED_BLOCKS.b, LINKED_BLOCKS.c].every((one) => counted.get(one) === 1)
    ) {
      break;
    }
    if (Date.now() > deadline) {
      throw new Error(
        `${key}'s coverage counts ${JSON.stringify([...counted])}, want one check for each statement`
      );
    }
    await pause(100);
  }
  // A pass of a before_merge check states the negative control that would have caught its
  // failure, which the record route requires of it (400 NEGATIVE_CONTROL_REQUIRED).
  await recordCheckResult(
    key,
    {
      check: LINKED_BLOCKS.check,
      commit: HEAD_SHA,
      negative_control: "Dropped the check's links and the run failed.",
      observed: "1 passed",
      role: "tester",
      run: "bun run e2e -- e2e/issue-page-v.e2e.ts",
      surface: "browser",
      verdict: "pass",
    },
    asRecorder
  );

  const specSlug = (await getArtifact(issue.primary_artifact_id ?? "")).slug;
  return {
    key,
    nodes: {
      a: blockNodeId(specSlug, LINKED_BLOCKS.a),
      b: blockNodeId(specSlug, LINKED_BLOCKS.b),
      c: blockNodeId(specSlug, LINKED_BLOCKS.c),
      check: blockNodeId(verification.artifact.slug, LINKED_BLOCKS.check),
    },
    referenceToB: statement(LINKED_BLOCKS.b),
  };
}
