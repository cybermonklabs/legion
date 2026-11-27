import { getSentMessages, plannerSession, resetFakeEnvoy, setLiveSessions } from "./agents";
import {
  createIssue,
  createProject,
  getIssue,
  getIssueArtifactText,
  getIssueResults,
  listIssueMessages,
  listIssues,
  replyToMessageDelivery,
  setNewIssueSettings,
} from "./api";
import { expect, type Locator, type Page, rowKey, rowSessionId, test } from "./test";
import { asUser } from "./users";

// Check k16 of dispatch://INFRA-1042/artifact/verification-md, serving R14: words that match
// nothing become an issue whose first content is the person's own declaration, with an agent on
// it. The bar offers, the person accepts, and only then is anything written.
//
// The one sentence of that check this spec does not read is "With the fake decision model
// returning 'file an issue' above the fitted threshold, no issue is created until the person
// accepts": the decision model belongs to another issue, and nothing here is model-triggered.
// What this spec reads is the explicit path - the person types, the bar offers, the person
// accepts - which is the same "nothing is written until the person accepts".

/** Words no seeded item holds, which is what makes the bar offer them as a new issue. Each is a
 *  key of the row's own (`rowKey`, e2e/test.ts): the bar searches every project of the stack,
 *  the issue an earlier row made from its words holds those words, and a query whose words all
 *  match nothing together is answered by its rarest words one at a time (`searchRarestWords`,
 *  packages/envoy/internal/dispatch/api/search.go), so a word another row's issue also holds
 *  would answer this query with that issue. None of them is the key of the project this row
 *  makes (`rowKey("CORE")`): the issue made in it holds that word in its own key (`CORE…-1`),
 *  and the fallback takes first the words the term statistics have counted, so once the
 *  statistics know the project it answers this query with that issue. */
function words(): string {
  return `the ${rowKey("SEXTANT")} ${rowKey("ROTA")} of ${rowKey("COMPASS")}`;
}

/** The label and the session this Dispatch files such words under. A deployment sets them in
 *  its environment (DISPATCH_NEW_ISSUE_LABEL, DISPATCH_NEW_ISSUE_SESSION) and the server starts
 *  with both unset; the test hook is the only way to change them while the server runs. A
 *  Dispatch that serves no such hook holds neither setting and offers nothing, which is the
 *  state this row's negative control reads, so a 404 here is left to the assertions below. */
async function configure(settings: { label: string; session: string }): Promise<void> {
  try {
    await setNewIssueSettings(settings);
  } catch (error) {
    if (!(error instanceof Error) || !error.message.includes("failed: 404")) {
      throw error;
    }
  }
}

const label = "from-the-bar";

function bar(page: Page): Locator {
  return page.getByRole("dialog", { name: "Search" });
}

/** The row the bar offers the typed words as a new issue on. */
function offer(page: Page): Locator {
  return bar(page).getByRole("option", { name: `Make a new issue from “${words()}”` });
}

/** Opens the bar over the project's issue list and types the words. */
async function typeWords(page: Page): Promise<void> {
  await page.goto(`/projects/${rowKey("CORE")}/issues`);
  await expect(page.getByRole("link", { name: /Hull plating/ })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("/");
  await expect(bar(page)).toBeVisible();
  await page.getByRole("combobox", { name: "Search" }).fill(words());
  await expect(bar(page).getByText(`No results for "${words()}"`)).toBeVisible();
}

/** The key of the issue the page is on. */
function keyOnPage(page: Page): string {
  const key = new URL(page.url()).pathname.split("/")[2];
  if (key === undefined) {
    throw new Error(`the page is on ${page.url()}, which names no issue`);
  }
  return key;
}

test.beforeEach(async () => {
  const core = rowKey("CORE");
  await resetFakeEnvoy();
  await createProject({ key: core, name: "Core" });
  await createIssue({ project: core, title: "Hull plating survey" });
});

// SLOW(>=2s): the bar makes an issue, writes its criterion, routes it and sends its first
// message, and the session's reply is read back on the issue page.
test("the bar offers unmatched words as a new issue, and accepting makes it and starts the conversation @phone", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The session the settings file the words under: the row's own (`rowSessionId`,
  // e2e/test.ts), so what the bar sends it lands in no other row's session.
  const session = rowSessionId("planner");
  await setLiveSessions([{ ...plannerSession, session_id: session }]);
  await configure({ label, session });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await typeWords(page);

    // Offering is not making.
    await expect(offer(page)).toBeVisible();
    expect(await listIssues(core)).toHaveLength(1);

    await offer(page).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${core}-\\d+$`));
    const key = keyOnPage(page);
    await expect(page.getByRole("heading", { name: words() })).toBeVisible();

    // The words are the issue's first verification criterion, word for word, and Dispatch wrote
    // nothing else into it: no spec text, and no document beside the two.
    const results = await getIssueResults(key);
    expect((results.checks ?? []).map((check) => check.text)).toEqual([words()]);
    expect((await getIssueArtifactText(key, "spec")).markdown.trim()).toBe("");
    const issue = await getIssue(key);
    expect((issue.artifacts ?? []).map((artifact) => artifact.name).sort()).toEqual([
      "spec.md",
      "verification.md",
    ]);
    expect(issue.labels).toEqual([label]);
    expect(issue.route).toBe(`session:${session}`);

    // It was handed to the session the settings name, and that session's reply is on the issue.
    // The send leaves Dispatch after the message's own answer, so this waits for it.
    await expect
      .poll(async () => (await getSentMessages()).map((message) => message.target_session))
      .toEqual([session]);
    const messages = await listIssueMessages(key);
    expect(messages.map((message) => message.message.body)).toEqual([words()]);
    const first = messages[0];
    if (first === undefined) {
      throw new Error("the issue holds no message to answer");
    }
    await replyToMessageDelivery(
      first.message.id,
      { attempt: 1, body: "Taking the rota now." },
      { id: session, kind: "session" }
    );

    await page.goto(`/issues/${key}/conversation`);
    await expect(page.getByText("Taking the rota now.")).toBeVisible();
  } finally {
    await context.close();
  }
});

test("with the session cleared the issue is still made and the bar says no conversation was started @phone", async ({
  browser,
}) => {
  await setLiveSessions([{ ...plannerSession, session_id: rowSessionId("planner") }]);
  await configure({ label, session: "" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await typeWords(page);
    await offer(page).click();

    await expect(bar(page).getByTestId("search-new-issue-said")).toContainText(
      "No conversation was started"
    );
    const made = (await listIssues(rowKey("CORE"))).filter((issue) => issue.title === words());
    expect(made).toHaveLength(1);
    const key = made[0]?.key ?? "";
    expect((await getIssue(key)).labels).toEqual([label]);
    expect(((await getIssueResults(key)).checks ?? []).map((check) => check.text)).toEqual([
      words(),
    ]);

    // Nothing was started on it: no message on the issue, and nothing sent to any session.
    expect(await listIssueMessages(key)).toHaveLength(0);
    expect(await getSentMessages()).toHaveLength(0);
    expect((await getIssue(key)).route).toBeNull();
  } finally {
    await context.close();
  }
});
