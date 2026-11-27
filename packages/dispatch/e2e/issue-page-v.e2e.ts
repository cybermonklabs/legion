import AxeBuilder from "@axe-core/playwright";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { chipWords } from "../web/src/features/issue-page/node-row-chip";
import {
  CHECK_WHEN_WORDS,
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  STATE_LINES,
  VERIFICATION_WORDS,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import {
  createComment,
  createIssue,
  createProject,
  getArtifact,
  getIssueCoverage,
  getIssueResults,
  recordCheckResult,
  retractCheckResult,
} from "./api";
import { nextFrames } from "./frames";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { HEAD_SHA, PROJECT } from "./issue-layout-seed-support";
import { requirementBlockId } from "./issue-layout-seed-text";
import { gotoIssue, issuePage, phase } from "./issue-page";
import {
  DESKTOP,
  expectIssuePageDrawn,
  expectOpeningLanded,
  focusThePage,
  longWords,
  nodeRows,
  PHONE,
  rowBody,
  rowMore,
  rowWords,
  tree,
} from "./issue-page-g";
import { activePreset, focusInside, identityBlockBottom, openPreset } from "./issue-page-i";
import {
  asRecorder,
  boxesPastTheirCard,
  checkLegendCounts,
  contradictions,
  copyCommandControls,
  type DrawnCheck,
  glyphsWithNoBox,
  linkLook,
  namedCommit,
  nodeCommands,
  openSettledView,
  overlappingTexts,
  paintedMarks,
  RUNBOOK,
  readServed,
  refuseBlocksRead,
  STATEMENT_B_WORDS,
  seedLinkedChecks,
  tabWalk,
  verificationBody,
  verificationCard,
  verificationCardBoxes,
  verificationCards,
  verificationCheck,
  verificationChip,
  verificationFrame,
  verificationIssueRow,
  verificationUnreadable,
} from "./issue-page-v";
import { expect, type Route, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * The rows of the new issue page's Verification view, read on the real stack. Each row turns the
 * new page on for its reader with `phase` (`e2e/issue-page.ts`), seeds what it reads, and reads
 * the view's cards through `e2e/issue-page-v.ts`, whose measures fail where the view draws no
 * card.
 *
 * The file holds eight rows. Rows 1 and 2 come from an earlier `e2e/issue-layout.e2e.ts`, and the
 * other six were written for this view; each runs under the title the Verification document
 * names it by.
 */

/** The issue's results read, which row 1 rewrites to draw the seventh state. */
const RESULTS_ROUTE = "**/api/v1/issues/*/results";

/** Records a pass of the whole issue's check 1 at the head commit, as the recorder's session,
 *  with what it observed and, where a row gives them, its screenshots. */
function recordPass(seeded: WholeIssueSeed, observed: string, screenshots?: string[]) {
  return recordCheckResult(
    seeded.key,
    {
      check: seeded.checks.passed,
      commit: HEAD_SHA,
      failed: 0,
      negative_control: "Removed the row's state and the run failed.",
      observed,
      role: "tester",
      run: "bun run e2e -- e2e/issue-page-v.e2e.ts",
      ...(screenshots === undefined ? {} : { screenshots }),
      surface: "browser",
      verdict: "pass",
    },
    asRecorder
  );
}

// Row 1. The body the Verification view draws is `verification-filter/`: each statement with
// the checks that serve it, each check beside its newest result, and its earlier results folded
// to one line, as the approved picture draws one result for each check. Two of the seven states
// the seed cannot make, so this row makes them: `retracted` by withdrawing every result of one
// check, and `unreadable` by taking `pass_stands` out of the results answer in a route handler
// of its own, which is the one way a page meets a server that does not send it. The seeded
// Verification document holds no picture, so the row records a result carrying the issue's own
// uploaded shot. It reads too the view's measure at each break, the legend's counts, the facts
// opening on the focus and not on hover, and the forced-colours marks.
// Each load's cards are counted before the page's tree is read, so at a base whose slot draws
// nothing the row fails on its own message.
// SLOW(>=2s): a whole seeded issue with one result recorded and another withdrawn, then seven
// loads of the view - six widths and a rewritten answer - each read clause by clause, with
// presses, focus moves and a forced-colours pass waited on at each width.
test("the Verification filter is readable @phone", async ({ browser }) => {
  test.setTimeout(420_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;

    await recordPass(seeded, "1 passed, with a shot of the page", [
      `dispatch://${seeded.key}/artifact/${seeded.documents.image}`,
    ]);
    const recorded = await getIssueResults(seeded.key);
    const withdrawing = (recorded.checks ?? []).find(
      (check) => check.block_id === seeded.checks.uncontrolled
    );
    expect(withdrawing?.results ?? [], "the seed recorded no result to withdraw").not.toEqual([]);
    // The session that recorded a result is one of the three callers that may withdraw it, and
    // the agent bearer acts as no session unless it names one (`retractCheckResult`).
    for (const result of withdrawing?.results ?? []) {
      await retractCheckResult(result.id, { reason: "The run was wrong." }, asRecorder);
    }

    // What the issue's own reads say, which the page is read against.
    const statements = (await getIssueCoverage(seeded.key)).requirements ?? [];
    const covered = statements.filter((statement) => statement.checks > 0).length;
    const nodeOf = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const twice = blockNodeId(verificationSlug, seeded.checks.passed);
    // The checks that serve each statement: check i serves statement i, for 1 to 6.
    const servedBy = new Map(
      seeded.checks.all.map((check, at) => [nodeOf(at + 1), [blockNodeId(verificationSlug, check)]])
    );
    // What the legend counts: each drawn check once, in the state it is drawn in, and 0 for a
    // state no drawn check is in.
    const legendOf = (checks: readonly DrawnCheck[]) => {
      const states = [...new Map(checks.map((check) => [check.id, check.state])).values()];
      return Object.fromEntries(
        Object.keys(NODE_STATE_WORDS.check).map((state) => [
          state,
          states.filter((held) => held === state).length,
        ])
      );
    };
    // Every check the view draws now, read afresh, under each card it is drawn on.
    const drawnChecksNow = async () =>
      (await verificationCards(page)).flatMap((card) => card.checks);

    for (const width of [1440, 1024, 800, 760, 390, 320]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await openPreset(page, seeded.key, "verification");
      expect(await activePreset(page), `the view is not named Verification at ${width} px`).toBe(
        PRESET_NAMES.verification
      );
      await expect
        .poll(() => verificationCardBoxes(page).count(), {
          message: `the Verification filter draws no statement at ${width} px`,
        })
        .toBeGreaterThan(0);
      await expectIssuePageDrawn(page);
      const cards = await verificationCards(page);

      // Each statement of the issue is drawn, with the checks that serve it under it.
      expect(
        cards.map((card) => card.id).sort(),
        `the Verification view draws other statements than the issue's at ${width} px`
      ).toEqual(statements.map((statement) => blockNodeId(specSlug, statement.block_id)).sort());
      expect(
        new Set(cards.flatMap((card) => card.checks.map((check) => check.id))).size,
        `the Verification view draws no six checks at ${width} px`
      ).toBe(6);
      expect(
        Object.fromEntries(cards.map((card) => [card.id, card.checks.map((check) => check.id)])),
        `a card draws other checks than the ones that serve its statement at ${width} px`
      ).toEqual(Object.fromEntries(cards.map((card) => [card.id, servedBy.get(card.id) ?? []])));

      // No value reaches past either side of its card at any width, and no two values overlap:
      // the sixteen faults of the old tab this requirement is here for.
      expect(
        await boxesPastTheirCard(page),
        `an element reaches past its card at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page),
        `two pieces of text are drawn over each other at ${width} px`
      ).toEqual([]);

      // The view's measure. The document is no wider than the screen. From 761 px up the body is
      // 1180 px wide, or its column less 48 px where that is less, with 22 px above it and 120 px
      // under it, and the table's three columns stand at 38, 36 and 26 per cent of its width; at
      // 760 px and below the body is its column less 20 px with 10 px above it, no heading is
      // drawn, and each statement is a card with a 4 px left edge and 8 px corners, 8 px from the
      // next.
      const frame = await verificationFrame(page);
      expect(frame.page.scroll, `the page scrolls sideways at ${width} px`).toBeLessThanOrEqual(
        frame.page.viewport
      );
      if (width === 1440 || width === 1024) {
        const measure = width === 1440 ? 1180 : frame.body.parent - 48;
        expect(
          Math.abs(frame.body.width - measure),
          `the body is ${frame.body.width} px wide, not ${measure} px, at ${width} px`
        ).toBeLessThanOrEqual(1);
      }
      if (width > 760) {
        expect(
          [frame.body.top, frame.body.bottom],
          `the body's top and bottom margins are not 22 and 120 px at ${width} px`
        ).toEqual([22, 120]);
      } else {
        expect(frame.body.top, `the body's top margin is not 10 px at ${width} px`).toBe(10);
        expect(
          Math.abs(frame.body.width - (frame.body.parent - 20)),
          `the body is ${frame.body.width} px wide in a column of ${frame.body.parent} px at ${width} px`
        ).toBeLessThanOrEqual(1);
        await expect(
          page.locator("[data-verification-head]"),
          `a column heading is drawn at ${width} px`
        ).toBeHidden();
        expect(
          frame.firstCard,
          `the first statement is no card with a 4 px left edge and 8 px corners at ${width} px`
        ).toMatchObject({ borderLeft: "4px", radius: "8px" });
        expect(
          Math.abs((frame.firstCard.gap ?? Number.NaN) - 8),
          `the first two cards do not stand 8 px apart at ${width} px`
        ).toBeLessThanOrEqual(1);
      }
      if (width === 800) {
        expect(
          frame.columns.heads,
          "the table draws other than three headings at 800 px"
        ).toHaveLength(3);
        for (const [at, percent] of [38, 36, 26].entries()) {
          const head = frame.columns.heads[at] ?? 0;
          expect(
            Math.abs(head - (frame.columns.table * percent) / 100),
            `heading ${at + 1} is ${head} px of the table's ${frame.columns.table} px, not ${percent} per cent, at 800 px`
          ).toBeLessThanOrEqual(2);
        }
      }

      // The page names one commit for the issue's results, the newest standing result's, drawn
      // once, in the ten characters the prototype draws a commit in, and on no result's own
      // line.
      expect(
        await namedCommit(page),
        `the page names no one commit for the issue's results at ${width} px`
      ).toBe(HEAD_SHA.slice(0, 10));
      expect(
        (await page.locator("[data-verification-result]").allInnerTexts()).filter((drawn) =>
          drawn.includes(HEAD_SHA.slice(0, 10))
        ),
        `a result draws a commit of its own at ${width} px`
      ).toEqual([]);

      // How many statements carry a check is drawn as covered, and no count, legend or heading
      // reads proven or met for it.
      expect(
        (await page.locator("[data-verification-covered]").innerText()).replace(/\s+/g, " "),
        `the count of the statements that carry a check does not read covered at ${width} px`
      ).toBe(
        `${covered} ${VERIFICATION_WORDS.of} ${statements.length} ${VERIFICATION_WORDS.covered}`
      );
      for (const part of [
        "[data-verification-covered]",
        "[data-verification-legend]",
        "[data-verification-head]",
      ]) {
        const read = (await page.locator(part).allInnerTexts()).join(" ").toLowerCase();
        expect(read, `${part} reads proven at ${width} px`).not.toContain("proven");
      }
      expect(
        (await page.locator("[data-verification-covered]").innerText()).toLowerCase(),
        `the coverage count reads met at ${width} px`
      ).not.toContain("met");

      // A statement reads Met where every check of it passed at that commit, Not met where one
      // failed there, and Unknown otherwise - here no result, an out-of-date one, a commit
      // Dispatch cannot place, and a withdrawn one.
      const stateOf = new Map(cards.map((card) => [card.id, card.state]));
      expect(
        [1, 2, 3, 4, 5, 6].map((index) => stateOf.get(nodeOf(index))),
        `a statement does not read by how its checks stand at ${width} px`
      ).toEqual(["met", "not_met", "unknown", "unknown", "unknown", "unknown"]);
      expect(
        cards.find((card) => card.id === nodeOf(1))?.stateWord,
        `the statement every check of which passed does not read Met at ${width} px`
      ).toBe(VERIFICATION_WORDS.states.met);
      expect(
        cards.find((card) => card.id === nodeOf(2))?.stateWord,
        `the statement one check of which failed does not read Not met at ${width} px`
      ).toBe(VERIFICATION_WORDS.states.not_met);

      // A statement no check serves carries its gap.
      expect(
        cards.find((card) => card.id === nodeOf(7))?.gapKinds,
        `a statement no check serves carries no gap at ${width} px`
      ).toEqual(["no_check"]);
      expect(
        (await page.locator('[data-gap-kind="no_check"]').first().innerText()).trim(),
        `the gap is not said in the page's own words at ${width} px`
      ).toBe(GAP_LINES.no_check);

      // Each of the seven states a check can be in is told apart by a word and a shape. Six of
      // them stand on the seeded checks; the seventh is below, on the rewritten answer.
      const drawnChecks = cards.flatMap((card) => card.checks);
      const states = new Map(
        drawnChecks.map((check) => [check.state ?? "", `${check.word} ${check.shape}`])
      );
      expect(
        [...states.keys()].sort(),
        `the Verification view draws not every seeded state at ${width} px`
      ).toEqual(["failed", "no-result", "older", "passed", "retracted", "unmarked"]);
      expect(
        [...states],
        `a state is drawn in other words or another shape than the page declares at ${width} px`
      ).toEqual(
        [...states.keys()].map((state) => [
          state,
          `${NODE_STATE_WORDS.check[state as CheckState]} ${GLYPH_TOKENS.states.check[state as CheckState]}`,
        ])
      );
      expect(
        new Set([...states.values()]).size,
        `two of the seeded states are drawn the same way at ${width} px: ${JSON.stringify([...states])}`
      ).toBe(states.size);

      // The legend counts each drawn check once, in the state it is drawn in, the states no
      // check is in included.
      expect(
        await checkLegendCounts(page),
        `the legend does not count the drawn checks in their states at ${width} px`
      ).toEqual(legendOf(drawnChecks));

      // The body draws the newest result of each check, and its earlier results are
      // one line with their count, which opens them in place. The check with two results draws
      // one, marked as the one its state stands on, says "1 earlier result", draws both once
      // that line is pressed, and one again at a second press.
      expect(
        drawnChecks.filter((check) => check.results > 1).map((check) => check.id),
        `a check draws more than its newest result at ${width} px`
      ).toEqual([]);
      expect(
        drawnChecks.find((check) => check.id === twice)?.earlier,
        `the check with two results does not say how many earlier ones it holds at ${width} px`
      ).toBe(`1 ${VERIFICATION_WORDS.earlier_one}`);
      const opened = verificationCheck(page, twice);
      const standing = opened.locator("[data-verification-result][data-result-stands]");
      await expect(
        standing,
        `check 1 marks no one result as the one it stands on at ${width} px`
      ).toHaveCount(1);
      expect(
        (await standing.locator("p").first().innerText()).slice(
          0,
          VERIFICATION_WORDS.stands.length
        ),
        `check 1's standing result's line does not begin with its mark at ${width} px`
      ).toBe(VERIFICATION_WORDS.stands);
      const folded = opened.locator("[data-verification-earlier]");
      await folded.click();
      await expect(folded, "the line does not say it opened").toHaveAttribute(
        "aria-expanded",
        "true"
      );
      expect(
        (await drawnChecksNow()).find((check) => check.id === twice)?.results,
        `the line does not open the earlier result in place at ${width} px`
      ).toBe(2);
      await folded.click();
      await expect
        .poll(async () => (await drawnChecksNow()).find((check) => check.id === twice)?.results, {
          message: `a second press on the line does not fold the earlier result at ${width} px`,
        })
        .toBe(1);

      // A command renders as code with a copy control, and `when` reads as words: both in the
      // check's own facts, which the reader's focus on its words opens, with no press.
      await focusThePage(page);
      await expect(
        rowBody(opened),
        `check 1's facts are drawn with the focus off it at ${width} px`
      ).toHaveCount(0);
      await opened.locator("[data-verification-check-words]").focus();
      await expect(
        rowBody(opened).first(),
        `the focus on check 1's words does not open its facts at ${width} px`
      ).toBeVisible();
      expect(
        await nodeCommands(page).count(),
        `the check's command is not drawn as code at ${width} px`
      ).toBeGreaterThan(0);
      expect(
        (
          await nodeCommands(page)
            .first()
            .evaluate((element) => getComputedStyle(element).fontFamily)
        ).toLowerCase(),
        `the command is not set in a monospace face at ${width} px`
      ).toMatch(/mono/);
      expect(
        await copyCommandControls(page).count(),
        `a command has no copy control at ${width} px`
      ).toBe(await nodeCommands(page).count());
      expect(
        (await opened.innerText()).replace(/\s+/g, " "),
        `the check draws its when attribute as a schema token at ${width} px`
      ).toContain(CHECK_WHEN_WORDS.before_merge);
      expect(
        await opened.innerText(),
        `the check draws the schema's own token at ${width} px`
      ).not.toContain("before_merge");

      // The screenshot the result recorded renders as a picture.
      const shot = rowBody(opened).locator("img").first();
      await expect(
        shot,
        `the seeded screenshot is drawn as no picture at ${width} px`
      ).toBeVisible();
      await expect
        .poll(() => shot.evaluate((element: HTMLImageElement) => element.naturalWidth), {
          message: `the seeded screenshot draws no image at ${width} px`,
        })
        .toBeGreaterThan(0);
      // The opened facts stay inside their card too, and draw no text over another.
      expect(
        await boxesPastTheirCard(page),
        `an element of the opened check reaches past its card at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page),
        `two pieces of text are drawn over each other with check 1's facts open at ${width} px`
      ).toEqual([]);
      // No result draws a commit of its own in the facts it opens either.
      expect(
        (await page.locator("[data-verification-result]").allInnerTexts()).filter((drawn) =>
          drawn.includes(HEAD_SHA.slice(0, 10))
        ),
        `a result draws a commit of its own with check 1's facts open at ${width} px`
      ).toEqual([]);

      // Nothing opens on hover: with the focus off the view, the pointer over check 2's
      // words draws no facts under it. React draws what a pointer's handler sets in a later
      // task, so the row is read two frames after the hover, and then waited on.
      const failing = verificationCheck(page, blockNodeId(verificationSlug, seeded.checks.failed));
      await focusThePage(page);
      await failing.locator("[data-verification-check-words]").hover();
      await nextFrames(page);
      await expect(
        failing,
        `the pointer over check 2's words says it opened its facts at ${width} px`
      ).toHaveAttribute("aria-expanded", "false");
      await expect(
        rowBody(failing),
        `the pointer over check 2's words opened its facts at ${width} px`
      ).toHaveCount(0);

      // Nothing the body draws is told apart by colour alone: with the colours forced, each
      // state's word and shape in the cards is still painted where a reader sees it.
      await page.emulateMedia({ forcedColors: "active" });
      const painted = await paintedMarks(page);
      expect(painted.forced, `the page does not read forced colours at ${width} px`).toBe(true);
      expect(
        painted.read,
        `the cards draw no state's word or shape once the colours are forced at ${width} px`
      ).toBeGreaterThan(0);
      expect(
        painted.unseen,
        `a state's word or shape is not painted where a reader sees it once the colours are forced at ${width} px`
      ).toEqual([]);
      // Each shape keeps a box of its own, and the pressed chip keeps a mark of its own, its
      // outline, where the system draws every chip's fill and border alike.
      const glyphs = await glyphsWithNoBox(page);
      expect(
        glyphs.drawn,
        `the view draws no shape once the colours are forced at ${width} px`
      ).toBeGreaterThan(0);
      expect(
        glyphs.empty,
        `a shape draws no box once the colours are forced at ${width} px`
      ).toEqual([]);
      const pressed = issuePage(page).locator('[data-verification-chip][aria-pressed="true"]');
      await expect(pressed, `no one chip is pressed at ${width} px`).toHaveCount(1);
      await expect(
        pressed,
        `the pressed chip draws no outline once the colours are forced at ${width} px`
      ).not.toHaveCSS("outline-style", "none");
      await page.emulateMedia({ forcedColors: "none" });
    }

    // The seventh state: a payload carrying no answer over a check's results at all, which is
    // an older build behind the same load balancer as the one this page came from. The page
    // says what is true of it - that it could not read how the check stands - and never one of
    // the other six.
    await page.route(RESULTS_ROUTE, async (route) => {
      const answer = await route.fetch();
      // The results answer as the server sends it, of which this handler reads two fields.
      const body: { checks?: { block_id: string; pass_stands?: unknown }[] } = await answer.json();
      for (const check of body.checks ?? []) {
        if (check.block_id === seeded.checks.passed) delete check.pass_stands;
      }
      await route.fulfill({ json: body, response: answer });
    });
    await page.setViewportSize(DESKTOP);
    await openPreset(page, seeded.key, "verification");
    await expect(
      verificationCardBoxes(page).first(),
      "the Verification view draws no statement on the rewritten answer"
    ).toBeVisible();
    await expectIssuePageDrawn(page);
    await expect
      .poll(
        async () =>
          (await drawnChecksNow())
            .filter((check) => check.state === "unreadable")
            .map((check) => [check.word, check.shape]),
        { message: "a check whose results the page cannot read draws no state of its own" }
      )
      .toEqual([[NODE_STATE_WORDS.check.unreadable, GLYPH_TOKENS.states.check.unreadable]]);
    expect(
      await checkLegendCounts(page),
      "the legend does not count the check whose results the page cannot read in its own state"
    ).toEqual(legendOf(await drawnChecksNow()));
    await page.unroute(RESULTS_ROUTE);
  });
});

