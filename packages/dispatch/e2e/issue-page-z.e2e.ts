/** the rows ask a command bar run of the server and change an issue inside it, and read what
 *  Details and the run's page draw for its asker and for another login, which the server's reads
 *  answer for the asker alone */
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  NODE_REFERENCES_WORDS,
  RUN_PAGE_WORDS,
  STATE_LINES,
} from "../web/src/features/issue-page/page-words";
import {
  buildDispatchReference,
  buildIssuePath,
  buildRunPath,
} from "../web/src/features/refs/routes";
import { gotoIssue, issuePage, openDetails, phase } from "./issue-page";
import { DESKTOP, PHONE } from "./issue-page-g";
import { marginSection } from "./issue-page-h";
import { withIssuePage } from "./issue-page-option";
import {
  commentReads,
  follows,
  LONG_RUN_QUESTION,
  listedEdgesOf,
  listedRowsOf,
  mainContent,
  notFoundText,
  noViewTreeReads,
  RUN_COMMENT,
  RUN_QUESTION,
  RUN_WORD,
  referenceReads,
  referencesControl,
  retryBeside,
  runReference,
  runRowsOf,
  seedLongRunIssue,
  seedRunIssue,
  seedRunReferences,
  targetReadsNaming,
  treeReadFailure,
} from "./issue-page-z";
import { measureLayout } from "./layout-measure";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { asUser } from "./users";

/**
 * A command bar run that changed an issue is a row of its asker's Referenced by in Details, and
 * the run's own page at `/runs/<id>` is its asker's alone.
 *
 * Every row seeds the run through the server (`seedRunIssue` and `seedRunReferences`,
 * `e2e/issue-page-z.ts`) and reads it on the real stack. The run's writes carry its header
 * (`GOING_RUN_HEADER`), which links the issue to the run. The tree read with no view
 * (`GET /api/v1/issues/{key}/tree`) answers the run to its asker alone, and Details draws it from
 * that read as a row of the asker's Referenced by. The run's page reads the run by its address
 * (`GET /api/v1/references?to=dispatch://interaction/<id>&kind=changed_by_run`), which the server
 * answers to the asker alone. Details and the run's page each name what a run's words cite in one
 * batched read of reference targets (`GET /api/v1/reference-targets`).
 */

