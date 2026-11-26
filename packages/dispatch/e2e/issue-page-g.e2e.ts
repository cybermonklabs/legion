import type { Locator, Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  GLYPH_TOKENS,
  issueStatusWord,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
} from "../web/src/features/issue-page/page-words";
import { getAsk, getIssue, listComments } from "./api";
import { nextFrames } from "./frames";
import {
  DESKTOP,
  type DrawnRow,
  drawnRows,
  expectIssuePageDrawn,
  expectOpeningLanded,
  expectRowGroups,
  expectRowLayouts,
  expectSeedDrawn,
  focusRow,
  focusThePage,
  LONG_ROWS,
  leftOutOfConversation,
  nodeRow,
  nodeRows,
  ONE_LINE_WORDS,
  openIssue,
  PHONE,
  PLANNER_TITLE,
  parentOfRow,
  phase,
  READER,
  type RowSeed,
  rowAct,
  rowBody,
  rowGroups,
  rowLayouts,
  rowMore,
  rowOf,
  rowWords,
  seedLongRows,
  seedLongStatement,
  seedRows,
  seedWideNote,
  unfoldEveryBranch,
  WIDE_NOTE_COLUMNS,
} from "./issue-page-g";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

// What this file's init script keeps on the page's window.
declare global {
  interface Window {
    /** The long row in the first view at each frame from the first it drew its words in. */
    __firstViewFrames?: FirstViewFrame[];
  }
}

/**
 * Part G of the new issue page, "A node's row": one node of an issue
 * drawn as one row of the approved design, in the new page's body, which draws the
 * conversation's rows with every branch unfolded until part I's outline replaces them. Each row
 * is one `check` block of part G's Verification document,
 * named by its `-g` string, and reads the page through `e2e/issue-page-g.ts`; every row unfolds
 * each folded branch before it reads, so it holds on part I's outline too.
 *
 * The rows are on the real stack: the page draws what the server stored and indexed - a
 * document's blocks, the decisions read, the batched names - and the new page is the reader's
 * own preference (`withIssuePage`).
 */

/** An address, the short form of one, or a slug, which no chip, author or tag draws (R4). */
function addressesOf(seeded: RowSeed): RegExp {
  return new RegExp(`dispatch://|${seeded.key} \\S+( block)?$|-md\\b|session:`, "u");
}

/** An author as a row's group draws it: the name and the `data-glyph` token of its shape. */
interface DrawnAuthor {
  readonly author: string;
  readonly shape: string;
}

/** The kinds of node the model holds no author for, whose rows draw none (R3, R5). A decision is
 *  none of them: its row draws who decided it, as the decisions read's entry names them. */
const AUTHORLESS: readonly string[] = ["check", "child_issue", "plan_task", "requirement"];

/** Each row that draws an author other than the one `stored` gives its node, or that draws one
 *  where its kind has none. A row of neither, which the seed gave no known author, is left out. */
function authorOffences(
  rows: readonly DrawnRow[],
  stored: ReadonlyMap<string, DrawnAuthor>
): string[] {
  return rows.flatMap((row) => {
    const want =
      stored.get(row.id ?? "") ?? (AUTHORLESS.includes(row.type ?? "") ? null : undefined);
    if (want === undefined) return [];
    const drawn = row.author === null ? null : { author: row.author, shape: row.authorShape };
    return JSON.stringify(drawn) === JSON.stringify(want)
      ? []
      : [`the ${row.type} ${row.id} draws ${JSON.stringify(drawn)}, not ${JSON.stringify(want)}`];
  });
}

/** How many rendering frames the page's writer goes on looking for its field in after a Reply or
 *  an Answer (`DRAWING_FRAMES` in `outline-writer.tsx`: ten after the first), and one more. */
const WRITER_FRAMES = 12;

/** Waits out those frames, two at a time. */
async function writerFramesPass(page: Page): Promise<void> {
  for (let passed = 0; passed < WRITER_FRAMES; passed += 2) await nextFrames(page);
}

