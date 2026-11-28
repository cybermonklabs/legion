import { createIssue, createProject } from "./api";
import { chatCalls, modelsLane, prose, scriptChat } from "./fake-models-helpers";
import { ANSWER_ENDS_MS, answerPanel, answerText, openBarOver, searchBox } from "./going-locators";
import { countAskedRuns, plainWords, seedIssue } from "./going-panel";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

// Check c2 of dispatch://INFRA-1588/artifact/verification-md@v16, serving R3 of spec v61: a run
// the person types past is cancelled, so a stale answer neither keeps running nor arrives. The
// fake writing model streams the first answer slowly; one more character typed once it has begun
// cancels the run, the fake counts its reply abandoned within one step, no further frame of it
// reaches the panel, and the answer for the new words streams in its place. With cancellation off
// the first stream runs to its end and the fake counts nothing abandoned, which this row catches.
//
// The second row serves R3 too: the answer the person typed past stays on the screen, its link
// with it, marked as the earlier words' answer, while its run is cancelled, until the new words'
// first text takes its place. Where the bar clears the panel at the keystroke instead, the mark
// and the kept answer are missing, which that row catches.

/** The bar's cancels from here on, `POST /api/v1/going/requests/{id}/cancel`, by their URLs. */
function countCancels(page: Page): string[] {
  const cancels: string[] = [];
  page.on("request", (request) => {
    if (
      request.method() === "POST" &&
      /\/api\/v1\/going\/requests\/[^/]+\/cancel$/.test(request.url())
    ) {
      cancels.push(request.url());
    }
  });
  return cancels;
}

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
    const cancels = countCancels(page);

    await searchBox(page).fill("quasar");
    await expect(answerText(page)).toContainText("alpha0", { timeout: ANSWER_ENDS_MS });
    await expect(answerPanel(page)).toHaveAttribute("data-state", "streaming");
    expect((await chatCalls()).abandoned).toBe(0);

    // From the keystroke itself on, every text the panel shows is recorded, and so is the text it
    // showed at the keystroke: a chunk of the first answer that lands before the key is pressed
    // is still its to show, and the panel keeps that much of it until the new words' answer.
    await page.evaluate(() => {
      const shown: string[] = [];
      Reflect.set(window, "__goingShown", shown);
      const text = () => document.querySelector('[data-testid="going-answer-text"]')?.textContent;
      let armed = false;
      document.addEventListener(
        "keydown",
        () => {
          armed = true;
          Reflect.set(window, "__goingShownAtKey", text() ?? "");
        },
        { capture: true, once: true }
      );
      new MutationObserver(() => {
        if (!armed) return;
        const now = text();
        if (now !== undefined && now !== null) shown.push(now);
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

    // No frame of the first answer reached the panel after the keystroke: of its text the panel
    // showed only what it held at the key, kept as the earlier words' answer.
    const { atKey, shown } = await page.evaluate(() => {
      const texts: unknown = Reflect.get(window, "__goingShown");
      return {
        atKey: String(Reflect.get(window, "__goingShownAtKey")),
        shown: Array.isArray(texts) ? texts.map(String) : [],
      };
    });
    expect(atKey).toContain("alpha0");
    expect(shown.length).toBeGreaterThan(0);
    expect(shown.filter((text) => text.includes("alpha") && text !== atKey)).toEqual([]);
    expect((await chatCalls()).abandoned).toBe(1);
  } finally {
    await context.close();
  }
});

/** What the panel showed at each change of the page from the next keystroke on, and its text at
 *  that keystroke: the answer's text, "" with none, and whether it was marked as answering words
 *  the person typed past. */
interface PanelStates {
  readonly atKey: string;
  readonly states: readonly { readonly text: string; readonly stale: boolean }[];
}

/** Records, in the page, what the panel shows at each change from the next keystroke on. */
async function recordPanelFromKeystroke(page: Page): Promise<void> {
  await page.evaluate(() => {
    const record = { atKey: "", states: [] as { text: string; stale: boolean }[] };
    Reflect.set(window, "__goingPanel", record);
    const text = () =>
      document.querySelector('[data-testid="going-answer-text"]')?.textContent ?? "";
    let armed = false;
    document.addEventListener(
      "keydown",
      () => {
        armed = true;
        record.atKey = text();
      },
      { capture: true, once: true }
    );
    new MutationObserver(() => {
      if (!armed) return;
      const stale = document.querySelector('[data-testid="going-answer-stale"]') !== null;
      record.states.push({ stale, text: text() });
    }).observe(document.body, {
      attributes: true,
      characterData: true,
      childList: true,
      subtree: true,
    });
  });
}

function recordedPanel(page: Page): Promise<PanelStates> {
  return page.evaluate(() => {
    const record: unknown = Reflect.get(window, "__goingPanel");
    return record as PanelStates;
  });
}

// SLOW(>=2s): two decisions, a slow first answer with a link cut short, and a slow second one.
test("one more character keeps the answer that began on the screen, its link with it, marked as the earlier words', while its run is cancelled, until the new words' text replaces it", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const issue = await seedIssue("quasar", "Quasar sightings log");
  await scriptChat({
    // The first conversation links the issue, then writes eighty words a few characters at a
    // time, so it is still streaming when the next character is typed. The second writes a frame
    // a second, so the kept answer is read before the new words' first text reaches the panel.
    default: prose(plainWords("beta", 3), { delayMs: 1_000 }),
    replies: [
      prose(`Kept in [Quasar sightings log](${issue.href}). ${plainWords("alpha", 80)}`, {
        chunk: 4,
        delayMs: 300,
      }),
    ],
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    const asked = countAskedRuns(page);
    const cancels = countCancels(page);
    const item = answerPanel(page).getByTestId("going-answer-item");
    const mark = answerPanel(page).getByTestId("going-answer-stale");

    await searchBox(page).fill("quasar");
    await expect(item).toHaveAttribute("href", issue.href, { timeout: ANSWER_ENDS_MS });
    await expect(answerText(page)).toContainText("alpha0", { timeout: ANSWER_ENDS_MS });
    await expect(answerPanel(page)).toHaveAttribute("data-state", "streaming");
    await recordPanelFromKeystroke(page);
    await searchBox(page).press("s");

    // The answer stays, its link with it, under a status naming the words it answers.
    await expect(answerPanel(page)).toHaveAttribute("data-state", "stale");
    await expect(mark).toHaveAttribute("role", "status");
    await expect(mark).toContainText("quasar");
    await expect(mark).not.toContainText("quasars");
    await expect(answerText(page)).toContainText("alpha0");
    await expect(item).toHaveAttribute("href", issue.href);

    // Its run is cancelled all the same, and its model call abandoned within one step.
    await expect.poll(() => cancels.length, { timeout: 5_000 }).toBe(1);
    await expect.poll(async () => (await chatCalls()).abandoned, { timeout: 5_000 }).toBe(1);

    // The new words' first text takes its place: no mark, no link, no word of the first answer.
    await expect(answerText(page)).toContainText("beta0", { timeout: ANSWER_ENDS_MS });
    await expect(mark).toHaveCount(0);
    await expect(item).toHaveCount(0);
    await expect(answerText(page)).not.toContainText("alpha");
    await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
      timeout: ANSWER_ENDS_MS,
    });
    expect(asked.words).toEqual(["quasar", "quasars"]);

    // Until then the panel never emptied: from the keystroke to the new words' first text it held
    // the first answer as it stood at the key, marked, through the pause and the new hits.
    const { atKey, states } = await recordedPanel(page);
    expect(atKey).toContain("alpha0");
    const replaced = states.findIndex((state) => state.text.includes("beta"));
    expect(replaced).toBeGreaterThan(0);
    expect(
      states.slice(0, replaced).filter((state) => !state.stale || state.text !== atKey)
    ).toEqual([]);
    expect(states.slice(replaced).filter((state) => state.stale)).toEqual([]);
  } finally {
    await context.close();
  }
});
