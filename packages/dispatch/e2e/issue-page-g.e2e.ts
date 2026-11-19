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
  expectRowGroups,
  expectRowLayouts,
  expectSeedDrawn,
  focusRow,
  focusThePage,
  LONG_ROWS,
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

/** The kinds of node the model holds no author for, whose rows draw none (R3, R5). */
const AUTHORLESS: readonly string[] = [
  "check",
  "child_issue",
  "decision",
  "plan_task",
  "requirement",
];

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
        // row of the seed's 160 words is taller than the screen, which holds it from its top.
        await expect
          .poll(
            async () => {
              const top = rowOf(await drawnRows(page), question)?.box?.y ?? -1;
              return top >= 0 && top < size.height;
            },
            { message: `${where} holds the top of that question's row off screen` }
          )
          .toBe(true);
      }
      await expectSeedDrawn(page, seeded);
      await expect
        .poll(() => nodeRows(page).count(), { message: `${where} draws no row` })
        .toBeGreaterThan(0);
      const layouts = await rowLayouts(page, seeded.slugs.spec);
      expectRowLayouts(layouts, where, true, size.width);
      // Decision 246: the spec's requirement is a statement and the decisions document's is a
      // block, so the view draws a bullet of each colour, each on its own row, for the check
      // above to read.
      expect(
        layouts
          .filter((row) => row.type === "requirement" && !row.context)
          .map((row) => [row.id, row.statement]),
        `${where} draws no statement beside a requirement of another document`
      ).toEqual(
        expect.arrayContaining([
          [`${seeded.slugs.spec}#b-r-one`, true],
          [`${seeded.slugs.decisions}#b-r1`, false],
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
    // A one-line statement with no state is 29 px tall at 1440 px (R1).
    const oneLine = (await rowLayouts(page, seeded.slugs.spec)).find(
      (row) => row.id === `${seeded.slugs.spec}#b-r-one`
    );
    expect(
      oneLine?.height ?? 1e6,
      "the one-line statement is taller than 29 px"
    ).toBeLessThanOrEqual(29);

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

    // The needs chip names its target by its first words, with no mark of markdown (F18).
    const needs = nodeRow(page, `${seeded.slugs.plan}#b-t2`).first().locator("[data-node-chip]");
    await expect(needs, "t2 draws other than its one needs chip").toHaveCount(1);
    await expect(needs.locator("[data-chip-name]")).toHaveText(/^1\. Stand/u);
    await expect(needs, "the needs chip draws a mark of markdown").not.toHaveText(/[*\\]/u);

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

    // An agent's link and a word that only looks like a key (R4).
    const linked = nodeRow(page, seeded.linked).first();
    await expect(linked.locator("a", { hasText: "UTF-16" })).toHaveCount(0);
    await expect(rowWords(linked)).toContainText("in UTF-16.");

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
// and the table's own scroll still reaches its last column.
// SLOW(>=2s): an issue with one note seeded and loaded at 390 px, and its table waited for until
// the batched read that carries a block's words draws it.
test("at 390 px a note's table of sixteen columns scrolls inside its row, and the page is no wider than the screen", async ({
  browser,
}) => {
  test.setTimeout(60_000);
  await phase(browser, seedWideNote, async (page, seeded) => {
    await page.setViewportSize(PHONE);
    await openIssue(page, seeded.key);
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

// c-cut: ruling 1 on R2 at 1440 px.
// SLOW(>=2s): a seeded issue of every kind of node, its rows and their names waited for, then
// ten presses and focus moves, each waited on until the row redraws.
test("an agent message and an answered question are cut at six lines, and the words of the reader and a waiting question are whole", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedRows, async (page, seeded) => {
    await openIssue(page, seeded.key);
    await expectSeedDrawn(page, seeded);
    const agent = nodeRow(page, seeded.longMessage).first();
    // The page cuts a row in the first view before it paints it, and any other row once it
    // stands within a screen of the viewport but wholly outside the view (decision 261), so each
    // row read here is first scrolled to just under the view, or just over it where the row is
    // too near the page's top to stand under it, waited for there, and then brought into view.
    const cutOf = async (id: string) => {
      const target = nodeRow(page, id).first();
      await target.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const under = box.top + window.scrollY - window.innerHeight - 1;
        window.scrollTo(0, under >= 0 ? under : box.bottom + window.scrollY + 1);
      });
      await expect
        .poll(
          async () =>
            (await rowLayouts(page, seeded.slugs.spec)).find((row) => row.id === id)?.more ?? "",
          { message: `${id} draws no more` }
        )
        .toMatch(/^more · [\d,]+ words$/u);
      await target.scrollIntoViewIfNeeded();
      return (await rowLayouts(page, seeded.slugs.spec)).find((row) => row.id === id);
    };
    const cut = await cutOf(seeded.longMessage);
    expect(
      Number(/([\d,]+)/u.exec(cut?.more ?? "")?.[1]?.replace(",", "")),
      "the cut holds back 20 words or fewer"
    ).toBeGreaterThan(20);
    expect((await rowWords(agent).boundingBox())?.height ?? 1e6).toBeLessThanOrEqual(6 * 24 + 12);
    await rowMore(agent).click();
    // They stay whole once the focus has been on the message and has left it.
    await focusRow(page, seeded.longMessage);
    await focusRow(page, seeded.comment);
    await expect
      .poll(async () =>
        (await rowLayouts(page, seeded.slugs.spec)).find((row) => row.id === seeded.longMessage)
      )
      .toMatchObject({ more: CONTROL_LABELS.read_less, wordsHidden: 0 });

    const layouts = await rowLayouts(page, seeded.slugs.spec);
    expect(
      layouts.find((row) => row.id === seeded.longComment),
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
    expect(
      layouts.find((row) => row.id === seeded.waitingLong),
      "the waiting question is cut"
    ).toMatchObject({
      more: null,
      wordsHidden: 0,
    });
    expect((await cutOf(seeded.answeredLong))?.more ?? "").toMatch(/^more · /u);

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

    // Keep open keeps a body when the focus moves on; a second press lets it close (R6).
    await rowBody(statement).getByRole("button", { name: CONTROL_LABELS.keep_open }).click();
    await focusRow(page, seeded.comment);
    await expect(rowBody(statement)).toHaveCount(1);
    await focusRow(page, `${seeded.slugs.spec}#b-r-one`);
    await rowBody(statement).getByRole("button", { name: CONTROL_LABELS.let_close }).click();
    await focusRow(page, seeded.comment);
    await expect(rowBody(statement)).toHaveCount(0);

    // No element of a row carries a `title` (R6).
    expect(await nodeRows(page).locator("[title]").count()).toBe(0);
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
 *  with every branch open. */
async function openLongRows(
  page: Page,
  key: string,
  size: { height: number; width: number },
  where: string
): Promise<void> {
  await page.setViewportSize(size);
  await openIssue(page, key);
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        return nodeRows(page).count();
      },
      { message: `${where} the page never drew every long row` }
    )
    .toBeGreaterThan(LONG_ROWS);
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

/** Waits until row `above`, which a jump left above the view, draws its cut, and two frames
 *  more, and fails unless the first row wholly in view that `jump` read stands within a pixel of
 *  where it stood, and unless it and row `crossing`, which a jump left across the view's top,
 *  are still drawn whole: a row in view keeps its length (decision 261). */
async function expectFirstHeld(
  page: Page,
  rows: { readonly above: string; readonly crossing: string },
  jump: Jump,
  where: string
): Promise<void> {
  await expect(
    rowMore(nodeRow(page, rows.above)),
    `${where} the row above the view was not cut`
  ).toHaveCount(1);
  await nextFrames(page);
  const first = nodeRow(page, jump.first ?? "");
  const top = await first.evaluate((row) => row.getBoundingClientRect().top);
  expect(
    Math.abs(top - jump.top),
    `${where} the first row wholly in view moved from ${jump.top} px to ${top} px`
  ).toBeLessThanOrEqual(1);
  await expect(rowMore(first), `${where} the first row wholly in view was cut`).toHaveCount(0);
  await expect(
    rowMore(nodeRow(page, rows.crossing)),
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

// Decision 252: a long jump into the middle of the page - a fast drag of the scrollbar - lands
// on rows no batch measured, and the browser's own scroll anchoring holds the view only while
// the rows that shrink are outside it. At each width the row scrolls a conversation of long rows
// to its foot and waits there until the rows above the view are cut, then jumps to 40 px above
// the foot of the words of the row that holds the page's middle: that row crosses the view's
// top, and the one under it is the first row wholly in view. Once the row above them draws its
// cut, the first row wholly in view stands where it stood, within a pixel, and both rows in view
// are still whole (decision 261). It carries `@phone` because the phone project lays the page
// out at three device pixels to the CSS pixel, where the scroll that holds the row snaps to a
// device pixel.
// SLOW(>=2s): sixty-six long rows seeded, loaded at two widths, scrolled to their foot and to
// their middle, each scroll waited out until the rows above the view are cut.
test("a long jump into the middle of the page keeps the first row wholly in view where it stood as the rows there are cut @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await openLongRows(page, seeded.key, size, where);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      await expect
        .poll(
          () =>
            page.evaluate(() =>
              [...document.querySelectorAll("[data-node-row]")].some((row) => {
                const box = row.getBoundingClientRect();
                return box.bottom <= 0 && row.querySelector("[data-node-more]") !== null;
              })
            ),
          { message: `${where} no row above the view at the page's foot was cut` }
        )
        .toBe(true);
      const crossing = await middleRow(page, seeded.rows, where);
      const jump = await jumpAbove(page, crossing);
      const at = seeded.rows.indexOf(crossing);
      expect(
        jump,
        `${where} the jump did not land with a whole row crossing the view's top above the first row wholly in view`
      ).toMatchObject({ cut: false, first: seeded.rows[at + 1] });
      expect(jump.crossingTop, `${where} the crossing row's words start in view`).toBeLessThan(0);
      await expectFirstHeld(page, { above: seeded.rows[at - 1] ?? "", crossing }, jump, where);
    }
  });
});

// Decision 260: the row a slice holds is the first row wholly in view of any kind, not only one
// that can be cut. At each width the row jumps from the page's top to 40 px above the foot of the
// words of the planner's row right above the reader's own run of long rows, which no cut ever
// shortens: the planner's row crosses the view's top, and the reader's rows alone stand in view
// under it. Once the planner's row above it draws its cut, the reader's first row stands where
// it stood, within a pixel, and both rows are whole. It carries `@phone` for the reason the row
// above gives.
// SLOW(>=2s): sixty-six long rows seeded, loaded at two widths, and each jump into them waited
// out until the planner's row above the view is cut.
test("a long jump that lands on the reader's own rows keeps the first of them where it stood as the row above them is cut @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
    const mine = seeded.own[0] ?? "";
    const above = seeded.rows[seeded.rows.indexOf(seeded.aboveOwn) - 1] ?? "";
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await openLongRows(page, seeded.key, size, where);
      // The rows drawn at load are cut before the jump, so no cut of theirs lands after it.
      await expect(
        rowMore(nodeRow(page, seeded.rows[0] ?? "")),
        `${where} the first row was not cut`
      ).toHaveCount(1);
      const jump = await jumpAbove(page, seeded.aboveOwn);
      expect(
        jump,
        `${where} the jump did not land with the planner's whole row across the view's top`
      ).toMatchObject({ cut: false, first: mine });
      expect(jump.crossingTop, `${where} the planner's row's words start in view`).toBeLessThan(0);
      expect(
        jump.inView,
        `${where} a row other than the reader's own stands in view under the planner's`
      ).toEqual([seeded.aboveOwn, ...seeded.own.slice(0, jump.inView.length - 1)]);
      await expectFirstHeld(page, { above, crossing: seeded.aboveOwn }, jump, where);
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

// Decision 263: a row that stands in view as the page opens is cut before the page paints it, so
// the reader never sees it whole and it never shrinks in view. At each width a script that runs
// before the page's own reads the planner's long statement, the body's first row, which a check
// nothing proves keeps open in an outline folded to what is open, at each animation frame from
// the first one it is drawn in: it is cut on that first frame, and its height is the same 60
// frames later. The row is drawn from the spec's blocks before the coverage read brings its
// words, and a row with no words has nothing to cut, so it is read from the first frame it draws
// its words in: CI run 44547033390 read it at 390 px with none for two frames, and then whole,
// 472 px tall, for the 59 after. It carries `@phone` because the phone's first view is another
// set of rows, laid out at three device pixels to the CSS pixel.
// SLOW(>=2s): sixty-six long rows seeded, and the page loaded at two widths, each watched over
// 61 frames.
test("a long row in the first view is drawn cut on the first frame it is drawn in, and its height holds 60 frames later @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
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
      expect(first.cut, `${where} the row was drawn whole on the first frame it was drawn in`).toBe(
        true
      );
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
    const plannerAuthor = { author: "Planner", shape: GLYPH_TOKENS.authors.agent };
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
      [seeded.result, { author: CONTROL_LABELS.unnamed_author, shape: GLYPH_TOKENS.authors.agent }],
    ]);
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize(size);
      await openIssue(page, seeded.key);
      await expectSeedDrawn(page, seeded);
      // A person by their login and an agent by its role's word, or by the page's word for one
      // that names none, each by its kind's shape: people share one, agents another. A node the
      // model holds no author for draws none.
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
    // shape.
    const planner = await focusRow(page, seeded.linked);
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
        rowOf(rows, seeded.linked)?.authorShape,
        unknown,
      ]).size,
      "a person, an agent and an author nobody recorded do not each have a shape of their own"
    ).toBe(3);
  });
});