// Row 2. An issue that holds no statement and no check says so in the Verification view's own
// column once the load has settled, in the page's one line for it, and the line is read inside
// the tree the view draws and not off the whole page, which says it nowhere else. The body is
// counted before the page's tree is read, so at a base whose slot draws nothing the row fails on
// its own message.
// SLOW(>=2s): a new issue's view loaded and waited out to a 1.5 s quiet window before it is read.
test("the Verification filter says an issue holds no checks, in its own column", async ({
  browser,
}) => {
  const bare = async () => {
    await createProject({ key: PROJECT, name: "Issue layout" });
    return await createIssue({ force: true, project: PROJECT, title: "Nothing to verify yet" });
  };
  await phase(browser, bare, async (page, seeded) => {
    const settling = watchApiRequests(page);
    await openPreset(page, seeded.key, "verification");
    await expectLoadSettled(settling);
    const body = verificationBody(page);
    await expect(body, "the Verification filter draws no body of its own").toHaveCount(1);
    await expectIssuePageDrawn(page);
    await expect(
      tree(page).getByText(EMPTY_STATE_LINES.verification, { exact: true }),
      "the view's tree does not say the issue holds no checks"
    ).toHaveCount(1);
    await expect(
      page.getByText(EMPTY_STATE_LINES.verification, { exact: true }),
      "the line is drawn outside the Verification view's tree as well"
    ).toHaveCount(1);
    await expect(verificationCardBoxes(page), "an issue with no statement draws one").toHaveCount(
      0
    );
    expect(await nodeRows(page).count(), "an issue with no check draws a row").toBe(0);
  });
});

