import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";
import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  deleteRepoProject,
  getIssue,
  getIssueEvents,
  mintAgentToken,
  patchIssue,
  putRepoProject,
  requestApproval,
} from "./api";
import { recordClipboard } from "./clipboard";
import { needsYouCards } from "./editor";
import { nextFrames } from "./frames";
import { keptWrites } from "./keyed-writes";
import { clearIssueCreator, resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { type Browser, expect, type Locator, type Page, test } from "./test";
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
    const subscribers = page.waitForResponse(
      (response) => new URL(response.url()).pathname === `/api/v1/issues/${issue.key}/subscribers`
    );
    await page.goto(`/issues/${issue.key}`);
    expect((await subscribers).status()).toBe(200);
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
    await expect(sessionHeader).toContainText(/Opened by:\s*chief of staff/);
    const sessionScreenshot = testInfo.outputPath("issue-header-session-creator.png");
    await sessionHeader.screenshot({ path: sessionScreenshot });
    await testInfo.attach("session-authored issue header", {
      contentType: "image/png",
      path: sessionScreenshot,
    });

    await page.goto(`/issues/${humanIssue.key}`);
    const humanHeader = page.getByTestId("issue-header");
    await expect(humanHeader).toContainText(/Opened by:\s*alice/);
    const humanScreenshot = testInfo.outputPath("issue-header-human-creator.png");
    await humanHeader.screenshot({ path: humanScreenshot });
    await testInfo.attach("human-authored issue header", {
      contentType: "image/png",
      path: humanScreenshot,
    });

    await page.goto(`/issues/${historicalIssue.key}`);
    const historicalHeader = page.getByTestId("issue-header");
    await expect(historicalHeader).toBeVisible();
    await expect(historicalHeader.getByText("Opened by:", { exact: true })).toHaveCount(0);
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
    const routeLabel = page.getByText("Message route:", { exact: true });
    await routeLabel.hover();
    // The screenshot shows the hint as a fixed box under its label. The box sits over the header's
    // controls, so the hint gets its own style back before anything below is clicked.
    const hintStyle = await routeLabel.evaluate((label) => {
      const hintId = label.getAttribute("aria-describedby");
      const hint = hintId === null ? null : document.getElementById(hintId);
      if (hint === null) {
        throw new Error("Message route hint is missing");
      }
      const style = hint.getAttribute("style");
      const labelBox = label.getBoundingClientRect();
      Object.assign(hint.style, {
        background: "Canvas",
        border: "1px solid CanvasText",
        borderRadius: "0.375rem",
        boxShadow: "0 4px 6px rgb(0 0 0 / 0.15)",
        clip: "auto",
        clipPath: "none",
        color: "CanvasText",
        height: "auto",
        left: `${Math.min(labelBox.left, window.innerWidth - 304)}px`,
        maxWidth: "calc(100vw - 16px)",
        overflow: "visible",
        padding: "0.5rem",
        position: "fixed",
        top: `${labelBox.bottom + 8}px`,
        whiteSpace: "normal",
        width: "18rem",
        zIndex: "50",
      });
      return style;
    });
    await page.screenshot({
      path: testInfo.outputPath("issue-header-1280.png"),
    });
    await routeLabel.evaluate((label, style) => {
      const hint = document.getElementById(label.getAttribute("aria-describedby") ?? "");
      if (hint === null) {
        throw new Error("Message route hint is missing");
      }
      if (style === null) {
        hint.removeAttribute("style");
      } else {
        hint.setAttribute("style", style);
      }
    }, hintStyle);
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
    // No component attached anywhere on the chain: the rail's read-only line says so.
    await expect(page.getByTestId("issue-components")).toContainText("Components:Not attached");
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
        await expect(moverPage.getByRole("link", { name: oldParent.key })).toBeVisible();
      })(),
    ]);
    await moverPage.getByRole("button", { name: "Edit parent issue" }).click();
    // Exactly, because the rail item behind the open panel is still on the page and its own
    // name holds this word: `getByLabel` without it is a case-insensitive substring match.
    await moverPage.getByLabel("Parent", { exact: true }).fill(newParent.key);
    await moverPage.getByRole("button", { name: "Save parent" }).click();
    await expect(moverPage.getByRole("link", { name: newParent.key })).toBeVisible();

    // The new parent's open page hears child.added on the live event feed and gains the row
    // without a reload.
    const movedRow = parentPage
      .getByRole("tabpanel")
      .getByRole("link", { name: `${mover.key} · Moving decision` });
    await expect(movedRow).toBeVisible();

    await moverPage.getByRole("button", { name: "Edit parent issue" }).click();
    await moverPage.getByLabel("Parent", { exact: true }).fill("");
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