// c-writer: R7 and R10 at 1440 px.
// SLOW(>=2s): a seeded issue of every kind of node, its rows and their names waited for, then
// six rows focused, two presses waited out for the writer's frames, and a read of the API.
test("a row's Reply and Answer set the page's writer, and a decision asked to change draws it", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedRows, async (page, seeded) => {
    await openIssue(page, seeded.key);
    await expectSeedDrawn(page, seeded);
    const writes = watchApiRequests(page);

    const comment = await focusRow(page, seeded.comment);
    await expect(rowAct(comment, "reply"), "the focused comment draws no Reply").toHaveCount(1);
    await expect(rowAct(comment, "answer")).toHaveCount(0);
    await rowAct(comment, "reply").click();
    await writerFramesPass(page);
    await expect(comment).toBeFocused();

    const question = await focusRow(page, seeded.openQuestion);
    await expect(rowAct(question, "answer")).toHaveCount(1);
    await expect(rowAct(question, "reply")).toHaveCount(1);
    const option = question.locator('[data-pick-option="1024 px"]');
    await option.click();
    await expect(option).toHaveAttribute("aria-pressed", "true");
    await expect(option).toContainText("●");
    await expect(question.locator('[data-pick-option="1280 px"]')).toContainText("○");
    await rowAct(question, "answer").click();
    await writerFramesPass(page);
    await expect(question).toBeFocused();

    // A statement and a check take no reply in today's model, so they draw no act.
    for (const id of [`${seeded.slugs.spec}#b-r-one`, `${seeded.slugs.verification}#b-c1`]) {
      const typed = await focusRow(page, id);
      await expect(typed.locator("[data-row-act]")).toHaveCount(0);
    }

    // The decision that shaped a statement waits for review: Keep and Change.
    const shaping = await focusRow(page, seeded.shaping);
    await expect(shaping.locator("button[data-row-action]")).toHaveCount(2);

    // The decision a person asked to change says so, holds the change under it, and draws no
    // Keep and no Change (R10, F6).
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
      "the change is not drawn under its decision"
    ).toBe(seeded.changed);
    const outlined = await focusRow(page, seeded.changed);
    await expect(outlined.locator("button[data-row-action]")).toHaveCount(0);

    // Nothing was sent: no box exists until part L draws it. A thread's read mark is the page's
    // own record of what the reader read (`outline-thread-read.tsx`), and no write of theirs.
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
