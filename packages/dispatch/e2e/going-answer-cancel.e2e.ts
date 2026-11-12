import { createIssue, createProject } from "./api";
import { chatCalls, modelsLane, scriptChat } from "./fake-models-helpers";
import {
  ANSWER_ENDS_MS,
  answerPanel,
  answerText,
  countAskedRuns,
  openBarOver,
  plainWords,
  prose,
  searchBox,
} from "./going-panel";
import { expect, test } from "./test";
import { asUser } from "./users";

// Check c2 of dispatch://INFRA-1588/artifact/verification-md, serving R1: a run the person types
// past is cancelled, so a stale answer neither keeps running nor arrives. The fake writing model
// streams the first answer slowly; one more character typed once it has begun cancels the run,
// the fake counts its reply abandoned within one step, no further frame of it reaches the panel,
// and the answer for the new words streams in its place. With cancellation off the first stream
// runs to its end and the fake counts nothing abandoned, which this row catches.

// SLOW(>=2s): two decisions, a slow first answer cut short, and a second answer.
test("one more character cancels the run that began: its model call is abandoned, none of its text arrives after, and the new words are answered", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Quasar sightings log" });
  await scriptChat({
    // The first conversation: eighty words a few characters at a time, half a second apart, so
    // it is still streaming when the next character is typed. Every later one answers at once.
    default: prose(`beta ${plainWords("beta", 10)}`),
    replies: [prose(plainWords("alpha", 80), { chunk: 4, delayMs: 500 })],
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    const asked = countAskedRuns(page);
    const cancels: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "POST" &&
        /\/api\/v1\/going\/requests\/[^/]+\/cancel$/.test(request.url())
      ) {
        cancels.push(request.url());
      }
    });

    await searchBox(page).fill("quasar");
    await expect(answerText(page)).toContainText("alpha0", { timeout: ANSWER_ENDS_MS });
    await expect(answerPanel(page)).toHaveAttribute("data-state", "streaming");
    expect((await chatCalls()).abandoned).toBe(0);

    // From the keystroke on, every text the panel shows is recorded.
    await page.evaluate(() => {
      const shown: string[] = [];
      Reflect.set(window, "__goingShown", shown);
      new MutationObserver(() => {
        const text = document.querySelector('[data-testid="going-answer-text"]')?.textContent;
        if (text !== undefined && text !== null) shown.push(text);
      }).observe(document.body, { characterData: true, childList: true, subtree: true });
    });
    await searchBox(page).press("s");

    // The first run is cancelled, and its model call abandoned within one step of the stream.
    await expect.poll(() => cancels.length, { timeout: 5_000 }).toBe(1);
    await expect.poll(async () => (await chatCalls()).abandoned, { timeout: 5_000 }).toBe(1);

    // The new words are asked and answered in its place.
    await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
      timeout: ANSWER_ENDS_MS,
    });
    await expect(answerText(page)).toContainText("beta0");
    expect(asked.words).toEqual(["quasar", "quasars"]);

    // Nothing of the first answer reached the panel after the keystroke.
    const shown = await page.evaluate(() => {
      const texts: unknown = Reflect.get(window, "__goingShown");
      return Array.isArray(texts) ? texts.map(String) : [];
    });
    expect(shown.length).toBeGreaterThan(0);
    expect(shown.filter((text) => text.includes("alpha"))).toEqual([]);
    expect((await chatCalls()).abandoned).toBe(1);
  } finally {
    await context.close();
  }
});
