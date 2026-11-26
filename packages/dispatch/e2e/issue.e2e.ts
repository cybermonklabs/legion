import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";
import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getIssue,
  getIssueEvents,
  mintAgentToken,
  patchIssue,
  putProjectDefaultRepository,
} from "./api";
import { recordClipboard } from "./clipboard";
import { needsYouCards } from "./editor";
import { nextFrames } from "./frames";
import { issueDetailsButton, issueDetailsRow, openIssueDetails } from "./issue-details";
import { clearIssueCreator, resetDatabase } from "./seed";
import { chunkFile, holdAssetUntilReleased } from "./shell";
import { expect, type Locator, test } from "./test";
import { assertPageFits, assertWhole } from "./unclipped";
import { asUser } from "./users";

test("database reset clears fake Envoy subscribers before the next issue", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const staleIssue = await createIssue({ project: "CORE", title: "Stale subscriber" });
  await setLiveSessions([{ session_id: "stale-session", title: "Stale session" }]);
  await setInterests([
    {
      session_id: "stale-session",
      topics: [
        `notifications.dispatch.issue.${staleIssue.key}`,
        `notifications.dispatch.issue.${staleIssue.key}.>`,
      ],
    },
  ]);
  // The one reset this file calls itself, and the row is about it: the `test` of this suite
  // empties the database before the row (e2e/test.ts), and this call proves what a reset does
  // IN a row - the fake Envoy's subscribers go with the tables, so the recycled issue key below
  // starts with none. Deleting it would leave the row asserting nothing.
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "No stale subscriber" });
  expect(issue.key).toBe(staleIssue.key);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    // The subscribers of this issue, by whichever read carries them: the landing of this page
    // on a cold load, whose one answer holds them and seeds them under the key the control
    // reads them by (`web/src/api/queries.ts`, `seedIssuePage`), and the subscribers' own route
    // for a read after that. What the row needs of it is that it answered - the fake Envoy was
    // asked for this recycled key and said nobody follows it - so either carrier is that
    // answer.
    const subscribers = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        url.pathname === `/api/v1/issues/${issue.key}/subscribers` ||
        (url.pathname === "/api/v1/landing" && url.searchParams.get("key") === issue.key)
      );
    });
    await page.goto(`/issues/${issue.key}`);
    expect((await subscribers).status()).toBe(200);
    await openIssueDetails(page);
    await expect(page.getByRole("button", { name: "Subscribers: 0" })).toBeVisible();
  } finally {
    await context.close();
  }
});

// @pr: Create through answer to close: the header's own Close, and the Reopen that puts the issue back into Backlog.
test("issue header closes an issue and reopens it into Backlog @pr", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Close from the header" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const close = page.getByRole("button", { name: "Close issue" });
    await expect(close).toBeVisible();
    const closing = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await close.click();
    expect((await closing).status()).toBe(200);
    await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
    const reopen = page.getByRole("button", { name: "Reopen issue" });
    await expect(reopen).toHaveAttribute("title", "Reopen into Backlog");
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        closed_at: expect.any(String),
        status: "done",
      });

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    // Done is a collapsed rail by default: it counts the closed card until the edges are shown.
    await expect(page.getByRole("region", { name: "Done (collapsed)" })).toContainText("1");
    await page.getByRole("button", { name: "Show Icebox & Done" }).click();
    const done = page.getByRole("region", { name: "Done" });
    await expect(
      done.getByRole("article", { name: `${issue.key} Close from the header` })
    ).toBeVisible();

    await page.goto(`/issues/${issue.key}`);
    const reopening = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await reopen.click();
    expect((await reopening).status()).toBe(200);
    await expect(page.getByText("This issue is closed.", { exact: true })).toHaveCount(0);
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({ closed_at: null, status: "backlog" });

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const backlog = page.getByRole("region", { name: "Backlog" });
    await expect(
      backlog.getByRole("article", { name: `${issue.key} Close from the header` })
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

const session = {
  actor: {
    kind: "session" as const,
    id: "e2e-session",
    origin: { session_title: "e2e-session-title", tmux: "dispatch:1.2" },
  },
  as: "agent" as const,
};

test("issue header identifies session and human creators", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await setLiveSessions([
    {
      session_id: "e2e-session",
      title: "chief of staff",
    },
  ]);
  const sessionIssue = await createIssue(
    { project: "CORE", title: "Session-authored issue" },
    session
  );
  const humanIssue = await createIssue({ project: "CORE", title: "Human-authored issue" });
  const historicalIssue = await createIssue({ project: "CORE", title: "Historical issue" });
  await clearIssueCreator(historicalIssue.key);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${sessionIssue.key}`);
    const sessionHeader = page.getByTestId("issue-header");
    // Who opened the issue is a row of Details, which stays open for alice on every issue below.
    await openIssueDetails(page);
    await expect(sessionHeader.getByTestId("issue-details-opened-by")).toContainText(
      /Opened by\s*chief of staff/
    );
    const sessionScreenshot = testInfo.outputPath("issue-header-session-creator.png");
    await sessionHeader.screenshot({ path: sessionScreenshot });
    await testInfo.attach("session-authored issue header", {
      contentType: "image/png",
      path: sessionScreenshot,
    });

    await page.goto(`/issues/${humanIssue.key}`);
    const humanHeader = page.getByTestId("issue-header");
    await openIssueDetails(page);
    await expect(humanHeader.getByTestId("issue-details-opened-by")).toContainText(
      /Opened by\s*alice/
    );
    const humanScreenshot = testInfo.outputPath("issue-header-human-creator.png");
    await humanHeader.screenshot({ path: humanScreenshot });
    await testInfo.attach("human-authored issue header", {
      contentType: "image/png",
      path: humanScreenshot,
    });

    await page.goto(`/issues/${historicalIssue.key}`);
    const historicalHeader = page.getByTestId("issue-header");
    await expect(historicalHeader).toBeVisible();
    await openIssueDetails(page);
    await expect(historicalHeader.getByTestId("issue-details-opened-by")).toHaveCount(0);
    await expect(historicalHeader.getByText("Opened by", { exact: true })).toHaveCount(0);
    const historicalScreenshot = testInfo.outputPath("issue-header-historical-creator.png");
    await historicalHeader.screenshot({ path: historicalScreenshot });
    await testInfo.attach("historical issue header", {
      contentType: "image/png",
      path: historicalScreenshot,
    });
  } finally {
    await context.close();
  }
});

test("issue header copies its key and persists its title, status, and route", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const copied = await recordClipboard(page);
  const issueWrites: string[] = [];
  page.on("request", (request) => {
    if (request.method() === "PATCH" && request.url().includes("/api/v1/issues/")) {
      issueWrites.push(`${new Date().toISOString()} PATCH ${request.url()} ${request.postData()}`);
    }
  });
  const attachIssueWrites = () =>
    testInfo.attach("issue-writes.log", {
      body: issueWrites.join("\n"),
      contentType: "text/plain",
    });

  try {
    await page.goto(`/issues/${issue.key}`);
    const heading = page
      .getByTestId("issue-header")
      .getByRole("heading", { level: 1, name: "First decision" });
    await expect(heading).toBeVisible();
    const copyKey = page.getByRole("button", { name: `Copy issue key ${issue.key}` });
    await copyKey.click();
    await expect(page.getByText("Copied", { exact: true })).toBeVisible();
    await expect.poll(copied).toEqual([issue.key]);
    // The same button copies the dispatch:// reference under Ctrl/Cmd.
    await copyKey.click({ modifiers: ["ControlOrMeta"] });
    await expect.poll(copied).toEqual([issue.key, `dispatch://${issue.key}`]);
    // The route's hint is visible text under the route in Details, not a tooltip a pointer
    // reader alone could reach.
    const details = await openIssueDetails(page);
    await expect(
      details.getByTestId("issue-details-route").locator("#issue-route-hint")
    ).toHaveText(
      "New asks, comments, and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim."
    );
    await expect(
      details.getByRole("button", { name: "Messages default to no route" })
    ).toHaveAccessibleDescription(/It is where messages go/);
    await page.screenshot({
      path: testInfo.outputPath("issue-header-1280.png"),
    });
    await heading.click();
    const issueTitle = page.getByLabel("Issue title");
    await issueTitle.fill("First decision revised");
    await issueTitle.press("Enter");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ title: "First decision revised" });
    await page.getByLabel("Status").selectOption("todo");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ status: "todo" });
    await page.getByRole("button", { name: "Messages default to no route" }).click();
    await page.getByLabel("Message route").fill("role:legion-controller-core");
    await page.getByRole("button", { name: "Save route" }).click();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        route: "role:legion-controller-core",
      });
    await page
      .getByRole("button", { name: "Messages default to role:legion-controller-core" })
      .click();
    const routeInput = page.getByLabel("Message route");
    await routeInput.fill("");
    // The field must still read "" when the earlier PATCH's response has been applied;
    // a revert here (not a wrong request body) is the race this test guards.
    await expect(page.getByRole("button", { name: "Save route" })).toBeEnabled();
    await expect(routeInput).toHaveValue("");
    await page.getByRole("button", { name: "Save route" }).click();
    try {
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ route: null });
    } finally {
      await attachIssueWrites();
    }
  } finally {
    await context.close();
  }
});