// SLOW(>=2s): three issues, a run and its three writes seeded, then four loads of the issue:
// alice's on Conversation, hers on Graph, hers on Conversation again with the tree read with no
// view refused until its retries run out, and bob's on Conversation in a context of his own.
// The run alice asked that changed the issue is a row of her Referenced by, after the list's own
// rows, in the page's words and none of the graph's, its question whole, read from the tree read
// with no view and counted in the control with the list's own rows. The row counts the reads of
// the tree with no view and no kind the page sent, each by its first page (`noViewTreeReads`):
// on Conversation, which reads the tree through `kind`, its load sends none, the list's first
// opening one and its second opening none more; on Graph, which reads the tree with no view on
// its load, the list's opening sends none beyond Graph's own. That read refused leaves the list's
// own rows drawn with the failure and its Retry under them; and bob, who asked no run, reads the
// list's own rows alone. Which rows the list's own read holds is the server's and the list's to
// say (`listedEdgesOf`): the row reads that the citing issue's comment is one of them, and counts
// them as each reader's list draws them.
test("a command bar run that changed the issue is a reference for the person who ran it", async ({
  browser,
}) => {
  await phase(browser, seedRunIssue, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    const conversation = buildIssuePath({ key: seeded.key, kind: "conversation" });
    await gotoIssue(page, conversation);
    await expectLoadSettled(traffic);
    expect(noViewTreeReads(traffic), "Conversation's load sends no tree read with no view").toBe(0);
    await openDetails(page);
    const control = referencesControl(page);
    await expect(control, "Details draws no Referenced by control").toHaveText(
      PROPERTY_LABELS.referenced_by
    );
    await control.click();

    const runRow = runRowsOf(page);
    await expect(runRow.getByRole("link"), `the run's row reads ${RUN_WORD}`).toHaveText([
      RUN_WORD,
    ]);
    await expectLoadSettled(traffic);
    expect(
      noViewTreeReads(traffic),
      "the list's first opening did not read the tree with no view once"
    ).toBe(1);
    const listed = listedRowsOf(page);
    await expect(listed, "the list's own rows hold no row of the citing issue").toContainText([
      seeded.citing,
    ]);
    // The list's own rows are drawn together from its one read, which has answered once the
    // citing issue's row stands, so this count reads them settled (`listedEdgesOf`).
    const own = await listedEdgesOf(page).count();
    expect(
      await follows(listed.last(), runRow),
      "the run's row stands before the last of the list's own rows"
    ).toBe(true);
    // The run's row's "Only you" mark is drawn by later work, so no assertion reads it yet.
    await expect(runRow).toContainText(RUN_QUESTION);
    await expect(
      runRow.getByRole("button", { name: "Show more" }),
      "the run's row clamps its question behind Show more"
    ).toHaveCount(0);
    await expect(runRow).toContainText(NODE_REFERENCES_WORDS.edges.changed_by_run);
    const time = runRow.locator("time");
    await expect(time).toHaveCount(1);
    const datedAt = Date.parse((await time.getAttribute("datetime")) ?? "");
    expect(
      Math.abs(datedAt - Date.parse(seeded.runAt)),
      "the run's row is not dated when the run was asked"
    ).toBeLessThanOrEqual(1_000);
    await expect(runRow.getByRole("link")).toHaveAttribute("href", buildRunPath(seeded.run));

    // The run's row is the tree read's: the only reads of what references a node are the list's
    // own of the issue and Blocks' of the issues it blocks.
    const issueReference = buildDispatchReference({ key: seeded.key, kind: "issue" });
    const reads = referenceReads(traffic);
    expect(
      reads.filter(({ kind, to }) => to === issueReference && kind === null),
      "Details did not read the issue's references once"
    ).toHaveLength(1);
    expect(
      reads.filter(
        ({ kind, to }) => to !== issueReference || (kind !== null && kind !== "blocked_by")
      ),
      "the page read references beside the list's and Blocks'"
    ).toEqual([]);
    await expect(
      referencesControl(page, own + 1),
      "Referenced by does not count the run's row with the list's own rows"
    ).toBeVisible();
    const drawn = await marginSection(page, "references").innerText();
    for (const token of ["interaction", "changed_by_run", seeded.run]) {
      expect(drawn, `Details' references read "${token}"`).not.toContain(token);
    }

    // The list closed and opened again reads the tree no more: its run rows are the read its
    // first opening made.
    const counted = referencesControl(page, own + 1);
    await counted.click();
    await expect(runRow, "the list's control did not close the list").toHaveCount(0);
    await counted.click();
    await expect(
      runRow.getByRole("link"),
      `the list's second opening draws no run's row reading ${RUN_WORD}`
    ).toHaveText([RUN_WORD]);
    await expectLoadSettled(traffic);
    expect(
      noViewTreeReads(traffic),
      "the list's second opening read the tree with no view again"
    ).toBe(1);

    // Graph, loaded again, reads the tree with no view on its load, and the list's opening there
    // reads it no more: its run rows are Graph's read.
    traffic.reset();
    await gotoIssue(page, buildIssuePath({ key: seeded.key, kind: "graph" }));
    await expectLoadSettled(traffic);
    const graphs = noViewTreeReads(traffic);
    expect(graphs, "Graph's load did not read the tree with no view").toBeGreaterThanOrEqual(1);
    await openDetails(page);
    await referencesControl(page).click();
    await expect(
      runRow.getByRole("link"),
      `the list on Graph draws no run's row reading ${RUN_WORD}`
    ).toHaveText([RUN_WORD]);
    await expectLoadSettled(traffic);
    expect(
      noViewTreeReads(traffic),
      "the list's opening on Graph read the tree beyond Graph's own read"
    ).toBe(graphs);

    // The tree read with no view refused, on Conversation again: Conversation's read through
    // `kind` and the views still answer, and the list's own rows stand, with the failure and its
    // Retry under them.
    await page.route("**/api/v1/issues/*/tree*", (route) => {
      const query = new URL(route.request().url()).searchParams;
      return query.has("view") || query.has("kind")
        ? route.continue()
        : route.fulfill({
            body: JSON.stringify({ code: "INTERNAL", error: "refused" }),
            contentType: "application/json",
            status: 500,
          });
    });
    await gotoIssue(page, conversation);
    await openDetails(page);
    await referencesControl(page).click();
    await expect(
      listedEdgesOf(page),
      `the list's own rows are not the ${own} it drew before the tree read was refused`
    ).toHaveCount(own);
    await expect(listed, "the list's own rows hold no row of the citing issue").toContainText([
      seeded.citing,
    ]);
    const failure = treeReadFailure(page);
    await expect(failure, "Details does not say the tree read failed").toBeVisible();
    expect(
      await follows(listed.last(), failure),
      "the tree read's failure stands before the last of the list's own rows"
    ).toBe(true);
    await expect(retryBeside(failure)).toBeVisible();

    // Bob asked no run: his list holds its own rows alone, and its control counts those.
    const bob = await asUser(browser, "bob");
    try {
      await withIssuePage(bob, "bob");
      const his = await bob.newPage();
      await his.setViewportSize(DESKTOP);
      await gotoIssue(his, conversation);
      await openDetails(his);
      await referencesControl(his).click();
      await expect(
        listedRowsOf(his),
        "bob's list's own rows hold no row of the citing issue"
      ).toContainText([seeded.citing]);
      // His list's own rows are drawn together from its one read, which has answered once the
      // citing issue's row stands, so this count reads them settled (`listedEdgesOf`).
      const bobsOwn = await listedEdgesOf(his).count();
      expect(bobsOwn, `bob's list's own rows are not the ${own} alice's list drew`).toBe(own);
      await expect(
        referencesControl(his, bobsOwn),
        "bob's Referenced by counts a row beside the list's own rows"
      ).toBeVisible();
      await expect(
        marginSection(his, "references").locator("[data-extra-references]"),
        "bob's Details draws a run he did not ask"
      ).toHaveCount(0);
      await expect(
        marginSection(his, "references"),
        "bob's Details reads the question of a run he did not ask"
      ).not.toContainText(RUN_QUESTION);
    } finally {
      await bob.close();
    }
  });
});