// Row 3. A check's words are drawn with their links through `RowText`: a link in the words'
// colour, underlined in the bullets' grey, and a `dispatch://` reference as the chip that names
// its target, never as its address. A result an agent recorded names it by its role's word. A
// check that serves two statements is drawn under each, names the other by its first words, and
// is counted once. A statement that only a child's check serves is covered, carries no gap, and
// says so in one line. A statement of 200 words is cut at six lines, and its "more" is a tab stop
// between the chips and the check's link. With A opened, and again with the check's facts open,
// every element stays inside its card and no two texts overlap, this issue's cards being the only
// ones that hold what the view adds to a card.
// SLOW(>=2s): an issue, its child and their two documents seeded and waited for in the coverage
// read, then the view loaded and read at two widths, with a walk by Tab at the first.
test("the Verification view draws a check's links, its agent's role and a child's checks @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedLinkedChecks, async (page, seeded) => {
    const { a, b, c, check } = seeded.nodes;
    const cut = new RegExp(
      `^${CONTROL_LABELS.read_more} · [\\d,]+ ${CONTROL_LABELS.read_more_words}$`,
      "u"
    );
    for (const width of [1440, 390]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await openPreset(page, seeded.key, "verification");
      await expectOpeningLanded(page);
      const statementA = verificationCard(page, a);
      await expect(
        statementA,
        `the Verification view draws no card for statement A at ${width} px`
      ).toHaveCount(1);
      await expectIssuePageDrawn(page);

      // The check is drawn on the cards of A and B, and on no other, and the legend counts it
      // once, in its state.
      const cards = await verificationCards(page);
      expect(
        Object.fromEntries(cards.map((card) => [card.id, card.checks.map((one) => one.id)])),
        `the check is not drawn under each statement it serves and under no other at ${width} px`
      ).toEqual({ [a]: [check], [b]: [check], [c]: [] });
      const legend = await checkLegendCounts(page);
      const state = cards.find((card) => card.id === a)?.checks[0]?.state ?? "";
      expect(
        [Object.values(legend).reduce((sum, count) => sum + count, 0), legend[state]],
        `the legend does not count the check it draws twice once, in its state, at ${width} px`
      ).toEqual([1, 1]);

      // A is cut at six lines, and its "more" opens it whole: at 1440 px from the keyboard, Tab
      // from the chip All reaching it and then the link in the check's words; at 390 px by a
      // press.
      const words = rowWords(statementA);
      const more = rowMore(statementA);
      // The page measures a statement's cut once the statement stands within a screen of the
      // viewport, in the frame after it comes there, so A is brought there before its cut is read.
      await statementA.scrollIntoViewIfNeeded();
      await expect(more, `statement A draws no "more" at ${width} px`).toHaveText(cut);
      // The line has just matched its cut words above, so one reading of it is settled.
      const heldBack = Number(/[\d,]+/u.exec(await more.innerText())?.[0]?.replace(/,/g, ""));
      expect(
        heldBack,
        `statement A's cut holds back 20 words or fewer at ${width} px`
      ).toBeGreaterThan(20);
      // The words of A are set on 24 px lines, so six of them are more than five lines' height
      // and no more than six.
      const cutHeight = (await words.boundingBox())?.height ?? Number.NaN;
      expect(cutHeight, `statement A is not cut at six lines at ${width} px`).toBeGreaterThan(
        5 * 24
      );
      expect(cutHeight, `statement A is not cut at six lines at ${width} px`).toBeLessThanOrEqual(
        6 * 24 + 1
      );
      const link = statementA.locator(`[data-verification-check-words] a[href="${RUNBOOK}"]`);
      const tabTo = tabWalk(page, 20);
      if (width === 1440) {
        await verificationChip(page, "all").focus();
        await tabTo(
          more,
          `Tab from the chip All does not reach statement A's "more" at ${width} px`
        );
        await page.keyboard.press("Enter");
      } else {
        await more.click();
      }
      await expect(more, `"more" does not open statement A at ${width} px`).toHaveText(
        CONTROL_LABELS.read_less
      );
      expect(
        await words.evaluate((element) => element.scrollHeight - element.clientHeight),
        `statement A hides a word once "more" opened it at ${width} px`
      ).toBeLessThanOrEqual(1);
      // Opened, the words of A, the check's link and chip, its "Also under" line and the line of
      // checks elsewhere on C stay inside their cards and draw no text over another.
      expect(
        await boxesPastTheirCard(page),
        `an element reaches past its card with statement A opened at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page),
        `two pieces of text are drawn over each other with statement A opened at ${width} px`
      ).toEqual([]);
      if (width === 1440) {
        await tabTo(
          link,
          `Tab from statement A's "more" does not reach the link in the check's words at ${width} px`
        );
      }

      // The link is an `a` with its address, in the words' colour, underlined in #A8A29E.
      await expect(
        link,
        `the check's words draw no link to the runbook at ${width} px`
      ).toHaveCount(1);
      const look = await linkLook(link);
      expect(look.color, `the link is not in the check's words' colour at ${width} px`).toBe(
        look.words
      );
      expect(look.line, `the link is not underlined at ${width} px`).toContain("underline");
      expect(
        look.underline,
        `the link is underlined in another colour than #A8A29E at ${width} px`
      ).toBe("rgb(168, 162, 158)");

      // No text reads an address: the check's reference to B is the chip that names B by its
      // first words, as a chip names a block it cites (`RowText`'s `chipName`).
      const referenceToB = statementA.locator(
        `[data-verification-check-words] a[data-row-reference="${seeded.referenceToB}"]`
      );
      await expect(
        referenceToB,
        `the check's words under A draw no chip for B at ${width} px`
      ).toHaveCount(1);
      await expect(
        referenceToB,
        `the chip for B in the check's words does not name B by its first words at ${width} px`
      ).toHaveText(chipWords(STATEMENT_B_WORDS));
      await expect
        .poll(() => verificationBody(page).innerText(), {
          message: `the view draws a dispatch:// address as words at ${width} px`,
        })
        .not.toContain("dispatch://");

      // The result names the agent that recorded it by its role's word.
      await expect(
        statementA.locator("[data-verification-result] [data-node-author]"),
        `the result does not name its agent by its role at ${width} px`
      ).toHaveText("Recorder");

      // "Also under" names B by its first words, cut at a word within 32 characters.
      const alsoUnder = statementA.locator("[data-verification-check] [data-node-trail]");
      const chip = alsoUnder.locator("[data-node-chip]");
      await expect(
        chip,
        `the check under A names no other statement it serves at ${width} px`
      ).toHaveCount(1);
      const named = (await chip.innerText()).trim();
      await expect(
        alsoUnder,
        `the check under A does not say it is also under B at ${width} px`
      ).toHaveText(`${CONTROL_LABELS.also_under} ${named}`);
      expect(named, `"Also under" names B by no words at ${width} px`).not.toBe("");
      expect(
        named.length,
        `"Also under" names B by more than 32 characters at ${width} px: "${named}"`
      ).toBeLessThanOrEqual(32);
      expect(
        STATEMENT_B_WORDS.startsWith(named.replace(/…$/u, "")),
        `"Also under" names B by other than its first words at ${width} px: "${named}"`
      ).toBe(true);

      // C is covered by the child's check alone: it carries no gap, says so in one line, and the
      // summary counts it covered.
      expect(
        cards.find((card) => card.id === c)?.gapKinds,
        `statement C carries a gap at ${width} px`
      ).toEqual([]);
      await expect(
        verificationCard(page, c).locator("[data-verification-elsewhere]"),
        `statement C does not say a check of the issue under this one serves it at ${width} px`
      ).toHaveText(`1 ${VERIFICATION_WORDS.elsewhere_one}`);
      await expect(
        page.locator("[data-verification-covered]"),
        `statement C is not counted covered at ${width} px`
      ).toHaveText(`3 ${VERIFICATION_WORDS.of} 3 ${VERIFICATION_WORDS.covered}`);

      // With the check's facts open under A, which the focus on its words opens, every element
      // still stays inside its card and no two texts overlap.
      const checkUnderA = statementA.locator("[data-verification-check]");
      await checkUnderA.locator("[data-verification-check-words]").focus();
      await expect(
        rowBody(checkUnderA).first(),
        `the focus on the check's words under A does not open its facts at ${width} px`
      ).toBeVisible();
      expect(
        await boxesPastTheirCard(page),
        `an element reaches past its card with the check's facts open under A at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page),
        `two pieces of text are drawn over each other with the check's facts open under A at ${width} px`
      ).toEqual([]);
    }
  });
});