test("issue pages show subscribers, external-link fallbacks, and children", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });
  const child = await createIssue({
    parent: issue.key,
    project: "CORE",
    title: "Child decision",
  });
  const grandchild = await createIssue({
    parent: child.key,
    project: "CORE",
    title: "Grandchild decision",
  });
  await patchIssue(grandchild.key, { status: "done" });
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_issue", url: "https://github.com/sjawhar/legion/issues/815" }],
  });
  await createAsk(issue.key, { question: "First ask" }, session);
  await subscribeSession(issue.key);
  await expect
    .poll(async () =>
      (await getIssueEvents(issue.key)).some((event) => event.actor.id === "e2e-session")
    )
    .toBe(true);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    await openIssueDetails(page);
    await page.getByRole("button", { name: "Subscribers: 1" }).click();
    const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribedAgents).toContainText("e2e-session-title");
    await expect(subscribedAgents.getByText("e2e-session", { exact: true })).toHaveCount(0);
    await expect(subscribedAgents.locator("[title='e2e-session']")).toHaveCount(1);
    // Without GitHub App credentials the link still reads as the reference, never the raw address.
    await expect(page.getByRole("link", { name: "#815 sjawhar/legion" })).toHaveAttribute(
      "title",
      "GitHub details are unavailable for this repository."
    );
    await expect(
      page.getByRole("link", { name: "https://github.com/sjawhar/legion/issues/815" })
    ).toHaveCount(0);
    // No component attached anywhere on the chain: the Components row of Details says so.
    await expect(page.getByTestId("issue-details-components")).toContainText(
      "ComponentsNot attached"
    );
    await page.getByRole("tab", { name: "Children" }).click();
    const childRow = page
      .getByRole("tabpanel")
      .locator("li")
      .filter({ hasText: `${child.key} · Child decision` });
    await expect(
      childRow.getByRole("link", { name: `${child.key} · Child decision` })
    ).toBeVisible();
    // The row rolls up the whole subtree: the child itself plus its done grandchild.
    await expect(childRow).toContainText("1/2 done");
    await expect(childRow.locator("time")).toHaveCount(1);
  } finally {
    await context.close();
  }
});

