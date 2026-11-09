/** The Inbox's thread rows (INFRA-450): the Reply and Agent rows that stand in the one list
 *  beside the reader's open asks and approvals, and what the Type filter makes of them.
 *
 *  Each row reads what the page draws over one fixed list, so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). Two rows
 *  of this family stayed in `inbox-threads.e2e.ts`, each with its reason written there.
 *
 *  Two seeds, because the first row counts a whole Inbox of its own.
 *
 *  Everything here is made up: this repository is public, so nothing names a real person, host or
 *  project.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject } from "./api";
import { setPendingCredentialRequests } from "./fake-broker-helpers";
import {
  agentRowId,
  asSession,
  compactInboxLink,
  expectSidebarInboxCount,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  seedMessageThread,
  seedTwelve,
  THREAD_SESSION,
} from "./inbox-threads";
import { stubbed } from "./stub/stub-test";

// ---------------------------------------------------------------------------
// A1: the old Replies link, and the one Inbox count.
// ---------------------------------------------------------------------------

/** Two open asks waiting on the reader, one credential request, and three unread threads: the
 *  Inbox count is the sum of exactly those. */
const repliesList = stubbed(async () => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Ledger rollout", "Mirror retries"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title}: ship tonight?` }, asSession());
  }
  await setPendingCredentialRequests([
    {
      approve_url: "https://secrets.internal.example/approve/record-alice",
      approver: "alice",
      identifiers: ["LEDGER_API_KEY"],
      kind: "agent_secret",
      record_id: "record-alice",
      requested_at: new Date().toISOString(),
      session: { enrollment: null, request: null },
    },
  ]);
  const commentIssue = await createIssue({ project: "CORE", title: "Audit clock" });
  const comment = await seedCommentThread(
    commentIssue.key,
    "Please fix the flaky clock test.",
    "Done: the clock test is green."
  );
  const messageIssue = await createIssue({ project: "CORE", title: "Build status" });
  const message = await seedMessageThread(
    messageIssue.key,
    "Where is the nightly build?",
    "On the mirror, replaying."
  );
  await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] });
  return { comment, counted: 6, message };
});

repliesList(
  "the old Replies address opens the Inbox on its Reply rows, under one Inbox count @pr",
  async ({ page, scene }) => {
    await page.goto("/replies");

    // The address is the Inbox narrowed to its Reply rows, and the filter says so in a token.
    // It is read parameter by parameter rather than whole: the reading pane writes its own
    // selection into the same address the moment it picks a row (`?item=reply:<thread id>`, which
    // A7 requires it to hold), so an assertion anchored at the end of the query string is a race
    // between the redirect and the pane.
    await expect(page).toHaveURL(
      (url) => url.pathname === "/" && url.searchParams.get("type") === "reply"
    );
    await expect(page.locator('[data-inbox-filter-token="type:reply"]')).toHaveAttribute(
      "aria-label",
      "Remove filter Type: Reply"
    );
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(2);
    await expect(page.locator(`[data-inbox-row="${replyRowId(scene.comment)}"]`)).toBeVisible();
    await expect(page.locator(`[data-inbox-row="${replyRowId(scene.message)}"]`)).toBeVisible();

    // The sidebar has no Replies entry at all, and its Inbox entry carries the one count: two
    // asks, one credential request and three unread threads.
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    await expect(navigation.getByRole("link", { name: /Replies/ })).toHaveCount(0);
    await expectSidebarInboxCount(page, scene.counted);

    // Below 1280 px the same number is the compact header's one Inbox link - on a page that is
    // not the Inbox, where the header draws the count as a link to it rather than as plain text.
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect(compactInboxLink(page)).toHaveCount(1);
    await expect(compactInboxLink(page)).toHaveText(`Inbox ${scene.counted}`);
  }
);

// ---------------------------------------------------------------------------
// A2: one list of twelve rows, and the Type filter over it.
// ---------------------------------------------------------------------------

