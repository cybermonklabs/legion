import {
  createArtifactAsk,
  createAsk,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  patchIssue,
} from "./api";
import { needsYouCards } from "./editor";
import { inboxRow } from "./inbox-pane";
import { expectInboxCount } from "./inbox-threads";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * dispatch://INFRA-3090 from the browser: every project names a default assignee, and the person
 * a question reaches is three steps resolved as the question is read - the person it was handed
 * to, the assignee of its issue, then the default assignee of its project
 * (`askedOfPersonExpression`, `packages/envoy/internal/dispatch/api/ask_rows.go`). So no question
 * reaches nobody.
 *
 * This row drives the last two steps between two people's open Inboxes, with no reload at all: a
 * question on an issue nobody holds sits in the list of the person its project names, moves when
 * that default moves, and moves back when somebody is written on the issue itself. Each move
 * reaches the open pages over the live socket alone. Each reader holds one Inbox read that names
 * no assignee, so both lists hold the question, with the person it waits on, all along; what
 * moves is that person. The default write reads again every list holding a question it decides
 * (`inboxRowsDecidedByDefault`, `web/src/api/event-refresh.ts`), and the two people it names
 * (`thread_logins`) their whole Inbox; the assignee write reads again every list holding a row
 * of the issue (`inboxRowsOf`). `e2e/refetch-budget.e2e.ts` holds a third reader to what each
 * write costs her.
 *
 * The second row holds the issue page and the document page to the same move: each draws the
 * question's Asked-of badge from its own ask list, which the default write reads again when it
 * holds a question nobody handed on (`issueAskListsDecidedByDefault` and
 * `DOCUMENT_ASK_LISTS_DECIDED_BY_DEFAULT`, beside `inboxRowsDecidedByDefault`).
 */

/** The agent that asks the question: an autonomous session, which holds no issue and names
 *  nobody, so only the chain above can decide who its question reaches. */
const asker = {
  actor: { id: "e2e-default-assignee-asker", kind: "session" as const },
  as: "agent" as const,
};

/** Names `login` CORE's default from the Settings page, as a person does: the people the control
 *  offers are read only once the reader reaches for it, so it is focused first and the option
 *  waited for. `from` is the person it names before. */
async function nameDefaultFromSettings(settings: Page, from: string, login: string): Promise<void> {
  await settings.goto("/settings");
  const control = settings.getByRole("combobox", {
    exact: true,
    name: "Default assignee of CORE",
  });
  await expect(control).toBeEnabled();
  await expect(control).toHaveValue(from);
  await control.focus();
  await expect(control.locator(`option[value="${login}"]`)).toHaveCount(1);
  await control.selectOption(login);
}

/** The issue page's own card for an unanchored question, which the page draws on its
 *  Conversation turn from the issue's ask list (`["asks", key, "thread"]`). */
async function conversationCard(page: Page, issueKey: string, askId: string): Promise<Locator> {
  await page.goto(`/issues/${issueKey}`);
  await page.getByRole("tab", { name: "Conversation" }).click();
  return page.locator("#issue-conversation-panel").getByTestId(`ask-${askId}`);
}

