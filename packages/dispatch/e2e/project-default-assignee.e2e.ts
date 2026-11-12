import { createAsk, createIssue, createProject, getAsk, patchIssue } from "./api";
import { inboxRow } from "./inbox-pane";
import { expectInboxCount } from "./inbox-threads";
import { expect, test } from "./test";
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
 */

/** The agent that asks the question: an autonomous session, which holds no issue and names
 *  nobody, so only the chain above can decide who its question reaches. */
const asker = {
  actor: { id: "e2e-default-assignee-asker", kind: "session" as const },
  as: "agent" as const,
};

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
    await expect(alicePage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
    await expectInboxCount(alicePage, 1);
    await expect(inboxRow(bobPage, ask.id)).toHaveCount(0);
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(0);
    await expectInboxCount(bobPage, 0);

    // 2. Bob names himself CORE's default from the Settings page, in a page of his own. The
    // question moves with no reload of either Inbox: out of hers, into his, counts and all.
    const settings = await bob.newPage();
    await settings.goto("/settings");
    const control = settings.getByRole("combobox", {
      exact: true,
      name: "Default assignee of CORE",
    });
    await expect(control).toBeEnabled();
    await expect(control).toHaveValue("alice");
    await control.focus();
    await expect(control.locator('option[value="bob"]')).toHaveCount(1);
    await control.selectOption("bob");

    await expect(inboxRow(bobPage, ask.id)).toBeVisible();
    await expect(bobPage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
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
    await expect(alicePage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
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
