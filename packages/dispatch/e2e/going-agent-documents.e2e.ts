import { setLiveSessions } from "./agents";
import {
  acceptSuggestion,
  claimIssue,
  createIssue,
  createProject,
  getIssueArtifactText,
  listComments,
  listIssueMessages,
  mintAgentToken,
} from "./api";
import { modelsLane, proposePlan, say, scriptChat } from "./fake-models-helpers";
import {
  approveAndFinish,
  askTheAgent,
  openBarOver,
  planCard,
  planRows,
  prose,
  RUN_STEPS_MS,
  writeSpec,
} from "./going-panel";
import { expect, test } from "./test";
import { withAlice } from "./users";

// Check c9 of dispatch://INFRA-1588/artifact/verification-md (spec R7, R4): on an issue whose claim
// a session of the fake Envoy holds, a rewrite of the spec is a plan of one row, a suggestion in
// spec.md that shows two or three sentences with its diff closed; Approve posts the suggestion and
// leaves the spec's text as it was until Accept makes the new version; the words "suggest it and
// ask the architect to review it" give two rows, and the issue's Conversation then holds the
// message; and "edit the spec directly" gives a row that edits the spec. Negative control: a
// runner that writes the edit in place of a suggestion changes the spec before any Accept.

const SPEC = "# Spec\n\nThe list is numbered.\n\nSee dispatch://CORE-1 for the parent.\n";

async function seedClaimedSpec(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Number the list" });
  await writeSpec(issue, SPEC);
  const token = await mintAgentToken("documents-e2e", "alice");
  await setLiveSessions([{ session_id: "documents-architect", title: "Architect" }]);
  await claimIssue(issue.key, {
    actor: { id: "documents-architect", kind: "session" },
    as: "agent",
    token,
  });
  return issue.key;
}

const suggestion = (issue: string) => ({
  arguments: {
    artifact: "spec",
    body: "The back-link moves to the top, so a reader sees the parent first. The list is unchanged.",
    issue,
    quote: "The list is numbered.",
    replace_with: "See dispatch://CORE-1 for the parent.\n\nThe list is numbered.",
  },
  tool: "dispatch_suggest",
});

// SLOW(>=2s): a claimed issue, a run that plans a suggestion, its approval and the Accept.
test("a rewrite of the spec is a suggestion with its sentences and its diff closed, and the spec changes only on Accept", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(150_000);
  const issue = await seedClaimedSpec();
  const words = "rewrite the spec so that the back-link comes first";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Suggested."),
        turns: [proposePlan("Suggest moving the back-link first.", [suggestion(issue)])],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${issue}`);
    await askTheAgent(page, words);
    await expect(planRows(planCard(page))).toHaveCount(1, { timeout: RUN_STEPS_MS });
    await expect(planCard(page).getByTestId("going-plan-sentences")).toContainText(
      "back-link moves to the top"
    );
    await expect(planCard(page).getByTestId("going-plan-diff")).not.toHaveAttribute("open", "");
    await approveAndFinish(page);
    expect((await getIssueArtifactText(issue, "spec")).markdown).toBe(SPEC);
    const posted = (await listComments(issue)).find((comment) => comment.suggestion !== null);
    expect(posted?.suggestion?.replace_with).toBe(suggestion(issue).arguments.replace_with);
    await acceptSuggestion(posted?.id ?? "");
    expect((await getIssueArtifactText(issue, "spec")).markdown).toContain(
      "See dispatch://CORE-1 for the parent.\n\nThe list is numbered."
    );
  });
});

// SLOW(>=2s): a run that plans a suggestion and a message to the issue's architect.
test("suggest it and ask the architect to review it gives two rows, and the issue's Conversation holds the message", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(150_000);
  const issue = await seedClaimedSpec();
  const words = "suggest it and ask the architect to review it";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Suggested and asked."),
        turns: [
          proposePlan("Suggest moving the back-link first, and ask the architect to review it.", [
            suggestion(issue),
            {
              arguments: {
                body: "Please review my suggestion in spec.md that moves the back-link first.",
                issue,
              },
              needs: [0],
              tool: "dispatch_message",
            },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${issue}`);
    await askTheAgent(page, words);
    await expect(planRows(planCard(page))).toHaveCount(2, { timeout: RUN_STEPS_MS });
    await approveAndFinish(page);
    expect(JSON.stringify(await listIssueMessages(issue))).toContain("Please review my suggestion");
  });
});

// SLOW(>=2s): a run that plans a direct edit of the spec.
test("edit the spec directly gives a row that edits the spec", async ({ browser }) => {
  modelsLane();
  test.setTimeout(120_000);
  const issue = await seedClaimedSpec();
  const words = "edit the spec directly";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        afterwards: say("Edited."),
        turns: [
          proposePlan("Move the back-link first in the spec.", [
            {
              arguments: {
                artifact: "spec",
                issue,
                ops: [
                  {
                    find: "The list is numbered.",
                    op: "replace",
                    with: "The list is numbered from 1.",
                  },
                ],
                summary: "The list says where it starts.",
              },
              tool: "dispatch_doc_edit",
            },
          ]),
        ],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${issue}`);
    await askTheAgent(page, words);
    await expect(planRows(planCard(page))).toHaveCount(1, { timeout: RUN_STEPS_MS });
    await expect(planRows(planCard(page))).toContainText("doc edit");
    await approveAndFinish(page);
    expect((await getIssueArtifactText(issue, "spec")).markdown).toContain("numbered from 1.");
  });
});
