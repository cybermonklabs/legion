/** The search palette as the page draws one fixed answer: the control that opens it and the
 *  focus it hands back, the touch targets and the width a phone gives its rows, the lines it
 *  writes when it has nothing to list, and the group each owner row names.
 *
 *  Each reads what the browser does, so each runs against a recording rather than against a
 *  harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The rows whose claim is what the
 *  index found - the grouped hits, their marks, an ask hit's answer, and the two routes a hit
 *  opens - stayed in `search.e2e.ts`, with the keyboard-hints row, which is read in the desktop
 *  and the phone configurations at once.
 */
import { expect } from "@playwright/test";

import { answerAsk, createAsk, createIssue, createProject } from "./api";
import { openSearchFromRail, searchSession, searchTerm, seedSearchFixture } from "./search";
import { stubbed } from "./stub/stub-test";

/** The file's own three issues - a document, a comment and a message - with the two hits the
 *  term finds among them and a word that is in none of them. */
const test = stubbed(async () => ({
  ...(await seedSearchFixture()),
  hits: 2,
  missing: "zzqqxx",
}));

/** The same, with an answered ask whose answer runs past its three-line clamp, so the overflow
 *  check covers the ask hit's status line and answer text as well. */
const phoneTest = stubbed(async () => {
  const fixture = await seedSearchFixture();
  const ask = await createAsk(
    fixture.firstIssueKey,
    {
      options: [{ label: "On the chart table" }, { label: "In the captain's cabin" }],
      question: "Where does the astrolabe live between voyages?",
    },
    searchSession
  );
  await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: ["On the chart table"],
    text: "Keep the astrolabe on the chart table during every crossing, and recalibrate its rete against the meridian at local noon whenever the sky is clear enough to take a clean sighting. Log each reading beside the dead-reckoning position in the ship's book, note the observer and the instrument's serial number, and send any drift larger than half a degree to the instrument maker before the next voyage leaves port.",
  });
  return fixture;
});

/** One issue, whose row names the group its hits sit in. */
const groupTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Conversation view" });
  return { issue: issue.key };
});

test("the rail Search control opens the palette and Escape returns focus to it @pr", async ({
  page,
  scene,
}, testInfo) => {
  await openSearchFromRail(page, false);

  const dialog = page.getByRole("dialog", { name: "Search" });
  const search = page.getByRole("combobox", { name: "Search" });
  await expect(dialog).toBeVisible();
  await expect(search).toBeFocused();
  await search.fill(searchTerm);
  await expect(dialog.getByRole("option")).toHaveCount(scene.hits);
  await page.screenshot({
    fullPage: true,
    path: testInfo.outputPath(`search-palette-${testInfo.project.name}.png`),
  });

  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^search/i })).toBeFocused();
});

phoneTest(
  "phone search rows are at least 44px, do not overflow, and a comment result opens Conversation @phone @pr",
  async ({ page, scene }) => {
    await openSearchFromRail(page, true);
    await page.getByRole("combobox", { name: "Search" }).fill(searchTerm);

    // Scoped to the palette: inbox rows carry a priority <select> whose options share the role.
    const dialog = page.getByRole("dialog", { name: "Search" });
    const options = dialog.getByRole("option");
    await expect(options).toHaveCount(3);
    await expect(dialog.getByRole("option", { name: /^ask / })).toContainText(
      "Chose On the chart table"
    );
    for (let index = 0; index < (await options.count()); index += 1) {
      const box = await options.nth(index).boundingBox();
      expect(box, `result option ${index} has a layout box`).not.toBeNull();
      expect(box?.height ?? 0, `result option ${index} is a touch target`).toBeGreaterThanOrEqual(
        44
      );
    }
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);

    await dialog.getByRole("option", { name: /^comment / }).click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${scene.commentIssueKey}/comments/${scene.commentID}$`)
    );
    // The deep link loads the Conversation, then pages events until the comment is found; start
    // the text assertion's clock once the region is mounted so a loaded runner does not fail it.
    const conversation = page.getByRole("region", { name: "Conversation" });
    await expect(conversation).toBeVisible();
    // The comment link opens its thread full-screen over the Conversation; Back leaves it on the
    // focused turn.
    const thread = page.getByRole("dialog", { name: "Thread" });
    await expect(thread.getByText("Move the astrolabe diagram.")).toBeVisible();
    await thread.getByRole("button", { name: "Back" }).click();
    await expect(thread).toHaveCount(0);
    await expect(
      conversation.locator(`li[data-turn="comment:${scene.commentID}"][aria-current="true"]`)
    ).toContainText("Move the astrolabe diagram.");
    await expect(page.getByTestId("margin-sheet")).not.toContainText("Move the astrolabe diagram.");
  }
);

test("no results and short queries are stated, not silent @pr", async ({ page, scene }) => {
  await openSearchFromRail(page, false);
  const search = page.getByRole("combobox", { name: "Search" });

  await search.fill("a");
  await expect(page.getByText("Type at least 2 characters")).toBeVisible();
  await search.fill(scene.missing);
  await expect(page.getByText(`No results for "${scene.missing}"`)).toBeVisible();
});

// The hits of one owner are a group named by the row above them, so a reader who cannot see
// that row still hears which issue a hit belongs to.
groupTest("each palette group is named by its owner row @pr", async ({ page, scene }) => {
  await page.goto("/");
  await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await page.getByRole("combobox", { name: "Search" }).fill("conversation");
  const group = page.getByRole("group", { name: new RegExp(`^${scene.issue}: `) });
  await expect(group).toBeVisible();
  // The options are inside it, and the listbox still owns them.
  await expect(group.getByRole("option").first()).toBeVisible();
  // The owner row names the group and is not a second node in the accessibility tree, so a
  // reader hears the owner once rather than twice. `aria-labelledby` computes a name from a
  // hidden element, which is why the group above still has one.
  const row = page.locator(`[id="search-group-issue:${scene.issue}"]`);
  await expect(row).toHaveAttribute("aria-hidden", "true");
  // Exactly one node carries the owner's accessible name: the group.
  await expect(page.getByRole("group", { name: new RegExp(`^${scene.issue}: `) })).toHaveCount(1);
});
