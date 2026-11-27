import { DISPATCH_READ_ONLY_TOKEN } from "@legion/contracts";

import { baseUrl, createIssue, createProject } from "./api";
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
    await alicePage.getByRole("button", { name: "New token" }).click();
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

// @pr: Settings and read-only tokens: the form makes tokens that only read, their rows list what they may do, and Dispatch answers each as a token that only reads.
test("read-only tokens made in Settings list their scopes, read whoami, and are refused a comment @pr", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Read-only tokens" });
  const box = (label: string) =>
    page.getByRole("checkbox", { exact: true, name: `${label} (new token)` });
  const actAsYou = box("Act as you");
  const approve = box("Approve and control credentials");
  const readOnly = box("Read only");
  /** The mint form holds exactly `held`, and none of its boxes is disabled. */
  const expectFormHolds = async (held: readonly string[]) => {
    for (const [scope, each] of [
      ["person", actAsYou],
      ["approve", approve],
      ["read", readOnly],
    ] as const) {
      await expect(each).toBeChecked({ checked: held.includes(scope) });
      await expect(each).toBeEnabled();
    }
  };

  await page.goto("/settings");
  await expect(page.getByText("No agent tokens yet.")).toBeVisible();
  // A build before read-only tokens has no such box, so it fails here.
  await expect(readOnly, 'the form that makes a token has a "Read only" box').toBeVisible();
  await expectFormHolds(["person", "approve"]);

  // Ticking Read only clears Approve, and ticking Approve clears Read only. Neither box is
  // disabled for it: each press is taken, and the other box follows it.
  await readOnly.check();
  await expectFormHolds(["person", "read"]);
  await approve.check();
  await expectFormHolds(["person", "approve"]);

  // A token that only reads, for nobody: Read only ticked and Act as you cleared.
  await readOnly.check();
  await actAsYou.uncheck();
  await expectFormHolds(["read"]);
  const reader = await makeToken(page, "Reader");

  // A token that reads as its person. The form is back at its start after the mint.
  await expectFormHolds(["person", "approve"]);
  await readOnly.check();
  await expectFormHolds(["person", "read"]);
  const personalReader = await makeToken(page, "Personal reader");

  // Each row as Dispatch lists it after a reload, not as the mint's answer: its scopes as words.
  await page.reload();
  for (const [label, words] of [
    ["Reader", ["Read only"]],
    ["Personal reader", ["Act as you", "Read only"]],
  ] as const) {
    await expect(
      page.getByRole("list", { exact: true, name: `What ${label} may do` }).getByRole("listitem")
    ).toHaveText([...words]);
  }

  // Dispatch names each token read only, and refuses it a write.
  for (const [label, token, scopes] of [
    ["Reader", reader, ["read"]],
    ["Personal reader", personalReader, ["person", "read"]],
  ] as const) {
    const whoami = await whoamiWithToken(token);
    expect(whoami.status, `GET /api/v1/whoami with ${label}`).toBe(200);
    expect(await whoami.json(), `the scopes whoami names for ${label}`).toMatchObject({
      scopes: [...scopes],
    });
    const comment = await createCommentWithToken(issue.key, token);
    expect(comment.status, `POST /api/v1/issues/${issue.key}/comments with ${label}`).toBe(403);
    expect(await comment.json()).toMatchObject({ code: DISPATCH_READ_ONLY_TOKEN });
  }
});