// Row 4. From the issue's own address `t k` turns the view on with the focus on the issue's row
// and the view's top on screen, and asks the server for nothing the first load did not; Tab
// alone then reaches the chips, a check's words, the copy control its facts draw and its line of
// earlier results, in reading order, and Enter works each one; the tree answers the outline's
// keys, and a press on a result's cell or on a statement's words opens a check's facts; axe finds
// no break of the tree's structure; `t k` from a row turns the view off; and the view's own
// address leaves the focus on the issue's row.
// SLOW(>=2s): a whole seeded issue loaded and waited out to a quiet window twice, a walk of up to
// 40 Tabs and some twenty keys and presses each waited on, an axe pass, and a third load.
test("the Verification view is worked by the keyboard alone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
    await recordPass(seeded, "1 passed, a second time");
    const [{ slug: specSlug }, { slug: verificationSlug }, coverage, results] = await Promise.all([
      getArtifact(seeded.specArtifactId),
      getArtifact(seeded.verificationArtifactId),
      getIssueCoverage(seeded.key),
      getIssueResults(seeded.key),
    ]);
    const nodeOf = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const checkOf = (index: number) =>
      blockNodeId(verificationSlug, seeded.checks.all[index - 1] ?? "");
    const statements = coverage.requirements ?? [];
    const command =
      (results.checks ?? []).find((one) => one.block_id === seeded.checks.passed)?.run ?? "";
    expect(command, "check 1 holds no command").not.toBe("");

    // From the issue's own address, with the focus on the page, `t k` turns the view on, leaves
    // the focus on the issue's row inside it with the view's top on screen under the header's
    // band, and asks the server for nothing the first load did not.
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectLoadSettled(traffic);
    await expectIssuePageDrawn(page);
    const loaded = new Set(traffic.requests.map((request) => request.path));
    traffic.reset();
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("k");
    const body = verificationBody(page);
    await expect
      .poll(() => focusInside(body), {
        message: "t k left the focus outside the Verification view",
      })
      .toBe(true);
    await expect(page, "t k did not open the Verification view's address").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "verification")}$`)
    );
    await expect(
      verificationIssueRow(page),
      "t k left the focus on another element of the view than the issue's row"
    ).toBeFocused();
    // The header's band, where it stays at the top of the screen; 0 where the page draws none
    // or it does not stay (`identityBlockBottom`).
    const band = (await identityBlockBottom(page)) ?? 0;
    const top = await body.evaluate((element) => element.getBoundingClientRect().top);
    expect(
      top,
      `t k left the view's top under the header's band, which ends at ${band} px`
    ).toBeGreaterThanOrEqual(band);
    expect(top, "t k left the view's top below the screen").toBeLessThan(
      page.viewportSize()?.height ?? 0
    );
    await expectLoadSettled(traffic);
    expect(
      traffic.requests
        .filter((request) => !loaded.has(request.path))
        .map((request) => `${request.method} ${request.path}`),
      "t k sent a request under /api/ to a path the first load did not"
    ).toEqual([]);

    // Tab alone, at most 40 presses, reaches each control in reading order, and Enter works it.
    const tabTo = tabWalk(page, 40);
    expect(
      await tabTo(verificationChip(page, "all"), "Tab does not reach the chip All"),
      "the first Tab after t k reaches another control than the chip All"
    ).toBe(1);
    await tabTo(verificationChip(page, "not_met"), "Tab does not reach the chip Not met");
    await page.keyboard.press("Enter");
    await expect
      .poll(async () => (await verificationCards(page)).map((card) => card.id), {
        message: "Enter on the chip Not met does not draw statement 2 alone",
      })
      .toEqual([nodeOf(2)]);
    await page.keyboard.press("Enter");
    await expect
      .poll(async () => (await verificationCards(page)).length, {
        message: "a second Enter on the chip Not met does not draw every statement",
      })
      .toBe(statements.length);
    const check1 = verificationCheck(page, checkOf(1));
    await tabTo(
      check1.locator("[data-verification-check-words]"),
      "Tab does not reach check 1's words"
    );
    await expect(
      rowBody(check1).first(),
      "the focus on check 1's words does not open its facts"
    ).toBeVisible();
    await tabTo(
      rowBody(check1)
        .getByRole("button", { exact: true, name: CONTROL_LABELS.copy_command })
        .first(),
      "Tab does not reach the copy control of check 1's command"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()), {
        message: "Enter on the copy control does not put check 1's whole command on the clipboard",
      })
      .toBe(command);
    await tabTo(
      check1.locator("[data-verification-earlier]"),
      "Tab does not reach check 1's line of earlier results"
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => check1.locator("[data-verification-result]").count(), {
        message: "Enter on check 1's line of earlier results does not draw two results",
      })
      .toBe(2);

    // The tree's keys, counted from the row the focus is on or from the row that holds the
    // control it is on.
    await page.keyboard.press("Home");
    await expect(verificationIssueRow(page), "Home does not focus the issue's row").toBeFocused();
    await page.keyboard.press("j");
    await expect(
      verificationCard(page, nodeOf(1)),
      "j does not move from the issue's row to statement 1's"
    ).toBeFocused();
    await page.keyboard.press("ArrowRight");
    await expect(check1, "ArrowRight does not move from statement 1 to its check").toBeFocused();
    await expect(
      rowBody(check1).first(),
      "the focus on check 1's row does not draw its facts"
    ).toBeVisible();
    await page.keyboard.press("Enter");
    await expect(check1, "Enter on check 1's row does not shut its facts").toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await expect(rowBody(check1), "check 1 draws facts it shut").toHaveCount(0);
    await page.keyboard.press("Enter");
    await expect(check1, "a second Enter on check 1's row does not open its facts").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await page.keyboard.press("j");
    await expect(check1, "j does not move on from check 1's row").not.toBeFocused();
    await expect(check1, "j shut the facts that Enter opened on check 1").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await page.keyboard.press("End");
    await expect(
      body.locator("[data-node-row]").last(),
      "End does not reach the view's last row"
    ).toBeFocused();

    // A check's facts open from a press on its latest result's cell and on its statement's words.
    const check2 = verificationCheck(page, checkOf(2));
    await expect(check2, "check 2's facts are drawn before a press").toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await check2.locator("[data-verification-result]").first().click();
    await expect(
      check2,
      "a click on check 2's latest result does not open its facts"
    ).toHaveAttribute("aria-expanded", "true");
    const check3 = verificationCheck(page, checkOf(3));
    await expect(check3, "check 3's facts are drawn before a press").toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await rowWords(verificationCard(page, nodeOf(3))).click();
    await expect(
      check3,
      "a click on statement 3's words does not open check 3's facts"
    ).toHaveAttribute("aria-expanded", "true");

    // The tree's structure, by the rules that read it.
    const scan = await new AxeBuilder({ page })
      .include("[data-verification-body]")
      .withRules([
        "aria-required-children",
        "aria-required-parent",
        "nested-interactive",
        "aria-allowed-attr",
        "aria-valid-attr-value",
        "button-name",
        "link-name",
      ])
      .analyze();
    expect(
      scan.violations.flatMap((violation) =>
        violation.nodes.map(
          (node) =>
            `${violation.id} at ${node.target
              .map((part) => (Array.isArray(part) ? part.join(" ") : String(part)))
              .join(" ")}`
        )
      ),
      "axe finds the view's tree breaking a rule of its structure"
    ).toEqual([]);

    // `t` then `k` with the focus on a row of the view turns it off: the tree's keys do nothing
    // while a chord is pending, so the page's keymap takes the second key.
    await verificationCard(page, nodeOf(1)).focus();
    await page.keyboard.press("t");
    await page.keyboard.press("k");
    await expect(page, "t k from a row of the view does not turn it off").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "conversation")}$`)
    );
    await expect(body, "t k from a row of the view leaves the view drawn").toHaveCount(0);

    // The view's own address, opened with nothing focused, leaves the focus on the issue's row.
    await gotoIssue(page, presetPath(seeded.key, "verification"));
    await expectOpeningLanded(page);
    await expect(
      verificationIssueRow(page),
      "the view's own address leaves the focus elsewhere than on the issue's row"
    ).toBeFocused();
  });
});