// The subscribers endpoint reads a row's own `last_seen` (the session's last heartbeat as the
// listener still knows it) before the row's `updated_at`, which only says when the subscription
// was last written and can lag the heartbeat by the listener's whole interest refresh age. The
// two are hours apart here, so the chip's `<time>` says which one the page drew.
test("a subscriber no longer live shows its last heartbeat, not its subscription's last write", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Heartbeat of a departed agent" });
  const subscribedAt = Date.now() - 3 * 60 * 60 * 1000;
  const lastHeartbeat = Date.now() - 60 * 60 * 1000;
  // No live session at all: a session the registry still listed would carry its own heartbeat,
  // which the endpoint prefers over both of these.
  await setLiveSessions([]);
  await setInterests([
    {
      last_seen: lastHeartbeat,
      session_id: "e2e-departed-session",
      topics: [
        `notifications.dispatch.issue.${issue.key}`,
        `notifications.dispatch.issue.${issue.key}.>`,
      ],
      updated_at: subscribedAt,
    },
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    await openIssueDetails(page);
    await page.getByRole("button", { name: "Subscribers: 1" }).click();
    const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
    const row = subscribedAgents
      .locator("li")
      .filter({ has: page.locator("[title='e2e-departed-session']") });
    await expect(row.locator("time")).toHaveAttribute(
      "dateTime",
      new Date(lastHeartbeat).toISOString()
    );
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two open pages, and two parent changes made through the mover's own UI, each
// landing live on the page that watches.
test("editing the parent line moves the issue across Children tabs live", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const oldParent = await createIssue({ project: "CORE", title: "Old parent" });
  const newParent = await createIssue({ project: "CORE", title: "New parent" });
  const mover = await createIssue({
    parent: oldParent.key,
    project: "CORE",
    title: "Moving decision",
  });

  const context = await asUser(browser, "alice");
  const parentPage = await context.newPage();
  const moverPage = await context.newPage();
  try {
    // Neither page waits on the other, and each keeps its own baseline, both taken before the
    // first save below.
    await Promise.all([
      (async () => {
        await parentPage.goto(`/issues/${newParent.key}`);
        await parentPage.getByRole("tab", { name: "Children" }).click();
        await expect(parentPage.getByText("No child issues")).toBeVisible();
      })(),
      (async () => {
        await moverPage.goto(`/issues/${mover.key}`);
        await openIssueDetails(moverPage);
        await expect(moverPage.getByRole("link", { name: oldParent.key })).toBeVisible();
      })(),
    ]);
    await moverPage.getByRole("button", { name: "Edit parent issue" }).click();
    await moverPage.getByLabel("Parent").fill(newParent.key);
    await moverPage.getByRole("button", { name: "Save parent" }).click();
    await expect(moverPage.getByRole("link", { name: newParent.key })).toBeVisible();

    // The new parent's open page hears child.added on the live event feed and gains the row
    // without a reload.
    const movedRow = parentPage
      .getByRole("tabpanel")
      .getByRole("link", { name: `${mover.key} · Moving decision` });
    await expect(movedRow).toBeVisible();

    await moverPage.getByRole("button", { name: "Edit parent issue" }).click();
    await moverPage.getByLabel("Parent").fill("");
    await moverPage.getByRole("button", { name: "Save parent" }).click();
    await expect(moverPage.getByRole("button", { name: "None, set parent issue" })).toBeVisible();
    await expect
      .poll(() => getIssue(mover.key), { intervals: [50, 50, 100, 200, 500] })
      .toMatchObject({ parent: null });

    // And child.removed empties it again, still without a reload.
    await expect(movedRow).toHaveCount(0);
    await expect(parentPage.getByText("No child issues")).toBeVisible();
  } finally {
    await context.close();
  }
});

test("an answered ask remains in the issue conversation", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Newest ask" },
    session
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const askCard = page.getByTestId(`ask-${ask.id}`);
    await askCard.getByRole("radio", { name: "Ship" }).check();
    await askCard.getByRole("button", { exact: true, name: "Answer" }).click();
    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationAsk = page.getByTestId(`ask-${ask.id}`);
    await expect(conversationAsk).toHaveCount(1);
    await expect(conversationAsk).toContainText("Answered by");
  } finally {
    await context.close();
  }
});

test("a quote-anchored ask identifies its document in the margin and Conversation", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "A quoted passage.",
    title: "Anchored decision",
  });
  const ask = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote: "quoted passage" }, question: "What does this mean?" },
    session
  );
  if (ask.anchor === null || ask.anchor.block_id === null) {
    throw new Error("anchored ask is missing its document block");
  }
  const documentHref = `/issues/${issue.key}/spec#b-${encodeURIComponent(ask.anchor.block_id)}`;

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const marginCard = needsYouCards(page).getByTestId(`ask-${ask.id}`);
    await expect(marginCard.getByRole("link", { name: "spec.md" })).toHaveAttribute(
      "href",
      documentHref
    );
    await expect(marginCard).toContainText("quoted passage");

    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationCard = page.locator("#issue-conversation-panel").getByTestId(`ask-${ask.id}`);
    await expect(conversationCard.getByRole("link", { name: "spec.md" })).toHaveAttribute(
      "href",
      documentHref
    );
    await expect(conversationCard).toContainText("quoted passage");
  } finally {
    await alice.close();
  }
});

// @pr: Create through answer to close: a close has to take the issue's asks out of the Inbox, and a pin stays the pinner's own.
test("closing an issue removes its asks from the inbox and pinning stays private @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });
  const otherIssue = await createIssue({ project: "CORE", title: "Second decision" });
  await createAsk(issue.key, { question: "First ask" }, session);
  await createAsk(otherIssue.key, { question: "Second ask" }, session);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  // The Inbox is a second tab of alice's own browser: it makes no write, so sharing the keyed
  // write registry and `navigator.locks` with the issue page costs this row nothing.
  const inbox = await alice.newPage();
  try {
    await Promise.all([page.goto(`/issues/${issue.key}`), inbox.goto("/")]);
    // The Inbox's rows are what the close takes a question out of: from 1280 px a card is drawn
    // for the selected row alone (INFRA-575), so the rows are the list.
    await expect(inbox.locator("[data-inbox-row]")).toHaveCount(2);
    await page.getByRole("button", { name: "Close issue" }).click();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        closed_at: expect.any(String),
        status: "done",
      });
    await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
    await expect(inbox.locator("[data-inbox-row]")).toHaveCount(1);
    await expect(inbox.getByText("First ask", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Pin issue" }).click();
    await expect(page.getByRole("button", { name: "Unpin issue" })).toBeEnabled();
    await page.reload();
    await expect(page.getByRole("heading", { name: "Pinned" })).toBeVisible();
    const bob = await asUser(browser, "bob");
    const bobPage = await bob.newPage();
    try {
      // Alice's issues are nobody else's own work; the closed one is gone from the cleared
      // filter too.
      await bobPage.goto("/?assignee=everyone");
      await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(1);
      await expect(bobPage.getByText("First ask", { exact: true })).toHaveCount(0);
      await expect(bobPage.getByRole("heading", { name: "Pinned" })).toHaveCount(0);
    } finally {
      await bob.close();
    }
  } finally {
    await alice.close();
  }
});

