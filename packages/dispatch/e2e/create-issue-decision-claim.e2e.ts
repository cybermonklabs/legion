import { answerAsk, createAsk, createIssue, createProject } from "./api";
import { expect, test } from "./test";
import { asUser } from "./users";

/** The ask the check finds. Its question carries the rare words the draft title is typed with,
 *  and the issue that owns it carries none of them, so the ask is its owner's only match: the
 *  check lists one item, and that item is the answered question itself. */
const hangarQuestion = "Which zeppelin hangar stores the blimps over winter?";

/** The title of the issue the ask sits on, which shares no word with the question or the draft. */
const mastTitle = "Mooring mast paint rota";

/** The draft a person types, which finds the answered ask by its rare words. */
const draftTitle = "Zeppelin hangar for the blimps over winter";

/** The agent that opens the question, as a question to a person is opened, so alice answers an
 *  ask she did not ask herself. */
const session = {
  actor: { id: "e2e-decision-claim", kind: "session" as const },
  as: "agent" as const,
};

test("the dialog lists the answered ask it found and claims nothing about what settles the draft", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The mast is repainted each spring, before the sailing season.\n",
    title: mastTitle,
  });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "North" }, { label: "South" }], question: hangarQuestion },
    session
  );
  await answerAsk(ask.id, {
    expected_edited_at: null,
    selected: ["North"],
    text: "The north one, which holds them all winter.",
  });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();

    await page.locator("body").focus();
    await page.keyboard.press("c");
    const dialog = page.getByRole("dialog", { name: "Create issue" });
    await expect(dialog).toBeVisible();
    const title = dialog.getByRole("textbox", { name: "Title" });
    await expect(title).toBeFocused();
    const related = dialog.getByRole("region", { name: "Related items" });

    await title.fill(draftTitle);

    // What the section still does, once the check's answer has reached the page: it lists what
    // it found, under the link of the issue the answered ask sits on, with the ask's own state.
    await expect(related.getByRole("link", { name: `${issue.key} ${mastTitle}` })).toBeVisible();
    await expect(related.getByText("Answered", { exact: true })).toBeVisible();

    // What it no longer does: nothing on the dialog says one of those items answers this draft.
    // The match is loose on purpose: it catches the deleted line and the same claim in any other
    // wording the dialog could print.
    await expect(dialog.getByText(/may already answer/iu)).toHaveCount(0);
  } finally {
    await context.close();
  }
});