// The slot (`slots/view-verification.tsx`) once counted its document's blocks read as answered
// by a latch of its own, set from that read's `isSuccess` and empty each time the stage mounts
// the slot again. So once a background refetch of that read failed, a reader who came back to
// the view found it drawing nothing for as long as the refetch kept failing, though the blocks
// it had read were still held. A read the view has had an answer from stays answered while it
// holds that answer, across a failed refetch and across the view turned off and on. The outline
// mounted within the page's 30 s stale time asks nothing again, so a comment posted in the
// Verification document is what refreshes its reads, as it does for a reader
// (`api/event-refresh.ts`).
// SLOW(>=2s): a whole seeded issue loaded and settled, then its blocks read refused three times
// with the page's retry waits between them while the outline is drawn, and the view turned off and
// on by its keys.
test("the Verification view keeps its rows when its document's blocks read fails after it answered", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The view once every read has answered: each of the six statements draws the one check
    // that serves it.
    const { ids, truth } = await openSettledView(page, seeded);
    const cards = await verificationCardBoxes(page).count();
    const { slug: verificationSlug } = await getArtifact(seeded.verificationArtifactId);

    // From here on the Verification document's blocks read is refused, as many times as the page
    // asks it.
    const blocksRead = await refuseBlocksRead(page, seeded.verificationArtifactId);

    // The view off, from a row of it: the outline takes its place.
    await verificationCard(page, ids[0] ?? "").focus();
    await page.keyboard.press("t");
    await page.keyboard.press("k");
    await expect(page, "t k from a row of the view does not turn it off").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "conversation")}$`)
    );
    await expect(verificationBody(page), "t k from a row of the view leaves it drawn").toHaveCount(
      0
    );
    await expectIssuePageDrawn(page);

    // The comment's event refreshes the document's reads, and the page asks its blocks again and
    // twice more after the first refusal (`queryRetry`), which fails the read with its blocks held.
    await createComment(seeded.key, {
      anchor: { artifact: verificationSlug, quote: "How the issue page is proved." },
      body: "Read this before the checks.",
    });
    await expect
      .poll(() => blocksRead.refusals(), {
        message: "the page does not ask the Verification document's blocks again, three times",
      })
      .toBeGreaterThanOrEqual(3);
    await nextFrames(page);

    // The view on again, from the page: it draws every card it drew, and each statement the check
    // that serves it.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("k");
    await expect(page, "t k from the page does not turn the view on again").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "verification")}$`)
    );
    await expect(
      verificationCardBoxes(page),
      "the view drew nothing once its blocks read failed after it had answered"
    ).toHaveCount(cards);
    await expect
      .poll(() => readServed(page, ids), {
        message: "a statement draws otherwise once the blocks read failed after the view answered",
      })
      .toEqual(truth);
    await blocksRead.lift();
  });
});

