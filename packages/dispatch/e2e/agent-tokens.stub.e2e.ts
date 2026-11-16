/** The projects a personal agent token names, as Settings draws and sends them, read over a
 *  recording (packages/dispatch/AGENTS.md, "The stubbed layer").
 *
 *  The row asserts what the page does with fixed answers: a token's row reads the projects the
 *  token list names, the mint form names none until the reader picks, and the mint sends exactly
 *  the projects picked. That Dispatch stores the list, and a row's edit of it survives a reload,
 *  is the real-stack row's claim, in `agent-tokens.e2e.ts`, which runs on the full lane. */
import { expect } from "@playwright/test";

import { baseUrl, createProject, userHeaders } from "./api";
import { stubbed } from "./stub/stub-test";

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "SIDE", name: "Side work" });
  const minted = await fetch(new URL("/api/v1/me/agent-tokens", baseUrl), {
    body: JSON.stringify({ name: "Scoped", projects: ["CORE"] }),
    headers: { ...(await userHeaders("alice")), "Content-Type": "application/json" },
    method: "POST",
  });
  if (minted.status !== 201) {
    throw new Error(`minting the token Scoped answered ${minted.status}: ${await minted.text()}`);
  }
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
