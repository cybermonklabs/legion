import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createIssue,
  createProject,
  listAgentMessages,
  listIssueMessages,
  mintAgentToken,
} from "./api";
import {
  chatCalls,
  decided,
  decisionCalls,
  modelsLane,
  prose,
  releaseDecisions,
  scriptChat,
  scriptDecision,
} from "./fake-models-helpers";
import { bar, lineOnTop, newestRun, openBarOver, RUN_STEPS_MS, searchBox } from "./going-locators";
import { seedCoreIssue } from "./going-panel";
import { expect, type Page, test } from "./test";
import { withAlice } from "./users";

// Check c2 of dispatch://INFRA-1588/artifact/verification-md (spec R2): a key gives its hit and
// the fake models record no call; the fake Jev's action names the line on top - none gives Ask,
// dispatch_issue_update gives Do, dispatch_issue gives File as a new issue, which names the
// project it files in and, on an issue's page, the issue it files under; and with Jev holding
// its answer the hits show at once and the line reads Ask after 500 ms. With c8's browser half
// (spec R7): on an issue whose claim a live session holds, the fast tier offers no line that sends
// to that session at once, and Enter on the line on top sends it nothing. Negative controls: a bar
// that waits for Jev before it shows hits shows none for 5 s; one that keeps a row that sends to
// the issue's session offers it, and the session gets the words with no plan; a File line that
// names the parent and not the project (the tester's `m-fileline`) fails the DEMO-7 row.

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
    // The key's hit ranks above the line on top and holds the highlight, so Enter opens it, and
    // the line reads Ask, which no model decided (design v3, state 1: "A key or an exact title
    // ranks above the line on top, so Enter opens it (R2)").
    const rows = bar(page).getByRole("option");
    await expect(rows.nth(0)).toHaveId(`search-option-issue-${key}`);
    await expect(rows.nth(1)).toHaveId("search-option-line");
    await expect(searchBox(page)).toHaveAttribute(
      "aria-activedescendant",
      `search-option-issue-${key}`
    );
    await expect(lineOnTop(page)).toHaveAttribute("data-line", "ask");
    await expect(bar(page).getByTestId("search-line-deciding")).toHaveCount(0);
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

// SLOW(>=2s): seven seeded issues and Jev's answer.
test("on an issue's page Jev answering dispatch_issue makes the line on top File as a new issue in the page's project, under the page's issue", async ({
  browser,
}) => {
  modelsLane();
  // The page the ruling names (dispatch://INFRA-1588/comment/2df72357-f9f5-44cb-9cf9-d6daa3ce6b5e):
  // DEMO-7, the seventh issue of the project DEMO.
  await createProject({ key: "DEMO", name: "Demo" });
  let parent = "";
  for (const title of [
    "Polish the telescope",
    "Grind the mirror",
    "Align the finder scope",
    "Oil the dome's rails",
    "Count the moons of Saturn",
    "Log the meteor shower",
    "Map the lunar craters",
  ]) {
    parent = (await createIssue({ project: "DEMO", title })).key;
  }
  expect(parent, "the seed's seventh issue of DEMO").toBe("DEMO-7");
  await scriptDecision({ default: decided("dispatch_issue", 0.99) });
  await scriptChat({ default: prose("A new issue.") });
  const words = "we need a dome for the telescope";
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${parent}`);
    await searchBox(page).fill(words);
    await expect(lineOnTop(page)).toHaveAttribute("data-line", "file");
    // The label whole, the project and then the parent, and the words it files after it.
    await expect(lineOnTop(page).getByTestId("search-line-words")).toHaveText(
      `File as a new issue in DEMO, under DEMO-7: ${words}`
    );
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
    // The line on top repeats the words, which name the agent; no other row offers to send
    // them to the session (design v3, state 1).
    const rows = bar(page).getByRole("option");
    await expect(rows.filter({ hasText: /send/i })).toHaveCount(0);
    await expect(
      rows.and(bar(page).locator(":not([data-line])")).filter({ hasText: /agent/i })
    ).toHaveCount(0);
    // Enter on the line on top runs the words, and the run ends sending the session nothing.
    await searchBox(page).press("Enter");
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    expect(JSON.stringify(await listAgentMessages(session))).not.toContain(words);
    expect(JSON.stringify(await listIssueMessages(key))).not.toContain(words);
  });
});