test("issue header keeps every control in a shared row on a phone and the whose-turn indicator in view at every width @phone", async ({
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
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    const indicator = page.getByTestId("issue-whose-turn");
    const openedBy = page.getByText("Opened by:", { exact: true });
    const subscribers = page.getByRole("button", { name: "Subscribers: 1" });
    const link = page.getByRole("link", { name: "#1051 sjawhar/legion" });
    await expect(indicator).toHaveText("Waiting on you (1)");
    await expect(subscribers).toBeVisible();
    await expect(link).toBeVisible();
    await expect(
      page.getByRole("link", { name: "https://github.com/sjawhar/legion/pull/1051" })
    ).toHaveCount(0);

    // Close shares the state row; creator, labels, subscribers, and the GitHub link share the
    // details line with the indicator — nothing but the title sits on a row of its own.
    const stateRow = await top(page.getByLabel("Status"));
    expect(await top(page.getByLabel("Priority"))).toBe(stateRow);
    expect(await top(page.getByRole("button", { name: "Close issue" }))).toBe(stateRow);
    const rail = page.getByTestId("issue-metadata-rail");
    const detailsRow = await top(rail);
    expect(detailsRow).toBeGreaterThan(stateRow);
    const detailsRowMiddle = await middle(page.getByTestId("issue-labels"));
    expect(await middle(openedBy)).toBe(detailsRowMiddle);
    expect(await middle(subscribers)).toBe(detailsRowMiddle);
    expect(await middle(link)).toBe(detailsRowMiddle);
    expect(await inView(indicator, 390)).toBe(true);
    const phoneHeader = await header.boundingBox();
    expect(phoneHeader?.height ?? Number.POSITIVE_INFINITY).toBeLessThan(300);
    await expect(
      page.evaluate(
        () => document.documentElement.scrollWidth === document.documentElement.clientWidth
      )
    ).resolves.toBe(true);
    const phoneShot = testInfo.outputPath("issue-header-390.png");
    await page.screenshot({ path: phoneShot });
    await testInfo.attach("issue header (390px)", { contentType: "image/png", path: phoneShot });

    await page.setViewportSize({ height: 900, width: 1280 });
    // The shell swaps its phone bar for the sidebar, and the margin its sheet for the column, in
    // a React commit after the media query's change event. Until then the phone DOM is laid out
    // under the desktop CSS, which squeezes <main> to its padding at the right edge.
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible();
    await expect(indicator).toHaveText("Waiting on you (1)");
    expect(await inView(indicator, 1280)).toBe(true);
    const desktopHeader = await header.boundingBox();
    expect(desktopHeader?.height ?? Number.POSITIVE_INFINITY).toBeLessThanOrEqual(140);

    await page.setViewportSize({ height: 900, width: 1536 });
    await expect(indicator).toHaveText("Waiting on you (1)");
    expect(await inView(indicator, 1536)).toBe(true);
    const wideDetailsRowMiddle = await middle(page.getByTestId("issue-labels"));
    for (const item of [
      indicator,
      openedBy,
      page.getByRole("button", { name: "Messages default to no route" }),
      subscribers,
      link,
    ]) {
      expect(await middle(item)).toBe(wideDetailsRowMiddle);
    }
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

test("issue header gives the title the row's free space beside a short details line", async ({
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
      return {
        contentWidth:
          header.clientWidth -
          Number.parseFloat(style.paddingLeft) -
          Number.parseFloat(style.paddingRight),
        width: heading.getBoundingClientRect().width,
      };
    });
  try {
    // One cold load for the whole row: the header's layout is CSS plus a `ResizeObserver`
    // (`IssueHeader.tsx`), so a resize re-runs every decision a fresh load would make, and the
    // writes below reach the open page on its event feed - whose refresh its own clock fires
    // rather than the wall clock.
    await page.clock.install();
    await page.setViewportSize({ height: 800, width: 1024 });
    await page.goto(`/issues/${issue.key}`);
    await expect(title).toHaveText("Header keeps its title readable");

    // Below 1280 the details line shares the row with the title once it fits beside the state
    // controls; the title must still take the rest of the row rather than split it with the line.
    for (const width of [1024, 1200, 1279]) {
      await page.setViewportSize({ height: 800, width });
      await nextFrames(page);
      const measured = await measureTitle();
      expect(
        measured.width,
        `title ${measured.width}px of a ${measured.contentWidth}px card at ${width}px`
      ).toBeGreaterThanOrEqual(measured.contentWidth / 2);
      if (width === 1024) {
        const shot = testInfo.outputPath("issue-header-title-1024.png");
        await page.screenshot({ path: shot });
        await testInfo.attach("issue header title (1024px)", {
          contentType: "image/png",
          path: shot,
        });
      }
    }

    // A busy details line (whose-turn badge, one label, Route, Subscribers) that fits beside a
    // 12rem title but not beside the whole title: just below 1280 the title must keep at least
    // half the card rather than share the row and drop to its minimum. The line without the
    // GitHub link is the one that could squeeze (with the link it is too wide to share).
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
    await expect(page.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    const githubLink = page.locator(
      '[data-testid=issue-header] a[href="https://github.com/sjawhar/legion/pull/1051"]'
    );
    for (const withLink of [false, true]) {
      await patchIssue(issue.key, {
        external_links: withLink
          ? [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }]
          : [],
      });
      // The link's own state is what the measurement below depends on, so it is waited for.
      await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
      await expect(githubLink).toHaveCount(withLink ? 1 : 0);
      for (const width of [1265, 1279]) {
        await page.setViewportSize({ height: 800, width });
        await nextFrames(page);
        const measured = await measureTitle();
        const label = `${width}px ${withLink ? "with" : "without"} the GitHub link`;
        expect(
          measured.width,
          `title ${measured.width}px of a ${measured.contentWidth}px card at ${label}`
        ).toBeGreaterThanOrEqual(measured.contentWidth / 2);
      }
    }
  } finally {
    await context.close();
  }
});

// The widths dispatch://INFRA-299/spec#b-r1 names, and what each of them is: 1024 and 1279 px
// sit below `xl`, where the shell gives the page the whole window; at 1280 px the sidebar and
// the margin column take 704 px of it, so a title wraps where it did not; and from 1536 px
// (`2xl`) the header's title slot is content-sized (`2xl:basis-auto` in `IssueHeader.tsx`), so
// the slot's width is the width of whichever of the heading and the editor is in it.
const titleEditWidths = [1024, 1279, 1280, 1536, 1920] as const;

// The title's line box: `leading-7` on both the heading and the editor (`IssueHeader.tsx`). A
// row that needs a wrapped heading reads the real box against this rather than trusting a
// string to be long enough for the width it runs at.
const titleLineBox = 28;

/** The title dispatch://INFRA-299/spec#b-r1 names for the five widths above, 75 characters: one
 *  line of the heading below 1280 px, and more than one from there. Every row that presses a
 *  control out of an open editor at all five widths reads this same header. */
const pressedTitle = "Deliver the first press after a title edit to the control under the pointer";

/** The box one control of the header occupies, under the name that control is reached by. */
interface HeaderControl {
  height: number;
  name: string;
  width: number;
  x: number;
  y: number;
}

/** Opens the title editor the way a person does, by clicking the heading. */
async function openTitleEditor(page: Page): Promise<void> {
  await page.getByTestId("issue-header").getByRole("heading", { level: 1 }).click();
  await expect(page.getByLabel("Issue title")).toBeFocused();
}

/**
 * A real click is mousedown then mouseup: the mousedown blurs the title editor, and the header
 * must not re-lay out on that blur, or the mouseup lands on a different element and the browser
 * delivers no click to the control the person pressed. `click()` is never used for a press that
 * ends a title edit, because it finds the control again after the blur and hides the fault. The
 * raw sequence skips `click()`'s actionability wait, so the control is waited for here: the pin
 * and Close stay `disabled` while their previous write is in flight, and a browser drops a
 * click on a disabled button.
 */
async function pressUnderPointer(page: Page, control: Locator): Promise<void> {
  await expect(control).toBeEnabled();
  const box = await control.boundingBox();
  if (box === null) {
    throw new Error("the control must be visible while the title is being edited");
  }
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.up();
}

/** One in-place editor of the issue header, as the rows below work it: the words they name it
 *  by, the control it is opened from, the field it holds while it is open, and how a person
 *  opens it. The title's edit view covers its heading in the heading's own cell; the route's
 *  and the parent's open over their rail item in an `AnchoredPanel`, which costs the row no
 *  layout at all (`web/src/components/InPlaceEditBox.tsx`). Either way the header must not
 *  re-lay out between the mousedown that ends the edit and the mouseup. */
interface HeaderEditor {
  /** What the rows call this editor, in the titles they carry and the messages they fail with:
   *  `a route edit`. */
  edit: string;
  /** The control a person opens it from, which a panel editor also hangs off. */
  anchor: (page: Page) => Locator;
  /** The editor's own field, which is on the page only while it is open. */
  field: (page: Page) => Locator;
  /** Opens it the way a person does, and returns once that field holds the focus. */
  open: (page: Page) => Promise<void>;
}

const titleEditor: HeaderEditor = {
  anchor: (page) => page.getByTestId("issue-header").getByRole("heading", { level: 1 }),
  edit: "a title edit",
  field: (page) => page.getByLabel("Issue title"),
  open: openTitleEditor,
};

/** The route's and the parent's editors, which are the same editor in two places: a rail item
 *  pressed to open a panel over it, and a field in that panel. `panelAnchor` is the rail item's
 *  name on the issue these rows create — routed nowhere, and with no parent — and `panelField`
 *  is the field's own name, matched exactly, because `getByLabel` matches a substring of any
 *  label on the page and the rail item stays on the page while its panel is open. */
function panelEditor(edit: string, panelAnchor: string, panelField: string): HeaderEditor {
  const anchor = (page: Page) => page.getByRole("button", { name: panelAnchor });
  const field = (page: Page) => page.getByLabel(panelField, { exact: true });
  return {
    anchor,
    edit,
    field,
    open: async (page) => {
      await anchor(page).click();
      // `AnchoredPanel` takes the focus into the panel as it opens, so the editor is open and
      // has something for the press to end once its field holds the focus.
      await expect(field(page)).toBeFocused();
    },
  };
}

const routeEditor = panelEditor("a route edit", "Messages default to no route", "Message route");

const parentEditor = panelEditor("a parent edit", "None, set parent issue", "Parent");

/** Leaves an open editor the way Escape does — the title's field discards its draft, and
 *  `AnchoredPanel` dismisses a panel editor and hands the focus back to the item it hangs off —
 *  and returns once that field has gone. */
async function dismissEditor(page: Page, editor: HeaderEditor): Promise<void> {
  const field = editor.field(page);
  await field.press("Escape");
  await expect(field).toHaveCount(0);
}

/** Opens `editor` and presses `control` straight out of it, with nothing in between. */
async function pressWhileEditing(
  page: Page,
  editor: HeaderEditor,
  control: Locator
): Promise<void> {
  await editor.open(page);
  await pressUnderPointer(page, control);
}

/** The heading's own box, read rather than guessed: how many line boxes of its own line height
 *  it is tall, with that line height beside it, so a row that needs a wrapped heading fails
 *  where its fixture stopped wrapping instead of measuring a one-line case a second time. */
function headingLineBoxes(page: Page): Promise<{ lineHeight: number; lines: number }> {
  return page.evaluate(() => {
    const heading = document.querySelector("[data-testid=issue-header] h1");
    if (heading === null) {
      throw new Error("the issue header holds no h1 while its title shows");
    }
    const style = getComputedStyle(heading);
    const text =
      heading.getBoundingClientRect().height -
      Number.parseFloat(style.paddingTop) -
      Number.parseFloat(style.paddingBottom) -
      Number.parseFloat(style.borderTopWidth) -
      Number.parseFloat(style.borderBottomWidth);
    const lineHeight = Number.parseFloat(style.lineHeight);
    return { lineHeight, lines: Math.round(text / lineHeight) };
  });
}

/** Every control of the header by the name it is reached by, with the box it occupies: what the
 *  invariant of dispatch://INFRA-299/spec#b-r1 compares between the heading and the editor, and
 *  what carries that requirement to the controls no row presses. The title element is the one
 *  that changes and is left out; a control's name is its `aria-label`, its `title` or its own
 *  text, which is what the rows of this file locate these controls by. */
function headerControls(page: Page): Promise<HeaderControl[]> {
  return page.evaluate(() => {
    const header = document.querySelector("[data-testid=issue-header]");
    if (header === null) {
      throw new Error("the issue header is not on the page");
    }
    // The details rail scrolls sideways (`overflow-x: auto`), and a pick made in one of its own
    // controls scrolls that control into view, which moves every item of the rail by the same
    // number of pixels. That is the rail scrolling and not a control changing its box, which is
    // what dispatch://INFRA-299/spec#b-r1 and #b-r5 are about, so every reading is taken with
    // the rail at its start. Setting `scrollLeft` lays the page out again at once, so the boxes
    // below are read after the move.
    const rail = header.querySelector("[data-testid=issue-metadata-rail]");
    if (rail !== null) {
      rail.scrollLeft = 0;
    }
    return [...header.querySelectorAll<HTMLElement>("a[href], button, input, select")]
      .filter((control) => control.getAttribute("aria-label") !== "Issue title")
      .map((control) => {
        const box = control.getBoundingClientRect();
        return {
          height: Math.round(box.height),
          name:
            control.getAttribute("aria-label") ??
            control.getAttribute("title") ??
            (control.textContent ?? "").trim(),
          width: Math.round(box.width),
          x: Math.round(box.x),
          y: Math.round(box.y),
        };
      });
  });
}

/** The names an editor's own Save and Cancel carry. Each is on the page in one state only, so
 *  the comparison of the two states drops them by name: dropping the controls an open editor
 *  adds by their position would hide a control that the same editor took out of the header.
 *  A panel editor's form is portalled out of the header (`AnchoredPanel`) and the title's field
 *  is dropped by `headerControls` above, so no reading holds one today; the filter is what
 *  keeps the comparison right for an editor whose Save and Cancel are drawn in the header. */
const editorOwnControls = ["Cancel", "Save parent", "Save route"];

/** The header's controls that both states draw, out of a reading of them all. */
function controlsInBothStates(controls: HeaderControl[]): HeaderControl[] {
  return controls.filter((control) => !editorOwnControls.includes(control.name));
}

/** The pin, the unpin and then Close issue, each pressed straight out of `editor`, which is
 *  opened again before every press, and the issue left unpinned and open again for the next
 *  width. `wrapped` is read before every press: a fixture that stopped wrapping would otherwise
 *  press a one-line heading and pass while measuring nothing. The measurement is a floor rather
 *  than an exact count, because what dispatch://INFRA-299/spec#b-r2 asks of the fixture is that
 *  the heading wraps past the one line the editor is, and how many lines a string takes is the
 *  font's business and the width's. */
async function pinAndCloseFromEdit(
  page: Page,
  editor: HeaderEditor,
  issueKey: string,
  width: number,
  wrapped: boolean
): Promise<void> {
  const readHeading = async () => {
    if (wrapped) {
      const heading = await headingLineBoxes(page);
      expect(heading.lineHeight, `the heading's line box at ${width}px`).toBe(titleLineBox);
      expect(heading.lines, `the heading at ${width}px must wrap past one line`).toBeGreaterThan(1);
    }
  };
  const pin = page.getByRole("button", { name: "Pin issue" });
  const unpin = page.getByRole("button", { name: "Unpin issue" });
  await readHeading();
  await pressWhileEditing(page, editor, pin);
  await expect(unpin, `the pin pressed out of ${editor.edit} at ${width}px`).toHaveAttribute(
    "aria-pressed",
    "true"
  );
  await readHeading();
  await pressWhileEditing(page, editor, unpin);
  await expect(pin, `the unpin pressed out of ${editor.edit} at ${width}px`).toHaveAttribute(
    "aria-pressed",
    "false"
  );
  await readHeading();
  await pressWhileEditing(page, editor, page.getByRole("button", { name: "Close issue" }));
  await expect(
    page.getByRole("button", { name: "Reopen issue" }),
    `Close issue pressed out of ${editor.edit} at ${width}px`
  ).toBeVisible();
  await expect.poll(() => getIssue(issueKey)).toMatchObject({ status: "done" });
  await patchIssue(issueKey, { status: "triage" });
}

// SLOW(>=2s): five widths, each a load of the issue and three presses out of an open title
// editor, with the issue closed and opened again between them.
test("issue header delivers the click that ends a title edit to the control under the pointer", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: pressedTitle });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const width of titleEditWidths) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      await expect(
        page.getByTestId("issue-header").getByRole("heading", { level: 1, name: pressedTitle })
      ).toBeVisible();
      await pinAndCloseFromEdit(page, titleEditor, issue.key, width, false);
      if (width === 1024 || width === 1536) {
        const shot = testInfo.outputPath(`issue-header-title-edit-click-${width}.png`);
        await page.screenshot({ path: shot });
        await testInfo.attach(`close clicked straight out of a title edit (${width}px)`, {
          contentType: "image/png",
          path: shot,
        });
      }
    }
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the same five widths on a reviewed issue, whose header carries the approval
// chip's two actions and the GitHub link beside everything the row above presses.
test("issue header delivers the click that ends a title edit beside the approval buttons and a GitHub link", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // 41 characters beside the busiest state row the header draws: the primary document's
  // approval chip with its Approve and Request changes actions, and the pull request the issue
  // links, which is the second fixture of dispatch://INFRA-299/spec#b-r1.
  const title = "Approve the header's press at every width";
  const issue = await createIssue({ project: "CORE", spec: "The approach.", title });
  await requestApproval(issue.primary_artifact_id, {}, session);
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }],
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const width of titleEditWidths) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      const header = page.getByTestId("issue-header");
      await expect(header.getByRole("heading", { level: 1, name: title })).toBeVisible();
      await expect(header.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
      await expect(header.getByRole("button", { name: "Request changes" })).toBeVisible();
      await expect(header.getByRole("link", { name: "#1051 sjawhar/legion" })).toBeVisible();
      await pinAndCloseFromEdit(page, titleEditor, issue.key, width, false);
    }
  } finally {
    await context.close();
  }
});

