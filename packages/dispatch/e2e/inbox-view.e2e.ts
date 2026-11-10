import { expect, test } from "@playwright/test";

import { createIssue, createProject, listMyReplies, patchIssue } from "./api";
import { holdReadMarks, replyRowId, seedBulkReplyThreads, unreadThreadRows } from "./inbox-threads";
import { drawnIds, openField } from "./inbox-view";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's one list at the size the server's own ordering decides it: a hundred and twenty
 * threads, counted and sorted by priority, one of which follows its issue to another band while
 * the page watches. What the **View** menu and the **Filter** bar draw over a fixed list is in
 * `inbox-view.stub.e2e.ts`, and the rows they are read over are in `inbox-view.ts`.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

// ---------------------------------------------------------------------------
// A11: a hundred and twenty threads.
// ---------------------------------------------------------------------------

test("a hundred and twenty threads sort by priority, count up, and follow their issue's priority live", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const urgent = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await patchIssue(urgent.key, { priority: 0 });
  const later = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(later.key, { priority: 2 });
  // A hundred and twenty threads across the two priorities, in one statement apiece. The P0
  // issue's one thread is seeded wholly behind the rest, so its last message is the oldest of
  // all of them and recency alone would put its row last.
  await seedBulkReplyThreads({ count: 119, issueKey: later.key, prefix: "Mirror" });
  await seedBulkReplyThreads({
    count: 1,
    issueKey: urgent.key,
    olderThan: 600,
    prefix: "Ledger",
  });
  expect(await unreadThreadRows()).toBe(120);
  const listed = await listMyReplies({ state: "unread" });
  const oldest = [...(listed.rows ?? [])].sort((left, right) =>
    left.reply.created_at < right.reply.created_at ? -1 : 1
  )[0];
  expect(oldest?.subject).toBe("Ledger root 1");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Every row here is a thread, so the pane marks the row it selects read as it draws it
    // (A7) and that row leaves the unread list the next time it is read. The marks are held,
    // so the list this row is about keeps all 120 (`inbox-thread-pane.e2e.ts` holds them the
    // same way); what the pane marks is `inbox-read-state.e2e.ts`'s subject.
    await holdReadMarks(page);
    // Sorted by priority, over one list: the oldest message of all is still the first row,
    // because it is the P0 thread.
    await page.goto("/?group=none&sort=priority");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.first()).toBeVisible();
    expect((await drawnIds(page))[0]).toBe(replyRowId(oldest?.thread_id ?? ""));

    // The Priority filter's counts add up to every thread.
    const options = await openField(page, "priority");
    await expect(options.getByRole("option", { name: /^P0,/ })).toHaveAttribute(
      "aria-label",
      "P0, 1 rows"
    );
    await expect(options.getByRole("option", { name: /^P2,/ })).toHaveAttribute(
      "aria-label",
      "P2, 119 rows"
    );
    await page.keyboard.press("Escape");

    // Banded by priority, the P0 thread's row follows its issue to P3 with no reload.
    await page.goto("/?sort=priority");
    const row = page.locator(`[data-inbox-row="${replyRowId(oldest?.thread_id ?? "")}"]`);
    await expect(row).toHaveAttribute("data-inbox-group", "P0");
    await expect(page.getByRole("button", { name: "P0 (1)" })).toBeVisible();
    await patchIssue(urgent.key, { priority: 3 });
    await expect(row).toHaveAttribute("data-inbox-group", "P3");
    await expect(page.getByRole("button", { name: "P3 (1)" })).toBeVisible();
  } finally {
    await alice.close();
  }
});