const test = stubbed(seedTwelve);

/** Opens the bar's Filter panel, which is shut until the reader presses **Filter**. */
async function openFilterFields(page: Page): Promise<Locator> {
  const fields = page.locator("[data-inbox-filter-fields]");
  if ((await fields.count()) === 0) {
    await page.locator("[data-inbox-filter]").click();
  }
  await expect(fields).toHaveCount(1);
  return fields;
}

/** Opens one field's popover and hands back its listbox. */
async function openField(page: Page, field: string, label: string): Promise<Locator> {
  const fields = await openFilterFields(page);
  const trigger = fields.getByRole("button", { name: `Filter by ${field}` });
  if ((await trigger.getAttribute("aria-expanded")) !== "true") {
    await trigger.click();
  }
  const options = page.getByRole("listbox", { name: `${label} options` });
  await expect(options).toHaveCount(1);
  return options;
}

test("the Type filter counts and narrows the one list of asks, approvals, replies and agent messages @pr", async ({
  page,
  scene,
}) => {
  await page.goto("/");
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(12);

  // Every thread row is unread: a bold title and a dot of its own.
  const unread = page.locator("[data-inbox-row] [aria-label='Unread']");
  await expect(unread).toHaveCount(7);
  const threadTitle = page
    .locator(`[data-inbox-row="${replyRowId(scene.askThread)}"] a[data-inbox-owner]`)
    .first();
  await expect(threadTitle).toHaveClass(/font-semibold/);

  // The Send the session took as a turn of its live conversation is no row of the Inbox.
  await expect(page.locator(`[data-inbox-row="${agentRowId(scene.accepted)}"]`)).toHaveCount(0);

  // The Type field counts every kind under the other four filters: the two question shapes
  // add to the three asks, and the threads are counted as what they are.
  const options = await openField(page, "type", "Type");
  const counted = async (name: string): Promise<number> => {
    const label = await options
      .getByRole("option", { name: new RegExp(`^${name}, `) })
      .getAttribute("aria-label");
    return Number((label ?? "").replace(`${name}, `, "").replace(" rows", ""));
  };
  expect((await counted("Question")) + (await counted("Decision"))).toBe(3);
  expect(await counted("Approval")).toBe(2);
  expect(await counted("Reply")).toBe(4);
  expect(await counted("Agent message")).toBe(3);

  // The search box narrows the values the popover lists.
  const search = page.getByRole("combobox", { name: "Search type" });
  await search.fill("rep");
  await expect(options.getByRole("option")).toHaveCount(1);
  await expect(options.getByRole("option").first()).toHaveAttribute("aria-label", "Reply, 4 rows");
  await search.fill("");

  // Each value alone lists only its own rows.
  for (const [value, name, count] of [
    ["approval", "Approval", 2],
    ["reply", "Reply", 4],
    ["agent", "Agent message", 3],
  ] as const) {
    await options.getByRole("option", { name: new RegExp(`^${name}, `) }).click();
    await expect(rows).toHaveCount(count);
    await expect(page.locator(`[data-inbox-row] [data-inbox-kind="${value}"]`)).toHaveCount(count);
    await options.getByRole("option", { name: new RegExp(`^${name}, `) }).click();
    await expect(rows).toHaveCount(scene.listed);
  }

  // Two values together are the union, and a reload keeps what the reader chose.
  await options.getByRole("option", { name: /^Approval, / }).click();
  await options.getByRole("option", { name: /^Reply, / }).click();
  await expect(rows).toHaveCount(6);
  await expect(page).toHaveURL(/type=approval%2Creply/);
  await page.reload();
  await expect(rows).toHaveCount(6);
  await expect(page.locator('[data-inbox-filter-token="type:approval"]')).toHaveCount(1);
  await expect(page.locator('[data-inbox-filter-token="type:reply"]')).toHaveCount(1);
});
