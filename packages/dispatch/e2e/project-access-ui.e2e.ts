import { createProject } from "./api";
import { expect, test } from "./test";

// One owner's whole journey through who may see a project, on the real server: every write here
// is the server's to take or refuse (the owner check, the email check, the group's existence), a
// reload reads back what it stored, and giving the project away hands the owner check to bob, so
// the page alice still holds must disable every control it offered her.
// @pr: Who may see a project: the Access tab's and Settings → Groups' writes are the server's to take or refuse, and giving the project away must leave every control disabled.
test("the owner makes a group in Settings, names it and a person on the Access tab, takes both off, sees a refusal in the server's words, gives the project away and then finds every control disabled, and deletes the group @pr", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });

  // Settings → Groups: alice makes crew, and owns it.
  await page.goto("/settings");
  const settingsGroups = page.getByRole("region", { exact: true, name: "Groups" });
  await expect(settingsGroups.getByText("No groups yet.")).toBeVisible();
  await settingsGroups.getByLabel("Group name").fill("crew");
  await settingsGroups.getByLabel("Members", { exact: true }).fill("carol@example.com");
  await settingsGroups.getByRole("button", { name: "New group" }).click();
  const crewRow = settingsGroups.getByRole("row").filter({
    has: page.getByRole("cell", { exact: true, name: "crew" }),
  });
  await expect(crewRow.getByRole("cell", { exact: true, name: "alice (you)" })).toBeVisible();
  await expect(crewRow.getByRole("cell", { exact: true, name: "carol@example.com" })).toBeVisible();

  // The Access tab: alice made CORE, so she owns it, and its list names nobody yet.
  await page.goto("/projects/CORE/access");
  const access = page.getByRole("tabpanel", { name: "Access" });
  await expect(access.getByTestId("access-owner")).toHaveText("alice (you)");
  await expect(access.getByTestId("access-visibility")).toHaveText("Public");
  await expect(access.getByText("No one is named on this list.")).toBeVisible();
  await expect(access.getByText("No group is on this list.")).toBeVisible();

  await access.getByLabel("Add a person").fill("carol@example.com");
  await access.getByRole("button", { name: "Add person" }).click();
  const people = access.getByRole("list", { exact: true, name: "People" });
  await expect(people.getByRole("listitem")).toHaveCount(1);
  await expect(people).toContainText("carol@example.com");
  await access.getByLabel("Add a group").selectOption("crew");
  await access.getByRole("button", { name: "Add group" }).click();
  const groups = access.getByRole("list", { exact: true, name: "Groups" });
  await expect(groups.getByRole("listitem")).toHaveCount(1);
  await expect(groups).toContainText("crew");

  // Read back from the server, not from what the page wrote into its cache.
  await page.reload();
  await expect(people).toContainText("carol@example.com");
  await expect(groups).toContainText("crew");

  await access.getByRole("button", { name: "Remove carol@example.com" }).click();
  await expect(access.getByText("No one is named on this list.")).toBeVisible();
  await access.getByRole("button", { name: "Remove crew" }).click();
  await expect(access.getByText("No group is on this list.")).toBeVisible();

  // The server refuses a person named by anything but an email, and the page says so in its words,
  // beside the field, which keeps what was typed.
  await access.getByLabel("Add a person").fill("bob");
  await access.getByRole("button", { name: "Add person" }).click();
  await expect(
    access.getByRole("region", { exact: true, name: "People" }).getByRole("alert")
  ).toContainText('"bob" is no email');
  await expect(access.getByLabel("Add a person")).toHaveValue("bob");

  // Giving the project to bob asks first, and then every control is bob's alone.
  page.once("dialog", (dialog) => void dialog.accept());
  await access.getByLabel("Give this project to").fill("bob");
  await access.getByRole("button", { name: "Give project" }).click();
  await expect(access.getByTestId("access-owner")).toHaveText("bob");
  const reason = "Only bob, the project's owner, changes who may see it.";
  await expect(access.getByRole("note")).toHaveText(reason);
  for (const field of ["Give this project to", "Add a person", "Add a group"]) {
    await expect(access.getByLabel(field)).toBeDisabled();
    await expect(access.getByLabel(field)).toHaveAttribute("title", reason);
  }
  for (const name of ["Give project", "Add person", "Add group"]) {
    await expect(access.getByRole("button", { name })).toBeDisabled();
    await expect(access.getByRole("button", { name })).toHaveAttribute("title", reason);
  }
  // The server holds it too: a reload reads bob as the owner.
  await page.reload();
  await expect(access.getByTestId("access-owner")).toHaveText("bob");
  await expect(access.getByRole("button", { name: "Add person" })).toBeDisabled();

  // Back in Settings, alice still owns crew and deletes it.
  await page.goto("/settings");
  page.once("dialog", (dialog) => void dialog.accept());
  await settingsGroups.getByRole("button", { name: "Delete crew" }).click();
  await expect(settingsGroups.getByText("No groups yet.")).toBeVisible();
});
