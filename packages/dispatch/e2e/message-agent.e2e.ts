import { type Body, routePath } from "../web/src/api/paths";

import {
  agentMention,
  type FakeSession,
  getSentMessages,
  setLiveSessions,
  setSessionSendStatus,
} from "./agents";
import { createIssue, createProject, patchIssue } from "./api";
import { pickReference } from "./ref-picker";
import { expect, type Page, rowKey, rowSessionId, test } from "./test";
import { asUser } from "./users";

// Each session's id is the running row's own (`rowSessionId`, e2e/test.ts), read from a getter
// when a row names it, as `agentSessions` (e2e/agents.ts) does: Dispatch keeps a comment's
// deliveries to a session by its id, for every person at once, so under a fixed id a repeat of a
// row would meet the deliveries its earlier copy made.
const planner: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/w/legion",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: ["legion-planner"],
  get session_id() {
    return rowSessionId("planner");
  },
  title: "planner",
};
const worker: FakeSession = {
  capabilities: ["steer"],
  dir: "/w/worker",
  last_seen: 1_699_000_000_000,
  machine_id: "e2e",
  roles: [],
  get session_id() {
    return rowSessionId("worker");
  },
  title: "worker",
};

async function selectMention(page: Page, name: string): Promise<void> {
  const composer = page.getByRole("form", { name: "Comment composer" });
  await pickReference(composer.getByLabel("Comment"), "@", name);
}

type IssueCommentBody = Body<"/api/v1/issues/{key}/comments", "post">;

async function postPayload(page: Page, issueKey: string): Promise<IssueCommentBody> {
  const path = routePath("post", "/api/v1/issues/{key}/comments", { key: issueKey });
  const response = page.waitForResponse(
    (candidate) =>
      candidate.request().method() === "POST" && new URL(candidate.url()).pathname === path
  );
  await page
    .getByRole("form", { name: "Comment composer" })
    .getByRole("button", { name: "Send" })
    .click();
  return (await response).request().postDataJSON() as IssueCommentBody;
}

// @pr: A write a person makes: the composer offers the live roles and sessions and posts the canonical mention target for the one picked.
test("the unified composer offers live roles and sessions and posts canonical mention targets @pr", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await setLiveSessions([planner, worker]);
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Mention the worker" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "worker");
    const payload = await postPayload(page, issue.key);
    expect(payload).toEqual({
      body: agentMention(worker),
      delivery: "steer",
    });
  } finally {
    await alice.close();
  }
});

test("an issue route remains an owner route, not a recipient control", async ({ browser }) => {
  const core = rowKey("CORE");
  await setLiveSessions([planner]);
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Plain comment" });
  await patchIssue(issue.key, { route: "role:legion-planner" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    await expect(
      composer.getByRole("button", { name: /Choose recipient|BTW|Aside|Steer/ })
    ).toHaveCount(0);
    await composer.getByLabel("Comment").fill("Stored as a plain comment");
    const payload = await postPayload(page, issue.key);
    expect(payload).toEqual({ body: "Stored as a plain comment" });
  } finally {
    await alice.close();
  }
});

test("an offline selected mention is posted and exposes its failed delivery", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await setLiveSessions([worker]);
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Offline mention" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await selectMention(page, "worker");
    await setSessionSendStatus(worker.session_id, 404);
    const payload = await postPayload(page, issue.key);
    expect(payload.body).toBe(agentMention(worker));
    await expect(page.getByRole("list", { name: "Mention deliveries" })).toContainText("failed");
    // The sender retries a "no live session" answer under the one attempt and its key for the
    // listener take-over window - half a second under the harness's
    // DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW (e2e/run-server.sh), of the twelve
    // listenerTakeoverWindow gives a deployment (packages/envoy/internal/dispatch/api/
    // envoy_resolve.go) - so the listener records more than one send, all of one attempt: every
    // one to the worker under the same idempotency key.
    const sends = await getSentMessages();
    expect(sends.length).toBeGreaterThan(0);
    expect(sends.every((send) => send.target_session === worker.session_id)).toBe(true);
    expect(new Set(sends.map((send) => send.idempotency_key)).size).toBe(1);
  } finally {
    await alice.close();
  }
});
