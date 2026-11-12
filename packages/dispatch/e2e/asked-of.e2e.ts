import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { agentRow, setLiveSessions } from "./agents";
import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  getIssue,
  patchIssue,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import { documentEditor, indexedBlockAsk, needsYouCards } from "./editor";
import { inboxList, inboxPane, inboxRow, READING_PANE_WIDTH, selectRow } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * dispatch://INFRA-1576 from the browser: a question is for a person, and a person may hand it
 * to another person.
 *
 * The first row drives the hand-on control the ask card carries on every surface that draws an
 * open ask, with both people's Inboxes open at once, and holds the server's one expression
 * (`coalesce(a.asked_of, i.assignee)`, which it answers as `waiting_on_person`) to what every
 * reader of that ask shows: which list the row is in, the counts beside it, each person's
 * Replies, and the issue page.
 *
 * The second row drives `dispatch issue-update --assignee` as an agent runs it, against the
 * issue page a person is reading, so the command's write and the page agree with no reload.
 */

/** The session that asks these questions: an agent of the team, live, so the Agents page has a
 *  card to count its open asks on. */
const asker = {
  actor: {
    id: "e2e-asked-of-asker",
    kind: "session" as const,
    origin: { session_title: "Planner" },
  },
  as: "agent" as const,
};

/** The ask card's hand-on control, wherever that card is drawn. It is a native `<select>`, which
 *  is also what the Inbox's own filters are built from, so every query for it is scoped to the
 *  card's own ask rather than to a bare role. */
function handOnControl(scope: Locator | Page, askId: string): Locator {
  return scope.locator(`select[data-ask-hand-on="${askId}"]`);
}

/**
 * Hands the ask the control belongs to over to `login`, as a reader does: the people it offers
 * are read only once the reader reaches for it, so the control is focused first and the option
 * waited for. Naming the assignee of the ask's own issue is how a reader hands it back.
 */
async function handTo(control: Locator, login: string): Promise<void> {
  await control.focus();
  await expect(control.locator(`option[value="${login}"]`)).toHaveCount(1);
  await control.selectOption(login);
}

/** Every band heading the Inbox list is drawing, with its count. */
function bandHeadings(page: Page): Locator {
  return inboxList(page).locator("h2 button");
}

/**
 * Takes the reader's hand off the Inbox list. A row the reader's focus or pointer is on is kept
 * where they last saw it until they move on (`interacted` in
 * `web/src/features/shell/ViewportAnchor.tsx`), so a list the reader's hand is still in keeps
 * drawing a row the server has moved; the counts beside it, which read the answer itself, move
 * straight away. Every row below reads a list after the hand that pressed something in it has
 * left, which is what a reader does next.
 */
async function handsOff(page: Page): Promise<void> {
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
  await page.mouse.move(2, 2);
}

/** The issue page's own ask card for an unanchored question, which the page draws on its
 *  Conversation turn; the page is opened on the issue here, so every reader of it starts the
 *  same way. */
async function conversationCard(page: Page, issueKey: string, askId: string): Promise<Locator> {
  await page.goto(`/issues/${issueKey}`);
  await page.getByRole("tab", { name: "Conversation" }).click();
  return page.locator("#issue-conversation-panel").getByTestId(`ask-${askId}`);
}