// c-row-layout: every row at both widths, read off the page's own layout (R1, R4, R5, R8).
// SLOW(>=2s): a seeded issue of every kind of node, loaded at two widths with each load waited
// out to a 1.5 s quiet window, a third load read row by row, and a picture opened.
test("a node draws its kind at the right of its row and every node starts at one left edge @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedRows, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize(size);
      const settling = watchApiRequests(page);
      await openIssue(page, seeded.key);
      await expectLoadSettled(settling);
      const where = `the conversation at ${size.width} px`;
      if (size.width === PHONE.width) {
        // The outline's load puts the focus on the first question that waits on the reader, in
        // the outline's order, and holds its row on screen: the seed holds two such.
        const waiting = [seeded.waitingLong, seeded.openQuestion];
        const first = (await drawnRows(page)).find((row) => waiting.includes(row.id ?? ""));
        expect(first, `${where} draws no question that waits on the reader`).toBeDefined();
        const question = first?.id ?? "";
        await expect(
          nodeRow(page, question).first(),
          `${where} does not focus that question at load`
        ).toBeFocused();
        // A waiting question's words are drawn whole and the focused row draws its body, so the
        // row of the seed's 160 words is taller than the screen, which holds it from its top: at
        // or below what stays at the top of the screen (the view row, at 390 px) and above the
        // screen's foot. Read against the window's top alone, a row under that band passed.
        await expect
          .poll(
            async () => {
              const row = nodeRow(page, question).first();
              const below = await belowWhatStays(row);
              const top = (await row.boundingBox())?.y;
              return below !== undefined && below >= 0 && top !== undefined && top < size.height;
            },
            {
              message: `${where} holds the top of that question's row under what stays at the top or off screen`,
            }
          )
          .toBe(true);
      }
      await expectSeedDrawn(page, seeded);
      await expect
        .poll(() => nodeRows(page).count(), { message: `${where} draws no row` })
        .toBeGreaterThan(0);
      const layouts = await rowLayouts(page, seeded.slugs.spec);
      expectRowLayouts(layouts, where, true, size.width);
      // Decision 246: the spec's requirement is a statement and the requirement inside the
      // plan's decision is a block, so the view draws a bullet of each colour, each on its own
      // row, for the check above to read.
      expect(
        layouts
          .filter((row) => row.type === "requirement" && !row.context)
          .map((row) => [row.id, row.statement]),
        `${where} draws no statement beside a requirement of another document`
      ).toEqual(
        expect.arrayContaining([
          [`${seeded.slugs.spec}#b-r-one`, true],
          [`${seeded.slugs.plan}#b-r1`, false],
        ])
      );
      expectRowGroups(await rowGroups(page), where, size.width, addressesOf(seeded));
      // R17 of the root, F17: the rows name what their words cite from the one batched read.
      expect(
        settling.requests
          .map((request) => request.path)
          .filter(
            (path) =>
              path.includes(`/issues/${seeded.child}/summary`) || path.includes("/issues/UTF-16")
          ),
        `${where}: a row reads a target of its own for a reference its words cite`
      ).toEqual([]);
    }

    await page.setViewportSize(DESKTOP);
    await openIssue(page, seeded.key);
    await expectSeedDrawn(page, seeded);
    const rows = await drawnRows(page);
    // No row for the issue, which the header names (R8).
    expect(
      rows.filter((row) => row.type === "issue"),
      "the body draws the issue's own row"
    ).toEqual([]);
    // Conversation draws its read alone: no result, plan task or message, and no comment on a
    // node's text, which is drawn beside that node.
    const leftOut = leftOutOfConversation(seeded);
    expect(
      rows.filter((row) => leftOut.includes(row.id ?? "")).map((row) => row.id),
      "Conversation draws a row its read does not hold"
    ).toEqual([]);
    // The statement's words take one line. Its face and acts are now drawn underneath even
    // without focus, so the old 29 px limit for the whole closed card no longer applies.
    const oneLine = rowWords(nodeRow(page, `${seeded.slugs.spec}#b-r-one`).first());
    await expect(oneLine).toBeVisible();
    expect((await oneLine.boundingBox())?.height, "the statement is taller than one line").toBe(24);

    // A child: its status, what blocks it and who holds it, and no author (R5).
    const child = nodeRow(page, seeded.child).first();
    const facts = child.locator("[data-child-facts]");
    await expect(facts).toContainText(issueStatusWord((await getIssue(seeded.child)).status));
    await expect(facts).toContainText(seeded.blocker);
    await expect(facts).toContainText(PROPERTY_LABELS.blocked_by);
    await expect(facts.locator("a", { hasText: seeded.blocker })).toHaveCount(1);
    await expect(
      facts.locator("[data-child-holder]"),
      "the child's holder is drawn in a link"
    ).not.toContainText("session:");
    await expect(
      facts.locator("[data-child-holder] a"),
      "the child's holder is a link"
    ).toHaveCount(0);
    await expect(child.locator("[data-node-margin] [data-node-author]")).toHaveCount(0);

    // `r-cites`: the document it cites by the page's word, the child it cites by its key, among
    // its bold words and its two paragraphs (R4, F14).
    const cites = nodeRow(page, `${seeded.slugs.spec}#b-r-cites`).first();
    await expect(cites.locator("a[data-row-chip]").first()).toHaveText(NODE_TYPE_NAMES.document);
    await expect(rowWords(cites)).not.toContainText("notes.md");
    await expect(cites.locator(`a[data-row-reference="dispatch://${seeded.child}"]`)).toHaveText(
      new RegExp(`^${seeded.child}\\b`, "u")
    );
    await expect(rowWords(cites).locator("strong")).toHaveCount(1);
    await expect(rowWords(cites).locator("p")).toHaveCount(2);

    // `r-ties`: its words cite r-one's block by its address, which they draw as a chip in place
    // naming r-one by its first words, with no source line, no mark and no address (R4).
    const ties = rowWords(nodeRow(page, `${seeded.slugs.spec}#b-r-ties`).first());
    await expect(ties.locator("a[data-row-chip]")).toHaveCount(1);
    await expect(ties.locator("a[data-row-chip]")).toHaveText(ONE_LINE_WORDS);
    for (const source of [":::", "**", "dispatch://"]) {
      await expect(ties, `r-ties' words draw "${source}"`).not.toContainText(source);
    }

    // The decision that quotes a typed block draws none of its source, and its reference is a
    // chip in place, which names r-one by its first words (F20).
    const raw = rowOf(rows, seeded.raw);
    expect(raw?.text ?? ":::", "the decision draws a typed block's source").not.toContain(":::");
    expect(raw?.text ?? "dispatch://", "the decision draws an address").not.toContain(
      "dispatch://"
    );
    const rawChip = rowWords(nodeRow(page, seeded.raw).first()).locator("a[data-row-chip]");
    await expect(rawChip).toHaveCount(1);
    await expect(rawChip).toHaveText(ONE_LINE_WORDS);

    // A chip in a node's words names its target in plain words, as the chip at the end of the
    // words does: the unsure decision cites t1, whose words are bold and open on "1\." (R4, F18).
    const cited = rowWords(nodeRow(page, seeded.unsure).first()).locator("a[data-row-chip]");
    await expect(cited).toHaveCount(1);
    await expect(cited).toHaveText(/^1\. Stand/u);
    await expect(cited, "a chip in a decision's words draws a mark of markdown").not.toHaveText(
      /[*\\]/u
    );

    // A picture is 84 px tall, and a press opens it whole (R4).
    const picture = nodeRow(page, seeded.pictured).first().locator("[data-node-words] img");
    await expect(picture).toBeVisible();
    expect(
      Math.round((await picture.boundingBox())?.height ?? 0),
      "the picture is not 84 px tall"
    ).toBe(84);
    await picture.click();
    await expect(
      page.locator("dialog[open] img"),
      "a press does not open the picture whole"
    ).toBeVisible();
    await page.keyboard.press("Escape");

    // A fold control folds its branch to one line with its count, and a second press unfolds it.
    // The row is named by its node's id: a locator that filters on `aria-expanded` would move to
    // the next unfolded row once this one folds.
    const unfolded = await nodeRows(page)
      .filter({ has: page.locator(':scope[aria-expanded="true"]') })
      .first()
      .getAttribute("data-node-id");
    expect(unfolded, "no row holds an unfolded branch").not.toBeNull();
    const parent = nodeRow(page, unfolded ?? "").first();
    await parent.locator("button[data-node-bullet]").click();
    await expect(parent).toHaveAttribute("aria-expanded", "false");
    await expect(parent.locator("[data-node-count]")).toHaveCount(1);
    // The press takes no focus, so the folded row draws its one line and not its words whole.
    await expect(
      parent.locator("[data-node-first-line]"),
      "the folded branch is not drawn on one line"
    ).toHaveCount(1);
    await parent.locator("button[data-node-bullet]").click();
    await expect(parent).toHaveAttribute("aria-expanded", "true");

    // At 390 px a focused comment's Reply is under 44 px (R1).
    await page.setViewportSize(PHONE);
    const comment = await focusRow(page, seeded.comment);
    expect((await rowAct(comment, "reply").boundingBox())?.height ?? 1e6).toBeLessThan(44);

    // Graph draws the whole tree read, the plan's tasks and the messages Conversation leaves out
    // among it, each as Conversation draws a node. The needs chip names its target by its first
    // words, with no mark of markdown (F18); an agent's link, and a word that only looks like a
    // key (R4).
    await page.setViewportSize(DESKTOP);
    await page.goto(presetPath(seeded.key, "graph"));
    const needs = nodeRow(page, `${seeded.slugs.plan}#b-t2`).first().locator("[data-node-chip]");
    await expect(needs, "t2 draws other than its one needs chip in Graph").toHaveCount(1);
    await expect(needs.locator("[data-chip-name]")).toHaveText(/^1\. Stand/u);
    await expect(needs, "the needs chip draws a mark of markdown").not.toHaveText(/[*\\]/u);
    const linked = nodeRow(page, seeded.linked).first();
    await expect(rowWords(linked)).toContainText("in UTF-16.");
    await expect(linked.locator("a", { hasText: "UTF-16" })).toHaveCount(0);
  });
});

