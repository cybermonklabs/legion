/** The row reads what the server's agent process made of the fake writing model's turns: the
 *  answer the server keeps when a run ends, the model calls the fake counted, and the result of a
 *  list of issues that the server gave the model. */
import { createIssue, createProject, listIssues } from "./api";
import { callTool, modelsLane, prose, runCalls, say, scriptChat } from "./fake-models-helpers";
import { askTheAgent, newestRun, openBarOver, RUN_STEPS_MS } from "./going-locators";
import { plainWords } from "./going-panel";
import { expect, test } from "./test";
import { withAlice } from "./users";

// A count in an answer's prose is the server's: in a preview with the real models at 8a3e562b an
// answer said 41 issues beside a chart of 42, so a list result gives its total, as the architect
// ruled (dispatch://INFRA-1588/comment/96cd5279-bc9b-4f8a-b8f8-435901cdca75).
// The row runs in the models lane alone (`bun run e2e:models`).

/** How many plain words the title of each issue the row seeds holds. */
const TITLE_WORDS = 8;

/** Seeds project CORE with `count` issues, each titled with TITLE_WORDS plain words of its own,
 *  since a second issue of one title is refused as a copy of the first. */
async function seedIssues(count: number): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < count; index += 1) {
    await createIssue({ project: "CORE", title: plainWords(`ledger${index}x`, TITLE_WORDS) });
  }
}

// SLOW(>=2s): three seeded issues and a run of two model calls and a read.
test("the list of a project's issues gives the model its total", async ({ browser }) => {
  modelsLane();
  test.setTimeout(120_000);
  await seedIssues(3);
  const total = (await listIssues("CORE")).length;
  const words = "how many ledgers does the core project hold in all";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        turns: [callTool("dispatch_issues", { all: true, project: "CORE" }), say("Listed them.")],
        words,
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, words);

    await expect(newestRun(page).getByTestId("going-run-answer")).toContainText("Listed them.", {
      timeout: RUN_STEPS_MS,
    });
    expect((await listIssues("CORE")).length).toBe(total);
    const calls = await runCalls(words);
    expect(calls.map((call) => call.turn)).toEqual([0, 1]);
    expect(calls[1].toolResults).toHaveLength(1);
    // The result names its total, the count the project's list shows, beside its rows: a row's
    // own "total" is the count of its children, inside its progress, and is not that.
    expect(calls[1].toolResults[0]).toMatch(
      new RegExp(`(?<!"progress":\\{[^}]{0,80})total\\D{0,12}${total}\\b`, "i")
    );
  });
});
