import { expect, type Locator, type Page } from "@playwright/test";

import { answerAsk, createAsk, getAsk } from "./api";
import { seedWholeIssue } from "./issue-layout-seed";
import { asPlanner } from "./issue-layout-seed-support";
import { issuePage, presetRow } from "./issue-page";
import { longWords, READER } from "./issue-page-g";

/**
 * How the Decisions view's browser rows read it on the new issue page (the rows are in
 * `e2e/issue-page-q.e2e.ts`): the readers that D's `e2e/issue-page.ts`, G's `e2e/issue-page-g.ts`
 * and I's `e2e/issue-page-i.ts` do not hold (decision 2 of the parts). A later file's rows
 * import from this file and change nothing in it.
 *
 * What the view draws (`decisions-filter/DecisionsFilterBody.tsx`):
 *
 * - **The list** `[data-decisions-body]`, a region named by the view, holding the tree.
 * - **The named rows** first inside the tree: the node the address names where it is neither an
 *   ask nor a decision, an outline row `[data-node-row]` under no heading and in no part.
 * - **Each part** `[data-decisions-part="open_questions" | "review" | "yours" | "agents"]` with
 *   its heading `[data-decisions-heading]` and count `[data-decisions-count]`, its rows the
 *   outline's own `[data-node-row]`; another person's entry stands in `yours` with their name.
 *   The foot's control is the `button` in `[data-decisions-foot]`.
 * - **Keep and Change** `button[data-row-action="keep" | "change"]` on each row of the review
 *   part, drawn with no focus on the row.
 * - **A decision's "informs" line**, each statement it shaped a link in
 *   `[data-decision-statement]`, named as the body names it (its `statementNames`).
 * - **The Decisions control's flag** `[data-preset-flag="to_review"]` in D's view row, whose
 *   number is how many decisions wait for the reader's review.
 *
 * `seedDecisionsLayout` is c-layout's seed: the whole issue, two long questions and a short one
 * whose words hold a link.
 */

/** The selectors the readers pass into the browser. */
const SELECTORS = {
  body: "[data-issue-page] [data-decisions-body]",
  issuePage: "[data-issue-page]",
} as const;

/** One part of the Decisions view, as the page draws it. */
export interface DrawnPart {
  readonly count: string;
  readonly heading: string;
  readonly part: string;
  readonly rows: readonly string[];
}

/** Each part of the Decisions view, in the order it is drawn. */
export function decisionsPartsDrawn(page: Page): Promise<DrawnPart[]> {
  return page.evaluate((selector) => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    return [...document.querySelectorAll(`${selector} [data-decisions-part]`)].map((part) => ({
      count: text(part.querySelector("[data-decisions-count]")),
      heading: text(part.querySelector("[data-decisions-heading]")),
      part: part.getAttribute("data-decisions-part") ?? "",
      rows: [...part.querySelectorAll("[data-node-row]")].map(
        (row) => row.getAttribute("data-node-id") ?? ""
      ),
    }));
  }, SELECTORS.issuePage);
}

/** The id of each row one part of the Decisions view draws, in the order it draws them: none
 *  where the part is not drawn. */
export async function decisionsPartRows(page: Page, part: string): Promise<readonly string[]> {
  return (await decisionsPartsDrawn(page)).find((one) => one.part === part)?.rows ?? [];
}

/** The Decisions view's list, which holds every row and part the view draws. */
export function decisionsBody(page: Page): Locator {
  return issuePage(page).locator("[data-decisions-body]");
}

/** One part of the Decisions view, by its `data-decisions-part`. */
export function decisionsPart(page: Page, part: string): Locator {
  return decisionsBody(page).locator(`[data-decisions-part="${part}"]`);
}

/** One row of the Decisions view: its node, the part it stands in - null for the row of a node
 *  an address names, which stands in none - and the Keep and Change controls it draws. */
export interface ListRow {
  readonly actions: readonly string[];
  readonly id: string;
  readonly part: string | null;
}