/** How far `row`'s top stands below the bottom of the band that stays at the top of the screen
 *  above the rows, in CSS pixels: the view row at 390 px, the sticky box that holds the view
 *  controls (`[data-filter-row]`), as part D's padding row finds it. Undefined while nothing
 *  above the rows stays. */
async function belowWhatStays(row: Locator): Promise<number | undefined> {
  return row.evaluate((element) => {
    let band = document.querySelector("[data-filter-row]");
    while (band !== null && getComputedStyle(band).position !== "sticky") {
      band = band.parentElement;
    }
    if (band === null) return undefined;
    return element.getBoundingClientRect().top - band.getBoundingClientRect().bottom;
  });
}

// Decision 113: a focused row stands below everything that stays at the top of the screen. At
// 390 px the view row stays there. A row whose top lies 10 px under it takes the focus with no
// scroll of the browser's own (`preventScroll`, as a chord or an address puts the focus), so
// only the row's own placement (`useRowOnScreen`) can move it: under part G the header publishes
// no band, and `bringOnScreen` alone leaves the row where it is. Then `t s` puts the focus on the
// Spec view's tab-stop row from the page's foot, which the chord shows and the row places: the
// Spec view draws its statements as rows, so `t s` alone reaches one.
// SLOW(>=2s): a seeded issue of every kind of node, its rows and their names waited for, a
// focus and a chord each waited out.
test("at 390 px a focused row stands below the view row that stays at the top, after a focus that does not scroll and after t s", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedRows, async (page, seeded) => {
    await page.setViewportSize(PHONE);
    await openIssue(page, seeded.key);
    await expectSeedDrawn(page, seeded);
    await focusThePage(page);
    // A row with room below it to be scrolled up to the view row, scrolled until its top lies
    // 10 px under the view row's bottom.
    const id = await page.evaluate(() => {
      let band = document.querySelector("[data-filter-row]");
      while (band !== null && getComputedStyle(band).position !== "sticky") {
        band = band.parentElement;
      }
      const stays = band?.getBoundingClientRect().height ?? 0;
      const room = document.documentElement.scrollHeight - window.innerHeight;
      for (const row of document.querySelectorAll<HTMLElement>("[data-node-row]")) {
        const top = row.getBoundingClientRect().top + window.scrollY;
        const at = top - stays + 10;
        if (top > window.innerHeight && at < room) {
          window.scrollTo(0, at);
          return row.getAttribute("data-node-id");
        }
      }
      return null;
    });
    expect(id, "no row has room below it to be scrolled under the view row").not.toBeNull();
    const row = nodeRow(page, id ?? "").first();
    await nextFrames(page);
    expect(
      (await belowWhatStays(row)) ?? 0,
      "the row was not scrolled under the view row"
    ).toBeLessThan(0);
    await row.evaluate((element) => (element as HTMLElement).focus({ preventScroll: true }));
    await expect(row).toBeFocused();
    await expect
      .poll(async () => (await belowWhatStays(row)) ?? -1, {
        message: "a focused row's top lies under the view row that stays at the top",
      })
      .toBeGreaterThanOrEqual(0);

    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(page, "t s did not open Spec").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "spec")}$`)
    );
    const first = page.locator("[data-node-row]:focus");
    await expect(first, "t s focused no row").toHaveCount(1);
    await expect
      .poll(async () => (await belowWhatStays(first)) ?? -1, {
        message: "after t s the focused row's top lies under the view row that stays at the top",
      })
      .toBeGreaterThanOrEqual(0);
  });
});

// Decision 282: at 390 px a note whose body holds a table of sixteen columns, drawn in its row and
// not edited, made the whole page 794 px wide. A wide block in a node's words scrolls sideways
// inside itself, as a table does in a document's reading: the page is no wider than the screen,
// and the table's own scroll still reaches its last column. A note of the spec is no kind
// Conversation's read holds, so Conversation draws no row of it, and the row reads it in Graph,
// which draws the whole tree read.
// SLOW(>=2s): an issue with one note seeded and loaded at 390 px twice, and its table waited for
// until the batched read that carries a block's words draws it.
test("at 390 px a note's table of sixteen columns scrolls inside its row, and the page is no wider than the screen", async ({
  browser,
}) => {
  test.setTimeout(60_000);
  await phase(browser, seedWideNote, async (page, seeded) => {
    await page.setViewportSize(PHONE);
    const settling = watchApiRequests(page);
    await page.goto(`/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectLoadSettled(settling);
    await expect(nodeRow(page, seeded.note), "Conversation draws the spec's note").toHaveCount(0);
    await page.goto(presetPath(seeded.key, "graph"));
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    const table = rowWords(nodeRow(page, seeded.note)).locator("table");
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return table.locator("th").count();
        },
        { message: "the note's row never drew the heads of its table" }
      )
      .toBe(WIDE_NOTE_COLUMNS);
    await expect(table, "the note's table is not drawn").toBeVisible();
    // The poll above waited until the row drew the whole table, and a read of the page's widths
    // lays the page out first, so one read gives the widths the table leaves the page.
    const widths = await page.evaluate(() => ({
      client: document.documentElement.clientWidth,
      scroll: document.documentElement.scrollWidth,
    }));
    expect(widths.client, "the page is laid out wider than the screen").toBeLessThanOrEqual(
      PHONE.width
    );
    expect(widths.scroll, "the note's table makes the page wider than the screen").toBe(
      widths.client
    );
    // The table is drawn whole and a read of its widths lays it out first, and the scroll this
    // sets is read back in the same task, where nothing else can move it, so one read holds.
    const own = await table.evaluate((element) => {
      const box = { client: element.clientWidth, scroll: element.scrollWidth };
      element.scrollLeft = element.scrollWidth;
      return { ...box, left: element.scrollLeft };
    });
    expect(
      own.scroll,
      "the table is no wider than its own box, so it has nothing to scroll"
    ).toBeGreaterThan(own.client);
    expect(own.left, "the table does not scroll sideways to its last column").toBeGreaterThan(0);
  });
});

