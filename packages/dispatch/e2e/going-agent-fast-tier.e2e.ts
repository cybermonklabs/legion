import { setLiveSessions } from "./agents";
import { claimIssue, listAgentMessages, listIssueMessages, mintAgentToken } from "./api";
import {
  chatCalls,
  decisionCalls,
  modelsLane,
  releaseDecisions,
  scriptChat,
  scriptDecision,
} from "./fake-models-helpers";
import {
  bar,
  decided,
  lineOnTop,
  newestRun,
  openBarOver,
  prose,
  RUN_STEPS_MS,
  searchBox,
  seedCoreIssue,
} from "./going-panel";
import { expect, type Page, test } from "./test";
import { withAlice } from "./users";

// Check c2 of dispatch://INFRA-1588/artifact/verification-md (spec R2): a key gives its hit and
// the fake models record no call; the fake Jev's action names the line on top - none gives Ask,
// dispatch_issue_update gives Do, dispatch_issue gives File as a new issue; and with Jev holding
// its answer the hits show at once and the line reads Ask after 500 ms. With c8's browser half
// (spec R7): on an issue whose claim a live session holds, the fast tier offers no line that sends
// to that session at once, and Enter on the line on top sends it nothing. Negative controls: a bar
// that waits for Jev before it shows hits shows none for 5 s; one that keeps a row that sends to
// the issue's session offers it, and the session gets the words with no plan.

/** The hit of `key`, as the bar lists it: the option of the issue the key names. */
function hitOf(page: Page, key: string) {
  return bar(page).locator(`#search-option-issue-${key}`);
}

// SLOW(>=2s): a seeded issue and a wait that no model is asked.
test("a key gives its hit, and the fake models record no call", async ({ browser }) => {
  modelsLane();
  const key = await seedCoreIssue("Polish the telescope");
  await scriptChat({ default: prose("No short answer should be asked for a key.") });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill(key);
    await expect(hitOf(page, key)).toBeVisible();
    await expect(lineOnTop(page)).toHaveCount(0);
    // Well past the pause the short answer would be asked on.
    await page.waitForTimeout(1500);
    expect((await chatCalls()).calls).toBe(0);
    expect((await decisionCalls()).calls).toBe(0);
  });
});

// SLOW(>=2s): a seeded issue and Jev's answer.
test("Jev answers none: Ask", async ({ browser }) => {
  modelsLane();
  await seedCoreIssue("Polish the telescope");
  await scriptDecision({ default: decided("none", 0.99) });
  await scriptChat({ default: prose("The telescope is CORE's.") });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill("who looks after the telescope");
    await expect(lineOnTop(page)).toContainText("Ask");
  });
});

// SLOW(>=2s): a seeded issue and Jev's answer.
test("Jev answering dispatch_issue_update makes the line on top Do", async ({ browser }) => {
  modelsLane();
  await seedCoreIssue("Polish the telescope");
  await scriptDecision({ default: decided("dispatch_issue_update", 0.99) });
  await scriptChat({ default: prose("On it.") });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill("mark the telescope work as started");
    await expect(lineOnTop(page)).toContainText("Do");
  });
});

// SLOW(>=2s): a seeded issue and Jev's answer.
test("Jev answering dispatch_issue makes the line on top File as a new issue", async ({
  browser,
}) => {
  modelsLane();
  await seedCoreIssue("Polish the telescope");
  await scriptDecision({ default: decided("dispatch_issue", 0.99) });
  await scriptChat({ default: prose("A new issue.") });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill("we need a dome for the telescope");
    await expect(lineOnTop(page)).toContainText("File as a new issue in CORE");
  });
});

// SLOW(>=2s): Jev held for its whole wait.
test("with Jev holding its answer the hits show at once, and after 500 ms the line on top reads Ask", async ({
  browser,
}) => {
  modelsLane();
  const key = await seedCoreIssue("Polish the telescope");
  await scriptDecision({ default: { ...decided("dispatch_issue_update", 0.99), hold: true } });
  await scriptChat({ default: prose("The telescope.") });
  await withAlice(browser, async (page) => {
    try {
      await openBarOver(page, "/projects/CORE/issues");
      await searchBox(page).fill("telescope polish");
      await expect(hitOf(page, key)).toBeVisible({ timeout: 2000 });
      await expect(lineOnTop(page)).toContainText("Ask", { timeout: 2000 });
    } finally {
      await releaseDecisions();
    }
  });
});

// SLOW(>=2s): a live session's claim, a pause in the bar over its issue, and a run of its words.
test("on an issue a live session holds, the fast tier offers no line that sends to it at once", async ({
  browser,
}) => {
  modelsLane();
  const key = await seedCoreIssue("Grind the mirror");
  const session = "fast-tier-holder";
  const token = await mintAgentToken("fast-tier", "alice");
  await setLiveSessions([{ session_id: session, title: "Mirror grinder" }]);
  await claimIssue(key, { actor: { id: session, kind: "session" }, as: "agent", token });
  await scriptChat({ default: prose("The mirror is being ground.") });
  const words = "tell the agent to use the finer grit";
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${key}`);
    await searchBox(page).fill(words);
    await expect(lineOnTop(page)).toBeVisible();
    await expect(bar(page).getByRole("option", { name: /send|agent/i })).toHaveCount(0);
    // Enter on the line on top runs the words, and the run ends sending the session nothing.
    await searchBox(page).press("Enter");
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    expect(JSON.stringify(await listAgentMessages(session))).not.toContain(words);
    expect(JSON.stringify(await listIssueMessages(key))).not.toContain(words);
  });
});
