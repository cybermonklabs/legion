import { type Refusal, routePath } from "../web/src/api/paths";
import {
  agentRow,
  type FakeSession,
  pollAgentListEvery,
  setInterests,
  setListsState,
  setLiveSessions,
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
import { openIssueDetails } from "./issue-details";
import { frontLiveView } from "./live-views";
import { openRefPicker } from "./ref-picker";
import { expect, type Locator, type Page, rowKey, rowPerson, rowSessionId, test } from "./test";
import { asUser, type SignInLogin } from "./users";

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
// move from a complete read to a refilling one in place, keeping the rows they had; assertions on
// those two pages wait for that poll with `POLL_TIMEOUT`. The other views read again on a reload
// or a Retry. A refused read surfaces only once the shared retry policy's two retries are out
// (`web/src/main.tsx`), and a refusal that names a wait is left alone for it: Dispatch passes the
// listener's `Retry-After` on with ENVOY_REFILLING (`agentsAnswer` below pins it, the real
// listener's 5 and the rows' own 1), and each retry waits that long plus a random share of it
// again (`queryRetryDelay`, `web/src/api/query-client.ts`). At the listener's 5 s a view's first
// refilling read says so up to 20 s after the page made it; the rows set `ROW_RETRY_AFTER_SECONDS`
// so each view's wait is up to 4 s. Every assertion that waits for one gives it `REFILL_TIMEOUT`.
// Running-state marks are found by their accessible names (`Running`, `Not running`, `Running
// state unknown`: `AgentName`'s mark).

const REFILLING = "The agent list is refilling. Try again in a moment.";
// The agent list's cadence on these rows' pages.
const ROW_POLL_MS = 1_000;
// The `Retry-After` these rows' refilling list carries.
const ROW_RETRY_AFTER_SECONDS = 1;
// The two retries of a read Dispatch refused as refilling, each waiting out the answer's own
// `Retry-After` and up to as much again, and a margin for a loaded runner. An upper bound only:
// an assertion passes as soon as the view says so.
const REFILL_TIMEOUT = 20_000;
// A poll, the refilling read it makes (`REFILL_TIMEOUT`) and a margin, measured from whenever the
// state flipped.
const POLL_TIMEOUT = 30_000;
const PLANNER_ROLE = "refill-planner";
const DOCUMENT_SLUG = "refill-notes";

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/srv/refill-planner",
  machine_id: "box-refill",
  roles: [PLANNER_ROLE],
  // The row's own (`rowSessionId`, e2e/test.ts), read when a row names it: Dispatch keeps a
  // session's asks and messages by its id, for every person at once.
  get session_id() {
    return rowSessionId("refillplanner");
  },
  title: "Planner (refill)",
};
/** The picker's row for that session, told from the role row beneath it: a row's accessible
 *  name is its words and the line under them, and a role's line names the session holding it,
 *  so the session's name alone matches both (`e2e/ref-picker.ts`). */
const plannerRow = new RegExp(`^${planner.title.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&")}`);
const watcher: FakeSession = {
  dir: "/srv/refill-watcher",
  machine_id: "box-refill",
  get session_id() {
    return rowSessionId("refillwatcher");
  },
  title: "Watcher (refill)",
};

interface RefillFixture {
  askId: string;
  issueKey: string;
}

/** One issue routed to the planner's role with an open ask the planner wrote (so the planner
 *  follows it), one project document, and the watcher subscribed to both. `sessions` is what the
 *  listener's list names while it answers. `reader` makes the project and the issue, so the ask
 *  waits on her and her Inbox draws its card: the row's own person (`rowPerson`, e2e/test.ts),
 *  whose Inbox holds no other row's question to select in its place. */
async function seedRefillFixture(
  sessions: FakeSession[],
  reader: SignInLogin
): Promise<RefillFixture> {
  const core = rowKey("CORE");
  await setLiveSessions(sessions);
  await createProject({ key: core, name: "Core" }, { login: reader });
  const issue = await createIssue({ project: core, title: "Refilling route" }, { login: reader });
  await patchIssue(issue.key, { route: `role:${PLANNER_ROLE}` });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the refill?" },
    { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
  );
  await createProjectDocument(core, {
    content: "# Refill notes\n\nWho is listening.\n",
    name: "Refill notes",
  });
  await setInterests([
    {
      session_id: watcher.session_id,
      topics: [
        `notifications.dispatch.issue.${issue.key}.>`,
        `notifications.dispatch.document.${core}.${DOCUMENT_SLUG}.>`,
      ],
    },
  ]);
  return { askId: ask.id, issueKey: issue.key };
}

/** The planner's live view. */
function livePath(): string {
  return `/agents/${planner.session_id}/live`;
}
/** The fixture's project document, under the running row's own project. */
function documentPath(): string {
  return `/projects/${rowKey("CORE")}/documents/${DOCUMENT_SLUG}`;
}