// c-cut: ruling 1 on R2 at 1440 px, as every node is now drawn whole, with no limit of lines.
// SLOW(>=2s): a seeded issue of every kind of node, its rows and their names waited for, then
// a few focus moves, each waited on, and Graph loaded for the agent's message.
test("an agent message and an answered question are drawn whole, as the words of the reader and a waiting question are", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedRows, async (page, seeded) => {
    await openIssue(page, seeded.key);
    await expectSeedDrawn(page, seeded);
    // Each row read here is brought into view first, so it is read where the reader reads it.
    const layoutOf = async (id: string) => {
      await nodeRow(page, id).first().scrollIntoViewIfNeeded();
      return (await rowLayouts(page, seeded.slugs.spec)).find((row) => row.id === id);
    };
    expect(await layoutOf(seeded.answeredLong), "the answered question is cut").toMatchObject({
      more: null,
      wordsHidden: 0,
    });
    expect(
      await layoutOf(seeded.longComment),
      "the reader's own long comment is cut"
    ).toMatchObject({
      more: null,
      own: true,
      wordsHidden: 0,
    });
    await expect(nodeRow(page, seeded.answered).first().locator("[data-answer-text]")).toHaveText(
      seeded.answerWords,
      { useInnerText: true }
    );
    expect(
      await nodeRow(page, seeded.answered)
        .first()
        .locator("[data-answer-text]")
        .evaluate((text) => text.scrollHeight - text.clientHeight),
      "the reader's answer hides a word"
    ).toBeLessThanOrEqual(1);
    expect((await getAsk(seeded.waitingLong)).ask.waiting_on_person).toBe(READER);
    expect(await layoutOf(seeded.waitingLong), "the waiting question is cut").toMatchObject({
      more: null,
      wordsHidden: 0,
    });
    await expect(
      rowMore(nodeRows(page)),
      "a row of Conversation draws a control for more words"
    ).toHaveCount(0);

    // Focused, the answered question is whole, with no more, on the focused card.
    const answered = await focusRow(page, seeded.answeredLong);
    await expect(rowMore(answered)).toHaveCount(0);
    expect(
      await rowWords(answered).evaluate((words) => words.scrollHeight - words.clientHeight),
      "the focused answered question hides a word"
    ).toBeLessThanOrEqual(1);
    await expect(answered.locator("[data-node-card]").first()).toHaveCSS(
      "background-color",
      "rgb(250, 250, 249)"
    );

    // A statement with no author says so in its body, not on its row (R3).
    const statement = await focusRow(page, `${seeded.slugs.spec}#b-r-one`);
    await expect(rowBody(statement).locator("[data-node-author]")).toHaveText(
      CONTROL_LABELS.unknown_author
    );
    await expect(statement.locator("[data-node-margin] [data-node-author]")).toHaveCount(0);

    // A row draws its body whatever the focus, so no row draws Keep open, and the statement's
    // body stays when the focus moves on.
    await expect(
      nodeRows(page).getByRole("button", { name: CONTROL_LABELS.keep_open }),
      "a row draws Keep open"
    ).toHaveCount(0);
    await focusRow(page, seeded.comment);
    await expect(
      rowBody(statement),
      "the statement's body closed as the focus left it"
    ).toHaveCount(1);

    // No element of a row carries a `title` (R6).
    expect(await nodeRows(page).locator("[title]").count()).toBe(0);

    // The agent's long message is no row of Conversation, whose read holds no message; Graph
    // draws it, whole as every node.
    await page.goto(presetPath(seeded.key, "graph"));
    await expect(rowWords(nodeRow(page, seeded.longMessage).first())).toContainText("plan160.");
    expect(await layoutOf(seeded.longMessage), "the agent's message is cut").toMatchObject({
      more: null,
      wordsHidden: 0,
    });
  });
});