test("issue header delivers the click that ends a title edit where the heading wraps past one line", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // One fixture per width of dispatch://INFRA-299/spec#b-r2, each long enough to wrap there.
  // Measured in this pod at 7974f2dd: the 55-character title below is three line boxes of the
  // heading at 1280 px, and the 201-character one is more than one at 1920 px. Each press below
  // reads the heading's own box first, so a fixture that stopped wrapping fails the row rather
  // than measuring a one-line case a second time.
  const laptopTitle = "A title that wraps to two lines on a laptop screen here";
  const wideTitle =
    "The header title that a reader keeps editing until it is long enough to wrap past the first line of a nineteen-twenty screen, where the sidebar and the review margin leave the title about a thousand px";
  const laptop = await createIssue({ project: "CORE", title: laptopTitle });
  const wide = await createIssue({ project: "CORE", title: wideTitle });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const wrapped of [
      { issue: laptop, title: laptopTitle, width: 1280 },
      { issue: wide, title: wideTitle, width: 1920 },
    ]) {
      await page.setViewportSize({ height: 800, width: wrapped.width });
      await page.goto(`/issues/${wrapped.issue.key}`);
      await expect(
        page.getByTestId("issue-header").getByRole("heading", { level: 1, name: wrapped.title })
      ).toBeVisible();
      await nextFrames(page);
      await pinAndCloseFromEdit(page, titleEditor, wrapped.issue.key, wrapped.width, true);
    }
  } finally {
    await context.close();
  }
});