test("pinning survives an immediate full navigation", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Navigation-safe event pin" });
  await createMessage(issue.key, { body: "Pinned before navigation" });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    const initialState = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/me/state" && response.ok()
    );
    await page.goto(`/issues/${issue.key}/conversation`);
    await initialState;
    const getIntercepted = Promise.withResolvers<void>();
    const releaseGet = Promise.withResolvers<void>();
    const queueState = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/me/state" && response.ok()
    );
    const putIntercepted = Promise.withResolvers<void>();
    const releasePut = Promise.withResolvers<void>();
    let firstPut = true;
    await page.route("**/api/v1/me/state", async (route) => {
      getIntercepted.resolve();
      await releaseGet.promise;
      await route.continue();
    });
    await page.route("**/api/v1/me/issues/*/state", async (route) => {
      const body = route.request().postDataJSON();
      if (
        route.request().method() !== "PUT" ||
        !firstPut ||
        body === null ||
        typeof body !== "object" ||
        !("dismissed" in body)
      ) {
        await route.continue();
        return;
      }
      firstPut = false;
      putIntercepted.resolve();
      await releasePut.promise;
      await route.continue();
    });
    await page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ has: page.getByText("Pinned before navigation", { exact: true }) })
      .getByRole("button", { name: "Pin" })
      .click();
    await getIntercepted.promise;
    releaseGet.resolve();
    await queueState;
    await putIntercepted.promise;
    const navigated = page.waitForEvent(
      "framenavigated",
      (frame) => frame === page.mainFrame() && new URL(frame.url()).pathname === "/"
    );
    const navigation = page.goto("/");
    await navigated;
    releasePut.resolve();
    await navigation;
    await page.unroute("**/api/v1/me/state");
    await page.unroute("**/api/v1/me/issues/*/state");
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Pinned" }).click();
    await expect(page.getByText("Pinned before navigation", { exact: true })).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a human can unsubscribe an agent from an issue and the session is told", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Subscriber removal" });
  await setLiveSessions([{ session_id: "e2e-unsub-session", title: "Worker (e2e)" }]);
  await setInterests([
    {
      session_id: "e2e-unsub-session",
      topics: [
        `notifications.dispatch.issue.${issue.key}`,
        `notifications.dispatch.issue.${issue.key}.>`,
      ],
    },
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();

  try {
    await page.goto(`/issues/${issue.key}`);
    await openIssueDetails(page);
    await page.getByRole("button", { name: "Subscribers: 1" }).click();
    const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribedAgents.getByText("Worker (e2e)", { exact: true })).toBeVisible();
    // Its running state is the agent registry's, which lists it.
    await expect(subscribedAgents.locator('[data-agent-name="e2e-unsub-session"]')).toHaveAttribute(
      "data-agent-state",
      "running"
    );

    await subscribedAgents.getByRole("button", { name: "Unsubscribe" }).click();
    const dialog = page.getByRole("dialog", { name: "Unsubscribe" });
    await expect(dialog).toContainText(
      `Unsubscribe Worker (e2e) from ${issue.key}? They will be told.`
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();

    await expect(page.getByRole("region", { name: "Subscribed agents" })).toHaveCount(0);

    await expect
      .poll(async () =>
        (await getUnsubscribeCalls()).some((call) => call.session_id === "e2e-unsub-session")
      )
      .toBe(true);

    await expect
      .poll(async () =>
        (await getIssueEvents(issue.key)).some((event) => event.type === "subscription.removed")
      )
      .toBe(true);

    // The removal is an event, so it is the Activity tab's line and not a Conversation card.
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.getByRole("list", { name: "Issue activity" })).toContainText("unsubscribed");
    await page.getByRole("tab", { name: "Conversation" }).click();
    await expect(page.getByRole("list", { name: "Conversation turns" })).not.toContainText(
      "unsubscribed"
    );
  } finally {
    await alice.close();
  }
});

async function subscribeSession(issueKey: string): Promise<void> {
  await setLiveSessions([{ session_id: "e2e-session", title: "e2e-session-title" }]);
  await setInterests([
    {
      session_id: "e2e-session",
      topics: [
        `notifications.dispatch.issue.${issueKey}`,
        `notifications.dispatch.issue.${issueKey}.>`,
      ],
    },
  ]);
}

test("issue header keeps each group whole, its properties in Details, and the whose-turn indicator in view at every width @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Header on a phone" });
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }],
    labels: ["Frontend", "api"],
    priority: 1,
  });
  await createAsk(issue.key, { question: "Which layout?" }, session);
  await subscribeSession(issue.key);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const top = async (locator: Locator) => Math.round((await locator.boundingBox())?.y ?? -1);
  const middle = async (locator: Locator) => {
    const box = await locator.boundingBox();
    return Math.round((box?.y ?? -1) + (box?.height ?? 0) / 2);
  };
  const inView = async (locator: Locator, width: number) => {
    const box = await locator.boundingBox();
    return box !== null && box.x >= 0 && box.x + box.width <= width;
  };
  try {
    const pageFits = () =>
      page.evaluate(
        () => document.documentElement.scrollWidth === document.documentElement.clientWidth
      );
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    const indicator = page.getByTestId("issue-whose-turn");
    const close = page.getByRole("button", { name: "Close issue" });
    const detailsButton = issueDetailsButton(page);
    await expect(indicator).toHaveText("Waiting on you (1)");
    // Details is closed until the reader opens it: its rows are not on the page.
    await expect(detailsButton).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("issue-details")).toBeHidden();

    // The State group keeps the status and the priority on one line; Close and Details end the
    // header's last line together, after the labels.
    const stateRow = await top(page.getByLabel("Status"));
    expect(await top(page.getByLabel(/^Priority of /))).toBe(stateRow);
    expect(await top(close)).toBe(await top(detailsButton));
    expect(await top(close)).toBeGreaterThan(stateRow);
    expect(await top(page.getByTestId("issue-labels"))).toBeGreaterThan(stateRow);
    expect(await inView(indicator, 390)).toBe(true);
    expect(await inView(detailsButton, 390)).toBe(true);
    // Measured 314 px at 390 px (2026-11-23). A line of this header is at least 44 px tall here,
    // so one line piled on reaches 358 px and fails.
    const phoneHeader = await header.boundingBox();
    expect(phoneHeader?.height ?? Number.POSITIVE_INFINITY).toBeLessThan(358);
    await expect(pageFits()).resolves.toBe(true);
    const phoneShot = testInfo.outputPath("issue-header-390.png");
    await page.screenshot({ path: phoneShot });
    await testInfo.attach("issue header (390px)", { contentType: "image/png", path: phoneShot });

    // Open, Details holds who opened the issue, the subscribers and the GitHub link, as its
    // reference and never its raw address, and the page still never scrolls sideways.
    const details = await openIssueDetails(page);
    await expect(details.getByTestId("issue-details-opened-by")).toContainText("alice");
    const subscribers = details.getByRole("button", { name: "Subscribers: 1" });
    const link = details.getByRole("link", { name: "#1051 sjawhar/legion" });
    await expect(subscribers).toBeVisible();
    await expect(link).toBeVisible();
    await expect(
      page.getByRole("link", { name: "https://github.com/sjawhar/legion/pull/1051" })
    ).toHaveCount(0);
    await expect(pageFits()).resolves.toBe(true);
    await detailsButton.click();

    await page.setViewportSize({ height: 900, width: 1280 });
    // The shell swaps its phone bar for the sidebar, and the margin its sheet for the column, in
    // a React commit after the media query's change event. Until then the phone DOM is laid out
    // under the desktop CSS, which squeezes <main> to its padding at the right edge.
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible();
    await expect(indicator).toHaveText("Waiting on you (1)");
    expect(await inView(indicator, 1280)).toBe(true);
    // Two labels leave room for Close and Details on their line.
    expect(await middle(close)).toBe(await middle(detailsButton));
    // Measured 134 px at 1280 px (2026-11-23), under the bound the header had before.
    const desktopHeader = await header.boundingBox();
    expect(desktopHeader?.height ?? Number.POSITIVE_INFINITY).toBeLessThanOrEqual(140);
    await expect(pageFits()).resolves.toBe(true);

    await page.setViewportSize({ height: 900, width: 1536 });
    await expect(indicator).toHaveText("Waiting on you (1)");
    expect(await inView(indicator, 1536)).toBe(true);
    // At a desktop width a row of Details draws its value beside its name.
    await openIssueDetails(page);
    const routeRow = page.getByTestId("issue-details-route");
    const nameTop = await top(routeRow.locator("dt"));
    const valueTop = await top(routeRow.locator("dd"));
    // Beside it: the two share a line (baseline alignment moves the taller value a few pixels).
    expect(Math.abs(nameTop - valueTop)).toBeLessThan(20);
    await expect(pageFits()).resolves.toBe(true);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a cold issue load and the reload the row proves the indicator survives, with a