/** What a jump read in the task that made it, before the page could measure any row it landed
 *  on. */
interface Jump {
  /** The top of the words of the row the jump put across the view's top. */
  readonly crossingTop: number;
  /** Whether that row or `first` was drawn cut already. */
  readonly cut: boolean;
  /** The first row wholly in view, and its top. */
  readonly first: string | null;
  readonly top: number;
  /** Every row in view, by node id, top to bottom. */
  readonly inView: readonly string[];
}

/** Opens issue `key` at `size` and waits until it draws every long row `seedLongRows` wrote,
 *  with every branch open and every page of the head comment's replies drawn: an outline draws a
 *  node's first 50 children, and a control after them that draws the next 50 (R15). */
async function openLongRows(
  page: Page,
  key: string,
  size: { height: number; width: number },
  where: string
): Promise<void> {
  await page.setViewportSize(size);
  await openIssue(page, key);
  const nextPage = page.locator("[data-issue-page] [data-tree-page] button");
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        // The poll reads the rows again after this press, so a control drawn after this count
        // is pressed on the poll's next pass rather than missed.
        if ((await nextPage.count()) > 0) await nextPage.first().click();
        return nodeRows(page).count();
      },
      { message: `${where} the page never drew every long row` }
    )
    .toBeGreaterThan(LONG_ROWS);
  await expect(nextPage, `${where} a page of long rows is still held back`).toHaveCount(0);
}

/** Jumps the page, as a drag of the scrollbar does, to 40 px above the foot of the words of row
 *  `crossing`, and reads where the rows stand in the same task. */
function jumpAbove(page: Page, crossing: string): Promise<Jump> {
  return page.evaluate((id): Jump => {
    const row = document.querySelector(`[data-node-row][data-node-id="${id}"]`);
    const words = row?.querySelector("[data-node-words]");
    if (row == null || words == null) throw new Error(`the row of ${id} is not drawn`);
    window.scrollTo(0, words.getBoundingClientRect().bottom + window.scrollY - 40);
    const boxes = [...document.querySelectorAll("[data-node-row]")].map(
      (drawn) => [drawn, drawn.getBoundingClientRect()] as const
    );
    const first = boxes.find(([, box]) => box.top >= 0 && box.bottom <= window.innerHeight);
    return {
      crossingTop: words.getBoundingClientRect().top,
      cut: [row, first?.[0]].some((drawn) => drawn?.querySelector("[data-node-more]") != null),
      first: first?.[0].getAttribute("data-node-id") ?? null,
      inView: boxes
        .filter(([, box]) => box.bottom > 0 && box.top < window.innerHeight)
        .map(([drawn]) => drawn.getAttribute("data-node-id") ?? ""),
      top: first?.[1].top ?? Number.NaN,
    };
  }, crossing);
}