// SLOW(>=2s): one hand-on read on nine surfaces at once - two Inboxes, two Agents pages, two
// issue pages, two Replies lists and a document - each of which must follow the move with no
// reload, so the row pays for every one of those pages and the four hand-ons it drives.
test("a person hands an ask to another person and it moves between the two Inboxes", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  await setLiveSessions([{ session_id: asker.actor.id, title: "Planner" }]);
  // CORE names carol, a third person: every project names a default assignee, and making it
  // somebody other than these two readers keeps the two questions below out of both counts.
  await createProject({ key: "CORE", name: "Core" }, "carol");

  // A human's issue is assigned to its creator, so these two are alice's and bob's own. Two
  // questions sit on alice's and one on bob's, so every count below moves by one rather than
  // appearing and disappearing.
  const hers = await createIssue({ project: "CORE", title: "Alice's rollout" }, { login: "alice" });
  expect(hers.assignee).toBe("alice");
  const moved = await createAsk(hers.key, { question: "Which rollout flag?" }, asker);
  await createAsk(hers.key, { question: "Which region goes first?" }, asker);
  const his = await createIssue({ project: "CORE", title: "Bob's migration" }, { login: "bob" });
  expect(his.assignee).toBe("bob");
  await createAsk(his.key, { question: "Drop the legacy column?" }, asker);

  // Two more open asks neither reader waits on - a project document's, and an `:::ask` block on
  // an issue the shared token created, which nobody holds, so both reach CORE's default - so the
  // card's control is read on a document's margin and in a document's ask block without either
  // moving a count above.
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
  const blocked = await createIssue(
    {
      project: "CORE",
      spec: ':::ask{#rollout-decision urgency="med" multiple="false"}\nShip on Friday?\n\n- Ship\n- Hold\n:::\n',
      title: "Blocked decision",
    },
    asker
  );
  expect(blocked.assignee).toBe("carol");
  const blockAsk = await indexedBlockAsk(blocked.key, "rollout-decision");

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Alice reads below the reading column's width, where every row draws its own card: that is
    // where she presses the control. Bob reads above it, where the card is the reading column's,
    // which is the second surface this row drives it on.
    const alicePage = await alice.newPage();
    await alicePage.setViewportSize({ height: 900, width: 1100 });
    const bobPage = await bob.newPage();
    await bobPage.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
    const aliceIssue = await alice.newPage();
    const aliceAgents = await alice.newPage();
    const bobAgents = await bob.newPage();

    // The turn bands are the groups of questions that need a person, so that is the grouping
    // these two lists are read under.
    await alicePage.goto("/?group=waiting-on");
    await bobPage.goto("/?group=waiting-on");
    const aliceCard = await conversationCard(aliceIssue, hers.key, moved.id);
    await aliceAgents.goto("/agents");
    await bobAgents.goto("/agents");

    // 1. The question is in alice's group of questions that need her, and in no group of bob's.
    await expect(inboxRow(alicePage, moved.id)).toBeVisible();
    await expect(alicePage.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(bandHeadings(alicePage)).toHaveText(["Waiting on you (2)"]);
    await expect(alicePage.getByText(/^Blocked on you: 2 items/)).toBeVisible();
    await expect(
      agentRow(aliceAgents, asker.actor.id).getByRole("link", { name: "Needs you 2" })
    ).toBeVisible();

    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(1);
    await expect(bandHeadings(bobPage)).toHaveText(["Waiting on you (1)"]);
    await expect(bobPage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
    await expect(
      agentRow(bobAgents, asker.actor.id).getByRole("link", { name: "Needs you 1" })
    ).toBeVisible();

    // 2. Alice hands it to bob with the control on her Inbox row, which names her as the person
    // it waits on until she does.
    const rowControl = handOnControl(inboxRow(alicePage, moved.id), moved.id);
    await expect(rowControl).toHaveCount(1);
    await expect(rowControl).toHaveValue("alice");
    await expect(inboxRow(alicePage, moved.id)).toContainText("Asked of alice");
    await handTo(rowControl, "bob");
    await handsOff(alicePage);

    // 3. Without a reload, the row leaves her list and enters his, and every count beside it
    // follows: the band's own, the banner's, and the per-person number on the Agents page.
    await expect(inboxRow(alicePage, moved.id)).toHaveCount(0);
    await expect(alicePage.locator("[data-inbox-row]")).toHaveCount(1);
    await expect(bandHeadings(alicePage)).toHaveText(["Waiting on you (1)"]);
    await expect(alicePage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
    await expect(
      agentRow(aliceAgents, asker.actor.id).getByRole("link", { name: "Needs you 1" })
    ).toBeVisible();

    await expect(inboxRow(bobPage, moved.id)).toBeVisible();
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(bandHeadings(bobPage)).toHaveText(["Waiting on you (2)"]);
    await expect(bobPage.getByText(/^Blocked on you: 2 items/)).toBeVisible();
    await expect(
      agentRow(bobAgents, asker.actor.id).getByRole("link", { name: "Needs you 2" })
    ).toBeVisible();

    for (const [who, page] of [
      ["alice", alicePage],
      ["bob", bobPage],
    ] as const) {
      const shot = testInfo.outputPath(`asked-of-inbox-${who}-${testInfo.project.name}.png`);
      await page.screenshot({ path: shot, fullPage: true });
      await testInfo.attach(`${who}'s Inbox after the hand-on (${testInfo.project.name})`, {
        contentType: "image/png",
        path: shot,
      });
    }

    // 4. The issue page still lists the question for both people, and the card names who handed
    // it on and when - the moment the server stored, not one this spec invents.
    const handedOn = (await getAsk(moved.id, { login: "alice" })).ask;
    expect(handedOn.asked_of).toBe("bob");
    expect(handedOn.asked_of_by).toBe("alice");
    expect(handedOn.waiting_on_person).toBe("bob");
    await expect(aliceCard).toBeVisible();
    await expect(aliceCard).toContainText("Asked of bob");
    await expect(aliceCard).toContainText("handed on by alice");
    await expect(aliceCard.locator(`time[datetime="${handedOn.asked_of_at}"]`)).toHaveCount(1);

    const bobIssue = await bob.newPage();
    const bobIssueCard = await conversationCard(bobIssue, hers.key, moved.id);
    await expect(bobIssueCard).toBeVisible();
    await expect(bobIssueCard).toContainText("Asked of bob");
    await expect(handOnControl(bobIssueCard, moved.id)).toHaveCount(1);
    await bobIssue.close();

    // 5. The control is on the card wherever the card renders an open ask: on the Inbox row
    // (pressed above), on the reading column's card, on the issue page, on a document's margin
    // and in a document's ask block.
    await selectRow(bobPage, moved.id);
    const paneControl = handOnControl(inboxPane(bobPage), moved.id);
    await expect(paneControl).toHaveCount(1);
    await expect(paneControl).toHaveValue("bob");
    await expect(handOnControl(aliceCard, moved.id)).toHaveCount(1);

    const docPage = await alice.newPage();
    await docPage.goto(`/projects/CORE/documents/${runbook.artifact.slug}`);
    const marginCard = needsYouCards(docPage).getByTestId(`ask-${docAsk.id}`);
    await expect(marginCard).toBeVisible();
    await expect(handOnControl(marginCard, docAsk.id)).toHaveCount(1);
    await docPage.goto(`/issues/${blocked.key}`);
    const block = documentEditor(docPage).locator('[data-dispatch-ask-block="rollout-decision"]');
    const blockCard = block.locator(`article[data-testid="ask-${blockAsk.id}"]`);
    await expect(blockCard).toBeVisible();
    await expect(handOnControl(blockCard, blockAsk.id)).toHaveCount(1);
    await docPage.close();

    // 6. Each person's Replies list follows the same expression: the thread of a question the
    // Inbox already lists for a reader is not a Reply row of theirs, and the same thread is one
    // for the other reader. Both people write in this thread, and the agent writes last, so
    // each of them has a thread somebody else has written in since.
    await createComment(hers.key, { ask_id: moved.id, body: "Which flag do you mean?" });
    await createComment(hers.key, { ask_id: moved.id, body: "The rollout one." }, asker);
    await createComment(
      hers.key,
      { ask_id: moved.id, body: "I will take this one." },
      { login: "bob" }
    );
    await createComment(hers.key, { ask_id: moved.id, body: "Over to you, then." }, asker);

    const aliceReplies = await alice.newPage();
    const bobReplies = await bob.newPage();
    await aliceReplies.goto("/?type=reply");
    await bobReplies.goto("/?type=reply");
    await expect(inboxRow(aliceReplies, replyRowId(moved.id))).toBeVisible();
    await expect(inboxRow(bobReplies, replyRowId(moved.id))).toHaveCount(0);

    // 7. The same control on the reading column hands it back to the issue's assignee, which
    // empties the stored field, and the row returns to her list. Each person's Replies list
    // follows the same move, with no reload either: the hand-on event names the person it moved
    // from and the one it moved to in its `thread_logins`, which is what tells each of those two
    // readers their thread rows have moved (`movesInboxThreads`, `web/src/api/event-refresh.ts`).
    await handTo(paneControl, "alice");
    await handsOff(bobPage);
    await expect
      .poll(async () => (await getAsk(moved.id, { login: "alice" })).ask.asked_of ?? null)
      .toBeNull();
    expect((await getAsk(moved.id, { login: "alice" })).ask.waiting_on_person).toBe("alice");
    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);
    await expect(inboxRow(alicePage, moved.id)).toBeVisible();
    await expect(inboxRow(bobReplies, replyRowId(moved.id))).toBeVisible();
    await expect(inboxRow(aliceReplies, replyRowId(moved.id))).toHaveCount(0);
    await aliceReplies.close();
    await bobReplies.close();

    // 8. The control on the issue page hands it on and hands it back the same way.
    const issueControl = handOnControl(aliceCard, moved.id);
    await handTo(issueControl, "bob");
    await expect(inboxRow(bobPage, moved.id)).toBeVisible();
    await expect(inboxRow(alicePage, moved.id)).toHaveCount(0);
    await handTo(issueControl, "alice");
    await expect
      .poll(async () => (await getAsk(moved.id, { login: "alice" })).ask.asked_of ?? null)
      .toBeNull();
    await expect(inboxRow(alicePage, moved.id)).toBeVisible();
    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);

    // 9. Bob answers it, which a person who is not the issue's assignee may do, and the control
    // is gone from the card the moment the question is no longer open.
    await handTo(issueControl, "bob");
    await expect(inboxRow(bobPage, moved.id)).toBeVisible();
    await selectRow(bobPage, moved.id);
    const bobCard = inboxPane(bobPage).getByTestId(`ask-${moved.id}`);
    await bobCard.getByLabel("Your answer").fill("Read DISPATCH_ROLLOUT_FLAG.");
    await bobCard.getByRole("button", { name: "Answer", exact: true }).click();
    await handsOff(bobPage);
    await expect
      .poll(async () => (await getAsk(moved.id, { login: "bob" })).ask.state)
      .toBe("answered");
    await expect(aliceCard).toContainText("Answered by bob");
    await expect(handOnControl(aliceCard, moved.id)).toHaveCount(0);
    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);
    await expect(inboxRow(alicePage, moved.id)).toHaveCount(0);
  } finally {
    await alice.close();
    await bob.close();
  }
});