// live comment written on either side of it.
test("issue header flips between Waiting on you and Waiting on agents as the newest reply changes", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Whose turn" });
  const ask = await createAsk(issue.key, { question: "Which layout?" }, session);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const indicator = page.getByTestId("issue-whose-turn");
    await expect(indicator).toHaveText("Waiting on you (1)");

    // A human clarification hands the turn to the asker, live and after a reload.
    await createComment(issue.key, { ask_id: ask.id, body: "Which widths matter?" });
    await expect(indicator).toHaveText("Waiting on agents (1)");
    await page.reload();
    await expect(indicator).toHaveText("Waiting on agents (1)");

    // The agent's reply hands it back.
    await createComment(issue.key, { ask_id: ask.id, body: "390, 1280 and 1536." }, session);
    await expect(indicator).toHaveText("Waiting on you (1)");
  } finally {
    await context.close();
  }
});

// The header's title slot takes a row of its own below 1536 px and shares its row with the State
// and Attention groups only from there, and then only where they fit beside the whole title: so
// however busy those groups are, and whether Details is open or shut, the title keeps at least
// half the card, or its whole width on one line where that is less.
test("issue header keeps the title at least half the card beside busy State and Attention groups", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Header keeps its title readable" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The title is not the page's only `h1`: the shell's loading skeleton is one, and a rendered
  // document whose text opens on a top-level heading puts that heading on the page too. The
  // header holds the one heading the issue is identified by.
  const title = page.getByTestId("issue-header").getByRole("heading", { level: 1 });
  const measureTitle = () =>
    page.evaluate(() => {
      const heading = document.querySelector("[data-testid=issue-header] h1") as HTMLElement;
      const header = document.querySelector("[data-testid=issue-header]") as HTMLElement;
      const style = getComputedStyle(header);
      // The title's width on one line, in its own font, laid out apart from the header.
      const probe = heading.cloneNode(true) as HTMLElement;
      probe.style.cssText =
        "position:absolute;visibility:hidden;width:max-content;white-space:nowrap";
      document.body.append(probe);
      const oneLine = probe.getBoundingClientRect().width;
      probe.remove();
      return {
        contentWidth:
          header.clientWidth -
          Number.parseFloat(style.paddingLeft) -
          Number.parseFloat(style.paddingRight),
        oneLine,
        width: heading.getBoundingClientRect().width,
      };
    });
  const widths = [1024, 1279, 1440, 1600];
  const titleHoldsHalf = async (label: string) => {
    for (const width of widths) {
      await page.setViewportSize({ height: 800, width });
      await nextFrames(page);
      const measured = await measureTitle();
      expect(
        measured.width,
        `title ${measured.width}px (${measured.oneLine}px on one line) of a ${measured.contentWidth}px card at ${width}px ${label}`
      ).toBeGreaterThanOrEqual(Math.min(measured.contentWidth / 2, measured.oneLine - 1));
    }
  };
  try {
    await page.setViewportSize({ height: 800, width: 1024 });
    await page.goto(`/issues/${issue.key}`);
    await expect(title).toHaveText("Header keeps its title readable");
    await titleHoldsHalf("with quiet groups");
    const shot = testInfo.outputPath("issue-header-title-1024.png");
    await page.setViewportSize({ height: 800, width: 1024 });
    await page.screenshot({ path: shot });
    await testInfo.attach("issue header title (1024px)", { contentType: "image/png", path: shot });

    // Busy groups: whose turn, a label and a subscriber, beside a longer title.
    await patchIssue(issue.key, {
      labels: ["api"],
      title: "Migrate the issue header to a single row",
    });
    await createAsk(issue.key, { question: "Which layout?" }, session);
    await subscribeSession(issue.key);
    // One reload: the subscription is Envoy's, not an issue event, so no refresh carries it.
    await page.reload();
    await expect(title).toHaveText("Migrate the issue header to a single row");
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");
    await titleHoldsHalf("with busy groups, Details shut");
    await openIssueDetails(page);
    await expect(page.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await titleHoldsHalf("with busy groups, Details open");
  } finally {
    await context.close();
  }
});

test("issue header delivers the click that ends a title edit to the control under the pointer", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Short title" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // A real click is mousedown then mouseup: the mousedown blurs the title editor, and the header
  // must not re-lay out on that blur, or the mouseup lands on a different element and the
  // browser delivers no click to the control the user pressed. The raw mouse sequence skips
  // `click()`'s actionability wait, so wait for the control ourselves: the pin and Close stay
  // `disabled` while their previous write is in flight, and a browser drops a click on a
  // disabled button.
  const pressWhileEditingTitle = async (control: Locator) => {
    await page.getByTestId("issue-header").getByRole("heading", { level: 1 }).click();
    await expect(page.getByLabel("Issue title")).toBeFocused();
    await expect(control).toBeEnabled();
    await expect(control).toBeVisible();
    // The wait above drew the control, and a drawn element has a box, so the box is there.
    const box = await control.boundingBox();
    if (box === null) {
      throw new Error("the control must be visible while the title is being edited");
    }
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.up();
  };
  try {
    for (const width of [1024, 1279]) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      await expect(
        page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Short title" })
      ).toBeVisible();

      const pin = page.getByRole("button", { name: "Pin issue" });
      await pressWhileEditingTitle(pin);
      await expect(page.getByRole("button", { name: "Unpin issue" })).toHaveAttribute(
        "aria-pressed",
        "true"
      );
      await pressWhileEditingTitle(page.getByRole("button", { name: "Unpin issue" }));
      await expect(pin).toHaveAttribute("aria-pressed", "false");

      await pressWhileEditingTitle(page.getByRole("button", { name: "Close issue" }));
      await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ status: "done" });
      if (width === 1024) {
        const shot = testInfo.outputPath("issue-header-title-edit-click-1024.png");
        await page.screenshot({ path: shot });
        await testInfo.attach("close clicked straight out of a title edit (1024px)", {
          contentType: "image/png",
          path: shot,
        });
      }
      await patchIssue(issue.key, { status: "triage" });
    }
  } finally {
    await context.close();
  }
});

