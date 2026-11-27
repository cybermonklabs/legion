import { mkdirSync } from "node:fs";

import { DISPATCH_READ_ONLY_TOKEN } from "@legion/contracts";

import { baseUrl, createIssue, createProject, e2eAgentToken, getIssue } from "./api";
import { runDispatch } from "./dispatch-cli";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

async function createAskWithToken(issue: string, token: string): Promise<Response> {
  return fetch(new URL(`/api/v1/issues/${issue}/asks`, baseUrl), {
    body: JSON.stringify({
      actor: { id: "architect-session", kind: "session", origin: { session_title: "Architect" } },
      question: "Which release gate should we use?",
    }),
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    method: "POST",
  });
}

async function whoamiWithToken(token: string): Promise<Response> {
  return fetch(new URL("/api/v1/whoami", baseUrl), {
    headers: { Authorization: `Bearer ${token}` },
  });
}

async function createCommentWithToken(issue: string, token: string): Promise<Response> {
  return fetch(new URL(`/api/v1/issues/${issue}/comments`, baseUrl), {
    body: JSON.stringify({
      actor: { id: "reader-session", kind: "session" },
      body: "Written with a read-only token.",
    }),
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    method: "POST",
  });
}

/** Presses New token with the label `label` and the boxes as they stand. Answers the token the
 *  page shows once, after checking it is the one the mint answered. */
async function makeToken(page: Page, label: string): Promise<string> {
  await page.getByLabel("Token label").fill(label);
  const minted = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/me/agent-tokens"
  );
  await page.getByRole("button", { name: "New token" }).click();
  const answer = await minted;
  expect(answer.status(), `the mint of ${label}`).toBe(201);
  const { token } = (await answer.json()) as { token: string };
  await expect(
    page.getByRole("status").filter({ hasText: "Copy this token now." }).locator("code").first()
  ).toHaveText(token);
  return token;
}

// This row carries no `@pr` tag: the pull-request lane's real-stack share is at its cap
// (`realStackBudget`, e2e/__tests__/pull-request-lane.test.ts), and the read-only token row below
// took its place there, since it drives the same Settings section. The full lane runs this one.
test("a personal agent token attributes its ask, remains private, and is revoked from Settings", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agent token attribution" });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const alicePage = await alice.newPage();
  const bobPage = await bob.newPage();

  try {
    await alicePage.goto("/settings");
    await alicePage.getByLabel("Token label").fill("Architect");
    await alicePage.getByRole("button", { exact: true, name: "New token" }).click();
    // textContent waits for the code element to attach, and an element's text is never null,
    // so the check below only narrows the type.
    const token = await alicePage.getByRole("status").locator("code").first().textContent();
    if (token === null) {
      throw new Error("new agent token was not rendered");
    }
    expect(token).toMatch(/^dsp_[A-Za-z0-9_-]{43}$/);
    const tokenPrefix = `dsp_${token.slice(4, 12)}…`;

    // The card tells the reader which directory a session reads its envoy.json from. Asserted
    // whole, so a variable the client no longer reads cannot be listed here unnoticed.
    await expect(alicePage.getByRole("status").getByText(/^If the file exists/)).toHaveText(
      "If the file exists, replace its dispatch section with this one and keep the rest. " +
        "A session under a named omp profile, or with CLAUDE_CONFIG_DIR set, reads the " +
        "envoy.json in that directory instead; any other session reads the file " +
        "ENVOY_CONFIG_FILE names when it is set."
    );

    const created = await createAskWithToken(issue.key, token);
    expect(created.status).toBe(201);

    await alicePage.goto("/");
    await expect(alicePage.getByText("Architect (for alice)")).toBeVisible();

    await bobPage.goto("/settings");
    await expect(bobPage.getByRole("cell", { name: tokenPrefix })).toHaveCount(0);
    await expect(bobPage.getByText("No agent tokens yet.")).toBeVisible();

    alicePage.once("dialog", (dialog) => dialog.accept());
    await alicePage.goto("/settings");
    await alicePage.getByRole("button", { name: "Revoke Architect" }).click();
    await expect(
      alicePage
        .getByRole("row", { name: new RegExp(`Architect ${tokenPrefix}`) })
        .getByText("Revoked")
    ).toBeVisible();

    const revoked = await createAskWithToken(issue.key, token);
    expect(revoked.status).toBe(401);
    expect(await revoked.json()).toMatchObject({ code: "TOKEN_REVOKED" });
  } finally {
    await alice.close();
    await bob.close();
  }
});

// A personal token names the projects it may use at the mint, and its row changes them. The full
// lane runs it: the pull-request lane's real-stack rows are at their budget of 150
// (e2e/__tests__/pull-request-lane.test.ts).
test("a personal agent token names the projects it may use, at the mint and from its row", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "SIDE", name: "Side work" });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();

  try {
    await page.goto("/settings");
    // A token names no project, and so uses the public projects only, until the reader picks.
    const newPicker = page.getByRole("button", { name: "Projects (new token)" });
    await expect(newPicker).toHaveText("Public projects only");
    await newPicker.click();
    await page.getByRole("option", { name: "CORE, Core" }).click();
    await page.keyboard.press("Escape");
    await expect(newPicker).toHaveText("CORE");
    await page.getByLabel("Token label").fill("Scoped");
    await page.getByRole("button", { exact: true, name: "New token" }).click();
    await expect(page.getByRole("status").locator("code").first()).toHaveText(
      /^dsp_[A-Za-z0-9_-]{43}$/
    );
    await expect(newPicker).toHaveText("Public projects only");

    // The row reads the list Dispatch stored, and its picker writes the list back whole.
    const rowPicker = page.getByRole("button", { name: "Projects (Scoped)" });
    await expect(rowPicker).toHaveText("CORE");
    await rowPicker.click();
    await page.getByRole("option", { name: "SIDE, Side work" }).click();
    await page.getByRole("option", { name: "CORE, Core" }).click();
    await page.keyboard.press("Escape");
    await expect(rowPicker).toHaveText("SIDE");
    await page.reload();
    await expect(page.getByRole("button", { name: "Projects (Scoped)" })).toHaveText("SIDE");
  } finally {
    await alice.close();
  }
});
