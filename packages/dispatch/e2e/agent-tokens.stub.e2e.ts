/** The projects a personal agent token names, as Settings draws and sends them, read over a
 *  recording (packages/dispatch/AGENTS.md, "The stubbed layer").
 *
 *  The rows assert what the page does with fixed answers: a token's row reads the projects the
 *  token list names, the mint form names none until the reader picks, and the mint sends exactly
 *  the projects picked; a token that lists a project its reader no longer sees reads as such, and
 *  an edit of its scopes leaves its list alone. That Dispatch stores the list, and a row's edit of
 *  it survives a reload, is the real-stack row's claim, in `agent-tokens.e2e.ts`, which runs on the
 *  full lane. */
import { expect } from "@playwright/test";

import { baseUrl, createProject, userHeaders } from "./api";
import { sql } from "./psql";
import { stubbed } from "./stub/stub-test";

/** Mints alice's token `name` listing `projects`, at her browser. */
async function mintAs(name: string, projects: string[]): Promise<void> {
  const minted = await fetch(new URL("/api/v1/me/agent-tokens", baseUrl), {
    body: JSON.stringify({ name, projects }),
    headers: { ...(await userHeaders("alice")), "Content-Type": "application/json" },
    method: "POST",
  });
  if (minted.status !== 201) {
    throw new Error(`minting the token ${name} answered ${minted.status}: ${await minted.text()}`);
  }
}
const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "SIDE", name: "Side work" });
  await mintAs("Scoped", ["CORE"]);
  return { label: "Reviewer", token: "Scoped" };
});

test("a token's row reads the projects it names, and the mint sends the projects picked @pr", async ({
  page,
  scene,
}) => {
  await page.goto("/settings");
  await expect(page.getByRole("button", { name: `Projects (${scene.token})` })).toHaveText("CORE");

  // A new token names no project, and so uses the public projects only, until the reader picks.
  const newPicker = page.getByRole("button", { name: "Projects (new token)" });
  await expect(newPicker).toHaveText("Public projects only");
  await newPicker.click();
  await page.getByRole("option", { name: "SIDE, Side work" }).click();
  await page.keyboard.press("Escape");
  await expect(newPicker).toHaveText("SIDE");

  await page.getByLabel("Token label").fill(scene.label);
  const mint = page.waitForRequest(
    (request) =>
      request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/me/agent-tokens"
  );
  await page.getByRole("button", { exact: true, name: "New token" }).click();
  expect((await mint).postDataJSON()).toEqual({
    name: scene.label,
    projects: ["SIDE"],
    scopes: ["person", "approve"],
  });
  await expect(page.getByRole("status").locator("code").first()).toHaveText(
    /^dsp_[A-Za-z0-9_-]{43}$/
  );
  await expect(newPicker).toHaveText("Public projects only");
});

/** alice's token Hidden lists PRIV, which bob owns and made private after she minted it, so she
 *  no longer sees it: Dispatch answers her browser the token with no list and `projects_hidden`.
 *  This release's route refuses a private setting until the deployment's bus can hold it, so the
 *  row is written as that write writes it. */
const hiddenTest = stubbed(async () => {
  await createProject({ key: "PRIV", name: "Private work" }, { login: "bob" });
  await mintAs("Hidden", ["PRIV"]);
  await sql(`update projects set visibility = 'private' where key = 'PRIV'`);
  return { token: "Hidden" };
});

hiddenTest(
  "a token listing only projects its reader cannot see says so, and a scope edit leaves its list alone @pr",
  async ({ page, scene }) => {
    await page.goto("/settings");
    await expect(page.getByRole("button", { name: `Projects (${scene.token})` })).toHaveText(
      "No project you can see"
    );
    await expect(page.getByText("A change here drops the projects you cannot see.")).toBeVisible();

    const edit = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname.startsWith("/api/v1/me/agent-tokens/")
    );
    await page.getByLabel(`Approve and control credentials (${scene.token})`).click();
    expect((await edit).postDataJSON()).toEqual({ scopes: ["person"] });
  }
);

// The picker of a token whose every project is hidden from its reader opens with nothing ticked.
// The row's button saves the list the reader sees, here none, once the reader confirms it: the
// edit sends the empty list, and the token reads as one of the public projects only.
hiddenTest(
  "a token listing only projects its reader cannot see is made public-only by one confirmed press @pr",
  async ({ page, scene }) => {
    await page.goto("/settings");
    const picker = page.getByRole("button", { name: `Projects (${scene.token})` });
    await expect(picker).toHaveText("No project you can see");

    const asked: string[] = [];
    page.on("dialog", (dialog) => {
      asked.push(dialog.message());
      void dialog.accept();
    });
    const edit = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname.startsWith("/api/v1/me/agent-tokens/")
    );
    await page
      .getByRole("button", { name: `Drop the projects you cannot see (${scene.token})` })
      .click();
    expect((await edit).postDataJSON()).toEqual({ projects: [], scopes: ["person", "approve"] });
    expect(asked).toEqual([
      `Drop the projects you cannot see from ${scene.token}? It then uses the public projects only, and a project you cannot see cannot be added back.`,
    ]);
    await expect(picker).toHaveText("Public projects only");
    await expect(page.getByText("A change here drops the projects you cannot see.")).toHaveCount(0);
  }
);