function refillingNotice(scope: Page | Locator): Locator {
  return scope.getByTestId("agent-list-refilling");
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
  const response = await fetch(new URL(routePath("get", "/api/v1/agents", {}), baseUrl), {
    headers: await userHeaders(),
  });
  const body = (await response.json()) as Refusal<"/api/v1/agents", "get">;
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

/** Opens the issue's Conversation and the header's Details, where the subscribers are. */
async function openIssueConversation(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/conversation`);
  await expect(page.getByRole("form", { name: "Comment composer" })).toBeVisible();
  await openIssueDetails(page);
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
  await page.goto(`/projects/${rowKey("CORE")}/issues`);
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
  await expect(refillingNotice(header)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
  await expect(refillingNotice(header)).toHaveAttribute("role", "status");
  await expect(page.getByRole("button", { name: "Subscribers", exact: true })).toBeVisible();
  await expect(page.getByTestId("issue-route-refilling")).toHaveText(
    `Route reach unknown: ${REFILLING}`
  );
  const suggestions = await openReferencePicker(page);
  await expect(refillingNotice(suggestions)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await page.goto(documentPath());
  await expect(refillingNotice(page)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
  await expect(page.getByRole("region", { name: "Subscribed agents" })).toHaveCount(0);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await page.goto("/");
  const recipients = askRecipients(page, fixture.askId);
  await expect(refillingNotice(recipients)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
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
  test.setTimeout(120_000);
  const me = rowPerson("alice");
  const fixture = await seedRefillFixture([planner, watcher], me);
  const alice = await asUser(browser, me);
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    // A complete read: every view names the agents and counts what it reaches.
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expectNoRefillingWording(agentsPage);

    const livePage = await alice.newPage();
    await livePage.goto(livePath());
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
    await page.goto(documentPath());
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
    await expect(refillingNotice(during)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
    await expect(during.getByRole("option", { name: plannerRow })).toBeVisible();
    await expect(issuePage.getByText("No reference matches.")).toHaveCount(0);

    await expectFreshViewsRefilling(page, fixture);

    // The two polling pages refreshed in place: rows and details from the complete read stay,
    // under the refilling notice, and nothing reads as gone or unreachable.
    await expect(refillingNotice(agentsPage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expect(agentsPage.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(agentsPage);
    await expectNoUnreachableWording(agentsPage);
    await expect(refillingNotice(livePage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expectPlannerFacts(livePage);
    await expectNoAbsenceClaim(livePage);
    await expectNoUnreachableWording(livePage);

    // The ask card read while the list refilled is the one a Retry brings back.
    await page.goto("/");
    const refilled = askRecipients(page, fixture.askId);
    await expect(refillingNotice(refilled)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });

    await setListsState("ok");

    // Recovery: the polls take the complete read back, and a Retry or a reload counts again.
    await expect(refillingNotice(agentsPage)).toHaveCount(0, { timeout: POLL_TIMEOUT });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expect(refillingNotice(livePage)).toHaveCount(0, { timeout: POLL_TIMEOUT });
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

    await page.goto(documentPath());
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
  test.setTimeout(120_000);
  const me = rowPerson("alice");
  const fixture = await seedRefillFixture([], me);
  const alice = await asUser(browser, me);
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    // A complete, empty read is a true absence, and each view says so.
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentsPage.getByRole("region", { name: "Agents empty state" })).toHaveText(
      "No agents are connected."
    );

    const livePage = await alice.newPage();
    await livePage.goto(livePath());
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
    await page.goto(documentPath());
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
    await expect(refillingNotice(during)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
    await expect(issuePage.getByText("No reference matches.")).toHaveCount(0);

    await expectFreshViewsRefilling(page, fixture);

    await expect(refillingNotice(agentsPage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expect(agentsPage.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(agentsPage);
    await expectNoUnreachableWording(agentsPage);
    await expect(refillingNotice(livePage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expectNoAbsenceClaim(livePage);
    await expectNoUnreachableWording(livePage);
  } finally {
    await alice.close();
  }
});

test("a listener refilling from the first read shows every view refilling", async ({ browser }) => {
  test.setTimeout(120_000);
  const me = rowPerson("alice");
  const fixture = await seedRefillFixture([planner, watcher], me);
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
  const alice = await asUser(browser, me);
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(refillingNotice(page)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
    await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(page);
    await expectNoUnreachableWording(page);

    await page.goto(livePath());
    await expect(refillingNotice(page)).toHaveText(REFILLING, { timeout: REFILL_TIMEOUT });
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
  const me = rowPerson("alice");
  const fixture = await seedRefillFixture([planner, watcher], me);
  const alice = await asUser(browser, me);
  await pollAgentListEvery(alice, ROW_POLL_MS);
  try {
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    const livePage = await alice.newPage();
    await livePage.goto(livePath());
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
    await expect(page.getByText(/^Could not load agents: /)).toBeVisible();
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto(livePath());
    await expect(page.getByTestId("agent-list-unreachable")).toHaveText(
      "Could not check the live session list — Envoy listener unreachable."
    );
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await openIssueConversation(page, fixture.issueKey);
    await expect(
      page
        .getByRole("alert")
        .filter({ hasText: "Subscribed agents unavailable — Envoy listener unreachable." })
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Subscribers", exact: true })).toBeVisible();
    await expect(page.getByTestId("issue-route-unknown")).toHaveText("Route reach unknown");
    const suggestions = await openReferencePicker(page);
    await expect(suggestions.getByTestId("agent-list-unreachable")).toHaveText(
      "Could not load live agents — Envoy listener unreachable."
    );
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto(documentPath());
    await expect(
      page
        .getByRole("alert")
        .filter({ hasText: "Subscribed agents unavailable — Envoy listener unreachable." })
    ).toBeVisible();
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    await expect(
      recipients
        .getByRole("alert")
        .filter({ hasText: "Subscribers unavailable — Envoy listener unreachable." })
    ).toBeVisible();
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
    // failed, not that the list is refilling.
    await expect(agentsPage.getByRole("alert")).toHaveText(/^Could not refresh agents: /, {
      timeout: POLL_TIMEOUT,
    });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expectNoRefillingWording(agentsPage);
    await expectNoAbsenceClaim(agentsPage);
    await expect(livePage.getByTestId("agent-list-unreachable")).toBeVisible({
      timeout: POLL_TIMEOUT,
    });
    await expectPlannerFacts(livePage);
    await expectNoRefillingWording(livePage);
    await expectNoAbsenceClaim(livePage);
  } finally {
    await alice.close();
  }
});