/** How many frames a jump's reading is held for: a second of the page's own frames, in which a
 *  row that changes its height after the jump would move the rows under it. */
const HELD_FRAMES = 60;

/** Waits out `HELD_FRAMES` frames, and fails unless the first row wholly in view that `jump`
 *  read stands within a pixel of where it stood, and unless it and row `crossing`, which the jump
 *  left across the view's top, are drawn whole: a row in view keeps its length. */
async function expectFirstHeld(
  page: Page,
  crossing: string,
  jump: Jump,
  where: string
): Promise<void> {
  await page.evaluate(async (count) => {
    for (let frame = 0; frame < count; frame += 1) {
      const next = Promise.withResolvers<void>();
      requestAnimationFrame(() => next.resolve());
      await next.promise;
    }
  }, HELD_FRAMES);
  const first = nodeRow(page, jump.first ?? "");
  const top = await first.evaluate((row) => row.getBoundingClientRect().top);
  expect(
    Math.abs(top - jump.top),
    `${where} the first row wholly in view moved from ${jump.top} px to ${top} px`
  ).toBeLessThanOrEqual(1);
  await expect(rowMore(first), `${where} the first row wholly in view was cut`).toHaveCount(0);
  await expect(
    rowMore(nodeRow(page, crossing)),
    `${where} the row across the view's top was cut`
  ).toHaveCount(0);
}

/** The planner's row that holds the page's middle, of the rows `rows` names. */
async function middleRow(page: Page, rows: readonly string[], where: string): Promise<string> {
  const middle = await page.evaluate(
    (ids) =>
      ids.find((id) => {
        const words = document.querySelector(
          `[data-node-row][data-node-id="${id}"] [data-node-words]`
        );
        const half = document.documentElement.scrollHeight / 2;
        return words !== null && words.getBoundingClientRect().bottom + window.scrollY >= half;
      }) ?? null,
    rows
  );
  expect(middle, `${where} no long row holds the page's middle`).not.toBeNull();
  return middle ?? "";
}

// A long jump into the middle of the page - a fast drag of the scrollbar - lands on rows no
// frame has laid out yet, and the first row wholly in view stays where it stood.
// At each width the row jumps to 40 px above the foot of the words of the row that holds the
// page's middle: that row crosses the view's top, and the one under it is the first row wholly
// in view. Then it jumps to 40 px above the foot of the words of the planner's row right above
// the reader's own run of long rows: the planner's row crosses the view's top, and the reader's
// rows alone stand in view under it. After each jump the first row wholly in view stands where
// it stood, within a pixel, for a second of frames, and both rows are drawn whole. It carries
// `@phone` because the phone project lays the page out at three device pixels to the CSS pixel,
// where the scroll that holds the row snaps to a device pixel.
// SLOW(>=2s): sixty-six long rows seeded, loaded at two widths, and two jumps into them at each,
// each held for a second of frames.
test("a long jump into the middle of the page keeps the first row wholly in view where it stood @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
    const mine = seeded.own[0] ?? "";
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await openLongRows(page, seeded.key, size, where);
      const crossing = await middleRow(page, seeded.rows, where);
      const jump = await jumpAbove(page, crossing);
      const at = seeded.rows.indexOf(crossing);
      expect(
        jump,
        `${where} the jump did not land with a whole row crossing the view's top above the first row wholly in view`
      ).toMatchObject({ cut: false, first: seeded.rows[at + 1] });
      expect(jump.crossingTop, `${where} the crossing row's words start in view`).toBeLessThan(0);
      await expectFirstHeld(page, crossing, jump, where);

      const own = await jumpAbove(page, seeded.aboveOwn);
      expect(
        own,
        `${where} the jump did not land with the planner's whole row across the view's top`
      ).toMatchObject({ cut: false, first: mine });
      expect(own.crossingTop, `${where} the planner's row's words start in view`).toBeLessThan(0);
      expect(
        own.inView,
        `${where} a row other than the reader's own stands in view under the planner's`
      ).toEqual([seeded.aboveOwn, ...seeded.own.slice(0, own.inView.length - 1)]);
      await expectFirstHeld(page, seeded.aboveOwn, own, `${where}, landing on the reader's rows,`);
    }
  });
});

/** What a row read of the long row in the first view at each frame from the first it drew its
 *  words in, from a script that runs before the page's own. */
interface FirstViewFrame {
  readonly cut: boolean;
  readonly height: number;
  readonly top: number;
  readonly view: number;
}

