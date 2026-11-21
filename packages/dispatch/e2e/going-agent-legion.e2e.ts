import { setLiveSessions } from "./agents";
import {
  createIssue,
  createProject,
  getIssue,
  getIssueEvents,
  listIssueMessages,
  listIssues,
  setNewIssueSettings,
} from "./api";
import { modelsLane, proposePlan, say, scriptChat, scriptDecision } from "./fake-models-helpers";
import {
  askTheAgent,
  decided,
  lineOnTop,
  newestRun,
  newIssueRun,
  openBarOver,
  planCard,
  planOnPage,
  planRows,
  prose,
  RUN_STEPS_MS,
  searchBox,
} from "./going-panel";
import { expect, test } from "./test";
import { withAlice } from "./users";

// Check c10 of dispatch://INFRA-1588/artifact/verification-md (spec R8, R9): a hand-off to Legion
// is the changes the words name, as the person - a label and a status, and no other row - and the
// words alone give a question and no plan; and with a session set for new issues, Enter on the
// line File as a new issue files it at once with its title, project, parent, priority, spec and
// that route, and no message, and the notice has Undo. Negative control: a runner that writes the
// label as the bar's own session, whose event log then names no person.

// SLOW(>=2s): a backlog issue, a plan of two changes, and its approval.
test("a hand-off to Legion is the label and the status the words name, made as the person", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Catalogue the nebulae" });
  const words = `Hand ${issue.key} to Legion: add the label legion and set it to todo`;
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Handed over."),
        turns: [
          proposePlan("Label it legion and set it to todo.", [
            { arguments: { issue: issue.key, labels: ["legion"] }, tool: "dispatch_issue_update" },
            { arguments: { issue: issue.key, status: "todo" }, tool: "dispatch_issue_update" },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${issue.key}`);
    await askTheAgent(page, words);
    await expect(newestRun(page)).toHaveAttribute("data-state", "waiting", {
      timeout: RUN_STEPS_MS,
    });
    const rows = planRows(planOnPage(page)).or(planRows(planCard(page)));
    await expect(rows).toHaveCount(2);
    await searchBox(page).focus();
    await searchBox(page).press("Enter");
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    const after = await getIssue(issue.key);
    expect(after.labels).toContain("legion");
    expect(after.status).toBe("todo");
    const events = await getIssueEvents(issue.key);
    const changes = events.filter((event) => event.type === "issue.updated");
    expect(changes.length).toBeGreaterThan(0);
    for (const event of changes) {
      expect(event.actor).toMatchObject({ id: "alice", kind: "user" });
    }
  });
});

// SLOW(>=2s): a run whose model asks which changes to make.
test("Hand the issue to Legion alone gives a question about which changes to make, and no plan", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Catalogue the nebulae" });
  const words = `Hand ${issue.key} to Legion`;
  const question = "Which changes hand it to Legion here: a label, a status, a route?";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say(question)], words }],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${issue.key}`);
    await askTheAgent(page, words);
    await expect(newestRun(page).getByTestId("going-run-answer")).toHaveText(question, {
      timeout: RUN_STEPS_MS,
    });
    await expect(newestRun(page)).toHaveAttribute("data-state", "done");
    await expect(planCard(page)).toHaveCount(0);
  });
});

// SLOW(>=2s): a new issue filed from the line on top with the session set for new issues.
test("with a session set for new issues, Enter on File as a new issue files it at once with its route and no message, and the notice has Undo", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await setLiveSessions([{ session_id: "new-issue-session", title: "New issues" }]);
  await setNewIssueSettings({ label: "", session: "new-issue-session" });
  await scriptDecision({ default: decided("dispatch_issue", 0.99) });
  const words = "we need a spectrograph for the nebulae";
  const title = "A spectrograph for the nebulae";
  // The pause's short answer, and the run Enter on the line starts, which proposes the create.
  await scriptChat({ default: prose("A new issue."), runs: [newIssueRun(words, title)] });
  const before = (await listIssues("CORE")).map((made) => made.key);
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill(words);
    await expect(lineOnTop(page)).toContainText("File as a new issue in CORE", {
      timeout: RUN_STEPS_MS,
    });
    await searchBox(page).press("Enter");
    await expect(newestRun(page).getByTestId("going-undo-button")).toBeVisible({
      timeout: RUN_STEPS_MS,
    });
    const made = (await listIssues("CORE")).find((issue) => !before.includes(issue.key));
    expect(made).toBeDefined();
    const filed = await getIssue(made?.key ?? "");
    expect(filed.title).toBe(title);
    expect(filed.project).toBe("CORE");
    expect(filed.route).toContain("new-issue-session");
    expect(await listIssueMessages(made?.key ?? "")).toHaveLength(0);
  });
});
