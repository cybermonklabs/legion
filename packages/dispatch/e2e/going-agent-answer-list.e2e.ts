/** Every row reads what the bar draws of an answer the server's agent process kept from the fake
 *  writing model's turn: the mark the browser draws before each line of the answer's lists. */
import { modelsLane, prose, say, scriptChat } from "./fake-models-helpers";
import { askTheAgent, newestRun, openBarOver, RUN_STEPS_MS } from "./going-locators";
import { seedIssues } from "./going-panel";
import { expect, type Locator, type Page, test } from "./test";
import { withAlice } from "./users";

// The answer's rule for a list (ConversationView.tsx; its test in ConversationView.test.tsx says
// it): a line whose first word is a chip takes no list mark, since the chip marks the line as a
// bullet would, and a line of words keeps its bullet. In a preview with the real models at
// 6607a6b4, the answers to "What is the state of this issue's open children?" drew it both ways
// (dispatch://INFRA-1588/artifact/tester-c32-6607a6b4-q1-try4-png): in 4 of 10 answers each line
// began with its chip and drew a bullet, and in one answer the first of four lines that began with
// words drew none and the other three drew theirs. Two causes. The rule looks for the chip's link
// as the first element of its line, and the bar wraps a chip and the punctuation written right
// after it in a span (MarkdownBody.tsx, holdPunctuation), so a chip with a colon, a comma or a
// full stop after it is no longer that first element. And the first element of a line is its
// first element whatever words stand before it, so a line of words whose first link is a chip
// with no punctuation after it loses its bullet.
// Every row runs in the models lane alone (`bun run e2e:models`).

const TITLES = [
  "Mount alignment drifts overnight",
  "Dome motor stalls in frost",
  "Guide camera drops frames",
];

/** A link to the issue `key`, which the bar draws as its chip. */
function chip(key: string): string {
  return `[${key}](/issues/${key})`;
}

/** For each line of the lists in `answer`, in order: whether the browser draws its list mark. */
async function listMarks(answer: Locator): Promise<boolean[]> {
  return answer.evaluate((element) =>
    Array.from(element.querySelectorAll("li")).map(
      (line) => getComputedStyle(line).listStyleType !== "none"
    )
  );
}

/** Asks `words` in the bar over CORE's issues, to which the fake writing model answers `answer`,
 *  and gives the answer once the run is done and its `chips` chips are drawn. */
async function answered(page: Page, words: string, chips: number): Promise<Locator> {
  await openBarOver(page, "/projects/CORE/issues");
  await askTheAgent(page, words);
  const shown = newestRun(page).getByTestId("going-run-answer");
  await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
  await expect(shown.getByTestId("item-chip")).toHaveCount(chips);
  return shown;
}

// SLOW(>=2s): three seeded issues and a run of one model call.
test("a line of a list whose first word is a chip draws no bullet, whatever is written after the chip", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const [first, second, third] = await seedIssues(TITLES);
  const words = "which parts of the northern dome are still open";
  // A colon, a comma and a word after the chip, in a tight list.
  const answer = [
    "Three are open:",
    "",
    `- ${chip(first)}: the mount is aligned again tonight`,
    `- ${chip(second)}, with nobody on it yet`,
    `- ${chip(third)} waits on clear weather`,
  ].join("\n");
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say(answer)], words }],
  });
  await withAlice(browser, async (page) => {
    const shown = await answered(page, words, 3);
    await expect(shown.locator("li")).toHaveCount(3);
    await expect.poll(() => listMarks(shown)).toEqual([false, false, false]);
  });
});

// SLOW(>=2s): three seeded issues and a run of one model call.
test("a line of a loose list whose first word is a chip draws no bullet, whatever is written after the chip", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const [first, second, third] = await seedIssues(TITLES);
  const words = "which parts of the southern dome are still open";
  // The same three lines with a blank line between them: each line's words are a paragraph.
  const answer = [
    "Three are open:",
    "",
    `- ${chip(first)}: the mount is aligned again tonight`,
    "",
    `- ${chip(second)}. Nobody is on it yet`,
    "",
    `- ${chip(third)} waits on clear weather`,
  ].join("\n");
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say(answer)], words }],
  });
  await withAlice(browser, async (page) => {
    const shown = await answered(page, words, 3);
    await expect(shown.locator("li")).toHaveCount(3);
    await expect.poll(() => listMarks(shown)).toEqual([false, false, false]);
  });
});

// SLOW(>=2s): two seeded issues and a run of one model call.
test("a line of a list whose first word is not a chip keeps its bullet, whether or not a chip follows", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const [first, second] = await seedIssues(TITLES.slice(0, 2));
  const words = "what is the state of the eastern dome by its parts";
  // Words, then a chip with a word after it; words, then a chip with a comma after it; words alone.
  const answer = [
    "By state:",
    "",
    `- Aligning: ${chip(first)} is in hand tonight`,
    `- Stalled: ${chip(second)}, since the frost`,
    "- Nothing else is open there",
  ].join("\n");
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say(answer)], words }],
  });
  await withAlice(browser, async (page) => {
    const shown = await answered(page, words, 2);
    await expect(shown.locator("li")).toHaveCount(3);
    await expect.poll(() => listMarks(shown)).toEqual([true, true, true]);
  });
});
