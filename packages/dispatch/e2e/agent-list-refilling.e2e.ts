import {
  agentRow,
  type FakeSession,
  pollAgentListEvery,
  setInterests,
  setListsState,
  setLiveSessions,
  spendPageTimeUntil,
} from "./agents";
import {
  baseUrl,
  createAsk,
  createIssue,
  createProject,
  createProjectDocument,
  getIssue,
  patchIssue,
  userHeaders,
} from "./api";
import { frontLiveView } from "./live-views";
import { openRefPicker } from "./ref-picker";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// The Envoy listener refuses its two list reads while its copy of a bucket cannot be served: 503,
// `Retry-After` and code `refilling` (the fake's `refilling` state). Dispatch answers that as
// ENVOY_REFILLING, and every view that reads the agent list or an owner's subscribers says the
// list is refilling where it would otherwise say an agent is absent: no "No agents are connected",
// no "may have ended", no "No reference matches" in the reference picker, no subscriber or
// recipient count, no offline dot and no no_holder route marker. A listener that could not answer
// at all (the fake's `down` state) is unreachable wording on the same views, never the refilling
// notice.
//
// The Agents page and an agent's conversation page poll the list (the shell's 15 s,
// `AGENT_REGISTRY_POLL_MS`; these rows' pages every `ROW_POLL_MS`, `pollAgentListEvery`), so they
// move from a complete read to a refilling one in place, keeping the rows they had. The other
// views read again on a reload or a Retry. A refused read surfaces only once the shared retry
// policy's two retries are out (`web/src/main.tsx`), and a refusal that names a wait is left alone
// for it: Dispatch passes the listener's `Retry-After` on with ENVOY_REFILLING (`agentsAnswer`
// below pins it, the real listener's 5 and the rows' own 1), and each retry waits that long plus a
// random share of it again (`queryRetryDelay`, `web/src/api/query-client.ts`). At the listener's
// 5 s a view's first refilling read says so only once up to 20 s of the page's own time are out;
// the rows set `ROW_RETRY_AFTER_SECONDS`, so each view's wait is up to 4 s. The polls and those
// waits are page timers: a row about a refilling list installs its context's clock before its
// first page loads, and each wait for a view to say it is refilling, or to stop, moves that clock
// until the view does (`untilRefilling` and `untilNotRefilling` below), rather than waiting the
// timers out. The row whose listener cannot answer at all moves the same clock: each of its views
// says so only once its read's two retries are out, and its two polling pages only after their
// next poll, so it moves the clock until each one does (`untilShown`). Running-state marks are
// found by their accessible names (`Running`, `Not running`, `Running state unknown`:
// `AgentName`'s mark).

const REFILLING = "The agent list is refilling. Try again in a moment.";
// The agent list's cadence on these rows' pages.
const ROW_POLL_MS = 1_000;
// The `Retry-After` these rows' refilling list carries.
const ROW_RETRY_AFTER_SECONDS = 1;
// The bound on Dispatch's answer taking up the rows' own `Retry-After`: it serves the listener's
// answer from a copy at most 5 s old (`routeRosterTTL`), and a margin for a loaded runner. An
// upper bound only: the poll passes as soon as the answer does.
const REFILL_TIMEOUT = 20_000;
// The bound on the two polling pages' reads in the row whose listener cannot answer: one poll
// (`ROW_POLL_MS`), the two half-second retries of the read it makes and a margin, measured from
// whenever the state flipped. That row spends this time on the page's clock before it reads them
// (`untilShown`), so what the bound is left to cover is the real round trips of the reads the
// jumps fire.
const POLL_TIMEOUT = 30_000;
const PLANNER_ROLE = "refill-planner";
const DOCUMENT_SLUG = "refill-notes";

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/srv/refill-planner",
  machine_id: "box-refill",
  roles: [PLANNER_ROLE],
  session_id: "refill-planner-session",
  title: "Planner (refill)",
};
/** The picker's row for that session, told from the role row beneath it: a row's accessible
 *  name is its words and the line under them, and a role's line names the session holding it,
 *  so the session's name alone matches both (`e2e/ref-picker.ts`). */
const plannerRow = new RegExp(`^${planner.title.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&")}`);
const watcher: FakeSession = {
  dir: "/srv/refill-watcher",
  machine_id: "box-refill",
  session_id: "refill-watcher-session",
  title: "Watcher (refill)",
};

