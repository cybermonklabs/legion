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
  await page.getByRole("button", { exact: true, name: "New token" }).click();
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

/** The `--priority` row of a command's help: the flag's own line and the lines of its
 *  description under it, up to the next flag. */
function priorityRow(help: string): string {
  const lines = help.split("\n");
  const start = lines.findIndex((line) => /^ {2}--priority\b/.test(line));
  expect(start, `the help has no --priority row:\n${help}`).toBeGreaterThan(-1);
  const next = lines.findIndex((line, index) => index > start && /^ {2}--/.test(line));
  return lines.slice(start, next === -1 ? undefined : next).join("\n");
}

// This row carries no `@pr` tag: the pull-request lane's real-stack share is at its cap
// (`realStackBudget`, e2e/__tests__/pull-request-lane.test.ts), so the full lane runs it.
// SLOW(>=2s): seven cold `bun` starts of the real `dispatch` command around minting and widening
// personal tokens in Settings.
test("Settings gives p0 only where its box is ticked, and the command sets P0 only under a token that holds it", async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  // Each credential writes an issue of its own, seeded at priority 2 as alice, so no earlier step
  // leaves a later step writing priority 0 onto an issue already at P0.
  const [first, second, third, fourth] = [
    await createIssue({ priority: 2, project: "CORE", title: "Rotate the signing key" }),
    await createIssue({ priority: 2, project: "CORE", title: "Drain the stale worker pool" }),
    await createIssue({ priority: 2, project: "CORE", title: "Trim the audit log retention" }),
    await createIssue({ priority: 2, project: "CORE", title: "Verify the backup restore" }),
  ] as const;

  const home = testInfo.outputPath("dispatch-home");
  mkdirSync(home, { recursive: true });

  await page.goto("/settings");
  await expect(page.getByText("No agent tokens yet.")).toBeVisible();
  const newTokenSetP0 = page.getByRole("checkbox", { exact: true, name: "Set P0 (new token)" });
  // A build before p0 draws no Set P0 box on the mint form, so it fails here.
  await expect(
    newTokenSetP0,
    'the mint form has a "Set P0 (new token)" box once whoami answers'
  ).toBeVisible();
  await expect(newTokenSetP0).not.toBeChecked();
  await expect(
    page.locator("form").getByText("Set an issue's priority to P0.", { exact: true })
  ).toBeVisible();

  const withoutP0 = await makeToken(page, "Without P0");
  await newTokenSetP0.check();
  const withP0 = await makeToken(page, "With P0");
  await expect(newTokenSetP0).not.toBeChecked();

  // Whoami names p0 for "With P0" alone among the three credentials.
  for (const [label, token, holdsP0] of [
    ["With P0", withP0, true],
    ["Without P0", withoutP0, false],
    ["shared token", e2eAgentToken, false],
  ] as const) {
    const whoami = await whoamiWithToken(token);
    expect(whoami.status, `GET /api/v1/whoami with ${label}`).toBe(200);
    const { scopes } = (await whoami.json()) as { scopes: string[] };
    expect(scopes.includes("p0"), `whoami scopes for ${label}: ${scopes.join(",")}`).toBe(holdsP0);
  }

  // The command's help names p0 on the --priority row under each credential.
  for (const [label, token] of [
    ["With P0", withP0],
    ["Without P0", withoutP0],
    ["shared token", e2eAgentToken],
  ] as const) {
    const help = await runDispatch(["issue-update", "--help"], home, { token });
    expect(help.code, `issue-update --help under ${label}: ${help.stdout}${help.stderr}`).toBe(0);
    expect(priorityRow(help.stdout), `the --priority row under ${label}`).toMatch(/\bp0\b/);
  }

  // Both credentials without p0 are refused priority 0, and each issue keeps priority 2.
  for (const [label, token, issue] of [
    ["Without P0", withoutP0, first],
    ["shared token", e2eAgentToken, second],
  ] as const) {
    const refused = await runDispatch(
      ["issue-update", "--issue", issue.key, "--priority", "0"],
      home,
      { token }
    );
    expect(refused.code, `P0 under ${label}: ${refused.stdout}${refused.stderr}`).toBe(1);
    const lines = `${refused.stdout}\n${refused.stderr}`.split("\n");
    expect(
      lines.some((line) => line.includes("OAUTH_SCOPE_REQUIRED") && /\bp0\b/.test(line)),
      `refusal under ${label} names OAUTH_SCOPE_REQUIRED and p0: ${refused.stdout}${refused.stderr}`
    ).toBe(true);
    expect((await getIssue(issue.key)).priority, `${issue.key} after refusal under ${label}`).toBe(
      2
    );
  }

  // The token minted with Set P0 ticked sets priority 0.
  const allowed = await runDispatch(
    ["issue-update", "--issue", third.key, "--priority", "0"],
    home,
    { token: withP0 }
  );
  expect(allowed.code, `P0 under With P0: ${allowed.stdout}${allowed.stderr}`).toBe(0);
  expect((await getIssue(third.key)).priority).toBe(0);

  // Giving p0 to "Without P0" on its row widens it in place, and the command then sets priority 0.
  const rowSetP0 = page.getByRole("checkbox", { exact: true, name: "Set P0 (Without P0)" });
  await expect(rowSetP0).toBeVisible();
  await expect(rowSetP0).not.toBeChecked();
  const patched = page.waitForResponse(
    (response) =>
      response.request().method() === "PATCH" &&
      /^\/api\/v1\/me\/agent-tokens\/[^/]+$/.test(new URL(response.url()).pathname)
  );
  await rowSetP0.click();
  expect((await patched).status(), "PATCH scopes of Without P0").toBe(200);
  await expect(rowSetP0).toBeChecked();

  const widenedWhoami = await whoamiWithToken(withoutP0);
  expect(widenedWhoami.status, "GET /api/v1/whoami after widening Without P0").toBe(200);
  const widened = (await widenedWhoami.json()) as { scopes: string[] };
  expect(widened.scopes, `widened scopes of Without P0: ${widened.scopes.join(",")}`).toContain(
    "p0"
  );

  const afterWiden = await runDispatch(
    ["issue-update", "--issue", fourth.key, "--priority", "0"],
    home,
    { token: withoutP0 }
  );
  expect(
    afterWiden.code,
    `P0 after widening Without P0: ${afterWiden.stdout}${afterWiden.stderr}`
  ).toBe(0);
  expect((await getIssue(fourth.key)).priority).toBe(0);
});
