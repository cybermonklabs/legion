import { setLiveSessions } from "./agents";
import {
  createIssue,
  createProject,
  getIssue,
  listComments,
  listIssueMessages,
  listIssues,
  patchIssue,
  setNewIssueSettings,
} from "./api";
import { modelsLane, proposePlan, say, scriptChat, scriptDecision } from "./fake-models-helpers";
import {
  approveAndFinish,
  askTheAgent,
  decided,
  lineOnTop,
  newestRun,
  newIssueRun,
  openBarOver,
  prose,
  RUN_STEPS_MS,
  searchBox,
  seedSplit,
  writeSpec,
} from "./going-panel";
import { expect, type Page, test } from "./test";
import { withAlice } from "./users";

// Check c7 of dispatch://INFRA-1588/artifact/verification-md (spec R6): Undo on the result of the
// split writes each old value back, closes the new issue, and names a value another person changed
// in between as not reversed, keeping that person's value; Undo of a run that posted a suggestion
// and a message rejects the open suggestion and names the message sent, not undone; and Undo of a
// new issue that has a route closes it, with no message on it. Negative controls: an Undo that
// writes back over a later change of another person, and one that keeps the undo kind trace for a
// suggestion, which stays open.

function undoLines(page: Page) {
  return newestRun(page).getByTestId("going-undo-line");
}

// SLOW(>=2s): a split planned, approved and run, another person's change, and the Undo.
test("Undo of the split writes each old value back, closes the new issue, and keeps another person's later value", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(150_000);
  const split = await seedSplit();
  const words = `split ${split.parent} and undo it`;
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Split."),
        turns: [
          proposePlan("Split the observatory.", [
            {
              arguments: { parent: split.parent, project: "CORE", title: "The second half" },
              tool: "dispatch_issue",
            },
            {
              arguments: { issue: split.first, title: "Pour the first foundation" },
              tool: "dispatch_issue_update",
            },
            {
              arguments: { issue: split.second, title: "Raise the first dome" },
              tool: "dispatch_issue_update",
            },
          ]),
        ],
        words,
      },
    ],
  });
  const before = (await listIssues("CORE")).map((issue) => issue.key);
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await askTheAgent(page, words);
    await approveAndFinish(page);
    const made = (await listIssues("CORE")).find((issue) => !before.includes(issue.key));
    expect(made?.title).toBe("The second half");
    // Bob renames the second child after the run.
    await patchIssue(split.second, { title: "Bob's dome" }, { login: "bob" });

    await newestRun(page).getByTestId("going-undo-button").click();
    await expect(undoLines(page)).toHaveCount(3, { timeout: RUN_STEPS_MS });
    expect((await getIssue(split.first)).title).toBe("Pour the foundation");
    expect((await getIssue(split.second)).title).toBe("Bob's dome");
    await expect(undoLines(page).filter({ hasText: split.second })).toContainText("kept");
    expect((await getIssue(made?.key ?? "")).status).toBe("done");
    expect(await listIssueMessages(made?.key ?? "")).toHaveLength(0);
  });
});

// SLOW(>=2s): a suggestion and a message planned, approved and run, and the Undo.
test("Undo of a run that posted a suggestion and a message rejects the suggestion and names the message sent, not undone", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(150_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Write the observing guide" });
  await writeSpec(issue, "# Guide\n\nLook up.\n");
  const words = `suggest a change to ${issue.key}'s spec and say so`;
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Suggested and said."),
        turns: [
          proposePlan("Suggest a clearer first line, and say so on the issue.", [
            {
              arguments: {
                artifact: "spec",
                body: "The first line says where to look.",
                issue: issue.key,
                quote: "Look up.",
                replace_with: "Look up, towards the north.",
              },
              tool: "dispatch_suggest",
            },
            {
              arguments: { body: "I suggested a clearer first line.", issue: issue.key },
              tool: "dispatch_message",
            },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${issue.key}`);
    await askTheAgent(page, words);
    await approveAndFinish(page);
    await newestRun(page).getByTestId("going-undo-button").click();
    await expect(undoLines(page)).toHaveCount(2, { timeout: RUN_STEPS_MS });
    await expect(undoLines(page).filter({ hasText: "message" })).toContainText("sent, not undone");
    const suggested = (await listComments(issue.key)).find(
      (comment) => comment.suggestion !== null
    );
    expect(suggested?.suggestion?.accepted).toBe(false);
    expect(suggested?.resolved).toBe(true);
  });
});

// SLOW(>=2s): a new issue filed from the line on top with a route, and its Undo.
test("Undo of a new issue that has a route closes it, and the issue holds no message", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await setLiveSessions([{ session_id: "new-issue-owner", title: "Owner of new issues" }]);
  await setNewIssueSettings({ label: "", session: "new-issue-owner" });
  await scriptDecision({ default: decided("dispatch_issue", 0.99) });
  const words = "we need a shutter for the dome";
  // The pause's short answer, and the run Enter on the line starts, which proposes the create.
  await scriptChat({
    default: prose("A new issue."),
    runs: [newIssueRun(words, "A shutter for the dome")],
  });
  const before = (await listIssues("CORE")).map((issue) => issue.key);
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill(words);
    await expect(lineOnTop(page)).toContainText("File as a new issue", { timeout: RUN_STEPS_MS });
    await searchBox(page).press("Enter");
    await expect(newestRun(page).getByTestId("going-undo-button")).toBeVisible({
      timeout: RUN_STEPS_MS,
    });
    const made = (await listIssues("CORE")).find((issue) => !before.includes(issue.key));
    expect(made?.route).toContain("new-issue-owner");
    await newestRun(page).getByTestId("going-undo-button").click();
    await expect(undoLines(page)).toHaveCount(1, { timeout: RUN_STEPS_MS });
    expect((await getIssue(made?.key ?? "")).status).toBe("done");
    expect(await listIssueMessages(made?.key ?? "")).toHaveLength(0);
  });
});
