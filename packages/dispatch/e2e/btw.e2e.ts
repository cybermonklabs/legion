import { agentMention, type FakeSession, setLiveSessions, setSessionSendStatus } from "./agents";
import { createIssue, createProject, listComments } from "./api";
import { selectMention } from "./mention";
import { expect, type Page, rowKey, rowSessionId, test } from "./test";
import { asUser } from "./users";

/** The planner session, under the running row's own id (`rowSessionId`, e2e/test.ts): Dispatch
 *  keeps a comment's deliveries to a session by its id, for every person at once, so a repeat of
 *  a row under a fixed id would meet the deliveries its earlier copy sent. */
function plannerSession(): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: "/w/legion",
    last_seen: 1_700_000_000_000,
    machine_id: "e2e",
    roles: [],
    session_id: rowSessionId("planner"),
    title: "planner",
  };
}

async function post(page: Page): Promise<Record<string, unknown>> {
  const response = page.waitForResponse(
    (candidate) =>
      candidate.request().method() === "POST" &&
      /\/api\/v1\/issues\/[^/]+\/comments$/.test(candidate.url())
  );
  await page
    .getByRole("form", { name: "Comment composer" })
    .getByRole("button", { name: "Send" })
    .click();
  return (await response).request().postDataJSON() as Record<string, unknown>;
}

// This row carries no `@pr` tag: the pull-request lane is at its row budget (`laneBudget`,
// e2e/__tests__/pull-request-lane.test.ts). Two rows keep the /btw claim on every pull request:
// `mention-composer.e2e.ts`'s "E1 and E2: explicit role mentions deliver once while /btw without a
// mention stays a plain comment @pr", whose /btw role mention posts `delivery: "btw"` with the
// token gone from the body and is sent once, and Go's `TestCommentBodyMentionDelivers`
// (packages/envoy/internal/dispatch/api/comment_body_mentions_test.go), in which a btw comment
// whose body names a session is stored as written and delivered to it once, in btw mode. The full
// lane runs this one.
test("/btw with a surviving mention strips its token and sends one comment-level mode", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  const planner = plannerSession();
  await setLiveSessions([planner]);
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "BTW comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "/btw @", "planner");
    expect(await post(page)).toEqual({
      body: agentMention(planner),
      delivery: "btw",
    });
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([
        {
          body: agentMention(planner),
          mentions: [{ target: `session:${planner.session_id}` }],
        },
      ]);
  } finally {
    await alice.close();
  }
});

test("/btw without a surviving mention remains verbatim and never sends delivery", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Plain BTW comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await page
      .getByRole("form", { name: "Comment composer" })
      .getByLabel("Comment")
      .fill("/btw hello");
    expect(await post(page)).toEqual({ body: "/btw hello" });
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([{ body: "/btw hello", deliveries: [], mentions: [] }]);
  } finally {
    await alice.close();
  }
});

test("an unsupported selected mode warns but does not prevent Send", async ({ browser }) => {
  const core = rowKey("CORE");
  const offline: FakeSession = {
    ...plannerSession(),
    capabilities: [],
    session_id: rowSessionId("worker"),
    title: "worker",
  };
  await setLiveSessions([offline]);
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Unadvertised delivery" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "/aside @", "worker");
    await expect(
      page.getByText("worker advertises no delivery mode, so sending it records a failed attempt.")
    ).toBeVisible();
    await setSessionSendStatus(offline.session_id, 404);
    expect(await post(page)).toEqual({
      body: agentMention(offline),
      delivery: "aside",
    });
  } finally {
    await alice.close();
  }
});

test("/btw with nothing after it keeps an agent's direct composer from sending, and Send says why on screen", async ({
  browser,
}) => {
  // No `last_seen`: the fixture stamps one, so the session lists as live rather than inactive.
  const worker: FakeSession = {
    capabilities: ["aside", "btw", "steer"],
    dir: "/w/legion",
    machine_id: "e2e",
    roles: [],
    session_id: rowSessionId("worker"),
    title: "worker",
  };
  await setLiveSessions([worker]);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const posts: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && request.url().includes("/api/v1/agents/")) {
        posts.push(request.url());
      }
    });
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const card = agents
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "worker" }) });
    await card.getByRole("button", { exact: true, name: "worker" }).click();
    const field = card.getByRole("textbox", { name: "Comment" });
    const send = card.getByRole("button", { exact: true, name: "Send" });
    const hint = card.getByText("Ctrl/Cmd+Enter to send · Enter for a new line", { exact: true });
    const reason = card.getByText("Type the message after /btw.", { exact: true });

    // The box holds text, and the command strips to an empty message: Send refuses, and says why.
    await field.fill("/btw ");
    await expect(send).toBeDisabled();
    await expect(send).toHaveAccessibleDescription("Type the message after /btw.");
    await expect(send).toHaveAttribute("title", "Type the message after /btw.");
    // The reason is on screen, in the hint's place, for a reader with no pointer to hover Send
    // with. `toBeVisible` passes for an `sr-only` line, a 1 px box, so its height is the check.
    await expect(hint).toHaveCount(0);
    await expect(reason).toBeVisible();
    expect((await reason.boundingBox())?.height ?? 0).toBeGreaterThan(8);
    // A keyboard reaches it, so the reason does too: Tab from the message lands on Send.
    await field.press("Tab");
    await expect(send).toBeFocused();
    await send.click({ force: true });
    await field.press("Control+Enter");
    // Neither the forced click nor Ctrl+Enter may send while `/btw ` strips to an empty message,
    // and a send that never happens marks nothing to wait on: this window is the check. A POST
    // would leave the page in the task of the gesture that made it and `page.on` above records
    // it as it goes, so 500 ms of real time is many frames past where it would show.
    await page.waitForTimeout(500);
    expect(posts).toEqual([]);

    await field.fill("/btw status?");
    await expect(send).toBeEnabled();
    await expect(send).not.toHaveAttribute("aria-describedby");
    await expect(reason).toHaveCount(0);
    await expect(hint).toBeVisible();
  } finally {
    await alice.close();
  }
});