// R11: a row that stands in view as the page opens is drawn whole on the first frame it is
// drawn in, and never changes its height in view after. At each width a
// script that runs before the page's own reads the planner's long statement, the body's second
// row, under its one question (`seedLongStatement`), at each animation frame from the first one
// it is drawn in: it draws no control for more words on that first frame, and its height is the
// same 60 frames later. The row is drawn from the spec's blocks before the reads that bring its
// words, and a row with no words has no height of its words yet, so it is read from the first
// frame it draws its words in. It carries `@phone` because the phone's first view is another set
// of rows, laid out at three device pixels to the CSS pixel.
// SLOW(>=2s): the page loaded at two widths, each watched over 61 frames.
test("a long row in the first view is drawn whole on the first frame it is drawn in, and its height holds 60 frames later @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongStatement, async (page, seeded) => {
    await page.addInitScript((id) => {
      const frames: FirstViewFrame[] = [];
      window.__firstViewFrames = frames;
      const read = () => {
        const row = document.querySelector(`[data-node-row][data-node-id="${id}"]`);
        const words = row?.querySelector("[data-node-words] [data-author-text]");
        if (row !== null && (words?.textContent ?? "").trim() !== "") {
          const box = row.getBoundingClientRect();
          frames.push({
            cut: row.querySelector("[data-node-more]") !== null,
            height: box.height,
            top: box.top,
            view: window.innerHeight,
          });
        }
        if (frames.length <= 60) requestAnimationFrame(read);
      };
      requestAnimationFrame(read);
    }, seeded.statement);
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await page.setViewportSize(size);
      await openIssue(page, seeded.key);
      await page.waitForFunction(() => (window.__firstViewFrames?.length ?? 0) > 60);
      // The wait above saw more than 60 frames recorded, and the script stops recording once it
      // holds 61, so one read holds every frame the checks below read.
      const frames = await page.evaluate(() => window.__firstViewFrames ?? []);
      const [first] = frames;
      if (first === undefined) throw new Error(`${where} the row never drew its words`);
      expect(first.top, `${where} the row was drawn outside the first view`).toBeLessThan(
        first.view
      );
      expect(
        first.cut,
        `${where} the row was drawn cut on the first frame it was drawn in, not whole`
      ).toBe(false);
      expect(
        frames.map((frame) => frame.height).filter((height) => height !== first.height),
        `${where} the row's height changed in the 60 frames after its first`
      ).toEqual([]);
    }
  });
});

// c-author: R3 at 1440 px with every branch unfolded, and at 390 px.
// SLOW(>=2s): a seeded issue of every kind of node, loaded and read at two widths with its rows
// and their names waited for each time, then two rows focused.
test("an author is drawn by name and shape, and an agent's decision by its uncertainty @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedRows, async (page, seeded) => {
    const reader = { author: READER, shape: GLYPH_TOKENS.authors.person };
    // The planner, whom the registry does not list, by the title its session stamped on each
    // write the seed made as it: its messages and asks, and the decisions it wrote, whose
    // decider the decisions read names with that title (`DecisionEntry.decided_by.title`).
    const plannerAuthor = { author: "Planner", shape: GLYPH_TOKENS.authors.agent };
    // An agent the registry does not list and whose own record stamps no title: the recorder of
    // the result, whom the page names by its own word for an agent nothing names.
    const unnamedAgent = {
      author: CONTROL_LABELS.unnamed_author,
      shape: GLYPH_TOKENS.authors.agent,
    };
    const stored = new Map<string, DrawnAuthor>([
      [seeded.comment, reader],
      [seeded.longComment, reader],
      [seeded.pictured, reader],
      [seeded.change, reader],
      ...seeded.replies.map((reply): [string, DrawnAuthor] => [reply, reader]),
      [seeded.linked, plannerAuthor],
      [seeded.longMessage, plannerAuthor],
      [seeded.answered, plannerAuthor],
      [seeded.answeredLong, plannerAuthor],
      [seeded.waitingLong, plannerAuthor],
      [seeded.openQuestion, plannerAuthor],
      [seeded.result, unnamedAgent],
      [seeded.shaping, plannerAuthor],
      [seeded.changed, plannerAuthor],
      [seeded.unsure, plannerAuthor],
      [seeded.raw, plannerAuthor],
    ]);
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize(size);
      await openIssue(page, seeded.key);
      await expectSeedDrawn(page, seeded);
      // A person by their login and an agent by its role's word, or by the page's word for one
      // that names none, each by its kind's shape: people share one, agents another. A decision
      // draws who decided it. A node the model holds no author for draws none.
      await expect
        .poll(async () => authorOffences(await drawnRows(page), stored), {
          message: "a row draws its author other than by the stored actor's name and kind",
        })
        .toEqual([]);
      // The poll above waited until every row drew its stored author, and nothing moved since,
      // so one read lists the rows that poll saw.
      const rows = await drawnRows(page);
      const where = `at ${size.width} px`;
      for (const row of rows) {
        expect(row.author ?? "", `the ${row.type} ${row.id} ${where} draws an id`).not.toMatch(
          /session:|019a1c0a/u
        );
      }
      // The answer is word for word, with no "Unsure"; only the seeded decision says it.
      expect(
        rowOf(rows, seeded.answered)?.answerText,
        `the answer ${where} is not drawn word for word`
      ).toBe(seeded.answerWords);
      expect(rowOf(rows, seeded.answered)?.unsure ?? null).toBeNull();
      expect(rows.filter((row) => row.unsure !== null).map((row) => row.id)).toEqual([
        seeded.unsure,
      ]);
      expect(rowOf(rows, seeded.unsure)?.unsure).toBe(CONTROL_LABELS.unsure);
    }
    await page.setViewportSize(DESKTOP);
    // The planner's open row gives its whole title; a node with no author says so in a third
    // shape. The planner's messages are no rows of Conversation, so its open question stands for
    // the planner there.
    const planner = await focusRow(page, seeded.openQuestion);
    await expect(rowBody(planner).locator("[data-node-author-title]")).toHaveText(PLANNER_TITLE);
    const statement = await focusRow(page, `${seeded.slugs.spec}#b-r-one`);
    const unknown = await rowBody(statement)
      .locator("[data-node-author] svg[data-glyph]")
      .getAttribute("data-glyph");
    // The rows drew their stored authors at both widths above, and since then only the width and
    // the focus moved, which leave each row's margin shape as drawn, so one read gives each one.
    const rows = await drawnRows(page);
    expect(
      new Set([
        rowOf(rows, seeded.comment)?.authorShape,
        rowOf(rows, seeded.openQuestion)?.authorShape,
        unknown,
      ]).size,
      "a person, an agent and an author nobody recorded do not each have a shape of their own"
    ).toBe(3);
    // Graph draws the result and the messages Conversation leaves out, each by its stored author.
    await page.goto(presetPath(seeded.key, "graph"));
    const graphOnly = [seeded.result, seeded.longMessage, seeded.linked, ...seeded.replies];
    await expect
      .poll(
        async () => {
          const drawn = await drawnRows(page);
          return graphOnly.filter((id) => rowOf(drawn, id) === undefined);
        },
        { message: "Graph never drew the result and the messages Conversation leaves out" }
      )
      .toEqual([]);
    await expect
      .poll(async () => authorOffences(await drawnRows(page), stored), {
        message: "a row of Graph draws its author other than by the stored actor's name and kind",
      })
      .toEqual([]);
  });
});

