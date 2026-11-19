import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  NODE_REFERENCES_WORDS,
  RUN_PAGE_WORDS,
  STATE_LINES,
} from "../web/src/features/issue-page/page-words";
import { buildDispatchReference, buildRunPath } from "../web/src/features/refs/routes";
import { gotoIssue, issuePage, issueRead, openDetails, phase } from "./issue-page";
import { DESKTOP } from "./issue-page-g";
import { marginSection } from "./issue-page-h";
import { withIssuePage } from "./issue-page-option";
import {
  follows,
  listedRowsOf,
  mainContent,
  notFoundText,
  RUN_COMMENT,
  RUN_QUESTION,
  RUN_WORD,
  referenceReads,
  referencesControl,
  retryBeside,
  runReadServed,
  runReference,
  runRowsOf,
  seedRunIssue,
  treeReadFailure,
  treeRunServed,
} from "./issue-page-z";
import { expect, test } from "./test";
import { watchApiRequests } from "./traffic";
import { asUser } from "./users";

/**
 * A command bar run that changed an issue is a row of its asker's Referenced by in Details, and
 * the run's own page at `/runs/<id>` is its asker's alone.
 *
 * Both rows seed the run through the server (`seedRunIssue`, `e2e/issue-page-z.ts`) and read it
 * on the real stack: the run's writes carry its header, the outline's tree read answers the run
 * to its asker alone, and the run's page reads it at its own address. The stack this branch
 * builds serves none of the three yet - the run header on a write, the issue's tree read
 * (`GET /api/v1/issues/{key}/tree`) and the run's read by its address
 * (`GET /api/v1/references?to=dispatch://interaction/<id>&kind=changed_by_run`) reach it with
 * the server's branches that add them. So each row asks the server, once the seed is written,
 * for the reads it needs, and while one is not served the row is skipped with an annotation that
 * says what it owes: a skip, never a pass, and no tag, so the row runs once the server serves
 * those reads.
 */

// SLOW(>=2s): three issues, a run and its three writes seeded, then three loads of the issue:
// alice's, hers again with the tree read refused until its retries run out, and bob's in a
// context of his own.
// The run alice asked that changed the issue is a row of her Referenced by, after the list's own
// row, in the page's words and none of the graph's, read from the outline's tree read with no
// request of its own and counted in the control; a tree read that fails leaves the list's own
// row drawn with the failure and its Retry under it; and bob, who asked no run, reads the list's
// own row alone.
test("a command bar run that changed the issue is a reference for the person who ran it", async ({
  browser,
}) => {
  await phase(browser, seedRunIssue, async (page, seeded) => {
    test.skip(
      !(await treeRunServed(seeded.key)),
      "owed: the run's row in Details' Referenced by needs the server's issue tree read to answer the reader's command bar runs, and an issue update made for a run to link the issue to it"
    );
    const traffic = watchApiRequests(page);
    await gotoIssue(page, seeded.path);
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
    const listed = listedRowsOf(page);
    await expect(
      listed,
      "the list's own rows are not the citing issue's comment alone"
    ).toHaveCount(1);
    await expect(listed).toContainText(seeded.citing);
    expect(await follows(listed, runRow), "the run's row stands before the list's own").toBe(true);
    // The run's row's "Only you" mark is drawn by later work, so no assertion reads it yet.
    await expect(runRow).toContainText(RUN_QUESTION);
    await expect(runRow).toContainText(NODE_REFERENCES_WORDS.edges.changed_by_run);
    const time = runRow.locator("time");
    await expect(time).toHaveCount(1);
    const datedAt = Date.parse((await time.getAttribute("datetime")) ?? "");
    expect(
      Math.abs(datedAt - Date.parse(seeded.runAt)),
      "the run's row is not dated when the run was asked"
    ).toBeLessThanOrEqual(1_000);
    await expect(runRow.getByRole("link")).toHaveAttribute("href", buildRunPath(seeded.run));

    // The run's row is the outline's tree read: the only reads of what references a node are the
    // list's own of the issue and Blocks' of the issues it blocks.
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
      referencesControl(page, 2),
      "Referenced by does not count the run's row with the list's own"
    ).toBeVisible();
    const drawn = await marginSection(page, "references").innerText();
    for (const token of ["interaction", "changed_by_run", seeded.run]) {
      expect(drawn, `Details' references read "${token}"`).not.toContain(token);
    }

    // The tree read refused: the list's own row stands, with the failure and its Retry under it.
    await page.route("**/api/v1/issues/*/tree*", (route) =>
      route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "refused" }),
        contentType: "application/json",
        status: 500,
      })
    );
    await page.reload();
    await issueRead(page);
    await openDetails(page);
    await referencesControl(page).click();
    await expect(listed, "a refused tree read took the list's own row with it").toHaveCount(1);
    await expect(listed).toContainText(seeded.citing);
    const failure = treeReadFailure(page);
    await expect(failure, "Details does not say the tree read failed").toBeVisible();
    expect(
      await follows(listed, failure),
      "the tree read's failure stands before the list's own row"
    ).toBe(true);
    await expect(retryBeside(failure)).toBeVisible();

    // Bob asked no run: his list holds its own row alone, and its control counts that one.
    const bob = await asUser(browser, "bob");
    try {
      await withIssuePage(bob, "bob");
      const his = await bob.newPage();
      await his.setViewportSize(DESKTOP);
      await gotoIssue(his, seeded.path);
      await openDetails(his);
      await referencesControl(his).click();
      await expect(
        listedRowsOf(his),
        "bob's list is not the citing issue's comment alone"
      ).toHaveCount(1);
      await expect(listedRowsOf(his)).toContainText(seeded.citing);
      await expect(
        referencesControl(his, 1),
        "bob's Referenced by counts a row beside the list's own"
      ).toBeVisible();
      await expect(
        marginSection(his, "references").locator("[data-extra-references]"),
        "bob's Details draws a run he did not ask"
      ).toHaveCount(0);
    } finally {
      await bob.close();
    }
  });
});

// SLOW(>=2s): the seed of the row above, the run's page and the issue's in alice's context, and
// two contexts of their own - bob's, and alice's on today's page - each reading the not-found
// page.
// The run's page draws its asker's question, its answer's one line and what it changed from one
// read by the run's own address, and the issue's run row leads to it; to bob, and to alice on
// today's page, the run's address is the page an unknown address draws.
test("the run's page is its asker's alone", async ({ browser }) => {
  await phase(browser, seedRunIssue, async (page, seeded) => {
    // The row reads the run's page by its own read, then opens it from the issue's run row,
    // which the tree read draws: it needs both served.
    test.skip(
      !((await runReadServed(seeded.run)) && (await treeRunServed(seeded.key))),
      "owed: the run's own page and the run row that opens it need the server to answer a run's own read with its whole question, and the issue tree read to answer the reader's runs"
    );
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