// A check learns the statements it serves from the blocks read of the Verification document,
// which `useIssueTree` reads, and the view once drew its rows as soon as the coverage and results
// reads had answered, whether or not that read had. So while it was out, and for good once it
// failed, the view drew every check of the issue as one that serves no statement, and each
// statement as served by none of them. While that read is held the view draws nothing its reads
// do not say once they have all answered, and once it answers each statement draws its check. A
// read refused from the first load holds nothing to draw, and the view once drew an empty tree
// for it, which says nothing a reader can act on. So the view says, over its tree, that it could
// not load the verification document, in one line with a Retry, and draws no card, no row and
// nothing its reads do not say; once the refusal is lifted, that Retry asks the read again and the
// view settles on each statement's check.
// SLOW(>=2s): a whole seeded issue loaded three times, two of them read for 1.5 s each while the
// blocks read is held or refused, then the refused read asked again by Retry.
test("the Verification view says nothing of a check's statements before the Verification document's blocks are read", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The view once every read has answered: each of the six statements draws the one check
    // that serves it, and no check is drawn apart from a statement.
    const { ids, truth } = await openSettledView(page, seeded);
    expect(truth.loose, "the settled view draws a check that serves no statement").toEqual([]);

    const blocksRoute = `**/api/v1/artifacts/${seeded.verificationArtifactId}/blocks`;
    const answered = (path: string) =>
      page.waitForResponse((response) => new URL(response.url()).pathname === path);
    // A full page load gets the coverage in the landing read, which stores it under the key the
    // view reads (`seedIssuePage`, `web/src/api/queries.ts`), so the coverage route is asked only
    // where the landing carried none; the results read has its own route on every load.
    const coverageAnswered = () =>
      page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          url.pathname === `/api/v1/issues/${seeded.key}/coverage` ||
          (url.pathname === "/api/v1/landing" && url.searchParams.get("key") === seeded.key)
        );
      });
    const readsAnswered = () =>
      Promise.all([coverageAnswered(), answered(`/api/v1/issues/${seeded.key}/results`)]);
    // Every contradiction of the settled view the page draws over `ms`, read every 100 ms.
    const contradictedOver = async (ms: number): Promise<string[]> => {
      const end = Date.now() + ms;
      const seen = new Set<string>();
      while (Date.now() < end) {
        // One reading at one instant is the point here: a contradiction drawn at any moment
        // fails the row.
        for (const one of contradictions(truth, await readServed(page, ids))) seen.add(one);
        // The gap between two readings of the view: a contradiction must not be drawn at any reading of the window, and the held or refused read keeps one on screen for the whole window, so a reading every 100 ms cannot miss it.
        await page.waitForTimeout(100);
      }
      return [...seen];
    };

    // The blocks read held: the coverage and results reads answer, the blocks read does not.
    const held = Promise.withResolvers<void>();
    const hold = async (route: Route) => {
      await held.promise;
      await route.continue();
    };
    await page.route(blocksRoute, hold);
    const heldReads = readsAnswered();
    await openPreset(page, seeded.key, "verification");
    await heldReads;
    expect(
      await contradictedOver(1500),
      "the view draws what its reads do not say while the Verification document's blocks read is out"
    ).toEqual([]);
    held.resolve();
    await expect
      .poll(() => readServed(page, ids), {
        message: "the view does not settle once the held blocks read answers",
      })
      .toEqual(truth);
    await page.unroute(blocksRoute, hold);

    // The blocks read refused from the first load, as many times as the page asks it (two
    // retries after the first): the view says so in one line with a Retry, over a tree that
    // holds no card and no row, and draws nothing its reads do not say.
    const blocksRead = await refuseBlocksRead(page, seeded.verificationArtifactId);
    const refusedReads = readsAnswered();
    await openPreset(page, seeded.key, "verification");
    await Promise.all([refusedReads, blocksRead.refused(3)]);
    const failedLine = verificationUnreadable(page, "blocks");
    const retry = failedLine.getByRole("button", { exact: true, name: "Retry" });
    await expect(
      failedLine,
      "the view draws no failed-read line with Retry once its blocks read failed from the first load"
    ).toHaveCount(1);
    await expect(
      failedLine,
      "the failed-read line does not say the view could not load the verification document"
    ).toContainText(STATE_LINES.verification_unreadable);
    await expect(retry, "the blocks read's failed-read line draws no Retry").toHaveCount(1);
    await expect(
      verificationUnreadable(page),
      "the view draws a failed-read line for a read that answered"
    ).toHaveCount(1);
    expect(
      await contradictedOver(1500),
      "the view draws what its reads do not say once the Verification document's blocks read failed"
    ).toEqual([]);
    await expect(
      verificationCardBoxes(page),
      "the view draws a card once its blocks read failed from the first load"
    ).toHaveCount(0);
    await expect(
      nodeRows(page),
      "the view draws a row once its blocks read failed from the first load"
    ).toHaveCount(0);

    // The refusal lifted, Retry asks the blocks read again, and the view settles on each
    // statement's check.
    await blocksRead.lift();
    await retry.click();
    await expect
      .poll(() => readServed(page, ids), {
        message: "Retry does not bring the view to its settled rows",
      })
      .toEqual(truth);
    await expect(
      verificationUnreadable(page),
      "the failed-read line stays once Retry brought the view its blocks"
    ).toHaveCount(0);
  });
});