// SLOW(>=2s): four cold `bun` starts of the real `dispatch` command, each its own process,
// against a page that must follow every one of them.
test("an agent sets and clears the issue assignee from the command", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(240_000);
  // CORE names carol, so neither reader below holds anything of it to start with.
  await createProject({ key: "CORE", name: "Core" }, "carol");
  // The shared token holds nobody and names nobody, so this issue starts on CORE's default and
  // no Inbox of the two readers lists its question: the command is the only thing that can put
  // either on one of them.
  const issue = await createIssue({ project: "CORE", title: "Rotate the agent token" }, asker);
  expect(issue.assignee).toBe("carol");
  const ask = await createAsk(issue.key, { question: "Rotate before the deploy?" }, asker);
  const closed = await createIssue({ project: "CORE", title: "Already finished" }, asker);
  await patchIssue(closed.key, { status: "done" }, asker);

  const home = mkdtempSync(join(tmpdir(), "dispatch-asked-of-cli-"));
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const assignee = page.getByLabel(`Assignee of ${issue.key}`);
    await expect(assignee).toHaveValue("carol");

    const bobPage = await bob.newPage();
    await bobPage.goto("/?group=waiting-on");
    await expect(bobPage.getByText("Nothing needs you")).toBeVisible();

    // 1. The command names a person who has signed in: the page shows the new assignee with no
    // reload, and the question nobody handed on moves to that person's Inbox with it.
    const assigned = await runDispatch(
      ["issue-update", "--issue", issue.key, "--assignee", "bob"],
      home
    );
    expect(assigned.code, `${assigned.stdout}${assigned.stderr}`).toBe(0);
    await expect(assignee).toHaveValue("bob");
    await expect(inboxRow(bobPage, ask.id)).toBeVisible();
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(1);

    // 2. `--clear-assignee` leaves the issue with nobody, which the page shows and the Inbox
    // follows.
    const cleared = await runDispatch(
      ["issue-update", "--issue", issue.key, "--clear-assignee"],
      home
    );
    expect(cleared.code, `${cleared.stdout}${cleared.stderr}`).toBe(0);
    await expect(assignee).toHaveValue("");
    await expect(
      page.getByTestId("issue-header").locator("span", { hasText: /^Unassigned$/ })
    ).toBeVisible();
    await expect(inboxRow(bobPage, ask.id)).toHaveCount(0);

    // 3. An address nobody has signed in with is refused, and the page does not change.
    const stranger = await runDispatch(
      ["issue-update", "--issue", issue.key, "--assignee", "mallory@example.com"],
      home
    );
    expect(stranger.code).not.toBe(0);
    expect(`${stranger.stdout}${stranger.stderr}`).toContain("ASSIGNEE_NOT_ALLOWED");
    expect((await getIssue(issue.key)).assignee).toBeNull();
    await expect(assignee).toHaveValue("");

    // 4. The same call on a closed issue is refused too: a closed issue takes no assignee until
    // somebody reopens it.
    const onClosed = await runDispatch(
      ["issue-update", "--issue", closed.key, "--assignee", "bob"],
      home
    );
    expect(onClosed.code).not.toBe(0);
    expect(`${onClosed.stdout}${onClosed.stderr}`).toContain("ISSUE_CLOSED");
    expect((await getIssue(closed.key)).assignee).toBe("carol");
  } finally {
    await alice.close();
    await bob.close();
    rmSync(home, { force: true, recursive: true });
  }
});