interface RefillFixture {
  askId: string;
  issueKey: string;
}

/** One issue routed to the planner's role with an open ask the planner wrote (so the planner
 *  follows it), one project document, and the watcher subscribed to both. `sessions` is what the
 *  listener's list names while it answers. */
async function seedRefillFixture(sessions: FakeSession[]): Promise<RefillFixture> {
  await setLiveSessions(sessions);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refilling route" });
  await patchIssue(issue.key, { route: `role:${PLANNER_ROLE}` });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the refill?" },
    { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
  );
  await createProjectDocument("CORE", {
    content: "# Refill notes\n\nWho is listening.\n",
    name: "Refill notes",
  });
  await setInterests([
    {
      session_id: watcher.session_id,
      topics: [
        `notifications.dispatch.issue.${issue.key}.>`,
        `notifications.dispatch.document.CORE.${DOCUMENT_SLUG}.>`,
      ],
    },
  ]);
  return { askId: ask.id, issueKey: issue.key };
}

const livePath = `/agents/${planner.session_id}/live`;
const documentPath = `/projects/CORE/documents/${DOCUMENT_SLUG}`;

function refillingNotice(scope: Page | Locator): Locator {
  return scope.getByTestId("agent-list-refilling");
}

/** Moves the page's clock until `scope` shows the refilling notice: after a read's refusal and its
 *  two retries, or, on a page that polls the list, its next poll and those. */
async function untilRefilling(scope: Page | Locator): Promise<void> {
  await untilShown(refillingNotice(scope), "the refilling notice shows");
}

/** Moves the page's clock until `scope` shows no refilling notice: a polling page's next poll, or
 *  the retry it was waiting on, reads the complete list again. */
async function untilNotRefilling(scope: Page | Locator): Promise<void> {
  const notice = refillingNotice(scope);
  await spendPageTimeUntil(
    notice.page(),
    async () => (await notice.count()) === 0,
    "the refilling notice goes"
  );
}

/** Moves the page's clock until `locator` shows: how the row whose listener cannot answer waits
 *  for a view to say so - after its read's two half-second retries, or, on a page that polls the
 *  list, after its next poll and those - the page timers `untilRefilling` spends as well. */
async function untilShown(locator: Locator, message: string): Promise<void> {
  await spendPageTimeUntil(locator.page(), () => locator.isVisible(), message);
}

/** The planner's live view names its machine and its working directory, each in a part of the
 *  composer's footer (`AgentConversationPage`), scoped to the view in front because two live
 *  views can be mounted at once (`e2e/live-views.ts`). */
async function expectPlannerFacts(page: Page): Promise<void> {
  const footer = frontLiveView(page).getByTestId("agent-composer-footer");
  await expect(footer.getByTestId("agent-footer-machine")).toHaveText("box-refill");
  await expect(footer.getByTestId("agent-footer-dir")).toHaveText("/srv/refill-planner");
}

/** What the server answers `GET /api/v1/agents`: its status, its `Retry-After` and its code. */
async function agentsAnswer(): Promise<{
  code: unknown;
  retryAfter: string | null;
  status: number;
}> {
  const response = await fetch(new URL("/api/v1/agents", baseUrl), {
    headers: await userHeaders(),
  });
  const body = (await response.json()) as { code?: unknown };
  return {
    code: body.code,
    retryAfter: response.headers.get("Retry-After"),
    status: response.status,
  };
}

/** No view on the page claims an agent is absent. Every assertion is a count of zero, so a caller
 *  first waits for what the page does say. */
async function expectNoAbsenceClaim(page: Page): Promise<void> {
  await expect(page.getByText("No agents are connected.")).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Agents empty state" })).toHaveCount(0);
  await expect(page.getByText("No reference matches.")).toHaveCount(0);
  await expect(page.getByText(/may have ended/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Subscribers: \d/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Reaches \d/ })).toHaveCount(0);
  await expect(page.getByRole("img", { exact: true, name: "Not running" })).toHaveCount(0);
  await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
}

/** The page says nothing of an unreachable listener. */
async function expectNoUnreachableWording(page: Page): Promise<void> {
  await expect(page.getByText(/Envoy listener unreachable/)).toHaveCount(0);
  await expect(page.getByText(/Could not (load|refresh) agents/)).toHaveCount(0);
  await expect(page.getByTestId("agent-list-unreachable")).toHaveCount(0);
  await expect(page.getByTestId("issue-route-unknown")).toHaveCount(0);
}

