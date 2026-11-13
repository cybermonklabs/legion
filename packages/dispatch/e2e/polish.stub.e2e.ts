/** The collapsed project filters and the compact composer a margin ask draws. The row reads
 *  only what the page draws from one fixed answer, so it runs against a recording rather than
 *  against a harness (packages/dispatch/AGENTS.md, "The stubbed layer").
 *
 *  Two rows of this family stayed in `polish.e2e.ts`, each with its reason written there.
 */
import { expect } from "@playwright/test";

import { createAsk, createIssue, createProject, patchIssue } from "./api";
import { needsYouCards } from "./editor";
import { filterPicker, pickFilterOption } from "./filters";
import { navigateInApp } from "./sends";
import { stubbed } from "./stub/stub-test";

const session = { actor: { id: "e2e-polish", kind: "session" as const }, as: "agent" as const };

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Polish the queue" });
  await patchIssue(issue.key, { labels: ["frontend"], status: "todo" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Which option should ship?" },
    session
  );
  return { ask: ask.id, issue: issue.key };
});

test("project filters stay collapsed until needed and margin asks use the compact composer @pr", async ({
  page,
  scene,
}, testInfo) => {
  await page.setViewportSize({ height: 800, width: 1280 });

  await page.goto("/projects/CORE");
  await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await page.screenshot({
    path: testInfo.outputPath("polish-project-list-1280.png"),
    fullPage: true,
  });

  await pickFilterOption(page, "Labels", "frontend");
  await expect(filterPicker(page, "Labels")).toHaveAttribute(
    "aria-label",
    "Filter Labels: 1 picked"
  );
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await page.getByRole("button", { name: "Board" }).click();
  await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
  const boardCard = page.getByRole("article", { name: `${scene.issue} Polish the queue` });
  await boardCard.scrollIntoViewIfNeeded();
  await expect(boardCard).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("polish-project-board-1280.png"),
    fullPage: true,
  });

  await navigateInApp(page, "/");
  const inboxCard = page.getByTestId(`ask-${scene.ask}`);
  await expect(inboxCard.getByRole("radio", { name: "Ship" })).toBeVisible();
  await expect(inboxCard.getByLabel("Your answer")).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("polish-inbox-1280.png"),
    fullPage: true,
  });

  await navigateInApp(page, `/issues/${scene.issue}`);
  // Read inside the issue page's own panel. The page fills the margin's slot in an effect, so
  // the margin draws once plain and again inside that panel, and a press that landed on the
  // first draw was thrown away with it: the disclosure's open state is the card's own, and
  // the replacement card came back shut. The panel is what says the card is the one to press.
  const marginCard = needsYouCards(page.getByTestId("issue-margin")).getByTestId(
    `ask-${scene.ask}`
  );
  const ownWords = marginCard.getByRole("button", {
    exact: true,
    name: "Add a note or answer in your own words",
  });
  await expect(marginCard.getByRole("radio", { name: "Ship" })).toBeVisible();
  await expect(ownWords).toHaveAttribute("aria-expanded", "false");
  await expect(marginCard.getByLabel("Your answer")).toBeHidden();
  await ownWords.click();
  // The control says the press landed before the box behind it is looked for.
  await expect(ownWords).toHaveAttribute("aria-expanded", "true");
  await expect(marginCard.getByLabel("Your answer")).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("polish-margin-1280.png"),
    fullPage: true,
  });
  await navigateInApp(page, "/agents");
  await expect(page.getByRole("region", { name: "Agents empty state" })).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("polish-agents-1280.png"),
    fullPage: true,
  });
});