/** Every row of the Decisions view, in the order it is drawn. */
export function decisionsRows(page: Page): Promise<ListRow[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.body} [data-node-row]`)].map((row) => ({
        actions: [...row.querySelectorAll("button[data-row-action]")].map(
          (control) => control.getAttribute("data-row-action") ?? ""
        ),
        id: row.getAttribute("data-node-id") ?? "",
        part: row.closest("[data-decisions-part]")?.getAttribute("data-decisions-part") ?? null,
      })),
    SELECTORS
  );
}

/** The words of each row of the Decisions view that draw the source they were written in: a
 *  typed block's `:::` fence or a `dispatch://` address, where the page names a reference by
 *  plain words (the tester's F20). */
export function decisionsWordsWithSource(page: Page): Promise<string[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.body} [data-node-words]`)]
        .map((words) => (words.textContent ?? "").replace(/\s+/gu, " ").trim())
        .filter((words) => words.includes(":::") || words.includes("dispatch://")),
    SELECTORS
  );
}

/** The words each decision of the Decisions view names a statement it shaped by, on its
 *  "informs" line, in the order they are drawn. */
export function decisionStatementNames(page: Page): Promise<string[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.body} [data-decision-statement] a`)].map((link) =>
        (link.textContent ?? "").replace(/\s+/gu, " ").trim()
      ),
    SELECTORS
  );
}

/** How one control is drawn, as computed: its fill, its words' colour, and each side of its
 *  border, top, right, bottom and left. */
export interface ControlLook {
  readonly background: string;
  readonly borderColors: readonly string[];
  readonly borderWidths: readonly string[];
  readonly color: string;
}

/** How `control` is drawn, as computed. */
export function controlLook(control: Locator): Promise<ControlLook> {
  return control.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      background: style.backgroundColor,
      borderColors: [
        style.borderTopColor,
        style.borderRightColor,
        style.borderBottomColor,
        style.borderLeftColor,
      ],
      borderWidths: [
        style.borderTopWidth,
        style.borderRightWidth,
        style.borderBottomWidth,
        style.borderLeftWidth,
      ],
      color: style.color,
    };
  });
}

/** What the browser computes for a border written `width` wide. Chromium rounds a border's
 *  width down to whole device pixels before it is read (1.5px reads 1px at a device scale of 1),
 *  so a control's computed border is compared with this and not with the width as written. */
export function computedBorderWidth(page: Page, width: string): Promise<string> {
  return page.evaluate((written) => {
    const probe = document.createElement("i");
    probe.style.border = `${written} solid`;
    document.body.append(probe);
    const computed = getComputedStyle(probe).borderTopWidth;
    probe.remove();
    return computed;
  }, width);
}

/** The control at the Decisions view's foot, which unfolds the agents' untied decisions. */
export function decisionsFoot(page: Page): Locator {
  return issuePage(page).locator("[data-decisions-foot] button");
}

/** The number the Decisions control of D's view row draws before "to review", where it draws
 *  the five controls apart (from 1280 px up). It is not drawn where nothing waits. */
export function reviewFlag(page: Page): Locator {
  return presetRow(page).locator('[data-preset-flag="to_review"] [data-author-text]');
}

/** What `seedDecisionsLayout` wrote beside the whole issue. */
export interface DecisionsLayoutSeed {
  /** A question of 160 words alice answered, which the view cuts at six lines. */
  readonly answered: string;
  readonly key: string;
  /** A question of 160 words that waits on alice, which the view draws whole. */
  readonly waiting: string;
}

/** `seedWholeIssue` and the checkpoint's two long questions of its c-row row, each asked by the
 *  planner: one answered by alice in a short line of her own, and one that waits on her; and a
 *  short question of the planner's whose words hold a markdown link. */
export async function seedDecisionsLayout(): Promise<DecisionsLayoutSeed> {
  const seeded = await seedWholeIssue();
  const answered = await createAsk(
    seeded.key,
    { question: longWords("asked", 160), urgency: "high" },
    asPlanner
  );
  await answerAsk(answered.id, {
    expected_edited_at: null,
    selected: [],
    text: "The first of the two.",
  });
  const waiting = await createAsk(
    seeded.key,
    { question: longWords("waits", 160), urgency: "high" },
    asPlanner
  );
  // The view draws a question whole only where it waits on the reader, so the seed proves it
  // does before a row reads the cut.
  expect(
    (await getAsk(waiting.id)).ask.waiting_on_person,
    "the seeded open question does not wait on the reader"
  ).toBe(READER);
  // A link in a row's words, which G's group reading reads the colours of: the whole issue's
  // questions and decisions hold none, and every reference in their words but an issue's is
  // drawn as a chip, which that reading leaves out.
  await createAsk(
    seeded.key,
    {
      question: "Read [the notes](https://example.com/notes) first. Do they hold?",
      urgency: "med",
    },
    asPlanner
  );
  return { answered: answered.id, key: seeded.key, waiting: waiting.id };
}