/** The page says nothing of a refilling list. */
async function expectNoRefillingWording(page: Page): Promise<void> {
  await expect(refillingNotice(page)).toHaveCount(0);
  await expect(page.getByText(REFILLING)).toHaveCount(0);
  await expect(page.getByTestId("issue-route-refilling")).toHaveCount(0);
}

async function openIssueConversation(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/conversation`);
  await expect(page.getByRole("form", { name: "Comment composer" })).toBeVisible();
}

/** Opens the issue composer's reference picker afresh. Opening it reads the agent list again
 *  (`useRefPicker`), so a list that changed since the last open is the one shown. */
async function openReferencePicker(page: Page): Promise<Locator> {
  const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
  await field.fill("");
  return openRefPicker(field, "@");
}

function askRecipients(page: Page, askId: string): Locator {
  return page.getByTestId(`ask-${askId}`).getByRole("region", { name: "Recipients" });
}

async function openProjectList(page: Page, issueKey: string): Promise<void> {
  await page.goto("/projects/CORE/issues");
  await expect(
    page
      .getByRole("table", { name: "Project issues table" })
      .locator(`[data-issue-row="${issueKey}"]`)
  ).toHaveCount(1);
}

async function openProjectBoard(page: Page, issueKey: string): Promise<void> {
  await page.getByRole("button", { name: "Board" }).click();
  await expect(page.locator(`[data-board-card="${issueKey}"]`)).toHaveCount(1);
}

/** Every refilling view a fresh page shows, after a reload or a first visit: the issue header's
 *  subscribers and route, the reference picker, the document's subscribers, the ask card's
 *  recipients, and the project list and board. */
async function expectFreshViewsRefilling(page: Page, fixture: RefillFixture): Promise<void> {
  await openIssueConversation(page, fixture.issueKey);
  // The issue's ask card shows its own recipients notice in the Conversation and the margin.
  const header = page.getByTestId("issue-header");
  await untilRefilling(header);
  await expect(refillingNotice(header)).toHaveText(REFILLING);
  await expect(refillingNotice(header)).toHaveAttribute("role", "status");
  await expect(page.getByRole("button", { name: "Subscribers", exact: true })).toBeVisible();
  await expect(page.getByTestId("issue-route-refilling")).toHaveText(
    `Route reach unknown: ${REFILLING}`
  );
  const suggestions = await openReferencePicker(page);
  await untilRefilling(suggestions);
  await expect(refillingNotice(suggestions)).toHaveText(REFILLING);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await page.goto(documentPath);
  await untilRefilling(page);
  await expect(refillingNotice(page)).toHaveText(REFILLING);
  await expect(page.getByRole("region", { name: "Subscribed agents" })).toHaveCount(0);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await page.goto("/");
  const recipients = askRecipients(page, fixture.askId);
  await untilRefilling(recipients);
  await expect(refillingNotice(recipients)).toHaveText(REFILLING);
  const toggle = recipients.getByRole("button", { name: "Recipients", exact: true });
  await toggle.click();
  await expect(
    recipients
      .getByRole("list", { name: "Followers" })
      .getByRole("img", { exact: true, name: "Running state unknown" })
  ).toHaveCount(1);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await openProjectList(page, fixture.issueKey);
  await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
  await openProjectBoard(page, fixture.issueKey);
  await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
}

test("a list that starts refilling after a complete read claims no agent absent, and recovers", async ({
  browser,
}) => {
  // Four pages and a dozen loads of them, each a few seconds' work on a busy machine; the waits
  // on the agent list are page timers the clock spends, so they cost this deadline only the real
  // round trips of the reads they wait on and the short pauses between two readings of the page
  // (`spendPageTimeUntil`, `e2e/agents.ts`).
  test.setTimeout(120_000);
  const fixture = await seedRefillFixture([planner, watcher]);
  const alice = await asUser(browser, "alice");
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    // The clock is the context's, so all four pages below share it, and moving it for one moves
    // the timers of each: the polling pages' polls as much as the fresh page's retries.
    await alice.clock.install();
    // A complete read: every view names the agents and counts what it reaches.
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expectNoRefillingWording(agentsPage);

    const livePage = await alice.newPage();
    await livePage.goto(livePath);
    await expectPlannerFacts(livePage);
    await expectNoRefillingWording(livePage);

    const issuePage = await alice.newPage();
    await openIssueConversation(issuePage, fixture.issueKey);
    await expect(issuePage.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(issuePage.getByTestId("issue-route-unknown")).toHaveCount(0);
    await expect(issuePage.getByTestId("issue-route-unreachable")).toHaveCount(0);
    const before = await openReferencePicker(issuePage);
    await expect(before.getByRole("option", { name: plannerRow })).toBeVisible();
    await expectNoRefillingWording(issuePage);

    const page = await alice.newPage();
    await page.goto(documentPath);
    const subscribed = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribed.getByText(watcher.title, { exact: true })).toBeVisible();
    await expect(subscribed.getByRole("img", { exact: true, name: "Running" })).toHaveCount(1);
    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    await expect(recipients.getByRole("button", { name: "Reaches 2" })).toBeVisible();

    await setListsState("refilling", { retryAfterSeconds: ROW_RETRY_AFTER_SECONDS });

    // The open composer reads the list again as its suggestions open: the rows it had stay, and
    // the refilling notice stands where the agent and role rows are read from.
    const during = await openReferencePicker(issuePage);
    await untilRefilling(during);
    await expect(refillingNotice(during)).toHaveText(REFILLING);
    await expect(during.getByRole("option", { name: plannerRow })).toBeVisible();
    await expect(issuePage.getByText("No reference matches.")).toHaveCount(0);

    await expectFreshViewsRefilling(page, fixture);

    // The two polling pages refreshed in place: rows and details from the complete read stay,
    // under the refilling notice, and nothing reads as gone or unreachable.
    await untilRefilling(agentsPage);
    await expect(refillingNotice(agentsPage)).toHaveText(REFILLING);
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expect(agentsPage.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(agentsPage);
    await expectNoUnreachableWording(agentsPage);
    await untilRefilling(livePage);
    await expect(refillingNotice(livePage)).toHaveText(REFILLING);
    await expectPlannerFacts(livePage);
    await expectNoAbsenceClaim(livePage);
    await expectNoUnreachableWording(livePage);

    // The ask card read while the list refilled is the one a Retry brings back.
    await page.goto("/");
    const refilled = askRecipients(page, fixture.askId);
    await untilRefilling(refilled);
    await expect(refillingNotice(refilled)).toHaveText(REFILLING);

    await setListsState("ok");

    // Recovery: the polls take the complete read back, and a Retry or a reload counts again.
    await untilNotRefilling(agentsPage);
    await expect(refillingNotice(agentsPage)).toHaveCount(0);
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await untilNotRefilling(livePage);
    await expect(refillingNotice(livePage)).toHaveCount(0);
    await expectPlannerFacts(livePage);

    await openIssueConversation(issuePage, fixture.issueKey);
    await expect(issuePage.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(issuePage.getByTestId("issue-route-refilling")).toHaveCount(0);
    await expect(issuePage.getByTestId("issue-route-unknown")).toHaveCount(0);
    await expect(
      (await openReferencePicker(issuePage)).getByRole("option", { name: plannerRow })
    ).toBeVisible();
    await expectNoRefillingWording(issuePage);

    await refilled.getByRole("button", { name: "Retry" }).click();
    await expect(refilled.getByRole("button", { name: "Reaches 2" })).toBeVisible();
    await expectNoRefillingWording(page);

    await page.goto(documentPath);
    await expect(
      page
        .getByRole("region", { name: "Subscribed agents" })
        .getByRole("img", { exact: true, name: "Running" })
    ).toHaveCount(1);
    await expectNoRefillingWording(page);
  } finally {
    await alice.close();
  }
});

test("an empty list that starts refilling never says nobody is connected", async ({ browser }) => {
  // Four pages and ten loads of them, each a few seconds' work on a busy machine; the waits on
  // the agent list are page timers the clock spends, so they cost this deadline only the real
  // round trips of the reads they wait on and the short pauses between two readings of the page
  // (`spendPageTimeUntil`, `e2e/agents.ts`).
  test.setTimeout(120_000);
  const fixture = await seedRefillFixture([]);
  const alice = await asUser(browser, "alice");
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    // The clock is the context's, so all four pages below share it, and moving it for one moves
    // the timers of each: the polling pages' polls as much as the fresh page's retries.
    await alice.clock.install();
    // A complete, empty read is a true absence, and each view says so.
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentsPage.getByRole("region", { name: "Agents empty state" })).toHaveText(
      "No agents are connected."
    );

    const livePage = await alice.newPage();
    await livePage.goto(livePath);
    await expect(
      livePage.getByText("Not in the live session list — it may have ended.")
    ).toBeVisible();

    const issuePage = await alice.newPage();
    await openIssueConversation(issuePage, fixture.issueKey);
    await expect(issuePage.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(issuePage.getByTestId("issue-route-unreachable")).toContainText(
      "Nobody holds it right now"
    );
    const before = await openReferencePicker(issuePage);
    // A complete, empty list: the picker offers no agent and no role of its own, and says
    // nothing about a list it could not read.
    await expect(before.getByRole("option", { name: plannerRow })).toHaveCount(0);
    await expect(refillingNotice(before)).toHaveCount(0);

    const page = await alice.newPage();
    await page.goto(documentPath);
    await expect(
      page
        .getByRole("region", { name: "Subscribed agents" })
        .getByRole("img", { exact: true, name: "Not running" })
    ).toHaveCount(1);
    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    await recipients.getByRole("button", { name: "Reaches 2" }).click();
    await expect(recipients.getByRole("img", { exact: true, name: "Not running" })).toHaveCount(2);
    await openProjectList(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(1);
    await openProjectBoard(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(1);
    // The view choice is remembered; the refilling pass opens the List first, as here.
    await page.getByRole("button", { name: "List" }).click();

    await setListsState("refilling", { retryAfterSeconds: ROW_RETRY_AFTER_SECONDS });

    const during = await openReferencePicker(issuePage);
    await untilRefilling(during);
    await expect(refillingNotice(during)).toHaveText(REFILLING);
    await expect(issuePage.getByText("No reference matches.")).toHaveCount(0);

    await expectFreshViewsRefilling(page, fixture);

    await untilRefilling(agentsPage);
    await expect(refillingNotice(agentsPage)).toHaveText(REFILLING);
    await expect(agentsPage.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(agentsPage);
    await expectNoUnreachableWording(agentsPage);
    await untilRefilling(livePage);
    await expect(refillingNotice(livePage)).toHaveText(REFILLING);
    await expectNoAbsenceClaim(livePage);
    await expectNoUnreachableWording(livePage);
  } finally {
    await alice.close();
  }
});

test("a listener refilling from the first read shows every view refilling", async ({ browser }) => {
  const fixture = await seedRefillFixture([planner, watcher]);
  // What every view below reads: Dispatch's own refilling answer, not an unreachable listener,
  // with the listener's `Retry-After` passed on as it is - the real listener's 5, and then the
  // rows' own 1, which the views below wait out. Dispatch serves the listener's answer from a copy
  // at most 5 s old (`routeRosterTTL`, `readRouteRoster` in internal/dispatch/api/agents.go), so
  // the second figure shows once that copy is read again.
  await setListsState("refilling");
  expect(await agentsAnswer()).toEqual({ code: "ENVOY_REFILLING", retryAfter: "5", status: 503 });
  await setListsState("refilling", { retryAfterSeconds: ROW_RETRY_AFTER_SECONDS });
  await expect.poll(agentsAnswer, { timeout: REFILL_TIMEOUT }).toEqual({
    code: "ENVOY_REFILLING",
    retryAfter: String(ROW_RETRY_AFTER_SECONDS),
    status: 503,
  });
  expect(await getIssue(fixture.issueKey)).toMatchObject({
    route_status: "unknown",
    route_status_reason: "refilling",
  });
  const alice = await asUser(browser, "alice");
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto("/agents");
    await untilRefilling(page);
    await expect(refillingNotice(page)).toHaveText(REFILLING);
    await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(page);
    await expectNoUnreachableWording(page);

    await page.goto(livePath);
    await untilRefilling(page);
    await expect(refillingNotice(page)).toHaveText(REFILLING);
    await expect(page.getByTestId("agent-list-loading")).toHaveCount(0);
    await expectNoAbsenceClaim(page);
    await expectNoUnreachableWording(page);

    await expectFreshViewsRefilling(page, fixture);
  } finally {
    await alice.close();
  }
});

test("a listener that cannot answer reads as unreachable on every view, never as refilling", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const fixture = await seedRefillFixture([planner, watcher]);
  const alice = await asUser(browser, "alice");
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    // The clock is the context's, so all three pages below share it, and moving it for one moves
    // the timers of each: the polling pages' polls as much as the fresh page's retries.
    await alice.clock.install();
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    const livePage = await alice.newPage();
    await livePage.goto(livePath);
    await expectPlannerFacts(livePage);

    await setListsState("down");
    expect(await agentsAnswer()).toEqual({
      code: "ENVOY_UNAVAILABLE",
      retryAfter: null,
      status: 503,
    });
    expect(await getIssue(fixture.issueKey)).toMatchObject({
      route_status: "unknown",
      route_status_reason: "unreachable",
    });

    const page = await alice.newPage();
    await page.goto("/agents");
    const couldNotLoad = page.getByText(/^Could not load agents: /);
    await untilShown(couldNotLoad, "the Agents page says it could not load the agents");
    await expect(couldNotLoad).toBeVisible();
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto(livePath);
    const unreachable = page.getByTestId("agent-list-unreachable");
    await untilShown(unreachable, "the live view says the listener is unreachable");
    await expect(unreachable).toHaveText(
      "Could not check the live session list — Envoy listener unreachable."
    );
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await openIssueConversation(page, fixture.issueKey);
    const issueSubscribers = page
      .getByRole("alert")
      .filter({ hasText: "Subscribed agents unavailable — Envoy listener unreachable." });
    await untilShown(issueSubscribers, "the issue says its subscribed agents are unavailable");
    await expect(issueSubscribers).toBeVisible();
    await expect(page.getByRole("button", { name: "Subscribers", exact: true })).toBeVisible();
    await expect(page.getByTestId("issue-route-unknown")).toHaveText("Route reach unknown");
    const suggestions = await openReferencePicker(page);
    const suggestionsUnreachable = suggestions.getByTestId("agent-list-unreachable");
    await untilShown(
      suggestionsUnreachable,
      "the reference picker says the listener is unreachable"
    );
    await expect(suggestionsUnreachable).toHaveText(
      "Could not load live agents — Envoy listener unreachable."
    );
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto(documentPath);
    const documentSubscribers = page
      .getByRole("alert")
      .filter({ hasText: "Subscribed agents unavailable — Envoy listener unreachable." });
    await untilShown(
      documentSubscribers,
      "the document says its subscribed agents are unavailable"
    );
    await expect(documentSubscribers).toBeVisible();
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    const askSubscribers = recipients
      .getByRole("alert")
      .filter({ hasText: "Subscribers unavailable — Envoy listener unreachable." });
    await untilShown(askSubscribers, "the ask card says its subscribers are unavailable");
    await expect(askSubscribers).toBeVisible();
    await recipients.getByRole("button", { name: "Recipients", exact: true }).click();
    await expect(
      recipients
        .getByRole("list", { name: "Followers" })
        .getByRole("img", { exact: true, name: "Running state unknown" })
    ).toHaveCount(1);
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await openProjectList(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
    await openProjectBoard(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);

    // The polling pages refreshed in place: they keep the rows they had and say the refresh
    // failed, not that the list is refilling. Each says so after its next poll (`ROW_POLL_MS`) and
    // that read's two retries, which the clock is moved through.
    await untilShown(
      agentsPage.getByRole("alert").filter({ hasText: /^Could not refresh agents: / }),
      "the Agents page's poll says it could not refresh"
    );
    await expect(agentsPage.getByRole("alert")).toHaveText(/^Could not refresh agents: /, {
      timeout: POLL_TIMEOUT,
    });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expectNoRefillingWording(agentsPage);
    await expectNoAbsenceClaim(agentsPage);
    const liveUnreachable = livePage.getByTestId("agent-list-unreachable");
    await untilShown(liveUnreachable, "the live view's poll says the listener is unreachable");
    await expect(liveUnreachable).toBeVisible({
      timeout: POLL_TIMEOUT,
    });
    await expectPlannerFacts(livePage);
    await expectNoRefillingWording(livePage);
    await expectNoAbsenceClaim(livePage);
  } finally {
    await alice.close();
  }
});