test("issue header reassigns through the Assignee picker; a personal token's issues go to its owner", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  // A human's issue is assigned to its creator, whatever casing their sign-in carries.
  const issue = await createIssue(
    { project: "CORE", title: "Who answers this?" },
    { login: "Bob" }
  );
  expect(issue.assignee).toBe("bob");

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const control = page.getByLabel(`Assignee of ${issue.key}`);
    await expect(control).toHaveValue("bob");
    // Who has signed in is read only once the reader reaches for the control.
    await control.focus();
    await expect(control.locator("option")).toHaveText(["Unassigned", "alice", "bob"]);

    // Anyone signed in may reassign: Alice takes it, and the header shows her at once.
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await control.selectOption("alice");
    expect((await patch).postDataJSON()).toEqual({ assignee: "alice" });
    await expect(
      page.getByTestId("issue-header").locator("span", { hasText: /^alice$/ })
    ).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ assignee: "alice" });
    const events = await getIssueEvents(issue.key);
    expect(events.filter((event) => event.type === "issue.updated").at(-1)).toMatchObject({
      actor: { id: "alice", kind: "user" },
      payload: { assignee: "alice" },
    });
    const shot = testInfo.outputPath(`issue-header-assignee-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`issue header assignee (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    // Clearing it leaves the issue unassigned, and a reload agrees.
    await control.selectOption("");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ assignee: null });
    await page.reload();
    await expect(page.getByLabel(`Assignee of ${issue.key}`)).toHaveValue("");
    await expect(
      page.getByTestId("issue-header").locator("span", { hasText: /^Unassigned$/ })
    ).toBeVisible();

    // A session acting for Alice through her personal token creates issues assigned to her; a
    // child the shared token (nobody's) creates under her issue inherits her.
    const token = await mintAgentToken("Architect", "alice");
    const agent = {
      actor: { id: "architect-session", kind: "session" as const },
      as: "agent" as const,
      token,
    };
    const hers = await createIssue({ project: "CORE", title: "Minted for Alice" }, agent);
    expect(hers.assignee).toBe("alice");
    const child = await createIssue(
      { parent: hers.key, project: "CORE", title: "Child of Alice's issue" },
      { actor: agent.actor, as: "agent" }
    );
    expect(child.assignee).toBe("alice");
    await page.goto(`/issues/${hers.key}`);
    await expect(page.getByLabel(`Assignee of ${hers.key}`)).toHaveValue("alice");
  } finally {
    await context.close();
  }
});

/** The repositories a repository box suggests: its own `<datalist>`, which is never drawn, so
 *  no role, text or visibility query reaches it. */
async function repoSuggestions(box: Locator): Promise<string[]> {
  return await box.evaluate((element) =>
    Array.from((element as HTMLInputElement).list?.options ?? []).map((option) => option.value)
  );
}

test("issue header suggests its project's default repository, and a child resolves to the repository the issue takes", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" });
  await putProjectDefaultRepository("CORE", "acme/widgets");
  await putProjectDefaultRepository("OPS", "acme/elsewhere");
  const parent = await createIssue({ project: "CORE", title: "Change the pinned toolchain" });
  expect(parent).toMatchObject({ repo: null, resolved_repo: "acme/widgets" });
  const child = await createIssue({
    force: true,
    parent: parent.key,
    project: "CORE",
    title: "Bump",
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${parent.key}`);
    const header = page.getByTestId("issue-header");
    // The repository is a row of Details; once opened, Details stays open for alice across the
    // reload and the other issue below.
    await openIssueDetails(page);
    const control = page.getByLabel(`Repository of ${parent.key}`, { exact: true });
    // The box holds the issue's own repository and it has none, so its placeholder names the
    // project's default, which the issue inherits; the box's description says the same in
    // words, which is what a screen reader hears.
    await expect(control).toHaveValue("");
    await expect(control).toHaveAttribute("placeholder", "acme/widgets");
    await expect(control).toHaveAccessibleDescription(
      "acme/widgets, inherited or the project's default"
    );
    // The box suggests the project's default repository; another project's is not offered.
    await expect.poll(() => repoSuggestions(control)).toEqual(["acme/widgets"]);

    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${parent.key}`
    );
    await control.fill("acme/gadgets");
    await control.press("Enter");
    expect((await patch).postDataJSON()).toEqual({ repo: "acme/gadgets" });
    // The issue has a repository of its own now: the box holds it, and a control inside the box
    // clears it.
    await expect(control).toHaveValue("acme/gadgets");
    await expect(control).toHaveAccessibleDescription("acme/gadgets");
    // The child names no repository of its own, so it resolves to its parent's.
    await expect
      .poll(() => getIssue(child.key))
      .toMatchObject({ repo: null, resolved_repo: "acme/gadgets" });
    const events = await getIssueEvents(parent.key);
    expect(events.filter((event) => event.type === "issue.updated").at(-1)).toMatchObject({
      actor: { id: "alice", kind: "user" },
      payload: { repo: "acme/gadgets", resolved_repo: "acme/gadgets" },
    });
    const shot = testInfo.outputPath(`issue-header-repository-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`issue header repository (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    // Clearing the box's own control clears the issue's repository: it resolves to the project's
    // default again.
    await header.getByRole("button", { name: `Clear the repository of ${parent.key}` }).click();
    await expect
      .poll(() => getIssue(parent.key))
      .toMatchObject({ repo: null, resolved_repo: "acme/widgets" });
    await page.reload();
    await expect(control).toHaveValue("");
    await expect(control).toHaveAttribute("placeholder", "acme/widgets");

    // A project with no default repository has nothing to suggest, and its issues take a
    // repository all the same: Details carries the row with an empty suggestion list. The box
    // takes its suggestion from the project list, so the row waits until the bare issue's page
    // has read that list, and then reads the box: in the landing of this page, which carries the
    // list in its shell and seeds it under the key the box reads it by
    // (`web/src/api/queries.ts`, `seedShell`), or on its own route for a read after that. A read
    // that the page before it made for the project.created event does not count.
    await createProject({ key: "BARE", name: "Bare" });
    const bare = await createIssue({ project: "BARE", title: "No default repository" });
    expect(bare.resolved_repo).toBeNull();
    const projectsRead = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        (url.pathname === "/api/v1/projects" &&
          new URL(page.url()).pathname === `/issues/${bare.key}`) ||
        (url.pathname === "/api/v1/landing" && url.searchParams.get("key") === bare.key)
      );
    });
    await page.goto(`/issues/${bare.key}`);
    await projectsRead;
    await expect(issueDetailsRow(page, "parent")).toContainText("Parent");
    await expect(issueDetailsRow(page, "repository")).toContainText("Repository");
    const bareControl = page.getByLabel(`Repository of ${bare.key}`, { exact: true });
    await expect(bareControl).toHaveAttribute("placeholder", "owner/name");
    expect(await repoSuggestions(bareControl)).toEqual([]);
  } finally {
    await context.close();
  }
});