// SLOW(>=2s): the seed of the row above, the run's page and the issue's in alice's context, and
// two contexts of their own - bob's, and alice's on today's page - each reading the not-found
// page.
// The run's page draws its asker's question, its answer's one line and what it changed from one
// read by the run's own address, and fits 390 px and 1440 px with nothing cut; the issue's run
// row leads to it; to bob, and to alice on today's page, the run's address is the page an
// unknown address draws.
test("the run's page is its asker's alone", async ({ browser }) => {
  await phase(browser, seedRunIssue, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    const address = buildRunPath(seeded.run);
    await page.goto(address);
    const run = issuePage(page);
    await expect(run, "the run's page holds its question").toContainText(RUN_QUESTION);
    const reference = runReference(seeded.run);
    expect(
      referenceReads(traffic).filter(({ to }) => to === reference),
      "the run's page did not read its run once, by its address and its kind"
    ).toEqual([{ kind: "changed_by_run", to: reference }]);
    await expect(run.getByRole("heading", { level: 1 })).toHaveText(RUN_WORD);
    await expect(run.locator("time")).toHaveCount(1);
    // The stack has no model, so the run's one frame is its `NO_MODEL_CONFIGURED` error.
    await expect(run.getByText(STATE_LINES.run_no_model, { exact: true })).toBeVisible();
    const changed = run.getByRole("region", { exact: true, name: RUN_PAGE_WORDS.what_changed });
    await expect(changed.getByRole("listitem")).toHaveCount(2);
    await expect(changed.locator(`a[href="${seeded.path}"]`)).toHaveCount(1);
    await expect(changed.getByRole("listitem").filter({ hasText: RUN_COMMENT })).toHaveCount(1);
    const text = await run.innerText();
    for (const token of ["NO_MODEL_CONFIGURED", "error", "interaction", "changed_by_run"]) {
      expect(text, `the run's page reads "${token}"`).not.toContain(token);
    }

    // The run's page drawn at 390 px and at 1440 px. Each width is loaded afresh, as a reader who
    // opens the address at that width loads it: the shell picks its layout by a media query
    // (`app.tsx`'s `isCompactViewport`), whose answer a resize reaches only in a later render, so
    // a reading taken straight after a resize could read the layout of the width before. Each
    // drawing is read by the layout rule's own measure (`e2e/layout-measure.ts`'s
    // `measureLayout`) once the page's reads have gone quiet and its fonts have loaded, since a
    // name or a face that lands later changes the width of a line: nothing makes the page scroll
    // sideways, and nothing is cut at a side without an ellipsis. The layout rows leave this page
    // out (`e2e/layout-routes.ts`'s `layoutUnread`), so no entry of `e2e/layout-breaches.ts` can
    // accept a breach of it. 1440 px comes last because it is the width `phase` gave the page,
    // which the rest of the row reads the page at.
    for (const viewport of [PHONE, DESKTOP]) {
      const at = `at ${viewport.width} px`;
      await page.setViewportSize(viewport);
      await page.goto(address);
      await expect(run, `the run's page holds its question ${at}`).toContainText(RUN_QUESTION);
      await expect(
        changed.getByRole("listitem").filter({ hasText: RUN_COMMENT }),
        `the run's page draws the comment the run wrote ${at}`
      ).toHaveCount(1);
      await expectLoadSettled(traffic);
      await page.evaluate(() => document.fonts.ready.then(() => undefined));
      const reading = await page.evaluate(measureLayout);
      expect(reading.viewport, `the run's page is read ${at}`).toBe(viewport.width);
      expect(reading.breaches, `the run's page fits ${at} with nothing cut`).toEqual([]);
    }

    // The issue's run row leads to the same address.
    await gotoIssue(page, seeded.path);
    await openDetails(page);
    await referencesControl(page).click();
    const link = runRowsOf(page).getByRole("link");
    await expect(link).toHaveAttribute("href", address);
    await link.click();
    await expect
      .poll(() => new URL(page.url()).pathname, "the run's row did not open the run's page")
      .toBe(address);
    await expect(issuePage(page)).toContainText(RUN_QUESTION);

    // To bob, the run's address and a run nobody asked are one page: the unknown address's.
    let unknown: string;
    const bob = await asUser(browser, "bob");
    try {
      await withIssuePage(bob, "bob");
      const his = await bob.newPage();
      await his.setViewportSize(DESKTOP);
      unknown = await notFoundText(his);
      for (const path of [address, buildRunPath(crypto.randomUUID())]) {
        await his.goto(path);
        await expect(mainContent(his), "bob reads the unknown run's page").toHaveText(unknown);
      }
    } finally {
      await bob.close();
    }

    // To alice on today's page, which draws no run's page, the address is that page too.
    const today = await asUser(browser, "alice");
    try {
      const hers = await today.newPage();
      await hers.setViewportSize(DESKTOP);
      await hers.goto(address);
      await expect(
        mainContent(hers),
        "alice reads her run's page with the new page turned off"
      ).toHaveText(unknown);
    } finally {
      await today.close();
    }
  });
});

