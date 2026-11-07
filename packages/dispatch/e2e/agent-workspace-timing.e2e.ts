import { expect, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { publishAgentStreamFrame } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * What a live view costs in time (INFRA-812). Every row here reads a wall clock - a view comes
 * back inside half a second, a view paints inside 100 ms with no task longer than 50 - so the
 * whole file runs in the `chromium-alone` project, with the machine to itself (D14,
 * `playwright.config.ts`'s `clockSpecs` and `packages/dispatch/AGENTS.md`). Four lanes put four
 * browsers, four Dispatch servers and four Postgres servers on one machine, and a second of
 * wall clock there holds what the other three lanes took as surely as the work the row
 * measured. The untimed half of each of those checks lives in `agent-view.e2e.ts`, where it
 * runs in `chromium` and `iphone` as any other row does.
 */

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  machine_id: "planner-host",
  roles: [],
  session_id: "01a0e0c1-0812-7000-8000-00000000d14a",
  title: "Planner",
};

/** The session's replay: one turn, which is what a painted view shows. */
const replay = {
  frames: [
    {
      kind: "message",
      message: {
        at: 1_000,
        id: "u1000",
        parts: [{ text: "Read the release notes", type: "text" }],
        role: "user",
        streaming: false,
      },
      seq: 1,
      v: 1,
    },
  ],
  session_id: planner.session_id,
  v: 1,
};

test.beforeEach(async () => {
  await resetDatabase();
  await setLiveSessions([planner]);
});

// No clock is read here: this row is what the timed ones measure, so a project that cannot open
// a live view at all fails on this rather than on a number.
test("the live view opens on the session's transcript", async ({ browser }) => {
  await publishAgentStreamFrame(planner.session_id, replay, "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${planner.session_id}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    await expect(page.getByTestId("agent-message-user")).toContainText("Read the release notes");
  } finally {
    await context.close();
  }
});