test("an open issue's header follows its project's default repository as it is set and cleared", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultRepository("CORE", "acme/widgets");
  const issue = await createIssue({ project: "CORE", title: "Follows the default" });
  expect(issue).toMatchObject({ repo: null, resolved_repo: "acme/widgets" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // Each write below appends `project.updated`, and the refresh it starts is held by the
    // scheduler for at most `MIN_REFETCH_INTERVAL_MS`; that is a page timer, so the page's own
    // clock fires it.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);
    await openIssueDetails(page);
    const control = page.getByLabel(`Repository of ${issue.key}`, { exact: true });
    // The box holds the issue's own repository, which it has none of, so what it resolves to is
    // its placeholder, and the project's default is its suggestion.
    await expect(control).toHaveAttribute("placeholder", "acme/widgets");
    await expect.poll(() => repoSuggestions(control)).toEqual(["acme/widgets"]);

    // A new default: the page shows it and suggests it without a reload.
    await putProjectDefaultRepository("CORE", "acme/gadgets");
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(control).toHaveAttribute("placeholder", "acme/gadgets");
    await expect(control).toHaveAccessibleDescription(
      "acme/gadgets, inherited or the project's default"
    );
    await expect.poll(() => repoSuggestions(control)).toEqual(["acme/gadgets"]);

    // The default cleared: nothing resolves, and there is nothing to suggest.
    await putProjectDefaultRepository("CORE", null);
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(control).toHaveAttribute("placeholder", "owner/name");
    await expect(control).toHaveAccessibleDescription("No repository");
    await expect.poll(() => repoSuggestions(control)).toEqual([]);

    // The default set again: the page shows it again.
    await putProjectDefaultRepository("CORE", "acme/widgets");
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(control).toHaveAttribute("placeholder", "acme/widgets");
    await expect(control).toHaveValue("");
  } finally {
    await context.close();
  }
});

// @pr: INFRA-2543's whole point, which only the real stack can show: the server takes a
// repository other than the project's default, and this is the one row that drives such a
// repository through the page - typed into the box, written, and read back from the server on a
// reload.
test("an issue takes a repository other than its project's default, and keeps it across a reload @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultRepository("CORE", "acme/widgets");
  const issue = await createIssue({ project: "CORE", title: "Work in another repository" });
  expect(issue).toMatchObject({ repo: null, resolved_repo: "acme/widgets" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    await openIssueDetails(page);
    const control = page.getByLabel(`Repository of ${issue.key}`, { exact: true });
    await expect(control).toHaveValue("");
    // The project's default is acme/widgets; the box suggests it and takes anything else.
    await expect.poll(() => repoSuggestions(control)).toEqual(["acme/widgets"]);

    const written = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await control.fill("other/elsewhere");
    await control.press("Enter");
    expect((await written).status()).toBe(200);
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({ repo: "other/elsewhere", resolved_repo: "other/elsewhere" });

    // The server kept it: the page reads it back as the issue's own, with the box's own clear
    // control beside it.
    await page.reload();
    await expect(control).toHaveValue("other/elsewhere");
    await expect(
      page
        .getByTestId("issue-header")
        .getByRole("button", { name: `Clear the repository of ${issue.key}` })
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

/** Where `refusal` stands against the Repository box: wholly below it, and inside the cell the
 *  box's row gives its value. */
async function refusalBelowBox(box: Locator, refusal: Locator) {
  const [boxRect, refusalRect, cellRect] = await Promise.all([
    box.boundingBox(),
    refusal.boundingBox(),
    box.evaluate((node) => node.closest("dd")?.getBoundingClientRect().toJSON()),
  ]);
  if (boxRect === null || refusalRect === null || cellRect === undefined) {
    throw new Error("expected the box, the refusal and the cell to have boxes");
  }
  return {
    below: refusalRect.y >= boxRect.y + boxRect.height - 0.5,
    insideCell: refusalRect.x + refusalRect.width <= cellRect.right + 0.5,
  };
}

// The Repository line of today's Details wraps, so a refusal moves whole onto the line under the
// box rather than standing beside it and squeezing it (dispatch://INFRA-5200, c-review F3). At
// 360 px, both refusals the line draws: its own, for a name that is no `owner/name`, and the
// server's, with its Retry.
test("at 360 px the Repository line puts its own refusal and the server's under the box", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultRepository("CORE", "acme/widgets");
  const issue = await createIssue({ project: "CORE", title: "A narrow refusal" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 800, width: 360 });
    await page.goto(`/issues/${issue.key}`);
    await openIssueDetails(page);
    const control = page.getByLabel(`Repository of ${issue.key}`, { exact: true });
    const line = page.getByTestId("issue-header");

    await control.fill("acme gadgets");
    await control.press("Enter");
    const own = line.getByRole("alert").filter({ hasText: "Write the repository as owner/name." });
    await expect(own).toBeVisible();
    expect(await refusalBelowBox(control, own)).toEqual({ below: true, insideCell: true });

    const refusal = "repo must be owner/name, said the server";
    await page.route(`**/api/v1/issues/${issue.key}`, (route) =>
      route.request().method() === "PATCH"
        ? route.fulfill({
            body: JSON.stringify({ code: "INVALID_ISSUE", error: refusal }),
            contentType: "application/json",
            status: 400,
          })
        : route.fallback()
    );
    await control.fill("acme/gadgets");
    await control.press("Enter");
    const server = line.getByText(refusal);
    await expect(server).toBeVisible();
    expect(await refusalBelowBox(control, server)).toEqual({ below: true, insideCell: true });
  } finally {
    await context.close();
  }
});

test("an issue's whole title wraps at every width, whether prose or one unbroken word @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  // Five repeats of a 48-character sentence, cut to exactly 200: the length the spec names. It
  // ends mid-sentence on a letter, so the server trims nothing and the page shows all 200.
  const title200 = "Dispatch cuts off titles that are far too long. ".repeat(5).slice(0, 200);
  const prose = await createIssue({ project: "CORE", title: title200 });
  expect(prose.title).toHaveLength(200);
  // A single unbroken 100-character string: no space for the browser to break on, so wrapping
  // depends on `break-words` rather than the ordinary word-wrap every other title gets for free.
  const title100 = "supercalifragilisticexpialidocious".repeat(3).slice(0, 100);
  expect(title100).toHaveLength(100);
  expect(title100).not.toContain(" ");
  const oneWord = await createIssue({ project: "CORE", title: title100 });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    for (const issue of [prose, oneWord]) {
      await page.goto(`/issues/${issue.key}`);
      const title = page.getByTestId("issue-header").getByRole("heading", { level: 1 });
      await expect(title).toHaveText(issue.title);
      // Whole means nothing between the title and the document clips it: no clamp, ellipsis, or
      // box shorter or narrower than the text it holds, at the width this project renders.
      await assertWhole(title);
      // The one unbroken word wraps rather than pushing the page wider than the viewport.
      await assertPageFits(page);
      await page.screenshot({
        path: testInfo.outputPath(`issue-title-${issue.key}-whole.png`),
        fullPage: true,
      });
    }
  } finally {
    await context.close();
  }
});