test("issue header delivers the click that ends a title edit into a draft that has grown past one line", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Short title" });
  // 98 characters typed over an 11-character title: the heading that returns at the blur is
  // this draft, because the header renders `drafts.title`, so a box sized to the stored title
  // alone still changes size under the press and still loses it.
  const grown =
    "Typed while the editor is open: a draft long enough to wrap the heading of a 1536 px screen in two";

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 800, width: 1536 });
    await page.goto(`/issues/${issue.key}`);
    const heading = page.getByTestId("issue-header").getByRole("heading", { level: 1 });
    await expect(heading).toHaveText("Short title");
    await openTitleEditor(page);
    await page.getByLabel("Issue title").fill(grown);
    await nextFrames(page);

    await pressUnderPointer(page, page.getByRole("button", { name: "Pin issue" }));
    await expect(
      page.getByRole("button", { name: "Unpin issue" }),
      "the pin pressed out of a title edit whose draft grew past one line"
    ).toHaveAttribute("aria-pressed", "true");
    await expect(heading).toHaveText(grown);
    const returned = await headingLineBoxes(page);
    expect(returned.lineHeight).toBe(titleLineBox);
    expect(
      returned.lines,
      "the draft must wrap at 1536 px or this row measures no grown box"
    ).toBeGreaterThan(1);
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ title: grown });
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): five widths, each a load of the issue and three presses out of an open route
// editor, with the issue closed and opened again between them.
test("issue header delivers the press that ends a route edit to the control under the pointer", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: pressedTitle });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const width of titleEditWidths) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      await expect(
        page.getByTestId("issue-header").getByRole("heading", { level: 1, name: pressedTitle })
      ).toBeVisible();
      // The route's editor is a panel over its rail item, and the mousedown of the press below
      // is what dismisses it (`AnchoredPanel`): the rail item must not move as it goes, or the
      // mouseup lands elsewhere and the browser delivers no click at all.
      await pinAndCloseFromEdit(page, routeEditor, issue.key, width, false);
    }
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): five widths, each a load of the issue and three presses out of an open parent
// editor, with the issue closed and opened again between them.
test("issue header delivers the press that ends a parent edit to the control under the pointer", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: pressedTitle });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const width of titleEditWidths) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      await expect(
        page.getByTestId("issue-header").getByRole("heading", { level: 1, name: pressedTitle })
      ).toBeVisible();
      await pinAndCloseFromEdit(page, parentEditor, issue.key, width, false);
    }
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): five widths, each opening the header's three editors in turn and reading every
// control of the header twice.
test("issue header moves nothing between the heading and the editor, the invariant behind the press that ends a title edit", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // The crowded header, so the invariant is read over every control the page draws rather than
  // over the two this file presses: the approval chip with its actions, the GitHub link, the
  // labels, a subscriber and the whose-turn indicator.
  const issue = await createIssue({ project: "CORE", spec: "The approach.", title: pressedTitle });
  await requestApproval(issue.primary_artifact_id, {}, session);
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }],
    labels: ["Frontend", "api"],
    priority: 1,
  });
  await createAsk(issue.key, { question: "Which layout?" }, session);
  await subscribeSession(issue.key);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 800, width: 1024 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    // Everything the header reads for asynchronously is on the page before the first
    // measurement: an arrival between the two would move a control for a reason of its own.
    await expect(header.getByRole("heading", { level: 1, name: pressedTitle })).toBeVisible();
    await expect(header.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await expect(header.getByRole("link", { name: "#1051 sjawhar/legion" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    // Two asks wait on the reader here: the approval request above, and the question below it.
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (2)");

    // A pass of its own, which presses nothing: pressing the pin renames the control from
    // `Pin issue` to `Unpin issue` and Close issue adds the closed callout, so a pass that
    // presses cannot compare like with like.
    for (const width of titleEditWidths) {
      await page.setViewportSize({ height: 800, width });
      await nextFrames(page);
      const names = (await headerControls(page)).map((control) => control.name);
      expect(names, `the header's controls at ${width}px`).toContain("Pin issue");
      expect(names, `the header's controls at ${width}px`).toContain("Close issue");
      for (const editor of [titleEditor, routeEditor, parentEditor]) {
        // The details rail scrolls sideways (`overflow-x-auto` in `IssueHeader.tsx`), so the
        // press that opens a rail item's editor would scroll that item into view and move every
        // item beside it, after the reading this compares against. The scroll is taken first,
        // and the reading after it.
        await editor.anchor(page).scrollIntoViewIfNeeded();
        await nextFrames(page);
        const showing = controlsInBothStates(await headerControls(page));
        await editor.open(page);
        await nextFrames(page);
        // Soft, so one width that moves a control does not hide the widths and the editors
        // after it: the row still fails, and its report names every one the header moves under.
        expect
          .soft(
            controlsInBothStates(await headerControls(page)),
            `the header's controls while ${editor.edit} is open at ${width}px`
          )
          .toEqual(showing);
        await dismissEditor(page, editor);
      }
    }
  } finally {
    await context.close();
  }
});

/** The login the two rows below give the assignee: longer than the fourteen characters its
 *  badge reserves (dispatch://INFRA-299/spec#b-r5), so the badge has to cut it. */
const LONG_LOGIN = "octavia-the-architect";

/** A repository name longer than that reservation too, and the one the project defaults to. */
const LONG_REPO = "cybermonklabs/legion-infrastructure";

/** Signs `login` in, so `GET /users` lists them and the assignee picker offers them, and
 *  leaves no context of theirs open: these rows act as alice throughout. */
async function signInApart(browser: Browser, login: string): Promise<void> {
  const theirs = await asUser(browser, login);
  await theirs.close();
}

/** One control of the header by name, out of a reading of them all. */
function controlNamed(controls: HeaderControl[], name: string): HeaderControl {
  const found = controls.find((control) => control.name === name);
  if (found === undefined) {
    throw new Error(`the header draws no control named ${name}`);
  }
  return found;
}

// SLOW(>=2s): one issue, four picks, and the box of every control of the header read on either
// side of each of them.
test("a value a reader picks moves no control beside it in the header", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  // The repository the project defaults to and a far longer one to pick: a badge sized by its
  // value would grow by the difference and take the rail's controls with it.
  await putRepoProject("acme/widgets", { default: true, project: "CORE" });
  await putRepoProject(LONG_REPO, { project: "CORE" });
  await signInApart(browser, LONG_LOGIN);
  const issue = await createIssue({ project: "CORE", title: "Pick a value, move nothing" });
  // A label of the project's own, which the popover offers this issue.
  const other = await createIssue({ project: "CORE", title: "Another issue of this project" });
  await patchIssue(other.key, { labels: ["alpha"] });
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }],
  });
  await subscribeSession(issue.key);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    // The visible badge of a picker; the select's own description repeats its text elsewhere.
    const badge = (text: RegExp) => header.locator('span[aria-hidden="true"]', { hasText: text });
    const priority = page.getByLabel(`Priority of ${issue.key}`);
    const assignee = page.getByLabel(`Assignee of ${issue.key}`);
    const repo = page.getByLabel(`Repository of ${issue.key}`);
    const labels = page.getByRole("button", { name: "Edit labels" });
    // Everything the header reads for is on the page before the first measurement: an arrival
    // between two of them would move a control for a reason of its own.
    await expect(header.getByRole("heading", { level: 1 })).toHaveText(
      "Pick a value, move nothing"
    );
    await expect(page.getByRole("link", { name: "#1051 sjawhar/legion" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(badge(/^acme\/widgets$/)).toBeVisible();
    await nextFrames(page);

    // The priority: `Priority`, the tag it wears while unset, is its widest value and `P0` its
    // narrowest, so an unreserved badge changes width on this one pick.
    let before = await headerControls(page);
    await priority.selectOption("0");
    await expect(badge(/^P0$/)).toBeVisible();
    await nextFrames(page);
    expect(await headerControls(page), "the header after a priority pick").toEqual(before);

    // The assignee: `Unassigned` gives way to a login half as long again.
    before = await headerControls(page);
    await assignee.focus();
    await expect(assignee.locator(`option[value="${LONG_LOGIN}"]`)).toHaveCount(1);
    await assignee.selectOption(LONG_LOGIN);
    await expect(badge(new RegExp(`^${LONG_LOGIN}$`))).toBeVisible();
    await nextFrames(page);
    expect(await headerControls(page), "the header after an assignee pick").toEqual(before);

    // The repository: the project's default gives way to one three times its length.
    before = await headerControls(page);
    await repo.focus();
    await expect(repo.locator(`option[value="${LONG_REPO}"]`)).toHaveCount(1);
    await repo.selectOption(LONG_REPO);
    await expect(badge(new RegExp(`^${LONG_REPO.replace("/", "\\/")}$`))).toBeVisible();
    await nextFrames(page);
    expect(await headerControls(page), "the header after a repository pick").toEqual(before);

    // The labels are drawn as a chip apiece beside their trigger, so the label the reader adds
    // lengthens the rail by that chip: a chip is the value itself rather than a control, and no
    // reserved width can make a list of them one width. What R5 reserves here is the trigger,
    // whose three words - `Labels`, `+` and `Saving labels…` - must draw one box. So the row
    // holds the save the pick starts, where the trigger says it is saving and the chip has not
    // arrived, against the whole header as it stood before the pick; and then reads the
    // trigger's own box again with the chip beside it.
    const held = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      const request = route.request();
      const body = request.method() === "PATCH" ? request.postDataJSON() : null;
      if (body === null || typeof body !== "object" || !("labels" in body)) {
        await route.continue();
        return;
      }
      await held.promise;
      await route.continue();
    });
    before = await headerControls(page);
    await expect(labels).toHaveText("Labels");
    await labels.click();
    await page.getByRole("option", { exact: true, name: "alpha" }).click();
    // The trigger closes the popover, and the close is what commits the draft and saves it.
    await labels.click();
    await expect(labels).toHaveText("Saving labels…");
    await nextFrames(page);
    expect(await headerControls(page), "the header while the labels save").toEqual(before);

    held.resolve();
    await expect(header.getByText("alpha", { exact: true })).toBeVisible();
    await expect(labels).toHaveText("+");
    await nextFrames(page);
    const settled = controlNamed(await headerControls(page), "Edit labels");
    const started = controlNamed(before, "Edit labels");
    expect(
      { height: settled.height, width: settled.width },
      "the labels trigger with its chip beside it"
    ).toEqual({ height: started.height, width: started.width });
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ labels: ["alpha"] });
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): one issue read at two widths, with every reserved-width control at its widest.
test("issue header keeps its rows and heights with every reserved-width control at its widest", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await putRepoProject(LONG_REPO, { default: true, project: "CORE" });
  await signInApart(browser, LONG_LOGIN);
  // The title is short on purpose: what this row measures is the width the reserved controls
  // take, not the width a title takes beside them.
  const title = "Widest values";
  const issue = await createIssue({ project: "CORE", title });
  await patchIssue(issue.key, {
    assignee: LONG_LOGIN,
    labels: ["frontend", "needs-design", "dispatch"],
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 900, width: 1536 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    const badge = (text: RegExp) => header.locator('span[aria-hidden="true"]', { hasText: text });
    await expect(header.getByRole("heading", { level: 1 })).toHaveText(title);
    // Each control at its widest: the priority unset, which is the widest of its closed set,
    // the long login, the long repository, and labels enough to fill the rail.
    await expect(badge(/^Priority$/)).toBeVisible();
    await expect(badge(new RegExp(`^${LONG_LOGIN}$`))).toBeVisible();
    await expect(badge(new RegExp(`^${LONG_REPO.replace("/", "\\/")}$`))).toBeVisible();
    await expect(header.getByText("needs-design", { exact: true })).toBeVisible();
    await nextFrames(page);

    // R3 at 1536 px: every item of the details rail on one row.
    const railRows = await page.evaluate(() => {
      const rail = document.querySelector("[data-testid=issue-metadata-rail]");
      if (rail === null) {
        throw new Error("the issue header draws no details rail");
      }
      return [
        ...new Set(
          [...rail.children].map((item) => {
            const box = item.getBoundingClientRect();
            return Math.round(box.y + box.height / 2);
          })
        ),
      ];
    });
    expect(railRows, "the rows the details rail takes at 1536px").toHaveLength(1);

    // A value its box cuts keeps the whole of itself where a pointer reader can ask for it: on
    // the picker around the badge, since the select laid over the badge is what a pointer
    // meets. The badge's own box is what cuts it.
    for (const [name, whole] of [
      [`Assignee of ${issue.key}`, LONG_LOGIN],
      [`Repository of ${issue.key}`, `${LONG_REPO}, inherited or the project's default`],
    ] as const) {
      await expect(page.getByLabel(name).locator("xpath=..")).toHaveAttribute("title", whole);
    }
    for (const text of [
      new RegExp(`^${LONG_LOGIN}$`),
      new RegExp(`^${LONG_REPO.replace("/", "\\/")}$`),
    ]) {
      expect(
        await badge(text).evaluate((node) => node.scrollWidth > node.clientWidth),
        `${text.source} must be cut by the box its control reserves`
      ).toBe(true);
    }

    // R3 at 1770 px: the header is 96 px tall or less, reserved widths and all.
    await page.setViewportSize({ height: 900, width: 1770 });
    await nextFrames(page);
    const height = await header.evaluate((element) => element.getBoundingClientRect().height);
    const shot = testInfo.outputPath("issue-header-widest-values-1770.png");
    await page.screenshot({ path: shot });
    await testInfo.attach("issue header with every reserved control at its widest (1770px)", {
      contentType: "image/png",
      path: shot,
    });
    expect(height, "the header's height at 1770px").toBeLessThanOrEqual(96);
  } finally {
    await context.close();
  }
});