// SLOW(>=2s): two issues, three comments, a run and its two writes seeded, then two loads in
// alice's context: the issue's on Conversation, with Details and its Referenced by opened, and the
// run's page.
// A run's question and the line of the comment it wrote name the three comments they cite by their
// words, each surface reading the three in one batched read of reference targets (decision 515)
// and none in a read of the comment alone (`GET /api/v1/comments/{id}`), which the code before
// that decision sent for each comment on each surface. Two choices keep each surface's batch its
// own. The run writes its comment on the second issue (`seedRunReferences`): the issue page's own
// tree read publishes the names of what its comments cite, so a comment on the issue whose Details
// is read would have the load name the three, and the run's row would read none of them; the row
// asserts that the load names none. And the row loads the address of the run's row's link, after
// it asserts that address, rather than pressing the link: a move inside the app keeps the query
// cache, and the run's page asks for the same three addresses under the key of the batch Details
// read, so it would send no read of its own.
test("a run's references are named in one batched read, on Details and on the run's page", async ({
  browser,
}) => {
  await phase(browser, seedRunReferences, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    await gotoIssue(page, buildIssuePath({ key: seeded.key, kind: "conversation" }));
    await expectLoadSettled(traffic);
    expect(
      targetReadsNaming(traffic, seeded.cited),
      "the issue's load read the names of the comments the run cites"
    ).toEqual([]);
    traffic.reset();
    await openDetails(page);
    await referencesControl(page).click();
    const runRow = runRowsOf(page);
    await expect(runRow.getByRole("link"), `the run's row reads ${RUN_WORD}`).toHaveText([
      RUN_WORD,
    ]);
    await expect(
      runRow,
      "the run's row does not name each comment its question cites by the comment's words"
    ).toContainText(seeded.namedQuestion);
    await expect(runRow, "the run's row draws an address its question cites").not.toContainText(
      "dispatch://"
    );
    await expectLoadSettled(traffic);
    expect(
      commentReads(traffic, seeded.ids),
      "Details read a comment the run cites by a read of its own"
    ).toEqual([]);
    expect(
      targetReadsNaming(traffic, seeded.cited),
      "Details did not name the three comments in one batched read"
    ).toEqual([seeded.cited]);

    const address = buildRunPath(seeded.run);
    await expect(runRow.getByRole("link")).toHaveAttribute("href", address);
    traffic.reset();
    await page.goto(address);
    const changed = issuePage(page).getByRole("region", {
      exact: true,
      name: RUN_PAGE_WORDS.what_changed,
    });
    await expect(
      changed.getByRole("listitem").filter({ hasText: seeded.namedComment }),
      "the run's page does not name each comment the run's comment cites by the comment's words"
    ).toHaveCount(1);
    await expect(
      changed,
      "the run's page draws an address the run's comment cites"
    ).not.toContainText("dispatch://");
    await expectLoadSettled(traffic);
    const reference = runReference(seeded.run);
    expect(
      referenceReads(traffic).filter(({ to }) => to === reference),
      "the run's page did not read its run once, by its address and its kind"
    ).toEqual([{ kind: "changed_by_run", to: reference }]);
    expect(
      commentReads(traffic, seeded.ids),
      "the run's page read a comment the run cites by a read of its own"
    ).toEqual([]);
    expect(
      targetReadsNaming(traffic, seeded.cited),
      "the run's page did not name the three comments in one batched read"
    ).toEqual([seeded.cited]);
  });
});

// SLOW(>=2s): an issue and a run seeded, then alice's load of the issue on Conversation and the
// list's opening.
// Spec R1: the asker's question is the run's row's excerpt, whole, with no clamp and no "Show
// more", however long it is. The tree read alone answers the runs (R4), so it must answer the
// question whole too: a question longer than the 240 characters the server cuts an item's text
// to beside an edge must not lose its end in Details.
test("a run's question longer than an item's excerpt is drawn whole in Details", async ({
  browser,
}) => {
  await phase(browser, seedLongRunIssue, async (page, seeded) => {
    await gotoIssue(page, buildIssuePath({ key: seeded.key, kind: "conversation" }));
    await openDetails(page);
    await referencesControl(page).click();
    const runRow = runRowsOf(page);
    await expect(runRow.getByRole("link"), `the run's row reads ${RUN_WORD}`).toHaveText([
      RUN_WORD,
    ]);
    await expect(runRow.getByRole("link")).toHaveAttribute("href", buildRunPath(seeded.run));
    await expect(runRow.locator("p"), "the run's row cut its question").toHaveText(
      LONG_RUN_QUESTION
    );
    await expect(runRow.getByRole("button", { name: "Show more" })).toHaveCount(0);
  });
});