// On a running build of the page, Tab from the first check's line of earlier results took 18
// presses through the second statement's links before it reached that statement's
// "more · 1,152 words", and the cut hid every one of them, so the focus was drawn nowhere on
// screen. A statement cut at six lines keeps the controls of its hidden words out of the tab
// order, and Tab in reading order reaches its "more" next; once "more" opens the words, a link in
// them takes the focus where it is drawn.
// SLOW(>=2s): an issue with one statement of 200 words seeded and its view loaded and walked by
// Tab.
test("the Verification view gives no tab stop to a link its cut statement hides", async ({
  browser,
}) => {
  const seed = async () => {
    await createProject({ key: PROJECT, name: "Issue layout" });
    return await createIssue({
      force: true,
      project: PROJECT,
      spec: `:::requirement{#r-cut}\n${longWords("stated", 200)} Read [the runbook](${RUNBOOK}) last.\n:::\n`,
      title: "A cut statement hides its last link",
    });
  };
  await phase(browser, seed, async (page, seeded) => {
    await openPreset(page, seeded.key, "verification");
    await expectOpeningLanded(page);
    const card = verificationCardBoxes(page).first();
    const words = card.locator("[data-node-words]").first();
    const more = card.locator("[data-node-more]");
    const link = words.locator(`a[href="${RUNBOOK}"]`);
    // The page measures a statement's cut once the statement stands within a screen of the
    // viewport, in the frame after it comes there, so it is brought there before its cut is read.
    await card.scrollIntoViewIfNeeded();
    await expect(more, "the statement of 200 words is not cut").toHaveText(/^more · /u);
    // Whether the focused element is inside the statement's words and outside the box the cut
    // draws them in.
    const focusHidden = () =>
      words.evaluate((element) => {
        const focused = document.activeElement;
        if (focused === null || !element.contains(focused)) return null;
        const box = element.getBoundingClientRect();
        const rect = focused.getBoundingClientRect();
        const drawn = rect.bottom > box.top && rect.top < box.bottom;
        return drawn ? null : (focused.textContent ?? "").trim();
      });
    expect(
      await link.evaluate((element) => {
        const box = element.closest("[data-node-words]")?.getBoundingClientRect();
        return box !== undefined && element.getBoundingClientRect().top >= box.bottom;
      }),
      "the cut does not hide the statement's last link"
    ).toBe(true);

    // From the issue's row, where the view's own address leaves the focus, Tab to "more".
    await expect(verificationIssueRow(page)).toBeFocused();
    const hidden: string[] = [];
    let reached = false;
    for (let pressed = 0; pressed < 20 && !reached; pressed += 1) {
      await page.keyboard.press("Tab");
      // Tab moves the focus within the press itself, so the focus read just after it is settled.
      const held = await focusHidden();
      if (held !== null) hidden.push(held);
      // Tab moves the focus within the press itself, so this one reading of it is settled.
      reached = await more.evaluate((element) => element === document.activeElement);
    }
    expect(hidden, "Tab puts the focus on a link the cut statement hides").toEqual([]);
    expect(reached, "Tab does not reach the cut statement's more in 20 presses").toBe(true);

    // Opened, the words draw the link, and the focus reaches it where it is drawn.
    await page.keyboard.press("Enter");
    await expect(more, "Enter on more does not open the statement").toHaveText(
      CONTROL_LABELS.read_less
    );
    await page.keyboard.press("Shift+Tab");
    await expect(
      link,
      "Shift+Tab from less does not reach the opened statement's link"
    ).toBeFocused();
    expect(await focusHidden(), "the opened statement's link is focused outside its words").toBe(
      null
    );
  });
});

