import { createIssue, createProject } from "./api";
import { expect, test } from "./test";
import { asUser } from "./users";

// Closing a picker that saves what the close committed must leave the reader where Escape put
// them. The save re-renders the header, and a trigger disabled while it runs cannot hold focus:
// the keyboard place would be gone a frame later, with nothing to say so. happy-dom has no
// browser focus or blur of a disabled control, which is why this is a browser row.
test("Escape after picking a label leaves focus on the labels trigger", async ({ browser }) => {
  await createProject({ key: "SCR", name: "Scratch" });
  const issue = await createIssue({ project: "SCR", title: "Labels focus row", labels: ["bug"] });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}`);
    const labels = page.getByTestId("issue-labels");
    await expect(labels.getByText("bug", { exact: true })).toBeVisible();
    const trigger = page.getByRole("button", { name: "Edit labels" });
    await trigger.click();
    await page.getByRole("option", { name: "bug" }).click();
    await page.keyboard.press("Escape");
    await expect(trigger).toBeFocused();
    // The save the close committed lands here, taking the chip away and re-rendering the
    // header around the trigger. Focus must still be where Escape put it.
    await expect(labels.getByText("bug", { exact: true })).toHaveCount(0);
    await expect(trigger).toBeFocused();
  } finally {
    await context.close();
  }
});