// SLOW(>=2s): two browser contexts and three pages, then two live moves, each of which both open
// Inboxes and their counts must follow with no reload and none of which the page may send a read
// for before the write's event reaches it.
test("a question on an issue nobody holds follows its project's default assignee between two open Inboxes", async ({
  browser,
}, testInfo) => {
  // CORE names alice: a project created by a person names that person when the call names
  // nobody, which is the step the server took here.
  const project = await createProject({ key: "CORE", name: "Core" });
  expect(project.default_assignee).toBe("alice");
  // The issue holds nobody, so its question reaches the project's default and no earlier step of
  // the chain can answer for it. A create always lands on a person, so it is created and then
  // cleared.
  const created = await createIssue({ project: "CORE", title: "Nobody holds this" }, asker);
  const issue = await patchIssue(created.key, { assignee: null }, asker);
  expect(issue.assignee).toBeNull();
  const ask = await createAsk(issue.key, { question: "Which rollout flag?" }, asker);
  expect((await getAsk(ask.id)).ask.waiting_on_person).toBe("alice");

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await alicePage.goto("/");
    await bobPage.goto("/");

    // 1. The question is in the list of the person CORE names, and in no list of the other's.
    await expect(inboxRow(alicePage, ask.id)).toBeVisible();
    await expect(alicePage.getByRole("button", { name: /^Blocked on you: 1 item/ })).toBeVisible();
    await expectInboxCount(alicePage, 1);
    await expect(inboxRow(bobPage, ask.id)).toHaveCount(0);
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(0);
    await expectInboxCount(bobPage, 0);

    // 2. Bob names himself CORE's default from the Settings page, in a page of his own. The
    // question moves with no reload of either Inbox: out of hers, into his, counts and all.
    await nameDefaultFromSettings(await bob.newPage(), "alice", "bob");

    await expect(inboxRow(bobPage, ask.id)).toBeVisible();
    await expect(bobPage.getByRole("button", { name: /^Blocked on you: 1 item/ })).toBeVisible();
    await expectInboxCount(bobPage, 1);
    await expect(inboxRow(alicePage, ask.id)).toHaveCount(0);
    await expect(alicePage.getByText("Nothing needs you")).toBeVisible();
    await expectInboxCount(alicePage, 0);
    const shot = testInfo.outputPath(`project-default-assignee-moved-${testInfo.project.name}.png`);
    await bobPage.screenshot({ fullPage: true, path: shot });
    await testInfo.attach(`the question in the new default's Inbox (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    // 3. The asker then writes alice on the issue itself, which is the step above the project's
    // default: the question moves back, again with no reload, and neither browser made the
    // write.
    await patchIssue(issue.key, { assignee: "alice" }, asker);
    await expect(inboxRow(alicePage, ask.id)).toBeVisible();
    await expect(alicePage.getByRole("button", { name: /^Blocked on you: 1 item/ })).toBeVisible();
    await expectInboxCount(alicePage, 1);
    await expect(inboxRow(bobPage, ask.id)).toHaveCount(0);
    await expect(bobPage.getByText("Nothing needs you")).toBeVisible();
    await expectInboxCount(bobPage, 0);
    expect((await getAsk(ask.id)).ask.waiting_on_person).toBe("alice");
  } finally {
    await alice.close();
    await bob.close();
  }
});

// SLOW(>=2s): two browser contexts and three pages, one of them the live document, then one live
// move both open pages must follow with no reload.
test("issue and document pages follow a default move: a question's Asked-of badge names the new default on the issue page and on the document page", async ({
  browser,
}, testInfo) => {
  // CORE names alice, as the row above has it.
  const project = await createProject({ key: "CORE", name: "Core" });
  expect(project.default_assignee).toBe("alice");
  // A question on an issue nobody holds, and one on a project document: no step of the chain
  // above the project's default answers for either.
  const created = await createIssue({ project: "CORE", title: "Nobody holds this" }, asker);
  const issue = await patchIssue(created.key, { assignee: null }, asker);
  expect(issue.assignee).toBeNull();
  const issueAsk = await createAsk(issue.key, { question: "Which rollout flag?" }, asker);
  const runbook = await createProjectDocument(
    "CORE",
    { content: "# Runbook\n\nRestart, reseed, reopen.\n", name: "Runbook" },
    asker
  );
  const docAsk = await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    asker
  );
  expect((await getAsk(issueAsk.id)).ask.waiting_on_person).toBe("alice");
  expect((await getAsk(docAsk.id)).ask.waiting_on_person).toBe("alice");

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // 1. Alice reads the issue's page and the document's page, two tabs of one context, and
    // each names her as the person its question is asked of.
    const issuePage = await alice.newPage();
    const issueCard = await conversationCard(issuePage, issue.key, issueAsk.id);
    const docPage = await alice.newPage();
    await docPage.goto(`/projects/CORE/documents/${runbook.artifact.slug}`);
    const docCard = needsYouCards(docPage).getByTestId(`ask-${docAsk.id}`);
    await expect(issueCard).toContainText("Asked of alice");
    await expect(docCard).toContainText("Asked of alice");

    // 2. Bob names himself CORE's default from the Settings page, in a page of his own.
    await nameDefaultFromSettings(await bob.newPage(), "alice", "bob");

    // 3. Without a reload, each page names bob: the issue page first, then the document page.
    await expect(issueCard).toContainText("Asked of bob");
    await expect(docCard).toContainText("Asked of bob");
    for (const [name, page] of [
      ["issue", issuePage],
      ["document", docPage],
    ] as const) {
      const shot = testInfo.outputPath(
        `project-default-assignee-${name}-page-${testInfo.project.name}.png`
      );
      await page.screenshot({ fullPage: true, path: shot });
      await testInfo.attach(`the ${name} page after the move (${testInfo.project.name})`, {
        contentType: "image/png",
        path: shot,
      });
    }
    expect((await getAsk(issueAsk.id)).ask.waiting_on_person).toBe("bob");
    expect((await getAsk(docAsk.id)).ask.waiting_on_person).toBe("bob");
  } finally {
    await alice.close();
    await bob.close();
  }
});