// c-writer: R7 and R10 at 1440 px.
// SLOW(>=2s): a seeded issue of every kind of node, its rows and their names waited for, then
// six rows focused, three presses waited out for the writer's frames, and a read of the API.
test("a row's Reply and Answer set the page's writer, and a decision asked to change draws it", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedRows, async (page, seeded) => {
    await openIssue(page, seeded.key);
    await expectSeedDrawn(page, seeded);
    const writes = watchApiRequests(page);

    // Reply aims the box at the foot (part L's `IssueComposer.tsx`) and puts the caret in its
    // line, the one the box draws: the line that writes on the issue stays mounted, hidden, while
    // the line edits a comment. Answer and a press on an option put no caret in the box, which
    // answers no ask (`OutlineWriter.answer`).
    const boxField = page.locator('[data-testid="issue-composer"] textarea:visible');
    const comment = await focusRow(page, seeded.comment);
    await expect(rowAct(comment, "reply"), "the focused comment draws no Reply").toHaveCount(1);
    await expect(rowAct(comment, "answer")).toHaveCount(0);
    await rowAct(comment, "reply").click();
    await writerFramesPass(page);
    await expect(boxField, "Reply does not put the caret in the box").toBeFocused();
    await expect(comment).not.toBeFocused();

    const question = await focusRow(page, seeded.openQuestion);
    await expect(rowAct(question, "answer")).toHaveCount(1);
    await expect(rowAct(question, "reply")).toHaveCount(1);
    const option = question.locator('[data-pick-option="1024 px"]');
    await option.click();
    await expect(option).toHaveAttribute("aria-pressed", "true");
    await expect(option).toContainText("●");
    await expect(question.locator('[data-pick-option="1280 px"]')).toContainText("○");
    await writerFramesPass(page);
    await expect(
      boxField,
      "a press on an option put the caret in the box, which answers no ask"
    ).not.toBeFocused();
    await rowAct(question, "answer").click();
    await writerFramesPass(page);
    await expect(
      boxField,
      "Answer put the caret in the box, which answers no ask"
    ).not.toBeFocused();
    await expect(question).toBeFocused();

    // A statement takes a Reply, which writes a comment on its text (part L's R11), so it draws
    // Reply; a check takes none in today's model, so it draws no act.
    const statement = await focusRow(page, `${seeded.slugs.spec}#b-r-one`);
    await expect(statement.locator("[data-row-act]")).toHaveCount(1);
    await expect(rowAct(statement, "reply"), "the focused statement draws no Reply").toHaveCount(1);
    const check = await focusRow(page, `${seeded.slugs.verification}#b-c1`);
    await expect(check.locator("[data-row-act]")).toHaveCount(0);

    // The decision that shaped a statement waits for review: Keep and Change.
    const shaping = await focusRow(page, seeded.shaping);
    await expect(shaping.locator("button[data-row-action]")).toHaveCount(2);

    // The decision a person asked to change says so, and draws no Keep and no Change (R10, F6).
    // The change is a comment on the decision's words, which Conversation draws beside its node
    // and not as a row.
    const changed = rowOf(await drawnRows(page), seeded.changed);
    expect(changed).toMatchObject({
      state: "change_requested",
      stateWord: NODE_STATE_WORDS.decision.change_requested,
    });
    expect(
      (await listComments(seeded.key)).some((one) => one.id === seeded.change),
      "the server holds no comment for the change"
    ).toBe(true);
    expect(
      await parentOfRow(page, seeded.change),
      "the change is drawn as a row of Conversation"
    ).toBeUndefined();
    const outlined = await focusRow(page, seeded.changed);
    await expect(outlined.locator("button[data-row-action]")).toHaveCount(0);

    // Nothing was sent: Reply aims the box at the foot (`IssueComposer.tsx`), Answer and an
    // option set the page's writer alone, and nothing was typed in the box or sent from it. A
    // thread's read mark is the page's own record of what the reader read
    // (`outline-thread-read.tsx`), and no write of theirs.
    expect(
      writes.requests.filter(
        (request) =>
          ["PATCH", "POST", "PUT"].includes(request.method) &&
          !request.path.includes("/me/threads/")
      ),
      "a press on a row's act wrote something"
    ).toEqual([]);
  });
});