// With the Verification document's blocks read refused from the first load, Enter on the
// failed-read line's Retry once left the focus on the page's body, both while the read was asked
// again and after it answered, and `j` then moved no row. A keyboard action never leaves the
// focus on the page's body: Retry pressed by the keyboard keeps the focus inside the view while
// the read is asked again, when it is refused again, and once it answers, and the tree then
// answers its keys.
// SLOW(>=2s): a whole seeded issue loaded with its blocks read refused, asked again three times
// with the page's retry waits, then let answer.
test("the Verification view keeps the focus in the view when Retry is pressed by the keyboard", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const blocksRead = await refuseBlocksRead(page, seeded.verificationArtifactId);
    // no opening focus: the view's take (`slots/stage.tsx`) focuses the refused read's Retry, not
    // a row, as the Retry is drawn, which the count below waits for; it gives that focus once more
    // a frame later, which the frames `focusThePage` waits before its blur cover.
    await openPreset(page, seeded.key, "verification");
    const body = verificationBody(page);
    const retry = verificationUnreadable(page, "blocks").getByRole("button", {
      exact: true,
      name: "Retry",
    });
    await expect(retry, "the refused blocks read draws no Retry").toHaveCount(1);

    // `t k` with a refused read: the focus goes to that read's Retry, on screen under the header's
    // band, and not to the empty tree, which has nothing to act on and from which Tab never
    // reaches Retry. The Verification document names this row for that reading. The view is on,
    // so the first `t k` turns it off and the second turns it on with the blocks read still
    // refused. Turning the view off and on asks the refused read again, and its line stays drawn
    // while the read runs.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("k");
    await expect(page, "the first t k did not turn the Verification view off").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "conversation")}$`)
    );
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("k");
    await expect(
      retry,
      "t k with the blocks read refused leaves the focus elsewhere than its Retry"
    ).toBeFocused();
    await expect(retry, "t k leaves the refused read's Retry off the screen").toBeInViewport();
    // The header's band, as row 4 reads it: 0 where the page draws none or it does not stay. The
    // Retry is brought on screen a frame after it takes the focus, so its top is polled.
    const band = (await identityBlockBottom(page)) ?? 0;
    await expect
      .poll(() => retry.evaluate((element) => element.getBoundingClientRect().top), {
        message: `t k left the refused read's Retry under the header's band, which ends at ${band} px`,
      })
      .toBeGreaterThanOrEqual(band);

    const onPageBody = () =>
      page.evaluate(
        () => document.activeElement === null || document.activeElement === document.body
      );

    // Enter on Retry while the read is still refused: the read is asked again and refused again,
    // its line drawn throughout, and at no reading is the focus on the page's body. A press on
    // Retry while its read runs asks nothing more (`FailedReadLine`), so Enter waits for the run
    // the view's return started to be refused; its Retry is busy (`aria-busy`) while its read runs.
    await expect(
      retry,
      "the blocks read the view's return asked again is still out"
    ).not.toHaveAttribute("aria-busy");
    await retry.focus();
    const asked = blocksRead.refusals();
    await page.keyboard.press("Enter");
    await nextFrames(page);
    expect(
      await onPageBody(),
      "Enter on Retry leaves the focus on the page's body while the read is asked again"
    ).toBe(false);
    await expect
      .poll(() => blocksRead.refusals() - asked, {
        message: "Retry does not ask the blocks read again",
      })
      .toBeGreaterThanOrEqual(3);
    await expect(retry, "the read refused again draws no Retry").toHaveCount(1);
    await nextFrames(page);
    expect(
      await onPageBody(),
      "Enter on Retry leaves the focus on the page's body once the read is refused again"
    ).toBe(false);
    expect(
      await focusInside(body),
      "Enter on Retry leaves the focus outside the view once the read is refused again"
    ).toBe(true);

    // The refusal lifted, Enter on Retry: the read answers and the view draws its rows, the
    // focus stays in the view, and the tree answers `j`. The refusal is lifted once the run the
    // last Retry started has ended refused, so this press asks the read again.
    await expect(retry, "the blocks read the Retry asked again is still out").not.toHaveAttribute(
      "aria-busy"
    );
    await blocksRead.lift();
    await retry.focus();
    await page.keyboard.press("Enter");
    await nextFrames(page);
    expect(
      await onPageBody(),
      "Enter on Retry leaves the focus on the page's body while the read that will answer is out"
    ).toBe(false);
    await expect
      .poll(() => verificationCardBoxes(page).count(), {
        message: "Retry does not bring the view its rows once the read answers",
      })
      .toBeGreaterThan(0);
    await nextFrames(page);
    expect(
      await onPageBody(),
      "Enter on Retry leaves the focus on the page's body once the read answered"
    ).toBe(false);
    expect(
      await focusInside(body),
      "Enter on Retry leaves the focus outside the view once the read answered"
    ).toBe(true);
    // Once the read answers, the focus is on the view's first row.
    await expect(
      verificationIssueRow(page),
      "once the read answered, Enter on Retry leaves the focus elsewhere than the view's first row"
    ).toBeFocused();
    await page.keyboard.press("j");
    await nextFrames(page);
    expect(
      await body.evaluate(
        (element) =>
          element.contains(document.activeElement) &&
          document.activeElement?.getAttribute("role") === "treeitem"
      ),
      "after Retry the tree does not answer j with a row"
    ).toBe(true);
  });
});