// Whether Details is open is the reader's, and nothing else's: closed until they open it, then
// open for them on every issue they go to, until they close it, and then closed on every issue.
// Open, it draws a pull request under "Pull requests" and any other link under "Links", each a
// list the layout rule caps (`pull-requests`, `links`).
test("Details stays as the reader left it, open or closed, from one issue to the next", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "First issue" });
  const second = await createIssue({ project: "CORE", title: "Second issue" });
  const third = await createIssue({ project: "CORE", title: "Third issue" });
  const notes = "https://example.com/design-notes/issue-header-details";
  await patchIssue(second.key, {
    external_links: [
      { kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/2051" },
      { kind: "url", url: notes },
    ],
  });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const heading = (title: string) =>
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: title });
    await page.goto(`/issues/${first.key}`);
    await expect(heading("First issue")).toBeVisible();
    await expect(issueDetailsButton(page)).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("issue-details")).toBeHidden();

    await openIssueDetails(page);
    await page.goto(`/issues/${second.key}`);
    await expect(heading("Second issue")).toBeVisible();
    await expect(issueDetailsButton(page)).toHaveAttribute("aria-expanded", "true");
    await expect(issueDetailsRow(page, "repository")).toBeVisible();
    // The pull request is a row of its own, and the plain link is under Links, as its address.
    const pullRequests = issueDetailsRow(page, "pull-requests");
    await expect(pullRequests.locator("dt")).toHaveText("Pull requests");
    await expect(pullRequests.locator('[data-layout-group="pull-requests"] a')).toHaveAttribute(
      "href",
      "https://github.com/sjawhar/legion/pull/2051"
    );
    const links = issueDetailsRow(page, "links");
    await expect(links.locator("dt")).toHaveText("Links");
    await expect(
      links.locator('[data-layout-group="links"]').getByRole("link", { name: notes })
    ).toHaveAttribute("href", notes);
    await expect(pullRequests.getByRole("link", { name: notes })).toHaveCount(0);

    await issueDetailsButton(page).click();
    await expect(page.getByTestId("issue-details")).toBeHidden();
    await page.goto(`/issues/${third.key}`);
    await expect(heading("Third issue")).toBeVisible();
    await expect(issueDetailsButton(page)).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("issue-details")).toBeHidden();
  } finally {
    await context.close();
  }
});

/** One animation frame of the page's own clock, the step the row below spends it in. */
const FRAME_MS = 16;

/** The most of the page's clock the row below lets a tab's panel take to draw once its chunk has
 *  run: ten frames. A panel drawn in an ordinary update needs none of it, since React schedules
 *  that render on a message task and not a timer; and it is well inside the 300 ms React holds
 *  back a `Suspense` boundary's content after its fallback (`FALLBACK_THROTTLE_MS` in react-dom),
 *  whose timer a paused clock fires only as the row spends it. */
const PANEL_DRAWN_WITHIN_MS = 160;

// A landing on an issue tab draws the shell, the issue and the tab strip while the tab's own
// chunk is still on its way, with the tab's skeleton in its panel, and then the panel as soon as
// the chunk has run. Every panel but the Spec's is a chunk of its own
// (`web/src/features/issue/tab-chunks.ts`), which the entry starts beside the landing read
// (`preloadRouteChunk`'s tab arm, `web/src/app.tsx`) and the panel joins, drawn through
// `chunkComponent` (`web/src/features/shell/chunk-component.tsx`): its skeleton until the chunk is
// in, then the panel in an ordinary update. Behind `lazy` and a `Suspense`, as the panels were
// before 82e36c8d, the panel's first render suspends and React holds the boundary's content until
// 300 ms after its skeleton, on a page timer, so a chunk that landed early still drew its panel
// late. The page's clock is paused here, so that timer fires only as the row spends the clock,
// and the row spends at most `PANEL_DRAWN_WITHIN_MS` of it once the chunk has run.
test("a landing on an issue tab draws the shell and the tab strip while the tab's chunk is out, and the panel as soon as it lands", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Held plan tab" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const plan = await holdAssetUntilReleased(page, chunkFile("PlanTab"));
    // `install()` leaves the clock running at real speed, and `pauseAt` stops it, a second
    // ahead of the run's clock, since the page's clock refuses to move backwards and the two can
    // differ a little. It is paused before the load, so no page timer is armed for it to fire.
    await page.clock.install();
    await page.clock.pauseAt(Date.now() + 1_000);
    await page.goto(`/issues/${issue.key}/plan`);

    // A paused page draws no answer of its own accord (TanStack delivers each query's on a
    // zero-delay page timer), so the wait spends the clock a frame at a time until the page has
    // drawn the shell, the tab strip and the plan tab's skeleton.
    const panel = page.locator("#issue-plan-panel");
    const skeleton = panel.getByRole("status", { name: "Loading the plan tab" });
    const tabs = page.getByRole("tablist", { name: "Issue detail" });
    await expect
      .poll(
        async () => {
          await page.clock.runFor(FRAME_MS);
          return [
            await page.getByTestId("app-shell").count(),
            await tabs.count(),
            await skeleton.count(),
          ];
        },
        { intervals: [FRAME_MS] }
      )
      .toEqual([1, 1, 1]);

    plan.release();
    // An import of the chunk's own address settles once its module has run, and with it the
    // panel's download, which is the same module. Read from the hold, not the page's resource
    // timing (`ranChunk`), which the page's installed clock leaves empty.
    await page.evaluate((url) => import(url).then(() => undefined), await plan.url);
    let spent = 0;
    await expect
      .poll(
        async () => {
          // A throw ends the poll at once, so the row fails on the bound and not on the expect
          // timeout after it.
          if (spent >= PANEL_DRAWN_WITHIN_MS) {
            throw new Error(
              `the plan tab's panel did not draw in ${spent} ms of the page's clock after its chunk had run`
            );
          }
          await page.clock.runFor(FRAME_MS);
          spent += FRAME_MS;
          return panel.getByTestId("plan-empty").count();
        },
        { intervals: [FRAME_MS] }
      )
      .toBe(1);
    await expect(skeleton).toHaveCount(0);
    await expect(tabs).toBeVisible();
  } finally {
    await context.close();
  }
});
