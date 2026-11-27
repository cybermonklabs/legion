/** A document table whose cell names an issue with a long title, read for its width. The
 *  reference is drawn as the issue's title, and a table cell sizes its column to what it holds,
 *  so a cap on the reference that the cell cannot resolve would size the column to the whole
 *  title and scroll the table sideways. The layout rows let a table scroll inside its own box, so
 *  this row holds the table itself (dispatch://INFRA-5200). */
import { createIssue, createProject } from "./api";
import { pinFontFaces } from "./layout-check";
import { expect, test } from "./test";
import { withAlice } from "./users";

const LONG_TITLE =
  "Local testing workflow for Dispatch, with every header property production holds, measured on production at every width from 360 to 1920 pixels";

test.use({ locale: "en-US", timezoneId: "UTC" });

test("a document table whose cell names an issue with a long title is no wider than its column", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const named = await createIssue({ project: "CORE", title: LONG_TITLE });
  const document = await createIssue({
    project: "CORE",
    spec: [
      "The steps of the plan, one per row.",
      "",
      "| Step | Issue | Note |",
      "| --- | --- | --- |",
      `| one | ${named.key} | short |`,
      "| two | none | short |",
    ].join("\n"),
    title: "A plan whose table names an issue",
  });

  await withAlice(browser, async (page) => {
    // A 900 px document column, beside the sidebar and the comment margin.
    await page.setViewportSize({ height: 900, width: 1652 });
    await page.goto(`/issues/${document.key}/spec`);
    await pinFontFaces(page);
    const table = page.locator(".dispatch-doc .milkdown table").first();
    await expect(table).toBeVisible();
    await expect(table.locator(".dispatch-reference").first()).toContainText(
      LONG_TITLE.slice(0, 40)
    );
    const measured = await table.evaluate((element) => ({
      clientWidth: element.clientWidth,
      column: Math.round(element.parentElement?.getBoundingClientRect().width ?? 0),
      scrollWidth: element.scrollWidth,
    }));
    console.log(`TABLE ${JSON.stringify(measured)}`);
    expect(measured.column, "the document column's width").toBe(900);
    expect(measured.scrollWidth, "the table's width against its column").toBeLessThanOrEqual(
      measured.clientWidth + 1
    );
  });
});