test("issue header keeps one title element in the tree and the tab order, and the Tab that ends a title edit moves to the pin", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "One title element at a time";
  const issue = await createIssue({ project: "CORE", title });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 800, width: 1536 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    const heading = header.getByRole("heading", { level: 1 });
    const editor = page.getByLabel("Issue title");

    // While the title shows, the heading is the header's one title element and the editor is
    // not on the page.
    await expect(heading).toHaveText(title);
    await expect(editor).toHaveCount(0);
    await expect(header.locator("h1")).toHaveCount(1);

    // While the edit is open it is the other way round, and the header still holds one `h1`: a
    // copy kept to hold the box's size is hidden from assistive software and is that same
    // `h1`, never a second one, because the header's own measurement reads the first `h1` it
    // finds (dispatch://INFRA-299/spec#b-r9).
    await openTitleEditor(page);
    await expect(heading).toHaveCount(0);
    await expect(editor).toHaveCount(1);
    await expect(header.locator("h1")).toHaveCount(1);

    // Tab ends the edit as a press does, and the focus lands on the control after the editor,
    // never on a copy of the title kept for the box's size, which is out of the tab order. A
    // closed issue is outside this rule: its title cannot be edited and its heading is already
    // out of the tab order (`tabIndex={isClosed ? -1 : 0}` in `IssueHeader.tsx`).
    await page.keyboard.press("Tab");
    await expect(page.getByRole("button", { name: "Pin issue" })).toBeFocused();
    await expect(heading).toHaveText(title);
    await expect(editor).toHaveCount(0);
    await expect(header.locator("h1")).toHaveCount(1);
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

test("issue header picks the issue's repository from its project's mappings, and a child resolves to it", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" });
  await putRepoProject("acme/widgets", { default: true, project: "CORE" });
  await putRepoProject("acme/gadgets", { project: "CORE" });
  await putRepoProject("acme/elsewhere", { default: true, project: "OPS" });
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
    // The visible badge; the select's description repeats its text in a span of its own.
    const badge = (text: RegExp) => header.locator('span[aria-hidden="true"]', { hasText: text });
    const control = page.getByLabel(`Repository of ${parent.key}`);
    // The badge names the project's default, which the issue inherits, and so does the select's
    // description, which is what a screen reader hears beside Inherit.
    await expect(control).toHaveValue("");
    await expect(badge(/^acme\/widgets$/)).toBeVisible();
    await expect(control).toHaveAccessibleDescription(
      "acme/widgets, inherited or the project's default"
    );
    // The project's mappings are read once the reader reaches for the control; another project's
    // are not offered.
    await control.focus();
    await expect(control.locator("option")).toHaveText(["Inherit", "acme/gadgets", "acme/widgets"]);

    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${parent.key}`
    );
    await control.selectOption("acme/gadgets");
    expect((await patch).postDataJSON()).toEqual({ repo: "acme/gadgets" });
    await expect(badge(/^acme\/gadgets$/)).toBeVisible();
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

    // Inherit clears the issue's own repository: it resolves to the project's default again.
    await control.selectOption("");
    await expect
      .poll(() => getIssue(parent.key))
      .toMatchObject({ repo: null, resolved_repo: "acme/widgets" });
    await page.reload();
    await expect(page.getByLabel(`Repository of ${parent.key}`)).toHaveValue("");
    await expect(badge(/^acme\/widgets$/)).toBeVisible();

    // A project that maps no repository has nothing to pick: once the page has read the mappings,
    // its issues' rail has no item for the repository.
    await createProject({ key: "BARE", name: "Bare" });
    const bare = await createIssue({ project: "BARE", title: "No repository mapped" });
    expect(bare.resolved_repo).toBeNull();
    const mappingsRead = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/settings/repo-projects"
    );
    await page.goto(`/issues/${bare.key}`);
    await mappingsRead;
    const rail = page.getByTestId("issue-metadata-rail");
    await expect(rail.getByText("Parent:")).toBeVisible();
    await expect(page.getByLabel(`Repository of ${bare.key}`)).toHaveCount(0);
    await expect(rail.getByText("Repository:")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("an open issue's header follows its project's default repository as the mappings change", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" });
  await putRepoProject("acme/widgets", { default: true, project: "CORE" });
  await putRepoProject("acme/gadgets", { project: "CORE" });
  const issue = await createIssue({ project: "CORE", title: "Follows the default" });
  expect(issue).toMatchObject({ repo: null, resolved_repo: "acme/widgets" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // Each write below is followed by a refresh the scheduler holds for at most
    // `MIN_REFETCH_INTERVAL_MS`; that is a page timer, so the page's own clock fires it.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    const badge = (text: RegExp) => header.locator('span[aria-hidden="true"]', { hasText: text });
    await expect(badge(/^acme\/widgets$/)).toBeVisible();

    // A new default: the page shows it without a reload.
    await putRepoProject("acme/gadgets", { default: true, project: "CORE" });
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(badge(/^acme\/gadgets$/)).toBeVisible();

    // The default removed: nothing resolves, and the project still maps acme/widgets to pick.
    await deleteRepoProject("acme/gadgets");
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(badge(/^No repository$/)).toBeVisible();
    await expect(page.getByLabel(`Repository of ${issue.key}`)).toHaveAccessibleDescription(
      "No repository"
    );

    // The default again, then moved to another project: CORE maps nothing left to pick, so the
    // rail drops the item.
    await putRepoProject("acme/widgets", { default: true, project: "CORE" });
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(badge(/^acme\/widgets$/)).toBeVisible();
    await putRepoProject("acme/widgets", { project: "OPS" });
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(page.getByLabel(`Repository of ${issue.key}`)).toHaveCount(0);
    await expect(page.getByTestId("issue-metadata-rail").getByText("Repository:")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("an issue page whose mappings read fails says so and retries it, rather than hiding the repository", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putRepoProject("acme/widgets", { project: "CORE" });
  await putRepoProject("acme/gadgets", { project: "CORE" });
  const issue = await createIssue({ project: "CORE", title: "No default to inherit" });
  expect(issue.resolved_repo).toBeNull();

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    let failing = true;
    let refused = 0;
    await page.route("**/api/v1/settings/repo-projects", (route) => {
      if (!failing) {
        return route.continue();
      }
      refused += 1;
      return route.abort();
    });
    // The read is retried twice, `QUERY_RETRY_DELAY_MS` apart (`web/src/api/query-client.ts`),
    // before the rail may say so; those waits are page timers its own clock fires.
    await page.clock.install();
    await page.goto(`/issues/${issue.key}`);
    for (let attempt = 1; attempt <= 2; attempt += 1) {
      await expect.poll(() => refused).toBeGreaterThanOrEqual(attempt);
      await page.clock.fastForward(QUERY_RETRY_DELAY_MS);
    }
    const rail = page.getByTestId("issue-metadata-rail");
    await expect(rail.getByText("Could not load the repositories mapped to CORE.")).toBeVisible();
    await expect(rail.getByText("Repository:")).toBeVisible();

    failing = false;
    await rail.getByRole("button", { name: "Retry" }).click();
    const control = page.getByLabel(`Repository of ${issue.key}`);
    await expect(control.locator("option")).toHaveText(["Inherit", "acme/gadgets", "acme/widgets"]);
    await expect(rail.getByText("Could not load the repositories mapped to CORE.")).toHaveCount(0);
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

test("the details rail fades its trailing edge only while it has somewhere to scroll @phone @phone-only", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Rail fade" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const rail = page.getByTestId("issue-metadata-rail");
    const masked = () => rail.evaluate((element) => getComputedStyle(element).maskImage !== "none");
    const overflow = () => rail.evaluate((element) => element.scrollWidth - element.clientWidth);

    // A bare issue's details fit a desktop rail: nothing to scroll to, so nothing to fade.
    await page.setViewportSize({ height: 900, width: 1920 });
    await page.goto(`/issues/${issue.key}`);
    await expect(rail).toBeVisible();
    await expect.poll(overflow).toBe(0);
    expect(await masked()).toBe(false);

    // At a phone width the same details run past the rail, and the cut lands inside a control -
    // the Labels button alone is wider than the rail is.
    await patchIssue(issue.key, {
      labels: ["frontend", "documentation", "needs-design", "observability"],
    });
    await page.setViewportSize({ height: 844, width: 390 });
    await page.reload();
    await expect(page.getByTestId("issue-labels")).toBeVisible();
    await expect.poll(overflow).toBeGreaterThan(0);
    await expect.poll(masked).toBe(true);

    // At the end of the scroll the last item is all there is; a fade there would hide it.
    await rail.evaluate((element) => {
      element.scrollLeft = element.scrollWidth;
    });
    await expect.poll(masked).toBe(false);

    // Scrolling back re-arms it.
    await rail.evaluate((element) => {
      element.scrollLeft = 0;
    });
    await expect.poll(masked).toBe(true);
  } finally {
    await context.close();
  }
});

/** The priority the issue below is seeded at, which the server keeps for as long as that row
 *  holds its saves: a value that reaches the screen from the server shows as this one, so it is
 *  never one of the reader's picks. */
const HELD_SAVE_PRIORITY = 3;

/** The three priorities that row's reader picks, in the order they are picked. */
const HELD_SAVE_PICKS = [0, 1, 2] as const;

/** The title the project's other issue is given mid-row. Every event of an issue refreshes its
 *  project's list (`api/event-refresh.ts`), so this write is what starts the list read whose
 *  answer lands while the first priority save is still held. */
const RETITLED = "Another issue, retitled while a save was out";

/** One priority save the page made, as the route handler was given it: the body it carried, and
 *  the handle that answers it. */
interface HeldPrioritySave {
  body: unknown;
  release: () => void;
}

/**
 * Holds every `PATCH /issues/<key>` the page makes, in arrival order, each until its own release.
 * The write store sends the writes of one issue one at a time (`web/src/features/writes/
 * write-store.ts`, one line per `issue:<KEY>` target), so a held save keeps every save behind it
 * off the wire: the handler is given the next one only once the one before it has been answered.
 */
async function holdPrioritySaves(page: Page, issueKey: string): Promise<HeldPrioritySave[]> {
  const held: HeldPrioritySave[] = [];
  await page.route(`**/api/v1/issues/${issueKey}`, async (route) => {
    const request = route.request();
    if (request.method() !== "PATCH") return route.fallback();
    const answered = Promise.withResolvers<void>();
    held.push({ body: request.postDataJSON(), release: answered.resolve });
    await answered.promise;
    return route.fallback();
  });
  return held;
}

/** The priority a save's body carries, and undefined for a body carrying none. */
function prioritySaved(body: unknown): unknown {
  return typeof body === "object" && body !== null && "priority" in body
    ? body.priority
    : undefined;
}

/** How many answers to a project's issue-list read (`GET /issues?project=<key>`, the read
 *  `projectIssuesQuery` makes) the page has had, counted as each answer's body lands: what tells
 *  a row that the list it reads is the server's answer and not the cache a pick wrote. */
function countProjectListReads(page: Page, project: string): () => number {
  let answered = 0;
  page.on("requestfinished", (request) => {
    const url = new URL(request.url());
    if (
      request.method() === "GET" &&
      url.pathname === "/api/v1/issues" &&
      url.searchParams.get("project") === project
    ) {
      answered += 1;
    }
  });
  return () => answered;
}

// SLOW(>=2s): the project list and two issue pages loaded in turn, three picks around five
// in-app navigations, and the held saves released one at a time against the real server.
//
// INFRA-299's two faults, driven as one reader's journey with one priority save held open, each
// against the code that answers it now:
//  - A list answer puts an old value back on screen. Each of the three issue field controls
//    writes through `web/src/features/issue/useIssueFieldWrite.ts`: it shows the pick in the
//    issue detail, every project issue list and every Inbox row, and the pick itself joins the
//    write store on the issue's line (`issue:<KEY>`). Every read of those rows is built by
//    `web/src/api/row-query.ts`, which holds a fetch while the store holds a write of a line
//    its key covers, so no list read goes out - and no answer carrying the priority the server
//    still holds lands - while the PATCH is unanswered.
//  - An older pick is stored last. A pick made while a save is out is a write of its own on the
//    issue's line, kept with the request it will send, and the store sends one write of a line
//    at a time in press order; a pick that replaces one of the same control's that has not gone
//    out takes that one out of the line (`Writes.discard`). So the order the server is sent the
//    picks in survives the control that made them: `IssuePage` keys `IssueDetail` by the issue,
//    and leaving the issue page and opening it again gives a fresh control whose own pick joins
//    the line behind the picks already in it.
//
// The second pick is made on the control that made the first, since that is where a pick can
// still replace one waiting its turn; the list answer is read after it, with the same save held.
test("a pick stands while a list answers, and the newest pick is the one the server is sent last", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Priority under a held save" });
  const other = await createIssue({ project: "CORE", title: "Another issue of the project" });
  await patchIssue(issue.key, { priority: HELD_SAVE_PRIORITY });
  const [firstPick, secondPick, thirdPick] = HELD_SAVE_PICKS;

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const listReads = countProjectListReads(page, "CORE");
    const held = await holdPrioritySaves(page, issue.key);
    const listRow = page.locator(`[data-issue-row="${issue.key}"]`);
    const listPriority = listRow.getByLabel(`Priority of ${issue.key}`);
    const header = page.getByTestId("issue-header");
    const headerPriority = header.getByLabel(`Priority of ${issue.key}`);

    // The project's list, read once, so every pick below is shown over an answer it holds.
    await page.goto("/projects/CORE/issues");
    await expect(listPriority).toHaveValue(String(HELD_SAVE_PRIORITY));

    // The first pick, on the issue page. Its PATCH is held from here to the release below, so
    // the server keeps the seeded priority for the whole journey.
    await navigateInApp(page, `/issues/${issue.key}`);
    await expect(headerPriority).toHaveValue(String(HELD_SAVE_PRIORITY));
    await headerPriority.selectOption(String(firstPick));
    await expect.poll(() => held.length).toBe(1);
    await expect(headerPriority).toHaveValue(String(firstPick));

    // The second pick, on the same control: no request of its own, because the store sends this
    // issue's writes one at a time and the save in flight has not been answered.
    await headerPriority.selectOption(String(secondPick));
    await expect(headerPriority).toHaveValue(String(secondPick));
    await nextFrames(page);
    expect(held, "a pick made while a save is out waits its turn, not sent").toHaveLength(1);

    // The list is asked to read again while that save is still held. Nothing is held back: the
    // read goes out and its answer lands, carrying the other issue's new title, and the write,
    // still live, shows its own change again over that answer, so the reader's row keeps the
    // pick (dispatch://INFRA-299/spec#b-r7). At the commit the change is built on the answer
    // puts the server's priority back on the row, which is how this row measures the fault.
    await navigateInApp(page, "/projects/CORE/issues");
    await expect(listRow).toBeVisible();
    const readsBefore = listReads();
    await patchIssue(other.key, { title: RETITLED });
    await expect.poll(listReads).toBeGreaterThan(readsBefore);
    await expect(page.locator(`[data-issue-row="${other.key}"]`)).toContainText(RETITLED);
    await nextFrames(page);
    await expect(listPriority).toHaveValue(String(secondPick));

    // And the issue page stands on the same pick.
    await navigateInApp(page, `/issues/${issue.key}`);
    await expect(headerPriority).toHaveValue(String(secondPick));

    // Another issue and back again: the control that made the second pick is gone, and the
    // third pick is made on the fresh one the return mounts, which joins the line behind it.
    await navigateInApp(page, `/issues/${other.key}`);
    await expect(header).toContainText(other.key);
    await navigateInApp(page, `/issues/${issue.key}`);
    await expect(headerPriority).toHaveValue(String(secondPick));
    await headerPriority.selectOption(String(thirdPick));
    await expect(headerPriority).toHaveValue(String(thirdPick));

    // The release, one save at a time: the store sends this issue's writes one at a time, so the
    // next save reaches the handler only once the one before it has been answered. A turn ends
    // when the next save has arrived or the tab holds no unconfirmed write of its own, so a
    // journey that sends two saves ends as surely as one that sends three.
    for (let index = 0; index < held.length; index += 1) {
      held[index]?.release();
      await expect
        .poll(async () => held.length > index + 1 || (await keptWrites(page)) === 0)
        .toBe(true);
    }

    // The second fault: which values reached the server and in what order, read from the bodies
    // the handler saw. The picks ascend (P0, then P1, then P2), so what the server was sent must
    // read as those of them it was sent, in that order: nothing the reader never picked, and no
    // pick behind one made after it. The last of them is the third pick, where the issue is left.
    const saved = held.map(({ body }) => prioritySaved(body));
    const inPickOrder = HELD_SAVE_PICKS.filter((pick) => saved.includes(pick));
    expect(saved, "the picks the server was sent, in the order it was sent them").toEqual(
      inPickOrder
    );
    expect(saved.at(0), "the save that was already out carries the first pick").toBe(firstPick);
    expect(saved.at(-1), "the last value the server is sent is the third pick").toBe(thirdPick);
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: thirdPick });

    // And once every save has settled, no write of this issue is live and the list stands on
    // what the server holds: the other issue's new title, and the reader's own row on the pick
    // the server was sent last.
    await navigateInApp(page, "/projects/CORE/issues");
    await expect(page.locator(`[data-issue-row="${other.key}"]`)).toContainText(RETITLED);
    await expect(listPriority).toHaveValue(String(thirdPick));
  } finally {
    await context.close();
  }
});
